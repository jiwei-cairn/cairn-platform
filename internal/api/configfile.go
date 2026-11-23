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
// 保存语义：先备份（{file}.bak.{unix}，按 configFile.maxHistory 保留最近 N 份）
// → 原子写（.tmp + rename）。applyMode=restart 时由调用方决定是否重启。
package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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
	MaxHistory  int    `json:"maxHistory"`
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
	isAbs := strings.HasPrefix(p, "/") || (len(p) > 1 && p[1] == ':')
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
	MaxHistory  int    `json:"maxHistory"`
}

// parseConfigFile 解析 configFile blob（缺失字段给安全默认）。
func parseConfigFile(raw []byte) configFilePolicy {
	var p configFilePolicy
	_ = jsonUnmarshalLenient(raw, &p)
	if p.ApplyMode == "" {
		p.ApplyMode = "restart"
	}
	if p.MaxHistory <= 0 {
		p.MaxHistory = 10
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
		ApplyMode:  pol.ApplyMode, ValidateCmd: pol.ValidateCmd, MaxHistory: pol.MaxHistory,
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

	// 备份（尽力而为：备份失败不阻断保存，只告警——保住用户输入优先）。
	bak := makeBackup(abs)
	if bak != "" {
		s.pruneConfigBackups(abs, parseConfigFile(def.ConfigFile).MaxHistory)
	}

	// 原子写。
	if err := writeFileAtomicPath(abs, []byte(req.Content), cur.Mode().Perm()); err != nil {
		fail(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	st, _ := os.Stat(abs)
	logx.Module("api").Info("保存配置文件", "svc", def.Name, "path", rel, "bytes", len(req.Content), "backup", bak)
	s.logEvent("info", "settings", def.ID, def.Name,
		fmt.Sprintf("配置文件已保存：%s（%d 字节）", rel, len(req.Content)))

	ok(w, map[string]any{
		"path": rel, "sizeBytes": st.Size(),
		"modifiedAt": st.ModTime().UnixMilli(), "backup": bak,
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

// backupEntry 一条备份记录（历史列表用）。
type backupEntry struct {
	Name  string `json:"name"`
	At    int64  `json:"at"`
	Bytes int64  `json:"bytes"`
}

// ConfigfileHistory GET /api/services/{id}/configfile/history：版本历史（备份列表）。
func (s *Server) ConfigfileHistory(w http.ResponseWriter, r *http.Request) {
	def, okDef := s.loadServiceForConfig(w, r)
	if !okDef {
		return
	}
	abs, rel, _, err := s.locateConfigFile(def)
	if err != nil {
		// 历史不需要文件存在（可能刚被删）：按配置路径推断目录。
		cfg := parseConfigFile(def.ConfigFile)
		if strings.TrimSpace(cfg.Path) == "" {
			ok(w, []backupEntry{})
			return
		}
		abs = filepath.Join(s.installRoot(), def.Name, filepath.FromSlash(cfg.Path))
		rel = cfg.Path
	}
	_ = rel
	entries := listConfigBackups(abs)
	ok(w, entries)
}

/* ══════════════ 内部工具 ══════════════ */

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

// makeBackup 备份为 {file}.bak.{unix}，返回备份路径（失败返回空串，不阻断保存）。
func makeBackup(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	bak := fmt.Sprintf("%s.bak.%d", path, time.Now().Unix())
	if err := os.WriteFile(bak, raw, st.Mode().Perm()); err != nil {
		logx.Module("api").Warn("配置文件备份失败", "path", path, "err", err)
		return ""
	}
	return bak
}

// listConfigBackups 列出 {file}.bak.{unix} 备份（按时间倒序）。
func listConfigBackups(path string) []backupEntry {
	dir := filepath.Dir(path)
	prefix := filepath.Base(path) + ".bak."
	ents, err := os.ReadDir(dir)
	if err != nil {
		return []backupEntry{}
	}
	out := make([]backupEntry, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		suffix := strings.TrimPrefix(e.Name(), prefix)
		unix, err := strconv.ParseInt(suffix, 10, 64)
		if err != nil {
			continue
		}
		info, iErr := e.Info()
		var size int64
		if iErr == nil {
			size = info.Size()
		}
		out = append(out, backupEntry{Name: e.Name(), At: unix * 1000, Bytes: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out
}

// pruneConfigBackups 按 maxHistory 保留最近 N 份备份。
func (s *Server) pruneConfigBackups(path string, maxHistory int) {
	if maxHistory <= 0 {
		return
	}
	entries := listConfigBackups(path)
	if len(entries) <= maxHistory {
		return
	}
	dir := filepath.Dir(path)
	for _, e := range entries[maxHistory:] {
		_ = os.Remove(filepath.Join(dir, e.Name))
	}
}

// writeFileAtomicPath 原子写（.tmp + rename），保留原文件权限。
func writeFileAtomicPath(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
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
