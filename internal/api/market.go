// market.go — 市场目录（M2-9）：hub 两份清单 → 前端市场页。
//
// 数据源：hub 的静态清单（与设备 OTA 拉取同源、同通道）：
//
//	GET {ota.serverUrl}/cairn/services/index.json → 发现目录（按名称取本机平台最新版）
//	GET {ota.serverUrl}/cairn/products/index.json → 产品套件
//
// 安装状态由前端 join 本地服务列表（market.js marketJoined），后端只管目录。
// 目录条目兼作安装 manifest（前端 openInstallModal 直接 POST /services/install）。
//
// 市场下载走 hub 的 /v1/market/**，请求带已导入的准入证书。
// 设备远程升级仍读公开的 /cairn/**，不使用这张证书。
package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jiwei-cairn/cairn-pkg/pack"

	goruntime "runtime"
)

// marketCatalogEntry 市场目录条目（前端契约：目录条目即安装 manifest，同 schema 两处消费）。
type marketCatalogEntry struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName,omitempty"`
	Display     string   `json:"display,omitempty"` // 卡片展示名（与 displayName 同值；前端两处消费）
	Version     string   `json:"version"`
	ServiceType string   `json:"serviceType"` // 缺省归一为 native
	Author      string   `json:"author,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Deps        []string `json:"deps,omitempty"`
	Notes       string   `json:"notes,omitempty"`
	Os          string   `json:"os,omitempty"`
	Arch        string   `json:"arch,omitempty"`
	SizeBytes   int64    `json:"sizeBytes,omitempty"`
	PublishedAt int64    `json:"publishedAt,omitempty"` // releasedAt（毫秒）
}

// MarketCatalog GET /api/market/catalog：发现目录（本机平台，按名称取最新版）。
func (s *Server) MarketCatalog(w http.ResponseWriter, r *http.Request) {
	cfg := s.Settings.Get()
	if strings.TrimSpace(cfg.OTA.ServerURL) == "" {
		fail(w, http.StatusConflict, "尚未配置远程服务器（设置 → 远程升级 → 服务器地址）")
		return
	}
	if strings.TrimSpace(cfg.OTA.AccessCert) == "" {
		fail(w, http.StatusConflict, "请先在设置中导入 hub 签发的证书")
		return
	}
	idx, err := fetchServicesIndexFrom(cfg.OTA.ServerURL, cfg.OTA.TimeoutSec, "/v1/market", cfg.OTA.AccessCert)
	if err != nil {
		fail(w, http.StatusBadGateway, "市场目录拉取失败: "+err.Error())
		return
	}

	// 按名称取最新版（清单虽已按 name↑ version↓ 排序，防御性地显式比较）。
	latest := map[string]*pack.IndexEntry{}
	for i := range idx.Packages {
		e := &idx.Packages[i]
		if e.Os != goruntime.GOOS || e.Arch != goruntime.GOARCH {
			continue
		}
		cur, ok := latest[e.Name]
		if !ok || pack.MustCompareSemVer(e.Version, cur.Version) > 0 {
			latest[e.Name] = e
		}
	}

	out := make([]marketCatalogEntry, 0, len(latest))
	for _, e := range latest {
		svcType := e.ServiceType
		if svcType == "" {
			svcType = "native" // 旧清单/未标注的包按自研呈现
		}
		var pub int64
		if !e.ReleasedAt.IsZero() {
			pub = e.ReleasedAt.UnixMilli()
		}
		out = append(out, marketCatalogEntry{
			Name: e.Name, DisplayName: e.DisplayName, Display: e.DisplayName,
			Version: e.Version, ServiceType: svcType,
			Author: e.Author, Description: e.Description,
			Tags: e.Tags, Deps: e.Deps, Notes: e.Notes,
			Os: e.Os, Arch: e.Arch, SizeBytes: e.SizeBytes,
			PublishedAt: pub,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	ok(w, out)
}

// marketProductRef 产品内的服务引用。
type marketProductRef struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// marketProduct 产品套件条目（前端契约：p.services 用于套件卡渲染与顺序安装）。
type marketProduct struct {
	Name        string             `json:"name"`
	DisplayName string             `json:"displayName,omitempty"`
	Version     string             `json:"version"`
	Author      string             `json:"author,omitempty"`
	Description string             `json:"description,omitempty"`
	Tags        []string           `json:"tags,omitempty"`
	PublishedAt int64              `json:"publishedAt,omitempty"`
	Services    []marketProductRef `json:"services"`
}

// MarketProducts GET /api/market/products：官方产品套件。
func (s *Server) MarketProducts(w http.ResponseWriter, r *http.Request) {
	cfg := s.Settings.Get()
	if strings.TrimSpace(cfg.OTA.ServerURL) == "" {
		fail(w, http.StatusConflict, "尚未配置远程服务器（设置 → 远程升级 → 服务器地址）")
		return
	}
	if strings.TrimSpace(cfg.OTA.AccessCert) == "" {
		fail(w, http.StatusConflict, "请先在设置中导入 hub 签发的证书")
		return
	}
	pi, err := fetchProductsIndexFrom(cfg.OTA.ServerURL, cfg.OTA.TimeoutSec, "/v1/market", cfg.OTA.AccessCert)
	if err != nil {
		fail(w, http.StatusBadGateway, "产品套件拉取失败: "+err.Error())
		return
	}

	out := make([]marketProduct, 0, len(pi.Products))
	for _, p := range pi.Products {
		var pub int64
		if !p.ReleasedAt.IsZero() {
			pub = p.ReleasedAt.UnixMilli()
		}
		mp := marketProduct{
			Name: p.Name, DisplayName: p.DisplayName, Version: p.Version,
			Author: p.Author, Description: p.Description, Tags: p.Tags,
			PublishedAt: pub,
			Services:    make([]marketProductRef, 0, len(p.Services)),
		}
		for _, ref := range p.Services {
			mp.Services = append(mp.Services, marketProductRef{Name: ref.Name, Version: ref.Version})
		}
		out = append(out, mp)
	}
	ok(w, out)
}

// fetchProductsIndex 拉根索引 → 产品清单（URL 相对解析，G3/G7；镜像 fetchServicesIndex）。
func fetchProductsIndex(baseURL string, timeoutSec int) (*pack.ProductsIndex, error) {
	return fetchProductsIndexFrom(baseURL, timeoutSec, "/cairn", "")
}

func fetchProductsIndexFrom(baseURL string, timeoutSec int, prefix, certJSON string) (*pack.ProductsIndex, error) {
	base := cleanOtaBase(baseURL)
	if base == "" {
		return nil, fmt.Errorf("OTA 服务器未配置")
	}
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	client := &http.Client{Timeout: time.Duration(timeoutSec) * time.Second}

	rootData, rootURL, err := httpGetJSON(client, base+prefix+"/index.json", certJSON)
	if err != nil {
		return nil, fmt.Errorf("拉取根索引失败: %w", err)
	}
	root, err := pack.ParseRootIndex(rootData)
	if err != nil {
		return nil, err
	}
	prodData, _, err := httpGetJSON(client, resolveRel(rootURL, root.Domains.Products.Index), certJSON)
	if err != nil {
		return nil, fmt.Errorf("拉取产品清单失败: %w", err)
	}
	return pack.ParseProductsIndex(prodData)
}
