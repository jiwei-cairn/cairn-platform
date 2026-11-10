// deploy.go — 部署流水线：安装与升级共用（decisions.md C3/D5/D11）。
//
// 覆盖式换文件的三段保护：
//   .staging  解包过渡目录（断电 → 启动时清理）
//   .old      被替换的旧产物（Windows 无法覆盖运行中 exe，只能换名；
//             升级成功即删，启动兜底清理残留）
//   .bak      被新版配置覆盖的用户配置（仅一份，下次升级时被覆盖）
//
// data 角色文件若目标已存在则保留不覆盖（E2：旧数据留存不丢弃）。
package api

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/config"
	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/pack"
	"github.com/jiwei-cairn/cairn-platform/internal/store"

	goruntime "runtime"
)

// stagingDirName / oldSuffix / bakSuffix 约定的过渡文件标记。
const (
	stagingDirName = ".staging"
	oldSuffix      = ".old"
	bakSuffix      = ".bak"
)

// precheckDeploy 包级预检：平台匹配 / 版本可解析 /（best-effort）磁盘余量。
// 名称冲突与服务存在性由调用方按 install/upgrade 模式分别判定。
func (s *Server) precheckDeploy(m *pack.Manifest) error {
	if m.Os != goruntime.GOOS {
		return fmt.Errorf("包目标平台 %s 与本机 %s 不符", m.Os, goruntime.GOOS)
	}
	if m.Arch != goruntime.GOARCH {
		return fmt.Errorf("包目标架构 %s 与本机 %s 不符", m.Arch, goruntime.GOARCH)
	}
	if m.MinDiskMB > 0 {
		// 磁盘余量探测：数据根所在卷可写即视为可用，精确余量留 M2
		//（syscall.Statfs/GetDiskFreeSpaceExW 平台差异大，此处不阻塞部署）。
		probe := filepath.Join(dataDirOf(m.Name), ".diskprobe")
		if err := os.MkdirAll(filepath.Dir(probe), 0o755); err == nil {
			_ = os.WriteFile(probe, []byte("x"), 0o644)
			_ = os.Remove(probe)
		} else {
			return fmt.Errorf("数据目录不可写: %w", err)
		}
	}
	return nil
}

