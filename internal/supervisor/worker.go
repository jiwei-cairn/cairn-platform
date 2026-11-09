// worker.go — 单服务监管循环：进程事实 + L2 探针 → 九态监管语义（架构 §3.2 全表）。
//
// 事件驱动：亲管进程退出经 runtime.ExitWatcher 即时到达（cmdExit）；认领进程
// 由 runtime 探活协程发同一事件（Adopted=true）。对账 tick（2s）兜底状态机
// 自愈（门控重查/退避到期/启动超时/readyHold 计满/degraded 超时）；台账
// 30s 快照 + 状态迁移即写。
package supervisor

import (
	"fmt"
	"sync"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/runtime"
	"github.com/jiwei-cairn/cairn-platform/internal/store"
	"github.com/jiwei-cairn/cairn-platform/internal/supervisor/health"
)

type workerCmdKind int

const (
	cmdStart workerCmdKind = iota
	cmdStop
	cmdRestart
	cmdUpdateDef
	cmdDispose
	cmdExit
)

type workerCmd struct {
	kind workerCmdKind
	def  *store.Service
	ev   *runtime.UnitExit
}

// Snap worker 对外快照（engine 聚合 → 适配器转 api.RunState）。
type Snap struct {
	State               string
	Pid                 int
	UptimeSec           float64
	RestartTotal        int
	RestartFailures     int
	ConsecutiveFailures int
	HealthState         string
	LastError           string
	LastExitCode        int
	CpuPct              float64 // CPU 占用率（两次采样差分）
	MemMB               float64 // RSS 内存（MB）
	CpuTimeSec          float64 // 累计 CPU 时间
	NetRxKB             float64 // 累计接收（KB，留 M3）
	NetTxKB             float64 // 累计发送（KB，留 M3）
}

const (
	workerCmdBuf    = 16
	reconcileEvery  = 2 * time.Second
	snapshotEvery   = 30 * time.Second
	fastDeathSec    = 2 * time.Second  // 防抖阈值：上次启动后存活 <2s 即死
	fastDeathAdd    = 5 * time.Second  // 快死额外退避
	depAlarmEvery   = time.Minute      // 依赖不可用告警去抖
	startTimeoutPad = 10 * time.Second
)

type worker struct {
	id    int
	eng   *Engine
	rt    runtime.Runtime
	probe health.Prober
	cmdCh chan workerCmd

	mu           sync.Mutex
	def          store.Service
	pol          Policy
	st           State
	pid          int
	startedAt    time.Time
	lastExitCode int
	lastError    string

	l2FailStreak  int
	l2OkStreak    int
	l2FirstOkAt   time.Time
	lastProbeAt   time.Time
	degradedSince time.Time

	windowStart    time.Time
	windowFails    int
	lastStartAt    time.Time
	nextRetryAt    time.Time
	startingAt     time.Time
	pendingRestart bool
	depAlarmAt      time.Time

	// 进程指标（2s 对账周期采集，/proc 或 GetProcessTimes 差分）
	lastCpuTime   float64
	lastCpuAt     time.Time
	curCpuPct     float64
	curMemMB      float64
	curCpuTimeSec float64

	stopDone chan struct{} // doStop 完成信号（Close 等待用）
	stopOnce sync.Once

	row store.ServiceRuntime
}

func newWorker(eng *Engine, def store.Service, row store.ServiceRuntime) *worker {
	w := &worker{
		id: def.ID, eng: eng, rt: eng.rt,
		cmdCh:    make(chan workerCmd, workerCmdBuf),
		stopDone: make(chan struct{}),
		def:      def,
		pol:      ParsePolicy(def),
		st:       StateStopped, row: row,
	}
	w.probe = health.New(w.pol.HealthType)
	w.row.State = string(w.st)
	return w
}

// send 非阻塞投递（满 = 引擎忙，facade 层转 503）。
func (w *worker) send(c workerCmd) bool {
	select {
	case w.cmdCh <- c:
		return true
	default:
		return false
	}
}

func (w *worker) start() { go w.run() }

