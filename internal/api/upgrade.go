// upgrade.go — 升级任务队列与端点（decisions.md A/B/C/D）。
//
// 职责边界（E1/D10）：队列只负责把包安全地换到位并发出启动指令；
// 服务启动后的存活与健康归运行监控，不属于升级业务。
//
// 全局串行（D7）：单队列、同一时刻仅一个任务；每任务超时 10 分钟防卡死。
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jiwei-cairn/cairn-pkg/pack"
	"github.com/jiwei-cairn/cairn-platform/internal/assets"
	"github.com/jiwei-cairn/cairn-platform/internal/config"
	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/store"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	goruntime "runtime"
)

/* ═══════════════════ 任务管理器（全局串行队列） ═══════════════════ */

// 升级任务的阶段（phase）。
const (
	PhaseQueued      = "queued"
	PhaseVerifying   = "verifying"   // 包校验
	PhaseDownloading = "downloading" // OTA 下载
	PhasePrecheck    = "precheck"    // 预检
	PhaseStopping    = "stopping"    // 停服等待退出
	PhaseSwapping    = "swapping"    // 换文件
	PhaseStarting    = "starting"    // 发起启动
	PhaseSuccess     = "success"
	PhaseFailed      = "failed"
)

// taskTimeout 单任务超时（卡死防护，decisions.md §10）。
const taskTimeout = 10 * time.Minute

// maxRetainedTasks 内存中保留的任务数上限（超出淘汰最旧已结束任务）。
// 任务对象持有 run 闭包（捕获 Server 等），不清理会造成闭包链常驻。
const maxRetainedTasks = 200

// taskRetention 终态任务的保留期：结束后至少保留这么久才允许淘汰，
// 避免前端轮询迟到而拿到 404。
const taskRetention = 10 * time.Minute

