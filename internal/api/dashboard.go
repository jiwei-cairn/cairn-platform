package api

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/buildinfo"
	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/store"
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
		view.Overview.HostCpuPct, view.Overview.HostMemPct, view.Overview.HostDiskPct = s.Host.HostMetrics()
		/* 时长/累计流量为瞬时纯读；速率取采样器最近一次差分结果（见 hostSampler.updateRates） */
		view.Overview.HostUptimeSec, view.Overview.NetRxBytes, view.Overview.NetTxBytes = s.Host.HostCounters()
		view.Overview.NetRxKBps, view.Overview.NetTxKBps = hostLastRates()
	}
	ok(w, view)
}

// uptimeSince 平台自身运行时长。
func uptimeSince(t time.Time) float64 { return time.Since(t).Seconds() }

/* ══════════════ 主机指标采样与历史（sparkline 数据源） ══════════════

MetricPoint 表以 serviceId=0 承载「主机自身」时序点（模型注释既定约定）。
采样在 API 进程内定时执行，不依赖被管服务是否运行；落库节奏即
Settings.Collect.IntervalSec（默认 10s），与大盘轮询同源，避免两套节奏。

存储策略：只保留单一原始粒度（不做降采样表），保留窗口 2h——大盘只展示
「近 1 小时趋势」，2h 留一倍余量即可。10s × 2h ≈ 720 行，配合 10 分钟一次
的过期清理，写入量对嵌入式 SD 卡友好（这是刻意的上限：不为未展示的
更长窗口持续写盘）。
显示点数由查询侧按桶聚合压到 ≤120 点（见 bucketize），因此采样周期可调
而不影响曲线观感。 */

const (
	hostMetricSvcID = 0               // MetricPoint.serviceId=0 = 主机自身
	hostSampleFloor = 5 * time.Second // 采样周期下限（防误配 1s 刷库）
	hostRetention   = 2 * time.Hour   // 保留窗口（覆盖 1h 视图 + 一倍余量）
	hostPruneEvery  = 10 * time.Minute
	hostPruneBatch  = 2000 // 单次清理删除上限（避免长事务锁库）
	hostMaxPoints   = 120  // 单条 sparkline 的目标点数（查询侧桶聚合上限）
)

