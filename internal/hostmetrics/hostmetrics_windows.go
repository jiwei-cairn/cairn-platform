//go:build windows

// hostmetrics_windows.go — 大盘主机资源概览（Windows：kernel32 真实读取，零第三方依赖）。
//
//	CPU  GetSystemTimes 两次采样差分（kernel 时间已含 idle，故 total = kernel + user）
//	内存 GlobalMemoryStatusEx（dwMemoryLoad 即物理内存使用率百分比）
//	磁盘 GetDiskFreeSpaceExW（data 目录所在卷，缺失时回退当前工作目录所在卷）
//
// 与 hostmetrics_linux.go 同接口同口径（cpu/mem/disk 均为 0-100 百分比）。
package hostmetrics

import (
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

// HostMetrics 实现 api.HostSource（windows：kernel32 三接口）。
type HostMetrics struct{}

var (
	hmMu     sync.Mutex
	hmPrimed bool
	hmIdle   float64
	hmTotal  float64

	hmKernel32 = syscall.NewLazyDLL("kernel32.dll")

	procGetSystemTimes       = hmKernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = hmKernel32.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceExW  = hmKernel32.NewProc("GetDiskFreeSpaceExW")
	procGetTickCount64       = hmKernel32.NewProc("GetTickCount64")

	// iphlpapi：整机网卡累计收发字节（MIB_IF_ROW2 的 64 位计数器）
	hmIphlpapi       = syscall.NewLazyDLL("iphlpapi.dll")
	procGetIfTable2  = hmIphlpapi.NewProc("GetIfTable2")
	procFreeMibTable = hmIphlpapi.NewProc("FreeMibTable")
)

// dataDirOverride 磁盘探针的卷锚点（由 main 装配时注入 config.DataDir()）。
// 为空表示未注入，回退到 CWD 相对的 "data"（开发形态）。
var dataDirOverride string

// SetDataDir 注入数据目录，使磁盘使用率读取数据实际所在的卷。
// 须在首次读取指标前调用（main 装配阶段）。
func SetDataDir(dir string) { dataDirOverride = dir }

// init 预置 CPU 基线采样：使大盘首个请求即返回真实差分值（而非首次采样无差分返回 0）。
// 进程启动到首次 HTTP 请求之间的间隔即为差分窗口。
func init() {
	if s, ok := readCPUSample(); ok {
		hmIdle, hmTotal, hmPrimed = s.idle, s.total, true
	}
}

// HostMetrics cpu + mem + disk（0-100）。
func (HostMetrics) HostMetrics() (cpu, mem, disk float64) {
	return hostCPU(), hostMem(), hostDisk()
}

/* ── CPU：GetSystemTimes ── */

// winFiletime 对应 Win32 FILETIME（100ns 计数，高低双 DWORD）。
type winFiletime struct {
	Low  uint32
	High uint32
}

func (f winFiletime) ticks() float64 {
	return float64(uint64(f.High)<<32 | uint64(f.Low))
}

// cpuSample 单次系统时间采样：idle 与 total（kernel+user）累计值。
type cpuSample struct {
	idle  float64
	total float64
}

func readCPUSample() (cpuSample, bool) {
	var idle, kernel, user winFiletime
	r, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r == 0 {
		return cpuSample{}, false
	}
	// kernel 时间已包含 idle，故 total = kernel + user。
	return cpuSample{idle: idle.ticks(), total: kernel.ticks() + user.ticks()}, true
}

// cpuUsage 两次采样差分 → 忙碌占比。
func cpuUsage(prev, cur cpuSample) float64 {
	dTotal := cur.total - prev.total
	if dTotal <= 0 {
		return 0
	}
	return clampPct(100 * (1 - (cur.idle-prev.idle)/dTotal))
}

func hostCPU() float64 {
	cur, ok := readCPUSample()
	if !ok {
		return 0
	}
	hmMu.Lock()
	defer hmMu.Unlock()
	if !hmPrimed {
		// 基线缺失（init 采样失败）：本次仅建立基线。
		hmIdle, hmTotal, hmPrimed = cur.idle, cur.total, true
		return 0
	}
	pct := cpuUsage(cpuSample{idle: hmIdle, total: hmTotal}, cur)
	hmIdle, hmTotal = cur.idle, cur.total
	return pct
}

/* ── 内存：GlobalMemoryStatusEx ── */

// memoryStatusEx 对应 Win32 MEMORYSTATUSEX（Length 必须预置为结构体字节数）。
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func hostMem() float64 {
	var ms memoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	if r == 0 {
		return 0
	}
	return clampPct(float64(ms.MemoryLoad)) // dwMemoryLoad 已是 0-100
}

/* ── 磁盘：GetDiskFreeSpaceExW ── */

func hostDisk() float64 {
	// 优先用已注入的数据根目录：生产 exec-dir / --root 锚定形态下 CWD 可能是
	// System32 等无关目录，按 "data" 相对 CWD 取卷会读错盘。
	// 未注入（空串）时回退旧的 CWD 相对行为。
	for _, dir := range []string{dataDirOverride, "data"} {
		if dir == "" {
			continue
		}
		if abs, err := filepath.Abs(dir); err == nil {
			if pct, ok := diskUsagePct(abs); ok {
				return pct
			}
		}
	}
	// 都不可用：回退当前工作目录所在卷。
	if pct, ok := diskUsagePct("."); ok {
		return pct
	}
	return 0
}

func diskUsagePct(dir string) (float64, bool) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, false
	}
	var freeToCaller, total, totalFree uint64
	r, _, _ := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeToCaller)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 || total == 0 {
		return 0, false
	}
	if totalFree > total {
		totalFree = total
	}
	// totalFree 含保留块，与 linux 侧 statfs Bfree 口径一致。
	return clampPct(100 * float64(total-totalFree) / float64(total)), true
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

