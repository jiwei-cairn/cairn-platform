// Package config 持有平台配置类型、默认值与校验。
//
// 平台无任何配置文件：完整配置以 JSON 快照持久化在 SQLite 的 Setting
// 单行表（沿用 ThingsModel 模式），缺失字段回落默认值。
// 字段命名全 camelCase，与前端设置页（settings.js 的 SET_* 映射）一一对应。
package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/jiwei-cairn/cairn-platform/internal/logx"
)

// App 平台配置根节点。
type App struct {
	OTA      OTA      `json:"ota"`
	Web      Web      `json:"web"`
	LogQuota LogQuota `json:"logQuota"`
	Collect  Collect  `json:"collect"`
}

/* ══════════════ 数据根（data / logs / services 的共同父目录） ══════════════

平台的全部运行数据此前都是「进程当前工作目录（CWD）相对」的相对路径
（data/cairn.db、logs/console.log、services/{os}/{arch}…）。这在 Linux
前台/systemd 部署下成立（单元显式设置 WorkingDirectory），但 Windows 服务的
工作目录由 SCM 决定、默认 C:\Windows\System32——数据会被静默写到系统目录，
或在非管理员服务账户下直接失败。

解析优先级（高 → 低）：
 1. InitRoot(root) 显式指定（--root 标志）
 2. CAIRN_ROOT 环境变量（无法传标志的服务注册形态）
 3. 可执行文件所在目录（部署形态正确锚点：console[.exe] 旁的 data/ logs/ services/）
 4. 当前工作目录（兜底：go run / go test 开发形态）

第 3 条对开发形态做了让步：go run / go test 的二进制落在系统临时目录，
若直接采用 exec 目录，开发期数据会散落到 %TEMP% 且每次重建换目录，故检出
「可执行文件位于临时目录」时回退 CWD，保持原开发体验。 */

// RootEnv 环境变量名：无法传命令行标志时（如 Windows 服务注册）指定数据根。
const RootEnv = "CAIRN_ROOT"

var (
	rootOnce     sync.Once
	rootResolved string

	rootMu       sync.Mutex
	rootOverride string
)

// InitRoot 显式指定数据根；空字符串表示沿用自动解析。
// 必须在任何 Root() 之前调用（main 中完成），否则不生效。
func InitRoot(root string) {
	rootMu.Lock()
	defer rootMu.Unlock()
	rootOverride = strings.TrimSpace(root)
	if rootOverride != "" {
		// 显式指定时立即定型，避免与并发的惰性解析竞争。
		rootOnce.Do(func() { rootResolved = rootOverride })
	}
}

// Root 返回数据根目录（进程内恒定，已定型）。
// 可能是相对路径（CWD 兜底分支）或绝对路径（exec 目录 / 显式指定分支）。
func Root() string {
	rootOnce.Do(func() {
		rootMu.Lock()
		o := rootOverride
		rootMu.Unlock()
		rootResolved = resolveRoot(o)
	})
	return rootResolved
}

func resolveRoot(override string) string {
	if override != "" {
		return override
	}
	if env := strings.TrimSpace(os.Getenv(RootEnv)); env != "" {
		return env
	}
	if dir, ok := execDir(); ok {
		return dir
	}
	// CWD 兜底：返回 "." 而非绝对化，保持与原相对路径行为一致。
	return "."
}

// execDir 返回可执行文件所在目录；开发形态（临时目录中的构建产物）返回 false。
func execDir() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	// 解析软链：Linux 下软链部署时取真实位置。
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	dir := filepath.Dir(exe)
	if underTempDir(dir) {
		return "", false
	}
	// go run 构建目录名特征（GOCACHE 被重定向到非临时目录时的兜底）。
	if strings.Contains(strings.ToLower(dir), "go-build") {
		return "", false
	}
	return dir, true
}

