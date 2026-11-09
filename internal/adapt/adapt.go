// Package adapt — 装配层适配器：supervisor 引擎 → api facade / 定义钩子。
//
// 依赖方向：adapt 同时 import api 与 supervisor（它是唯一允许这么做的包），
// main 装配时将适配器注入 api.Server，保证 api 与 supervisor 互不 import。
package adapt

import (
	"github.com/jiwei-cairn/cairn-platform/internal/api"
	"github.com/jiwei-cairn/cairn-platform/internal/store"
	"github.com/jiwei-cairn/cairn-platform/internal/supervisor"
)

// SupervisorAdapter supervisor.Engine → api.SupervisorFacade。
// 高频指标（cpu/mem 等）留 0：M1 状态机口径。
type SupervisorAdapter struct {
	Eng *supervisor.Engine
}

func (a SupervisorAdapter) Start(id int) bool   { return a.Eng.Start(id) }
func (a SupervisorAdapter) Stop(id int) bool    { return a.Eng.Stop(id) }
func (a SupervisorAdapter) Restart(id int) bool { return a.Eng.Restart(id) }

func (a SupervisorAdapter) State(id int) (api.RunState, bool) {
	s, ok := a.Eng.Snapshot(id)
	return RunStateFrom(s), ok
}

func (a SupervisorAdapter) States() map[int]api.RunState {
	m := a.Eng.Snapshots()
	out := make(map[int]api.RunState, len(m))
	for id, s := range m {
		out[id] = RunStateFrom(s)
	}
	return out
}

// RunStateFrom supervisor.Snap → api.RunState。
func RunStateFrom(s supervisor.Snap) api.RunState {
	return api.RunState{
		State: s.State, Pid: s.Pid, UptimeSec: s.UptimeSec,
		RestartTotal: s.RestartTotal, RestartFailures: s.RestartFailures,
		ConsecutiveFailures: s.ConsecutiveFailures,
		HealthState: s.HealthState, LastError: s.LastError,
		CpuPct: s.CpuPct, MemMB: s.MemMB,
		CpuTimeSec: s.CpuTimeSec, NetRxKB: s.NetRxKB, NetTxKB: s.NetTxKB,
	}
}

// DefsHook api.DefsHook → 引擎差量调谐（异步触发，不阻塞 API 写路径）。
type DefsHook struct {
	Eng *supervisor.Engine
}

func (h DefsHook) OnDefsSaved(store.Service)  { go h.Eng.SyncFromDB() }
func (h DefsHook) OnDefsDeleted(int, string)  { go h.Eng.SyncFromDB() }