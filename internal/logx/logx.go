// Package logx 是平台的统一日志管理器：以标准库 log/slog 为核心，
// 将一条日志同时扇出到三路出口——终端窗口、滚动文件、前端出口接口。
//
//	业务代码 logx.Module("api").Info(...)
//	          └─ swapHandler ─┬─ TextHandler  → 终端(os.Stdout)
//	                          ├─ JSONHandler  → 滚动文件(大小 + 每日轮转)
//	                          └─ sinkHandler  → 环形缓冲 + SSE（/api/logs）
//
// 照搬 Gateway-Modbus 的实现（含热重载与旧写入器关闭的修复）。
package logx

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
)

// Options 日志管理器初始化参数。
type Options struct {
	Level       string // debug | info | warn | error（大小写不敏感）
	Console     bool   // 终端窗口输出
	File        string // 文件输出路径（空则不落文件）
	MaxSizeMB   int    // 单文件大小上限（MB），大小轮转阈值
	MaxBackups  int    // 保留历史文件份数
	MaxAgeDays  int    // 历史文件保留天数
	Compress    bool   // 历史文件 gzip 压缩
	DailyRotate bool   // 每日 00:00 轮转
	BufferSize  int    // 前端出口环形缓冲条数
}

type runtimeState struct {
	logger       *slog.Logger
	sink         *ringSink
	swap         *swapHandler
	stopRotation func()
	oldWriter    *closerHolder
}

var (
	mu      sync.RWMutex
	initMu  sync.Mutex
	runtime runtimeState
)

// Init 按配置装配三路输出并设置为全局默认日志器。可重复调用（热重载配置）。
//
// 关键点：全局只保留一个 swapHandler，Init 只替换它的内层 handler。
// 这样在构造时就捕获了 logger 的组件，也会立刻用上新配置。
func Init(opt Options) {
	initMu.Lock()
	defer initMu.Unlock()

	level := ParseLevel(opt.Level)
	var handlers []slog.Handler

	// 1) 终端窗口：文本格式，便于人读。
	if opt.Console {
		handlers = append(handlers, slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	}

	// 2) 滚动文件：JSON 格式，便于后续采集/检索；大小 + 每日轮转。
	var stopRotation func()
	var newWriter *closerHolder
	if opt.File != "" {
		writer, stop := newRotator(opt)
		stopRotation = stop
		handlers = append(handlers, slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: level}))
		if cw, ok := writer.(closableWriter); ok {
			newWriter = &closerHolder{w: cw}
		}
	}

	// 3) 前端出口：复用已有缓冲，避免热重载中断已建立的 SSE 订阅。
	mu.Lock()
	s := runtime.sink
	if s == nil {
		s = newRingSink(opt.BufferSize)
	} else {
		s.resize(opt.BufferSize)
	}
	previous := runtime
	mu.Unlock()
	handlers = append(handlers, s.handler(level))

	var inner slog.Handler
	if len(handlers) == 1 {
		inner = handlers[0]
	} else {
		inner = &fanout{handlers: handlers}
	}

	mu.Lock()
	if runtime.swap != nil {
		// 热重载：只换内层，保留已分发的 logger。
		runtime.swap.Swap(inner)
		runtime.stopRotation = stopRotation
		runtime.oldWriter = newWriter
	} else {
		swap := newSwapHandler(inner)
		runtime = runtimeState{
			logger: slog.New(swap), sink: s, swap: swap,
			stopRotation: stopRotation, oldWriter: newWriter,
		}
		slog.SetDefault(runtime.logger)
	}
	mu.Unlock()

	// 停掉旧的轮转 goroutine，并关闭旧的写入器句柄，
	// 避免每次保存设置泄漏一个文件句柄、新旧 lumberjack 互相踩踏丢日志。
	if previous.stopRotation != nil {
		previous.stopRotation()
	}
	if previous.oldWriter != nil {
		previous.oldWriter.Close()
	}
}

// Shutdown 关闭日志资源（进程退出前调用：停止轮转协程并关闭文件句柄）。
// 注意前端消费的环形缓冲是纯内存结构、从不落盘，此处不涉及"缓冲落盘"。
func Shutdown() {
	initMu.Lock()
	defer initMu.Unlock()

	mu.Lock()
	prev := runtime
	mu.Unlock()

	if prev.stopRotation != nil {
		prev.stopRotation()
	}
	if prev.oldWriter != nil {
		prev.oldWriter.Close()
	}
}

// Module 返回带模块标签的“分支”日志器；name 会作为 mod 字段附加到每条日志。
// 未初始化时回退到 slog 默认器，保证任何时刻调用都安全。
func Module(name string) *slog.Logger {
	mu.RLock()
	logger := runtime.logger
	mu.RUnlock()
	if logger == nil {
		return slog.Default().With("mod", name)
	}
	return logger.With("mod", name)
}

// ParseLevel 把级别字符串解析为 slog.Level，未知值回退 Info。
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// RequestLogLevel 决定一条 HTTP 访问日志的级别：
// 高频轮询类只读接口降到 Debug，避免打开的页面刷满日志文件。
func RequestLogLevel(method, path string, status int) slog.Level {
	if status >= 500 {
		return slog.LevelError
	}
	if status >= 400 {
		return slog.LevelWarn
	}
	if method == http.MethodGet {
		switch {
		case strings.HasPrefix(path, "/api/dashboard"),
			strings.HasPrefix(path, "/api/logs"),
			strings.HasPrefix(path, "/api/usb/status"),
			strings.HasPrefix(path, "/api/upgrade/tasks"),
			strings.HasPrefix(path, "/api/upgrade/records"),
			strings.HasPrefix(path, "/api/events"),
			strings.HasPrefix(path, "/api/services"),
			strings.HasPrefix(path, "/healthz"),
			strings.HasPrefix(path, "/readyz"):
			return slog.LevelDebug
		}
	}
	return slog.LevelInfo
}