// runDeployPipeline 包已就位（本地暂存或 OTA 下载）后的统一部署流程。
//
// 阶段：校验 → 预检 → （升级）停服等待退出 → 解包 .staging → 换文件 →
// 落库 → （升级）发起启动 → 清理。
// 任何阶段失败：升级模式尽力把 .old 换回（文件级可逆），任务标记 failed。
func (s *Server) runDeployPipeline(t *upgradeTask, pkgPath, source string) error {
	s.Tasks.updatePhase(t, PhaseVerifying, "打开并校验包")
	p, err := pack.Open(pkgPath)
	if err != nil {
		return fmt.Errorf("包校验失败: %w", err)
	}
	defer p.Close()
	m := p.Manifest

	// ── 预检 ──
	s.Tasks.updatePhase(t, PhasePrecheck, "平台/磁盘/版本预检")
	if err := s.precheckDeploy(m); err != nil {
		return err
	}

	var def store.Service
	hasSvc := s.DB.First(&def, "name = ?", m.Name).Error == nil
	if hasSvc {
		t.Mode = "upgrade"
		t.FromVer = def.Version
	} else {
		t.Mode = "install"
	}
	if t.Mode == "upgrade" && def.Version == m.Version {
		return fmt.Errorf("当前已是 %s，无需重复部署", m.Version)
	}

	svcDir := filepath.Join(s.installRoot(), m.Name)
	staging := filepath.Join(svcDir, stagingDirName)
	_ = os.RemoveAll(staging) // 上次中断的残留（启动也会清，双保险）

	// ──（升级）停服：同步等待进程完全退出（D11）──
	wasRunning := false
	if t.Mode == "upgrade" && s.Supervisor != nil {
		s.Tasks.updatePhase(t, PhaseStopping, "停止服务并等待退出")
		if rt, found := s.Supervisor.State(def.ID); found && isRunningState(rt.State) {
			wasRunning = true
			if !s.Supervisor.Stop(def.ID) {
				return fmt.Errorf("停止指令未被受理（引擎忙）")
			}
			if err := waitServiceStopped(s, def.ID, 30*time.Second); err != nil {
				return err
			}
		}
		// Windows：进程退出后句柄释放有延迟，用「可重命名」做可写性探测。
		if err := waitWritable(filepath.Join(svcDir, filepath.FromSlash(m.Entrypoint))); err != nil {
			return err
		}
	}

	// ── 解包到 .staging（逐文件 sha256 全量校验）──
	s.Tasks.updatePhase(t, PhaseSwapping, "解包并校验文件")
	if err := os.MkdirAll(svcDir, 0o755); err != nil {
		return err
	}
	if _, err := p.ExtractTo(staging, false); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("解包校验失败: %w", err)
	}

	// ── 换文件 ──
	s.Tasks.updatePhase(t, PhaseSwapping, "切换文件（旧产物 → .old，配置 → .bak）")
	swapped, err := s.materialize(svcDir, staging, m, t.Mode)
	if err != nil {
		// 失败回滚已换入的文件（文件级可逆）。
		s.rollbackSwap(swapped)
		_ = os.RemoveAll(staging)
		return err
	}

	// ── 落库：安装 = 新建行；升级 = 更新行 ──
	if err := s.upsertServiceDef(&def, hasSvc, m, source); err != nil {
		s.rollbackSwap(swapped)
		_ = os.RemoveAll(staging)
		return err
	}

	// ── 数据目录（安装时创建；升级不触碰内容，E2）──
	if err := os.MkdirAll(dataDirOf(m.Name), 0o755); err != nil {
		logx.Module("deploy").Warn("创建数据目录失败", "svc", m.Name, "err", err)
	}

	// ── 发起启动（D10：指令发出即任务收尾，不等待运行结果）──
	if t.Mode == "upgrade" && wasRunning && def.Enabled && s.Supervisor != nil {
		s.Tasks.updatePhase(t, PhaseStarting, "发起启动")
		s.Supervisor.Start(def.ID)
	}

	// ── 清理：.old 成功即删；.bak 保留（用户配置备份，下次升级时被覆盖）。
	for _, f := range swapped {
		if strings.HasSuffix(f.OldPath, oldSuffix) {
			_ = os.Remove(f.OldPath)
		}
	}
	_ = os.RemoveAll(staging)

	s.logEvent("info", "upgrade", def.ID, m.Name,
		fmt.Sprintf("部署完成：%s %s → %s（source=%s）", m.Name, t.FromVer, m.Version, source))
	return nil
}

// swappedFile 记录一次换名动作（失败时反向执行）。
type swappedFile struct {
	DstPath  string // 正式位置（新文件现在的位置）
	OldPath  string // 旧文件被换到的位置（.old/.bak）
	HadOld   bool   // 正式位置原本有文件
}