func (w *worker) run() {
	reconcile := time.NewTicker(reconcileEvery)
	defer reconcile.Stop()
	snapshot := time.NewTicker(snapshotEvery)
	defer snapshot.Stop()
	for {
		select {
		case c := <-w.cmdCh:
			if w.handle(c) {
				return
			}
		case <-reconcile.C:
			w.reconcile()
			w.probeNow()
			w.applyProbeResult()
		case <-snapshot.C:
			w.persistSnapshot()
		}
	}
}

func (w *worker) handle(c workerCmd) bool {
	switch c.kind {
	case cmdStart:
		w.doStart(true)
	case cmdStop:
		w.doStop("手动停止")
	case cmdRestart:
		w.doStop("手动重启")
		w.doStart(true)
	case cmdUpdateDef:
		if c.def != nil {
			w.updateDef(*c.def)
		}
	case cmdDispose:
		w.doStop("移除监管")
		return true
	case cmdExit:
		if c.ev != nil {
			w.onExit(*c.ev)
		}
	}
	return false
}

// updateDef 期望态热更新：监管参数变化只更新 worker 配置，绝不触发进程
// 重启（架构 §1.4 铁律；指纹变化引发的重启由 engine 差量决策）。
// worker 不重复持有指纹副本——比对全部发生在 Engine.Apply 的 store.Service 上。
func (w *worker) updateDef(def store.Service) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.def = def
	w.pol = ParsePolicy(def)
	w.probe = health.New(w.pol.HealthType)
}

// doStart 启动路径：→ waiting（依赖门控）→ tryLaunch。
// resetFuse=true 清零熔断计数（failed 人工"重置并启动"语义）。
func (w *worker) doStart(resetFuse bool) {
	w.mu.Lock()
	// enabled=false 的服务不得被拉起：指纹包含 enabled，用户取消纳管会触发
	// Apply 的重启路径，若无此门控就会把刚被禁用的服务重新启动起来。
	if !w.def.Enabled {
		w.mu.Unlock()
		return
	}
	if resetFuse {
		w.windowFails = 0
		w.row.RestartFailures = 0
		w.row.ConsecutiveFailures = 0
	}
	active := w.st == StateRunning || w.st == StateDegraded || w.st == StateStarting ||
		w.st == StateStopping || w.st == StateUpgrading
	w.mu.Unlock()
	if active {
		return
	}
	w.transition(StateWaiting, "info", "start", "启动请求（依赖门控检查）")
	w.tryLaunch()
}

// tryLaunch waiting → starting：依赖就绪则 fork，否则保持等待（不空转）。
func (w *worker) tryLaunch() {
	w.mu.Lock()
	if w.st != StateWaiting {
		w.mu.Unlock()
		return
	}
	ready := w.eng.depsReady(w.id)
	name := w.def.Name
	w.mu.Unlock()
	if !ready {
		if time.Since(w.depAlarmAt) > depAlarmEvery {
			w.mu.Lock()
			w.depAlarmAt = time.Now()
			w.mu.Unlock()
			w.eng.event("warn", "alert", w.id, name,
				fmt.Sprintf("等待依赖就绪（%v），暂不启动", w.eng.depNames(w.id)))
		}
		return
	}
	if !w.transition(StateStarting, "info", "start", "依赖就绪，发起启动") {
		return
	}
	w.mu.Lock()
	w.startingAt = time.Now()
	w.lastStartAt = w.startingAt
	w.l2FirstOkAt = time.Time{}
	w.l2FailStreak, w.l2OkStreak = 0, 0
	w.mu.Unlock()

	if err := w.rt.Start(unitKey(w.id), 0); err != nil {
		w.mu.Lock()
		w.lastError = "启动失败: " + err.Error()
		w.mu.Unlock()
		w.failAttempt("fork 失败: " + err.Error())
		return
	}
	st, _ := w.rt.Status(unitKey(w.id))
	w.mu.Lock()
	w.pid = st.MainPID
	w.row.Pid = w.pid
	w.mu.Unlock()
	w.persistSnapshot()
}

