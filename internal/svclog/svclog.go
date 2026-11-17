// Package svclog 被管服务日志出口：fork 接管的子进程 stdout/stderr 在此汇聚。
//
// 一个服务的输出进来后按 LF 切行（跨 Write 拼接残段），每行附加接收时间戳
// 与推断级别，扇出到两路：
//
//	内存环形缓冲（快照 /api/logs?service=<id> 与 SSE 实时流的数据源）
//	平台侧滚动文件 logs/services/svc-{id}.log（lumberjack 按 logPolicy 的
//	maxSizeMB/maxBackups 轮转；持久化与 zip 导出来源）
//
// 采集边界（M3 落地决策）：只采集标准输出/标准错误（fork 管道），不 tail
// 服务自写的日志文件；服务崩溃取证（D4 尾部现场）仍由 fork.tailBuffer
// 独立承担，与本包互不影响。落盘走独立 goroutine + 有界队列：磁盘慢只
// 丢弃并告警，绝不反压子进程的 write()。
package svclog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/config"
	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

const (
	// RingLines 每服务内存环形缓冲行数（快照与实时流窗口）。
	RingLines = 2000
	// MaxLineBytes 单行保留上限（超出截断；防无换行的巨块输出驻留内存）。
	MaxLineBytes = 8 << 10
	// fileQueue 落盘队列深度：满则丢弃（磁盘慢不反压子进程）。
	fileQueue = 1024
	// preloadBytes 平台重启后从平台侧文件尾部回看的字节数。
	preloadBytes = 512 << 10
)

// Entry 是一条被管服务日志（Msg 为服务的原始输出行，Time/Level 为平台侧
// 接收时间与推断级别——stdout 纯文本没有结构化时间戳/级别，只能附加）。
type Entry struct {
	Time  time.Time
	Level string // INFO | DEBUG | WARN | ERROR
	Msg   string
}

// Policy 从服务定义 logPolicy JSON 取用的参数。mode 不参与采集决策：
// 无论 stdout/file，平台都接管子进程的标准输出（file 只表示服务另自写
// 文件，平台不去读它）。
type Policy struct {
	MaxSizeMB  int `json:"maxSizeMB"`
	MaxBackups int `json:"maxBackups"`
}

// ParsePolicy 解析 logPolicy JSON（空/坏值回落默认）。
func ParsePolicy(raw []byte) Policy {
	var p Policy
	_ = json.Unmarshal(raw, &p)
	return p
}

func (p Policy) withDefaults() Policy {
	if p.MaxSizeMB < 1 {
		p.MaxSizeMB = 10
	}
	if p.MaxBackups < 1 {
		p.MaxBackups = 3
	}
	return p
}

// Sink 是一个服务的日志出口（io.Writer，fork 接到子进程 stdout/stderr 上）。
type Sink struct {
	id   int
	name string

	mu      sync.Mutex
	pending []byte // 跨 Write 的残段（无换行结尾的行尾）
	ring    []Entry
	subs    map[chan Entry]struct{}

	lj    *lumberjack.Logger
	q     chan string
	quit  chan struct{}
	done  chan struct{}
	drops atomic.Int64
}

// Write 实现 io.Writer。os/exec 为 Stdout/Stderr 各起一个拷贝协程，可能
// 并发调用，全程持锁串行化。本次调用内完成的行合并为一个批次投递落盘
// （突发大量输出也只占落盘队列一个槽位）。
func (s *Sink) Write(p []byte) (int, error) {
	now := time.Now()
	var batch []string
	s.mu.Lock()
	rest := p
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			s.pending = append(s.pending, rest...)
			// 无换行的巨块（base64/二进制垃圾）：攒到单行上限即强制吐出，
			// 防止残段无限驻留内存。
			if len(s.pending) >= MaxLineBytes {
				line := string(s.pending[:MaxLineBytes]) + "…（单行已截断）"
				s.pending = s.pending[:0]
				if l := s.ingest(line, now); l != "" {
					batch = append(batch, l)
				}
			}
			break
		}
		line := s.pending
		s.pending = s.pending[:0]
		line = append(line, rest[:i]...)
		rest = rest[i+1:]
		if l := s.ingest(string(line), now); l != "" {
			batch = append(batch, l)
		}
	}
	s.mu.Unlock()
	if len(batch) > 0 {
		s.enqueue(strings.Join(batch, "\n"))
	}
	return len(p), nil
}

