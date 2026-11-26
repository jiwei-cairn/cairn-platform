// export.go — 导出已安装服务为 .cairnpkg（decisions.md D9：闭环出口）。
//
// 导出内容 = 服务目录当前产物（bin/dependencies/config）+ 安装清单快照
// 重新打包。导出包与手工打包的包无任何区别，可用于本地升级、回退
// 或放到 OTA 服务器分发。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/pack"
	"github.com/jiwei-cairn/cairn-platform/internal/store"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
	goruntime "runtime"
)

// ExportService GET /api/services/{id}/export：导出当前服务为 .cairnpkg。
func (s *Server) ExportService(w http.ResponseWriter, r *http.Request) {
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

	outPath, name, err := s.buildExportPackage(&def)
	if err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	defer func() {
		_ = os.Remove(outPath)
	}()

	f, err := os.Open(outPath)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	st, _ := f.Stat()

	logx.Module("api").Info("导出服务包", "svc", def.Name, "version", def.Version, "file", name)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	if _, err := io.Copy(w, f); err != nil {
		logx.Module("api").Warn("导出传输中断", "err", err)
	}
}

// buildExportPackage 把当前服务安装态打包为 .cairnpkg（落临时文件）。
// 返回临时文件路径与规范文件名；调用方负责 os.Remove(path)。
// 导出（D9）与发布上传共用；错误可直接展示给用户。
//
// 清单来源两档：
//  1. 安装清单快照（files[] 完整）——deploy 管线安装的服务；
//  2. 快照缺失/不完整（旧式安装：市场登记 / 手工上传产物）→ 从磁盘现场合成，
//     让「已调试完的软件」无论经哪条路径进来都能导出/发布。
func (s *Server) buildExportPackage(def *store.Service) (path, name string, err error) {
	if strings.TrimSpace(def.Version) == "" {
		return "", "", fmt.Errorf("服务没有版本记录，无法导出（旧数据安装的服务）")
	}
	svcDir := filepath.Join(s.installRoot(), def.Name)
	if _, err := os.Stat(svcDir); err != nil {
		return "", "", fmt.Errorf("服务产物目录缺失: %s", svcDir)
	}

	m := new(pack.Manifest)
	snapshotOK := json.Unmarshal(def.Manifest, m) == nil && len(m.Files) > 0 && m.Entrypoint != ""
	if !snapshotOK {
		syn, serr := s.synthesizeManifest(def, svcDir)
		if serr != nil {
			return "", "", serr
		}
		m = syn
	}

	// 收集文件：按登记顺序，磁盘上缺失的条目报错（导出必须完整）。
	var sources []pack.SourceFile
	for _, f := range m.Files {
		src := filepath.Join(svcDir, filepath.FromSlash(f.Path))
		if _, err := os.Stat(src); err != nil {
			return "", "", fmt.Errorf("登记文件缺失: %s", f.Path)
		}
		sources = append(sources, pack.SourceFile{SrcPath: src, PkgPath: f.Path, Role: f.Role, Mode: f.Mode})
	}

	// 导出文件名按规范四元组（F2）。
	name = fmt.Sprintf("%s-%s-%s-%s.cairnpkg", def.Name, def.Version, goruntime.GOOS, goruntime.GOARCH)
	outPath := filepath.Join(os.TempDir(), "cairn-export-"+name)
	if _, err := pack.Build(pack.BuildOptions{
		Name: m.Name, Version: def.Version, Os: m.Os, Arch: m.Arch,
		Entrypoint:  m.Entrypoint,
		DisplayName: m.DisplayName, Author: m.Author, Description: m.Description,
		Tags: m.Tags, ServiceType: def.ServiceType, WebUrl: m.WebUrl, ReleaseNotes: m.ReleaseNotes,
		Args: m.Args, Env: m.Env, Deps: m.Deps, Ports: m.Ports, MinDiskMB: m.MinDiskMB,
		RestartPolicy: m.RestartPolicy, HealthCheck: m.HealthCheck,
		LogPolicy: m.LogPolicy, ConfigFile: m.ConfigFile,
	}, sources, outPath); err != nil {
		return "", "", fmt.Errorf("打包失败: %w", err)
	}
	return outPath, name, nil
}

