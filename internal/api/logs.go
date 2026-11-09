// logs.go — 平台日志中心（M3 被管服务日志聚合接入前的真实平台日志）。
// 快照 + SSE 实时流 + zip 导出；被管服务日志留 M3。
package api

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/logx"
)

// LogSnapshot GET /api/logs：平台日志快照（logx 环形缓冲过滤）。
func (s *Server) LogSnapshot(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	if service != "" && service != "platform" {
		notImplemented(w, "被管服务日志（M3）")
		return
	}
	limit := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	level := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("level")))
	keyword := r.URL.Query().Get("keyword")
	entries := logx.Recent()
	// 按时间正序返回（最旧在前）：SSE 实时流也是正序 flush，前端把快照直接
	// 当 oldest-first 渲染并在尾部 push 新行——原实现倒序收集会让首屏日志
	// 倒序、实时新行接在"最旧行"下方，时间线断裂。
	start := 0
	if len(entries) > limit {
		start = len(entries) - limit
	}
	out := make([]LogLine, 0, limit)
	for i := start; i < len(entries); i++ {
		e := entries[i]
		// 日志级别存的是 slog 的大写形态（"INFO"/"WARN"），查询参数惯用小写，
		// 统一折叠比较；原先直接 != 比较导致按级别过滤恒空。
		if level != "" && strings.ToLower(e.Level) != level {
			continue
		}
		if keyword != "" && !strings.Contains(e.Msg, keyword) {
			continue
		}
		out = append(out, LogLine{Ts: e.Time.UnixMilli(), Level: e.Level, Service: e.Mod, Msg: e.Msg})
	}
	ok(w, out)
}

// sseLine 一条日志序列化为 SSE data 行。
func sseLine(e logx.Entry) string {
	b, _ := json.Marshal(LogLine{Ts: e.Time.UnixMilli(), Level: e.Level, Service: e.Mod, Msg: e.Msg})
	return "data: " + string(b) + "\n\n"
}

// LogStream GET /api/logs/stream：SSE 实时流。
func (s *Server) LogStream(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	if service != "" && service != "platform" {
		notImplemented(w, "被管服务日志流（M3）")
		return
	}
	flusher, ok2 := w.(http.Flusher)
	if !ok2 {
		fail(w, http.StatusInternalServerError, "流式响应不可用")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	for _, e := range logx.Recent() {
		fmt.Fprint(w, sseLine(e))
	}
	flusher.Flush()

	ch, unsub := logx.Subscribe()
	defer unsub()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case e, ok3 := <-ch:
			if !ok3 {
				return
			}
			fmt.Fprint(w, sseLine(e))
			flusher.Flush()
		}
	}
}

// LogExport GET /api/logs/export：平台日志 zip 下载。
//
// 时间范围参数（range=1h|6h|24h|today|custom，custom 需 from/to）真实生效：
// 原先只读 service 参数、无视前端选择的范围，用户拿到的包与所选不符。
func (s *Server) LogExport(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	if service != "" && service != "platform" {
		notImplemented(w, "被管服务日志导出（M3）")
		return
	}
	since, until, err := parseLogRange(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=cairn-logs.zip")
	zw := zip.NewWriter(w)
	fw, err := zw.Create("cairn.log")
	if err != nil {
		fail(w, http.StatusInternalServerError, "创建 zip 失败: "+err.Error())
		return
	}
	for _, e := range logx.Recent() {
		if e.Time.Before(since) || e.Time.After(until) {
			continue
		}
		fmt.Fprintf(fw, "%s [%s] [%s] %s\n", e.Time.Format(time.RFC3339), e.Level, e.Mod, e.Msg)
	}
	_ = zw.Close()
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