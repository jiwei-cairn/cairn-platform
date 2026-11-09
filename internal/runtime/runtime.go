// Package runtime 是进程管理器抽象（平台级，非 supervisor 私有）。
//
// 目标运行环境为 Linux（amd64/arm64），开发机 Windows 可真跑。原则（架构 §3.4，
// M1 决策修订为 fork/exec 父子模型，见 M1 方案 §0）：runtime 只回答"进程管理器
// 能做什么"——进程生命周期原语；不懂健康、不懂策略、不懂依赖、不懂版本。
// 健康探针、重启策略、依赖门控全部在 supervisor；进程 fork/信号/孤儿认领细节
// 全部在 runtime/fork，互不越界。安装制定稿后全部服务统一 managed（平台安装、
// 平台定义）。
package runtime

import (
	"fmt"
	"time"
)

// UnitState 是进程事实层面的中立状态枚举（与监管语义的九态状态机是两个
// 层次：worker 负责把进程事实折叠进监管语义）。
//
// fork 实现只产生四值：running / stopped / failed / notFound（fork 即活，
// 无 OS 级过渡态）；starting / stopping 保留给有过渡语义的实现（如备选的
// systemd）与 fake 的惰性时间模型。
type UnitState string

const (
	UnitRunning  UnitState = "running"  // 进程存活（fork：Wait 未返回）
	UnitStarting UnitState = "starting" // 过渡：启动中（fork 不产生）
	UnitStopping UnitState = "stopping" // 过渡：停止中（fork Stop 同步等待，不产生）
	UnitStopped  UnitState = "stopped"  // 已正常退出
	UnitFailed   UnitState = "failed"   // 非零退出 / 被信号杀死
	UnitNotFound UnitState = "notFound" // 无登记（从未启动或已移除）
)

// UnitStatus 是一次状态查询的中立结果。
type UnitStatus struct {
	Name     string    `json:"name"`
	State    UnitState `json:"state"`
	MainPID  int       `json:"mainPid"`
	ExitCode int       `json:"exitCode"`
	Since    time.Time `json:"-"`
}

// UnitDef 是启动/更新一个受管进程所需的全部期望态。
// 安装制定稿：统一 managed 模式（平台安装、平台定义），adopt 已退役。
type UnitDef struct {
	ServiceID int               // 平台内服务 id
	Name      string            // 服务名（展示用）
	ExePath   string            //
	Args      []string          //
	WorkDir   string            //
	Env       map[string]string //
	AutoStart bool              // 开机自启
	Enabled   bool              // 是否纳入监管
}

// Runtime 是进程管理器抽象接口：八个生命周期原语（中立命名）。
// 实现：fork（M1，os/exec 父子模型 + 孤儿认领，跨平台）/ fake（内存实现：
// 单测桩 + UI 原型 mock，一物两用）。
type Runtime interface {
	// EnsureUnit 幂等登记期望态并生效（fork：内存台账 + 落盘登记供认领）。
	EnsureUnit(def UnitDef) error
	// RemoveUnit 删除单元（停止若活 + 清除登记）。
	RemoveUnit(name string) error
	// SetEnabled 开机自启开关（登记位翻转）。
	// 当前无调用方：enabled 经 EnsureUnit 的 def 更新流转，保留为实现自约束。
	SetEnabled(name string, enabled bool) error
	// Start 发起启动（fork 即活，立即返回；就绪判定归 supervisor worker）。
	Start(name string, timeout time.Duration) error
	// Stop 优雅停机（信号序列 + 宽限 = timeout + 强杀兜底），同步等待退出。
	Stop(name string, timeout time.Duration) error
	// Restart 重启（当前 supervisor 用 doStop+doStart 组合，不经此方法）。
	Restart(name string, timeout time.Duration) error
	// Status 查询进程当前状态（无登记时 State 为 UnitNotFound，不返回错误）。
	Status(name string) (UnitStatus, error)
	// Pids 返回主进程 + 子进程 PID 列表（采集统计用；M3 采集落地前无调用方）。
	Pids(name string) ([]int, error)
	// Close 释放底层资源。
	Close()
}

// UnitExit 是一次进程退出事件（fork：cmd.Wait() 结果；认领进程为轮询发现）。
type UnitExit struct {
	ServiceID int
	Name      string
	ExitCode  int       // 非 0 = 异常退出（被信号杀死时 Err 携带信号信息）
	Err       error     // Wait 返回的错误（含信号退出详情）
	Tail      string    // 崩溃现场：stdout/stderr 尾部行缓冲（D4 留存）
	At        time.Time
	Adopted   bool // true = 认领进程的对账轮询发现退出（非父子关系）
}

// ExitWatcher 是可选能力接口：能事件驱动感知退出的实现提供（fork）。
// 消费者（supervisor engine）按能力探测；未实现时退化为轮询 Status。
type ExitWatcher interface {
	Exits() <-chan UnitExit
}

// UnitKeyFor 返回 managed 模式的单元键名规则：cairn-{id}。
// fork 模型无 unit 文件，此键是登记表主键与日志关联名。
func UnitKeyFor(serviceID int) string {
	return fmt.Sprintf("cairn-%d", serviceID)
}

// Adopter 可选能力接口：支持孤儿认领的实现提供（fork：linux 零中断接管；
// windows 清场）。平台启动时由 supervisor 探测调用。
type Adopter interface {
	Adopt(def UnitDef) bool
}