// synthesizeManifest 为没有安装清单快照的服务（旧式安装/市场登记/手工上传）
// 从磁盘现场合成一份可导出的 manifest：
//
//	entrypoint = ExePath 相对服务目录
//	files      = 服务目录下全部常规文件（排除过渡文件与 manifest.json 本体）
//	role       = entrypoint→binary；config/ 前缀或 configFile.path→config；其余→dependency
//
// 展示元数据尽量从旧清单残留字段（ServiceManifest 形态）补齐。
func (s *Server) synthesizeManifest(def *store.Service, svcDir string) (*pack.Manifest, error) {
	exePath := strings.TrimSpace(def.ExePath)
	if exePath == "" {
		return nil, fmt.Errorf("服务没有可执行文件记录，无法导出")
	}
	rel, err := filepath.Rel(svcDir, exePath)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("可执行文件不在服务产物目录内，无法导出: %s", exePath)
	}
	entry := filepath.ToSlash(rel)
	if _, err := os.Stat(exePath); err != nil {
		return nil, fmt.Errorf("可执行文件缺失: %s", exePath)
	}

	// 旧清单残留的展示字段（ServiceManifest + source 信封，顶层字段即可用）。
	var legacy struct {
		DisplayName  string            `json:"displayName"`
		Author       string            `json:"author"`
		Description  string            `json:"description"`
		Tags         []string          `json:"tags"`
		Deps         []string          `json:"deps"`
		WebUrl       string            `json:"webUrl"`
		Args         []string          `json:"args"`
		Env          map[string]string `json:"env"`
		ReleaseNotes string            `json:"releaseNotes"`
	}
	_ = json.Unmarshal(def.Manifest, &legacy) // 尽力而为，失败不阻断

	m := &pack.Manifest{
		Schema:       pack.SchemaVersion,
		Name:         def.Name,
		Version:      def.Version,
		Os:           goruntime.GOOS,
		Arch:         goruntime.GOARCH,
		Entrypoint:   entry,
		DisplayName:  firstNonEmpty(def.Summary, legacy.DisplayName, def.Name),
		Author:       legacy.Author,
		Description:  legacy.Description,
		Tags:         legacy.Tags,
		WebUrl:       firstNonEmpty(def.WebUrl, legacy.WebUrl),
		Deps:         legacy.Deps,
		Args:         legacy.Args,
		Env:          legacy.Env,
		ReleaseNotes: legacy.ReleaseNotes,
	}

	// 配置路径（role 判定用）。
	cfgPath := ""
	var cf struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(def.ConfigFile, &cf) == nil {
		cfgPath = filepath.ToSlash(cf.Path)
	}

	var files []pack.FileEntry
	walkErr := filepath.WalkDir(svcDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".staging" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil // 包格式禁符号链接（Z4）
		}
		rp, rerr := filepath.Rel(svcDir, p)
		if rerr != nil {
			return rerr
		}
		rp = filepath.ToSlash(rp)
		base := filepath.Base(rp)
		// 过渡/元文件与清单本体不入包。
		if rp == "manifest.json" || strings.HasSuffix(base, ".old") || strings.HasSuffix(base, ".bak") ||
			strings.HasSuffix(base, ".part") || strings.HasSuffix(base, ".wprobe") || strings.HasSuffix(base, ".diskprobe") {
			return nil
		}
		role := pack.RoleDependency
		switch {
		case rp == entry:
			role = pack.RoleBinary
		case strings.HasPrefix(rp, "config/") || (cfgPath != "" && (rp == cfgPath || base == filepath.Base(cfgPath))):
			role = pack.RoleConfig
		}
		sum, herr := pack.Sha256File(p)
		if herr != nil {
			return herr
		}
		st, serr := os.Stat(p)
		if serr != nil {
			return serr
		}
		files = append(files, pack.FileEntry{Path: rp, Sha256: sum, SizeBytes: st.Size(), Role: role})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("扫描服务产物失败: %w", walkErr)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("服务产物目录为空，无法导出")
	}
	m.Files = files
	return m, nil
}
