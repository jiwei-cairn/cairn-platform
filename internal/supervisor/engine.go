// engine.go — 监管引擎：workers 表、差量调谐（指纹两档）、孤儿认领初始化、
// 事件/台账落库、启停编排（拓扑拉起 / 逆序停止传播）。
package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/runtime"
	"github.com/jiwei-cairn/cairn-platform/internal/store"

	"gorm.io/gorm"
)

type Engine struct {
	mu      sync.Mutex
	rt      runtime.Runtime
	db      *gorm.DB
	workers map[int]*worker
	defs    map[int]store.Service
	graph   *Graph
	closed  bool
}

// NewEngine 构造引擎（不启动；Init 完成装配与拉起）。
func NewEngine(rt runtime.Runtime, db *gorm.DB) *Engine {
	return &Engine{
		rt:      rt,
		db:      db,
		workers: make(map[int]*worker),
		defs:    make(map[int]store.Service),
		graph:   BuildGraph(nil),
	}
}

// Init 平台启动装配：登记期望态 → 认领对齐（孤儿零中断）→ 启动 workers →
// autoStart 按拓扑序拉起 → 订阅退出事件分发。
func (e *Engine) Init() {
	var defs []store.Service
	e.db.Order("priority asc, id asc").Find(&defs)

	e.mu.Lock()
	for _, def := range defs {
		_ = e.rt.EnsureUnit(toUnitDef(def))
		row := e.loadRuntime(def.ID)
		w := newWorker(e, def, row)
		e.workers[def.ID] = w
		e.defs[def.ID] = def
	}
	e.rebuildGraphLocked(defs)
	workers := make(map[int]*worker, len(e.workers))
	for k, w := range e.workers {
		workers[k] = w
	}
	e.mu.Unlock()

	// 认领：平台崩溃存活孤儿 → running 零中断对齐（不追认历史、只对齐现状）。
	adopted := make(map[int]bool)
	ad, canAdopt := e.rt.(runtime.Adopter)
	for _, def := range defs {
		if !def.Enabled || !canAdopt {
			continue
		}
		if ad.Adopt(toUnitDef(def)) {
			adopted[def.ID] = true
			if w := e.workerByID(def.ID); w != nil {
				w.markAdopted()
			}
		}
	}
	// worker 循环启动。
	for _, w := range workers {
		w.start()
	}
	// autoStart 拓扑拉起（认领的跳过）。
	for _, id := range e.graph.TopoOrder(defs) {
		def := defs[id2idx(defs, id)]
		if def.Enabled && def.AutoStart && !adopted[id] {
			if w := e.workerByID(id); w != nil {
				w.send(workerCmd{kind: cmdStart})
			}
		}
	}
	// 退出事件分发（事件驱动感知；通道关闭即退出）。
	if ew, ok := e.rt.(runtime.ExitWatcher); ok {
		go e.dispatch(ew)
	}
	logx.Module("supervisor").Info("监管引擎已启动",
		"services", len(defs), "adopted", len(adopted))
}

func id2idx(defs []store.Service, id int) int {
	for i := range defs {
		if defs[i].ID == id {
			return i
		}
	}
	return 0
}

func (e *Engine) dispatch(ew runtime.ExitWatcher) {
	for ev := range ew.Exits() {
		if w := e.workerByID(ev.ServiceID); w != nil {
			if !w.send(workerCmd{kind: cmdExit, ev: &ev}) {
				logx.Module("supervisor").Warn("退出事件投递失败（worker 忙）",
					"id", ev.ServiceID, "exit", ev.ExitCode)
			}
		}
	}
}

// SyncFromDB 读取全量期望态并差量调谐（API 保存/安装/删除后由
// main 的 defsHook 异步触发）。
func (e *Engine) SyncFromDB() {
	var defs []store.Service
	e.db.Order("priority asc, id asc").Find(&defs)
	e.Apply(defs)
}

func (e *Engine) workerByID(id int) *worker {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.workers[id]
}

// ── Engine 接口实现（main 装配 + facade 适配） ───────────

