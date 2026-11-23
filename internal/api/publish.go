// publish.go — 服务发布到服务端（业务流程验证版：暂无认证）。
//
// 流程（Phase 3a 的验证切片）：
//  1. buildExportPackage：当前安装态打成 .cairnpkg（与 D9 导出同源）；
//  2. uploadToHub：multipart 流式上传到 {ota.serverUrl}/publish；
//  3. 返回 hub 的落位结果（sha256 / canonical / idempotent）。
//
// 认证：平台身份证书 + 请求签名随 Phase 3a 凭证管理接入；当前 hub 需以
// -anon-publish 启动（hub 侧显式匿名开关，生产禁止）。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/store"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
)

// publishUploadTimeout 发布上传超时（大包走局域网/公网，给足余量；
// 任务化 + 进度条随 Phase 3a 前端接入）。
const publishUploadTimeout = 10 * time.Minute

// PublishService POST /api/services/{id}/publish：发布当前服务到服务端。
func (s *Server) PublishService(w http.ResponseWriter, r *http.Request) {
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
	cfg := s.Settings.Get()
	base := strings.TrimSpace(cfg.OTA.ServerURL)
	if base == "" {
		fail(w, http.StatusConflict, "尚未配置远程服务器（设置 → 远程升级 → 服务器地址）")
		return
	}

	pkgPath, name, err := s.buildExportPackage(&def)
	if err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	defer os.Remove(pkgPath)

	res, err := uploadToHub(base, pkgPath)
	if err != nil {
		var he *hubHTTPError
		if errors.As(err, &he) {
			switch he.code {
			case http.StatusConflict:
				fail(w, http.StatusConflict, err.Error()) // 冲突透传（版本已存在且内容不同）
				return
			case http.StatusUnauthorized, http.StatusForbidden:
				// 认证缺口是当前阶段最常见的一类失败：给出可操作指引，
				// 而非把 hub 的裸 401 文案直接抛给使用者。
				fail(w, he.code, fmt.Sprintf("%s；服务端已开启发布认证，"+
					"验证业务流程期间请以 -anon-publish 启动 cairn-hub（生产环境禁止）", err.Error()))
				return
			}
		}
		// 前缀由前端统一追加，此处只给原因（避免「发布失败：发布失败: …」）。
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	logx.Module("api").Info("发布服务包到服务端", "svc", def.Name, "version", def.Version, "hub", base)
	s.logEvent("info", "publish", def.ID, def.Name,
		fmt.Sprintf("发布到服务端：%s %s（%s）", def.Name, def.Version, name))
	ok(w, res)
}

// hubHTTPError 服务端非 200 响应（保留状态码供调用方映射）。
type hubHTTPError struct {
	code int
	body string
}

func (e *hubHTTPError) Error() string {
	return fmt.Sprintf("服务端拒绝（HTTP %d）：%s", e.code, e.body)
}

// uploadToHub 把本地包 multipart 流式上传到 hub /publish（无认证版）。
// 返回 hub 响应中的 data 字段（发布结果 JSON：name/version/canonical/sha256/sizeBytes/idempotent）。
func uploadToHub(baseURL, pkgPath string) (json.RawMessage, error) {
	base := cleanOtaBase(baseURL)
	if base == "" {
		return nil, fmt.Errorf("远程服务器未配置")
	}
	f, err := os.Open(pkgPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// 流式 multipart（不整包进内存）：管道 + chunked 上传，
	// hub 侧 MaxBytesReader + ParseMultipartForm 照常消费。
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		w, err := mw.CreateFormFile("file", filepath.Base(pkgPath))
		if err == nil {
			_, err = io.Copy(w, f)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()

	req, err := http.NewRequest(http.MethodPost, base+"/publish", pr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	client := &http.Client{Timeout: publishUploadTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode != http.StatusOK {
		return nil, &hubHTTPError{code: resp.StatusCode, body: strings.TrimSpace(string(body))}
	}
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
		Msg  string          `json:"msg"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("服务端响应解析失败: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("%s", env.Msg)
	}
	return env.Data, nil
}