// hostSampler 主机指标采样器（进程内单例，随 API 启动）。
type hostSampler struct {
	srv       *Server
	mu        sync.Mutex
	stop      chan struct{}
	lastPrune time.Time

	/* 速率差分状态（rateMu 保护）。

	   差分基线必须按固定采样节拍推进：若放在请求路径上按「上次请求时间」差分，
	   多个标签页/轮询就会把窗口切碎成零点几秒，算出的速率剧烈抖动。
	   故只有采样器写，大盘只读最近一次结果。 */
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
		// 立即采一点：否则刚启动时 sparkline 无数据，CPU 差分基线也无从建立。
		// 放在 goroutine 内，避免在持锁状态下做 DB 写入而阻塞 StartHostSampler。
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
func StopHostSampler() {
	hostSamplerMu.Lock()
	defer hostSamplerMu.Unlock()
	if hostSamplerIn == nil {
		return
	}
	close(hostSamplerIn.stop)
	hostSamplerIn = nil
}

// sampleOnce 采一点并落库（尽力而为：失败仅告警，不影响服务监管）。
func (h *hostSampler) sampleOnce() {
	cpu, mem, disk := h.srv.Host.HostMetrics()
	// 磁盘使用率变化极慢，且容器/目录缺失时该探针可能瞬时返回 0——
	// 用最近一个非零点兜底，避免曲线在 0 与真实值之间跳变。
	if disk <= 0 {
		disk = h.lastNonZeroDisk()
	}
	// 整机运行时长/累计流量走独立方法：纯读，不干扰上面的百分比口径。
	_, rxBytes, txBytes := h.srv.Host.HostCounters()
	rxKBps, txKBps := h.updateRates(rxBytes, txBytes, time.Now())
	pt := store.MetricPoint{
		ServiceID: hostMetricSvcID,
		Ts:        time.Now().UnixMilli(),
		CpuPct:    cpu,
		MemPct:    mem,
		DiskPct:   disk,
		NetRxKBps: rxKBps,
		NetTxKBps: txKBps,
	}
	if err := h.srv.DB.Create(&pt).Error; err != nil {
		logx.Module("api").Warn("主机指标落库失败", "err", err)
		return
	}
	h.pruneIfDue()
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

// lastRates 最近一次差分得到的速率（只读，无副作用）。
func (h *hostSampler) lastRates() (rxKBps, txKBps float64) {
	h.rateMu.Lock()
	defer h.rateMu.Unlock()
	return h.rxKBps, h.txKBps
}

// hostLastRates 当前采样器的最近速率；采样器未启动（Host 未装配）时返回 0。
func hostLastRates() (rxKBps, txKBps float64) {
	hostSamplerMu.Lock()
	h := hostSamplerIn
	hostSamplerMu.Unlock()
	if h == nil {
		return 0, 0
	}
	return h.lastRates()
}

// lastNonZeroDisk 最近一个非零磁盘使用率（无历史则返回 0）。
func (h *hostSampler) lastNonZeroDisk() float64 {
	var pt store.MetricPoint
	if err := h.srv.DB.Where("service_id = ? AND disk_pct > 0", hostMetricSvcID).
		Order("ts desc").First(&pt).Error; err != nil {
		return 0
	}
	return pt.DiskPct
}

// pruneIfDue 节流清理：距上次清理不足 hostPruneEvery 时直接返回。
// 注意用独立的 lastPrune 而非采样时间戳——后者每次采样都刷新，会让清理永不触发。
func (h *hostSampler) pruneIfDue() {
	h.mu.Lock()
	if !h.lastPrune.IsZero() && time.Since(h.lastPrune) < hostPruneEvery {
		h.mu.Unlock()
		return
	}
	h.lastPrune = time.Now()
	h.mu.Unlock()

	cut := time.Now().Add(-hostRetention).UnixMilli()
	// 分批删除：GORM 的 Limit 子句在 DELETE 中被 modernc sqlite 静默丢弃
	//（该驱动未编译 SQLITE_ENABLE_UPDATE_DELETE_LIMIT，实测 Limit(3) 会删光全部
	// 匹配行且返回 nil），因此必须显式用子查询限定 id。积压量大时逐批删，
	// 避免单事务长时间持锁阻塞采样写入。
	for {
		sub := h.srv.DB.Model(&store.MetricPoint{}).
			Select("id").
			Where("service_id = ? AND ts < ?", hostMetricSvcID, cut).
			Limit(hostPruneBatch)
		res := h.srv.DB.Where("id IN (?)", sub).Delete(&store.MetricPoint{})
		if res.Error != nil {
			logx.Module("api").Warn("主机指标清理失败", "err", res.Error)
			return
		}
		if res.RowsAffected < int64(hostPruneBatch) {
			return // 已清完（不足一批说明没有更多积压）
		}
	}
}

/* ── 查询端点 ── */

// hostMetricPointView 是对外的时序点（字段名对齐前端 sparkline 消费）。
type hostMetricPointView struct {
	Ts   int64   `json:"ts"`
	Cpu  float64 `json:"cpu"`
	Mem  float64 `json:"mem"`
	Disk float64 `json:"disk"`
	// 网络速率（KB/s）：左列「网络流量」卡的双线 sparkline 数据源
	RxKBps float64 `json:"rxKBps"`
	TxKBps float64 `json:"txKBps"`
}

// hostMetricsView GET /api/dashboard/host-metrics 响应。
type hostMetricsView struct {
	Range  string                `json:"range"`  // 当前固定为 1h
	StepMs int64                 `json:"stepMs"` // 聚合后相邻点的时间间隔（供前端标注）
	Points []hostMetricPointView `json:"points"`
}

// GetHostMetrics GET /api/dashboard/host-metrics
// 返回主机 CPU/内存/磁盘使用率的近 1 小时时序，供大盘 sparkline 绘制。
//
// 仅提供 1 小时窗口：保留窗口就是 2h（见 hostRetention），更长窗口只会
// 返回被截断的数据却在坐标轴上冒充完整区间——宁可少一个参数，不要给
// 调用方一个会误读的结果。无历史时返回空数组而非报错（前端显示占位）。
func (s *Server) GetHostMetrics(w http.ResponseWriter, r *http.Request) {
	if rng := strings.TrimSpace(r.URL.Query().Get("range")); rng != "" && rng != "1h" {
		fail(w, http.StatusBadRequest, "仅支持 range=1h")
		return
	}
	const window = time.Hour
	since := time.Now().Add(-window).UnixMilli()

	var pts []store.MetricPoint
	if err := s.DB.Where("service_id = ? AND ts >= ?", hostMetricSvcID, since).
		Order("ts asc").Find(&pts).Error; err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, bucketize(pts, "1h", window))
}

// bucketize 把原始点按时间桶聚合成 ≤ hostMaxPoints 个点（桶内取均值）。
//
// 为什么在查询侧聚合：采样周期（Settings 可调，默认 10s）与前端可绘制的
// 点数无关。1 小时窗口在 10s 采样下约 360 点，直接输出偏密且随采样周期
// 波动；聚合后步长固定（1h/120 ≈ 30s），前端无需关心采样密度。
func bucketize(pts []store.MetricPoint, rng string, window time.Duration) hostMetricsView {
	stepMs := (window / hostMaxPoints).Milliseconds()
	if stepMs <= 0 {
		stepMs = 1
	}
	view := hostMetricsView{Range: rng, StepMs: stepMs, Points: make([]hostMetricPointView, 0, hostMaxPoints+1)}
	if len(pts) == 0 {
		return view
	}
	type acc struct {
		bucket         int64
		ts             int64
		n              int
		cpu, mem, disk float64
		rxKBps, txKBps float64
	}
	var cur *acc
	flush := func() {
		if cur == nil || cur.n == 0 {
			return
		}
		view.Points = append(view.Points, hostMetricPointView{
			Ts:     cur.ts,
			Cpu:    cur.cpu / float64(cur.n),
			Mem:    cur.mem / float64(cur.n),
			Disk:   cur.disk / float64(cur.n),
			RxKBps: cur.rxKBps / float64(cur.n),
			TxKBps: cur.txKBps / float64(cur.n),
		})
	}
	for _, p := range pts {
		bucket := p.Ts / stepMs
		if cur == nil || cur.bucket != bucket {
			flush()
			cur = &acc{bucket: bucket, ts: p.Ts}
		}
		cur.n++
		cur.cpu += p.CpuPct
		cur.mem += p.MemPct
		cur.disk += p.DiskPct
		cur.rxKBps += p.NetRxKBps
		cur.txKBps += p.NetTxKBps
	}
	flush()
	return view
}
