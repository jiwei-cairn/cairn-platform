// manifest.go — 安装制核心：服务安装清单（manifest.json）解析、安装预检、
// 版本目录落盘与安装入口。
//
// 产品边界（安装制定稿）：平台只管理由平台安装的服务。所有微服务（自研与
// 开源）一律通过「下载版本 + 安装」进入平台，安装时携带 manifest 说明文档
// 与运行参数；不再支持纳管既有 systemd 单元（adopt 已退役）。
//
// 落盘布局（运行目录下服务仓库，os/arch 按本机平台自动展开；与
// ServiceVersion.retainedVersions / 回滚机制对齐）：
//
//	services/{os}/{arch}/{name}/{version}/bin/{binary}          ← 主程序
//	services/{os}/{arch}/{name}/{version}/dependencies/{dll…}   ← 依赖文件
//	services/{os}/{arch}/{name}/{version}/config/{conf…}        ← 初始配置
//	services/{os}/{arch}/{name}/{version}/manifest.json         ← 清单原样留存
//	services/{os}/{arch}/{name}/current → {version}             ← 版本切换软链（Linux）
//	services/{os}/{arch}/{name}/current.txt                     ← Windows 人读指针
//
// 启动参数可用相对路径引用（workDir 默认 = 版本目录）：
//	-c config/app.conf          ← 配置文件
//	-L dependencies/libxxx.so   ← 动态库
//
// 升级/回滚 = 落新版本目录后原子切换 current（rename(2) 语义）并更新
// ServiceVersion.Active 与定义的 exePath。exePath 以 DB 为权威（完整展开路径），
// fork 认领的 resolvedExe 前缀匹配不受影响。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	goruntime "runtime"

	"github.com/jiwei-cairn/cairn-platform/internal/config"
	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/store"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
)

// installRoot 服务仓库根：运行目录下 services/{os}/{arch}（os/arch 按本机
// 运行平台自动展开——设备上只见本机平台子树；同一结构兼作 U 盘服务仓库/
// 市场下载缓存形态，与 GOOS/GOARCH 工具链命名直接对应）。
func (s *Server) installRoot() string {
	return config.ServicesDir(goruntime.GOOS, goruntime.GOARCH)
}

