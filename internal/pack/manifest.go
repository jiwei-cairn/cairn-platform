// Package pack 实现 .cairnpkg 包格式：解析、校验、打包。
//
// 格式规范见 docs/upgrade/package-format.md（schema 1）。
// 核心约定：
//   - ZIP 容器，manifest.json 必须是第一个条目（支持流式先读清单）
//   - 语义化版本强制（MAJOR.MINOR.PATCH[-pre][+build]）
//   - files[] 逐文件 sha256/size/mode/role
//   - signature 字段预留（本轮恒为 null，非 null 包被拒绝）
package pack

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// SchemaVersion 当前包格式版本。
const SchemaVersion = 1

// 文件角色（升级时的处理策略依据，见 package-format.md §7）。
const (
	RoleBinary     = "binary"     // 主程序（必须与 entrypoint 一致）
	RoleDependency = "dependency" // 运行时依赖
	RoleConfig     = "config"     // 配置（升级时备份 .bak 后覆盖）
	RoleData       = "data"       // 初始数据（目标已存在则保留不覆盖）
)

var (
	// nameRE 服务名同时充当安装目录名，收紧为安全路径片段。
	nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	// semverRE 语义化版本（强制，D8）。
	semverRE = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)(-([0-9A-Za-z.-]+))?(\+([0-9A-Za-z.-]+))?$`)
	// hex64RE sha256 小写十六进制。
	hex64RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// entryPathRE 包内路径：正斜杠分隔，不得以 / 开头。
	entryPathRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/ -]*$`)
)

// FileEntry 清单中的单个文件登记。
type FileEntry struct {
	Path      string `json:"path"`                // 包内相对路径（/ 分隔）
	Sha256    string `json:"sha256"`              // 内容 SHA-256（小写 hex）
	SizeBytes int64  `json:"sizeBytes"`           // 字节数
	Mode      string `json:"mode"`                // Unix 权限八进制串（"0755"）
	Role      string `json:"role,omitempty"`      // binary|dependency|config|data
}