// ingest 处理一行（须持锁）：清 \r、跳过空行、截断、入环形缓冲、广播；
// 返回该行的落盘形态（空串 = 跳过未入缓冲）。
func (s *Sink) ingest(line string, now time.Time) string {
	line = strings.TrimRight(line, "\r")
	if line == "" {
		return "" // 空行零信息量，不进日志页/文件
	}
	if len(line) > MaxLineBytes {
		line = line[:MaxLineBytes] + "…（单行已截断）"
	}
	e := Entry{Time: now, Level: inferLevel(line), Msg: line}
	s.ring = append(s.ring, e)
	if len(s.ring) > RingLines {
		s.ring = s.ring[len(s.ring)-RingLines:]
	}
	for ch := range s.subs {
		select {
		case ch <- e:
		default: // 慢订阅者丢弃，绝不反压子进程
		}
	}
	return fmt.Sprintf("%s [%s] %s", now.Format(time.RFC3339), e.Level, e.Msg)
}

// enqueue 非阻塞投递落盘批次：队列满（磁盘持续跟不上）丢弃并按千次告警，
// 绝不反压子进程的 write()。
func (s *Sink) enqueue(batch string) {
	select {
	case s.q <- batch:
	default:
		if n := s.drops.Add(1); n%1000 == 1 {
			logx.Module("svclog").Warn("服务日志落盘跟不上，已丢弃批次", "service", s.name, "droppedBatches", n)
		}
	}
}

// writeLoop 独立落盘协程：管道字节只在内存里短暂停留，磁盘慢不影响子进程。
func (s *Sink) writeLoop() {
	defer close(s.done)
	for {
		select {
		case <-s.quit:
			// 退出前排空在途队列（尽力而为）。
			for {
				select {
				case line := <-s.q:
					_, _ = fmt.Fprintln(s.lj, line)
				default:
					return
				}
			}
		case line := <-s.q:
			if _, err := fmt.Fprintln(s.lj, line); err != nil {
				logx.Module("svclog").Warn("服务日志写文件失败", "service", s.name, "err", err)
				time.Sleep(time.Second) // 磁盘满等持续错误退避，避免刷屏
			}
		}
	}
}

// Snapshot 环形缓冲拷贝（时间正序，最旧在前）。
func (s *Sink) Snapshot() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, len(s.ring))
	copy(out, s.ring)
	return out
}

// Subscribe 订阅增量；返回的取消函数必须调用，否则通道泄漏。
func (s *Sink) Subscribe() (<-chan Entry, func()) {
	ch := make(chan Entry, 64)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// ID 服务 id。
func (s *Sink) ID() int { return s.id }

// Name 服务名（日志行的 service 字段）。
func (s *Sink) Name() string { return s.name }

// FilePath 平台侧落盘文件路径（导出用）。
func (s *Sink) FilePath() string { return s.lj.Filename }

func (s *Sink) close() {
	close(s.quit)
	<-s.done
	_ = s.lj.Close()
}

// preload 平台重启后的历史回填：读平台侧文件尾部（≤preloadBytes），按
// 落盘格式解析回 Entry 填入环形缓冲。读的是平台自己写的文件——与服务
// 自写文件无关，不违背「不 tail 服务日志文件」的采集边界。
func (s *Sink) preload() {
	f, err := os.Open(s.lj.Filename)
	if err != nil {
		return // 无历史文件（首次运行）
	}
	defer f.Close()
	off := int64(0)
	if fi, err := f.Stat(); err == nil && fi.Size() > preloadBytes {
		off = fi.Size() - preloadBytes
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), MaxLineBytes+64)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // 起点切在行中间，首行是残段
	}
	for _, l := range lines {
		if t, lvl, msg, valid := ParseEnriched(l); valid {
			s.ring = append(s.ring, Entry{Time: t, Level: lvl, Msg: msg})
		}
	}
	if len(s.ring) > RingLines {
		s.ring = s.ring[len(s.ring)-RingLines:]
	}
}

/* ══════════════ 全局出口登记 ══════════════ */

var (
	mgrMu sync.Mutex
	sinks = map[int]*Sink{}
)