// switchCurrentLink 版本切换软链（仅 Linux：tmp symlink + rename 原子切换）。
// Windows 无符号链接特权：不建链，DB 的 ServiceVersion.Active 为权威，
// exePath 指向具体版本目录，fork 拉起天然正确；current.txt 仅作人读指针。
func (s *Server) switchCurrentLink(name, version string) {
	if goruntime.GOOS == "windows" {
		_ = os.WriteFile(filepath.Join(s.installRoot(), name, "current.txt"), []byte(version+"\n"), 0o644)
		return
	}
	svcDir := filepath.Join(s.installRoot(), name)
	tmp := filepath.Join(svcDir, ".current.tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(version, tmp); err != nil {
		logx.Module("api").Warn("创建版本软链失败", "svc", name, "err", err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(svcDir, "current")); err != nil {
		logx.Module("api").Warn("切换 current 软链失败", "svc", name, "err", err)
		_ = os.Remove(tmp)
	}
}

// ServiceManifest 是安装包的说明文档（service manifest）。
// 字段与市场目录条目同构：一份 schema 两处消费（打包随包分发 / 市场镜像）。
type ServiceManifest struct {
	Name          string            `json:"name"`          // 唯一标识（目录名）
	DisplayName   string            `json:"displayName"`   // 展示名
	Version       string            `json:"version"`       // 安装版本
	ServiceType   string            `json:"serviceType"`   // native | opensource
	Author        string            `json:"author"`
	Description   string            `json:"description"`
	Tags          []string          `json:"tags"`
	Deps          []string          `json:"deps"`    // 依赖服务名（安装时解析为 id）
	Binary        string            `json:"binary"`  // 包内相对路径，缺省 bin/{name}
	Args          []string          `json:"args"`
	Env           map[string]string `json:"env"`
	WorkDir       string            `json:"workDir"`
	RestartPolicy json.RawMessage   `json:"restartPolicy"`
	HealthCheck   json.RawMessage   `json:"healthCheck"`
	ConfigFile    json.RawMessage   `json:"configFile"`
	LogPolicy     json.RawMessage   `json:"logPolicy"`
	WebUrl        string            `json:"webUrl"`
	Ports         []int             `json:"ports"` // 冲突预检
	MinDiskMB     int               `json:"minDiskMB"`
	Os            string            `json:"os"`   // 目标平台 linux|windows（空=不限；预检与本机比对）
	Arch          string            `json:"arch"` // 目标架构 amd64|arm|arm64（空=不限；GOARCH 命名）
	ExternalRef   bool              `json:"externalRef,omitempty"` // 外部引用模式：产物不复制入仓，exePath 指向外部路径（调试用）
	ExtraFiles    []string          `json:"extraFiles,omitempty"` // 依赖文件清单（dll/so 等，与主程序同目录落盘）
}

// installOverrides 用户在 manifest 默认之上可覆盖的运行参数（安装表单）。
type installOverrides struct {
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	Enabled   *bool             `json:"enabled"`
	AutoStart bool              `json:"autoStart"`
	Priority  int               `json:"priority"`
	// LocalBinary 本地已下载的包文件（可选）：安装时复制为版本目录内产物
	//（手动创建的"本地文件入仓"模式同用此字段）。
	LocalBinary string `json:"localBinary"`
	// ExternalPath 外部引用路径（可选，调试用）：产物不复制入仓，
	// exePath 直接指向该路径；manifest 标记 externalRef 留档。
	ExternalPath string `json:"externalPath"`
	// ExtraBinaries 依赖文件暂存路径（dll/so/静态库等）：复制到与主程序同目录。
	ExtraBinaries []string `json:"extraBinaries"`
	// InitialConfig 初始配置文件暂存路径（可选）：复制到版本目录 conf/，
	// 并自动填 configFile.path；启动参数可用相对路径 conf/{name} 引用。
	InitialConfig string `json:"initialConfig"`
}

// installRequest POST /api/services/install 请求体。
type installRequest struct {
	Manifest   ServiceManifest  `json:"manifest"`
	Overrides  installOverrides `json:"overrides"`
	PrecheckOnly bool           `json:"precheckOnly"`
	// Source 来源信封：market（市场/包安装）| manual（平台表单创建）。
	// 台账与导出说明卡时呈现来源。
	Source string `json:"source"`
}

// manifestNameRE 服务名同时充当安装目录名，收紧为安全的路径片段。
var manifestNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// manifestVersionRE 版本号同样充当安装目录名（services/{os}/{arch}/{name}/{version}），
// 必须与 name 同级收紧：只校验非空会让 version="../../evil" 借 filepath.Join
// 的目录回溯写到服务仓库外（实测可穿透 installRoot）。允许语义化版本与日期串。
var manifestVersionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// installIssue 预检结果条目（前端展示为清单）。
type installIssue struct {
	Level string `json:"level"` // error | warn
	Item  string `json:"item"`
	Msg   string `json:"msg"`
}

// manifestSummary 从安装清单取卡片展示名（摘要）。
//
// name 是安装目录名（预检收紧为小写 ASCII 安全片段），displayName 才是
// 面向人的展示名（可中文）。历史行为把二者都当目录名用，导致中文展示名
// 无处安放；此处按 displayName → description → name 取 Summary 落库。
// description 常为整句介绍，按 40 字符截断（与表单 summary 的校验一致）。
func manifestSummary(m ServiceManifest) string {
	if s := strings.TrimSpace(m.DisplayName); s != "" {
		return truncateRunes(s, 40)
	}
	if s := strings.TrimSpace(m.Description); s != "" {
		return truncateRunes(s, 40)
	}
	return truncateRunes(strings.TrimSpace(m.Name), 40)
}

// truncateRunes 按字符（非字节）截断，避免把多字节汉字切半。
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max])
}

