// deploy.go — 部署流水线：安装与升级共用（decisions.md C3/D5/D11）。
//
// 覆盖式换文件的三段保护：
//
//	.staging  解包过渡目录（断电 → 启动时清理）
//	.old      被替换的旧产物（Windows 无法覆盖运行中 exe，只能换名；
//	          升级成功即删，启动兜底清理残留）
//	.bak      被新版配置覆盖的用户配置（仅一份，下次升级时被覆盖）
//
// data 角色文件若目标已存在则保留不覆盖（E2：旧数据留存不丢弃）。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jiwei-cairn/cairn-pkg/pack"
	"github.com/jiwei-cairn/cairn-platform/internal/buildinfo"
	"github.com/jiwei-cairn/cairn-platform/internal/config"
	"github.com/jiwei-cairn/cairn-platform/internal/hostmetrics"
	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/store"

	goruntime "runtime"
)

// stagingDirName / oldSuffix / bakSuffix 约定的过渡文件标记。
const (
	stagingDirName = ".staging"
	oldSuffix      = ".old"
	bakSuffix      = ".bak"
)

// precheckDeploy 包级预检：平台匹配 / 平台版本下限 /（minDiskMB>0 时）磁盘余量。
// 名称冲突与服务存在性由调用方按 install/upgrade 模式分别判定。
func (s *Server) precheckDeploy(m *pack.Manifest) error {
	if m.Os != goruntime.GOOS {
		return fmt.Errorf("包目标平台 %s 与本机 %s 不符", m.Os, goruntime.GOOS)
	}
	if m.Arch != goruntime.GOARCH {
		return fmt.Errorf("包目标架构 %s 与本机 %s 不符", m.Arch, goruntime.GOARCH)
	}

	// ── 平台版本下限（package-format.md §4 minPlatformVersion）──
	// 语义化比较：本机平台版本 < 包要求 → 拒绝。生产构建经 ldflags 注入
	// git describe 版本串（v1.2.0-3-gabc 形态剥掉 v 前缀即为合法 semver，
	// 预发布段参与比较）；dev 等不可解析的版本串跳过门控并留痕，不因
	// 开发形态阻断部署。
	if req := strings.TrimSpace(m.MinPlatformVersion); req != "" {
		pv := strings.TrimPrefix(strings.TrimSpace(buildinfo.Version), "v")
		if !pack.IsSemVer(pv) {
			logx.Module("deploy").Warn("平台版本不可解析，跳过 minPlatformVersion 门控",
				"platformVersion", buildinfo.Version, "required", req)
		} else if pack.MustCompareSemVer(pv, req) < 0 {
			return fmt.Errorf("平台版本 %s 低于包要求的最低平台版本 %s，拒绝安装",
				buildinfo.Version, req)
		}
	}

	// ── 磁盘余量（minDiskMB>0 时按数据根所在卷精确校验）──
	if m.MinDiskMB > 0 {
		// 数据根目录可能尚未创建（理论仅在测试/异常路径；生产中 store.Open
		// 在启动时已建）——先确保存在，否则卷查询会因路径不存在而误报失败。
		if err := os.MkdirAll(config.DataDir(), 0o755); err != nil {
			return fmt.Errorf("数据目录不可写: %w", err)
		}
		need := uint64(m.MinDiskMB) << 20
		if free, _, err := hostmetrics.DiskFree(config.DataDir()); err == nil {
			if free < need {
				return fmt.Errorf("磁盘余量不足：需至少 %d MB，数据根所在卷仅剩 %d MB 可用",
					m.MinDiskMB, free>>20)
			}
		} else {
			// 卷信息不可得（挂载点异常等）：降级为仅可写探测并留痕，
			// 不静默放行，也不因探测手段缺失阻断部署。
			logx.Module("deploy").Warn("磁盘余量查询失败，退化为可写探测", "err", err)
		}
		// 可写探测保留：余量够 ≠ 有写权限（只读挂载 / 权限收紧）。
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
//
// 取消语义：每个阶段边界检查 ctx。任务超时后执行体会在此收尾退出，
// 保证「同一时刻只有一个流水线在动文件」，不会与后续任务并发写同一目录。
func (s *Server) runDeployPipeline(ctx context.Context, t *upgradeTask, pkgPath, source string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("任务已取消: %w", err)
	}
	s.Tasks.updatePhase(t, PhaseVerifying, "打开并校验包")
	p, err := pack.Open(pkgPath)
	if err != nil {
		return fmt.Errorf("包校验失败: %w", err)
	}
	defer p.Close()
	m := p.Manifest
	// ── 内容签名验证（F4 激活）：无签名放行（过渡态）；带签名必须通过信任列表
	//（编译内置 + TOFU：列表为空时从 OTA 服务器 /healthz 动态获取公钥）──
	if err := verifyContentSignatureDB(s.DB, m); err != nil {
		return err
	}

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
	if t.Mode == "upgrade" && pack.MustCompareSemVer(m.Version, def.Version) == 0 {
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
	// 解包是最长的一段 IO；取消检查放在其前的边界（换文件阶段不可中断）。
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("任务已取消: %w", err)
	}
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
	swapped, err := s.materialize(svcDir, staging, m)
	if err != nil {
		// 失败回滚已换入的文件（文件级可逆）。
		s.rollbackSwap(swapped)
		_ = os.RemoveAll(staging)
		return err
	}

	// ── 数据目录（安装时创建；升级不触碰内容，E2）──
	// 先于落库：失败时整体回滚文件、DB 不落任何行——否则升级场景 DB 已指向
	// 新版本而磁盘是旧文件，安装场景留下指向不存在 exePath 的幽灵行。
	if err := os.MkdirAll(dataDirOf(m.Name), 0o755); err != nil {
		s.rollbackSwap(swapped)
		_ = os.RemoveAll(staging)
		return fmt.Errorf("创建数据目录失败（服务将无法启动）: %w", err)
	}

	// ── 落库：安装 = 新建行；升级 = 更新行 ──
	// 这是部署的点之无归：成功后不再做任何需要回滚的可逆文件操作。
	if err := s.upsertServiceDef(&def, hasSvc, m, source); err != nil {
		s.rollbackSwap(swapped)
		_ = os.RemoveAll(staging)
		return err
	}

	// ── 升级：清理旧版独有、新版没有的产物文件 ──
	// 必须在落库成功后执行：删除不可逆，落库失败时回滚无需（也无法）还原它们。
	if t.Mode == "upgrade" {
		s.pruneStaleDependencies(svcDir, m)
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
	DstPath string // 正式位置（新文件现在的位置）
	OldPath string // 旧文件被换到的位置（.old/.bak）
	HadOld  bool   // 正式位置原本有文件
}

// materialize 把 staging 内容换入正式位置。
// 返回已执行的换名记录（回滚依据）。
func (s *Server) materialize(svcDir, staging string, m *pack.Manifest) ([]swappedFile, error) {
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

	// manifest.json 留存（当前版本清单快照，导出与排障用）。
	if raw, err := json.Marshal(m); err == nil {
		if err := os.WriteFile(filepath.Join(svcDir, "manifest.json"), raw, 0o644); err != nil {
			logx.Module("deploy").Warn("写 manifest 留存失败", "err", err)
		}
	}
	return swapped, nil
}

// pruneStaleDependencies 删除服务目录中不在新清单内的旧产物文件
// （仅清 bin/ dependencies/ config/ 前缀；data 目录不在 svcDir，天然不受影响）。
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
	selfID := 0
	if hasSvc {
		selfID = def.ID
	}
	deps, err := s.resolveDeps(m.Deps, selfID)
	if err != nil {
		return err
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
		s.Defs.OnDefsSaved(*def)
	}
	return nil
}

// applyManifestPolicies 包内运行策略覆盖默认值（仅非空字段生效）。
func applyManifestPolicies(def *store.Service, m *pack.Manifest) {
	if len(m.RestartPolicy) > 0 && string(m.RestartPolicy) != "null" {
		def.RestartPolicy = datatypesJSON(coerceRestartPolicy(m.RestartPolicy))
	}
	if len(m.HealthCheck) > 0 && string(m.HealthCheck) != "null" {
		def.HealthCheck = datatypesJSON(m.HealthCheck)
	}
	if len(m.ConfigFile) > 0 && string(m.ConfigFile) != "null" {
		def.ConfigFile = datatypesJSON(m.ConfigFile)
	} else if hasConfigRole(m) {
		// 包携带 config 文件但未声明 configFile 策略：指向包内相对路径。
		// 注意这里必须是包内路径（如 config/app.conf）——文件按 files[].path
		// 落盘在服务目录下，拼上服务名会得到 <svc>/cfgsvc/app.conf 这种不存在的
		// 路径（历史 bug：配置编辑器与启动参数解析都因此找不到文件）。
		for _, f := range m.Files {
			if f.Role == pack.RoleConfig {
				def.ConfigFile = configFileBlob(filepath.ToSlash(f.Path))
				logx.Module("deploy").Info("configFile 策略已按包内 config 角色生成", "svc", m.Name, "path", f.Path)
				break
			}
		}
	} else {
		logx.Module("deploy").Warn("applyManifestPolicies：包未声明 configFile 且无 config 角色文件",
			"svc", m.Name, "manifestConfigFile", string(m.ConfigFile), "files", len(m.Files))
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
	case "running", "degraded", "backoff", "starting", "waiting", "stopping":
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
