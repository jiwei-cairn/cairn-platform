// usb.go — U盘状态与扫描（M2 实现挂载检测与 upgrade/ 目录扫描）。
package api

import (
	"net/http"
)

// UsbStatus GET /api/usb/status：U盘状态与扫描结果。
func (s *Server) UsbStatus(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "U盘状态（M2）")
}