// validateManifest 清单字段校验（error 级问题阻止安装）。
// 注意：只做校验，不做默认值赋值（避免 precheck 值拷贝吞掉副作用）。
func (s *Server) validateManifest(m *ServiceManifest, ov installOverrides) []installIssue {
	var issues []installIssue
	if m.Name == "" {
		issues = append(issues, installIssue{"error", "name", "服务名必填"})
	} else if !manifestNameRE.MatchString(m.Name) {
		issues = append(issues, installIssue{"error", "name", "服务名仅允许小写字母/数字/._-（最长 64）"})
	}
	if m.Version == "" {
		issues = append(issues, installIssue{"error", "version", "版本必填"})
	} else if !manifestVersionRE.MatchString(m.Version) || strings.Contains(m.Version, "..") {
		// 双重防护：白名单已排除 "/" 与 "\\"，Contains 兜住 ".." 语义。
		issues = append(issues, installIssue{"error", "version", "版本号仅允许字母/数字/._+-，且不得含 ..（最长 64）"})
	}
	if m.ServiceType == "" {
		issues = append(issues, installIssue{"error", "serviceType", "serviceType 必填（native | opensource）"})
	} else if m.ServiceType != "native" && m.ServiceType != "opensource" {
		issues = append(issues, installIssue{"error", "serviceType", "serviceType 只支持 native / opensource"})
	}
	if m.Binary == "" {
		issues = append(issues, installIssue{"warn", "binary", "未指定包内路径，将按 bin/{name} 约定落盘"})
	}
	if strings.Contains(m.Binary, "..") || filepath.IsAbs(m.Binary) {
		issues = append(issues, installIssue{"error", "binary", "binary 必须是包内相对路径"})
	}
	// 客户端回传的暂存路径必须落在服务端上传暂存目录内：否则可把
	// data/cairn.db、系统任意文件复制进服务仓库（V1 无认证，危害放大）。
	upBase := filepath.Clean(config.UploadsDir())
	for _, f := range append([]string{ov.LocalBinary, ov.InitialConfig}, ov.ExtraBinaries...) {
		if strings.TrimSpace(f) == "" {
			continue
		}
		abs, err := filepath.Abs(f)
		if err != nil || !pathWithin(upBase, filepath.Clean(abs)) {
			issues = append(issues, installIssue{"error", "files", "上传文件路径不在暂存目录内: " + f})
		}
	}
	return issues
}

