// events.go — 事件流水（EventLog 表倒序 + 服务名 join）。
package api

import (
	"net/http"
	"strconv"

	"github.com/jiwei-cairn/cairn-platform/internal/store"
)

// EventView 是事件流的一条记录。
type EventView struct {
	Level       string `json:"level"`
	ServiceID   int    `json:"serviceId"`
	ServiceName string `json:"serviceName"`
	Message     string `json:"message"`
	At          int64  `json:"at"`
}

// ListEvents GET /api/events：EventLog 表倒序（服务名 join）。
func (s *Server) ListEvents(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	var rows []store.EventLog
	s.DB.Order("id desc").Limit(limit).Find(&rows)
	if len(rows) == 0 {
		ok(w, []EventView{})
		return
	}
	// 只取本页事件涉及的 service id，并仅查 id/name 两列拼名字
	//（避免每次拉事件都全表加载所有服务定义）。
	idSet := make(map[int]struct{}, len(rows))
	for _, row := range rows {
		if row.ServiceID > 0 {
			idSet[row.ServiceID] = struct{}{}
		}
	}
	names := make(map[int]string, len(idSet))
	if len(idSet) > 0 {
		ids := make([]int, 0, len(idSet))
		for id := range idSet {
			ids = append(ids, id)
		}
		type nameRow struct {
			ID   int
			Name string
		}
		var svcs []nameRow
		s.DB.Model(&store.Service{}).Select("id, name").Where("id IN ?", ids).Scan(&svcs)
		for _, d := range svcs {
			names[d.ID] = d.Name
		}
	}
	out := make([]EventView, 0, len(rows))
	for _, row := range rows {
		out = append(out, EventView{
			Level: row.Level, ServiceID: row.ServiceID, ServiceName: names[row.ServiceID],
			Message: row.Message, At: row.CreatedAt.UnixMilli(),
		})
	}
	ok(w, out)
}