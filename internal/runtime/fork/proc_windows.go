//go:build windows

// proc_windows.go — Windows 平台原语：Job Object 进程树管理 + 孤儿认领。
//
// 停止语义：优雅请求 = GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT)（子进程以
// CREATE_NEW_PROCESS_GROUP 启动，Go 运行时将其映射为 os.Interrupt，等价
// POSIX SIGTERM 的「请求退出」）；宽限期结束后 = TerminateJobObject 一次终结
// 整树（含孙进程），无 Job 时退化为 taskkill /F /T。
// 登录/注销类无控制台场景下 CTRL_BREAK 可能不可达，此时宽限期后直接硬杀。
package fork

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/runtime"
)

var (
	modkernel32             = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW    = modkernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJob  = modkernel32.NewProc("AssignProcessToJobObject")
	procOpenProcess         = modkernel32.NewProc("OpenProcess")
	procTerminateJobObject  = modkernel32.NewProc("TerminateJobObject")
	procCloseHandle         = modkernel32.NewProc("CloseHandle")
	procGetProcessTimes     = modkernel32.NewProc("GetProcessTimes")
	procGetExitCodeProcess  = modkernel32.NewProc("GetExitCodeProcess")
	procQueryFullProcessImg = modkernel32.NewProc("QueryFullProcessImageNameW")
	procGenerateConsoleCtrl = modkernel32.NewProc("GenerateConsoleCtrlEvent")
)

const (
	// 进程查询权限：LIMITED 是 Vista+ 的最小查询权限（避免对高完整性进程
	// 请求 QUERY_INFORMATION 被拒）；旧系统不支持时回退 QUERY_INFORMATION。
	processQueryLimitedInformation = 0x1000
	processQueryInformation        = 0x0400

	// stillActive GetExitCodeProcess 的「仍在运行」哨兵值。
	stillActive = 259

	// ctrlBreakEvent 控制台控制事件：Windows 上最接近 POSIX SIGTERM 的优雅
	// 停止请求（Go 子进程运行时将其映射为 os.Interrupt）。
	ctrlBreakEvent = 1

	// maxLongPath QueryFullProcessImageNameW 输出缓冲（字符数）。
	maxLongPath = 32767
)

// winFiletime 对应 Win32 FILETIME（100ns 计数，高低双 DWORD）。
type winFiletime struct {
	Low  uint32
	High uint32
}

// ticks 返回 100ns 原始计数（PID 复用锚点用原始值，不做单位换算以免精度损失）。
func (f winFiletime) ticks() uint64 { return uint64(f.High)<<32 | uint64(f.Low) }

// openProcessQuery 以查询权限打开进程（失败返回 0）。
func openProcessQuery(pid int) uintptr {
	if pid <= 0 {
		return 0
	}
	for _, access := range []uintptr{processQueryLimitedInformation, processQueryInformation} {
		if h, _, _ := procOpenProcess.Call(access, 0, uintptr(uint32(pid))); h != 0 {
			return h
		}
	}
	return 0
}

// processCreationTime 进程创建时间（FILETIME 原始计数，十进制串）。
// 该值在进程整个生命周期内不变，是 PID 复用的可靠锚点，对应 linux 侧
// /proc/{pid}/stat 的 starttime 字段。
func processCreationTime(pid int) (string, bool) {
	h := openProcessQuery(pid)
	if h == 0 {
		return "", false
	}
	defer func() { _, _, _ = procCloseHandle.Call(h) }()
	var creation, exit, kernel, user winFiletime
	r, _, _ := procGetProcessTimes.Call(h,
		uintptr(unsafe.Pointer(&creation)),
		uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		return "", false
	}
	return strconv.FormatUint(creation.ticks(), 10), true
}

// winProcessExe 进程映像真实路径（对应 linux 侧 /proc/{pid}/exe）。
func winProcessExe(pid int) string {
	h := openProcessQuery(pid)
	if h == 0 {
		return ""
	}
	defer func() { _, _, _ = procCloseHandle.Call(h) }()
	buf := make([]uint16, maxLongPath)
	size := uint32(len(buf))
	r, _, _ := procQueryFullProcessImg.Call(h, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)))
	if r == 0 || size == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:size])
}