// Manifest .cairnpkg 的清单（包内第一个条目）。
type Manifest struct {
	// ═══ 格式元信息（必需）═══
	Schema         int    `json:"schema"`
	Name           string `json:"name"`
	Version        string `json:"version"`
	Os             string `json:"os"`             // linux | windows
	Arch           string `json:"arch"`           // amd64 | arm64 | arm
	Entrypoint     string `json:"entrypoint"`     // 主程序包内相对路径（显式，不推理）
	TotalSizeBytes int64  `json:"totalSizeBytes"` // 解压后总大小（防 zip bomb）

	// ═══ 文件清单（必需）═══
	Files []FileEntry `json:"files"`

	// ═══ 展示信息（可选）═══
	DisplayName  string   `json:"displayName,omitempty"`
	Author       string   `json:"author,omitempty"`
	Description  string   `json:"description,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	WebUrl       string   `json:"webUrl,omitempty"`
	ReleaseNotes string   `json:"releaseNotes,omitempty"`

	// ═══ 运行约束（可选，缺省用平台默认）═══
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Deps      []string          `json:"deps,omitempty"`
	Ports     []int             `json:"ports,omitempty"`
	MinDiskMB int               `json:"minDiskMB,omitempty"`

	RestartPolicy json.RawMessage `json:"restartPolicy,omitempty"`
	HealthCheck   json.RawMessage `json:"healthCheck,omitempty"`
	LogPolicy     json.RawMessage `json:"logPolicy,omitempty"`
	ConfigFile    json.RawMessage `json:"configFile,omitempty"`

	// ═══ 兼容性（可选）═══
	MinPlatformVersion string `json:"minPlatformVersion,omitempty"`

	// ═══ 签名（预留，本轮恒为 null）═══
	// 解析规则：缺失或 null → 跳过验签；非 null → 本轮平台明确拒绝
	//（避免未来启用签名后，旧平台静默接受未验签包）。
	Signature json.RawMessage `json:"signature"`
}

// ValidateManifestName 校验服务名（供部署层在打包前单独校验）。
func ValidateManifestName(name string) bool { return nameRE.MatchString(name) }

// Validate 校验清单的全部格式约束。
func (m *Manifest) Validate() error {
	if m.Schema != SchemaVersion {
		return fmt.Errorf("schema 版本 %d 不受支持（当前支持 %d）", m.Schema, SchemaVersion)
	}
	if !nameRE.MatchString(m.Name) {
		return fmt.Errorf("服务名 %q 不合法（仅小写字母/数字/._-，最长 64，字母或数字开头）", m.Name)
	}
	if _, err := ParseSemVer(m.Version); err != nil {
		return fmt.Errorf("版本号 %q 不是合法的语义化版本: %w", m.Version, err)
	}
	switch m.Os {
	case "linux", "windows":
	default:
		return fmt.Errorf("os 必须是 linux|windows，实际 %q", m.Os)
	}
	switch m.Arch {
	case "amd64", "arm64", "arm":
	default:
		return fmt.Errorf("arch 必须是 amd64|arm64|arm，实际 %q", m.Arch)
	}
	if err := validateEntryPath(m.Entrypoint); err != nil {
		return fmt.Errorf("entrypoint 不合法: %w", err)
	}
	if len(m.Files) == 0 {
		return fmt.Errorf("files 清单为空")
	}

	var total int64
	paths := make(map[string]bool, len(m.Files))
	hasEntry := false
	for i, f := range m.Files {
		if err := validateEntryPath(f.Path); err != nil {
			return fmt.Errorf("files[%d].path 不合法: %w", i, err)
		}
		if paths[f.Path] {
			return fmt.Errorf("files[%d].path %q 重复登记", i, f.Path)
		}
		paths[f.Path] = true
		if !hex64RE.MatchString(f.Sha256) {
			return fmt.Errorf("files[%d] (%s) sha256 不是 64 位小写十六进制", i, f.Path)
		}
		if f.SizeBytes < 0 {
			return fmt.Errorf("files[%d] (%s) sizeBytes 为负", i, f.Path)
		}
		if err := validateMode(f.Mode); err != nil {
			return fmt.Errorf("files[%d] (%s) mode 不合法: %w", i, f.Path, err)
		}
		switch f.Role {
		case "", RoleBinary, RoleDependency, RoleConfig, RoleData:
			// 空 role 视为 dependency 语义（普通文件）。
		default:
			return fmt.Errorf("files[%d] (%s) role %q 不合法", i, f.Path, f.Role)
		}
		if f.Path == m.Entrypoint {
			if f.Role != "" && f.Role != RoleBinary {
				return fmt.Errorf("entrypoint 对应的文件 role 必须是 binary（或留空），实际 %q", f.Role)
			}
			hasEntry = true
		}
		total += f.SizeBytes
	}
	if !hasEntry {
		return fmt.Errorf("entrypoint %q 未在 files[] 中登记", m.Entrypoint)
	}
	if m.TotalSizeBytes != total {
		return fmt.Errorf("totalSizeBytes (%d) 与 files 总和 (%d) 不符", m.TotalSizeBytes, total)
	}

	// 签名预留：本轮不接受非 null 签名包。
	sig := strings.TrimSpace(string(m.Signature))
	if sig != "" && sig != "null" {
		return fmt.Errorf("本版本不支持带签名的包（signature 非 null）；请使用无签名包")
	}
	return nil
}

// validateEntryPath 包内路径约束：正斜杠、相对、无回溯、无盘符。
func validateEntryPath(p string) error {
	if p == "" {
		return fmt.Errorf("路径为空")
	}
	if strings.Contains(p, "\\") {
		return fmt.Errorf("路径 %q 必须使用 / 分隔符", p)
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("路径 %q 不得以 / 开头", p)
	}
	// 盘符形态（C:）与设备路径。
	if len(p) > 1 && p[1] == ':' {
		return fmt.Errorf("路径 %q 不得含盘符", p)
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("路径 %q 不得含 ..", p)
	}
	if !entryPathRE.MatchString(p) {
		return fmt.Errorf("路径 %q 含非法字符", p)
	}
	return nil
}

func validateMode(mode string) error {
	if mode == "" {
		return nil // 缺省 0644
	}
	n, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return fmt.Errorf("mode %q 不是八进制串", mode)
	}
	if n > 0o777 {
		return fmt.Errorf("mode %q 超出 0777", mode)
	}
	return nil
}

// FileMode 解析 mode 字段为 os.FileMode（空返回 0644）。
func (f FileEntry) FileMode(def os.FileMode) os.FileMode {
	if f.Mode == "" {
		return def
	}
	n, err := strconv.ParseUint(f.Mode, 8, 32)
	if err != nil || n > 0o777 {
		return def
	}
	return os.FileMode(n)
}

/* ═══════════════════ 语义化版本 ═══════════════════ */

// SemVer 语义化版本（比较用；build 元数据不参与比较）。
type SemVer struct {
	Major, Minor, Patch uint64
	Pre                 string // 预发布标识（如 rc.1），空 = 正式版
	Build               string // 构建元数据（不参与比较）
}

// ParseSemVer 解析并校验语义化版本。
func ParseSemVer(v string) (SemVer, error) {
	m := semverRE.FindStringSubmatch(v)
	if m == nil {
		return SemVer{}, fmt.Errorf("%q 不符合 MAJOR.MINOR.PATCH[-pre][+build]", v)
	}
	var out SemVer
	out.Major, _ = strconv.ParseUint(m[1], 10, 64)
	out.Minor, _ = strconv.ParseUint(m[2], 10, 64)
	out.Patch, _ = strconv.ParseUint(m[3], 10, 64)
	out.Pre = m[5]
	out.Build = m[7]
	return out, nil
}

// CompareSemVer 比较两个版本：a<b 返回 -1，相等返回 0，a>b 返回 1。
// 预发布版低于同版本正式版（1.0.0-rc.1 < 1.0.0）。
func CompareSemVer(a, b SemVer) int {
	if a.Major != b.Major {
		return cmpU64(a.Major, b.Major)
	}
	if a.Minor != b.Minor {
		return cmpU64(a.Minor, b.Minor)
	}
	if a.Patch != b.Patch {
		return cmpU64(a.Patch, b.Patch)
	}
	// 预发布比较：无 > 有；同为预发布按点分标识比较。
	switch {
	case a.Pre == "" && b.Pre == "":
		return 0
	case a.Pre == "":
		return 1
	case b.Pre == "":
		return -1
	}
	return comparePreRelease(a.Pre, b.Pre)
}

// MustCompareSemVer 字符串形式比较（已校验过的版本号便捷比较）。
func MustCompareSemVer(a, b string) int {
	pa, _ := ParseSemVer(a)
	pb, _ := ParseSemVer(b)
	return CompareSemVer(pa, pb)
}

func cmpU64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// comparePreRelease 按 semver 2.0 规范比较预发布标识：
// 点分段逐个比较；数字段按数值，字母段按 ASCII；段少的小。
func comparePreRelease(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var av, bv string
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		if av == bv {
			continue
		}
		an, aerr := strconv.ParseUint(av, 10, 64)
		bn, berr := strconv.ParseUint(bv, 10, 64)
		switch {
		case aerr == nil && berr == nil:
			return cmpU64(an, bn) // 数字段按数值
		case aerr == nil:
			return -1 // 数字段 < 字母段
		case berr == nil:
			return 1
		default:
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

// IsSemVer 快速判断是否为合法语义化版本。
func IsSemVer(v string) bool { return semverRE.MatchString(v) }
