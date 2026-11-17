// logs.go — 平台日志中心：平台自身日志（logx 环形缓冲）+ 被管服务日志
// （svclog：fork 接管的 stdout/stderr，平台侧落盘 logs/services/）。
// 快照 + SSE 实时流 + zip 导出。
//
// service 查询参数三态：空 = 平台 + 全部服务合并流（按时间归并）；
// "platform" = 仅平台自身；数字 = 指定服务。
//
// M3 落地决策：被管服务日志只采集标准输出/错误（fork 管道），不 tail
// 服务自写的日志文件；logPolicy.mode=file 仅表示服务另自写文件，其
// 标准输出同样进日志页。
package api

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/store"
	"github.com/jiwei-cairn/cairn-platform/internal/svclog"
)

/* ══════════════ 日志源抽象：平台与被管服务同构 ══════════════ */

// logFeed 统一日志源视图：快照 + 订阅（元素均已折算为 LogLine）。
type logFeed struct {
	recent    func() []LogLine
	subscribe func() (<-chan LogLine, func())
}

func logLineOf(e logx.Entry) LogLine {
	return LogLine{Ts: e.Time.UnixMilli(), Level: e.Level, Service: e.Mod, Msg: e.Msg}
}

func svcLineOf(name string, e svclog.Entry) LogLine {
	return LogLine{Ts: e.Time.UnixMilli(), Level: e.Level, Service: name, Msg: e.Msg}
}

// relayFeed 把一种元素的订阅通道包一层转换（unsub 时回收转发协程）。
func relayFeed[T any](in <-chan T, unsub func(), conv func(T) LogLine) (<-chan LogLine, func()) {
	stop := make(chan struct{})
	var once sync.Once
	out := make(chan LogLine, 64)
	go func() {
		defer unsub()
		for {
			select {
			case <-stop:
				return
			case e, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- conv(e):
				case <-stop:
					return
				}
			}
		}
	}()
	return out, func() { once.Do(func() { close(stop) }) }
}

// platformFeed 平台自身日志源（logx 单例环形缓冲）。
func platformFeed() logFeed {
	return logFeed{
		recent: func() []LogLine {
			es := logx.Recent()
			out := make([]LogLine, 0, len(es))
			for _, e := range es {
				out = append(out, logLineOf(e))
			}
			return out
		},
		subscribe: func() (<-chan LogLine, func()) {
			in, unsub := logx.Subscribe()
			return relayFeed(in, unsub, logLineOf)
		},
	}
}

// serviceFeed 被管服务日志源；sink 为 nil 时为空源（订阅静默等待，靠
// 心跳与 ctx 收尾）。
func serviceFeed(sink *svclog.Sink) logFeed {
	if sink == nil {
		ch := make(chan LogLine)
		return logFeed{
			recent:    func() []LogLine { return nil },
			subscribe: func() (<-chan LogLine, func()) { return ch, func() {} },
		}
	}
	name := sink.Name()
	return logFeed{
		recent: func() []LogLine {
			es := sink.Snapshot()
			out := make([]LogLine, 0, len(es))
			for _, e := range es {
				out = append(out, svcLineOf(name, e))
			}
			return out
		},
		subscribe: func() (<-chan LogLine, func()) {
			in, unsub := sink.Subscribe()
			return relayFeed(in, unsub, func(e svclog.Entry) LogLine { return svcLineOf(name, e) })
		},
	}
}

// mergedFeed 全部合并流：平台 + 所有已建服务出口，按时间归并。实时流按
// 接收时间自然交错（不追求逻辑时钟排序——管道缓冲偏移消不掉）。
func mergedFeed() logFeed {
	sinks := svclog.All()
	sources := make([]logFeed, 0, len(sinks)+1)
	sources = append(sources, platformFeed())
	for _, sk := range sinks {
		sources = append(sources, serviceFeed(sk))
	}
	return logFeed{
		recent: func() []LogLine {
			var all []LogLine
			for _, f := range sources {
				all = append(all, f.recent()...)
			}
			sort.SliceStable(all, func(i, j int) bool { return all[i].Ts < all[j].Ts })
			return all
		},
		subscribe: func() (<-chan LogLine, func()) {
			stop := make(chan struct{})
			var once sync.Once
			out := make(chan LogLine, 128)
			for _, f := range sources {
				in, unsub := f.subscribe()
				go func(in <-chan LogLine, unsub func()) {
					defer unsub()
					for {
						select {
						case <-stop:
							return
						case e, ok := <-in:
							if !ok {
								return
							}
							select {
							case out <- e:
							case <-stop:
								return
							}
						}
					}
				}(in, unsub)
			}
			return out, func() { once.Do(func() { close(stop) }) }
		},
	}
}

