// devserver — OTA 升级验证服务器（docs/upgrade/ota-server.md §8）。
//
// 定位：开发期在本地模拟静态托管服务器，验证设备端拉清单/下载/校验/
// 故障路径。生产替换为 Nginx/S3/CDN 时设备端零改动（目录结构完全一致）。
//
// 能力：
//   静态托管      http.ServeContent 原生支持 Range 断点续传
//   发布          POST /publish 上传 .cairnpkg → 落位 → 自动重扫三份 index.json
//   重扫          POST /rescan 手动刷新清单
//   故障注入      ?fault=bad-hash|corrupt|slow|500（验证设备端失败处理）
//
// 用法：
//   go run ./cmd/devserver -root ./ota-root -addr :8090
package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/pack"
)

var (
	root = flag.String("root", "ota-root", "静态托管根目录（结构见 ota-server.md）")
	addr = flag.String("addr", ":8090", "监听地址")
)

func main() {
	flag.Parse()
	cairnDir := filepath.Join(*root, "cairn")

	// 启动即重扫：目录里有包就能直接用。
	if err := ensureLayout(cairnDir); err != nil {
		log.Fatalf("初始化目录失败: %v", err)
	}
	if err := rescan(cairnDir); err != nil {
		log.Fatalf("初始清单生成失败: %v", err)
	}

	mux := http.NewServeMux()

	// 静态托管（含 Range）：/cairn/** → root/cairn/**。
	mux.Handle("/cairn/", http.StripPrefix("/cairn/", http.FileServer(http.Dir(cairnDir))))

	// 发布：上传包 → 落位 {name}/ → 重扫。
	mux.HandleFunc("/publish", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 512<<20)
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, "上传解析失败: "+err.Error(), http.StatusBadRequest)
			return
		}
		fh := r.MultipartForm.File["file"][0]
		src, err := fh.Open()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer src.Close()

		// 先暂存到 temp：服务名从包内 manifest 读取（权威来源），
		// 避免从文件名解析（服务名本身可含连字符，启发式不可靠）。
		tmp, err := os.CreateTemp("", "devpub-*.cairnpkg")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tmpPath := tmp.Name()
		n, err := io.Copy(tmp, src)
		tmp.Close()
		if err != nil {
			_ = os.Remove(tmpPath)
			http.Error(w, "保存失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// 校验 + 取 manifest（不合规的包不进清单）。
		p, err := pack.Open(tmpPath)
		if err != nil {
			_ = os.Remove(tmpPath)
			http.Error(w, "包不合规，已拒收: "+err.Error(), http.StatusBadRequest)
			return
		}
		m := p.Manifest
		_ = p.Close()

		dstDir := filepath.Join(cairnDir, "services", m.Name)
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			_ = os.Remove(tmpPath)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// 规范四元组文件名（F2），与包内声明一致。
		canonical := fmt.Sprintf("%s-%s-%s-%s.cairnpkg", m.Name, m.Version, m.Os, m.Arch)
		dst := filepath.Join(dstDir, canonical)
		if err := moveFile(tmpPath, dst); err != nil {
			_ = os.Remove(tmpPath)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := rescan(cairnDir); err != nil {
			http.Error(w, "包已保存但清单生成失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "published: %s (%d bytes), index refreshed\n", canonical, n)
	})

	// 手动重扫。
	mux.HandleFunc("/rescan", func(w http.ResponseWriter, r *http.Request) {
		if err := rescan(cairnDir); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, "index refreshed")
	})

	// 故障注入页（说明）。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `cairn devserver
  GET  /cairn/**          静态托管（Range 支持）
  POST /publish           上传 .cairnpkg（form 字段 file；可选 name）
  POST /rescan            重扫目录生成 index.json
  故障注入（附加在任意 /cairn 请求上）:
       ?fault=bad-hash    清单返回错误的 sha256
       ?fault=corrupt     包响应被截断
       ?fault=slow        响应限速（约 10KB/s）
       ?fault=500         返回 500
`)
	})

	// 故障注入中间件。
	handler := withFaultInjection(mux)

	// 端口预绑定：失败时给出可操作的占用诊断（多为上次 go run 的孤儿实例
	// 占着端口——go run 被 Ctrl+C/IDE 停止后，其编译产物常成孤儿继续存活）。
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("端口 %s 监听失败：%v\n"+
			"  多为上次运行的残留实例占用。定位并结束它：\n"+
			"    netstat -ano | findstr %s\n"+
			"    taskkill /PID <上面查到的PID> /F\n", *addr, err, *addr)
	}

	srv := &http.Server{Handler: handler}
	go func() {
		log.Printf("cairn devserver 监听 %s （root=%s）", *addr, cairnDir)
		log.Printf("设备端配置：设置 → 远程升级 → 服务器地址 http://<本机IP>%s", *addr)
		log.Printf("停止：Ctrl+C（优雅关闭，端口立即释放）")
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	// 优雅关闭：Ctrl+C / SIGTERM → 排空在途请求（最多 3s）→ 释放端口退出。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	log.Printf("收到停止信号，正在关闭（等待在途请求，最多 3s）…")
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	log.Printf("已退出，端口 %s 已释放", *addr)
}

// ensureLayout 建目录骨架 + products 占位清单。
func ensureLayout(cairnDir string) error {
	for _, d := range []string{
		filepath.Join(cairnDir, "services"),
		filepath.Join(cairnDir, "products"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	placeholder := map[string]any{
		"schema": 1, "generatedAt": time.Now().UTC(),
		"placeholder": true,
		"note":       "产品 / 组态域尚未实现，实现留待编排功能落地",
		"products":   []any{},
	}
	raw, _ := json.MarshalIndent(placeholder, "", "  ")
	return os.WriteFile(filepath.Join(cairnDir, "products", "index.json"), raw, 0o644)
}

// rescan 扫描 services/*/*.cairnpkg → 读每包首条目 manifest →
// 生成 services/index.json 与根 index.json（cairn-publish 的等价实现）。
func rescan(cairnDir string) error {
	svcDir := filepath.Join(cairnDir, "services")
	entries := []pack.IndexEntry{}

	err := filepath.WalkDir(svcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".cairnpkg") {
			return nil
		}
		e, perr := inspectPkg(path)
		if perr != nil {
			log.Printf("跳过不合规包 %s: %v", d.Name(), perr)
			return nil
		}
		entries = append(entries, *e)
		return nil
	})
	if err != nil {
		return err
	}

	svcIdx := map[string]any{
		"schema":      1,
		"generatedAt": time.Now().UTC(),
		"packages":    entries,
	}
	raw, _ := json.MarshalIndent(svcIdx, "", "  ")
	if err := os.WriteFile(filepath.Join(svcDir, "index.json"), raw, 0o644); err != nil {
		return err
	}

	rootIdx := map[string]any{
		"schema":      1,
		"generatedAt": time.Now().UTC(),
		"domains": map[string]any{
			"services": map[string]any{
				"index": "services/index.json", "count": len(entries),
				"updatedAt": time.Now().UTC(),
			},
			"products": map[string]any{
				"index": "products/index.json", "count": 0,
				"updatedAt": time.Now().UTC(), "placeholder": true,
			},
		},
	}
	raw, _ = json.MarshalIndent(rootIdx, "", "  ")
	return os.WriteFile(filepath.Join(cairnDir, "index.json"), raw, 0o644)
}

