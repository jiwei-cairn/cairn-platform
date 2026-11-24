package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/store"
	"github.com/jiwei-cairn/cairn-platform/internal/svclog"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
)

// serviceRow 是 GET /api/services 与 GET /api/dashboard 的聚合行：
// 定义和运行态一次返回。
type serviceRow struct {
	ID             int               `json:"id"`
	Name           string            `json:"name"`
	Summary        string            `json:"summary"`
	ServiceType    string            `json:"serviceType"`
	WebUrl         string            `json:"webUrl"`
	ExePath        string            `json:"exePath"`
	Args           []string          `json:"args"`
	WorkDir        string            `json:"workDir"`
	Env            map[string]string `json:"env"`
	Enabled        bool              `json:"enabled"`
	AutoStart      bool              `json:"autoStart"`
	Priority       int               `json:"priority"`
	DependsOn      []int             `json:"dependsOn"`
	DependsOnNames []string          `json:"dependsOnNames"`
	RestartPolicy  json.RawMessage   `json:"restartPolicy"`
	HealthCheck    json.RawMessage   `json:"healthCheck"`
	ConfigFile     json.RawMessage   `json:"configFile"`
	LogPolicy      json.RawMessage   `json:"logPolicy"`
	Version        string            `json:"version"`
	Runtime        RunState          `json:"runtime"`
}

func (s *Server) serviceRows() []serviceRow {
	var defs []store.Service
	if err := s.DB.Order("id asc").Find(&defs).Error; err != nil {
		logx.Module("api").Warn("读取服务定义失败", "err", err)
		return nil
	}
	var states map[int]RunState
	if s.Supervisor != nil {
		states = s.Supervisor.States()
	}
	names := make(map[int]string, len(defs))
	for _, d := range defs {
		names[d.ID] = d.Name
	}

	rows := make([]serviceRow, 0, len(defs))
	for _, d := range defs {
		rows = append(rows, buildServiceRow(d, states, names))
	}
	return rows
}

func buildServiceRow(d store.Service, states map[int]RunState, names map[int]string) serviceRow {
	row := serviceRow{
		ID: d.ID, Name: d.Name, Summary: d.Summary, ServiceType: d.ServiceType, WebUrl: d.WebUrl,
		ExePath: d.ExePath, WorkDir: d.WorkDir, Enabled: d.Enabled, AutoStart: d.AutoStart,
		Priority: d.Priority,
		Args:     []string{},
		Env:      map[string]string{},
		Runtime:  stoppedState(),
	}
	_ = json.Unmarshal(d.Args, &row.Args)
	_ = json.Unmarshal(d.Env, &row.Env)
	if len(d.DependsOn) > 0 {
		_ = json.Unmarshal(d.DependsOn, &row.DependsOn)
	}
	for _, dep := range row.DependsOn {
		if n, ok := names[dep]; ok {
			row.DependsOnNames = append(row.DependsOnNames, n)
		} else {
			row.DependsOnNames = append(row.DependsOnNames, fmt.Sprintf("#%d", dep))
		}
	}
	row.RestartPolicy = json.RawMessage(d.RestartPolicy)
	row.HealthCheck = json.RawMessage(d.HealthCheck)
	row.ConfigFile = json.RawMessage(d.ConfigFile)
	row.LogPolicy = json.RawMessage(d.LogPolicy)

	row.Version = d.Version

	if states != nil {
		if rt, ok := states[d.ID]; ok {
			row.Runtime = rt
		}
	}
	if !d.Enabled {
		// 停用是定义层事实，覆盖监管态供列表展示。
		row.Runtime.State = "disabled"
		row.Runtime.UptimeSec = 0
	}
	return row
}

// ListServices 返回全部服务（定义 + 运行态聚合视图）。
func (s *Server) ListServices(w http.ResponseWriter, r *http.Request) {
	ok(w, s.serviceRows())
}

// servicePayload POST /api/services 的请求体（扁平字段 + 嵌套策略对象）。
type servicePayload struct {
	ID            int               `json:"id"`
	Name          string            `json:"name"`
	Summary       string            `json:"summary"`
	ServiceType   string            `json:"serviceType"`
	WebUrl        string            `json:"webUrl"`
	ExePath       string            `json:"exePath"`
	WorkDir       string            `json:"workDir"`
	Args          []string          `json:"args"`
	Env           map[string]string `json:"env"`
	Enabled       *bool             `json:"enabled"`
	AutoStart     bool              `json:"autoStart"`
	Priority      int               `json:"priority"`
	DependsOn     []int             `json:"dependsOn"`
	RestartPolicy json.RawMessage   `json:"restartPolicy"`
	HealthCheck   json.RawMessage   `json:"healthCheck"`
	ConfigFile    json.RawMessage   `json:"configFile"`
	LogPolicy     json.RawMessage   `json:"logPolicy"`
}

