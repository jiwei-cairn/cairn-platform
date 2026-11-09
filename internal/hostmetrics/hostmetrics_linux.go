//go:build linux

// hostmetrics_linux.go — 大盘主机资源概览（真实读取 /proc 与 statfs）。
package hostmetrics

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// HostMetrics 实现 api.HostSource（linux：/proc + statfs）。
type HostMetrics struct{}

// dataDirOverride 磁盘探针的卷锚点（由 main 装配时注入 config.DataDir()）。
// 为空表示未注入，回退到 CWD 相对的 "data"（开发形态）。
var dataDirOverride string

// SetDataDir 注入数据目录，使磁盘使用率读取数据实际所在的卷。
// 须在首次读取指标前调用（main 装配阶段）。
func SetDataDir(dir string) { dataDirOverride = dir }

var (
	hmOnce                  sync.Mutex
	hmLastAt                time.Time
	hmLastIdle, hmLastTotal float64
)

// init 预置 CPU 基线采样：使大盘首个请求即返回真实差分值（而非首次采样无差分返回 0）。
// 进程启动到首次 HTTP 请求之间的间隔即为差分窗口。
func init() {
	if idle, total, ok := readProcStatCPU(); ok {
		hmLastIdle, hmLastTotal, hmLastAt = idle, total, time.Now()
	}
}

// HostMetrics cpu（/proc/stat 差分）+ mem（/proc/meminfo）+ disk（data 目录 statfs）。
func (HostMetrics) HostMetrics() (cpu, mem, disk float64) {
	cpu = hostCPU()
	mem = hostMem()
	disk = hostDisk()
	return
}

// readProcStatCPU 读 /proc/stat 首行聚合：idle（含 iowait）与 total 累计 jiffies。
func readProcStatCPU() (idle, total float64, ok bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		for i, f := range fields {
			v, _ := strconv.ParseFloat(f, 64)
			total += v
			if i == 3 || i == 4 { // idle + iowait
				idle += v
			}
		}
		return idle, total, true
	}
	return 0, 0, false
}

func hostCPU() float64 {
	idle, total, ok := readProcStatCPU()
	if !ok {
		return 0
	}
	hmOnce.Lock()
	defer hmOnce.Unlock()
	now := time.Now()
	// 基线缺失或计数器未推进（窗口过短）：本次仅建立基线。
	if hmLastAt.IsZero() || total <= hmLastTotal || !now.After(hmLastAt) {
		hmLastIdle, hmLastTotal, hmLastAt = idle, total, now
		return 0
	}
	pct := 100 * (1 - (idle-hmLastIdle)/(total-hmLastTotal))
	hmLastIdle, hmLastTotal, hmLastAt = idle, total, now
	return clampPct(pct)
}

func hostMem() float64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	var total, avail float64
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseFloat(fields[1], 64)
		switch fields[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	if total <= 0 {
		return 0
	}
	return clampPct(100 * (total - avail) / total)
}

// hostDisk 与 windows 侧对齐：优先 data 目录所在卷，缺失时回退当前工作目录、根分区。
func hostDisk() float64 {
	// 优先用已注入的数据根目录：--root / CAIRN_ROOT 把数据挪到其他卷时，
	// CWD 相对的 "data" 会指向不存在的路径并误读别的卷。
	for _, dir := range []string{dataDirOverride, "data", "."} {
		if dir == "" {
			continue
		}
		if pct, ok := diskUsagePct(dir); ok {
			return pct
		}
	}
	if pct, ok := diskUsagePct("/"); ok {
		return pct
	}
	return 0
}

func diskUsagePct(dir string) (float64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	if st.Blocks == 0 || st.Bfree > st.Blocks {
		return 0, false
	}
	// Bfree 含保留块，与 windows 侧 GetDiskFreeSpaceExW totalFree 口径一致。
	return clampPct(100 * float64(st.Blocks-st.Bfree) / float64(st.Blocks)), true
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

/* ── 整机运行时长与网卡累计流量 ──

与 windows 侧同口径（同为「开机以来」），见 hostmetrics_windows.go 顶部说明。
速率不在这里差分：本方法会被每个大盘请求调用，基线放这里会被多标签页轮询
切碎窗口；速率由 api 层采样器按采集周期差分。 */

// HostCounters 整机运行时长（秒）与网卡累计收发字节（开机以来）。
func (HostMetrics) HostCounters() (uptimeSec, rxBytes, txBytes float64) {
	uptimeSec = hostUptimeSec()
	rxBytes, txBytes = hostNetBytes()
	return
}

// hostUptimeSec 读 /proc/uptime 首字段（秒，含小数）。
func hostUptimeSec() float64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}

// hostNetBytes 汇总 /proc/net/dev 各网卡累计收发字节，排除回环（lo）。
//
// 只累加字节列，不碰包数/错误数；接口名以冒号结尾，故先切掉冒号再取字段。
// 排除 lo 的理由与 windows 侧一致：本机进程间通信不代表对外网络流量。
func hostNetBytes() (rx, tx float64) {
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue // 表头两行无冒号
		}
		name := strings.TrimSpace(line[:i])
		if name == "" || name == "lo" {
			continue
		}
		fields := strings.Fields(line[i+1:])
		// 收：bytes packets errs drop fifo frame compressed multicast（8 列）
		// 发：bytes packets errs drop fifo colls carrier compressed（从第 8 列起）
		if len(fields) < 16 {
			continue
		}
		r, _ := strconv.ParseFloat(fields[0], 64)
		t, _ := strconv.ParseFloat(fields[8], 64)
		rx += r
		tx += t
	}
	return rx, tx
}
