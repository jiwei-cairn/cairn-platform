// configfile.go — 被管服务配置文件管理（M3 实现读/校验/备份/生效/历史）。
// 当前骨架期返回 501。
package api

import (
	"net/http"
)

// GetConfigfile GET /api/services/{id}/configfile：读取配置文件内容。
func (s *Server) GetConfigfile(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "配置文件读取（M3）")
}

// PutConfigfile PUT /api/services/{id}/configfile：保存（含校验+备份）。
func (s *Server) PutConfigfile(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "配置文件保存（M3）")
}

// ValidateConfigfile POST /api/services/{id}/configfile/validate：dryRun 校验。
func (s *Server) ValidateConfigfile(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "配置文件校验（M3）")
}

// ApplyConfigfile POST /api/services/{id}/configfile/apply：按 applyMode 生效。
func (s *Server) ApplyConfigfile(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "配置文件生效（M3）")
}

// ConfigfileHistory GET /api/services/{id}/configfile/history：版本历史。
func (s *Server) ConfigfileHistory(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "配置文件历史（M3）")
}