// AssignProcessToJobObject 要求进程句柄具备 PROCESS_SET_QUOTA 与 PROCESS_TERMINATE
// 两项权限（MSDN: AssignProcessToJobObject → hProcess）。常量自带，不依赖
// syscall 是否导出：
//
//	PROCESS_SET_QUOTA  0x0100  作业配额相关设置（分配作业所必需）
//	PROCESS_TERMINATE  0x0001  终止进程
//
// 早期实现误用 0x0200（= PROCESS_SET_INFORMATION），致 AssignProcessToJobObject
// 恒失败（ERROR_ACCESS_DENIED），Job Object 从未真正持有进程，整树终结静默
// 退化为 taskkill。
const (
	processSetQuota  = 0x0100
	processTerminate = 0x0001
)

// newJob 创建 Job Object（返回 0 表示失败，进程照常启动只是失去树控）。
func newJob() uintptr {
	h, _, _ := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return 0
	}
	return h
}

// assignJob 把刚启动的子进程挂入 Job Object。分配失败时记录告警——
// 常见原因：父进程（console）本身已在其他 Job Object 中且未设
// BREAKAWAY，Windows 不允许跨 Job 分配（此时终止依赖 Process.Kill 兜底）。
func assignJob(job uintptr, cmd *exec.Cmd) {
	if job == 0 || cmd.Process == nil {
		return
	}
	if h, _, _ := procOpenProcess.Call(uintptr(processSetQuota|processTerminate), 0, uintptr(cmd.Process.Pid)); h != 0 {
		r1, _, _ := procAssignProcessToJob.Call(job, h)
		if r1 == 0 {
			logx.Module("fork").Warn("Job Object 分配失败（终止将依赖 Process.Kill 兜底）",
				"pid", cmd.Process.Pid)
		}
		_, _, _ = procCloseHandle.Call(h)
	} else {
		logx.Module("fork").Warn("OpenProcess 失败（终止将依赖 taskkill 兜底）",
			"pid", cmd.Process.Pid)
	}
}

// sysProcAttr 新进程组（Job 挂载在 assignJob 完成）。
func sysProcAttr(job uintptr) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// requestStop 优雅请求：向受管进程所在控制台发送 CTRL_BREAK_EVENT。
//
// 受管进程以 CREATE_NEW_PROCESS_GROUP 启动（sysProcAttr），同组进程可接收
// 该事件——Go 服务运行时将其映射为 os.Interrupt，等价 POSIX SIGTERM 的
// 「请求退出」语义。注意 GenerateConsoleCtrlEvent 只能作用于「与控制台同组
// 或同会话」的进程组，且需要进程组 ID（即组长 PID）。
//
// 返回是否成功送达。无控制台的形态（Windows 服务）恒失败，此时调用方跳过
// 宽限期直接强杀——避免每次停止都白等一个 Stop 超时；语义不退化为「无法停止」。
func requestStop(u *unit) bool {
	u.mu.Lock()
	pid := u.pid
	u.mu.Unlock()
	if pid <= 0 {
		return false
	}
	r, _, err := procGenerateConsoleCtrl.Call(uintptr(ctrlBreakEvent), uintptr(uint32(pid)))
	if r == 0 {
		logx.Module("fork").Debug("优雅停止请求未送达（直接进入强杀）", "pid", pid, "err", err)
		return false
	}
	return true
}

// terminateTree 终结 Job Object 内全部进程（硬杀兜底）。
func terminateTree(u *unit) {
	u.mu.Lock()
	job, pid := u.job, u.pid
	u.mu.Unlock()
	if job != 0 {
		if r, _, _ := procTerminateJobObject.Call(job, 1); r != 0 {
			return
		}
	}
	// 无 job 兜底：taskkill（可能被环境拒绝，尽力而为）。
	_ = taskkill(pid, true)
}

// closeJob 释放 Job Object 句柄。
func closeJob(job uintptr) {
	if job != 0 {
		_, _, _ = procCloseHandle.Call(job)
	}
}

// killByPid 按 PID 强杀（认领清场专用：无 job 的外来旧实例）。
func killByPid(pid int) error {
	if pid == 0 {
		return nil
	}
	return taskkill(pid, true)
}

func taskkill(pid int, force bool) error {
	args := []string{"/T", "/PID", fmt.Sprintf("%d", pid)}
	if force {
		args = append([]string{"/F"}, args...)
	}
	out, err := exec.Command("taskkill", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("taskkill: %v: %s", err, string(out))
	}
	return nil
}