大盘左列两卡（系统运行时长 / 网络流量）的数据源。两者同为「开机以来」口径：
运行时长用 GetTickCount64（64 位毫秒，无 49.7 天回绕），流量用网卡自身的
累计计数器，因此重启平台不会让任一数字跳变。

速率不在这里差分：差分基线必须按固定采样节拍推进，而本文件的方法会被每个
大盘 HTTP 请求调用——把基线放在这里会让多标签页轮询把速率窗口切碎成零点
几秒。速率由 api 层采样器按采集周期差分（见 api.hostSampler.updateRates）。 */

// HostCounters 整机运行时长（秒）与网卡累计收发字节（开机以来）。
// 纯读：不推进任何差分基线，可在任意请求中反复调用。
func (HostMetrics) HostCounters() (uptimeSec, rxBytes, txBytes float64) {
	uptimeSec = hostUptimeSec()
	rxBytes, txBytes = hostNetBytes()
	return
}

// hostUptimeSec 开机以来的秒数（GetTickCount64 毫秒 / 1000）。
func hostUptimeSec() float64 {
	ms, _, _ := procGetTickCount64.Call()
	return float64(ms) / 1000
}

// mibIfRow2 对应 Win32 MIB_IF_ROW2（netioapi.h）。
//
// 字段顺序与类型必须逐字对齐：本结构体跨过 1.2KB 才到计数器区，任何一处偏差
// 都会读到无关内存——读出来是一堆看似合理的垃圾数字，不报错，最难查。
// 实测校验：sizeof = 1352，Alias 偏移 28，Type 偏移 1128，InOctets 偏移 1208。
type mibIfRow2 struct {
	InterfaceLuid               uint64   // NET_LUID（联合体，取 64 位整数视图）
	InterfaceIndex              uint32   // NET_IFINDEX
	InterfaceGuid               [16]byte // GUID（4+2+2+8）
	Alias                       [257]uint16
	Description                 [257]uint16
	PhysicalAddressLength       uint32
	PhysicalAddress             [32]byte
	PermanentPhysicalAddress    [32]byte
	Mtu                         uint32
	Type                        uint32 // IFTYPE（C 枚举，4 字节）
	TunnelType                  uint32
	MediaType                   uint32
	PhysicalMediumType          uint32
	AccessType                  uint32
	DirectionType               uint32
	InterfaceAndOperStatusFlags [1]byte // 8 个 BOOLEAN 位域，合计 1 字节
	OperStatus                  uint32
	AdminStatus                 uint32
	MediaConnectState           uint32
	NetworkGuid                 [16]byte
	ConnectionType              uint32
	TransmitLinkSpeed           uint64
	ReceiveLinkSpeed            uint64
	// 计数器区（各 8 字节）。顺序经实测反查确认：In* 九项连续排列，
	// OutOctets 紧随其后——并非「InOctets / OutOctets 相邻」。
	// 证据：InUcastOctets + InMulticastOctets + InBroadcastOctets 恰等于 InOctets。
	// 若把 OutOctets 排在 InOctets 之后，读到的是 InUcastPkts（包数），
	// 发送量会显示成几 MB 而非真实的 GB 级——同样不报错，只静默错数。
	InOctets           uint64 // ← 累计接收字节（偏移 1208）
	InUcastPkts        uint64
	InNUcastPkts       uint64
	InDiscards         uint64
	InErrors           uint64
	InUnknownProtos    uint64
	InUcastOctets      uint64
	InMulticastOctets  uint64
	InBroadcastOctets  uint64
	OutOctets          uint64 // ← 累计发送字节（偏移 1280）
	OutUcastPkts       uint64
	OutNUcastPkts      uint64
	OutDiscards        uint64
	OutErrors          uint64
	OutUcastOctets     uint64
	OutMulticastOctets uint64
	OutBroadcastOctets uint64
	OutQLen            uint64
}

