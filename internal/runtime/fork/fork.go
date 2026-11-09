// Package fork 是 runtime 的 os/exec 父子模型实现（M1 方案 §3）。
//
// 被管服务是平台的直接子进程：cmd.Wait() 事件驱动感知退出、退出码精确、
// stdout/stderr 管道接管（尾部行缓冲，D4 崩溃现场）。平台崩溃时服务成为
// 孤儿继续运行，平台重启经 Adopt 零中断认领（linux /proc 校验；windows
// taskkill 清场后重拉）。pdeathsig 不设（默认关）：保证孤儿存活走认领路径。
package fork

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/runtime"
)

// stopGrace Stop 未显式给超时的默认宽限（与文档停止序列一致）。
const (
	stopGrace      = 10 * time.Second
	killGrace      = 3 * time.Second
	adoptProbeSec  = 2 * time.Second // 认领进程探活周期（supervisor 对账同节奏）
	tailLines      = 200             // 崩溃现场尾部行数（D4）
	exitsBuffer    = 64
)

var errClosed = errors.New("fork runtime 已关闭")

// unit 一个受管进程的全部内存态（亲管或认领）。
type unit struct {
	def     runtime.UnitDef
	mu      sync.Mutex
	pid     int
	job     uintptr // Windows Job Object 句柄（进程树终结用；linux 恒 0）
	started bool    // 亲管且 Wait 未返回，或认领存活
	adopted bool
	cmd     *exec.Cmd
	done    chan struct{} // 亲管进程 Wait 完成信号
	exitCh  bool          // exit 事件已发出（防重）
	exitCode int
	exitErr  error
	// expectedStop 平台已发起过 Stop：此后进程的退出（含宽限期后强杀导致的
	// 非零退出码）属「预期停止」而非「异常崩溃」。Status 据此折叠为 stopped，
	// 否则 Windows 上不响应 CTRL_BREAK 的进程会被强杀成 exit=1 并被误判 failed。
	expectedStop bool
	since    time.Time
	starttime string        // linux /proc starttime（认领校验用）
	tail    *tailBuffer
}

func (u *unit) alive() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.started
}

// Runtime 实现 runtime.Runtime 与 runtime.ExitWatcher。
type Runtime struct {
	mu     sync.Mutex
	units  map[string]*unit
	root   string // 登记目录（data/runtime）
	exits  chan runtime.UnitExit
	closed bool
}

// New 构造 fork 运行时；root 为登记落盘目录（空 = data/runtime）。
func New(root string) *Runtime {
	if root == "" {
		root = filepath.Join("data", "runtime")
	}
	return &Runtime{
		units: make(map[string]*unit),
		root:  root,
		exits: make(chan runtime.UnitExit, exitsBuffer),
	}
}

// Exits 实现 runtime.ExitWatcher：事件驱动退出通道（含退出码/尾部日志）。
func (r *Runtime) Exits() <-chan runtime.UnitExit { return r.exits }

func (r *Runtime) get(name string) *unit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.units[name]
}

// EnsureUnit 幂等登记期望态：无则建（stopped 态），有则仅更新 def 并落盘。
func (r *Runtime) EnsureUnit(def runtime.UnitDef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errClosed
	}
	name := runtime.UnitKeyFor(def.ServiceID)
	if u, ok := r.units[name]; ok {
		u.mu.Lock()
		u.def = def
		u.mu.Unlock()
		return nil
	}
	u := &unit{def: def, done: make(chan struct{}), tail: newTailBuffer(tailLines)}
	r.units[name] = u
	return nil
}

