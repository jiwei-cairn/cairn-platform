// Package api 持有全部 REST 处理器与三个 facade 接口。
//
// 依赖规则（架构 §1.3）：api 只依赖本包定义的 facade 接口，不 import
// supervisor/update/collect 具体包；具体实现由 main 装配注入。骨架期的
// B 类端点按里程碑逐块替换为真引擎适配器。

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
	NetRxKBps           float64 `json:"netRxKBps"`
	NetTxKBps           float64 `json:"netTxKBps"`
	CpuTimeSec          float64 `json:"cpuTimeSec"`
	NetRxKB             float64 `json:"netRxKB"`
	NetTxKB             float64 `json:"netTxKB"`
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

// UpdateItem 是一条可用更新（TFTP index.json 清单项）。
type UpdateItem struct {
	ServiceID   int    `json:"serviceId"`
	Version     string `json:"version"`
	SizeBytes   int64  `json:"sizeBytes"`
	PackageName string `json:"packageName"`
	Sha256      string `json:"sha256"`
}

// ApplyRequest 是 POST /api/upgrade/apply 的请求体。
type ApplyRequest struct {
	ServiceID int    `json:"serviceId"`
	Version   string `json:"version"`
	Source    string `json:"source"` // tftp | usb | manual
	Rollback  bool   `json:"rollback"`
}

// TaskBatch U盘批量升级的当前包指示。
type TaskBatch struct {
	Index       int    `json:"index"`
	Total       int    `json:"total"`
	PackageName string `json:"packageName"`
}

// TaskPackage U盘批量升级的单包结果。
type TaskPackage struct {
	PackageName string `json:"packageName"`
	ServiceID   int    `json:"serviceId"`
	Version     string `json:"version"`
	Result      string `json:"result"` // pending|success|failed|rolledback
	Detail      string `json:"detail"`
}

// TaskView 是 GET /api/upgrade/tasks/{id} 的响应形状（冻结契约）。
type TaskView struct {
	ID            string         `json:"id"`
	Mode          string         `json:"mode"` // upgrade|rollback|usb
	Source        string         `json:"source"`
	ServiceID     int            `json:"serviceId"`
	ServiceName   string         `json:"serviceName"`
	TargetVersion string         `json:"targetVersion"`
	Batch         *TaskBatch     `json:"batch"`
	PhaseIndex    int            `json:"phaseIndex"`
	Phase         string         `json:"phase"`
	Progress      *int           `json:"progress"`
	ReceivedBytes *int64         `json:"receivedBytes"`
	TotalBytes    *int64         `json:"totalBytes"`
	HealthElapsed *float64       `json:"healthElapsed"`
	HealthTotal   *float64       `json:"healthTotal"`
	Detail        string         `json:"detail"`
	Status        string         `json:"status"` // running|success|failed|rolledback|partial
	RolledBackTo  string         `json:"rolledBackTo"`
	ReportNote    string         `json:"reportNote"`
	StartedAt     int64          `json:"startedAt"`
	FinishedAt    *int64         `json:"finishedAt"`
	Packages      []*TaskPackage `json:"packages"`
}

// UpdateFacade 升级管线对 API 层暴露的能力（M2 替换为真管线）。
type UpdateFacade interface {
	// Check 按设置中的 TFTP 服务端返回可用更新清单。
	Check() ([]UpdateItem, int64, error)
	// Apply 受理一次升级/回滚/U盘批量任务；互斥：并发进行返回 409 httpError。
	Apply(req ApplyRequest) (string, error)
	// Task 返回任务当前视图（推进按惰性时间模型计算）。
	Task(id string) (*TaskView, error)
}

// StatsView 是单服务统计曲线视图（时间桶数组）。
type StatsView struct {
	UptimeSec    float64 `json:"uptimeSec"`
	RestartTotal int     `json:"restartTotal"`
	CpuTimeSec   float64 `json:"cpuTimeSec"`
	NetRxKB      float64 `json:"netRxKB"`
	NetTxKB      float64 `json:"netTxKB"`
	Cpu          []int   `json:"cpu"`
	Mem          []int   `json:"mem"`
}

// StatsSource 采集统计对 API 层暴露的能力（M3 替换为 gopsutil 采样）。
type StatsSource interface {
	Series(serviceID int) (StatsView, bool)
}

// LogLine 是日志中心的一条行对象（真实路径与 fake 路径产出同一形状）。
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
	Updater    UpdateFacade
	Stats      StatsSource

	// Defs 定义变更钩子（保存/安装/删除后触发引擎差量调谐）；
	// demo 与真引擎各挂实现（装配矩阵见 M1 方案 §1）。
	Defs DefsHook
	// Host 大盘主机资源概览源（demo 随机游走 / 真实 /proc 读取）。
	Host HostSource

	RuntimeName string    // "fork"
	Mock        bool      // 兼容字段（demo 已退役，恒 false）
	StartedAt   time.Time // 平台自身启动时间

	// Tasks 升级任务队列（全局串行，decisions.md D7）。
	Tasks *taskManager
}

// DefsHook 服务定义变更钩子：API 写路径（保存/安装/删除）完成后通知，
// 引擎据此差量调谐（指纹两档）；实现由 main 装配注入（demo / 真引擎适配）。
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

// New 构造基础 Server（无演示引擎；服务定义 CRUD/设置/版本仍可用）。
func New(db *gorm.DB, settings *store.SettingsStore, startedAt time.Time) *Server {
	return &Server{DB: db, Settings: settings, StartedAt: startedAt, Tasks: newTaskManager()}
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

// notImplemented 501：路由已注册、实现留待后续里程碑的统一出口，
// 前端 MOCK=false 联调时可区分“未实现”与“出错”。
func notImplemented(w http.ResponseWriter, what string) {
	fail(w, http.StatusNotImplemented, "未实现（"+what+"），留待后续里程碑")
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

// versionInfo GET /api/version 的响应（前端顶部演示角标据 mock/runtime 判定）。
type versionInfo struct {
	Name      string  `json:"name"`
	Version   string  `json:"version"`
	BuildTime string  `json:"buildTime"`
	OS        string  `json:"os"`
	Daemon    string  `json:"daemon"`
	UptimeSec float64 `json:"uptimeSec"`
	Mock      bool    `json:"mock"`
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
		Mock:      s.Mock,
		Runtime:   s.RuntimeName,
	})
}