// resolveLogFeed 解析 service 查询参数为日志源。
func (s *Server) resolveLogFeed(r *http.Request) (logFeed, error) {
	service := strings.TrimSpace(r.URL.Query().Get("service"))
	switch {
	case service == "", strings.EqualFold(service, "all"):
		return mergedFeed(), nil
	case strings.EqualFold(service, "platform"):
		return platformFeed(), nil
	default:
		id, err := strconv.Atoi(service)
		if err != nil || id <= 0 {
			return logFeed{}, fmt.Errorf("service 参数无效（需 all、platform 或服务 id）")
		}
		sink := s.serviceSink(id)
		if sink == nil {
			return logFeed{}, fmt.Errorf("服务不存在: #%d", id)
		}
		return serviceFeed(sink), nil
	}
}

// serviceSink 按定义取/建服务日志出口（无定义 = nil）。提前建出口使得
// 「从未启动的服务」也可订阅：服务稍后启动时日志直接流入已有订阅。
func (s *Server) serviceSink(id int) *svclog.Sink {
	var def store.Service
	if err := s.DB.First(&def, "id = ?", id).Error; err != nil {
		return nil
	}
	return svclog.Attach(def.ID, def.Name, svclog.ParsePolicy([]byte(def.LogPolicy)))
}

/* ══════════════ 快照 / 实时流 / 导出 ══════════════ */

// LogSnapshot GET /api/logs：日志快照（环形缓冲过滤，时间正序）。
func (s *Server) LogSnapshot(w http.ResponseWriter, r *http.Request) {
	feed, err := s.resolveLogFeed(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	level := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("level")))
	keyword := r.URL.Query().Get("keyword")
	lines := feed.recent()
	// 按时间正序返回（最旧在前）：SSE 实时流也是正序 flush，前端把快照直接
	// 当 oldest-first 渲染并在尾部 push 新行。
	start := 0
	if len(lines) > limit {
		start = len(lines) - limit
	}
	out := make([]LogLine, 0, limit)
	for i := start; i < len(lines); i++ {
		l := lines[i]
		// 级别存 slog 大写形态（"INFO"/"WARN"），查询参数惯用小写，折叠比较。
		if level != "" && strings.ToLower(l.Level) != level {
			continue
		}
		if keyword != "" && !strings.Contains(l.Msg, keyword) {
			continue
		}
		out = append(out, l)
	}
	ok(w, out)
}

// sseLine 一条日志序列化为 SSE data 行。
func sseLine(l LogLine) string {
	b, _ := json.Marshal(l)
	return "data: " + string(b) + "\n\n"
}

// LogStream GET /api/logs/stream：SSE 实时流。
//
// since（unix 毫秒，可选）：初始转储跳过该时间戳（含）之前的缓冲行。前端
// 先拉快照再开流，并把快照最新一行的时间戳带上——否则初始转储会把快照刚
// 渲染过的行再发一遍，面板里同一条日志出现两次；EventSource 原生重连按
// 同一 URL 重放，同样只补断线期间的增量。
func (s *Server) LogStream(w http.ResponseWriter, r *http.Request) {
	feed, err := s.resolveLogFeed(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	since := int64(0)
	if v, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64); err == nil && v > 0 {
		since = v
	}
	flusher, ok2 := w.(http.Flusher)
	if !ok2 {
		fail(w, http.StatusInternalServerError, "流式响应不可用")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	for _, l := range feed.recent() {
		if since > 0 && l.Ts <= since {
			continue
		}
		fmt.Fprint(w, sseLine(l))
	}
	flusher.Flush()

	ch, unsub := feed.subscribe()
	defer unsub()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			// 心跳必须是 data 事件而非 SSE 注释（": heartbeat"）：注释行不触发
			// 浏览器 EventSource 的 onmessage，前端 40s 看门狗看不到任何消息，
			// 会把健康的空闲连接当成半开连接掐断，陷入"断开→重连"循环。
			// 前端收到 "__heartbeat" 后只刷新活跃时间、不渲染。
			fmt.Fprint(w, "data: __heartbeat\n\n")
			flusher.Flush()
		case l, ok3 := <-ch:
			if !ok3 {
				return
			}
			fmt.Fprint(w, sseLine(l))
			flusher.Flush()
		}
	}
}

