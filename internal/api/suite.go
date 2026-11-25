// suite.go — 产品套件发布（P1-3：从已装服务编排 .cairnproduct 并上传 hub）。
//
// 业务流（P1-3 定案）：
//  1. 用户在设置页「产品套件发布」卡片勾选 2~N 个已装服务 + 填套件元信息；
//  2. 平台合成 ProductManifest（services[] = 勾选服务@当前版本）；
//  3. 打 manifest-only ZIP（.cairnproduct，H3：只含版本引用，不带二进制）；
//  4. 上传 hub POST /publish-product（复用 probeHub 能力探测 + 上传通道）。
//
// 引用完整性预检：hub 的 /publish-product 会校验引用的服务版本必须已在
// 服务域发布（否则 409）——因此用户通常应先发布所有组件服务再发布套件。
// 平台侧也做同样预检（发前拉 hub services/index.json 比对），失败提前到提交前。
package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/jiwei-cairn/cairn-pkg/pack"
	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/store"
)

// suitePublishReq POST /api/publish/suite 请求体。
type suitePublishReq struct {
	Name        string   `json:"name"`        // 套件标识（必填，服务名正则）
	Version     string   `json:"version"`     // 语义化版本（必填）
	DisplayName string   `json:"displayName"` // 展示名（可选）
	Description string   `json:"description"` // 描述（可选）
	Tags        []string `json:"tags"`        // 标签（可选）
	Services    []string `json:"services"`    // 勾选的已装服务名（必填 ≥2）
}

// SuitePublish POST /api/publish/suite：合成套件并上传 hub。
func (s *Server) SuitePublish(w http.ResponseWriter, r *http.Request) {
	var req suitePublishReq
	if err := jsonDecodeBody(r, &req); err != nil {
		fail(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}

	// ── 字段校验 ──
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Version) == "" {
		fail(w, http.StatusBadRequest, "套件名与版本为必填")
		return
	}
	if len(req.Services) < 2 {
		fail(w, http.StatusBadRequest, "套件至少需要勾选 2 个服务")
		return
	}
	// 名称合法性：与服务名同正则（复用 pack 校验）。
	if !pack.IsSemVer(req.Version) {
		fail(w, http.StatusBadRequest, fmt.Sprintf("版本 %q 不是语义化版本", req.Version))
		return
	}

	cfg := s.Settings.Get()
	base := strings.TrimSpace(cfg.OTA.ServerURL)
	if base == "" {
		fail(w, http.StatusConflict, "尚未配置远程服务器（设置 → 远程升级 → 服务器地址）")
		return
	}
	// hub 能力探测（复用 probeHub：旧二进制/匿名模式提前拦截）。
	if err := probeHub(cleanOtaBase(base), cfg.OTA.AccessCert); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}

	// ── 解析已装服务引用（从 DB 读当前版本）──
	refs := make([]pack.ProductServiceRef, 0, len(req.Services))
	for _, name := range req.Services {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		var def store.Service
		if err := s.DB.First(&def, "name = ?", name).Error; err != nil {
			fail(w, http.StatusBadRequest, fmt.Sprintf("服务 %q 未安装（套件只能引用已装服务）", name))
			return
		}
		if strings.TrimSpace(def.Version) == "" {
			fail(w, http.StatusConflict, fmt.Sprintf("服务 %q 没有版本记录（旧数据安装），无法引用", name))
			return
		}
		refs = append(refs, pack.ProductServiceRef{Name: def.Name, Version: def.Version})
	}
	if len(refs) < 2 {
		fail(w, http.StatusBadRequest, "去重后套件至少需要 2 个服务")
		return
	}

	// ── 引用完整性预检：hub 的服务域必须已含所有引用版本 ──
	idx, err := fetchServicesIndex(cfg.OTA.ServerURL, cfg.OTA.TimeoutSec)
	if err != nil {
		fail(w, http.StatusBadGateway, "拉取服务端清单失败: "+err.Error())
		return
	}
	published := make(map[string]bool, len(idx.Packages))
	for _, e := range idx.Packages {
		published[e.Name+"|"+e.Version] = true
	}
	var missing []string
	for _, ref := range refs {
		if !published[ref.Name+"|"+ref.Version] {
			missing = append(missing, ref.Name+"@"+ref.Version)
		}
	}
	if len(missing) > 0 {
		fail(w, http.StatusConflict,
			"以下服务尚未在服务端发布，请先发布组件服务再发布套件："+strings.Join(missing, "、"))
		return
	}

	// ── 合成 ProductManifest + manifest-only ZIP ──
	m := &pack.ProductManifest{
		Schema:      pack.SchemaVersion,
		Name:        req.Name,
		Version:     req.Version,
		DisplayName: req.DisplayName,
		Description: req.Description,
		Tags:        req.Tags,
		Services:    refs,
	}
	if err := m.Validate(); err != nil {
		fail(w, http.StatusBadRequest, "套件清单不合规: "+err.Error())
		return
	}
	buf, err := buildProductZip(m)
	if err != nil {
		fail(w, http.StatusInternalServerError, "打包套件失败: "+err.Error())
		return
	}

	// ── 上传 hub /publish-product ──
	res, err := uploadSuiteToHub(cleanOtaBase(base), buf.Bytes(), req.Name, req.Version, cfg.OTA.AccessCert)
	if err != nil {
		var he *hubHTTPError
		if errors.As(err, &he) {
			fail(w, he.code, err.Error())
			return
		}
		fail(w, http.StatusBadGateway, "套件上传失败: "+err.Error())
		return
	}
	logx.Module("api").Info("发布产品套件", "suite", req.Name, "version", req.Version, "services", len(refs))
	s.logEvent("info", "publish", 0, req.Name,
		fmt.Sprintf("发布产品套件：%s %s（引用 %d 个服务）", req.Name, req.Version, len(refs)))
	ok(w, res)
}

// buildProductZip 合成 manifest-only ZIP（.cairnproduct，H3：唯一条目 manifest.json）。
func buildProductZip(m *pack.ProductManifest) (*bytes.Buffer, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	fw, err := zw.Create("manifest.json")
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf, nil
}

// uploadSuiteToHub multipart 上传 .cairnproduct 到 hub /publish-product。
// 返回 hub 响应的 data 字段（发布结果）。
func uploadSuiteToHub(base string, body []byte, name, version, certJSON string) (json.RawMessage, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		w, err := mw.CreateFormFile("file", fmt.Sprintf("%s-%s.cairnproduct", name, version))
		if err == nil {
			_, err = w.Write(body)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()

	req, err := http.NewRequest(http.MethodPost, base+"/publish-product", pr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	applyHubCert(req, certJSON)

	client := &http.Client{Timeout: publishUploadTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode != http.StatusOK {
		return nil, &hubHTTPError{code: resp.StatusCode, body: strings.TrimSpace(string(respBody))}
	}
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
		Msg  string          `json:"msg"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, fmt.Errorf("服务端响应解析失败: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("%s", env.Msg)
	}
	return env.Data, nil
}