// doStop 停止路径（依赖方先行逆序停止由 engine 协调）。
func (w *worker) doStop(reason string) {
	w.mu.Lock()
	if w.st == StateStopped || w.st == StateFailed {
		w.mu.Unlock()
		w.stopOnce.Do(func() { close(w.stopDone) }) // 已停止也发完成信号
		return
	}
	name := w.def.Name
	w.mu.Unlock()

	w.transition(StateStopping, "info", "stop", "停止中: %s", reason)
	stopErr := w.rt.Stop(unitKey(w.id), 0)
	w.mu.Lock()
	w.pid = 0
	w.row.Pid = 0
	if stopErr != nil {
		// 停止失败（如权限受限杀不掉）：进程可能仍在运行，不能对外声称已停止。
		// 标 failed 并告警，让用户看到真实状态而非虚假的 stopped。
		w.lastError = "停止失败: " + stopErr.Error()
	}
	w.mu.Unlock()
	if stopErr != nil {
		w.eng.event("error", "stop", w.id, name, "停止失败: "+stopErr.Error())
		w.transition(StateFailed, "error", "alert", "停止失败，进程可能仍在运行: %s", stopErr.Error())
		w.stopOnce.Do(func() { close(w.stopDone) })
		return
	}
	w.transition(StateStopped, "info", "stop", "已停止: %s", reason)
	w.stopOnce.Do(func() { close(w.stopDone) }) // 停止完成信号
}

// onExit 进程事实 → 监管裁决（九态折叠核心）。
func (w *worker) onExit(ev runtime.UnitExit) {
	w.mu.Lock()
	st := w.st
	pending := w.pendingRestart
	w.pid = 0
	w.row.Pid = 0
	w.lastExitCode = ev.ExitCode
	tail := ""
	if ev.ExitCode != 0 {
		tail = ev.Tail
		if len(tail) > 800 {
			tail = tail[len(tail)-800:]
		}
		if ev.Err != nil {
			w.lastError = fmt.Sprintf("exit=%d (%v)", ev.ExitCode, ev.Err)
		} else {
			w.lastError = fmt.Sprintf("exit=%d", ev.ExitCode)
		}
		if tail != "" {
			w.lastError += "\n" + tail
		}
	} else {
		w.lastError = ""
	}
	w.mu.Unlock()

	// reasonTail 进程输出尾部（截断到事件可承载的长度），随失败原因
	// 同时写入事件流与终端日志——用户在告警页面即可看到"为什么挂了"。
	reasonTail := tail
	if len(reasonTail) > 300 {
		reasonTail = "…\n" + reasonTail[len(reasonTail)-300:]
	}

	switch st {
	case StateStopping, StateStopped, StateFailed:
		// 预期退出/陈旧事件：仅记账。
	case StateStarting:
		// readyHold 内失败 = 启动失败：计入熔断窗口。
		reason := fmt.Sprintf("启动验证期失败 exit=%d", ev.ExitCode)
		if reasonTail != "" {
			reason += "\n" + reasonTail
		}
		w.failAttempt(reason)
	case StateRunning, StateDegraded:
		if pending {
			w.mu.Lock()
			w.pendingRestart = false
			w.mu.Unlock()
			w.scheduleBackoff("degraded 超时重启")
			return
		}
		if ev.ExitCode == 0 {
			// 正常退出（任务完成语义）：不重启。
			w.transition(StateStopping, "info", "stop", "正常退出（exit 0），不重启")
			w.transition(StateStopped, "info", "stop", "正常退出（exit 0），不重启")
			return
		}
		reason := fmt.Sprintf("运行中异常退出 exit=%d", ev.ExitCode)
		if reasonTail != "" {
			reason += "\n" + reasonTail
		}
		w.failAttempt(reason)
	}
}