// LogExport GET /api/logs/export：日志 zip 下载。
//
// service 三态与查询一致：空/all = 平台 + 全部服务；platform = 仅平台；
// 数字 = 该服务（平台侧文件优先，无文件回落环形缓冲）。时间范围参数
// range=1h|6h|24h|today|custom（custom 需 from/to）真实生效。
func (s *Server) LogExport(w http.ResponseWriter, r *http.Request) {
	service := strings.TrimSpace(r.URL.Query().Get("service"))
	since, until, err := parseLogRange(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// zip 头写出前完成源解析与校验，失败仍能返回 JSON 错误。
	merged := service == "" || strings.EqualFold(service, "all")
	var svcSinks []*svclog.Sink
	if !merged && !strings.EqualFold(service, "platform") {
		id, idErr := strconv.Atoi(service)
		if idErr != nil || id <= 0 {
			fail(w, http.StatusBadRequest, "service 参数无效（需 all、platform 或服务 id）")
			return
		}
		sink := s.serviceSink(id)
		if sink == nil {
			fail(w, http.StatusNotFound, "服务不存在: #"+service)
			return
		}
		svcSinks = []*svclog.Sink{sink}
	} else if merged {
		svcSinks = svclog.All()
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=cairn-logs.zip")
	zw := zip.NewWriter(w)
	if merged || strings.EqualFold(service, "platform") {
		zipPlatformLog(zw, since, until)
	}
	for _, sk := range svcSinks {
		zipSinkLog(zw, sk, since, until)
	}
	_ = zw.Close()
}

// zipPlatformLog 平台自身日志入 zip（环形缓冲全量按时间过滤）。
func zipPlatformLog(zw *zip.Writer, since, until time.Time) {
	fw, err := zw.Create("cairn.log")
	if err != nil {
		return
	}
	for _, e := range logx.Recent() {
		if e.Time.Before(since) || e.Time.After(until) {
			continue
		}
		fmt.Fprintf(fw, "%s [%s] [%s] %s\n", e.Time.Format(time.RFC3339), e.Level, e.Mod, e.Msg)
	}
}

// zipSinkLog 被管服务日志入 zip：平台侧文件优先（按落盘时间戳逐行过滤），
// 无文件（从未有输出）回落环形缓冲。
func zipSinkLog(zw *zip.Writer, sk *svclog.Sink, since, until time.Time) {
	fw, err := zw.Create("services/" + exportMemberName(sk))
	if err != nil {
		return
	}
	wrote := false
	if f, err := os.Open(sk.FilePath()); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), svclog.MaxLineBytes+64)
		for sc.Scan() {
			if t, _, _, valid := svclog.ParseEnriched(sc.Text()); valid &&
				(t.Before(since) || t.After(until)) {
				continue
			}
			fmt.Fprintln(fw, sc.Text())
			wrote = true
		}
		f.Close()
	}
	if wrote {
		return
	}
	for _, e := range sk.Snapshot() {
		if e.Time.Before(since) || e.Time.After(until) {
			continue
		}
		fmt.Fprintf(fw, "%s [%s] %s\n", e.Time.Format(time.RFC3339), e.Level, e.Msg)
	}
}

// exportMemberName zip 成员名：svc-{id}-{净化名}.log（服务名可能含中文与
// 路径不安全字符）。
func exportMemberName(sk *svclog.Sink) string {
	var b strings.Builder
	fmt.Fprintf(&b, "svc-%d", sk.ID())
	name := sk.Name()
	if name != "" {
		b.WriteString("-")
		for _, r := range name {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
				b.WriteRune(r)
			default:
				b.WriteRune('_')
			}
		}
	}
	return b.String() + ".log"
}

// parseLogRange 解析导出时间范围；未指定时默认全量（返回零值下界与当前时刻）。
func parseLogRange(r *http.Request) (since, until time.Time, err error) {
	until = time.Now()
	q := r.URL.Query()
	switch q.Get("range") {
	case "", "all":
		return time.Time{}, until, nil
	case "1h":
		return until.Add(-time.Hour), until, nil
	case "6h":
		return until.Add(-6 * time.Hour), until, nil
	case "24h":
		return until.Add(-24 * time.Hour), until, nil
	case "today":
		y, m, d := until.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, until.Location()), until, nil
	case "custom":
		from, errFrom := parseLogTime(q.Get("from"))
		to, errTo := parseLogTime(q.Get("to"))
		if errFrom != nil || errTo != nil {
			return since, until, fmt.Errorf("自定义时间范围格式无效（需 RFC3339 或 yyyy-MM-dd HH:mm[:ss]）")
		}
		if to.Before(from) {
			return since, until, fmt.Errorf("结束时间不能早于开始时间")
		}
		return from, to, nil
	default:
		return since, until, fmt.Errorf("未知的时间范围: %s", q.Get("range"))
	}
}

// parseLogTime 兼容浏览器 datetime-local 的 "2006-01-02T15:04" 与带秒/时区形态。
func parseLogTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, fmt.Errorf("时间为空")
	}
	layouts := []string{
		time.RFC3339, "2006-01-02T15:04", "2006-01-02T15:04:05",
		"2006-01-02 15:04", "2006-01-02 15:04:05", "2006-01-02",
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, v, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析时间: %s", v)
}
