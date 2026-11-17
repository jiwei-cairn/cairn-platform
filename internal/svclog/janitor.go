package svclog

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/logx"
)

// janitorInterval 配额检查周期。日志经 lumberjack 已有单文件/份数上限，
// 这里只做总量兜底，分钟级足够。
const janitorInterval = time.Minute

// stuckWarnInterval「超限但无可删除」告警的最小间隔（防每分钟刷屏）。
const stuckWarnInterval = 10 * time.Minute

var (
	janitorMu   sync.Mutex
	janitorStop chan struct{}

	stuckWarnAt atomic.Int64
)

// StartJanitor 启动服务日志磁盘配额看门人：周期统计 dir 总占用（活动文件
// + 轮转备份），超过配额时从最老的轮转备份开始删除，直至回到配额内或
// 无可删除。各服务的活动文件（svc-{id}.log）受保护、绝不删除。
// quotaFn 每轮读取当前配额字节数（设置热保存即时生效；<=0 视为未启用）。
// 幂等：重复调用不叠加；Shutdown 时停止。
func StartJanitor(dir string, quotaFn func() int64) {
	janitorMu.Lock()
	defer janitorMu.Unlock()
	if janitorStop != nil {
		return
	}
	stop := make(chan struct{})
	janitorStop = stop
	go func() {
		t := time.NewTicker(janitorInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				quota := int64(0)
				if quotaFn != nil {
					quota = quotaFn()
				}
				if quota <= 0 {
					continue
				}
				enforceQuota(dir, quota, activeBaseNames())
			}
		}
	}()
}

// stopJanitor 停止看门人（Shutdown 调用）。
func stopJanitor() {
	janitorMu.Lock()
	defer janitorMu.Unlock()
	if janitorStop != nil {
		close(janitorStop)
		janitorStop = nil
	}
}

// activeBaseNames 当前所有出口的活动文件名集合（受保护的文件）。
func activeBaseNames() map[string]bool {
	out := make(map[string]bool)
	for _, s := range All() {
		out[filepath.Base(s.FilePath())] = true
	}
	return out
}

// enforceQuota 统计 dir 总占用，超限时按修改时间从旧到新删除非活动文件。
// 返回释放的字节数与删除的文件数。
func enforceQuota(dir string, quota int64, active map[string]bool) (freed int64, deleted int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0 // 目录不存在（尚无服务日志）视为未超限
	}
	type item struct {
		name  string
		size  int64
		mtime time.Time
	}
	var total int64
	var deletable []item
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
		if active[e.Name()] {
			continue
		}
		deletable = append(deletable, item{name: e.Name(), size: info.Size(), mtime: info.ModTime()})
	}
	if total <= quota {
		return 0, 0
	}
	sort.Slice(deletable, func(i, j int) bool { return deletable[i].mtime.Before(deletable[j].mtime) })
	for _, it := range deletable {
		if total <= quota {
			break
		}
		if err := os.Remove(filepath.Join(dir, it.name)); err == nil {
			total -= it.size
			freed += it.size
			deleted++
		}
	}
	if deleted > 0 {
		logx.Module("svclog").Warn("服务日志超配额，已清理最老的轮转备份", "dir", dir, "deleted", deleted, "freedBytes", freed)
		return freed, deleted
	}
	// 超限但全是活动文件：受保护不可删，限频告警。
	if now := time.Now().Unix(); now-stuckWarnAt.Load() > int64(stuckWarnInterval/time.Second) {
		stuckWarnAt.Store(now)
		logx.Module("svclog").Warn("服务日志超配额且无可删除的轮转备份（活动文件受保护）", "dir", dir, "overBytes", total-quota)
	}
	return 0, 0
}
