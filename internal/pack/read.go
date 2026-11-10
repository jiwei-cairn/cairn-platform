// read.go — .cairnpkg 读取与校验。
//
// 校验分两级（与 OTA 清单的两级哈希对应）：
//   - 结构校验（Open）：manifest 首条目、字段约束、路径约束、条目登记
//   - 内容校验（ExtractTo）：逐文件 sha256/size、解压总量、zip bomb 防护
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

	goruntime "runtime"
)

// ErrNotFirstManifest manifest.json 不是 zip 第一个条目。
var ErrNotFirstManifest = fmt.Errorf("manifest.json 必须是包内第一个条目")

// Package 一个已打开、结构校验通过的 .cairnpkg。
type Package struct {
	Path     string    // 包文件路径
	Manifest *Manifest // 已解析的清单

	zr *zip.ReadCloser
}

// Open 打开包并做结构校验（不解压、不算文件哈希）。
func Open(path string) (*Package, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("不是有效的 ZIP 包: %w", err)
	}
	if len(zr.File) == 0 {
		_ = zr.Close()
		return nil, fmt.Errorf("包内没有任何条目")
	}
	// Z1：manifest 必须是第一个条目（流式先读的前提）。
	if zr.File[0].Name != "manifest.json" {
		_ = zr.Close()
		return nil, ErrNotFirstManifest
	}
	data, err := readZipEntry(zr.File[0], 1<<20) // manifest 上限 1MB
	if err != nil {
		_ = zr.Close()
		return nil, fmt.Errorf("读取 manifest.json 失败: %w", err)
	}
	m := new(Manifest)
	if err := json.Unmarshal(data, m); err != nil {
		_ = zr.Close()
		return nil, fmt.Errorf("manifest.json 解析失败: %w", err)
	}
	if err := m.Validate(); err != nil {
		_ = zr.Close()
		return nil, fmt.Errorf("manifest 校验失败: %w", err)
	}
	p := &Package{Path: path, Manifest: m, zr: zr}

	// Z2/Z3/Z4：逐条目校验路径合法性与登记（不算哈希）。
	listed := make(map[string]FileEntry, len(m.Files))
	for _, f := range m.Files {
		listed[f.Path] = f
	}
	for _, zf := range zr.File {
		if zf.Name == "manifest.json" {
			continue
		}
		if err := validateEntryPath(zf.Name); err != nil {
			_ = zr.Close()
			return nil, fmt.Errorf("包内条目 %q: %w", zf.Name, err)
		}
		// Z4：拒绝符号链接 / 非常规文件。
		if zf.Mode()&os.ModeSymlink != 0 || !zf.Mode().IsRegular() && zf.Mode() != 0 {
			_ = zr.Close()
			return nil, fmt.Errorf("包内条目 %q 不是常规文件（禁止符号链接）", zf.Name)
		}
		if _, ok := listed[zf.Name]; !ok {
			_ = zr.Close()
			return nil, fmt.Errorf("包内条目 %q 未在 files[] 中登记", zf.Name)
		}
	}
	// 清单登记但包内缺失的条目。
	for _, f := range m.Files {
		found := false
		for _, zf := range zr.File {
			if zf.Name == f.Path {
				found = true
				break
			}
		}
		if !found {
			_ = zr.Close()
			return nil, fmt.Errorf("清单登记的文件 %q 在包内缺失", f.Path)
		}
	}
	return p, nil
}

// Close 释放包句柄。
func (p *Package) Close() error {
	if p.zr != nil {
		return p.zr.Close()
	}
	return nil
}

// ExtractTo 把包内容解压到 dir，同时做完整内容校验（Z5 + 逐文件 sha256/size）。
//
//	includeManifest 控制是否把 manifest.json 也写入 dir（部署时留存当前版本清单）。
//	返回实际解压的总字节数。
func (p *Package) ExtractTo(dir string, includeManifest bool) (int64, error) {
	if p.zr == nil {
		return 0, fmt.Errorf("包未打开")
	}
	listed := make(map[string]FileEntry, len(p.Manifest.Files))
	for _, f := range p.Manifest.Files {
		listed[f.Path] = f
	}

	var total int64
	for _, zf := range p.zr.File {
		if zf.Name == "manifest.json" {
			if includeManifest {
				data, err := readZipEntry(zf, 1<<20)
				if err != nil {
					return total, err
				}
				if err := writeFileAtomic(dir, "manifest.json", data, 0o644); err != nil {
					return total, err
				}
			}
			continue
		}
		entry, ok := listed[zf.Name]
		if !ok {
			return total, fmt.Errorf("条目 %q 未登记（结构校验后不应发生）", zf.Name)
		}

		// Z5：解压总量上限（防 zip bomb）。
		if total+int64(zf.UncompressedSize64) > p.Manifest.TotalSizeBytes {
			return total, fmt.Errorf("解压总量超出 totalSizeBytes (%d)，疑似 zip bomb", p.Manifest.TotalSizeBytes)
		}

		src, err := zf.Open()
		if err != nil {
			return total, fmt.Errorf("打开条目 %q 失败: %w", zf.Name, err)
		}
		// 边解压边算哈希；限制单文件读取不超过登记 size+1（防超读）。
		hasher := sha256.New()
		limit := io.LimitReader(src, entry.SizeBytes+1)
		tmp, err := io.ReadAll(limit)
		src.Close()
		if err != nil {
			return total, fmt.Errorf("读取条目 %q 失败: %w", zf.Name, err)
		}
		if int64(len(tmp)) != entry.SizeBytes {
			return total, fmt.Errorf("条目 %q 大小不符（清单 %d，实际 %d）", zf.Name, entry.SizeBytes, len(tmp))
		}
		_, _ = hasher.Write(tmp)
		if got := hex.EncodeToString(hasher.Sum(nil)); got != entry.Sha256 {
			return total, fmt.Errorf("条目 %q sha256 不符（清单 %s…，实际 %s…）", zf.Name, entry.Sha256[:8], got[:8])
		}

		mode := entry.FileMode(0o644)
		if entry.Role == RoleBinary || entry.Path == p.Manifest.Entrypoint {
			mode = entry.FileMode(0o755)
		}
		if err := writeFileAtomic(dir, entry.Path, tmp, mode); err != nil {
			return total, err
		}
		// Linux 上显式 chmod（rename 前的 WriteFile 已带 mode，此处兜底
		// umask 影响）；Windows 无 Unix 权限位，chmod 为无害空操作。
		if goruntime.GOOS != "windows" {
			_ = os.Chmod(filepath.Join(dir, filepath.FromSlash(entry.Path)), mode)
		}
		total += entry.SizeBytes
	}
	return total, nil
}

// readZipEntry 读取单个 zip 条目（带上限防护）。
func readZipEntry(zf *zip.File, limit int64) ([]byte, error) {
	if zf.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("条目 %q 超过大小上限 %d", zf.Name, limit)
	}
	rc, err := zf.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, limit))
}

// writeFileAtomic 写入 dir/rel：先写临时文件再 rename，目录自动创建。
// rel 已通过 validateEntryPath 校验，此处再做一次本地化防穿越（纵深防御）。
func writeFileAtomic(dir, rel string, data []byte, mode os.FileMode) error {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || len(clean) >= 2 && clean[1] == ':' {
		return fmt.Errorf("拒绝写入越界路径 %q", rel)
	}
	dst := filepath.Join(dir, filepath.FromSlash(clean))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".part"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