// failAttempt 失败裁决：滑动窗口计数 → 熔断 failed / 退避 backoff。
func (w *worker) failAttempt(reason string) {
	w.mu.Lock()
	now := time.Now()
	if w.pol.WindowSec > 0 && now.Sub(w.windowStart) > time.Duration(w.pol.WindowSec)*time.Second {
		w.windowStart, w.windowFails = now, 0
	}
	if w.windowStart.IsZero() {
		w.windowStart = now
	}
	w.windowFails++
	w.row.RestartFailures++
	w.row.ConsecutiveFailures = w.windowFails
	fuse := w.pol.MaxRetries > 0 && w.windowFails >= w.pol.MaxRetries
	n := w.windowFails
	w.mu.Unlock()

	w.eng.event("warn", "backoff", w.id, w.defName(), reason)
	if fuse {
		// 迁移合法性：running 崩溃先经 backoff 再熔断（架构 §3.2 全表）。
		w.transition(StateBackoff, "warn", "backoff", "%s（熔断判定中）", reason)
		w.transition(StateFailed, "error", "alert",
			"熔断：窗口内失败 %d 次达上限，停止自动重启等待人工介入", n)
		return
	}
	w.scheduleBackoff(reason)
}

// scheduleBackoff 指数退避 backoffBase·2^(n-1) 封顶 backoffMax；快死防抖 +5s。
func (w *worker) scheduleBackoff(reason string) {
	w.mu.Lock()
	n := w.windowFails
	if n < 1 {
		n = 1
	}
	var wait time.Duration
	if w.pol.RestartMode == "periodic" {
		// 固定周期重启：异常退出后按恒定间隔拉起（默认/最小 3s）。
		wait = time.Duration(w.pol.PeriodicSec) * time.Second
	} else {
		// 指数退避：base·2^(n-1) 封顶 max（默认 2s → 300s）。
		// 必须先钳制指数再移位：n 无上限（默认 maxRetries=0/windowSec=0 时
		// windowFails 永不封顶），2s<<53 会在 int64 内回绕成约 1ms，
		// 使退避塌缩为毫秒级、形成重试风暴（默认配置约 4.5 小时可触达）。
		maxB := time.Duration(w.pol.BackoffMax) * time.Second
		if maxB <= 0 {
			maxB = 300 * time.Second
		}
		base := time.Duration(w.pol.BackoffBase) * time.Second
		if base <= 0 {
			base = 2 * time.Second
		}
		// 上限：base·2^(shift-1) 已 ≥ maxB 时直接取 maxB。
		const maxShift = 30
		shift := n - 1
		if shift < 0 {
			shift = 0
		}
		if shift > maxShift {
			wait = maxB
		} else {
			wait = base << uint(shift)
			if wait <= 0 || wait > maxB {
				wait = maxB
			}
		}
	}
	if !w.lastStartAt.IsZero() && time.Since(w.lastStartAt) < fastDeathSec+time.Second {
		wait += fastDeathAdd
	}
	w.nextRetryAt = time.Now().Add(wait)
	w.mu.Unlock()
	w.transition(StateBackoff, "warn", "backoff",
		"%s，退避 %s 后自动重试", reason, wait)
}