// mibIfTable2 对应 Win32 MIB_IF_TABLE2：ULONG 计数 + 行数组。
// 行数组含 ULONG64，起点需 8 字节对齐，故 NumEntries 后有 4 字节填充——
// 这里显式声明 `_ uint32` 占位，读取时由编译器算出正确偏移。
type mibIfTable2 struct {
	NumEntries uint32
	_          uint32
	Table      [1]mibIfRow2
}

// ifTypeSoftwareLoopback IF_TYPE_SOFTWARE_LOOPBACK。
const ifTypeSoftwareLoopback = 24

// ifFlagFilterInterface InterfaceAndOperStatusFlags 的 FilterInterface 位（0x02）。
//
// 为什么必须排除：NDIS 轻量筛选器（WFP / QoS / Npcap）与物理网卡共用同一套
// 计数器，会各返回一份相同数字。实测本机 38 行中 WLAN 的 2.57GB 被 7 行重复
// 上报，合计虚高到 21.7GB（约 8 倍），而真值只有 2.76GB。物理网卡该位为 0、
// 筛选器行该位为 1，据此去重。
const ifFlagFilterInterface = 0x02

// hostNetBytes 汇总各网卡的累计收发字节。
//
// 排除两类行，理由都是「避免重复计数或虚高」：
//   - 回环：本机进程间通信不代表对外网络流量；
//   - NDIS 筛选器：与物理网卡共用计数器，计入即成倍虚高（见 ifFlagFilterInterface）。
//
// 为什么用 GetIfTable2 而非 GetIfTable：后者 MIB_IFROW.dwInOctets 是 32 位，
// 4GB 即回绕——在动辄数十 GB 的累计流量上会直接算错，必须用 64 位版本。
func hostNetBytes() (rx, tx float64) {
	var table *mibIfTable2
	if r, _, _ := procGetIfTable2.Call(uintptr(unsafe.Pointer(&table))); r != 0 || table == nil {
		return 0, 0
	}
	defer func() { _, _, _ = procFreeMibTable.Call(uintptr(unsafe.Pointer(table))) }()

	// 用 unsafe.Slice 覆盖尾部数组：跨步交给编译器，无需 uintptr 运算。
	for _, row := range unsafe.Slice(&table.Table[0], table.NumEntries) {
		if row.Type == ifTypeSoftwareLoopback {
			continue
		}
		if row.InterfaceAndOperStatusFlags[0]&ifFlagFilterInterface != 0 {
			continue
		}
		rx += float64(row.InOctets)
		tx += float64(row.OutOctets)
	}
	return rx, tx
}