func mustJSON(v any) datatypesJSON {
	b, err := json.Marshal(v)
	if err != nil {
		return datatypesJSON("null")
	}
	return datatypesJSON(b)
}

// defaultPolicyBlobs 新建时缺省的策略对象（与前端 collectServiceForm 默认一致）。
func defaultPolicyBlobs() (restart, health, cfg, logp datatypesJSON) {
	return mustJSON(map[string]any{"mode": "backoff", "maxRetries": 0, "windowSec": 0, "backoffBase": 2, "backoffMax": 60}),
		mustJSON(map[string]any{"type": "none", "target": "", "intervalSec": 10, "timeoutSec": 3, "readyHoldSec": 5, "failuresThreshold": 3, "degradedRestartSec": 30}),
		configFileBlob(""),
		mustJSON(map[string]any{"maxSizeMB": 10, "maxBackups": 3})
}

// configFileBlob 配置策略的统一形状。path 是相对服务根的包内路径，可为空。
func configFileBlob(path string) datatypesJSON {
	return mustJSON(map[string]any{
		"path": path, "validateCmd": "", "applyMode": "restart", "applyCmd": "",
	})
}

// acceptRestartPolicy 只接受当前两种模式。存量库里的 always/onFailure
// 由 supervisor.ParsePolicy 在读取时折成 backoff，这里拒绝新写入。
func acceptRestartPolicy(raw []byte) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var rp struct {
		Mode string `json:"mode"`
	}
	if json.Unmarshal(raw, &rp) != nil || rp.Mode == "" {
		return nil
	}
	if rp.Mode != "periodic" && rp.Mode != "backoff" {
		return fmt.Errorf("restartPolicy.mode 只支持 periodic / backoff")
	}
	return nil
}

// coerceRestartPolicy 把包内旧模式改写成 backoff，避免新安装再写入废弃值。
func coerceRestartPolicy(raw []byte) []byte {
	var rp map[string]any
	if json.Unmarshal(raw, &rp) != nil {
		return raw
	}
	mode, _ := rp["mode"].(string)
	if mode != "always" && mode != "onFailure" {
		return raw
	}
	rp["mode"] = "backoff"
	b, err := json.Marshal(rp)
	if err != nil {
		return raw
	}
	return b
}