// reconcile 对账 tick：门控重查 / 启动超时与 readyHold / 退避到期 / degraded 超时。
func (w *worker) reconcile() {
	w.mu.Lock()
	st := w.st
	pid := w.pid
	now := time.Now()
	// 进程指标采集（running/degraded 时每 2s 采一次差分）。
	if (st == StateRunning || st == StateDegraded) && pid > 0 {
		lastCpu, lastAt := w.lastCpuTime, w.lastCpuAt
		w.mu.Unlock()
		cpuPct, cpuTime, memMB := procMetricsPct(pid, lastCpu, lastAt)
		w.mu.Lock()
		if cpuTime >= 0 {
			// 仅在有有效采样时推进状态与基线；否则保留上次基线，
			// 避免「基线归零 → 下一拍差分成假满负载」。
			w.curCpuPct, w.curCpuTimeSec, w.curMemMB = cpuPct, cpuTime, memMB
			w.lastCpuTime, w.lastCpuAt = cpuTime, now
		}
	}
	w.mu.Unlock()

	w.mu.Lock()
	st = w.st
	now = time.Now()
	switch st {
	case StateWaiting:
		// 依赖就绪重查（waiting 是门控排队态，周期性尝试放行）。
		w.mu.Unlock()
		w.tryLaunch()
		return
	case StateStarting:
		hold := time.Duration(w.pol.ReadyHoldSec) * time.Second
		if !w.l2FirstOkAt.IsZero() && now.Sub(w.l2FirstOkAt) >= hold {
			w.mu.Unlock()
			w.transition(StateRunning, "info", "start", "启动验证通过（L2 稳定 %s）", hold)
			return
		}
		// 启动超时必须纳入探针间隔：首次 L2 成功最晚要等一个 IntervalSec，
		// 用户把 intervalSec 调到 60 时，若仍按 ReadyHold+Timeout+pad 判定
		// （默认约 18s），健康服务会被当成启动失败处决。
		timeout := time.Duration(w.pol.IntervalSec+w.pol.ReadyHoldSec+w.pol.TimeoutSec)*time.Second + startTimeoutPad
		if now.Sub(w.startingAt) > timeout {
			w.mu.Unlock()
			w.failAttempt("启动验证超时")
			return
		}
	case StateBackoff:
		if !w.nextRetryAt.IsZero() && now.After(w.nextRetryAt) {
			w.mu.Unlock()
			// 走 transition 而非直接赋值：直接改 st 不产生事件、不写台账，
			// 依赖长期不就绪时 row.State 会永久停在 backoff（前端显示失真）。
			w.transition(StateWaiting, "info", "backoff", "退避结束，重新尝试启动")
			w.tryLaunch()
			return
		}
	case StateDegraded:
		if now.Sub(w.degradedSince) > time.Duration(w.pol.DegradedRestartSec)*time.Second {
			// degraded 超时重启：必须由这里直接调度，不能依赖 onExit 的
			// pendingRestart 分支——doStop 是同步的，退出事件到达时状态已是
			// Stopping/Stopped，onExit 只会走"仅记账"分支，pending 判定永不成立
			//（该特性原先因此从未生效，服务会永久停在 stopped）。
			w.mu.Unlock()
			w.eng.event("warn", "restart", w.id, w.defName(),
				fmt.Sprintf("degraded 持续超 %ds，按策略重启", w.pol.DegradedRestartSec))
			w.doStop("degraded 超时重启")
			w.scheduleBackoff("degraded 超时重启")
			return
		}
	}
	w.mu.Unlock()
}

// probeNow L2 探针（intervalSec 节流）：starting 记稳定起点；
// running 连续失败 ≥ 阈值 → degraded；degraded 连续成功 ≥ 恢复阈值 → running。
func (w *worker) probeNow() {
	w.mu.Lock()
	st := w.st
	if st != StateRunning && st != StateDegraded && st != StateStarting {
		w.mu.Unlock()
		return
	}
	if time.Since(w.lastProbeAt) < time.Duration(w.pol.IntervalSec)*time.Second {
		w.mu.Unlock()
		return
	}
	w.lastProbeAt = time.Now()
	target, timeout := w.pol.HealthTarget, time.Duration(w.pol.TimeoutSec)*time.Second
	probeFn := w.probe
	w.mu.Unlock()

	err := probeFn(target, timeout)

	w.mu.Lock()
	defer w.mu.Unlock()
	if err == nil {
		w.l2OkStreak++
		w.l2FailStreak = 0
		w.row.HealthState = "ok"
		if w.st == StateStarting && w.l2FirstOkAt.IsZero() {
			w.l2FirstOkAt = time.Now()
		}
		return
	}
	w.l2OkStreak = 0
	w.l2FailStreak++
	w.row.HealthState = "fail"
	// starting 的失败由启动超时统一裁决；running/degraded 在此裁决。
}

