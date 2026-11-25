package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/jiwei-cairn/cairn-platform/internal/config"
	"github.com/jiwei-cairn/cairn-platform/internal/logx"
)

// GetSettings 返回平台配置（Setting 单行表快照，缺失字段已回落默认值）。
func (s *Server) GetSettings(w http.ResponseWriter, r *http.Request) {
	cfg := s.Settings.Get()
	imported := strings.TrimSpace(cfg.OTA.AccessCert) != ""
	cfg.OTA.AccessCert = ""
	ok(w, struct {
		config.App
		CertImported bool `json:"certImported"`
	}{App: cfg, CertImported: imported})
}

// needRestartItem 是“保存成功但需重启平台生效”的字段提示。
type needRestartItem struct {
	Field string `json:"field"`
	Label string `json:"label"`
}

// SaveSettings 保存平台配置（POST/PUT 均可）。
//
// 语义与前端 mock 一致：请求体在当前配置上做 JSON 覆盖（缺失字段保持原值）；
// 校验失败返回 400 + issues[]（field 用 tftp.server 式路径）；保存成功返回
// {applied, needRestart}——web.port 变更需重启平台，其余热应用
// （日志配额立即重建 logx 输出）。
func (s *Server) SaveSettings(w http.ResponseWriter, r *http.Request) {
	prev := s.Settings.Get()
	next := prev // 值类型拷贝；Decode 在其上覆盖
	if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
		fail(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}
	// 准入证书只由导入/移除接口改写，普通保存不动它。
	next.OTA.AccessCert = prev.OTA.AccessCert
	if issues := config.Validate(next); len(issues) > 0 {
		failIssues(w, fmt.Sprintf("%d 处字段无效", len(issues)), issues)
		return
	}

	// 变更归类：这些字段需重启平台才真正生效，其余则热应用。
	// collect.intervalSec 归入需重启：主机指标采样器的 ticker 在启动时定死
	//（StartHostSampler 幂等，不会因保存而重建节拍），原先谎报为"已热应用"。
	needRestartFields := map[string]string{
		"web.port":            "Web 端口",
		"collect.intervalSec": "指标采集周期",
	}
	applied := []string{}
	needRestart := []needRestartItem{}
	for _, section := range []struct {
		name string
		a, b any
	}{
		{"ota", prev.OTA, next.OTA},
		{"web", prev.Web, next.Web},
		{"logQuota", prev.LogQuota, next.LogQuota},
		{"collect", prev.Collect, next.Collect},
	} {
		for _, field := range diffFields(section.a, section.b) {
			path := section.name + "." + field
			if label, ok := needRestartFields[path]; ok {
				needRestart = append(needRestart, needRestartItem{Field: path, Label: label})
				continue
			}
			applied = append(applied, path)
		}
	}

	if err := s.Settings.Save(next); err != nil {
		fail(w, http.StatusInternalServerError, "保存平台配置失败: "+err.Error())
		return
	}
	// 日志配额热应用：swapHandler 只换内层，已建立的 SSE 订阅不受影响。
	if !reflect.DeepEqual(prev.LogQuota, next.LogQuota) {
		logx.Init(next.LogOptions())
		logx.Module("api").Info("日志配额已热应用")
	}
	ok(w, map[string]any{"applied": applied, "needRestart": needRestart})
}

// ImportHubCert POST /api/settings/hub-cert：导入 hub 签发的准入证书原文。
func (s *Server) ImportHubCert(w http.ResponseWriter, r *http.Request) {
	raw, cert, err := readAccessCert(r)
	if err != nil || cert.PlatformID == "" || cert.ID == "" || cert.Sig == "" {
		fail(w, http.StatusBadRequest, "不是有效的准入证书文件")
		return
	}
	cfg := s.Settings.Get()
	cfg.OTA.AccessCert = strings.TrimSpace(string(raw))
	if err := s.Settings.Save(cfg); err != nil {
		fail(w, http.StatusInternalServerError, "保存证书失败: "+err.Error())
		return
	}
	logx.Module("api").Info("已导入 hub 准入证书", "platform", cert.PlatformID, "expires", cert.ExpiresAt)
	ok(w, map[string]any{"platformId": cert.PlatformID, "expiresAt": cert.ExpiresAt})
}

// ClearHubCert DELETE /api/settings/hub-cert：移除已导入的证书。
func (s *Server) ClearHubCert(w http.ResponseWriter, r *http.Request) {
	cfg := s.Settings.Get()
	cfg.OTA.AccessCert = ""
	if err := s.Settings.Save(cfg); err != nil {
		fail(w, http.StatusInternalServerError, "移除证书失败: "+err.Error())
		return
	}
	ok(w, map[string]any{"cleared": true})
}

// diffFields 对比两个同类型结构体，返回值不同的 json 字段名（camelCase）。
func diffFields(a, b any) []string {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Kind() != reflect.Struct || vb.Kind() != reflect.Struct || va.NumField() != vb.NumField() {
		return nil
	}
	t := va.Type()
	var out []string
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := tag
		for j := 0; j < len(name); j++ {
			if name[j] == ',' {
				name = name[:j]
				break
			}
		}
		if name == "" {
			continue
		}
		if !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			out = append(out, name)
		}
	}
	return out
}
