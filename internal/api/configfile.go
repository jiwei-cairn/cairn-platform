// configfile.go — 被管服务配置文件管理：读 / 保存（含备份）/ 校验 / 生效 / 历史。
//
// 文件定位（与启动参数解析同一语义，见 runtime.ResolveArgs）：
//  1. 数据目录 data/{name}/{cfgPath} 存在 → 用它（服务自改的配置优先，尊重 P2）
//  2. 否则产物目录 services/{os}/{arch}/{name}/{cfgPath} → 用它（随包发行）
//  3. 都不存在 → 404，提示先安装带配置的包
//
// 安全边界：
//   - 路径穿越：解析后必须落在上述两个根之一（pathWithin 校验）
//   - 大小上限：读取/保存均 ≤ 1MB（配置是文本，不是日志/数据）
//   - 二进制拒绝：非文本内容只读（editable=false），避免编辑器把二进制写坏
//
// 保存语义：原子写（.tmp + rename），不保留多份历史。
// 启动验证通过后另存一份 {file}.lastgood，供一键恢复。
// applyMode=restart 由调用方决定是否重启；command 未实现。
package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/store"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
)

// maxConfigBytes 配置文件大小上限（读取与保存）。
const maxConfigBytes = 1 << 20 // 1MB

// configFileView GET /api/services/{id}/configfile 响应。
type configFileView struct {
	Path        string `json:"path"`     // 相对服务根的路径（config/app.conf）
	AbsPath     string `json:"absPath"`  // 实际读取位置（排障用）
	Location    string `json:"location"` // data | product
	Content     string `json:"content"`  // 文件内容
	SizeBytes   int64  `json:"sizeBytes"`
	ModifiedAt  int64  `json:"modifiedAt"` // 毫秒时间戳（保存时回传做冲突检测）
	Editable    bool   `json:"editable"`   // 非文本内容为 false
	ApplyMode   string `json:"applyMode"`  // restart | command
	ValidateCmd string `json:"validateCmd"`
}