// RemoveUnit 停止（若活；失败告警不阻断）并清除登记。
func (r *Runtime) RemoveUnit(name string) error {
	if u := r.get(name); u != nil && u.alive() {
		if err := r.Stop(name, stopGrace); err != nil {
			logx.Module("fork").Warn("删除单元前停止失败（继续清理登记）", "unit", name, "err", err)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.units, name)
	r.clearReg(name)
	return nil
}

// SetEnabled 登记位翻转（开机自启语义在 supervisor/编排层，这里只记台账）。
func (r *Runtime) SetEnabled(name string, enabled bool) error {
	u := r.get(name)
	if u == nil {
		return fmt.Errorf("单元不存在: %s", name)
	}
	u.mu.Lock()
	u.def.Enabled = enabled
	u.mu.Unlock()
	return nil
}

// Start 拉起子进程：setsid（Win: 新进程组）+ env 合并 + stdout/stderr 接管。
// fork 即活，立即返回；就绪判定（L2/readyHold）归 supervisor worker。
func (r *Runtime) Start(name string, timeout time.Duration) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errClosed
	}
	u, ok := r.units[name]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("单元不存在: %s", name)
	}
	u.mu.Lock()
	if u.started {
		u.mu.Unlock()
		return nil // 幂等
	}
	def := u.def
	u.mu.Unlock()

	// 相对路径必须解析为绝对路径：Windows 上 cmd.Dir 会导致相对 exePath
	// 被拼接到 workDir 下再查找（路径双重叠加 → file not found）。
	// 仅处理含路径分隔符的相对路径（纯命令名如 cmd/sh 交给 PATH 查找）。
	exePath := def.ExePath
	if !filepath.IsAbs(exePath) && strings.ContainsRune(exePath, filepath.Separator) {
		if abs, err := filepath.Abs(exePath); err == nil {
			exePath = abs
		}
	}
	workDir := def.WorkDir
	if workDir != "" && !filepath.IsAbs(workDir) {
		if abs, err := filepath.Abs(workDir); err == nil {
			workDir = abs
		}
	}
	cmd := exec.Command(exePath, def.Args...)
	cmd.Dir = workDir
	cmd.Env = mergeEnv(os.Environ(), def.Env)
	cmd.WaitDelay = 3 * time.Second // 进程死后管道最多等 3s（防子进程持有句柄卡 Wait）
	job := newJob()                 // Windows：进程树 Job Object（停止一次终结整树）
	cmd.SysProcAttr = sysProcAttr(job)
	tail := newTailBuffer(tailLines)
	cmd.Stdout = tail
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		closeJob(job)
		return err
	}

	u.mu.Lock()
	u.cmd = cmd
	u.job = job
	u.pid = cmd.Process.Pid
	u.started = true
	u.adopted = false
	u.exitCh = false
	u.exitCode = 0
	u.exitErr = nil
	u.expectedStop = false
	u.since = time.Now()
	u.tail = tail
	u.done = make(chan struct{})
	u.starttime = procStartTime(cmd.Process.Pid) // linux starttime / windows 创建时间（认领锚点）
	u.mu.Unlock()
	assignJob(job, cmd)

	r.saveReg(u)
	logx.Module("fork").Info("进程已启动", "unit", name, "pid", u.pid)

	go func() {
		err := cmd.Wait()
		r.onExit(u, name, err)
	}()
	return nil
}

// onExit 亲管进程 Wait 返回：记录结果、发退出事件（非阻塞，防 supervisor
// 阻塞拖死 Wait goroutine——通道满则丢弃并告警）。
func (r *Runtime) onExit(u *unit, name string, err error) {
	u.mu.Lock()
	u.started = false
	u.exitErr = err
	if u.cmd != nil && u.cmd.ProcessState != nil {
		u.exitCode = u.cmd.ProcessState.ExitCode()
	} else if err != nil {
		u.exitCode = -1
	}
	close(u.done)
	job := u.job
	u.job = 0 // job 内已无进程，句柄由 closeJob 释放
	pid, code := u.pid, u.exitCode
	tail := u.tail.String()
	first := !u.exitCh
	u.exitCh = true
	u.mu.Unlock()
	closeJob(job)

	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed || !first {
		return
	}
	ev := runtime.UnitExit{
		ServiceID: u.def.ServiceID, Name: name, ExitCode: code,
		Err: err, Tail: tail, At: time.Now(),
	}
	select {
	case r.exits <- ev:
	default:
		logx.Module("fork").Warn("退出事件通道已满，事件被丢弃", "unit", name, "pid", pid)
	}
}

// Stop 停止序列：优雅请求（linux 进程组 SIGTERM / windows CTRL_BREAK_EVENT）
// → 宽限（timeout）→ 强杀兜底，同步等待退出。
func (r *Runtime) Stop(name string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = stopGrace
	}
	u := r.get(name)
	if u == nil {
		return fmt.Errorf("单元不存在: %s", name)
	}
	if !u.alive() {
		return nil
	}
	u.mu.Lock()
	pid, adopted, done := u.pid, u.adopted, u.done
	cmd := u.cmd
	// 标记预期停止：本次 Stop 甚至强杀导致的退出都不应被判为异常崩溃。
	u.expectedStop = true
	u.mu.Unlock()

	// 宽限期：仅在优雅请求确实送达时等待（送达失败说明无人会响应，
	// 等满超时纯属浪费——直接进入强杀）。
	if requestStop(u) && waitExit(adopted, pid, done, timeout) {
		logx.Module("fork").Info("进程已优雅退出", "unit", name, "pid", pid)
		return nil
	}
	// 宽限期结束仍未退出：硬杀兜底。
	// terminateTree 负责整棵进程树（linux 进程组 SIGKILL / windows Job Object
	// 或 taskkill /T）；Process.Kill（TerminateProcess）作为直接子进程的最可靠
	// 单进程原语补充——Job 分配可能静默失败、taskkill 在受限环境可能被拒绝。
	terminateTree(u)
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if waitExit(adopted, pid, done, killGrace) {
		logx.Module("fork").Warn("进程经强杀退出", "unit", name, "pid", pid)
		return nil
	}
	if adopted {
		// 认领进程：标记退出（探活协程会发事件，这里兜底状态）
		u.mu.Lock()
		u.started = false
		u.exitCode = -1
		u.mu.Unlock()
		_ = killByPid(pid)
		return nil
	}
	return fmt.Errorf("停止失败：进程 %d 在强杀后仍未退出", pid)
}