// Attach 返回服务日志出口（幂等）：首次创建并从平台侧文件尾部回填历史；
// 此后（服务重启、API 侧查询）复用同一实例——服务重启不清空日志页历史。
func Attach(id int, name string, p Policy) *Sink {
	mgrMu.Lock()
	defer mgrMu.Unlock()
	if s, ok := sinks[id]; ok {
		return s
	}
	s := newSink(id, name, p, config.LogsDir("services"))
	sinks[id] = s
	return s
}

// Get 只读查询（无出口时 nil）。
func Get(id int) *Sink {
	mgrMu.Lock()
	defer mgrMu.Unlock()
	return sinks[id]
}

// All 全部出口（按 id 升序，合并流输出顺序稳定）。
func All() []*Sink {
	mgrMu.Lock()
	defer mgrMu.Unlock()
	out := make([]*Sink, 0, len(sinks))
	for _, s := range sinks {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// Shutdown 关闭全部出口（停落盘协程、关文件句柄）；进程退出前调用。
func Shutdown() {
	mgrMu.Lock()
	defer mgrMu.Unlock()
	for id, s := range sinks {
		s.close()
		delete(sinks, id)
	}
}

// newSink 构造服务日志出口；dir 为平台侧文件目录（测试可注入临时目录）。
func newSink(id int, name string, p Policy, dir string) *Sink {
	p = p.withDefaults()
	s := &Sink{
		id:   id,
		name: name,
		subs: make(map[chan Entry]struct{}),
		lj: &lumberjack.Logger{
			Filename:   filepath.Join(dir, fmt.Sprintf("svc-%d.log", id)),
			MaxSize:    p.MaxSizeMB,
			MaxBackups: p.MaxBackups,
			LocalTime:  true,
		},
		q:    make(chan string, fileQueue),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	s.preload()
	go s.writeLoop()
	return s
}

/* ══════════════ 纯文本行的级别推断与落盘格式 ══════════════ */

var (
	kvLevelRe = regexp.MustCompile(`(?i)\blevel\s*[=:]\s*(trace|debug|info|warn(?:ing)?|error|err|fatal|panic)\b`)
	tsPrefix  = regexp.MustCompile(`(?i)^\s*(?:\d{4}[-/]\d{1,2}[-/]\d{1,2}[T ][\d:.,]+(?:[+\-]\d{1,2}:?\d{2}|Z)?\s*|[a-z]{3}\s+\d{1,2}\s+\d{2}:\d{2}:\d{2}\s*)`)
)

// inferLevel 从纯文本行推断级别（约定俗成形态；推断不出按 INFO）：
// level=error 键值、"[ERROR] …" 括号标记、"2026-11-17 14:30:00 WARN …" 行首词。
func inferLevel(line string) string {
	if m := kvLevelRe.FindStringSubmatch(line); m != nil {
		return normLevel(m[1])
	}
	s := strings.TrimSpace(tsPrefix.ReplaceAllString(line, ""))
	for i := 0; i < 3 && strings.HasPrefix(s, "["); i++ {
		if j := strings.Index(s, "]"); j > 0 {
			if inner := strings.TrimSpace(s[1:j]); isLevelWord(inner) {
				return normLevel(inner)
			}
			s = strings.TrimSpace(s[j+1:])
			continue
		}
		break
	}
	word := s
	if i := strings.IndexAny(word, " \t:,;|"); i > 0 {
		word = word[:i]
	}
	word = strings.Trim(word, "[]")
	if isLevelWord(word) {
		return normLevel(word)
	}
	return "INFO"
}

func isLevelWord(s string) bool {
	switch strings.ToLower(s) {
	case "trace", "debug", "info", "warn", "warning", "error", "err", "fatal", "panic":
		return true
	}
	return false
}

func normLevel(s string) string {
	switch strings.ToLower(s) {
	case "trace", "debug":
		return "DEBUG"
	case "warn", "warning":
		return "WARN"
	case "error", "err", "fatal", "panic":
		return "ERROR"
	default:
		return "INFO"
	}
}

var enrichedRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T[\d:.]+(?:Z|[+\-]\d{2}:\d{2})) \[([A-Z]+)\] (.*)$`)

// ParseEnriched 解析平台侧落盘行（"time [LEVEL] msg"）；导出时间过滤与
// 重启回填共用。
func ParseEnriched(line string) (time.Time, string, string, bool) {
	m := enrichedRe.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}, "", "", false
	}
	t, err := time.Parse(time.RFC3339, m[1])
	if err != nil {
		return time.Time{}, "", "", false
	}
	return t, m[2], m[3], true
}
