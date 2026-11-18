package api

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/buildinfo"
	"github.com/jiwei-cairn/cairn-platform/internal/logx"
)

// dashboardOverview 概览摘要卡（总数/正常/告警/异常 + 停用数 + 主机资源）。
type dashboardOverview struct {
	Total       int     `json:"total"`
	Ok          int     `json:"ok"`
	Warn        int     `json:"warn"`
	Err         int     `json:"err"`
	Disabled    int     `json:"disabled"`
	HostCpuPct  float64 `json:"hostCpuPct"`
	HostMemPct  float64 `json:"hostMemPct"`
	HostDiskPct float64 `json:"hostDiskPct"`

	/* 左列两卡：整机运行时长 / 网络流量（同为「开机以来」口径）。
	   速率取采样器最近一次差分结果，累计流量直接读网卡计数器——
	   累计值不随平台重启跳变，故无需自行累加。 */
	HostUptimeSec float64 `json:"hostUptimeSec"`
	NetRxKBps     float64 `json:"netRxKBps"`
	NetTxKBps     float64 `json:"netTxKBps"`
	NetRxBytes    float64 `json:"netRxBytes"`
	NetTxBytes    float64 `json:"netTxBytes"`
}

// platformInfo 平台自身只读条（Q3：展示但不参与监管）。
type platformInfo struct {
	Name      string  `json:"name"`
	Version   string  `json:"version"`
	State     string  `json:"state"`
	UptimeSec float64 `json:"uptimeSec"`
	Daemon    string  `json:"daemon"`
}

