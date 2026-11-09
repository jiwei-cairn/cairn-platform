//go:build linux

// proc_linux.go — Linux 平台原语：setsid、进程组信号、/proc 校验。
package fork

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/runtime"
)

// sysProcAttr setsid 新会话：子进程自成进程组组长，kill(-pid) 组信号可达
// 全组（含 fork 出的孙进程）；不设 pdeathsig（孤儿存活 → 认领路径）。
// job 恒 0（Job Object 是 Windows 机制）。
func sysProcAttr(job uintptr) *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

func newJob() uintptr { return 0 }

func assignJob(uintptr, interface{}) {} // linux：setsid 已覆盖组控制，无需 Job

// requestStop 优雅请求：向进程组发 SIGTERM。
// 返回是否成功送达——未送达（如进程已退出）时调用方跳过宽限期直接强杀，
// 避免为一次注定无人响应的请求白等一个 Stop 超时。
func requestStop(u *unit) bool {
	// 持锁取值：u.pid 在 Start/adoptLive 中持锁写入，无锁读属数据竞争
	//（race detector 会报），此处与 proc_windows.go 的实现对齐。
	u.mu.Lock()
	pid := u.pid
	u.mu.Unlock()
	if pid > 0 {
		return syscall.Kill(-pid, syscall.SIGTERM) == nil
	}
	return false
}

// terminateTree 强杀兜底：进程组 SIGKILL。
func terminateTree(u *unit) {
	u.mu.Lock()
	pid := u.pid
	u.mu.Unlock()
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

func closeJob(uintptr) {}

func killByPid(pid int) error {
	if pid == 0 {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.EPERM {
		return err
	}
	return nil
}

// probeAlive kill(pid,0)：ESRCH=已死；EPERM=存活（他人进程）。
func probeAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// procStartTime /proc/{pid}/stat 第 22 字段 starttime（时钟滴答），
// PID 复用防护锚点（supervisord zombie_reaper 同款手法）。
func procStartTime(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return ""
	}
	fields := bytes.Fields(b[i+1:])
	if len(fields) < 20 {
		return ""
	}
	return string(fields[19])
}

// processExe /proc/{pid}/exe 符号链接即真实可执行路径。
func processExe(pid int) string {
	s, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return ""
	}
	return s
}

// adoptLive linux 认领：存活 + starttime 一致 + exe 前缀匹配 → 零中断接管。
//
// 安全网：存活但校验不通过（PID 已被复用 / 映像不符 / 无锚点）时必须清场——
// 否则编排层会按 autoStart 再拉一个，造成端口与资源双重占用。注意此时
// 不能对该 PID 动手（可能是无关进程）：仅当「创建时间锚点存在且一致」而
// 仅映像路径不符时才认定为我们的旧实例并清场；锚点不一致一律视为陌生进程，
// 只放弃认领、不杀（交由编排层重拉会端口冲突，但误杀无关进程风险更高）。
func (r *Runtime) adoptLive(name string, def runtime.UnitDef, reg *regEntry) bool {
	if !probeAlive(reg.PID) {
		return false // 已退出：无需清场。
	}
	st := ""
	if reg.StartTime != "" {
		st = procStartTime(reg.PID)
		if st == "" || st != reg.StartTime {
			// PID 复用或无法取锚点：不是我们的进程，放弃认领但绝不误杀。
			logx.Module("fork").Warn("旧实例 PID 已被复用或锚点不可取，放弃认领",
				"unit", name, "pid", reg.PID)
			return false
		}
	}
	if reg.Exe != "" {
		exe := processExe(reg.PID)
		want := strings.TrimSuffix(reg.Exe, "/") + string(os.PathSeparator)
		if exe == "" || !strings.HasPrefix(exe+string(os.PathSeparator), want) {
			// 锚点一致（确是同一进程）但映像不符：清场防重复拉起。
			logx.Module("fork").Warn("旧实例映像不符，先清场再重拉",
				"unit", name, "pid", reg.PID, "exe", exe)
			_ = killByPid(reg.PID)
			time.Sleep(200 * time.Millisecond)
			return false
		}
	}
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