// inspectPkg 读包首条目 manifest，生成清单条目（url 为相对 services/index.json）。
func inspectPkg(path string) (*pack.IndexEntry, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	if len(zr.File) == 0 || zr.File[0].Name != "manifest.json" {
		return nil, fmt.Errorf("manifest.json 不是首条目")
	}
	rc, err := zr.File[0].Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	if err != nil {
		return nil, err
	}
	m := new(pack.Manifest)
	if err := json.Unmarshal(data, m); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	sum, err := pack.Sha256File(path)
	if err != nil {
		return nil, err
	}
	return &pack.IndexEntry{
		Name: m.Name, Version: m.Version, Os: m.Os, Arch: m.Arch,
		// url 相对本清单（services/index.json）的路径：包在 services/{name}/ 下，
		// 无需 ../ 回退（ota-server.md S1/G6）。
		URL:        filepath.ToSlash(mustRel(filepath.Dir(filepath.Dir(path)), path)),
		Sha256:     sum,
		SizeBytes:  st.Size(),
		ReleasedAt: time.Now().UTC(),
		Notes:      m.ReleaseNotes,
	}, nil
}

func mustRel(base, target string) string {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return filepath.Base(target)
	}
	return rel
}

// moveFile rename 优先，跨盘（Windows 的 C:→E: 等）回退为 copy+delete。
// 源文件删除失败（杀毒/索引短暂锁定 %TEMP% 新文件）不视为失败：
// 目标已完整写入，残留的 temp 文件由系统清理。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	for i := 0; i < 5; i++ {
		if err := os.Remove(src); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil // 源删除尽力而为
}