// Apply 差量调谐：期望态变更后按指纹两档决策（架构 §1.4）——
// unitFingerprint 变 → 更新登记，运行中则重启；监管参数变 → 仅热更新 worker。
func (e *Engine) Apply(services []store.Service) {
	e.mu.Lock()
	type action struct {
		add    *store.Service
		update *store.Service
		restart bool
		remove int
	}
	var acts []action
	newIDs := make(map[int]bool, len(services))
	for i := range services {
		newIDs[services[i].ID] = true
	}
	for id := range e.workers {
		if !newIDs[id] {
			acts = append(acts, action{remove: id})
		}
	}
	for i := range services {
		def := services[i]
		old, ok := e.defs[def.ID]
		if !ok {
			d := def
			acts = append(acts, action{add: &d})
			continue
		}
		restart := UnitFingerprint(old) != UnitFingerprint(def)
		changed := restart || SuperviseFingerprint(old) != SuperviseFingerprint(def) || old.Name != def.Name
		if changed {
			d := def
			acts = append(acts, action{update: &d, restart: restart})
		}
	}
	// 先在锁内更新 defs/graph 快照（worker 增删在锁外执行后回写）。
	var removed []*worker
	for _, a := range acts {
		if a.remove != 0 {
			if w := e.workers[a.remove]; w != nil {
				removed = append(removed, w)
			}
			delete(e.workers, a.remove)
			delete(e.defs, a.remove)
		}
	}
	var added []*worker
	for _, a := range acts {
		if a.add != nil {
			_ = e.rt.EnsureUnit(toUnitDef(*a.add))
			w := newWorker(e, *a.add, e.loadRuntime(a.add.ID))
			e.workers[a.add.ID] = w
			e.defs[a.add.ID] = *a.add
			added = append(added, w)
		}
	}
	for _, a := range acts {
		if a.update != nil {
			e.defs[a.update.ID] = *a.update
		}
	}
	e.rebuildGraphLocked(nil) // 用 e.defs 重建
	updates := acts
	e.mu.Unlock()

	// 锁外执行 worker 侧动作。
	for _, w := range removed {
		e.stopDependents(w.id)
		w.send(workerCmd{kind: cmdDispose})
		_ = e.rt.RemoveUnit(unitKey(w.id))
		e.event("info", "stop", w.id, w.defName(), "服务已删除，移除监管")
	}
	for _, w := range added {
		w.start()
		if w.pol.HealthType != "" {
			// 新增即登记；autoStart 交由启动门控。
		}
		if d := e.defByID(w.id); d != nil && d.Enabled && d.AutoStart {
			w.send(workerCmd{kind: cmdStart})
		}
		e.event("info", "start", w.id, w.defName(), "服务定义新增，已纳入监管")
	}
	for _, a := range updates {
		if a.update == nil {
			continue
		}
		w := e.workerByID(a.update.ID)
		if w == nil {
			continue
		}
		w.send(workerCmd{kind: cmdUpdateDef, def: a.update})
		_ = e.rt.EnsureUnit(toUnitDef(*a.update))
		if a.restart {
			if snap := w.snapshot(); snap.State == string(StateRunning) || snap.State == string(StateDegraded) {
				if !a.update.Enabled {
					// 取消纳管：语义是"停"，不是"重启"。指纹含 enabled，
					// 若按 restart 处理会把刚禁用的服务重新拉起（doStart 已有
					// Enabled 门控兜底，这里仍显式走停止以免出现无谓的 stop 事件）。
					w.send(workerCmd{kind: cmdStop})
					e.event("info", "stop", a.update.ID, a.update.Name, "已取消纳管，停止运行")
				} else {
					w.send(workerCmd{kind: cmdRestart})
					e.event("info", "restart", a.update.ID, a.update.Name,
						"unitFingerprint 变化（exePath/args/env 等），重启生效")
				}
			}
		}
	}
}

// Start 手动启动（依赖门控后进入 starting）。
func (e *Engine) Start(id int) bool {
	w := e.workerByID(id)
	return w != nil && w.send(workerCmd{kind: cmdStart})
}

// Stop 手动停止：先停依赖方（逆序传播），最后停目标。
func (e *Engine) Stop(id int) bool {
	ok := true
	for _, x := range e.stopOrderFor(id) {
		if w := e.workerByID(x); w != nil {
			if !w.send(workerCmd{kind: cmdStop}) {
				ok = false
			}
		}
	}
	return ok
}

// Restart 手动重启（仅自身；依赖门控只作用于启动路径）。
func (e *Engine) Restart(id int) bool {
	w := e.workerByID(id)
	return w != nil && w.send(workerCmd{kind: cmdRestart})
}

// StopAll 优雅退出：全量逆拓扑序停止（main 关闭链调用）。
func (e *Engine) StopAll() {
	e.mu.Lock()
	var defs []store.Service
	for _, d := range e.defs {
		defs = append(defs, d)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Priority < defs[j].Priority })
	e.mu.Unlock()
	order := e.graph.TopoOrder(defs)
	for i := len(order) - 1; i >= 0; i-- {
		if w := e.workerByID(order[i]); w != nil {
			w.send(workerCmd{kind: cmdStop})
		}
	}
}

// Close 停止引擎：逆序停被管服务 → 等待全部停止完成（带超时） → 关 runtime。
func (e *Engine) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	// 收集全部 worker 的 stopDone 通道（停止前快照，防 StopAll 期间增删）。
	workers := make([]*worker, 0, len(e.workers))
	for _, w := range e.workers {
		workers = append(workers, w)
	}
	e.mu.Unlock()

	e.StopAll()

	// 等待全部 worker 停止完成（总超时 15s = stopGrace 10s + killGrace 3s + 余量）。
	deadline := time.After(15 * time.Second)
	for _, w := range workers {
		select {
		case <-w.stopDone:
			// 该 worker 已停止
		case <-deadline:
			logx.Module("supervisor").Warn("等待服务停止超时，强制退出", "id", w.id, "name", w.defName())
			goto done
		}
	}
done:
	e.rt.Close()
}

// ── 快照与辅助（facade 适配器消费） ─────────────────────

