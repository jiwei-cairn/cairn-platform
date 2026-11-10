// Package web 组装 chi 路由与 go:embed 静态 SPA。
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/api"
	"github.com/jiwei-cairn/cairn-platform/internal/logx"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

//go:embed static
var staticFS embed.FS

// requestLogger 把 HTTP 请求日志接入 logx（mod=http 分支）。
// 大盘/日志/U盘/任务等高频轮询接口降级为 Debug，避免刷满日志。
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		defer func() {
			log := logx.Module("http")
			level := logx.RequestLogLevel(r.Method, r.URL.Path, ww.Status())
			log.Log(r.Context(), level, "HTTP",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"elapsed", time.Since(start).String(),
			)
		}()
		next.ServeHTTP(ww, r)
	})
}

// Router 组装静态页面与 REST API（需求 §6 全量路由 + 架构 §2.1 增补）。
func Router(s *api.Server) http.Handler {
	r := chi.NewRouter()
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)

	// 健康探针挂在 /api 之外，便于容器/systemd 直接探活。
	r.Get("/healthz", s.Health)
	r.Get("/readyz", s.Ready)

	r.Route("/api", func(r chi.Router) {
		r.Get("/version", s.GetVersion)
		r.Get("/dashboard", s.GetDashboard)
		r.Get("/dashboard/host-metrics", s.GetHostMetrics)
		r.Get("/events", s.ListEvents)

		// 服务定义 CRUD + 手动操作 + 统计曲线。
		r.Get("/services", s.ListServices)
		r.Post("/services", s.SaveService)
		// 安装制入口：manifest 说明文档安装（预检 + 落盘 + 建档）+ 资源上传 + 说明卡导出。
		r.Post("/services/install", s.InstallService)
		r.Post("/services/install/precheck", s.InstallPrecheck)
		r.Post("/services/upload", s.UploadFiles)
		r.Get("/services/{id}/manifest", s.ExportManifest)
		r.Delete("/services/{id}", s.DeleteService)
		r.Post("/services/{id}/start", s.StartService)
		r.Post("/services/{id}/stop", s.StopService)
		r.Post("/services/{id}/restart", s.RestartService)
		r.Get("/services/{id}/stats", s.ServiceStats)

		// 配置文件五动作：读/存(dryRun)/校验/生效/历史。
		r.Get("/services/{id}/configfile", s.GetConfigfile)
		r.Put("/services/{id}/configfile", s.PutConfigfile)
		r.Post("/services/{id}/configfile/validate", s.ValidateConfigfile)
		r.Post("/services/{id}/configfile/apply", s.ApplyConfigfile)
		r.Get("/services/{id}/configfile/history", s.ConfigfileHistory)

		// 日志：快照 + SSE 实时 + zip 导出。
		r.Get("/logs", s.LogSnapshot)
		r.Get("/logs/stream", s.LogStream)
		r.Get("/logs/export", s.LogExport)

		// 升级：检查/受理/任务轮询/历史 + 本地上传包（decisions.md A1/A2/D1）。
		r.Post("/upgrade/check", s.UpgradeCheck)
		r.Post("/upgrade/apply", s.UpgradeApply)
		r.Post("/upgrade/local", s.UpgradeLocal)
		r.Get("/upgrade/tasks/{taskId}", s.UpgradeTask)
		r.Get("/upgrade/records", s.UpgradeRecords)

		// 导出服务包（.cairnpkg，闭环出口 D9）。
		r.Get("/services/{id}/export", s.ExportService)

		// 平台配置。
		r.Get("/settings", s.GetSettings)
		r.Post("/settings", s.SaveSettings)
		r.Put("/settings", s.SaveSettings)
	})

	sub, _ := fs.Sub(staticFS, "static")
	r.Handle("/*", http.FileServer(http.FS(sub)))
	return r
}