// probeAlive 探活：OpenProcess 查询句柄 + GetExitCodeProcess。
// STILL_ACTIVE 表示存活；句柄打开失败（进程已消失）视为已退出。
func probeAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h := openProcessQuery(pid)
	if h == 0 {
		return false
	}
	defer func() { _, _, _ = procCloseHandle.Call(h) }()
	var code uint32
	r, _, _ := procGetExitCodeProcess.Call(h, uintptr(unsafe.Pointer(&code)))
	if r == 0 {
		return false
	}
	return code == stillActive
}

// procStartTime 进程创建时间串（PID 复用防护锚点，与 linux starttime 同用途）。
func procStartTime(pid int) string {
	st, ok := processCreationTime(pid)
	if !ok {
		return ""
	}
	return st
}

// adoptLive windows 认领：存活 + 创建时间一致 + 映像路径匹配 → 零中断接管。
//
// 与 linux 分支同语义，差异仅在取值方式（GetProcessTimes 创建时间 对应
// /proc starttime；QueryFullProcessImageNameW 对应 /proc/{pid}/exe）。
// 平台重启后被管服务由此免于「清场重拉」，与 linux 行为对齐。
//
// 安全网：若进程存活但无法认领（无锚点 / 锚点不匹配 / 映像不符），必须清场——
// 否则编排层会按 autoStart 再拉一个，造成端口与资源双重占用。
func (r *Runtime) adoptLive(name string, def runtime.UnitDef, reg *regEntry) bool {
	if !probeAlive(reg.PID) {
		return false // 已退出：无需清场，直接由编排层重拉。
	}
	if r.adoptable(reg) {
		u := r.get(name)
		if u == nil {
			return false
		}
		u.mu.Lock()
		u.pid = reg.PID
		u.adopted = true
		u.started = true
		u.since = reg.StartedAt
		u.starttime = reg.StartTime
		u.mu.Unlock()
		logx.Module("fork").Info("孤儿认领成功（零中断）", "unit", name, "pid", reg.PID)
		r.startAdoptWatch(u, name)
		return true
	}
	// 存活但不可认领：清场，防重复拉起。
	logx.Module("fork").Warn("旧实例无法认领（锚点缺失或不匹配），先清场再重拉",
		"unit", name, "pid", reg.PID)
	_ = killByPid(reg.PID)
	time.Sleep(200 * time.Millisecond)
	return false
}

// adoptable 判定登记进程是否可安全认领。
//
// 两道校验缺一不可：
//  1. 创建时间一致 —— PID 复用的唯一可靠防线。不一致说明该 PID 已属于
//     另一个无关进程，此时认领会把无关进程纳入监管（后续停止会误杀它）。
//  2. 映像路径一致 —— 防止「同一 PID 号、同创建时间巧合」以外的错配，
//     并确认该进程确实是我们安装的可执行文件。
func (r *Runtime) adoptable(reg *regEntry) bool {
	if reg.StartTime == "" {
		return false // 旧登记（早于本实现）无锚点：保守不认领。
	}
	if st := procStartTime(reg.PID); st == "" || st != reg.StartTime {
		return false
	}
	if reg.Exe != "" {
		exe := winProcessExe(reg.PID)
		if exe == "" || !sameExePath(exe, reg.Exe) {
			return false
		}
	}
	return true
}

// sameExePath 比较两个映像路径是否指向同一文件。
//
// 不能只做前缀匹配：Windows 路径大小写不敏感（NTFS），且登记里的 resolvedExe
// 可能经 EvalSymlinks 解析、而 QueryFullProcessImageNameW 返回的是映像路径，
// 两者可能在 8.3 短名 / 大小写 / 分隔符上不同。这里做规范化后比较：
// 统一分隔符、清理冗余段、大小写折叠。
func sameExePath(got, want string) bool {
	g := normalizeExePath(got)
	w := normalizeExePath(want)
	if g == "" || w == "" {
		return false
	}
	return g == w
}

func normalizeExePath(p string) string {
	if p == "" {
		return ""
	}
	p = filepath.Clean(strings.ReplaceAll(p, "/", `\`))
	return strings.ToLower(p)
}