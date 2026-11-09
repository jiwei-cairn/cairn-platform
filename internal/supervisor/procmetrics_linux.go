//go:build linux

// procmetrics_linux.go — 进程级 CPU/内存采集（/proc 文件系统，零依赖）。
package supervisor

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// procCPUMem 从 /proc/{pid}/stat 和 statm 读取进程 CPU 时间与 RSS。
// 返回 (cpuTimeSec, memMB)。进程不存在时返回 (-1, 0)。
func procCPUMem(pid int) (cpuTimeSec, memMB float64) {
	if pid <= 0 {
		return -1, 0
	}
	// /proc/{pid}/stat：utime(field14) + stime(field15)，单位 clock ticks。
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return -1, 0
	}
	s := string(b)
	// comm 可能含空格/括号——在最后一个 ')' 之后分割。
	idx := strings.LastIndexByte(s, ')')
	if idx < 0 {
		return -1, 0
	}
	fields := strings.Fields(s[idx+1:])
	if len(fields) < 13 { // 需要到 field15（idx+13 after ')' offset=0 is field3）
		return -1, 0
	}
	utime, _ := strconv.ParseFloat(fields[11], 64) // field14 → idx11 after ')'
	stime, _ := strconv.ParseFloat(fields[12], 64) // field15 → idx12 after ')'
	const clkTck = 100.0                            // Linux 默认 CLK_TCK
	cpuTimeSec = (utime + stime) / clkTck

	// /proc/{pid}/statm：field2 = RSS pages。
	b2, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err == nil {
		f2 := strings.Fields(string(b2))
		if len(f2) >= 2 {
			rssPages, _ := strconv.ParseFloat(f2[1], 64)
			memMB = rssPages * float64(os.Getpagesize()) / (1024 * 1024)
		}
	}
	return cpuTimeSec, memMB
}

// procMetricsPct 差分计算 CPU 百分比。
// lastCpuTime/lastAt 是上次采样值，返回 (cpuPct, cpuTimeSec, memMB)。
//
// 采集失败（进程刚退出/无权限）时返回 cpuTimeSec=-1 且 cpuPct=0，调用方据此
// 判定「无有效采样」并保留既有基线——若把基线写成 0，下一次成功采样会算出
// 一个接近 100×numCPU 的假满负载尖峰。
func procMetricsPct(pid int, lastCpuTime float64, lastAt time.Time) (cpuPct, cpuTimeSec, memMB float64) {
	cpuTimeSec, memMB = procCPUMem(pid)
	if cpuTimeSec < 0 {
		return 0, -1, 0 // cpuPct=0（可展示）+ cpuTimeSec<0（无有效采样的哨兵）
	}
	if !lastAt.IsZero() {
		dt := time.Since(lastAt).Seconds()
		if dt > 0 {
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

var numCPU = 1

func init() {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err == nil {
		numCPU = strings.Count(string(b), "processor")
		if numCPU < 1 {
			numCPU = 1
		}
	}
}