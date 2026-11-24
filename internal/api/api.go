// Package api 持有全部 REST 处理器，以及监管与统计两个 facade。
//
// api 不 import supervisor。具体实现由 main 经 adapt 注入。
// 升级任务在本包内执行，不经独立 facade。

package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/buildinfo"
	"github.com/jiwei-cairn/cairn-platform/internal/config"
	"github.com/jiwei-cairn/cairn-platform/internal/store"

	goruntime "runtime"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// datatypesJSON 是 datatypes.JSON 的包内别名。
type datatypesJSON = datatypes.JSON

// goruntimeOS 返回“操作系统 · 架构”展示串。
func goruntimeOS() string {
	return goruntime.GOOS + " · " + goruntime.GOARCH
}

// daemonLabel 返回当前平台下平台自身的守护方式展示串。
// 平台自身不进自身纳管清单，其存活由操作系统级守护承担：
//   - Linux：systemd 单元（Restart=always），已随部署文档提供
//   - Windows：尚无 SCM 服务集成，当前需前台/计划任务方式常驻——
//     故此处如实标注「未注册」，不宣称已有服务守护
//
// 待 M1 后续落地 Windows 服务封装（StartServiceCtrlDispatcher）后，
// 本函数改回 "Windows 服务 (SCM · 自动重启)"。
func daemonLabel() string {
	if goruntime.GOOS == "windows" {
		return "未注册（需手动常驻）"
	}
	return "systemd (Restart=always)"
}

// RunState 是一个服务的运行态读模型快照（高频指标由内存态/采集层提供，
// 不落 ServiceRuntime 表）。
type RunState struct {
	State               string  `json:"state"`
	Pid                 int     `json:"pid"`
	UptimeSec           float64 `json:"uptimeSec"`
	RestartTotal        int     `json:"restartTotal"`
	RestartFailures     int     `json:"restartFailures"`
	ConsecutiveFailures int     `json:"consecutiveFailures"`
	CpuPct              float64 `json:"cpuPct"`
	MemMB               float64 `json:"memMB"`
	CpuTimeSec          float64 `json:"cpuTimeSec"`
	HealthState         string  `json:"healthState"`
	LastError           string  `json:"lastError"`
}

// stoppedState facade 未装配/无台账时的兜底运行态。
func stoppedState() RunState {
	return RunState{State: "stopped", HealthState: "n/a"}
}

// SupervisorFacade 监管引擎对 API 层暴露的能力。
// 允许为 nil：nil 时大盘返回 stopped 态、启停操作返回 503。
type SupervisorFacade interface {
	Start(serviceID int) bool
	Stop(serviceID int) bool
	Restart(serviceID int) bool
	State(serviceID int) (RunState, bool)
	States() map[int]RunState
}

// StatsView 是单服务统计曲线视图（时间桶数组）。
type StatsView struct {
	UptimeSec    float64 `json:"uptimeSec"`
	RestartTotal int     `json:"restartTotal"`
	CpuTimeSec   float64 `json:"cpuTimeSec"`
	Cpu          []int   `json:"cpu"`
	Mem          []int   `json:"mem"`
}

// StatsSource 单服务 CPU/内存曲线。实现由 main 注入。
type StatsSource interface {
	Series(serviceID int) (StatsView, bool)
}

// LogLine 是日志中心的一条行对象。
type LogLine struct {
	Ts      int64  `json:"ts"`
	Level   string `json:"level"`
	Service string `json:"service"`
	Msg     string `json:"msg"`
}

// Server 持有依赖句柄，挂载全部 REST 接口。
type Server struct {
	DB       *gorm.DB
	Settings *store.SettingsStore

	Supervisor SupervisorFacade
	Stats      StatsSource

	// Defs 定义变更钩子（保存/安装/删除后触发引擎差量调谐）。
	Defs DefsHook
	// Host 大盘主机资源概览（CPU/内存/磁盘百分比与网卡累计字节）。
	Host HostSource

	RuntimeName string    // "fork"
	StartedAt   time.Time // 平台自身启动时间

	// Tasks 升级任务队列（全局串行，decisions.md D7）。
	Tasks *taskManager
}

// DefsHook 服务定义变更钩子：API 写路径（保存/安装/删除）完成后通知，
// 引擎据此差量调谐（指纹两档）；实现由 main 装配注入。
type DefsHook interface {
	OnDefsSaved(def store.Service)
	OnDefsDeleted(id int, name string)
}

// HostSource 大盘主机资源概览。
//
// HostMetrics 给百分比（0-100）；HostCounters 给整机运行时长与网卡累计收发
// 字节（开机以来，纯读不推进基线）。分两个方法是因为调用节奏不同：
// 前者每个大盘请求都要刷新当前值，后者只在采样器按采集周期差分速率时才读。
type HostSource interface {
	HostMetrics() (cpu, mem, disk float64)
	HostCounters() (uptimeSec, rxBytes, txBytes float64)
}

// New 构造 Server。监管与统计由调用方在返回后注入。
func New(db *gorm.DB, settings *store.SettingsStore, startedAt time.Time) *Server {
	s := &Server{DB: db, Settings: settings, StartedAt: startedAt, Tasks: newTaskManager()}
	// 任务终态回调 → 升级台账落库（M2-8：operator/durationMs）。
	// db 为 nil 的构造形态（理论不存在，防御测试误用）不挂回调。
	if s.DB != nil {
		s.Tasks.onFinish = s.recordUpgradeRecord
	}
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ok 统一成功封装：{code:0, data}。
func ok(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "data": data})
}

// fail 统一失败封装：{code, msg}，HTTP 状态码与业务码一致。
func fail(w http.ResponseWriter, c int, m string) {
	writeJSON(w, c, map[string]any{"code": c, "msg": m})
}

// failIssues 400 + issues[]：字段级校验错误（issues[].field 用 tftp.server
// 式路径，前端据此高亮表单输入框）。
func failIssues(w http.ResponseWriter, summary string, issues []config.Issue) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"code":   http.StatusBadRequest,
		"msg":    summary,
		"issues": issues,
	})
}

// Health 存活探针：进程能响应即为存活。
func (s *Server) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().Format(time.RFC3339)})
}

// Ready 就绪探针：数据库可查询才算就绪。
func (s *Server) Ready(w http.ResponseWriter, r *http.Request) {
	sqlDB, err := s.DB.DB()
	if err == nil {
		err = sqlDB.Ping()
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "reason": "数据库不可用: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

// versionInfo GET /api/version 的响应。
type versionInfo struct {
	Name      string  `json:"name"`
	Version   string  `json:"version"`
	BuildTime string  `json:"buildTime"`
	OS        string  `json:"os"`
	Daemon    string  `json:"daemon"`
	UptimeSec float64 `json:"uptimeSec"`
	Runtime   string  `json:"runtime"`
}

// GetVersion 返回平台自身只读信息（Q3：展示但不参与监管）。
func (s *Server) GetVersion(w http.ResponseWriter, r *http.Request) {
	ok(w, versionInfo{
		Name:      buildinfo.PlatformName,
		Version:   buildinfo.Version,
		BuildTime: buildinfo.BuildTime,
		OS:        goruntimeOS(),
		Daemon:    daemonLabel(),
		UptimeSec: time.Since(s.StartedAt).Seconds(),
		Runtime:   s.RuntimeName,
	})
}