// precheck 安装预检：名称冲突 / 依赖解析 / 端口占用 / 磁盘余量。
// error 级问题返回 false（不可安装），warn 仅提示。
func (s *Server) precheck(m ServiceManifest, ov installOverrides) ([]installIssue, bool) {
	issues := s.validateManifest(&m, ov)
	installable := true
	for _, it := range issues {
		if it.Level == "error" {
			installable = false
		}
	}
	// 1) 名称冲突：同名服务已存在。
	var conflict store.Service
	if err := s.DB.First(&conflict, "name = ?", m.Name).Error; err == nil {
		issues = append(issues, installIssue{"error", "name", "服务名已被「" + conflict.Name + "」使用（先卸载或换名）"})
		installable = false
	}
	// 2) 依赖解析：按名找服务，缺失提示先安装。
	for _, dep := range m.Deps {
		var n int64
		s.DB.Model(&store.Service{}).Where("name = ?", dep).Count(&n)
		if n == 0 {
			issues = append(issues, installIssue{"error", "deps", "依赖「" + dep + "」未安装，请先安装依赖"})
			installable = false
		}
	}
	// 3) 端口占用（best-effort：试绑定成功即视为空闲）。
	for _, p := range m.Ports {
		if p <= 0 || p > 65535 {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err != nil {
			issues = append(issues, installIssue{"error", "ports", fmt.Sprintf("端口 %d 已被占用（请先停用占用它的系统服务）", p)})
			installable = false
			continue
		}
		_ = ln.Close()
	}
	// 4) 磁盘余量（minDiskMB > 0 时检查安装根所在卷）。
	if m.MinDiskMB > 0 {
		root := s.installRoot()
		_ = os.MkdirAll(root, 0o755)
		if st, err := os.Stat(root); err == nil && st.IsDir() {
			// du 余量检查依赖 syscall.Statfs（平台差异大），此处给 warn 提示。
			issues = append(issues, installIssue{"warn", "disk", fmt.Sprintf("请确认安装卷剩余空间 ≥ %d MB", m.MinDiskMB)})
		}
	}
	// 5) 平台校验：包目标平台必须与安装目录树（本机 os/arch）一致——
	// 防误装错架构包（目录树按本机展开，错架构产物无法运行）。
	if m.Os != "" && m.Os != goruntime.GOOS {
		issues = append(issues, installIssue{"error", "os",
			fmt.Sprintf("包目标平台 %s 与本机 %s 不符", m.Os, goruntime.GOOS)})
		installable = false
	}
	if m.Arch != "" && m.Arch != goruntime.GOARCH {
		issues = append(issues, installIssue{"error", "arch",
			fmt.Sprintf("包目标架构 %s 与本机 %s 不符", m.Arch, goruntime.GOARCH)})
		installable = false
	}
	// 6) 暂存文件架构校验：主程序与依赖文件按文件头（ELF/PE）判定目标平台，
	//    与本机不符即拒装（Windows 运行只收 .exe；Linux 按 amd64/arm/arm64）。
	for _, f := range append([]string{ov.LocalBinary}, ov.ExtraBinaries...) {
		if it := checkArchMatch("files", f); it != nil {
			issues = append(issues, *it)
			installable = false
		}
	}
	// 7) 同名 systemd 单元残留提醒（adopt 退役后的冲突防御提示）。
	if m.Name != "" && goruntime.GOOS == "linux" {
		unitPath := filepath.Join(string(filepath.Separator), "etc", "systemd", "system", m.Name+".service")
		if _, err := os.Stat(unitPath); err == nil {
			issues = append(issues, installIssue{"warn", "systemd", "检测到同名 systemd 单元 " + m.Name + ".service，建议先 disable --now 避免冲突"})
		}
	}
	return issues, installable
}

// InstallPrecheck POST /api/services/install：仅预检，返回问题清单与可安装结论。
func (s *Server) InstallPrecheck(w http.ResponseWriter, r *http.Request) {
	var req installRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}
	issues, installable := s.precheck(req.Manifest, req.Overrides)
	ok(w, map[string]any{"issues": issues, "installable": installable})
}

