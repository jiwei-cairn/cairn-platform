//go:build windows

// procmetrics_windows.go — 进程级 CPU/内存采集（kernel32 + psapi，零第三方依赖）。
//
//	CPU  GetProcessTimes：进程内核态+用户态累计时间差分（与 linux /proc utime+stime 同源语义）
//	内存 GetProcessMemoryInfo：WorkingSetSize（RSS 等价量）
//
// 取代原先 tasklist 解析方案：不再每 2s spawn 一个子进程，也不再受
// tasklist 输出本地化（"无任务" / "No tasks"）影响，并补齐 CPU 百分比。
package supervisor

import (
	goruntime "runtime"
	"syscall"
	"time"
	"unsafe"
)

var (
	pmKernel32 = syscall.NewLazyDLL("kernel32.dll")
	pmPsapi    = syscall.NewLazyDLL("psapi.dll")

	procOpenProcess          = pmKernel32.NewProc("OpenProcess")
	procCloseHandle          = pmKernel32.NewProc("CloseHandle")
	procGetProcessTimes      = pmKernel32.NewProc("GetProcessTimes")
	procGetProcessMemoryInfo = pmPsapi.NewProc("GetProcessMemoryInfo")
)

const (
	// PROCESS_QUERY_LIMITED_INFORMATION：最小查询权限，避免对高完整性进程请求
	// PROCESS_QUERY_INFORMATION 被拒（Vista+ 语义）。
	processQueryLimitedInformation = 0x1000
	// PROCESS_QUERY_INFORMATION 兜底：部分旧系统对 LIMITED 支持不佳。
	processQueryInformation = 0x0400
)

// winFiletime 对应 Win32 FILETIME（100ns 计数，高低双 DWORD）。
type winFiletime struct {
	Low  uint32
	High uint32
}

func (f winFiletime) seconds() float64 {
	return float64(uint64(f.High)<<32|uint64(f.Low)) / 1e7 // 100ns → s
}

// processMemoryCounters 对应 Win32 PROCESS_MEMORY_COUNTERS。
type processMemoryCounters struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// openProcessForQuery 以最小必要权限打开进程句柄（失败返回 0）。
func openProcessForQuery(pid int) uintptr {
	for _, access := range []uintptr{processQueryLimitedInformation, processQueryInformation} {
		if h, _, _ := procOpenProcess.Call(access, 0, uintptr(uint32(pid))); h != 0 {
			return h
		}
	}
	return 0
}

// procCPUMem 读取进程累计 CPU 时间（秒）与工作集（MB）。
// 进程不存在或无权限时返回 (-1, 0)（与 linux 实现同约定）。
func procCPUMem(pid int) (cpuTimeSec, memMB float64) {
	if pid <= 0 {
		return -1, 0
	}
	h := openProcessForQuery(pid)
	if h == 0 {
		return -1, 0
	}
	defer func() { _, _, _ = procCloseHandle.Call(h) }()

	// 累计 CPU 时间 = 内核态 + 用户态。
	var creation, exit, kernel, user winFiletime
	r, _, _ := procGetProcessTimes.Call(
		h,
		uintptr(unsafe.Pointer(&creation)),
		uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r == 0 {
		return -1, 0
	}
	cpuTimeSec = kernel.seconds() + user.seconds()

	// 工作集内存（RSS 等价量）。
	var pmc processMemoryCounters
	pmc.Cb = uint32(unsafe.Sizeof(pmc))
	r, _, _ = procGetProcessMemoryInfo.Call(h, uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.Cb))
	if r != 0 {
		memMB = float64(pmc.WorkingSetSize) / (1024 * 1024)
	}
	return cpuTimeSec, memMB
}

// procMetricsPct 差分计算 CPU 百分比（与 linux 实现同语义）。
// lastCpuTime/lastAt 是上次采样值，返回 (cpuPct, cpuTimeSec, memMB)。
//
// 采集失败（进程刚退出/权限不足）时返回 cpuTimeSec=-1 且 cpuPct=0，调用方据此
// 判定「无有效采样」并保留既有基线——若把基线写成 0，下一次成功采样会算出
// 一个接近 100×numCPU 的假满负载尖峰。
func procMetricsPct(pid int, lastCpuTime float64, lastAt time.Time) (cpuPct, cpuTimeSec, memMB float64) {
	cpuTimeSec, memMB = procCPUMem(pid)
	if cpuTimeSec < 0 {
		return 0, -1, 0 // cpuPct=0（可展示）+ cpuTimeSec<0（无有效采样的哨兵）
	}
	if !lastAt.IsZero() {
		if dt := time.Since(lastAt).Seconds(); dt > 0 {
			cpuPct = 100 * (cpuTimeSec - lastCpuTime) / dt
			if cpuPct < 0 {
				cpuPct = 0
			}
			if cpuPct > 100*float64(numCPU) {
				cpuPct = 100 * float64(numCPU) // 多核可超 100%
			}
		}
	}
	return cpuPct, cpuTimeSec, memMB
}

// numCPU 逻辑处理器数（多核 CPU 百分比上限 = 100 × numCPU，与 linux 同语义）。
var numCPU = detectNumCPU()

// detectNumCPU GetActiveProcessorCount(ALL_PROCESSOR_GROUPS)，
// 失败时回退 GOMAXPROCS 视角的 runtime.NumCPU()。
func detectNumCPU() int {
	procGetActiveProcessorCount := pmKernel32.NewProc("GetActiveProcessorCount")
	if r, _, _ := procGetActiveProcessorCount.Call(uintptr(0xFFFF)); r > 0 {
		return int(r)
	}
	if n := goruntime.NumCPU(); n > 0 {
		return n
	}
	return 1
}
