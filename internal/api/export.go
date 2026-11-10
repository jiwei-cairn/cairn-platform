// export.go — 导出已安装服务为 .cairnpkg（decisions.md D9：闭环出口）。
//
// 导出内容 = 服务目录当前产物（bin/dependencies/config）+ 安装清单快照
// 重新打包。导出包与手工打包的包无任何区别，可用于本地升级、回退
// 或放到 OTA 服务器分发。
package api

import (
	"encoding/json"
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
		fail(w, http.StatusNotFound, "服务不存在")
		return
	}
	if strings.TrimSpace(def.Version) == "" {
		fail(w, http.StatusConflict, "服务没有版本记录，无法导出（旧数据安装的服务）")
		return
	}
	svcDir := filepath.Join(s.installRoot(), def.Name)
	if _, err := os.Stat(svcDir); err != nil {
		fail(w, http.StatusConflict, "服务产物目录缺失: "+svcDir)
		return
	}

	// 从安装清单快照取 files/entrypoint 与展示信息（导出保真）。
	m := new(pack.Manifest)
	if err := json.Unmarshal(def.Manifest, m); err != nil || len(m.Files) == 0 {
		fail(w, http.StatusConflict, "安装清单快照缺失或不完整，无法导出")
		return
	}

	// 收集文件：按登记顺序，磁盘上缺失的条目报错（导出必须完整）。
	var sources []pack.SourceFile
	for _, f := range m.Files {
		src := filepath.Join(svcDir, filepath.FromSlash(f.Path))
		if _, err := os.Stat(src); err != nil {
			fail(w, http.StatusConflict, fmt.Sprintf("登记文件缺失: %s", f.Path))
			return
		}
		sources = append(sources, pack.SourceFile{SrcPath: src, PkgPath: f.Path, Role: f.Role, Mode: f.Mode})
	}

	// 导出文件名按规范四元组（F2）。
	name := fmt.Sprintf("%s-%s-%s-%s.cairnpkg", def.Name, def.Version, goruntime.GOOS, goruntime.GOARCH)
	outPath := filepath.Join(os.TempDir(), "cairn-export-"+name)
	if _, err := pack.Build(pack.BuildOptions{
		Name: m.Name, Version: def.Version, Os: m.Os, Arch: m.Arch,
		Entrypoint: m.Entrypoint,
		DisplayName: m.DisplayName, Author: m.Author, Description: m.Description,
		Tags: m.Tags, WebUrl: m.WebUrl, ReleaseNotes: m.ReleaseNotes,
		Args: m.Args, Env: m.Env, Deps: m.Deps, Ports: m.Ports, MinDiskMB: m.MinDiskMB,
		RestartPolicy: m.RestartPolicy, HealthCheck: m.HealthCheck,
		LogPolicy: m.LogPolicy, ConfigFile: m.ConfigFile,
	}, sources, outPath); err != nil {
		fail(w, http.StatusInternalServerError, "打包失败: "+err.Error())
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