// materialize 把 staging 内容换入正式位置。
// 返回已执行的换名记录（回滚依据）。
func (s *Server) materialize(svcDir, staging string, m *pack.Manifest, mode string) ([]swappedFile, error) {
	var swapped []swappedFile

	newPaths := make(map[string]pack.FileEntry, len(m.Files))
	for _, f := range m.Files {
		newPaths[f.Path] = f
	}

	err := filepath.WalkDir(staging, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(staging, path)
		if rerr != nil {
			return rerr
		}
		pkgPath := filepath.ToSlash(rel)
		entry, listed := newPaths[pkgPath]
		if !listed {
			return fmt.Errorf("staging 中出现未登记文件 %q", pkgPath)
		}
		dst := filepath.Join(svcDir, rel)

		// data 角色：目标已存在 → 保留旧数据（E2），新文件丢弃。
		if entry.Role == pack.RoleData {
			if _, err := os.Lstat(dst); err == nil {
				_ = os.Remove(path)
				return nil
			}
		}

		rec := swappedFile{DstPath: dst, OldPath: dst + oldSuffix}
		// 目标父目录可能不存在（首装/新增子目录）：rename 前先建。
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("创建目标目录失败 %s: %w", rel, err)
		}
		if _, err := os.Lstat(dst); err == nil {
			// 正式位置有旧文件 → 换名让位。
			oldPath := dst + oldSuffix
			if entry.Role == pack.RoleConfig {
				oldPath = dst + bakSuffix // 配置备份为 .bak（E3/E4）
			}
			rec.OldPath = oldPath
			rec.HadOld = true
			_ = os.Remove(oldPath) // 上次升级的残留（理论上启动已清）
			if err := os.Rename(dst, oldPath); err != nil {
				return fmt.Errorf("旧文件让位失败 %s: %w", rel, err)
			}
		}
		if err := os.Rename(path, dst); err != nil {
			return fmt.Errorf("新文件就位失败 %s: %w", rel, err)
		}
		swapped = append(swapped, rec)
		return nil
	})
	if err != nil {
		return swapped, err
	}

	// 升级模式：清理旧版独有、新版没有的 dependency 文件（防残留堆积）。
	if mode == "upgrade" {
		s.pruneStaleDependencies(svcDir, m)
	}

	// manifest.json 留存（当前版本清单快照，导出与排障用）。
	if raw, err := json.Marshal(m); err == nil {
		if err := os.WriteFile(filepath.Join(svcDir, "manifest.json"), raw, 0o644); err != nil {
			logx.Module("deploy").Warn("写 manifest 留存失败", "err", err)
		}
	}
	return swapped, nil
}

// pruneStaleDependencies 删除服务目录中不在新清单内的旧产物文件
//（仅清 bin/ dependencies/ config/ 前缀；data 目录不在 svcDir，天然不受影响）。
func (s *Server) pruneStaleDependencies(svcDir string, m *pack.Manifest) {
	keep := make(map[string]bool, len(m.Files))
	for _, f := range m.Files {
		keep[f.Path] = true
	}
	_ = filepath.WalkDir(svcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if path == filepath.Join(svcDir, "manifest.json") {
			return nil
		}
		rel, rerr := filepath.Rel(svcDir, path)
		if rerr != nil {
			return nil
		}
		p := filepath.ToSlash(rel)
		if keep[p] {
			return nil
		}
		base := filepath.Base(p)
		if strings.HasSuffix(base, oldSuffix) || strings.HasSuffix(base, bakSuffix) || strings.HasSuffix(base, ".part") {
			return nil // 过渡文件由各自的生命周期管理
		}
		if !strings.HasPrefix(p, "bin/") && !strings.HasPrefix(p, "dependencies/") {
			return nil // 只清产物前缀，其他未知文件不碰
		}
		if err := os.Remove(path); err == nil {
			logx.Module("deploy").Info("清理旧版独有文件", "file", p)
		}
		return nil
	})
}

// rollbackSwap 升级失败时把 .old/.bak 换回正式位置（尽力而为）。
func (s *Server) rollbackSwap(swapped []swappedFile) {
	// 逆序回滚（目录层次上深路径先回）。
	for i := len(swapped) - 1; i >= 0; i-- {
		rec := swapped[i]
		_ = os.Remove(rec.DstPath) // 换入失败的新文件
		if rec.HadOld {
			if err := os.Rename(rec.OldPath, rec.DstPath); err != nil {
				logx.Module("deploy").Error("回滚换名失败", "dst", rec.DstPath, "err", err)
			}
		}
	}
}

