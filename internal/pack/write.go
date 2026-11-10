// write.go — .cairnpkg 打包（服务导出与 cairn-pack 工具共用）。
package pack

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// SourceFile 打包输入：磁盘文件 → 包内路径。
type SourceFile struct {
	SrcPath string // 磁盘源文件
	PkgPath string // 包内相对路径（/ 分隔）
	Role    string // binary|dependency|config|data
	Mode    string // 缺省按 role 推断（binary=0755，其余 0644）
}

// BuildOptions 打包参数。
type BuildOptions struct {
	Name           string
	Version        string
	Os             string
	Arch           string
	Entrypoint     string
	DisplayName    string
	Author         string
	Description    string
	Tags           []string
	WebUrl         string
	ReleaseNotes   string
	Args           []string
	Env            map[string]string
	Deps           []string
	Ports          []int
	MinDiskMB      int
	RestartPolicy  json.RawMessage
	HealthCheck    json.RawMessage
	LogPolicy      json.RawMessage
	ConfigFile     json.RawMessage
}

// Build 把文件集合打包为 .cairnpkg（manifest.json 强制为第一个条目）。
//
// out 为输出路径；返回生成包的整包 sha256（供发布清单登记）。
func Build(opts BuildOptions, files []SourceFile, out string) (string, error) {
	if len(files) == 0 {
		return "", fmt.Errorf("没有任何文件可打包")
	}
	m := &Manifest{
		Schema:      SchemaVersion,
		Name:        opts.Name,
		Version:     opts.Version,
		Os:          opts.Os,
		Arch:        opts.Arch,
		Entrypoint:  opts.Entrypoint,
		DisplayName: opts.DisplayName,
		Author:      opts.Author,
		Description: opts.Description,
		Tags:        opts.Tags,
		WebUrl:      opts.WebUrl,
		ReleaseNotes: opts.ReleaseNotes,
		Args:        opts.Args,
		Env:         opts.Env,
		Deps:        opts.Deps,
		Ports:       opts.Ports,
		MinDiskMB:   opts.MinDiskMB,
		RestartPolicy: opts.RestartPolicy,
		HealthCheck:   opts.HealthCheck,
		LogPolicy:     opts.LogPolicy,
		ConfigFile:    opts.ConfigFile,
		Signature:   json.RawMessage("null"), // 预留字段（F4：本轮恒 null）
	}

	var total int64
	for _, f := range files {
		if err := validateEntryPath(f.PkgPath); err != nil {
			return "", fmt.Errorf("包内路径 %q: %w", f.PkgPath, err)
		}
		st, err := os.Stat(f.SrcPath)
		if err != nil {
			return "", fmt.Errorf("源文件 %q: %w", f.SrcPath, err)
		}
		if !st.Mode().IsRegular() {
			return "", fmt.Errorf("源文件 %q 不是常规文件", f.SrcPath)
		}
		sum, err := fileSHA256(f.SrcPath)
		if err != nil {
			return "", err
		}
		mode := f.Mode
		if mode == "" {
			if f.Role == RoleBinary || f.PkgPath == opts.Entrypoint {
				mode = "0755"
			} else {
				mode = "0644"
			}
		}
		m.Files = append(m.Files, FileEntry{
			Path: f.PkgPath, Sha256: sum, SizeBytes: st.Size(), Mode: mode, Role: f.Role,
		})
		total += st.Size()
	}
	m.TotalSizeBytes = total
	if err := m.Validate(); err != nil {
		return "", fmt.Errorf("生成的清单不合规: %w", err)
	}

	manifestRaw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	fw, err := os.Create(out)
	if err != nil {
		return "", err
	}
	defer fw.Close()

	hasher := sha256.New()
	multi := io.MultiWriter(fw, hasher)
	zw := zip.NewWriter(multi)

	// Z1：manifest.json 必须第一个写入。
	mw, err := zw.CreateHeader(&zip.FileHeader{
		Name:   "manifest.json",
		Method: zip.Deflate,
	})
	if err != nil {
		return "", err
	}
	if _, err := mw.Write(manifestRaw); err != nil {
		return "", err
	}
	for _, f := range files {
		entryMode := f.Mode
		if entryMode == "" {
			if f.Role == RoleBinary || f.PkgPath == opts.Entrypoint {
				entryMode = "0755"
			} else {
				entryMode = "0644"
			}
		}
		fh := zip.FileHeader{Name: f.PkgPath, Method: zip.Deflate}
		// 把权限写入 zip 头：Linux 解压侧能还原；Windows 忽略。
		if n, err := strconv.ParseUint(entryMode, 8, 32); err == nil {
			fh.SetMode(os.FileMode(n))
		}
		fw2, err := zw.CreateHeader(&fh)
		if err != nil {
			return "", err
		}
		src, err := os.Open(f.SrcPath)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(fw2, src)
		src.Close()
		if err != nil {
			return "", err
		}
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// fileSHA256 计算文件哈希（大文件流式）。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Sha256File 对外暴露（导出/发布工具需要算整包哈希）。
func Sha256File(path string) (string, error) { return fileSHA256(path) }