// upgradeTask 一个升级/安装任务。
type upgradeTask struct {
	ID        string    `json:"taskId"`
	Mode      string    `json:"mode"`   // install | upgrade
	Source    string    `json:"source"` // local | ota
	Name      string    `json:"name"`
	FromVer   string    `json:"fromVersion"`
	ToVer     string    `json:"toVersion"`
	Phase     string    `json:"phase"`
	Detail    string    `json:"detail,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// run 执行体：必须响应 ctx 取消。超时后 worker 不再等待，
	// 但执行体也应在阶段边界主动中止，避免与后续任务并发操作同一目录。
	run func(ctx context.Context, t *upgradeTask) error
}

// taskManager 全局串行任务管理器。
type taskManager struct {
	mu     sync.Mutex
	tasks  map[string]*upgradeTask
	order  []string // 插入顺序（用于按序淘汰）
	ch     chan *upgradeTask
	wg     sync.WaitGroup
	closed bool

	// onFinish 任务到达终态后的回调（升级台账落库，M2-8）。
	// 由 Server.New 装配；worker 收尾时以任务快照调用，失败不影响任务状态。
	// 注意：队列满被直接拒收的任务不经 worker，不会触发本回调（未执行，不记台账）。
	onFinish func(t *upgradeTask)
}

func newTaskManager() *taskManager {
	m := &taskManager{
		tasks: make(map[string]*upgradeTask),
		ch:    make(chan *upgradeTask, 64),
	}
	m.wg.Add(1)
	go m.worker()
	return m
}

// worker 单消费者：全局串行执行（D7）。
//
// 超时语义：ctx 到期后 worker 不再等待，但执行体自身也监听 ctx——两者共同
// 保证「同一时刻只有一个部署流水线在动文件」，避免超时任务与后续任务并发
// 操作同一服务目录（原实现只停止等待，goroutine 继续解包/改名）。
func (m *taskManager) worker() {
	defer m.wg.Done()
	for t := range m.ch {
		ctx, cancel := context.WithTimeout(context.Background(), taskTimeout)
		done := make(chan error, 1)
		go func() { done <- t.run(ctx, t) }()
		var err error
		select {
		case err = <-done:
		case <-ctx.Done():
			// 给执行体一个收敛窗口：它在阶段边界看到 ctx.Done 会自行退出。
			select {
			case err = <-done:
			case <-time.After(taskAbortGrace):
				err = fmt.Errorf("任务超时（%s）被中止", taskTimeout)
			}
		}
		cancel()
		m.mu.Lock()
		if err != nil {
			t.Phase = PhaseFailed
			t.Error = err.Error()
		} else {
			t.Phase = PhaseSuccess
		}
		t.UpdatedAt = time.Now()
		m.mu.Unlock()
		// 终态回调（升级台账 M2-8）：传快照，回调内部不再触碰 t。
		if m.onFinish != nil {
			m.onFinish(snapshotTask(t))
		}
		m.sweep() // 每轮结束清扫过期任务（不依赖数量超限）
	}
}

// taskAbortGrace 超时后等待执行体响应取消的宽限期。
// 执行体在解包/换文件等长操作之间检查 ctx，通常毫秒级退出；
// 超出宽限期也不再等待（标记失败，极端残留由下次启动清理 .staging 兜底）。
const taskAbortGrace = 5 * time.Second

// Submit 入队一个任务，返回任务快照。
func (m *taskManager) Submit(mode, source, name, from, to string, run func(ctx context.Context, t *upgradeTask) error) *upgradeTask {
	t := &upgradeTask{
		ID: "up-" + uuid.NewString()[:8], Mode: mode, Source: source,
		Name: name, FromVer: from, ToVer: to,
		Phase: PhaseQueued, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		run: run,
	}
	m.mu.Lock()
	m.tasks[t.ID] = t
	m.order = append(m.order, t.ID)
	m.evictLocked()
	m.mu.Unlock()
	// 队列满：极小概率（64 深度），拒绝而非阻塞。
	select {
	case m.ch <- t:
	default:
		m.mu.Lock()
		t.Phase = PhaseFailed
		t.Error = "升级队列已满，请稍后重试"
		t.UpdatedAt = time.Now()
		m.mu.Unlock()
	}
	return snapshotTask(t)
}

// evictLocked 淘汰超出上限的「已结束且已过保留期」任务（调用方须持锁）。
//
// 保护规则：
//   - 排队中/执行中的任务绝不淘汰（前端仍在轮询，丢弃即 404）
//   - 终态任务也需超过 taskRetention 才可淘汰（避免刚结束就被清掉，前端来不及拉结果）
func (m *taskManager) evictLocked() {
	if len(m.order) <= maxRetainedTasks {
		return
	}
	cutoff := time.Now().Add(-taskRetention)
	keep := make([]string, 0, len(m.order))
	excess := len(m.order) - maxRetainedTasks
	for _, id := range m.order {
		t := m.tasks[id]
		if excess > 0 && t != nil &&
			(t.Phase == PhaseSuccess || t.Phase == PhaseFailed) &&
			t.UpdatedAt.Before(cutoff) {
			delete(m.tasks, id)
			excess--
			continue
		}
		keep = append(keep, id)
	}
	m.order = keep
}

// Get 查询任务（返回快照，避免外部拿到内部指针）。
func (m *taskManager) Get(id string) *upgradeTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tasks[id]; ok {
		return snapshotTask(t)
	}
	return nil
}

// sweep 定期清扫过期终态任务（由 worker 在每轮任务结束时调用）。
// 与 evictLocked 的区别：sweep 不强依赖数量超限，只清「过期且终态」的。
func (m *taskManager) sweep() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.order) == 0 {
		return
	}
	cutoff := time.Now().Add(-taskRetention)
	keep := m.order[:0]
	for _, id := range m.order {
		t := m.tasks[id]
		if t != nil && (t.Phase == PhaseSuccess || t.Phase == PhaseFailed) && t.UpdatedAt.Before(cutoff) {
			delete(m.tasks, id)
			continue
		}
		keep = append(keep, id)
	}
	m.order = keep
}

// snapshotTask 拷贝任务状态（去掉 run 闭包）。
func snapshotTask(t *upgradeTask) *upgradeTask {
	cp := *t
	cp.run = nil
	return &cp
}

func (m *taskManager) updatePhase(t *upgradeTask, phase, detail string) {
	m.mu.Lock()
	t.Phase = phase
	t.Detail = detail
	t.UpdatedAt = time.Now()
	m.mu.Unlock()
}

/* ═══════════════════ OTA 清单拉取 ═══════════════════ */

// checkResultItem 单个服务的检查结果。
type checkResultItem struct {
	Name      string `json:"name"`
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	HasUpdate bool   `json:"hasUpdate"`
	SizeBytes int64  `json:"sizeBytes"`
	Notes     string `json:"notes"`
	Installed bool   `json:"installed"` // 本机是否已安装该服务
}

// UpgradeCheck POST /api/upgrade/check：拉远程清单并比对（B5/B7）。
//
// 优先级（ota-server.md §7）：远程 index.json > 内置默认清单（仅展示，I2）。
// 服务器未配置或不可达时返回 offline=true 并回落内置清单。
func (s *Server) UpgradeCheck(w http.ResponseWriter, r *http.Request) {
	cfg := s.Settings.Get()
	type svcRowLite struct {
		Name    string
		Version string
	}
	var defs []svcRowLite
	s.DB.Model(&store.Service{}).Select("name, version").Scan(&defs)
	installed := make(map[string]string, len(defs))
	for _, d := range defs {
		installed[d.Name] = d.Version
	}

	out := struct {
		Enabled  bool              `json:"enabled"` // OTA 是否已配置服务器
		Offline  bool              `json:"offline"` // 已配置但拉取失败
		Services []checkResultItem `json:"services"`
	}{Enabled: strings.TrimSpace(cfg.OTA.ServerURL) != "", Services: []checkResultItem{}}

	var idx *pack.ServicesIndex
	if out.Enabled {
		i, err := fetchServicesIndex(cfg.OTA.ServerURL, cfg.OTA.TimeoutSec)
		if err != nil {
			logx.Module("api").Warn("OTA 清单拉取失败，回落内置清单", "err", err)
			out.Offline = true
		} else {
			idx = i
		}
	}
	if idx == nil {
		idx = assets.EmbeddedServicesIndex() // 仅展示（I2/I3）
	}

	// 已装服务：比对最新版（D3：不一致即可升）。
	seen := make(map[string]bool)
	for name, cur := range installed {
		seen[name] = true
		item := checkResultItem{Name: name, Current: cur, Installed: true}
		if e := idx.LatestFor(name, goruntime.GOOS, goruntime.GOARCH); e != nil {
			item.Latest = e.Version
			item.SizeBytes = e.SizeBytes
			item.Notes = e.Notes
			// 语义化版本比对（修订决策：OTA 防倒升）：仅 latest > current 才提示，
			// 相等/更旧都不提示（回退走本地包通道）。预发布版低于同版本正式版。
			item.HasUpdate = pack.MustCompareSemVer(e.Version, cur) > 0
		}
		out.Services = append(out.Services, item)
	}
	// 服务器上有、本机未装的服务（可OTA安装）。
	for _, e := range idx.FilterPlatform(goruntime.GOOS, goruntime.GOARCH) {
		if seen[e.Name] {
			continue
		}
		if best := idx.LatestFor(e.Name, goruntime.GOOS, goruntime.GOARCH); best != nil && best.Version == e.Version {
			out.Services = append(out.Services, checkResultItem{
				Name: e.Name, Latest: e.Version, HasUpdate: false,
				SizeBytes: e.SizeBytes, Notes: e.Notes, Installed: false,
			})
		}
	}
	ok(w, out)
}

// cleanOtaBase 规范化用户配置的 OTA 服务器基址：
// 去空白、去尾斜杠、剥掉误加的 /cairn 后缀（平台会自行拼接 /cairn/... 前缀，
// 用户在地址里带上它是常见误会——实测曾导致 /cairn/cairn/index.json 404，
// 前端表现为「OTA 服务器不可达」，尽管 IP 与端口完全正确）。
func cleanOtaBase(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if strings.HasSuffix(s, "/cairn") {
		s = strings.TrimSuffix(s, "/cairn")
	}
	return s
}

// fetchServicesIndex 拉根索引 → 服务清单（URL 相对解析，G3/G7）。
func fetchServicesIndex(baseURL string, timeoutSec int) (*pack.ServicesIndex, error) {
	base := cleanOtaBase(baseURL)
	if base == "" {
		return nil, fmt.Errorf("OTA 服务器未配置")
	}
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	client := &http.Client{Timeout: time.Duration(timeoutSec) * time.Second}

	rootData, rootURL, err := httpGetJSON(client, base+"/cairn/index.json")
	if err != nil {
		return nil, fmt.Errorf("拉取根索引失败: %w", err)
	}
	root, err := pack.ParseRootIndex(rootData)
	if err != nil {
		return nil, err
	}
	svcData, _, err := httpGetJSON(client, resolveRel(rootURL, root.Domains.Services.Index))
	if err != nil {
		return nil, fmt.Errorf("拉取服务清单失败: %w", err)
	}
	return pack.ParseServicesIndex(svcData)
}

// httpGetJSON GET 并返回 body 与最终 URL。
func httpGetJSON(client *http.Client, u string) ([]byte, *url.URL, error) {
	resp, err := client.Get(u)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, err
	}
	return data, resp.Request.URL, nil
}

// resolveRel 相对 base 解析 ref（清单 url 是相对路径，G7）。
func resolveRel(base *url.URL, ref string) string {
	if ref == "" {
		return ""
	}
	if ru, err := url.Parse(ref); err == nil {
		return base.ResolveReference(ru).String()
	}
	return ref
}

/* ═══════════════════ OTA 下载与执行 ═══════════════════ */

// UpgradeApply POST /api/upgrade/apply {name}：从 OTA 下载最新版并升级（B5）。
func (s *Server) UpgradeApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		fail(w, http.StatusBadRequest, "请求体需要 {name}")
		return
	}
	cfg := s.Settings.Get()
	base := strings.TrimSpace(cfg.OTA.ServerURL)
	if base == "" {
		fail(w, http.StatusConflict, "OTA 服务器未配置（设置 → 远程升级）")
		return
	}
	idx, err := fetchServicesIndex(base, cfg.OTA.TimeoutSec)
	if err != nil {
		fail(w, http.StatusBadGateway, "拉取清单失败: "+err.Error())
		return
	}
	entry := idx.LatestFor(req.Name, goruntime.GOOS, goruntime.GOARCH)
	if entry == nil {
		fail(w, http.StatusNotFound, "清单中找不到该服务在本机平台的包")
		return
	}

	// 版本门控：仅当清单版本「严格大于」当前版本才允许升级（等于/低于均拒绝）；
	// 回退到旧版本请走本地包通道（POST /upgrade/local，不做版本方向限制）。
	var def store.Service
	hasSvc := s.DB.First(&def, "name = ?", req.Name).Error == nil
	if hasSvc && pack.MustCompareSemVer(entry.Version, def.Version) <= 0 {
		fail(w, http.StatusConflict, fmt.Sprintf(
			"清单最新 %s 不高于当前版本 %s，无需升级；如需更换版本请使用本地包升级", entry.Version, def.Version))
		return
	}

	srv := s
	task := s.Tasks.Submit(modeOf(hasSvc), "ota", req.Name, svcVersionOr(hasSvc, &def), entry.Version,
		func(ctx context.Context, t *upgradeTask) error {
			return srv.runOtaTask(ctx, t, entry, base)
		})
	ok(w, map[string]any{"taskId": task.ID, "mode": task.Mode})
}

// runOtaTask OTA 任务：下载 → 校验 → 部署流水线。
func (s *Server) runOtaTask(ctx context.Context, t *upgradeTask, entry *pack.IndexEntry, base string) error {
	s.Tasks.updatePhase(t, PhaseDownloading, entry.URL)
	pkgPath, err := s.otaDownload(ctx, t, entry, base)
	if err != nil {
		return fmt.Errorf("下载失败: %w", err)
	}
	return s.runDeployPipeline(ctx, t, pkgPath, "ota")
}

/* ═══════════════════ 下载可靠性（ota-server.md §7 步骤 8-9） ═══════════════════ */

// maxDownloadAttempts 下载尝试次数上限（含首次），对应「不符 → 重试（最多 N 次）」。
const maxDownloadAttempts = 3

// downloadKind 错误分类，决定重试策略。
type downloadKind int

const (
	downloadFatal   downloadKind = iota // 确定性失败：4xx / URL 非法 / 任务取消——立即返回
	downloadRetry                       // 瞬时失败：网络错误 / 5xx——重试，保留 .part 断点
	downloadCorrupt                     // 完整性不符：哈希/大小与清单不符——重试，丢弃 .part 从零重下
)

// downloadError 携带重试策略的下载错误。
type downloadError struct {
	kind downloadKind
	err  error
}

func (e *downloadError) Error() string { return e.err.Error() }
func (e *downloadError) Unwrap() error { return e.err }

// otaDownload 下载包到 data/upgrade-download/，整包 sha256 校验通过后落位（S4）。
//
// 可靠性语义：
//   - 断点续传：.part 残留字节数作为 Range 起点；服务器不支持 Range（回 200）
//     则断点作废整包重收；416 视为「断点已收满」先做最终校验再决定去留；
//   - 失败重试：网络错误 / 5xx 保留断点重试；哈希/大小不符丢弃 .part 从零重试；
//     4xx 与任务取消立即失败。重试间做线性退避，每次尝试独立受 OTA 超时约束
//     （client.Timeout 按次生效，长包不会因累计超时误判）。
//   - 最终校验对 .part 全文件计算 sha256，续传拼接的内容同样被完整覆盖。
//
// .part 生命周期：任务内跨尝试复用；跨任务同名包（四元组文件名唯一）续用残点；
// 判定损坏即删。最终成功后改名落位。
func (s *Server) otaDownload(ctx context.Context, t *upgradeTask, entry *pack.IndexEntry, base string) (string, error) {
	dir := config.DataDir("upgrade-download")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, filepath.Base(strings.ReplaceAll(entry.URL, "\\", "/")))
	part := dst + ".part"

	var lastErr error
	for attempt := 1; attempt <= maxDownloadAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("任务已取消: %w", err)
		}
		if attempt > 1 {
			// 线性退避：500ms × 已尝试次数，任务取消即刻退出。
			select {
			case <-time.After(time.Duration(attempt-1) * 500 * time.Millisecond):
			case <-ctx.Done():
				return "", fmt.Errorf("任务已取消: %w", ctx.Err())
			}
		}
		if t != nil {
			hint := ""
			if st, err := os.Stat(part); err == nil && st.Size() > 0 {
				hint = fmt.Sprintf("，续传自 %.1fMB", float64(st.Size())/(1<<20))
			}
			s.Tasks.updatePhase(t, PhaseDownloading,
				fmt.Sprintf("下载中（第 %d/%d 次%s）", attempt, maxDownloadAttempts, hint))
		}
		_, err := s.otaDownloadOnce(ctx, entry, base, part)
		if err == nil {
			if err := os.Rename(part, dst); err != nil {
				return "", err
			}
			return dst, nil
		}
		lastErr = err
		var de *downloadError
		if errors.As(err, &de) {
			switch de.kind {
			case downloadFatal:
				return "", err
			case downloadCorrupt:
				_ = os.Remove(part) // 内容不可信，断点作废
			}
			// downloadRetry：保留断点直接重试
		}
	}
	return "", fmt.Errorf("重试 %d 次后仍失败: %w", maxDownloadAttempts, lastErr)
}

// otaDownloadOnce 单次下载尝试：断点续传请求 → 追加写入 → 大小与整包 sha256 校验。
// 返回本次尝试后 .part 的字节数（诊断用）。
func (s *Server) otaDownloadOnce(ctx context.Context, entry *pack.IndexEntry, base, part string) (int64, error) {
	cfg := s.Settings.Get()
	timeout := cfg.OTA.TimeoutSec
	if timeout <= 0 {
		timeout = 30
	}
	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}

	u := entry.URL
	if ru, err := url.Parse(u); err == nil && !ru.IsAbs() {
		if bu, err2 := url.Parse(cleanOtaBase(base) + "/cairn/services/index.json"); err2 == nil {
			u = bu.ResolveReference(ru).String()
		}
	}

	// 断点续传：.part 已收字节作为 Range 起点。
	var have int64
	if st, err := os.Stat(part); err == nil {
		have = st.Size()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return have, &downloadError{downloadFatal, fmt.Errorf("构造请求失败: %w", err)}
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	// 请求绑定 ctx：任务超时取消时立刻中断下载，不留悬挂连接。
	resp, err := client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return have, &downloadError{downloadFatal, fmt.Errorf("任务已取消: %w", ctxErr)}
		}
		return have, &downloadError{downloadRetry, err}
	}
	defer resp.Body.Close()

	switch sc := resp.StatusCode; {
	case sc == http.StatusPartialContent:
		// 206：校验 Content-Range 起点与总长一致后，从断点续写。
		start, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != have {
			_ = os.Remove(part) // 服务器分片语义对不上，断点不可信
			return 0, &downloadError{downloadRetry, fmt.Errorf(
				"Content-Range 与断点不符（header %q，断点 %d）", resp.Header.Get("Content-Range"), have)}
		}
		if entry.SizeBytes > 0 && total > 0 && total != entry.SizeBytes {
			return have, &downloadError{downloadFatal, fmt.Errorf(
				"包总大小与清单不符（清单 %d，服务器 %d）", entry.SizeBytes, total)}
		}
	case sc == http.StatusOK:
		// 200：服务器忽略 Range（不支持续传）→ 断点作废，响应即整包。
		have = 0
	case sc == http.StatusRequestedRangeNotSatisfiable:
		// 416：断点 ≥ 总长 → .part 可能已收完；先做最终校验，通过即成功。
		if err := verifyPart(part, entry); err == nil {
			return have, nil
		}
		_ = os.Remove(part)
		return 0, &downloadError{downloadCorrupt, fmt.Errorf("断点失效（416），将整包重下")}
	case sc >= 500:
		return have, &downloadError{downloadRetry, fmt.Errorf("HTTP %d", sc)}
	default:
		// 404/403 等确定性失败：重试无意义。
		return have, &downloadError{downloadFatal, fmt.Errorf("HTTP %d", sc)}
	}

	// 快速确认：响应剩余字节数应与「清单总长 - 断点」吻合。
	if entry.SizeBytes > 0 && resp.ContentLength > 0 {
		if expect := entry.SizeBytes - have; resp.ContentLength != expect {
			return have, &downloadError{downloadFatal, fmt.Errorf(
				"剩余字节数与清单不符（应 %d，响应 %d）", expect, resp.ContentLength)}
		}
	}

	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return have, &downloadError{downloadFatal, err}
	}
	if have == 0 {
		_ = f.Truncate(0) // 200 回退场景：清掉不可信的旧断点内容
	}
	if _, err := f.Seek(have, io.SeekStart); err != nil {
		_ = f.Close()
		return have, &downloadError{downloadFatal, err}
	}
	n, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return have, &downloadError{downloadFatal, fmt.Errorf("任务已取消: %w", ctxErr)}
		}
		return have + n, &downloadError{downloadRetry, copyErr}
	}
	if closeErr != nil {
		return have + n, &downloadError{downloadFatal, closeErr}
	}
	// 下载完成后再确认一次任务未被取消：避免超时任务继续推进到部署阶段。
	if ctxErr := ctx.Err(); ctxErr != nil {
		return have, &downloadError{downloadFatal, fmt.Errorf("任务已取消: %w", ctxErr)}
	}

	// 最终校验：大小 + 整包 sha256（对 .part 全文件读回，覆盖续传拼接内容）。
	if err := verifyPart(part, entry); err != nil {
		return have + n, err
	}
	return have + n, nil
}

// verifyPart 校验 .part 的大小与整包 sha256（S4）。
func verifyPart(path string, entry *pack.IndexEntry) error {
	st, err := os.Stat(path)
	if err != nil {
		return &downloadError{downloadRetry, err}
	}
	if entry.SizeBytes > 0 && st.Size() != entry.SizeBytes {
		return &downloadError{downloadCorrupt, fmt.Errorf("下载不完整（%d/%d）", st.Size(), entry.SizeBytes)}
	}
	f, err := os.Open(path)
	if err != nil {
		return &downloadError{downloadFatal, err}
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return &downloadError{downloadRetry, err}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != entry.Sha256 {
		return &downloadError{downloadCorrupt, fmt.Errorf("整包 sha256 不符（清单 %s…，实际 %s…）",
			shortHash(entry.Sha256), shortHash(got))}
	}
	return nil
}

// shortHash 取哈希前 8 位用于日志/错误信息（防御空串与超短串）。
func shortHash(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// parseContentRange 解析 "bytes start-end/total"。total 为 "*" 时返回 -1（未知）。
func parseContentRange(v string) (start, total int64, ok bool) {
	v = strings.TrimSpace(v)
	rest, found := strings.CutPrefix(v, "bytes ")
	if !found {
		return 0, 0, false
	}
	rangePart, totalPart, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, false
	}
	startStr, _, found := strings.Cut(rangePart, "-")
	if !found {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if totalPart == "*" {
		return start, -1, true
	}
	total, err = strconv.ParseInt(totalPart, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return start, total, true
}

/* ═══════════════════ 本地上传升级 ═══════════════════ */

// UpgradeLocal POST /api/upgrade/local：上传 .cairnpkg，安装或升级（C1/C2/C3）。
//
// 服务名已存在 → 升级（可任意版本，含回退，B6/C2）；不存在 → 安装。
func (s *Server) UpgradeLocal(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		fail(w, http.StatusRequestEntityTooLarge, "上传超限或解析失败（单请求上限 256MB）: "+err.Error())
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		fail(w, http.StatusBadRequest, "未收到文件（字段名 file）")
		return
	}
	fh := files[0]
	if !strings.HasSuffix(strings.ToLower(fh.Filename), ".cairnpkg") {
		fail(w, http.StatusBadRequest, "仅接受 .cairnpkg 包")
		return
	}
	src, err := fh.Open()
	if err != nil {
		fail(w, http.StatusBadRequest, "读取上传失败: "+err.Error())
		return
	}
	defer src.Close()
	stageDir := config.UploadsDir(uuid.NewString())
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	pkgPath := filepath.Join(stageDir, "pkg.cairnpkg")
	if err := copyReaderToFile(src, pkgPath, fh.Size); err != nil {
		fail(w, http.StatusInternalServerError, "暂存失败: "+err.Error())
		return
	}

	// 快速结构校验：包不合法立即 400（不占用队列）。
	p, err := pack.Open(pkgPath)
	if err != nil {
		_ = os.RemoveAll(stageDir)
		fail(w, http.StatusBadRequest, "包校验失败: "+err.Error())
		return
	}
	name, version := p.Manifest.Name, p.Manifest.Version
	_ = p.Close()

	var def store.Service
	hasSvc := s.DB.First(&def, "name = ?", name).Error == nil
	// 语义化比较：1.0.0 与 1.0.0+build.1 视为同版本（构建元数据不参与比较），
	// 与 check/apply 的判定口径保持一致。
	if hasSvc && pack.MustCompareSemVer(version, def.Version) == 0 {
		// 同版本重装允许（幂等，D3 语义是"不一致即可升"，同版本需要显式确认）。
		_ = os.RemoveAll(stageDir)
		fail(w, http.StatusConflict, fmt.Sprintf("当前已是 %s；安装相同版本请先卸载或使用新版本号", version))
		return
	}

	srv := s
	task := s.Tasks.Submit(modeOf(hasSvc), "local", name, svcVersionOr(hasSvc, &def), version,
		func(ctx context.Context, t *upgradeTask) error {
			err := srv.runDeployPipeline(ctx, t, pkgPath, "local")
			// 安装引用的暂存目录用后即清（含失败路径）。
			_ = os.RemoveAll(stageDir)
			return err
		})
	ok(w, map[string]any{"taskId": task.ID, "mode": task.Mode})
}

/* ═══════════════════ 任务查询与历史 ═══════════════════ */

// UpgradeTask GET /api/upgrade/tasks/{taskId}。
func (s *Server) UpgradeTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "taskId")
	t := s.Tasks.Get(id)
	if t == nil {
		fail(w, http.StatusNotFound, "任务不存在")
		return
	}
	ok(w, t)
}

// recordUpgradeRecord 任务终态落库（M2-8：升级台账 operator/durationMs）。
// 由 taskManager.onFinish 在 worker 收尾时回调；落库失败仅告警，不影响任务状态。
func (s *Server) recordUpgradeRecord(t *upgradeTask) {
	if s.DB == nil || t == nil {
		return
	}
	status := "failed"
	if t.Phase == PhaseSuccess {
		status = "success"
	}
	detail := t.Error
	if detail == "" {
		detail = t.Detail
	}
	if status == "success" {
		if t.Mode == "install" {
			detail = fmt.Sprintf("首次安装：%s %s（source=%s）", t.Name, t.ToVer, t.Source)
		} else {
			detail = fmt.Sprintf("部署完成：%s %s → %s（source=%s）", t.Name, t.FromVer, t.ToVer, t.Source)
		}
	}
	rec := store.UpgradeRecord{
		TaskID: t.ID, ServiceName: t.Name, Mode: t.Mode, Source: t.Source,
		FromVersion: t.FromVer, ToVersion: t.ToVer,
		Status:   status,
		Operator: "user",
		// 耗时口径：任务受理（入队）到终态。排队等待也计入——对使用者而言
		// 「点了升级到看到结果」的等待就是这段时间；串行队列下排队通常极短。
		DurationMs: time.Since(t.CreatedAt).Milliseconds(),
		Detail:     detail,
		FinishedAt: time.Now(),
	}
	var svc store.Service
	if err := s.DB.First(&svc, "name = ?", t.Name).Error; err == nil {
		rec.ServiceID = svc.ID
	}
	if err := s.DB.Create(&rec).Error; err != nil {
		logx.Module("api").Warn("升级台账落库失败", "task", t.ID, "svc", t.Name, "err", err)
	}
}

// UpgradeRecords GET /api/upgrade/records：升级历史（M2-8 结构化台账）。
//
// 响应保持旧 EventLog 口径的基础字段（id/level/serviceId/message/at）向前兼容，
// 另追加结构化字段（taskId/name/mode/source/fromVersion/toVersion/status/
// operator/durationMs/detail）。台账表为空时回落 EventLog(type=upgrade) 旧口径，
// 保留升级历史连续性。
func (s *Server) UpgradeRecords(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	var rows []store.UpgradeRecord
	s.DB.Order("id desc").Limit(limit).Find(&rows)

	type recRow map[string]any
	out := make([]recRow, 0, len(rows))
	for _, row := range rows {
		level := "info"
		if row.Status != "success" {
			level = "error"
		}
		msg := row.Detail
		if msg == "" {
			msg = fmt.Sprintf("%s %s（%s → %s）", row.ServiceName, row.Mode, row.FromVersion, row.ToVersion)
		}
		at := row.FinishedAt
		if at.IsZero() {
			at = row.CreatedAt
		}
		out = append(out, recRow{
			// 旧字段（前端兼容）
			"id": row.ID, "level": level, "serviceId": row.ServiceID,
			"message": msg, "at": at.UnixMilli(),
			// 结构化字段（M2-8）
			"taskId": row.TaskID, "name": row.ServiceName,
			"mode": row.Mode, "source": row.Source,
			"fromVersion": row.FromVersion, "toVersion": row.ToVersion,
			"status": row.Status, "operator": row.Operator,
			"durationMs": row.DurationMs, "detail": row.Detail,
		})
	}

	// 台账表为空：回落 EventLog 旧口径（首版台账启用前的历史记录）。
	if len(out) == 0 {
		var evs []store.EventLog
		s.DB.Where("type = ?", "upgrade").Order("id desc").Limit(limit).Find(&evs)
		for _, row := range evs {
			out = append(out, recRow{
				"id": row.ID, "level": row.Level, "serviceId": row.ServiceID,
				"message": row.Message, "at": row.CreatedAt.UnixMilli(),
			})
		}
	}
	ok(w, out)
}

func modeOf(hasService bool) string {
	if hasService {
		return "upgrade"
	}
	return "install"
}

func svcVersionOr(has bool, def *store.Service) string {
	if has {
		return def.Version
	}
	return ""
}