// InstallService POST /api/services/install：预检 → 落盘 → 建定义与版本台账。
func (s *Server) InstallService(w http.ResponseWriter, r *http.Request) {
	var req installRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}
	m := req.Manifest
	// 默认值就地补齐（落盘与建档都以补齐后的为准）。
	if strings.TrimSpace(m.Binary) == "" {
		m.Binary = filepath.Join("bin", m.Name)
		// Windows 下 exec.Command 依赖扩展名（PATHEXT 只补 .exe/.bat/…，
		// 而 manifest 里的裸名无扩展名），不补则永远找不到可执行文件。
		if goruntime.GOOS == "windows" && filepath.Ext(m.Binary) == "" {
			m.Binary += ".exe"
		}
	}
	// 本地文件入仓时保留原始扩展名（Windows .exe 等），防 exec 不识别。
	if src := strings.TrimSpace(req.Overrides.LocalBinary); src != "" && filepath.Ext(src) != "" {
		if filepath.Ext(m.Binary) == "" {
			m.Binary += filepath.Ext(src)
		}
	}
	issues, installable := s.precheck(m, req.Overrides)
	if req.PrecheckOnly {
		ok(w, map[string]any{"issues": issues, "installable": installable})
		return
	}
	if !installable {
		writeJSON(w, http.StatusConflict, map[string]any{"code": http.StatusConflict,
			"msg": "安装预检未通过", "issues": issues})
		return
	}

	// ── 落盘：{root}/{name}/{version}/ + manifest.json 留存（统一说明卡）──
	source := strings.TrimSpace(req.Source)
	if source == "" {
		source = "market"
	}
	external := strings.TrimSpace(req.Overrides.ExternalPath)
	if external != "" {
		m.ExternalRef = true // 说明卡标记：产物不入仓，引用外部路径
	}
	dir := filepath.Join(s.installRoot(), m.Name, m.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, http.StatusInternalServerError, "创建安装目录失败: "+err.Error())
		return
	}
	manifestRaw, _ := json.Marshal(struct {
		ServiceManifest
		Source string `json:"source"`
	}{m, source})
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifestRaw, 0o644); err != nil {
		s.rollbackInstallDir(dir)
		fail(w, http.StatusInternalServerError, "写入 manifest.json 失败: "+err.Error())
		return
	}
	binPath := filepath.Join(dir, m.Binary)
	if external == "" {
		// 产物入仓：确保 bin 子目录存在（复制/占位写入前）。
		if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
			s.rollbackInstallDir(dir)
			fail(w, http.StatusInternalServerError, "创建产物目录失败: "+err.Error())
			return
		}
	}
	switch {
	case external != "":
		// 外部引用模式：产物不复制，exePath 指向外部路径（调试场景）。
		binPath = external
	case strings.TrimSpace(req.Overrides.LocalBinary) != "":
		if err := copyFile(req.Overrides.LocalBinary, binPath, 0o755); err != nil {
			s.rollbackInstallDir(dir)
			fail(w, http.StatusBadRequest, "复制本地包文件失败: "+err.Error())
			return
		}
	default:
		// 无本地包产物：安装包应由升级管线（M2）提供。此处不再写入占位 stub
		// 冒充二进制——那会让安装「看似成功、启动必然失败」，现场极难排查。
		// 显式拒绝，让调用方明确知道缺产物。
		if _, err := os.Stat(binPath); err != nil {
			s.rollbackInstallDir(dir)
			fail(w, http.StatusBadRequest,
				"缺少安装产物：未提供 localBinary，且包内未包含 "+m.Binary+"（升级管线将在 M2 提供）")
			return
		}
	}

	// ── 依赖文件：复制到版本目录 dependencies/（与 bin/ 平级，启动参数可用
	//    相对路径 dependencies/xxx.dll 引用；Windows DLL 如需自动加载，
	//    可在环境变量中追加 PATH=dependencies）──
	depDir := filepath.Join(dir, "dependencies")
	for _, extra := range req.Overrides.ExtraBinaries {
		if strings.TrimSpace(extra) == "" {
			continue
		}
		if err := os.MkdirAll(depDir, 0o755); err != nil {
			s.rollbackInstallDir(dir)
			fail(w, http.StatusInternalServerError, "创建依赖目录失败: "+err.Error())
			return
		}
		if err := copyFile(extra, filepath.Join(depDir, filepath.Base(extra)), 0o755); err != nil {
			s.rollbackInstallDir(dir)
			fail(w, http.StatusBadRequest, "复制依赖文件失败: "+err.Error())
			return
		}
		m.ExtraFiles = append(m.ExtraFiles, filepath.Base(extra))
	}
	// ── 初始配置文件：复制到版本目录 config/ 并自动填 configFile.path ──
	cfgPath := ""
	if cfg := strings.TrimSpace(req.Overrides.InitialConfig); cfg != "" {
		confDir := filepath.Join(dir, "config")
		if err := os.MkdirAll(confDir, 0o755); err != nil {
			s.rollbackInstallDir(dir)
			fail(w, http.StatusInternalServerError, "创建配置目录失败: "+err.Error())
			return
		}
		if err := copyFile(cfg, filepath.Join(confDir, filepath.Base(cfg)), 0o644); err != nil {
			s.rollbackInstallDir(dir)
			fail(w, http.StatusBadRequest, "复制配置文件失败: "+err.Error())
			return
		}
		cfgPath = filepath.Join(confDir, filepath.Base(cfg))
	}
	// 工作目录缺省 = 版本目录（安装产物与配置的相对引用基准）。
	if strings.TrimSpace(m.WorkDir) == "" {
		m.WorkDir = dir
	}

	// ── 依赖名 → id ──
	dependsOn := make([]int, 0, len(m.Deps))
	for _, dep := range m.Deps {
		var d store.Service
		if err := s.DB.First(&d, "name = ?", dep).Error; err == nil {
			dependsOn = append(dependsOn, d.ID)
		}
	}

	// ── 依赖环检测（写路径防御）──
	// 安装是前端唯一的新建入口，必须在此拦截：仅靠 SaveService 的检测
	// 管不住 manifest.Deps 互相依赖形成的环（会把 DAG 拓扑序打乱）。
	if len(dependsOn) > 0 {
		var all []store.Service
		s.DB.Order("id asc").Find(&all)
		if cycle := detectDependsCycle(all, 0, dependsOn); cycle != "" {
			s.rollbackInstallDir(dir)
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"code": http.StatusBadRequest,
				"msg":  "依赖形成环：" + cycle,
				"issues": []installIssue{
					{"error", "deps", "依赖成环：" + cycle},
				},
			})
			return
		}
	}

	// ── Service 定义（统一 managed：无 unitName，全由平台定义） ──
	enabled := req.Overrides.Enabled == nil || *req.Overrides.Enabled
	args := m.Args
	if req.Overrides.Args != nil {
		args = req.Overrides.Args
	}
	env := m.Env
	if req.Overrides.Env != nil {
		env = req.Overrides.Env
	}
	def := store.Service{
		Name: m.Name, Summary: manifestSummary(m), ServiceType: m.ServiceType, WebUrl: strings.TrimSpace(m.WebUrl),
		ExePath: binPath, WorkDir: m.WorkDir,
		Args: mustJSON(nonNil(args)), Env: mustJSON(env), DependsOn: mustJSON(dependsOn),
		Enabled: enabled, AutoStart: req.Overrides.AutoStart,
		Priority: req.Overrides.Priority,
	}
	def.RestartPolicy, def.HealthCheck, def.ConfigFile, def.LogPolicy = defaultPolicyBlobs()
	if cfgPath != "" {
		// 上传的初始配置自动接管 configFile.path（M3 编辑/校验/生效链路的锚点）。
		def.ConfigFile = mustJSON(map[string]any{"path": cfgPath, "validateCmd": "", "applyMode": "restart", "applyCmd": "", "maxHistory": 10})
	}
	if len(m.RestartPolicy) > 0 && string(m.RestartPolicy) != "null" {
		def.RestartPolicy = datatypesJSON(m.RestartPolicy)
	}
	if len(m.HealthCheck) > 0 && string(m.HealthCheck) != "null" {
		def.HealthCheck = datatypesJSON(m.HealthCheck)
	}
	if len(m.ConfigFile) > 0 && string(m.ConfigFile) != "null" {
		def.ConfigFile = datatypesJSON(m.ConfigFile)
	}
	if len(m.LogPolicy) > 0 && string(m.LogPolicy) != "null" {
		def.LogPolicy = datatypesJSON(m.LogPolicy)
	}
	// ── DB 落库（事务）：服务定义 + 版本台账同生共死 ──
	// 二者其一失败都会留下不可用状态（有目录无台账 / 有定义无产物），
	// 故置于同一事务，失败时回滚已有目录，避免重装被名称冲突预检挡住。
	sv := store.ServiceVersion{
		Version: m.Version, DirPath: dir,
		SizeBytes: int64(len(manifestRaw)), Manifest: datatypesJSON(manifestRaw),
		Active: true,
	}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&def).Error; err != nil {
			return fmt.Errorf("保存服务定义失败: %w", err)
		}
		sv.ServiceID = def.ID
		if err := tx.Create(&sv).Error; err != nil {
			return fmt.Errorf("写入版本台账失败: %w", err)
		}
		return nil
	}); err != nil {
		s.rollbackInstallDir(dir)
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}

	if s.Defs != nil {
		s.Defs.OnDefsSaved(def)
	}
	s.switchCurrentLink(m.Name, m.Version)
	// 清理本次安装引用的上传暂存目录（防磁盘累积）。
	s.cleanUploads(req.Overrides)
	logx.Module("api").Info("安装服务", "id", def.ID, "name", def.Name, "version", m.Version, "dir", dir, "source", source)
	ok(w, map[string]any{"id": def.ID, "dir": dir, "issues": issues})
}

