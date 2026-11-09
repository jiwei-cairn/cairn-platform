// upgrade.go — 升级管理（M2 实现统一管线：验签→预检→停服→安装→切换→健康验证→commit/回滚）。
// 当前骨架期返回 501。
package api

import (
	"net/http"
)

// UpgradeCheck POST /api/upgrade/check：按 TFTP 配置检查可用更新。
func (s *Server) UpgradeCheck(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "升级检查（M2）")
}

// UpgradeApply POST /api/upgrade/apply：受理升级/回滚/U盘批量任务。
func (s *Server) UpgradeApply(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "升级执行（M2）")
}

// UpgradeTask GET /api/upgrade/tasks/{taskId}：任务状态轮询。
func (s *Server) UpgradeTask(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "升级任务查询（M2）")
}

// UpgradeRecords GET /api/upgrade/records：升级历史（表查询真实可用）。
func (s *Server) UpgradeRecords(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "升级历史（M2）")
}

// UpgradeRollback POST /api/upgrade/records/{id}/rollback：一键回滚。
func (s *Server) UpgradeRollback(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "版本回滚（M2）")
}