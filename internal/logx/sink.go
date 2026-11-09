package logx

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Entry 是提供给前端出口的一条结构化日志。
type Entry struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Mod   string    `json:"mod"`
	Msg   string    `json:"msg"`
}

// ringSink 是一个有界环形缓冲，保留最近 N 条日志，并向订阅者广播。
type ringSink struct {
	mu   sync.RWMutex
	buf  []Entry
	size int
	subs map[chan Entry]struct{}
}

func newRingSink(size int) *ringSink {
	if size <= 0 {
		size = 500
	}
	return &ringSink{size: size, subs: make(map[chan Entry]struct{})}
}

// resize updates retention without replacing the sink or disconnecting subscribers.
func (s *ringSink) resize(size int) {
	if size <= 0 {
		size = 500
	}
	s.mu.Lock()
	s.size = size
	if len(s.buf) > size {
		s.buf = s.buf[len(s.buf)-size:]
	}
	s.mu.Unlock()
}

// push 追加一条日志：超出容量丢弃最旧，并非阻塞地广播给订阅者。
func (s *ringSink) push(e Entry) {
	s.mu.Lock()
	s.buf = append(s.buf, e)
	if len(s.buf) > s.size {
		s.buf = s.buf[len(s.buf)-s.size:]
	}
	for ch := range s.subs {
		select {
		case ch <- e:
		default: // 订阅者消费不过来则丢弃该条，绝不阻塞日志主流程
		}
	}
	s.mu.Unlock()
}

// recent 返回当前缓冲快照。
func (s *ringSink) recent() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, len(s.buf))
	copy(out, s.buf)
	return out
}

func (s *ringSink) subscribe() chan Entry {
	ch := make(chan Entry, 64)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

func (s *ringSink) unsubscribe(ch chan Entry) {
	s.mu.Lock()
	delete(s.subs, ch)
	s.mu.Unlock()
}

func (s *ringSink) handler(level slog.Level) slog.Handler {
	return &sinkHandler{sink: s, level: level}
}

// sinkHandler 把 slog 记录转成 Entry 压入环形缓冲。
type sinkHandler struct {
	sink  *ringSink
	level slog.Level
	mod   string
	group string
}

func (h *sinkHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *sinkHandler) Handle(_ context.Context, r slog.Record) error {
	e := Entry{Time: r.Time, Level: r.Level.String(), Msg: r.Message, Mod: h.mod}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "mod" {
			e.Mod = a.Value.String()
			return false
		}
		return true
	})
	h.sink.push(e)
	return nil
}

func (h *sinkHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	for _, a := range attrs {
		if a.Key == "mod" {
			next.mod = a.Value.String()
		}
	}
	return &next
}

func (h *sinkHandler) WithGroup(name string) slog.Handler {
	next := *h
	next.group = name
	return &next
}

// Recent 返回平台自身日志的环形缓冲快照（/api/logs?service=platform 的数据源）。
func Recent() []Entry {
	mu.RLock()
	s := runtime.sink
	mu.RUnlock()
	if s == nil {
		return nil
	}
	return s.recent()
}

// Subscribe 订阅平台自身日志增量（/api/logs/stream 的 platform 数据源）。
// 返回的取消函数必须调用，否则订阅通道泄漏。
func Subscribe() (<-chan Entry, func()) {
	mu.RLock()
	s := runtime.sink
	mu.RUnlock()
	if s == nil {
		ch := make(chan Entry)
		return ch, func() {}
	}
	ch := s.subscribe()
	return ch, func() { s.unsubscribe(ch) }
}