// Snapshot 单服务快照。
func (e *Engine) Snapshot(id int) (Snap, bool) {
	w := e.workerByID(id)
	if w == nil {
		return Snap{State: string(StateStopped)}, false
	}
	return w.snapshot(), true
}

// Snapshots 全部服务快照。
func (e *Engine) Snapshots() map[int]Snap {
	e.mu.Lock()
	ids := make([]int, 0, len(e.workers))
	for id := range e.workers {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	out := make(map[int]Snap, len(ids))
	for _, id := range ids {
		if w := e.workerByID(id); w != nil {
			out[id] = w.snapshot()
		}
	}
	return out
}

// depsReady 依赖就绪查询（仅 running 视为就绪；M1 保守口径）。
func (e *Engine) depsReady(id int) bool {
	e.mu.Lock()
	g := e.graph
	e.mu.Unlock()
	return g.Ready(id, func(dep int) bool {
		w := e.workerByID(dep)
		if w == nil {
			return false
		}
		s := w.snapshot()
		return s.State == string(StateRunning)
	})
}

// depNames 依赖名列表（事件文案用）。
func (e *Engine) depNames(id int) []string {
	e.mu.Lock()
	g := e.graph
	defs := e.defs
	e.mu.Unlock()
	var names []string
	for _, dep := range g.Deps(id) {
		if d, ok := defs[dep]; ok {
			names = append(names, d.Name)
		} else {
			names = append(names, fmt.Sprintf("#%d", dep))
		}
	}
	return names
}

// stopOrderFor 停止顺序：依赖方闭包（下游先停）+ 目标最后。
func (e *Engine) stopOrderFor(id int) []int {
	e.mu.Lock()
	g := e.graph
	var defs []store.Service
	for _, d := range e.defs {
		defs = append(defs, d)
	}
	e.mu.Unlock()
	closure := map[int]bool{id: true}
	for _, x := range g.DependentsClosure(id) {
		closure[x] = true
	}
	var order []int
	topo := g.TopoOrder(defs)
	for i := len(topo) - 1; i >= 0; i-- {
		if closure[topo[i]] && topo[i] != id {
			order = append(order, topo[i])
		}
	}
	return append(order, id)
}

// stopDependents 删除服务前停其依赖方。
func (e *Engine) stopDependents(id int) {
	for _, x := range e.stopOrderFor(id) {
		if x == id {
			continue
		}
		if w := e.workerByID(x); w != nil {
			w.send(workerCmd{kind: cmdStop})
		}
	}
}

func (e *Engine) defByID(id int) *store.Service {
	e.mu.Lock()
	defer e.mu.Unlock()
	if d, ok := e.defs[id]; ok {
		return &d
	}
	return nil
}

func (e *Engine) rebuildGraphLocked(defs []store.Service) {
	if defs == nil {
		for _, d := range e.defs {
			defs = append(defs, d)
		}
	}
	e.graph = BuildGraph(defs)
}

// ── 事件与台账落库 ─────────────────────────────────────

func (e *Engine) event(level, typ string, svcID int, name, msg string) {
	if name == "" {
		if d := e.defByID(svcID); d != nil {
			name = d.Name
		}
	}
	logx.Module("supervisor").Log(context.Background(), slogLevel(level), msg,
		"id", svcID, "service", name, "type", typ)
	e.db.Create(&store.EventLog{
		Type: typ, Level: level, ServiceID: svcID, Message: msg,
		CreatedAt: time.Now(),
	})
}

// slogLevel 事件 level → slog 级别（failed=error、backoff/degraded=warn、其余 info）。
func slogLevel(level string) slog.Level {
	switch level {
	case "error":
		return slog.LevelError
	case "warn":
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

func (e *Engine) saveRuntime(row store.ServiceRuntime) {
	// ServiceID 唯一索引 upsert：查现存主键回填，避免 Save 走 Insert 撞唯一约束。
	var exist store.ServiceRuntime
	if err := e.db.First(&exist, "service_id = ?", row.ServiceID).Error; err == nil {
		row.ID = exist.ID
	}
	if err := e.db.Save(&row).Error; err != nil {
		logx.Module("supervisor").Warn("运行台账落库失败", "serviceId", row.ServiceID, "err", err)
	}
}

func (e *Engine) loadRuntime(svcID int) store.ServiceRuntime {
	var row store.ServiceRuntime
	if err := e.db.First(&row, "service_id = ?", svcID).Error; err != nil {
		row = store.ServiceRuntime{
			ServiceID: svcID, State: string(StateStopped),
			StartedAt: time.Now(), HealthState: "n/a",
		}
		e.db.Create(&row)
	}
	return row
}

// toUnitDef Service 定义 → runtime 期望态。
func toUnitDef(def store.Service) runtime.UnitDef {
	var args []string
	_ = json.Unmarshal(def.Args, &args)
	env := map[string]string{}
	_ = json.Unmarshal(def.Env, &env)
	return runtime.UnitDef{
		ServiceID: def.ID, Name: def.Name,
		ExePath: def.ExePath, Args: args, WorkDir: def.WorkDir, Env: env,
		AutoStart: def.AutoStart, Enabled: def.Enabled,
	}
}