// Restart 重启（Stop + Start）。
func (r *Runtime) Restart(name string, timeout time.Duration) error {
	if err := r.Stop(name, timeout); err != nil {
		return err
	}
	return r.Start(name, timeout)
}

// Status 查询进程事实：活=running；退出按退出码分 stopped/failed；无登记 notFound。
// 平台已发起 Stop 的单元，其退出一律折叠为 stopped（强杀退出码不代表崩溃）。
func (r *Runtime) Status(name string) (runtime.UnitStatus, error) {
	u := r.get(name)
	if u == nil {
		return runtime.UnitStatus{Name: name, State: runtime.UnitNotFound}, nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	st := runtime.UnitStatus{Name: name, MainPID: u.pid, Since: u.since}
	switch {
	case u.started:
		st.State = runtime.UnitRunning
	case u.expectedStop:
		// 平台主动停止流程内的退出：语义是 stopped，不是 failed。
		st.State = runtime.UnitStopped
	case u.exitCode != 0:
		st.State = runtime.UnitFailed
		st.ExitCode = u.exitCode
	default:
		st.State = runtime.UnitStopped
	}
	return st, nil
}

// Pids 主进程 PID 列表（M1 最小实现：主 PID；子孙扫描留 M3 采集时）。
func (r *Runtime) Pids(name string) ([]int, error) {
	u := r.get(name)
	if u == nil {
		return nil, fmt.Errorf("单元不存在: %s", name)
	}
	if !u.alive() {
		return nil, nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return []int{u.pid}, nil
}

// Close 关闭运行时（不再发退出事件）。
func (r *Runtime) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
}

// waitExit 等待退出：亲管等 done 通道；认领进程轮询探活。
func waitExit(adopted bool, pid int, done chan struct{}, timeout time.Duration) bool {
	deadline := time.After(timeout)
	if !adopted {
		select {
		case <-done:
			return true
		case <-deadline:
			return false
		}
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			return false
		case <-tick.C:
			if !probeAlive(pid) {
				return true
			}
		}
	}
}

// mergeEnv 基底 + 覆盖合并（同键后者胜，保持顺序稳定便于指纹计算）。
func mergeEnv(base []string, over map[string]string) []string {
	if len(over) == 0 {
		return base
	}
	keys := make([]string, 0, len(over))
	for k := range over {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(base)+len(over))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if _, hit := over[k]; hit {
			continue // 覆盖项稍后按序追加
		}
		out = append(out, kv)
	}
	for _, k := range keys {
		out = append(out, k+"="+over[k])
	}
	return out
}

// tailBuffer 行缓冲 ring：Write 按 LF 切行、超限掐头；String 导出尾部现场。
//
// 除行数上限外还有字节上限：子进程可能输出无换行的超长行（二进制垃圾、
// base64、core dump 文本片段），仅限行数会让整段驻留内存并可能撑爆平台。
type tailBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

// maxTailLineBytes 单行保留上限（超出截断并标注）。
const maxTailLineBytes = 8 << 10

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	start := 0
	for i, b := range p {
		if b == '\n' {
			t.append(string(p[start:i]))
			start = i + 1
		}
	}
	if start < len(p) {
		t.append(string(p[start:]))
	}
	return len(p), nil
}

func (t *tailBuffer) append(line string) {
	// 单行字节上限：超长行截断保留首段（尾部现场主要看开头与结尾，
	// 这里保留开头即可定位，同时避免无换行的海量输出常驻内存）。
	if len(line) > maxTailLineBytes {
		line = line[:maxTailLineBytes] + "…（单行已截断）"
	}
	t.lines = append(t.lines, line)
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := ""
	for i, l := range t.lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}