/* ═══════════════════ 故障注入中间件 ═══════════════════ */

// withFaultInjection 对 /cairn 请求按 ?fault= 参数注入故障：
//
//	bad-hash  services/index.json 中的 sha256 被改写（验证下载后哈希校验）
//	corrupt   包响应截断一半（验证完整性校验与重试）
//	slow      限速 ~10KB/s（验证超时）
//	500       直接 500
func withFaultInjection(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fault := r.URL.Query().Get("fault")
		if fault == "" || !strings.HasPrefix(r.URL.Path, "/cairn/") {
			next.ServeHTTP(w, r)
			return
		}
		log.Printf("[fault] %s %s", fault, r.URL.Path)
		switch fault {
		case "500":
			http.Error(w, "injected 500", http.StatusInternalServerError)
		case "slow":
			next.ServeHTTP(&slowWriter{w}, r)
		case "bad-hash":
			// 仅对服务清单注入；其他请求原样。
			if strings.HasSuffix(r.URL.Path, "/services/index.json") {
				injectBadHash(w, r)
				return
			}
			next.ServeHTTP(w, r)
		case "corrupt":
			// 仅对 .cairnpkg 注入；其他请求原样。
			if strings.HasSuffix(r.URL.Path, ".cairnpkg") {
				injectCorrupt(w, r)
				return
			}
			next.ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// injectBadHash 返回 sha256 被改写的清单。
func injectBadHash(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(filepath.Join(*root, "cairn", "services", "index.json"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	var idx map[string]any
	if json.Unmarshal(data, &idx) != nil {
		http.Error(w, "index parse failed", http.StatusInternalServerError)
		return
	}
	if pkgs, ok := idx["packages"].([]any); ok {
		for _, p := range pkgs {
			if m, ok := p.(map[string]any); ok {
				m["sha256"] = strings.Repeat("0", 64) // 全零：必与真实值不符
			}
		}
	}
	out, _ := json.MarshalIndent(idx, "", "  ")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// injectCorrupt 返回截断一半的包体。
func injectCorrupt(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/cairn/")
	f, err := os.Open(filepath.Join(*root, "cairn", rel))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	half := st.Size() / 2
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", half)) // 谎报一半长度
	_, _ = io.CopyN(w, f, half)
}

// slowWriter 限速写入器（~10KB/s）。
type slowWriter struct {
	http.ResponseWriter
}

func (s *slowWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		chunk := 4 * 1024
		if len(p) < chunk {
			chunk = len(p)
		}
		n, err := s.ResponseWriter.Write(p[:chunk])
		total += n
		if err != nil {
			return total, err
		}
		if f, ok := s.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(400 * time.Millisecond) // 4KB/0.4s = 10KB/s
		p = p[chunk:]
	}
	return total, nil
}
