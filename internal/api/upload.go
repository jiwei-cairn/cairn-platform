// upload.go — 安装资源上传（multipart 暂存）与可执行文件架构嗅探。
//
// 上传流：前端 file input 即时上传 → 暂存 data/uploads/{uuid}/ → 安装请求
// 的 localBinary/extraBinaries/initialConfig 引用暂存路径 → 安装时复制入仓
// 并做架构校验（ELF/PE 文件头解析，零依赖，等价 file 命令且跨平台）。
package api

import (
	"debug/pe"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	goruntime "runtime"

	"github.com/jiwei-cairn/cairn-platform/internal/config"
	"github.com/jiwei-cairn/cairn-platform/internal/logx"

	"github.com/google/uuid"
)

// uploadedFile 一次上传返回的暂存条目。
type uploadedFile struct {
	Name string `json:"name"` // 原始文件名
	Path string `json:"path"` // 暂存路径（安装请求引用）
	Size int64  `json:"size"`
}

// maxUploadBytes 单次上传请求的硬上限（不只是内存阈值）。
// ParseMultipartForm 的参数只是"多少留在内存、超出落盘临时目录"的阈值，
// 配合 http.MaxBytesReader 才能真正拦住任意大请求写满 SD 卡
//（data 与 SQLite 同卷，写满会直接拖垮平台）。
const maxUploadBytes = 256 << 20

// UploadFiles POST /api/services/upload：multipart 多文件暂存。
func (s *Server) UploadFiles(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(64 << 20); err != nil { // 64MB 留内存，其余落系统临时目录
		fail(w, http.StatusRequestEntityTooLarge, "上传超限或解析失败（单请求上限 256MB）: "+err.Error())
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		fail(w, http.StatusBadRequest, "未收到文件（字段名 files）")
		return
	}
	dir := config.UploadsDir(uuid.NewString())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 顺带清扫过期暂存：上传后放弃安装的目录否则会永久累积。
	// 仅在本次上传路径上做，避免额外后台协程。
	pruneStaleUploads(config.UploadsDir(), staleUploadAge)
	out := make([]uploadedFile, 0, len(files))
	for _, fh := range files {
		if fh.Size == 0 {
			continue
		}
		// 文件名收紧为安全路径片段（防目录穿越）。
		name := filepath.Base(fh.Filename)
		dst := filepath.Join(dir, name)
		src, err := fh.Open()
		if err != nil {
			fail(w, http.StatusBadRequest, "读取上传失败: "+err.Error())
			return
		}
		err = copyReaderToFile(src, dst, fh.Size)
		src.Close()
		if err != nil {
			fail(w, http.StatusInternalServerError, "暂存失败: "+err.Error())
			return
		}
		out = append(out, uploadedFile{Name: name, Path: dst, Size: fh.Size})
	}
	ok(w, map[string]any{"files": out})
}

// staleUploadAge 孤儿暂存目录的保留时长：上传后超过该时长仍未被安装引用
// 即视为放弃（用户中途取消安装），可安全清理。
const staleUploadAge = 24 * time.Hour

// pruneStaleUploads 清理 root 下超过 staleUploadAge 未修改的子目录。
// 尽力而为：任何失败仅日志，绝不影响上传主流程。
func pruneStaleUploads(root string, age time.Duration) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return // 目录尚不存在（首次上传）或不可读：无需清理
	}
	cutoff := time.Now().Add(-age)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err == nil {
			removed++
		}
	}
	if removed > 0 {
		logx.Module("api").Info("清理过期上传暂存", "count", removed)
	}
}

// copyReaderToFile 复制上传流到目标文件并校验字节数。
func copyReaderToFile(src io.Reader, dst string, size int64) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, src)
	if err != nil {
		return err
	}
	if size > 0 && n != size {
		return fmt.Errorf("写入不完整: %d/%d", n, size)
	}
	return nil
}

// sniffArch 解析可执行文件头，返回其目标平台/架构（ELF→linux；
// PE→windows；脚本/数据文件返回空串不校验）。ELF 用手工文件头解析
//（最小 20 字节即可判定 e_machine，debug/elf 要求完整 ELF 加载段）。
func sniffArch(path string) (osName, arch string) {
	// 只读文件头 64 字节：原实现 os.ReadFile 全量载入，仅为了看前 20 字节，
	// 同批多个大包预检时内存峰值等于文件总大小（512MB 级设备有 OOM 风险）。
	if f, err := os.Open(path); err == nil {
		head := make([]byte, 64)
		n, _ := io.ReadFull(f, head)
		_ = f.Close()
		if n >= 20 && string(head[0:4]) == "\x7fELF" {
			osName = "linux"
			machine := uint16(head[18]) | uint16(head[19])<<8
			switch machine {
			case 62:
				arch = "amd64"
			case 183:
				arch = "arm64"
			case 40:
				arch = "arm"
			default:
				arch = fmt.Sprintf("elf-%d", machine)
			}
			return
		}
	}
	if f, err := pe.Open(path); err == nil {
		defer f.Close()
		osName = "windows"
		switch f.Machine {
		case pe.IMAGE_FILE_MACHINE_AMD64:
			arch = "amd64"
		case pe.IMAGE_FILE_MACHINE_ARM64:
			arch = "arm64"
		case pe.IMAGE_FILE_MACHINE_ARM:
			arch = "arm"
		default:
			arch = fmt.Sprintf("pe-0x%x", f.Machine)
		}
	}
	return
}

// checkArchMatch 校验暂存文件架构与本机匹配（可执行与动态库都校验；
// 非可执行文件跳过）。返回 error 级预检问题（nil = 通过或无需校验）。
func checkArchMatch(label, path string) *installIssue {
	if path == "" {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil // 暂存缺失由后续复制报错；此处只管架构
	}
	osName, arch := sniffArch(path)
	if osName == "" || arch == "" {
		return nil // 脚本/数据文件：不校验
	}
	if osName != goruntime.GOOS {
		return &installIssue{"error", label,
			fmt.Sprintf("%s 是 %s 平台文件，与本机 %s 不符", filepath.Base(path), osName, goruntime.GOOS)}
	}
	if arch != goruntime.GOARCH {
		return &installIssue{"error", label,
			fmt.Sprintf("%s 架构为 %s，与本机 %s 不符", filepath.Base(path), arch, goruntime.GOARCH)}
	}
	return nil
}