// SaveService 创建或更新服务定义。
// id==0 视为新建；更新保留 CreatedAt；dependsOn 引用与环检测返回 400。
func (s *Server) SaveService(w http.ResponseWriter, r *http.Request) {
	var p servicePayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		fail(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}
	p.Name = trimSpace(p.Name)
	if p.Name == "" {
		fail(w, http.StatusBadRequest, "服务名称必填")
		return
	}
	// 名称同时充当安装目录名，收紧为安全路径片段（与前端 / manifest 安装同规则），
	// 防止 DeleteService 拼路径时被 ".."/分隔符穿越（防御第一层）。
	if !manifestNameRE.MatchString(p.Name) {
		fail(w, http.StatusBadRequest, "服务名称仅限小写字母/数字/._-，且以字母或数字开头（最长 64）")
		return
	}
	if p.ServiceType != "native" && p.ServiceType != "opensource" {
		fail(w, http.StatusBadRequest, "serviceType 必须为 native 或 opensource")
		return
	}
	if trimSpace(p.ExePath) == "" {
		fail(w, http.StatusBadRequest, "可执行文件路径必填（安装制：路径由安装生成，手动编辑请保留）")
		return
	}
	// 摘要（卡片展示中文名）：可选，长度设上限防滥用；编辑时允许清空回落 Name。
	p.Summary = trimSpace(p.Summary)
	if utf8.RuneCountInString(p.Summary) > 40 {
		fail(w, http.StatusBadRequest, "摘要最长 40 个字符")
		return
	}
	// webUrl 只允许 http(s)（前端已校验，后端兜底防直连 API 写入 javascript: 等方案）。
	if wu := trimSpace(p.WebUrl); wu != "" && !strings.HasPrefix(wu, "http://") && !strings.HasPrefix(wu, "https://") {
		fail(w, http.StatusBadRequest, "webUrl 必须是 http:// 或 https:// 开头")
		return
	}
	if err := acceptRestartPolicy(p.RestartPolicy); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 名称唯一性。
	var conflict store.Service
	if err := s.DB.First(&conflict, "name = ? AND id <> ?", p.Name, p.ID).Error; err == nil {
		fail(w, http.StatusConflict, "服务名称已被「"+conflict.Name+"」使用")
		return
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}

	// dependsOn：引用存在性 + 环检测。
	for _, dep := range p.DependsOn {
		var n int64
		s.DB.Model(&store.Service{}).Where("id = ?", dep).Count(&n)
		if n == 0 {
			fail(w, http.StatusBadRequest, fmt.Sprintf("dependsOn 引用了不存在的服务: #%d", dep))
			return
		}
	}
	var all []store.Service
	s.DB.Order("id asc").Find(&all)
	if cycle := detectDependsCycle(all, p.ID, p.DependsOn); cycle != "" {
		fail(w, http.StatusBadRequest, "dependsOn 存在循环依赖: "+cycle)
		return
	}

	def := store.Service{
		ID: p.ID, Name: p.Name, Summary: p.Summary, ServiceType: p.ServiceType,
		WebUrl:  trimSpace(p.WebUrl),
		ExePath: trimSpace(p.ExePath), WorkDir: trimSpace(p.WorkDir),
		AutoStart: p.AutoStart, Priority: p.Priority,
		Args: mustJSON(nonNil(p.Args)), Env: mustJSON(p.Env), DependsOn: mustJSON(p.DependsOn),
		Enabled: p.Enabled == nil || *p.Enabled,
	}
	def.RestartPolicy, def.HealthCheck, def.ConfigFile, def.LogPolicy = defaultPolicyBlobs()
	if len(p.RestartPolicy) > 0 && string(p.RestartPolicy) != "null" {
		def.RestartPolicy = datatypesJSON(p.RestartPolicy)
	}
	if len(p.HealthCheck) > 0 && string(p.HealthCheck) != "null" {
		def.HealthCheck = datatypesJSON(p.HealthCheck)
	}
	if len(p.ConfigFile) > 0 && string(p.ConfigFile) != "null" {
		def.ConfigFile = datatypesJSON(p.ConfigFile)
	}
	if len(p.LogPolicy) > 0 && string(p.LogPolicy) != "null" {
		def.LogPolicy = datatypesJSON(p.LogPolicy)
	}

	// 更新：先查存在性，保留 CreatedAt；并计算 unitFingerprint 是否变化。
	needRestart := false
	var exist store.Service
	if p.ID != 0 {
		if err := s.DB.First(&exist, "id = ?", p.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				fail(w, http.StatusNotFound, "服务不存在")
			} else {
				fail(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		def.CreatedAt = exist.CreatedAt
		// 安装制字段（版本/清单快照/安装时间）不经手动编辑：更新时从旧行透传，
		// 否则全字段 Save 会把这些列清零，导出 409、OTA 版本门控失真。
		def.Version = exist.Version
		def.Manifest = exist.Manifest
		def.InstalledAt = exist.InstalledAt
		// 策略字段同理：payload 未显式提供（空 JSON）时从旧行透传。否则 API
		// 直连调用者（脚本/集成，不经前端 serviceToPayload）编辑一次 args 就
		// 会把安装时生成的 configFile.path 清空——lastgood 快照、配置编辑器、
		// 启动参数解析全部跟着失联（E2E 实测踩坑）。
		if len(p.RestartPolicy) == 0 || string(p.RestartPolicy) == "null" {
			def.RestartPolicy = exist.RestartPolicy
		}
		if len(p.HealthCheck) == 0 || string(p.HealthCheck) == "null" {
			def.HealthCheck = exist.HealthCheck
		}
		if len(p.ConfigFile) == 0 || string(p.ConfigFile) == "null" {
			def.ConfigFile = exist.ConfigFile
		}
		if len(p.LogPolicy) == 0 || string(p.LogPolicy) == "null" {
			def.LogPolicy = exist.LogPolicy
		}
		fingerprintChanged := exist.ExePath != def.ExePath ||
			exist.AutoStart != def.AutoStart || exist.Enabled != def.Enabled ||
			string(exist.Args) != string(def.Args) || string(exist.Env) != string(def.Env)
		if fingerprintChanged && s.Supervisor != nil {
			if rt, found := s.Supervisor.State(p.ID); found && (rt.State == "running" || rt.State == "degraded") {
				needRestart = true
			}
		}
	}
	if err := s.DB.Save(&def).Error; err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.Defs != nil {
		s.Defs.OnDefsSaved(def)
	}
	logx.Module("api").Info("保存服务定义", "id", def.ID, "name", def.Name, "needRestart", needRestart)
	ok(w, map[string]any{"id": def.ID, "needRestart": needRestart})
}

// DeleteService 删除服务定义；其它服务的 dependsOn 同步摘除引用。
func (s *Server) DeleteService(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		fail(w, http.StatusBadRequest, "无效的 ID")
		return
	}
	var def store.Service
	if err := s.DB.First(&def, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			fail(w, http.StatusNotFound, "服务不存在")
		} else {
			fail(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if err := s.DB.Delete(&store.Service{}, "id = ?", id).Error; err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 摘除其它服务对本服务的依赖引用。
	var others []store.Service
	s.DB.Find(&others)
	for _, o := range others {
		var deps []int
		changed := false
		if json.Unmarshal(o.DependsOn, &deps) == nil {
			kept := deps[:0]
			for _, d := range deps {
				if d == id {
					changed = true
					continue
				}
				kept = append(kept, d)
			}
			if changed {
				o.DependsOn = mustJSON(kept)
				if err := s.DB.Model(&store.Service{}).Where("id = ?", o.ID).Update("depends_on", o.DependsOn).Error; err != nil {
					logx.Module("api").Warn("摘除依赖引用失败", "id", o.ID, "err", err)
				}
			}
		}
	}
	if s.Defs != nil {
		s.Defs.OnDefsDeleted(id, def.Name)
	}
	// 清理磁盘产物与关联读模型。只删 Service 行会留下安装目录。
	s.purgeServiceArtifacts(id, def.Name)
	logx.Module("api").Info("删除服务定义", "id", id, "name", def.Name)
	ok(w, map[string]any{"id": id})
}

// purgeServiceArtifacts 卸载后的清理：安装目录、运行态、事件以外的关联行。
// 全部尽力而为（失败仅告警），不阻断卸载响应。
func (s *Server) purgeServiceArtifacts(id int, name string) {
	// 1) 安装目录 services/{os}/{arch}/{name}，不触碰 services 根。
	if name != "" {
		root := s.installRoot()
		svcDir := filepath.Join(root, name)
		// 双重防御（第二层）：SaveService 已用 manifestNameRE 校验 name，此处仍
		// 断言不越界——历史脏数据 / 直接改库时，name 含 ".." 或绝对路径即拒绝删除。
		if !pathWithin(root, svcDir) {
			logx.Module("api").Warn("服务名越界，跳过安装目录清理", "svc", name, "dir", svcDir)
		} else if err := os.RemoveAll(svcDir); err != nil {
			logx.Module("api").Warn("删除安装目录失败", "svc", name, "dir", svcDir, "err", err)
		}
	}
	// 2) 关联表行。
	for _, q := range []struct {
		model any
		desc  string
	}{
		{&store.ServiceRuntime{}, "运行态"},
	} {
		if err := s.DB.Where("service_id = ?", id).Delete(q.model).Error; err != nil {
			logx.Module("api").Warn("清理"+q.desc+"失败", "id", id, "err", err)
		}
	}
	// 3) 日志出口：回收落盘协程与文件句柄（平台侧文件按 E2 留存，仅停采释放资源；
	//    此前出口随服务删除永久泄漏，直到平台退出）。
	svclog.Detach(id)
}

// serviceOp 是 start/stop/restart 三个手动操作的共用实现。
func (s *Server) serviceOp(w http.ResponseWriter, r *http.Request, op string) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		fail(w, http.StatusBadRequest, "无效的 ID")
		return
	}
	var def store.Service
	if err := s.DB.First(&def, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			fail(w, http.StatusNotFound, "服务不存在")
		} else {
			fail(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if !def.Enabled {
		fail(w, http.StatusBadRequest, "服务已停用（enabled=false），请先启用监管")
		return
	}
	if s.Supervisor == nil {
		fail(w, http.StatusServiceUnavailable, "监管引擎未装配，手动操作不可用")
		return
	}
	var queued bool
	switch op {
	case "start":
		queued = s.Supervisor.Start(id)
	case "stop":
		queued = s.Supervisor.Stop(id)
	case "restart":
		queued = s.Supervisor.Restart(id)
	}
	if !queued {
		fail(w, http.StatusServiceUnavailable, "监管引擎忙，请稍后重试")
		return
	}
	ok(w, map[string]any{"ok": true})
}

// StartService 手动启动。
func (s *Server) StartService(w http.ResponseWriter, r *http.Request) { s.serviceOp(w, r, "start") }

// StopService 手动停止。
func (s *Server) StopService(w http.ResponseWriter, r *http.Request) { s.serviceOp(w, r, "stop") }

// RestartService 手动重启。
func (s *Server) RestartService(w http.ResponseWriter, r *http.Request) { s.serviceOp(w, r, "restart") }

// ServiceStats 返回单服务运行时长、重启次数和 CPU/内存曲线。
func (s *Server) ServiceStats(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		fail(w, http.StatusBadRequest, "无效的 ID")
		return
	}
	var n int64
	s.DB.Model(&store.Service{}).Where("id = ?", id).Count(&n)
	if n == 0 {
		fail(w, http.StatusNotFound, "服务不存在")
		return
	}
	if s.Stats == nil {
		fail(w, http.StatusServiceUnavailable, "统计源未装配")
		return
	}
	view, found := s.Stats.Series(id)
	if !found {
		view.UptimeSec = 0
	}
	ok(w, view)
}

// resolveDeps 把依赖服务名解析成 id，并做环检测。
// selfID 为 0 表示尚未落库的新服务；升级时传入已有服务 id，用新依赖替换该节点的边。
// 找不到的名字跳过（与安装预检的「依赖尚未安装」提示分工：这里不阻断）。
func (s *Server) resolveDeps(names []string, selfID int) ([]int, error) {
	deps := make([]int, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		var d store.Service
		if s.DB.First(&d, "name = ?", name).Error == nil {
			deps = append(deps, d.ID)
		}
	}
	if len(deps) == 0 {
		return deps, nil
	}
	var all []store.Service
	if err := s.DB.Order("id asc").Find(&all).Error; err != nil {
		return nil, err
	}
	if cycle := detectDependsCycle(all, selfID, deps); cycle != "" {
		return nil, fmt.Errorf("依赖形成环：%s", cycle)
	}
	return deps, nil
}

// detectDependsCycle 在全部定义上套用“目标服务的最新 dependsOn”后做 DFS
// 环检测；返回形如 “a → b → a” 的环描述，无环返回空串。
//
// updatedID == 0 表示“尚未落库的新服务”：SQLite 自增 ID 从 1 起，用 0 作为
// 虚拟节点键并入图，使新建路径的依赖也能参与环检测（原先该分支永不命中，
// 新建服务与其依赖根本不入图，检测空转）。
func detectDependsCycle(defs []store.Service, updatedID int, updatedDeps []int) string {
	edges := make(map[int][]int, len(defs)+1)
	names := make(map[int]string, len(defs)+1)
	for _, d := range defs {
		names[d.ID] = d.Name
		if d.ID == updatedID {
			edges[d.ID] = updatedDeps
			continue
		}
		var deps []int
		if json.Unmarshal(d.DependsOn, &deps) != nil {
			deps = nil
		}
		edges[d.ID] = deps
	}
	// 新建服务：以 updatedID(0) 为虚拟节点，名字由调用方在 updatedName 中给定
	// 时才有意义；这里用占位名保证环描述可读。
	if _, exists := edges[updatedID]; !exists {
		edges[updatedID] = updatedDeps
		if updatedID == 0 {
			names[0] = "新服务"
		}
	}
	// 自引用也算环。
	for id, deps := range edges {
		for _, d := range deps {
			if d == id {
				return names[id] + " → " + names[id]
			}
		}
	}
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[int]int, len(defs))
	var stack []int
	var visit func(id int) bool
	visit = func(id int) bool {
		color[id] = gray
		stack = append(stack, id)
		for _, next := range edges[id] {
			if _, known := names[next]; !known {
				continue
			}
			switch color[next] {
			case gray:
				stack = append(stack, next)
				return true
			case white:
				if visit(next) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return false
	}
	for id := range edges {
		if color[id] != white {
			continue
		}
		stack = stack[:0]
		if visit(id) {
			// 从环起点（栈中最后一次出现 gray 的节点）截取描述。
			last := stack[len(stack)-1]
			idx := 0
			for i, v := range stack {
				if v == last {
					idx = i
					break
				}
			}
			parts := make([]string, 0, len(stack)-idx)
			for _, v := range stack[idx:] {
				parts = append(parts, names[v])
			}
			return joinArrow(parts)
		}
	}
	return ""
}

func joinArrow(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " → "
		}
		out += p
	}
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func nonNil(args []string) []string {
	if args == nil {
		return []string{}
	}
	return args
}

// firstNonEmpty 返回第一个非空（去空白后）字符串，全空返回 ""。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := trimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