// upsertServiceDef 安装=新建、升级=更新 Service 行。
func (s *Server) upsertServiceDef(def *store.Service, hasSvc bool, m *pack.Manifest, source string) error {
	env := m.Env
	if env == nil {
		env = map[string]string{}
	}
	deps := make([]int, 0, len(m.Deps))
	for _, dep := range m.Deps {
		var d store.Service
		if s.DB.First(&d, "name = ?", dep).Error == nil {
			deps = append(deps, d.ID)
		}
	}
	// 依赖环检测（安装/升级写路径防御，新增节点入图）。
	if len(deps) > 0 {
		var all []store.Service
		s.DB.Order("id asc").Find(&all)
		if cycle := detectDependsCycle(all, 0, deps); cycle != "" {
			return fmt.Errorf("依赖形成环：%s", cycle)
		}
	}

	exePath := filepath.Join(s.installRoot(), m.Name, filepath.FromSlash(m.Entrypoint))
	manifestRaw, _ := json.Marshal(struct {
		*pack.Manifest
		Source string `json:"source"`
	}{m, source})

	if !hasSvc {
		def = &store.Service{
			Name: m.Name, Version: m.Version, Summary: firstNonEmpty(m.DisplayName, m.Name),
			ServiceType: "native", WebUrl: strings.TrimSpace(m.WebUrl),
			ExePath: exePath, WorkDir: dataDirOf(m.Name),
			Args: mustJSON(nonNil(m.Args)), Env: mustJSON(env),
			DependsOn: mustJSON(deps), Enabled: true, AutoStart: false,
			InstalledAt: time.Now(), Manifest: datatypesJSON(manifestRaw),
		}
		def.RestartPolicy, def.HealthCheck, def.ConfigFile, def.LogPolicy = defaultPolicyBlobs()
		applyManifestPolicies(def, m)
	} else {
		def.Version = m.Version
		def.ExePath = exePath
		def.WorkDir = dataDirOf(m.Name)
		if len(m.Args) > 0 {
			def.Args = mustJSON(m.Args)
		}
		if len(env) > 0 {
			def.Env = mustJSON(env)
		}
		def.DependsOn = mustJSON(deps)
		def.InstalledAt = time.Now()
		def.Manifest = datatypesJSON(manifestRaw)
		applyManifestPolicies(def, m)
	}
	if err := s.DB.Save(def).Error; err != nil {
		return fmt.Errorf("落库失败: %w", err)
	}
	if s.Defs != nil {
		if hasSvc {
			s.Defs.OnDefsSaved(*def)
		} else {
			s.Defs.OnDefsSaved(*def)
		}
	}
	return nil
}

// applyManifestPolicies 包内运行策略覆盖默认值（仅非空字段生效）。
func applyManifestPolicies(def *store.Service, m *pack.Manifest) {
	if len(m.RestartPolicy) > 0 && string(m.RestartPolicy) != "null" {
		def.RestartPolicy = datatypesJSON(m.RestartPolicy)
	}
	if len(m.HealthCheck) > 0 && string(m.HealthCheck) != "null" {
		def.HealthCheck = datatypesJSON(m.HealthCheck)
	}
	if len(m.ConfigFile) > 0 && string(m.ConfigFile) != "null" {
		def.ConfigFile = datatypesJSON(m.ConfigFile)
	} else if hasConfigRole(m) {
		// 包携带 config 文件但未声明 configFile 策略：指向包内路径。
		for _, f := range m.Files {
			if f.Role == pack.RoleConfig {
				def.ConfigFile = mustJSON(map[string]any{
					"path": filepath.Join(m.Name, filepath.Base(f.Path)),
					"validateCmd": "", "applyMode": "restart", "applyCmd": "", "maxHistory": 10,
				})
				break
			}
		}
	}
	if len(m.LogPolicy) > 0 && string(m.LogPolicy) != "null" {
		def.LogPolicy = datatypesJSON(m.LogPolicy)
	}
}