// underTempDir 判断 dir 是否位于系统临时目录之下。
func underTempDir(dir string) bool {
	tmp := os.TempDir()
	if tmp == "" {
		return false
	}
	rel, err := filepath.Rel(tmp, dir)
	if err != nil {
		return false // 跨卷等无法判定时保守视为「不在临时目录」。
	}
	// rel 不以 ".." 开头即为 tmp 的子目录。
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// DataDir 配置库与运行数据目录（root/data）。
func DataDir(parts ...string) string { return rootJoin("data", parts...) }

// LogsDir 平台日志目录（root/logs）。
func LogsDir(parts ...string) string { return rootJoin("logs", parts...) }

// UploadsDir 安装资源上传暂存目录（root/data/uploads）。
func UploadsDir(parts ...string) string { return rootJoin("data", append([]string{"uploads"}, parts...)...) }

// RuntimeDir 进程登记目录（root/data/runtime，fork 认领登记落盘处）。
func RuntimeDir(parts ...string) string { return rootJoin("data", append([]string{"runtime"}, parts...)...) }

// ServicesDir 服务仓库根（root/services/{os}/{arch}）——安装制统一落盘位置。
func ServicesDir(goos, goarch string, parts ...string) string {
	return rootJoin("services", append([]string{goos, goarch}, parts...)...)
}

// rootJoin 以 Root() 为前缀拼接各段（跳过空段）。
func rootJoin(head string, parts ...string) string {
	all := make([]string, 0, len(parts)+2)
	all = append(all, Root(), head)
	for _, p := range parts {
		if p != "" {
			all = append(all, p)
		}
	}
	return filepath.Join(all...)
}

// OTA 远程升级（Pull 模式，静态文件托管，decisions.md B1-B8）。
// 服务器目录规范见 docs/upgrade/ota-server.md。
type OTA struct {
	ServerURL  string `json:"serverUrl"`  // 服务器基址，如 http://192.168.1.10:8090；空 = 未启用 OTA（出厂默认）
	TimeoutSec int    `json:"timeoutSec"` // 拉清单/下载超时（5–120s）
}

// Web 平台 Web 服务。
type Web struct {
	Port              int `json:"port"`              // 监听端口（变更需重启平台）
	SessionTimeoutMin int `json:"sessionTimeoutMin"` // 会话超时（V1 无登录，预留）
}

// LogQuota 平台日志配额（lumberjack 轮转）。
type LogQuota struct {
	PlatformMaxMB      int    `json:"platformMaxMB"`
	PlatformMaxBackups int    `json:"platformMaxBackups"`
	RetentionDays      int    `json:"retentionDays"`
	BufferLines        int    `json:"bufferLines"` // 前端环形缓冲条数
	Level              string `json:"level"`       // debug|info|warn|error
}

// Collect 采集统计与版本保留策略。
type Collect struct {
	IntervalSec  int    `json:"intervalSec"`  // 指标采集周期（2–3600s；改动需重启平台）
	Downsample   string `json:"downsample"`   // none|two-level（M3 降采样落地前不生效）
	KeepVersions int    `json:"keepVersions"` // 本地保留版本数（默认 2；M2 回滚落地前不生效）
	DBQuotaMB    int    `json:"dbQuotaMB"`    // 指标库配额（M3 落地前不生效，仅持久化）
}

// Issue 是一条可定位到表单字段的配置问题。
// Field 使用 tftp.server 式路径，前端据 mapIssues 高亮对应输入框。
type Issue struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Default 返回内置默认配置（首启写入 Setting 表的快照）。
func Default() App {
	return App{
		OTA:      OTA{ServerURL: "", TimeoutSec: 30},
		Web:      Web{Port: 1995, SessionTimeoutMin: 30},
		LogQuota: LogQuota{PlatformMaxMB: 20, PlatformMaxBackups: 5, RetentionDays: 30, BufferLines: 2000, Level: "info"},
		Collect:  Collect{IntervalSec: 10, Downsample: "two-level", KeepVersions: 2, DBQuotaMB: 512},
	}
}

var (
	otaURLPattern = regexp.MustCompile(`^https?://[A-Za-z0-9.:-]+(:[0-9]{1,5})?(/[A-Za-z0-9._/-]*)?$`)
)

// Validate 逐字段校验，规则与前端 validateSettings 保持一致（后端二次校验），
// 一次返回全部问题，前端逐字段标注。
func Validate(app App) []Issue {
	var issues []Issue
	add := func(field, message string) { issues = append(issues, Issue{Field: field, Message: message}) }
	rangeIssue := func(field string, ok bool, message string) {
		if !ok {
			add(field, message)
		}
	}

	// OTA 服务端：空 = 未启用远程升级（出厂默认，避免硬编码内网地址）；
	// 一旦填写必须是 http(s):// 基址（不带尾斜杠，路径 /cairn/ 由平台拼接）。
	if u := strings.TrimSpace(app.OTA.ServerURL); u != "" {
		if !otaURLPattern.MatchString(u) {
			add("ota.serverUrl", "必须是 http:// 或 https:// 开头的基址（不含 /cairn 路径与尾斜杠）")
		}
	}
	rangeIssue("ota.timeoutSec", app.OTA.TimeoutSec >= 5 && app.OTA.TimeoutSec <= 120, "OTA 超时需在 5–120 秒")
	rangeIssue("web.port", app.Web.Port >= 1 && app.Web.Port <= 65535, "端口需在 1–65535 之间")
	rangeIssue("web.sessionTimeoutMin", app.Web.SessionTimeoutMin >= 5 && app.Web.SessionTimeoutMin <= 1440, "会话超时需在 5–1440 分钟")
	rangeIssue("logQuota.platformMaxMB", app.LogQuota.PlatformMaxMB >= 1 && app.LogQuota.PlatformMaxMB <= 512, "单文件上限需在 1–512 MB")
	rangeIssue("logQuota.platformMaxBackups", app.LogQuota.PlatformMaxBackups >= 1 && app.LogQuota.PlatformMaxBackups <= 20, "保留份数需在 1–20")
	rangeIssue("logQuota.retentionDays", app.LogQuota.RetentionDays >= 1 && app.LogQuota.RetentionDays <= 365, "保留天数需在 1–365 天")
	rangeIssue("logQuota.bufferLines", app.LogQuota.BufferLines >= 500 && app.LogQuota.BufferLines <= 5000, "前端缓冲需在 500–5000 行")
	switch app.LogQuota.Level {
	case "debug", "info", "warn", "error":
	default:
		add("logQuota.level", "日志级别需为 debug/info/warn/error")
	}
	rangeIssue("collect.intervalSec", app.Collect.IntervalSec >= 2 && app.Collect.IntervalSec <= 3600, "采集周期需在 2–3600 秒")
	rangeIssue("collect.keepVersions", app.Collect.KeepVersions >= 1 && app.Collect.KeepVersions <= 5, "版本保留数需在 1–5")
	rangeIssue("collect.dbQuotaMB", app.Collect.DBQuotaMB >= 64 && app.Collect.DBQuotaMB <= 16384, "数据库配额需在 64–16384 MB")
	switch app.Collect.Downsample {
	case "none", "two-level":
	default:
		add("collect.downsample", "降采样策略需为 none/two-level")
	}
	return issues
}

func parseOctet(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}

// LogOptions 把持久化设置转换为日志管理器参数（保存设置后热应用）。
func (app App) LogOptions() logx.Options {
	return logx.Options{
		Level:       app.LogQuota.Level,
		Console:     true,
		File:        LogsDir("cairn.log"),
		MaxSizeMB:   app.LogQuota.PlatformMaxMB,
		MaxBackups:  app.LogQuota.PlatformMaxBackups,
		MaxAgeDays:  app.LogQuota.RetentionDays,
		Compress:    true,
		DailyRotate: true,
		BufferSize:  app.LogQuota.BufferLines,
	}
}