// abnormalRow 异常服务置顶行。
type abnormalRow struct {
	ServiceID           int    `json:"serviceId"`
	Name                string `json:"name"`
	State               string `json:"state"`
	Reason              string `json:"reason"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
}

// dashboardView GET /api/dashboard 响应（冻结契约）。
type dashboardView struct {
	Overview dashboardOverview `json:"overview"`
	Platform platformInfo      `json:"platform"`
	Abnormal []abnormalRow     `json:"abnormal"`
	Services []serviceRow      `json:"services"`
}

// stateKind 与前端 constants.js SERVICE_STATE.kind 对齐：
// ok=正常 warn=告警 err=异常 muted=停用（muted 计入 err，与 mock 一致）。
func stateKind(state string) string {
	switch state {
	case "running", "starting", "waiting", "stopping", "upgrading":
		return "ok"
	case "degraded", "backoff":
		return "warn"
	default: // failed / stopped / unknown / disabled
		return "err"
	}
}

// abnormalReason 异常原因（与 mockAbnormalReason 同文案）。
func abnormalReason(rt RunState) string {
	switch rt.State {
	case "failed":
		if rt.LastError != "" {
			return rt.LastError
		}
		return "进程异常退出，已熔断等待人工介入"
	case "backoff":
		n := rt.ConsecutiveFailures
		if n < 1 {
			n = 1
		}
		return fmt.Sprintf("健康检查连续失败（第 %d 次），退避等待后自动重试", n)
	case "degraded":
		return "L2 探针连续失败 ≥ 阈值，进程存活但服务降级"
	case "stopped":
		return "已启用但处于停止状态"
	}
	return ""
}

// GetDashboard 大盘聚合：概览摘要 + 平台只读条 + 异常置顶 + 全服务运行态。
// Supervisor facade 未装配时全部按 stopped 兜底（A 类定义数据仍真实）。
// 主机指标一律读采样器的内存缓存（采集节奏见 hostSampler 注释），不在请求
// 路径上直接读 /proc——采集周期设置因此对大盘数据新鲜度有明确语义。
func (s *Server) GetDashboard(w http.ResponseWriter, r *http.Request) {
	rows := s.serviceRows()
	view := dashboardView{
		Overview: dashboardOverview{Total: len(rows)},
		Platform: platformInfo{
			Name:      buildinfo.PlatformName,
			Version:   buildinfo.Version,
			State:     "running",
			UptimeSec: uptimeSince(s.StartedAt),
			Daemon:    daemonLabel(),
		},
		Abnormal: []abnormalRow{},
		Services: rows,
	}
	for _, row := range rows {
		if !row.Enabled {
			view.Overview.Disabled++
		}
		switch stateKind(row.Runtime.State) {
		case "ok":
			view.Overview.Ok++
		case "warn":
			view.Overview.Warn++
		default:
			view.Overview.Err++
		}
		if reason := abnormalReason(row.Runtime); reason != "" {
			view.Abnormal = append(view.Abnormal, abnormalRow{
				ServiceID: row.ID, Name: row.Name, State: row.Runtime.State,
				Reason: reason, ConsecutiveFailures: row.Runtime.ConsecutiveFailures,
			})
		}
	}
	if s.Host != nil {
		if snap, ok := hostSnapshotNow(); ok {
			view.Overview.HostCpuPct = snap.CpuPct
			view.Overview.HostMemPct = snap.MemPct
			view.Overview.HostDiskPct = snap.DiskPct
			view.Overview.HostUptimeSec = snap.UptimeSec
			view.Overview.NetRxKBps = snap.RxKBps
			view.Overview.NetTxKBps = snap.TxKBps
			view.Overview.NetRxBytes = snap.RxBytes
			view.Overview.NetTxBytes = snap.TxBytes
		}
	}
	ok(w, view)
}

// uptimeSince 平台自身运行时长。
func uptimeSince(t time.Time) float64 { return time.Since(t).Seconds() }

/* ══════════════ 主机指标采样（仅内存实时缓存，不落库） ══════════════

决策（2026-11）：系统指标只缓存实时值——采样器按 Settings.Collect.IntervalSec
（默认 5s）定时刷新一份内存快照供大盘轮询读取；不写数据库、不做降采样与
保留窗口（原 MetricPoint 落库、过期清理与 sparkline 历史查询已随本决策移除）。

速率差分必须在采样器内按固定节拍推进：若放在请求路径上按「上次请求时间」
差分，多个标签页/轮询会把窗口切碎成零点几秒，算出的速率剧烈抖动。 */

const (
	hostSampleFloor = 5 * time.Second // 采样周期下限（防误配 1s 高频空转）
)

// hostSnapshot 主机实时指标快照（采样器每 tick 覆写，大盘只读）。
type hostSnapshot struct {
	CpuPct, MemPct, DiskPct float64
	RxKBps, TxKBps          float64
	UptimeSec               float64
	RxBytes, TxBytes        float64
	At                      time.Time
}

// hostSampler 主机指标采样器（进程内单例，随 API 启动）。
type hostSampler struct {
	srv  *Server
	stop chan struct{}

	mu       sync.Mutex
	snap     hostSnapshot
	hasSnap  bool
	lastDisk float64 // 最近一个非零磁盘使用率（探针瞬时 0 时兜底）

	/* 速率差分状态（rateMu 保护），只有采样器写。 */
	rateMu     sync.Mutex
	ratePrimed bool
	lastRx     float64
	lastTx     float64
	lastRateAt time.Time
	rxKBps     float64
	txKBps     float64
}

var (
	hostSamplerMu sync.Mutex
	hostSamplerIn *hostSampler
)

// StartHostSampler 启动主机指标采样（幂等：重复调用不叠加 goroutine）。
// intervalSec 取 Settings.Collect.IntervalSec；Host 未装配时静默不启动。
func (s *Server) StartHostSampler(intervalSec int) {
	if s.Host == nil {
		return
	}
	d := time.Duration(intervalSec) * time.Second
	if d < hostSampleFloor {
		d = hostSampleFloor
	}
	hostSamplerMu.Lock()
	defer hostSamplerMu.Unlock()
	if hostSamplerIn != nil {
		return // 已在运行
	}
	h := &hostSampler{srv: s, stop: make(chan struct{})}
	hostSamplerIn = h
	// 先登记再启动：重复调用由上面的 nil 判断挡住。
	go func() {
		// 立即采一点：否则刚启动时大盘无主机数据，速率差分基线也无从建立。
		h.sampleOnce()
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-t.C:
				h.sampleOnce()
			}
		}
	}()
	logx.Module("api").Info("主机指标采样已启动", "intervalSec", int(d.Seconds()))
}

// StopHostSampler 停止主机指标采样（进程退出时调用；幂等）。
// 包级函数而非 Server 方法：停止逻辑只碰包级单例状态，无需实例；
// main 关闭链以 api.StopHostSampler() 直接调用。
func StopHostSampler() {
	hostSamplerMu.Lock()
	defer hostSamplerMu.Unlock()
	if hostSamplerIn == nil {
		return
	}
	close(hostSamplerIn.stop)
	hostSamplerIn = nil
}

// sampleOnce 采一点并覆写内存快照（尽力而为：失败静默保留上次快照）。
func (h *hostSampler) sampleOnce() {
	cpu, mem, disk := h.srv.Host.HostMetrics()
	uptime, rxBytes, txBytes := h.srv.Host.HostCounters()
	rxKBps, txKBps := h.updateRates(rxBytes, txBytes, time.Now())
	h.mu.Lock()
	// 磁盘使用率变化极慢，且容器/目录缺失时该探针可能瞬时返回 0——
	// 用最近一个非零点兜底，避免数值在 0 与真实值之间跳变。
	if disk > 0 {
		h.lastDisk = disk
	} else {
		disk = h.lastDisk
	}
	h.snap = hostSnapshot{
		CpuPct: cpu, MemPct: mem, DiskPct: disk,
		RxKBps: rxKBps, TxKBps: txKBps,
		UptimeSec: uptime, RxBytes: rxBytes, TxBytes: txBytes,
		At: time.Now(),
	}
	h.hasSnap = true
	h.mu.Unlock()
}

// updateRates 按采样节拍差分网卡累计字节 → KB/s。
//
// 首次采样只建立基线（无速率可算），返回 0；采样间隔非正（时钟回拨/系统休眠
// 恢复）时保留上次结果，避免除零或负速率。计数器本身也可能因网卡重置而回退，
// 负增量按 0 处理——速率出现负值在界面上毫无意义。
func (h *hostSampler) updateRates(rx, tx float64, now time.Time) (rxKBps, txKBps float64) {
	h.rateMu.Lock()
	defer h.rateMu.Unlock()
	if !h.ratePrimed || h.lastRateAt.IsZero() {
		h.lastRx, h.lastTx, h.lastRateAt, h.ratePrimed = rx, tx, now, true
		h.rxKBps, h.txKBps = 0, 0
		return 0, 0
	}
	dt := now.Sub(h.lastRateAt).Seconds()
	if dt <= 0 {
		return h.rxKBps, h.txKBps
	}
	drx, dtx := rx-h.lastRx, tx-h.lastTx
	if drx < 0 {
		drx = 0
	}
	if dtx < 0 {
		dtx = 0
	}
	rxKBps, txKBps = drx/1024/dt, dtx/1024/dt
	h.lastRx, h.lastTx, h.lastRateAt = rx, tx, now
	h.rxKBps, h.txKBps = rxKBps, txKBps
	return rxKBps, txKBps
}

// hostSnapshotNow 当前内存快照；采样器未启动（Host 未装配）或尚未采到
// 首点时返回 false，大盘主机字段保持零值。
func hostSnapshotNow() (hostSnapshot, bool) {
	hostSamplerMu.Lock()
	h := hostSamplerIn
	hostSamplerMu.Unlock()
	if h == nil {
		return hostSnapshot{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snap, h.hasSnap
}
