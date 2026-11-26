// Package pack 是 .cairnpkg / .cairnproduct 的格式、清单和打包。
// 暂时放在本程序里。Hub 与平台各有一份，成熟后再抽成独立包。
package pack

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const SchemaVersion = 1

const (
	RoleBinary     = "binary"
	RoleDependency = "dependency"
	RoleConfig     = "config"
	RoleData       = "data"
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// FileEntry 包内一个文件。
type FileEntry struct {
	Path      string `json:"path"`
	Sha256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
	Mode      string `json:"mode,omitempty"`
	Role      string `json:"role"`
}

// Manifest 服务包清单。
type Manifest struct {
	Schema             int               `json:"schema"`
	Name               string            `json:"name"`
	Version            string            `json:"version"`
	Os                 string            `json:"os"`
	Arch               string            `json:"arch"`
	Entrypoint         string            `json:"entrypoint"`
	TotalSizeBytes     int64             `json:"totalSizeBytes,omitempty"`
	Files              []FileEntry       `json:"files"`
	DisplayName        string            `json:"displayName,omitempty"`
	Author             string            `json:"author,omitempty"`
	Description        string            `json:"description,omitempty"`
	Tags               []string          `json:"tags,omitempty"`
	WebUrl             string            `json:"webUrl,omitempty"`
	ReleaseNotes       string            `json:"releaseNotes,omitempty"`
	ServiceType        string            `json:"serviceType,omitempty"`
	Args               []string          `json:"args,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	Deps               []string          `json:"deps,omitempty"`
	Ports              []int             `json:"ports,omitempty"`
	MinDiskMB          int               `json:"minDiskMB,omitempty"`
	MinPlatformVersion string            `json:"minPlatformVersion,omitempty"`
	RestartPolicy      json.RawMessage   `json:"restartPolicy,omitempty"`
	HealthCheck        json.RawMessage   `json:"healthCheck,omitempty"`
	LogPolicy          json.RawMessage   `json:"logPolicy,omitempty"`
	ConfigFile         json.RawMessage   `json:"configFile,omitempty"`
	Signature          json.RawMessage   `json:"signature,omitempty"`
}

// Validate 校验清单字段。不读 zip 内容。
func (m *Manifest) Validate() error {
	if m == nil {
		return fmt.Errorf("清单为空")
	}
	if m.Schema != SchemaVersion {
		return fmt.Errorf("不支持的 schema %d", m.Schema)
	}
	if !namePattern.MatchString(m.Name) {
		return fmt.Errorf("服务名不合法")
	}
	if !IsSemVer(m.Version) {
		return fmt.Errorf("版本不是语义化版本")
	}
	switch m.Os {
	case "linux", "windows":
	default:
		return fmt.Errorf("os 必须是 linux 或 windows")
	}
	switch m.Arch {
	case "amd64", "arm64", "arm":
	default:
		return fmt.Errorf("arch 必须是 amd64、arm64 或 arm")
	}
	if m.Entrypoint == "" || !safeRel(m.Entrypoint) {
		return fmt.Errorf("entrypoint 不合法")
	}
	if len(m.Files) == 0 {
		return fmt.Errorf("files 为空")
	}
	seen := map[string]bool{}
	var total int64
	hasEntry := false
	for _, f := range m.Files {
		if !safeRel(f.Path) || len(f.Path) > 255 {
			return fmt.Errorf("文件路径不合法: %s", f.Path)
		}
		if seen[f.Path] {
			return fmt.Errorf("文件路径重复: %s", f.Path)
		}
		seen[f.Path] = true
		if len(f.Sha256) != 64 {
			return fmt.Errorf("sha256 不合法: %s", f.Path)
		}
		if f.SizeBytes < 0 {
			return fmt.Errorf("文件大小不合法: %s", f.Path)
		}
		switch f.Role {
		case RoleBinary, RoleDependency, RoleConfig, RoleData:
		default:
			return fmt.Errorf("文件角色不合法: %s", f.Path)
		}
		if f.Path == m.Entrypoint {
			if f.Role != RoleBinary {
				return fmt.Errorf("entrypoint 的角色必须是 binary")
			}
			hasEntry = true
		}
		total += f.SizeBytes
	}
	if !hasEntry {
		return fmt.Errorf("files 中没有 entrypoint")
	}
	if m.TotalSizeBytes != 0 && m.TotalSizeBytes != total {
		return fmt.Errorf("totalSizeBytes 与 files 之和不符")
	}
	return nil
}

// SourceFile 打包时的一个源文件。
type SourceFile struct {
	SrcPath string
	PkgPath string
	Role    string
	Mode    string
}

// BuildOptions 打包参数。未填的展示字段留空。
type BuildOptions struct {
	Name               string
	Version            string
	Os                 string
	Arch               string
	Entrypoint         string
	DisplayName        string
	Author             string
	Description        string
	Tags               []string
	ServiceType        string
	WebUrl             string
	ReleaseNotes       string
	Args               []string
	Env                map[string]string
	Deps               []string
	Ports              []int
	MinDiskMB          int
	MinPlatformVersion string
	RestartPolicy      json.RawMessage
	HealthCheck        json.RawMessage
	LogPolicy          json.RawMessage
	ConfigFile         json.RawMessage
}

// Build 写出 .cairnpkg。manifest.json 是第一个条目。返回整包 sha256。
func Build(opt BuildOptions, files []SourceFile, outPath string) (string, error) {
	if len(files) == 0 {
		return "", fmt.Errorf("没有要打包的文件")
	}
	m := &Manifest{
		Schema: SchemaVersion, Name: opt.Name, Version: opt.Version,
		Os: opt.Os, Arch: opt.Arch, Entrypoint: strings.ReplaceAll(opt.Entrypoint, `\`, "/"),
		DisplayName: opt.DisplayName, Author: opt.Author, Description: opt.Description,
		Tags: opt.Tags, ServiceType: opt.ServiceType, WebUrl: opt.WebUrl, ReleaseNotes: opt.ReleaseNotes,
		Args: opt.Args, Env: opt.Env, Deps: opt.Deps, Ports: opt.Ports, MinDiskMB: opt.MinDiskMB,
		MinPlatformVersion: opt.MinPlatformVersion,
		RestartPolicy:      opt.RestartPolicy, HealthCheck: opt.HealthCheck,
		LogPolicy: opt.LogPolicy, ConfigFile: opt.ConfigFile,
	}
	var total int64
	for _, f := range files {
		pkgPath := strings.ReplaceAll(f.PkgPath, `\`, "/")
		sum, size, err := hashFile(f.SrcPath)
		if err != nil {
			return "", fmt.Errorf("读取 %s: %w", f.SrcPath, err)
		}
		role := f.Role
		if role == "" {
			if pkgPath == m.Entrypoint {
				role = RoleBinary
			} else {
				role = RoleDependency
			}
		}
		mode := f.Mode
		if mode == "" {
			if role == RoleBinary {
				mode = "0755"
			} else {
				mode = "0644"
			}
		}
		m.Files = append(m.Files, FileEntry{Path: pkgPath, Sha256: sum, SizeBytes: size, Mode: mode, Role: role})
		total += size
	}
	m.TotalSizeBytes = total
	if err := m.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	fw, err := os.Create(outPath)
	if err != nil {
		return "", err
	}
	zw := zip.NewWriter(fw)
	mh := &zip.FileHeader{Name: "manifest.json", Method: zip.Deflate}
	mh.SetMode(0o644)
	mw, err := zw.CreateHeader(mh)
	if err != nil {
		zw.Close()
		fw.Close()
		return "", err
	}
	if _, err := mw.Write(raw); err != nil {
		zw.Close()
		fw.Close()
		return "", err
	}
	for _, f := range files {
		src, err := os.Open(f.SrcPath)
		if err != nil {
			zw.Close()
			fw.Close()
			return "", err
		}
		pkgPath := strings.ReplaceAll(f.PkgPath, `\`, "/")
		h := &zip.FileHeader{Name: pkgPath, Method: zip.Deflate}
		h.SetMode(0o644)
		w, err := zw.CreateHeader(h)
		if err != nil {
			src.Close()
			zw.Close()
			fw.Close()
			return "", err
		}
		if _, err := io.Copy(w, src); err != nil {
			src.Close()
			zw.Close()
			fw.Close()
			return "", err
		}
		src.Close()
	}
	if err := zw.Close(); err != nil {
		fw.Close()
		return "", err
	}
	if err := fw.Close(); err != nil {
		return "", err
	}
	return Sha256File(outPath)
}

// Package 已打开的服务包。
type Package struct {
	Manifest *Manifest
	z        *zip.ReadCloser
}

// Open 打开并校验 .cairnpkg：首条目是 manifest，字段合法，文件哈希与清单一致。
func Open(pkgPath string) (*Package, error) {
	z, err := zip.OpenReader(pkgPath)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Package, error) {
		z.Close()
		return nil, err
	}
	if len(z.File) == 0 || z.File[0].Name != "manifest.json" {
		return fail(fmt.Errorf("manifest.json 不是首条目"))
	}
	rc, err := z.File[0].Open()
	if err != nil {
		return fail(err)
	}
	data, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	rc.Close()
	if err != nil {
		return fail(err)
	}
	m := new(Manifest)
	if err := json.Unmarshal(data, m); err != nil {
		return fail(err)
	}
	if err := m.Validate(); err != nil {
		return fail(err)
	}
	listed := map[string]FileEntry{}
	for _, f := range m.Files {
		listed[f.Path] = f
	}
	seen := map[string]bool{}
	for _, zf := range z.File[1:] {
		name := strings.ReplaceAll(zf.Name, `\`, "/")
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		f, ok := listed[name]
		if !ok {
			return fail(fmt.Errorf("包内文件未在清单登记: %s", name))
		}
		sum, size, err := hashZip(zf)
		if err != nil {
			return fail(err)
		}
		if sum != f.Sha256 || size != f.SizeBytes {
			return fail(fmt.Errorf("文件内容与清单不符: %s", name))
		}
		seen[name] = true
	}
	for p := range listed {
		if !seen[p] {
			return fail(fmt.Errorf("清单文件不在包内: %s", p))
		}
	}
	return &Package{Manifest: m, z: z}, nil
}

// Close 关闭 zip。
func (p *Package) Close() error {
	if p == nil || p.z == nil {
		return nil
	}
	return p.z.Close()
}

// ExtractTo 把包内文件解到 dest。includeManifest 为 false 时不写出 manifest.json，
// 部署流程只换清单登记过的文件。写出时再次核对 sha256 和大小。返回写出的文件数。
func (p *Package) ExtractTo(dest string, includeManifest bool) (int, error) {
	if p == nil || p.z == nil || p.Manifest == nil {
		return 0, fmt.Errorf("包未打开")
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return 0, err
	}
	listed := map[string]FileEntry{}
	for _, f := range p.Manifest.Files {
		listed[f.Path] = f
	}
	n := 0
	for _, zf := range p.z.File {
		name := strings.ReplaceAll(zf.Name, `\`, "/")
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		if name == "manifest.json" && !includeManifest {
			continue
		}
		if !safeRel(name) {
			return n, fmt.Errorf("包内路径不合法: %s", name)
		}
		entry, ok := listed[name]
		if name != "manifest.json" && !ok {
			return n, fmt.Errorf("包内文件未在清单登记: %s", name)
		}
		outPath := filepath.Join(dest, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return n, err
		}
		rc, err := zf.Open()
		if err != nil {
			return n, err
		}
		sum, size, err := writeHashed(outPath, rc)
		rc.Close()
		if err != nil {
			return n, err
		}
		if ok && (sum != entry.Sha256 || size != entry.SizeBytes) {
			return n, fmt.Errorf("解包后内容与清单不符: %s", name)
		}
		mode := os.FileMode(0o644)
		if ok && entry.Mode != "" {
			if parsed, perr := parseMode(entry.Mode); perr == nil {
				mode = parsed
			}
		}
		_ = os.Chmod(outPath, mode)
		n++
	}
	for pathName := range listed {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(pathName))); err != nil {
			return n, fmt.Errorf("清单文件未解出: %s", pathName)
		}
	}
	return n, nil
}

// Sha256File 文件的小写十六进制 SHA-256。
func Sha256File(path string) (string, error) {
	sum, _, err := hashFile(path)
	return sum, err
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func writeHashed(outPath string, r io.Reader) (string, int64, error) {
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(f, io.TeeReader(r, h))
	cerr := f.Close()
	if err != nil {
		return "", 0, err
	}
	if cerr != nil {
		return "", 0, cerr
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func parseMode(s string) (os.FileMode, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 8, 32)
	if err != nil {
		return 0, err
	}
	return os.FileMode(v), nil
}

func hashZip(zf *zip.File) (string, int64, error) {
	rc, err := zf.Open()
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func safeRel(p string) bool {
	p = strings.ReplaceAll(p, `\`, "/")
	if p == "" || path.IsAbs(p) || strings.HasPrefix(p, "/") {
		return false
	}
	if len(p) >= 2 && p[1] == ':' {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
