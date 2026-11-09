// Package supervisor 是进程监管引擎（对标 Gateway 的 internal/engine）。
//
// Engine（engine.go）按架构 §3 实现完整状态机：worker per service 的探针
// 调度/重启决策/依赖门控（graph DAG）、差量调谐（指纹两档）与孤儿认领。
// api 层不 import 本包（依赖单向：api → facade 接口，由 main 装配注入）。
package supervisor

// State 监管状态机九态（架构 §3.1，与前端 SERVICE_STATE 表一一对应）。
type State string

const (
	StateStopped   State = "stopped"   // 已停止，定义仍在
	StateWaiting   State = "waiting"   // 等待依赖就绪（就绪门控排队）
	StateStarting  State = "starting"  // 已发起启动，L1+L2+readyHold 验证中
	StateRunning   State = "running"   // L1 存活且 L2 通过
	StateDegraded  State = "degraded"  // 进程活着但 L2 连续失败 ≥ 阈值
	StateBackoff   State = "backoff"   // 崩溃/启动失败后的退避等待
	StateFailed    State = "failed"    // 熔断：窗口内失败超限，需人工介入
	StateStopping  State = "stopping"  // 优雅停机中
	StateUpgrading State = "upgrading" // 升级管线接管，监管暂停（M2）
)

// Valid 判断是否合法状态值。
func (s State) Valid() bool {
	switch s {
	case StateStopped, StateWaiting, StateStarting, StateRunning, StateDegraded,
		StateBackoff, StateFailed, StateStopping, StateUpgrading:
		return true
	}
	return false
}

// legalTransitions 合法迁移表（架构 §3.2 全表的机器可读形式，worker 驱动）。
//
// 补充说明：Backoff 期间用户仍可手动停止/取消纳管，故 Backoff 必须允许
// Stopping（否则每次"退避中停止"都会刷出"非法状态迁移"告警）；
// Backoff 恢复走 Waiting 而非直接 Starting（先过依赖门控）。
var legalTransitions = map[State][]State{
	StateStopped:   {StateWaiting, StateUpgrading},
	StateWaiting:   {StateStarting, StateStopped, StateBackoff, StateUpgrading},
	StateStarting:  {StateRunning, StateBackoff, StateStopping, StateUpgrading},
	StateRunning:   {StateBackoff, StateDegraded, StateStopping, StateUpgrading},
	StateDegraded:  {StateRunning, StateBackoff, StateStopping, StateUpgrading},
	StateBackoff:   {StateWaiting, StateStarting, StateFailed, StateStopping, StateStopped, StateUpgrading},
	StateFailed:    {StateWaiting, StateStopped, StateUpgrading},
	StateStopping:  {StateStopped, StateFailed},
	StateUpgrading: {StateStarting, StateRunning, StateStopped, StateFailed},
}

// CanTransition 判断 from → to 是否为合法迁移（认领对齐 markAdopted 是
// 平台恢复专用例外，不走本表）。
func CanTransition(from, to State) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	for _, next := range legalTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}