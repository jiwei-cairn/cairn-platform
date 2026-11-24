// statsring.go — 单服务指标环形缓冲（P1-1：详情页 sparkline 数据源）。
//
// 设计（2026-11 指标不落库决策的既定延续）：
//   - 内存环形缓冲，保留最近 N=30 个采样点（2s 对账周期 ≈ 1 分钟趋势）；
//   - 写入方：worker.reconcile 的 procmetrics 采集段（同一拍差分，零新增开销）；
//   - 读取方：api.StatsSource.Series（经 adapt 适配器注入）→ /services/{id}/stats；
//   - 进程停止/重启不清空（保留停止前的一段曲线，排障看趋势更有用）；
//     只有采集恢复（新进程 running）才继续追加。
package supervisor

import (
	"sync"
)

// ringCapacity 采样点容量（30 点 × 2s ≈ 1 分钟窗口）。
const ringCapacity = 30

// samplePoint 一个采样点。
type samplePoint struct {
	CpuPct float64
	MemMB  float64
}

// statsRing 单服务的环形缓冲（并发安全：worker 写、API 读）。
type statsRing struct {
	mu    sync.Mutex
	buf   [ringCapacity]samplePoint
	head  int // 下一个写入位
	count int // 已写点数（≤ ringCapacity）
}

// push 追加一个采样点（覆盖最旧的）。
func (r *statsRing) push(p samplePoint) {
	r.mu.Lock()
	r.buf[r.head] = p
	r.head = (r.head + 1) % ringCapacity
	if r.count < ringCapacity {
		r.count++
	}
	r.mu.Unlock()
}

// cpuSeries 按时间序（旧→新）返回 CPU 百分比序列（前端 sparkline 直接消费）。
func (r *statsRing) cpuSeries() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, 0, r.count)
	start := (r.head - r.count + ringCapacity) % ringCapacity
	for i := 0; i < r.count; i++ {
		idx := (start + i) % ringCapacity
		v := int(r.buf[idx].CpuPct)
		if v < 0 {
			v = 0
		}
		if v > 100 {
			v = 100
		}
		out = append(out, v)
	}
	return out
}

// memSeries 按时间序返回内存 MB 序列。
func (r *statsRing) memSeries() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, 0, r.count)
	start := (r.head - r.count + ringCapacity) % ringCapacity
	for i := 0; i < r.count; i++ {
		idx := (start + i) % ringCapacity
		v := int(r.buf[idx].MemMB)
		if v < 0 {
			v = 0
		}
		out = append(out, v)
	}
	return out
}

// Engine 级注册表：serviceID → ring（worker 生命周期外持有，随服务删除移除）。
type statsRegistry struct {
	mu    sync.Mutex
	rings map[int]*statsRing
}

func newStatsRegistry() *statsRegistry {
	return &statsRegistry{rings: make(map[int]*statsRing)}
}

// ring 取（或惰性建）服务的 ring。
func (g *statsRegistry) ring(serviceID int) *statsRing {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r, ok := g.rings[serviceID]; ok {
		return r
	}
	r := &statsRing{}
	g.rings[serviceID] = r
	return r
}

// drop 服务删除时移除（防泄漏）。
func (g *statsRegistry) drop(serviceID int) {
	g.mu.Lock()
	delete(g.rings, serviceID)
	g.mu.Unlock()
}