// applyProbeResult 探针结果的状态裁决（reconcile 后调用；测试可直接注入）。
func (w *worker) applyProbeResult() {
	w.mu.Lock()
	st, fails, oks := w.st, w.l2FailStreak, w.l2OkStreak
	threshold := w.pol.FailuresThreshold
	recover := w.pol.RecoverThreshold
	w.mu.Unlock()
	if st == StateRunning && threshold > 0 && fails >= threshold {
		w.mu.Lock()
		w.degradedSince = time.Now()
		w.mu.Unlock()
		w.transition(StateDegraded, "warn", "alert",
			"L2 探针连续失败 ≥ 阈值，进程存活但服务降级")
	}
	if st == StateDegraded && recover > 0 && oks >= recover {
		w.transition(StateRunning, "info", "start", "L2 探针恢复（连续 %d 次通过）", oks)
	}
}

// transition 状态迁移唯一入口：合法迁移断言 + 事件落库 + 台账即写。
func (w *worker) transition(to State, level, typ, msg string, args ...any) bool {
	w.mu.Lock()
	from := w.st
	if from == to {
		w.mu.Unlock()
		return true
	}
	if !CanTransition(from, to) {
		logx.Module("supervisor").Warn("非法状态迁移（记录后仍迁移）",
			"id", w.id, "from", from, "to", to)
	}
	w.st = to
	switch to {
	case StateRunning:
		if w.startedAt.IsZero() {
			w.startedAt = time.Now()
		}
		w.row.RestartTotal++
		w.row.StartedAt = w.startedAt
		w.row.ConsecutiveFailures = 0
	case StateStopped, StateFailed:
		w.startedAt = time.Time{}
		w.pid = 0
		w.row.Pid = 0
	}
	w.row.State = string(to)
	w.row.LastExitCode = w.lastExitCode
	w.row.LastError = w.lastError
	row, name := w.row, w.def.Name
	w.mu.Unlock()

	w.eng.saveRuntime(row)
	w.eng.event(level, typ, w.id, name, fmt.Sprintf(msg, args...))
	return true
}

func (w *worker) persistSnapshot() {
	w.mu.Lock()
	row := w.row
	row.State = string(w.st)
	row.Pid = w.pid
	if !w.startedAt.IsZero() {
		row.StartedAt = w.startedAt
	}
	w.row = row
	w.mu.Unlock()
	w.eng.saveRuntime(row)
}

func (w *worker) snapshot() Snap {
	w.mu.Lock()
	defer w.mu.Unlock()
	up := 0.0
	if w.st == StateRunning || w.st == StateDegraded {
		if !w.startedAt.IsZero() {
			up = time.Since(w.startedAt).Seconds()
		}
	}
	return Snap{
		State: string(w.st), Pid: w.pid, UptimeSec: up,
		RestartTotal: w.row.RestartTotal, RestartFailures: w.row.RestartFailures,
		ConsecutiveFailures: w.row.ConsecutiveFailures,
		HealthState: w.row.HealthState, LastError: w.row.LastError,
		LastExitCode: w.row.LastExitCode,
		CpuPct: w.curCpuPct, MemMB: w.curMemMB, CpuTimeSec: w.curCpuTimeSec,
	}
}

func (w *worker) defName() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.def.Name
}

func unitKey(id int) string { return runtime.UnitKeyFor(id) }

// markAdopted 认领对齐：直接置 running（平台重启恢复的专用迁移，不走
// transition 断言——stopped→running 在正常状态机中不存在）+ 事件留痕。
func (w *worker) markAdopted() {
	w.mu.Lock()
	w.st = StateRunning
	w.startedAt = time.Now()
	w.pid = 0 // pid 由 runtime 侧登记持有，snapshot 前补查
	if st, err := w.rt.Status(unitKey(w.id)); err == nil {
		w.pid = st.MainPID
	}
	w.row.State = string(w.st)
	w.row.Pid = w.pid
	w.row.StartedAt = w.startedAt
	w.row.HealthState = "n/a"
	row := w.row
	name := w.def.Name
	w.mu.Unlock()
	w.eng.saveRuntime(row)
	w.eng.event("info", "start", w.id, name, "孤儿认领：平台重启后零中断接管为运行中")
}