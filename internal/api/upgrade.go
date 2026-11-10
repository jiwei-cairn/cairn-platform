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

	"github.com/jiwei-cairn/cairn-platform/internal/config"
	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/pack"
	"github.com/jiwei-cairn/cairn-platform/internal/store"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	goruntime "runtime"
)

/* ═══════════════════ 任务管理器（全局串行队列） ═══════════════════ */

// 升级任务的阶段（phase）。
const (
	PhaseQueued     = "queued"
	PhaseVerifying  = "verifying"  // 包校验
	PhaseDownloading = "downloading" // OTA 下载
	PhasePrecheck   = "precheck"   // 预检
	PhaseStopping   = "stopping"   // 停服等待退出
	PhaseSwapping   = "swapping"   // 换文件
	PhaseStarting   = "starting"   // 发起启动
	PhaseSuccess    = "success"
	PhaseFailed     = "failed"
)

// taskTimeout 单任务超时（卡死防护，decisions.md §10）。
const taskTimeout = 10 * time.Minute

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

	run func(t *upgradeTask) error // 队列 worker 调用
}

// taskManager 全局串行任务管理器。
type taskManager struct {
	mu    sync.Mutex
	tasks map[string]*upgradeTask
	ch    chan *upgradeTask
	wg    sync.WaitGroup
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
func (m *taskManager) worker() {
	defer m.wg.Done()
	for t := range m.ch {
		ctx, cancel := context.WithTimeout(context.Background(), taskTimeout)
		done := make(chan error, 1)
		go func() { done <- t.run(t) }()
		var err error
		select {
		case err = <-done:
		case <-ctx.Done():
			err = fmt.Errorf("任务超时（%s）被中止", taskTimeout)
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
	}
}

// Submit 入队一个任务，返回任务快照。
func (m *taskManager) Submit(mode, source, name, from, to string, run func(t *upgradeTask) error) *upgradeTask {
	t := &upgradeTask{
		ID: "up-" + uuid.NewString()[:8], Mode: mode, Source: source,
		Name: name, FromVer: from, ToVer: to,
		Phase: PhaseQueued, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		run: run,
	}
	m.mu.Lock()
	m.tasks[t.ID] = t
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

// Get 查询任务（返回快照，避免外部拿到内部指针）。
func (m *taskManager) Get(id string) *upgradeTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tasks[id]; ok {
		return snapshotTask(t)
	}
	return nil
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
		Enabled  bool            `json:"enabled"`  // OTA 是否已配置服务器
		Offline  bool            `json:"offline"`  // 已配置但拉取失败
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
		idx = pack.EmbeddedServicesIndex() // 仅展示（I2/I3）
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
		func(t *upgradeTask) error {
			return srv.runOtaTask(t, entry, base)
		})
	ok(w, map[string]any{"taskId": task.ID, "mode": task.Mode})
}

// runOtaTask OTA 任务：下载 → 校验 → 部署流水线。
func (s *Server) runOtaTask(t *upgradeTask, entry *pack.IndexEntry, base string) error {
	s.Tasks.updatePhase(t, PhaseDownloading, entry.URL)
	pkgPath, err := s.otaDownload(entry, base)
	if err != nil {
		return fmt.Errorf("下载失败: %w", err)
	}
	return s.runDeployPipeline(t, pkgPath, "ota")
}

// otaDownload 下载包到 data/upgrade-download/ 并校验整包 sha256（S4）。
func (s *Server) otaDownload(entry *pack.IndexEntry, base string) (string, error) {
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
	resp, err := client.Get(u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if entry.SizeBytes > 0 && resp.ContentLength > 0 && resp.ContentLength != entry.SizeBytes {
		return "", fmt.Errorf("包大小与清单不符（清单 %d，响应 %d）", entry.SizeBytes, resp.ContentLength)
	}
	dir := config.DataDir("upgrade-download")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, filepath.Base(strings.ReplaceAll(u, "\\", "/")))
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hasher), resp.Body)
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return "", closeErr
	}
	if entry.SizeBytes > 0 && n != entry.SizeBytes {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("下载不完整（%d/%d）", n, entry.SizeBytes)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != entry.Sha256 {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("整包 sha256 不符（清单 %s…，实际 %s…）", entry.Sha256[:8], got[:8])
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return dst, nil
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
	if hasSvc && def.Version == version {
		// 同版本重装允许（幂等，D3 语义是"不一致即可升"，同版本需要显式确认）。
		_ = os.RemoveAll(stageDir)
		fail(w, http.StatusConflict, fmt.Sprintf("当前已是 %s；安装相同版本请先卸载或使用新版本号", version))
		return
	}

	srv := s
	task := s.Tasks.Submit(modeOf(hasSvc), "local", name, svcVersionOr(hasSvc, &def), version,
		func(t *upgradeTask) error {
			err := srv.runDeployPipeline(t, pkgPath, "local")
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

// UpgradeRecords GET /api/upgrade/records：升级历史（EventLog type=upgrade）。
func (s *Server) UpgradeRecords(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	var rows []store.EventLog
	s.DB.Where("type = ?", "upgrade").Order("id desc").Limit(limit).Find(&rows)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"id": row.ID, "level": row.Level, "serviceId": row.ServiceID,
			"message": row.Message, "at": row.CreatedAt.UnixMilli(),
		})
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