func hasConfigRole(m *pack.Manifest) bool {
	for _, f := range m.Files {
		if f.Role == pack.RoleConfig {
			return true
		}
	}
	return false
}

/* ═══════════════════ 停止等待与可写性探测 ═══════════════════ */

// isRunningState 判断监管态是否"进程在跑"。
func isRunningState(st string) bool {
	switch st {
	case "running", "degraded", "backoff", "starting", "waiting", "upgrading", "stopping":
		return true
	}
	return false
}

// waitServiceStopped 轮询等待服务到达 stopped/failed/disabled（D11：完全退出）。
func waitServiceStopped(s *Server, id int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.Supervisor != nil {
			if rt, found := s.Supervisor.State(id); found {
				if !isRunningState(rt.State) {
					return nil
				}
			} else {
				return nil // 无监管态视为已停
			}
		} else {
			return nil // 无引擎（测试形态）
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("等待服务退出超时（%s）", timeout)
}

// waitWritable Windows 可写性探测：进程退出后句柄释放存在毫秒级延迟，
// 用「能否重命名」判定文件已不被占用（实测：Windows 覆盖运行中 exe 失败、
// 重命名成功）。最多重试 10 次 × 200ms。
func waitWritable(path string) error {
	if goruntime.GOOS != "windows" {
		return nil // Linux 无此问题
	}
	if _, err := os.Lstat(path); err != nil {
		return nil // 文件不存在（首装），无需探测
	}
	probe := path + ".wprobe"
	for i := 0; i < 10; i++ {
		_ = os.Remove(probe)
		if err := os.Rename(path, probe); err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if err := os.Rename(probe, path); err != nil {
			return fmt.Errorf("探测回位失败 %s: %w", path, err)
		}
		return nil
	}
	return fmt.Errorf("文件仍被占用（等待句柄释放超时）: %s", path)
}

/* ═══════════════════ 断电恢复（平台启动时） ═══════════════════ */

// RecoverInterruptedDeploys 清理升级中断残留（decisions.md §11）：
//   - .staging：解包未完成 → 整目录删除（正式文件未被触碰或已可独立回滚）
//   - .old：换名后未及删除 → 删除（新文件已就位，.old 只是旧副本）
//   - .wprobe：可写性探测残留 → 删除
//
// .bak 保留：它是被覆盖前的用户配置备份，不属于中断残留。
func (s *Server) RecoverInterruptedDeploys() {
	root := s.installRoot()
	if _, err := os.Stat(root); err != nil {
		return
	}
	cleaned := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 尽力而为
		}
		name := d.Name()
		if d.IsDir() && name == stagingDirName {
			if err := os.RemoveAll(path); err == nil {
				cleaned++
			}
			return filepath.SkipDir
		}
		if !d.IsDir() && (strings.HasSuffix(name, oldSuffix) || strings.HasSuffix(name, ".wprobe")) {
			if err := os.Remove(path); err == nil {
				cleaned++
			}
		}
		return nil
	})
	if cleaned > 0 {
		logx.Module("deploy").Info("清理升级中断残留", "count", cleaned)
	}
}

/* ═══════════════════ 小工具 ═══════════════════ */

// dataDirOf 服务数据目录 data/{name}（P2：数据与产物分离）。
func dataDirOf(name string) string { return config.DataDir(name) }

// logEvent 升级域事件（EventLog type=upgrade，供 /api/upgrade/records）。
func (s *Server) logEvent(level, typ string, svcID int, name, msg string) {
	logger := logx.Module("deploy")
	switch level {
	case "error":
		logger.Error(msg, "id", svcID, "service", name, "type", typ)
	case "warn":
		logger.Warn(msg, "id", svcID, "service", name, "type", typ)
	default:
		logger.Info(msg, "id", svcID, "service", name, "type", typ)
	}
	_ = s.DB.Create(&store.EventLog{
		Type: typ, Level: level, ServiceID: svcID, Message: msg, CreatedAt: time.Now(),
	}).Error
}