// normalizeCfgPath 把 configFile.path 归一为「相对服务根的包内路径」。
//
// 历史数据里 path 出现过多种形态（实测见用户报错：services/windows/amd64/
// nats-server/config/nats-server.conf）：
//  1. 相对路径 config/app.conf                     → 原样
//  2. 绝对路径 E:\...\nats-server\config\x         → 取服务名目录之后的部分
//  3. 相对但含 services 前缀 services/{os}/{arch}/{name}/config/x → 同样剥到 config/
//  4. 带服务名前缀 nats-server/config/x             → 去掉前缀（deploy 旧实现写法）
//
// 平台可能跨系统处理历史数据（Windows 上的库记录着 POSIX 绝对路径），
// 因此绝对性判定用「字符串形态」而非 filepath.IsAbs——后者按当前 GOOS 解释，
// 对 /opt/... 在 Windows 上返回 false。
//
// 归一失败（无法判定）时返回清理后的值，由调用方的穿越校验兜底拒绝。
func normalizeCfgPath(raw, svcName string) string {
	p := strings.TrimSpace(raw)
	if p == "" {
		return ""
	}
	// 统一分隔符：Windows 的反斜杠先归一为 /，后续全按 POSIX 形态处理。
	p = strings.ReplaceAll(p, `\`, "/")

	// 形态 2/3：绝对路径，或相对但带 services 段 / 服务名段的仓库路径。
	// 盘符判定要求首字符是字母且后随 /：跨平台读历史数据（Windows 库里的
	// "C:/..."）需识别，但 Linux 上 "a:b" 这类相对路径不能误判为绝对路径。
	isAbs := strings.HasPrefix(p, "/") ||
		(len(p) > 2 && isASCIILetter(p[0]) && p[1] == ':' && (p[2] == '/' || p[2] == '\\'))
	hasRepoPrefix := strings.HasPrefix(p, "services/") || strings.Contains(p, "/services/")
	if isAbs || hasRepoPrefix {
		// 优先按服务名定位（最可靠：服务根目录名就是服务名）。
		if i := strings.LastIndex(p, "/"+svcName+"/"); i >= 0 {
			p = p[i+len(svcName)+2:]
		} else if strings.HasPrefix(p, svcName+"/") {
			p = strings.TrimPrefix(p, svcName+"/")
		} else if k := strings.LastIndex(p, "/config/"); k >= 0 {
			p = p[k+1:] // 至少保住 config/ 段
		} else if j := strings.LastIndex(p, "/"); j >= 0 {
			p = p[j+1:]
		}
	}

	// 形态 4：以服务名打头的相对路径 → 去掉该前缀。
	if svcName != "" && strings.HasPrefix(p, svcName+"/") {
		p = strings.TrimPrefix(p, svcName+"/")
	}
	return filepath.ToSlash(filepath.Clean(p))
}

// isASCIILetter 判断是否为 ASCII 字母（盘符检测用）。
func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// configLocation 解析配置文件的落点。
//
// 返回绝对路径与位置标记（data / product）。cfgPath 相对服务根。
// 两个根都不存在该文件时返回 os.ErrNotExist 包装的错误。
func (s *Server) locateConfigFile(def *store.Service) (absPath, relPath, location string, err error) {
	cfg := parseConfigFile(def.ConfigFile)
	rel := normalizeCfgPath(cfg.Path, def.Name)
	if rel == "" {
		return "", "", "", fmt.Errorf("该服务未配置配置文件路径（configFile.path 为空）")
	}
	// 归一后仍是相对路径；拒绝绝对路径与穿越（双重保险）。
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", "", "", fmt.Errorf("配置文件路径不合法: %s", cfg.Path)
	}

	roots := []struct {
		base string
		kind string
	}{
		{dataDirOf(def.Name), "data"},
		{filepath.Join(s.installRoot(), def.Name), "product"},
	}
	for _, r := range roots {
		if r.base == "" {
			continue
		}
		cand := filepath.Join(r.base, filepath.FromSlash(rel))
		// 纵深防御：候选路径必须落在该根之内。
		if !pathWithin(filepath.Clean(r.base), filepath.Clean(cand)) {
			continue
		}
		if st, statErr := os.Stat(cand); statErr == nil && st.Mode().IsRegular() {
			return cand, rel, r.kind, nil
		}
	}
	return "", rel, "", os.ErrNotExist
}

// configFilePolicy configFile JSON blob 的取用子集。
type configFilePolicy struct {
	Path        string `json:"path"`
	ValidateCmd string `json:"validateCmd"`
	ApplyMode   string `json:"applyMode"`
	ApplyCmd    string `json:"applyCmd"`
}

// parseConfigFile 解析 configFile blob（缺失字段给安全默认）。
func parseConfigFile(raw []byte) configFilePolicy {
	var p configFilePolicy
	_ = jsonUnmarshalLenient(raw, &p)
	if p.ApplyMode == "" {
		p.ApplyMode = "restart"
	}
	return p
}

// loadServiceForConfig 按 id 取服务定义（统一 404/500 处理）。
func (s *Server) loadServiceForConfig(w http.ResponseWriter, r *http.Request) (*store.Service, bool) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		fail(w, http.StatusBadRequest, "无效的 ID")
		return nil, false
	}
	var def store.Service
	if err := s.DB.First(&def, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			fail(w, http.StatusNotFound, "服务不存在")
		} else {
			fail(w, http.StatusInternalServerError, err.Error())
		}
		return nil, false
	}
	return &def, true
}

// GetConfigfile GET /api/services/{id}/configfile：读取配置文件内容。
func (s *Server) GetConfigfile(w http.ResponseWriter, r *http.Request) {
	def, okDef := s.loadServiceForConfig(w, r)
	if !okDef {
		return
	}
	abs, rel, loc, err := s.locateConfigFile(def)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fail(w, http.StatusNotFound, fmt.Sprintf(
				"配置文件不存在：%s（已查找 data/%s/ 与产物目录）", rel, def.Name))
			return
		}
		fail(w, http.StatusConflict, err.Error())
		return
	}
	st, err := os.Stat(abs)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if st.Size() > maxConfigBytes {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"配置文件过大（%d 字节，上限 %d）：可在服务详情中调整后手工编辑", st.Size(), maxConfigBytes))
		return
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	pol := parseConfigFile(def.ConfigFile)
	ok(w, configFileView{
		Path: rel, AbsPath: abs, Location: loc,
		Content: string(raw), SizeBytes: st.Size(),
		ModifiedAt: st.ModTime().UnixMilli(),
		Editable:   isTextContent(raw),
		ApplyMode:  pol.ApplyMode, ValidateCmd: pol.ValidateCmd,
	})
}

// putConfigfileReq PUT 请求体。
type putConfigfileReq struct {
	Content    string `json:"content"`
	ModifiedAt int64  `json:"modifiedAt"` // 客户端读到的 mtime（冲突检测；0 = 不检查）
	DryRun     bool   `json:"dryRun"`     // 只校验不落盘
}

// PutConfigfile PUT /api/services/{id}/configfile：保存（备份 + 原子写）。
func (s *Server) PutConfigfile(w http.ResponseWriter, r *http.Request) {
	def, okDef := s.loadServiceForConfig(w, r)
	if !okDef {
		return
	}
	var req putConfigfileReq
	if err := jsonDecodeBody(r, &req); err != nil {
		fail(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}
	if int64(len(req.Content)) > maxConfigBytes {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("内容过大（上限 %d 字节）", maxConfigBytes))
		return
	}
	abs, rel, _, err := s.locateConfigFile(def)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fail(w, http.StatusNotFound, fmt.Sprintf("配置文件不存在：%s", rel))
			return
		}
		fail(w, http.StatusConflict, err.Error())
		return
	}

	cur, err := os.Stat(abs)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 冲突检测：磁盘上文件的 mtime 与客户端读到的不同 → 已被外部修改。
	if req.ModifiedAt > 0 && cur.ModTime().UnixMilli() != req.ModifiedAt {
		fail(w, http.StatusConflict,
			"文件已被外部修改（读取后 mtime 变化），请关闭弹窗重新打开后再编辑")
		return
	}

	// dryRun：只做校验（写临时文件跑 validateCmd），不落盘。
	if req.DryRun {
		out, vErr := s.validateConfigContent(def, req.Content)
		if vErr != nil {
			ok(w, map[string]any{"ok": false, "output": out})
			return
		}
		ok(w, map[string]any{"ok": true, "output": out})
		return
	}

	// P1-2 定案：保存不留档（无 .bak）——唯一的备份是 lastgood 快照，
	// 由服务启动验证通过时固化。改坏了靠「恢复」回最后正常配置。

	// 原子写。
	if err := writeFileAtomicPath(abs, []byte(req.Content), cur.Mode().Perm()); err != nil {
		fail(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	st, _ := os.Stat(abs)
	logx.Module("api").Info("保存配置文件", "svc", def.Name, "path", rel, "bytes", len(req.Content))
	s.logEvent("info", "settings", def.ID, def.Name,
		fmt.Sprintf("配置文件已保存：%s（%d 字节）", rel, len(req.Content)))

	ok(w, map[string]any{
		"path": rel, "sizeBytes": st.Size(),
		"modifiedAt": st.ModTime().UnixMilli(),
	})
}

// RestoreConfigfile POST /api/services/{id}/configfile/restore：
// 恢复最后正常配置（P1-2 定案：一键回到 lastgood 快照）。
//
// 语义 = 「保存 + 生效」：快照内容写回配置文件 → 重启服务。
// 恢复不额外留档（lastgood 本身就是权威副本，恢复动作不产生新备份）。
// 快照不存在（服务从未成功启动过）→ 409 说明情况。
func (s *Server) RestoreConfigfile(w http.ResponseWriter, r *http.Request) {
	def, okDef := s.loadServiceForConfig(w, r)
	if !okDef {
		return
	}
	abs, rel, _, err := s.locateConfigFile(def)
	if err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	snap := lastgoodPath(abs)
	raw, err := os.ReadFile(snap)
	if err != nil {
		fail(w, http.StatusConflict,
			"没有可恢复的快照（服务从未成功启动过，或配置未随包提供）")
		return
	}
	cur, err := os.Stat(abs)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := writeFileAtomicPath(abs, raw, cur.Mode().Perm()); err != nil {
		fail(w, http.StatusInternalServerError, "恢复失败: "+err.Error())
		return
	}
	st, _ := os.Stat(abs)
	logx.Module("api").Info("恢复最后正常配置", "svc", def.Name, "path", rel, "bytes", len(raw))
	s.logEvent("info", "settings", def.ID, def.Name,
		fmt.Sprintf("配置已恢复为最后正常版本：%s（%d 字节）", rel, len(raw)))

	// 生效：与「保存」同语义——写完即重启（配置在进程启动时读取）。
	if s.Supervisor == nil {
		ok(w, map[string]any{"restored": true, "restarted": false, "note": "已写回配置；监管引擎未装配，未重启"})
		return
	}
	if !s.Supervisor.Restart(def.ID) {
		fail(w, http.StatusServiceUnavailable,
			"配置已恢复，但重启未被受理（引擎忙），请在服务卡片手动重启")
		return
	}
	ok(w, map[string]any{
		"restored": true, "restarted": true,
		"path": rel, "sizeBytes": st.Size(),
		"modifiedAt": st.ModTime().UnixMilli(),
	})
}

// ValidateConfigfile POST /api/services/{id}/configfile/validate：dryRun 校验。
func (s *Server) ValidateConfigfile(w http.ResponseWriter, r *http.Request) {
	def, okDef := s.loadServiceForConfig(w, r)
	if !okDef {
		return
	}
	var req putConfigfileReq
	if err := jsonDecodeBody(r, &req); err != nil {
		fail(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}
	out, err := s.validateConfigContent(def, req.Content)
	if err != nil {
		ok(w, map[string]any{"ok": false, "output": out})
		return
	}
	ok(w, map[string]any{"ok": true, "output": out})
}

// ApplyConfigfile POST /api/services/{id}/configfile/apply：按 applyMode 生效。
//
// restart：重启服务（配置在进程启动时读取）。
// command：执行 applyCmd（服务支持热重载时用）。
// 两者都不配：返回提示，由调用方决定。
func (s *Server) ApplyConfigfile(w http.ResponseWriter, r *http.Request) {
	def, okDef := s.loadServiceForConfig(w, r)
	if !okDef {
		return
	}
	pol := parseConfigFile(def.ConfigFile)
	switch pol.ApplyMode {
	case "restart":
		if s.Supervisor == nil {
			fail(w, http.StatusServiceUnavailable, "监管引擎未装配，无法重启")
			return
		}
		if !s.Supervisor.Restart(def.ID) {
			fail(w, http.StatusServiceUnavailable, "重启指令未被受理（引擎忙），请稍后重试")
			return
		}
		s.logEvent("info", "restart", def.ID, def.Name, "配置生效：已发起重启")
		ok(w, map[string]any{"mode": "restart", "queued": true})
	case "command":
		if strings.TrimSpace(pol.ApplyCmd) == "" {
			fail(w, http.StatusConflict, "applyMode=command 但未配置 applyCmd")
			return
		}
		fail(w, http.StatusNotImplemented,
			"applyMode=command 尚未实现（需在服务工作目录执行任意命令，安全边界待评审）；"+
				"当前请改用 applyMode=restart，或手工执行: "+pol.ApplyCmd)
	default:
		fail(w, http.StatusConflict, "未知的 applyMode: "+pol.ApplyMode)
	}
}

// lastgoodView 一份快照的展示信息（前端「最后正常配置」区展示唯一一份）。
type lastgoodView struct {
	Exists bool  `json:"exists"`
	At     int64 `json:"at"` // 快照时刻（毫秒）
	Bytes  int64 `json:"bytes"`
}

// ConfigfileHistory GET /api/services/{id}/configfile/history：
// 最后正常配置快照信息（P1-2 定案：单份 lastgood，取代 N 份 .bak 历史）。
func (s *Server) ConfigfileHistory(w http.ResponseWriter, r *http.Request) {
	def, okDef := s.loadServiceForConfig(w, r)
	if !okDef {
		return
	}
	abs, _, _, err := s.locateConfigFile(def)
	view := lastgoodView{}
	if err == nil {
		if st, statErr := os.Stat(lastgoodPath(abs)); statErr == nil && st.Mode().IsRegular() {
			view.Exists = true
			view.At = st.ModTime().UnixMilli()
			view.Bytes = st.Size()
		}
	}
	ok(w, view)
}

/* ══════════════ lastgood 快照（P1-2 定案：单份快照 + 一键恢复） ══════════════
 *
 * 业务闭环（需求定稿）：
 *   - 快照时机：服务启动验证通过（starting→running 迁移）——此刻的配置
 *     被 L2 验证证明"能跑"，固化为唯一备份 {file}.lastgood；
 *   - 保存：用户自由修改→保存→重启；起不来就继续改（无限次重试）；
 *   - 恢复：一直改不好时点「恢复」→ 快照写回 → 重启 → 恢复运行；
 *   - 不保留多份历史，保存动作本身不留档。
 */

// lastgoodSuffix 快照文件后缀（服务根目录内，与配置文件同目录）。
const lastgoodSuffix = ".lastgood"

// lastgoodPath 快照路径：{配置文件}.lastgood。
func lastgoodPath(configFile string) string {
	return configFile + lastgoodSuffix
}

// SnapshotLastgood 固化快照：把当前配置文件复制为 {file}.lastgood。
//
// 由 Engine.OnRunning 钩子调用（服务 starting→running 迁移时）——api 层
// 订阅监管事件，supervisor 不懂配置文件（依赖方向：本函数属于 api 层）。
//
// 幂等：每次启动成功都会覆盖（快照永远对齐"最后一次正常启动"）。
// 尽力而为：快照失败只告警，绝不影响状态迁移（监管主流程优先）。
func (s *Server) SnapshotLastgood(serviceID int, name string) {
	if serviceID <= 0 || strings.TrimSpace(name) == "" {
		logx.Module("api").Warn("快照钩子参数无效", "svc", name, "id", serviceID)
		return
	}
	var def store.Service
	if err := s.DB.First(&def, "id = ?", serviceID).Error; err != nil {
		logx.Module("api").Warn("快照钩子：服务定义查询失败", "svc", name, "err", err)
		return
	}
	abs, _, _, err := s.locateConfigFile(&def)
	if err != nil {
		logx.Module("api").Warn("快照钩子：配置文件定位失败", "svc", name, "err", err)
		return
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		logx.Module("api").Warn("lastgood 快照读取失败", "svc", name, "err", err)
		return
	}
	st, err := os.Stat(abs)
	if err != nil {
		return
	}
	dst := lastgoodPath(abs)
	if err := writeFileAtomicPath(dst, raw, st.Mode().Perm()); err != nil {
		logx.Module("api").Warn("lastgood 快照写入失败", "svc", name, "err", err)
		return
	}
	logx.Module("api").Info("已固化最后正常配置快照", "svc", name, "bytes", len(raw))
}

// isTextContent 判定内容是否可按文本编辑（NUL 字节或高比例控制字符视为二进制）。
func isTextContent(raw []byte) bool {
	if len(raw) == 0 {
		return true
	}
	sample := raw
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	ctrl := 0
	for _, b := range sample {
		if b == 0 {
			return false // NUL：二进制
		}
		if b < 0x20 && b != '\n' && b != '\r' && b != '\t' {
			ctrl++
		}
	}
	return float64(ctrl)/float64(len(sample)) < 0.05
}

// writeFileAtomicPath 原子写（.tmp + rename），保留原文件权限。
//
// Linux 注意：os.WriteFile 的 perm 会被进程 umask 削减（如 umask 022 把 0666
// 变成 0644），而 rename 后新文件的权限就取自临时文件——因此必须显式 Chmod
// 复原为原文件权限，否则保存一次就把 0640 的报告类配置降级为 0644（可能
// 造成敏感项被同机其他用户读取）。Windows 上 chmod 是无害空操作。
func writeFileAtomicPath(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	// umask 补偿：显式设定目标权限（失败不阻断——保存成功优先）。
	if err := os.Chmod(tmp, perm); err != nil {
		logx.Module("api").Warn("调整临时文件权限失败（可能受 umask 影响）", "path", tmp, "perm", perm.String(), "err", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// validateConfigContent 把内容写到临时文件后执行 validateCmd（未配置则跳过）。
func (s *Server) validateConfigContent(def *store.Service, content string) (string, error) {
	pol := parseConfigFile(def.ConfigFile)
	cmdline := strings.TrimSpace(pol.ValidateCmd)
	if cmdline == "" {
		return "该服务未配置 validateCmd，已跳过校验（可直接保存并重启生效）", nil
	}
	abs, _, _, err := s.locateConfigFile(def)
	if err != nil {
		return "", fmt.Errorf("配置文件不存在，无法校验: %w", err)
	}
	// 写到内容相同的临时文件，validateCmd 里的 {file} 占位替换为该路径。
	tmp, err := os.CreateTemp(filepath.Dir(abs), "cfgvalidate-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	runCmd := strings.ReplaceAll(cmdline, "{file}", tmpPath)
	out, err := runShellLimited(runCmd, map[string]string{
		"CAIRN_SERVICE": def.Name,
		"CAIRN_WORKDIR": def.WorkDir,
	}, def.WorkDir, 30*time.Second)
	if err != nil {
		return out, fmt.Errorf("校验未通过: %w", err)
	}
	return out, nil
}