// cleanUploads 安装完成后清理引用的上传暂存目录（尽力而为，失败仅日志）。
//
// 前缀基准必须用 config.UploadsDir() 实际值：生产形态下数据根是绝对路径
// （--root / CAIRN_ROOT / exec 目录），原先拿相对 "data/uploads" 比对恒为 false，
// 清理从未生效（仅 go run 的 CWD 形态碰巧成立）。
func (s *Server) cleanUploads(ov installOverrides) {
	base := filepath.Clean(config.UploadsDir())
	dirs := map[string]bool{}
	for _, p := range append([]string{ov.LocalBinary, ov.InitialConfig}, ov.ExtraBinaries...) {
		if p == "" {
			continue
		}
		d := filepath.Clean(filepath.Dir(p))
		if pathWithin(base, d) {
			dirs[d] = true
		}
	}
	for d := range dirs {
		if err := os.RemoveAll(d); err != nil {
			logx.Module("api").Warn("清理上传暂存失败", "dir", d, "err", err)
		}
	}
}

// pathWithin 判断 child 是否位于 parent 之内（含相等）。两者均应已 Clean。
// 用 filepath.Rel 而非字符串前缀，避免 "/a/bc" 被误判为 "/a/b" 的子路径。
func pathWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// ExportManifest GET /api/services/{id}/manifest：导出服务说明卡（manifest）。
// 优先返回版本台账留存的安装时点说明卡（不可变快照）；无台账（历史数据）时
// 从当前 Service 定义合成。用途：提交开放平台上架 / 备份 / 跨设备迁移。
func (s *Server) ExportManifest(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		fail(w, http.StatusBadRequest, "无效的 ID")
		return
	}
	var def store.Service
	if err := s.DB.First(&def, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			fail(w, http.StatusNotFound, "服务不存在")
		} else {
			fail(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	// 优先：active 版本台账留存的说明卡（安装时点快照，与落盘文件一致）。
	var sv store.ServiceVersion
	if err := s.DB.First(&sv, "service_id = ? AND active = ?", id, true).Error; err == nil && len(sv.Manifest) > 0 {
		var out json.RawMessage
		if json.Unmarshal(sv.Manifest, &out) == nil {
			ok(w, map[string]any{"manifest": &out, "version": sv.Version, "origin": "installed"})
			return
		}
	}
	// 兜底：从当前定义合成（现场编辑过或历史数据——反映"当前态"）。
	m := synthesizeManifest(def)
	raw, _ := json.Marshal(struct {
		ServiceManifest
		Source string `json:"source"`
	}{m, "synthesized"})
	ok(w, map[string]any{"manifest": json.RawMessage(raw), "version": m.Version, "origin": "synthesized"})
}

// synthesizeManifest 从 Service 定义合成说明卡（导出兜底）。
func synthesizeManifest(def store.Service) ServiceManifest {
	var args []string
	_ = json.Unmarshal(def.Args, &args)
	env := map[string]string{}
	_ = json.Unmarshal(def.Env, &env)
	m := ServiceManifest{
		Name: def.Name, DisplayName: firstNonEmpty(def.Summary, def.Name), ServiceType: def.ServiceType,
		Version: "current", Args: args, Env: env, WorkDir: def.WorkDir,
		WebUrl: def.WebUrl,
		RestartPolicy: json.RawMessage(def.RestartPolicy),
		HealthCheck:   json.RawMessage(def.HealthCheck),
		ConfigFile:    json.RawMessage(def.ConfigFile),
		LogPolicy:     json.RawMessage(def.LogPolicy),
	}
	return m
}

// rollbackInstallDir 安装失败时尽力清理本次创建的版本目录，
// 避免「目录已落盘但 DB 无记录」的残留（会导致重装时名称冲突预检失败）。
func (s *Server) rollbackInstallDir(dir string) {
	if dir == "" {
		return
	}
	// 仅清理版本目录本身；不触碰 services 根，避免误删既有服务。
	if err := os.RemoveAll(dir); err != nil {
		logx.Module("api").Warn("回滚安装目录失败", "dir", dir, "err", err)
	}
}

// copyFile 复制文件并设定权限（本地包安装路径）。
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("不是普通文件: %s", src)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
