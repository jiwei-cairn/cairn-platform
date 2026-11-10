// index.go — OTA 服务器三份 index.json 的结构与解析。
// 规范见 docs/upgrade/ota-server.md。
package pack

import (
	"encoding/json"
	"fmt"
	"time"
)

// RootIndex 根索引 cairn/index.json：告知两个域的清单位置。
type RootIndex struct {
	Schema      int       `json:"schema"`
	GeneratedAt time.Time `json:"generatedAt"`
	Domains     struct {
		Services DomainRef `json:"services"`
		Products DomainRef `json:"products"`
	} `json:"domains"`
}

// DomainRef 域引用。
type DomainRef struct {
	Index       string    `json:"index"`               // 相对根索引的清单路径
	Count       int       `json:"count,omitempty"`     // 展示用
	UpdatedAt   time.Time `json:"updatedAt,omitempty"` // RFC3339
	Placeholder bool      `json:"placeholder,omitempty"` // true = 仅占位，设备跳过
}

// ServicesIndex 服务清单 cairn/services/index.json。
type ServicesIndex struct {
	Schema      int          `json:"schema"`
	GeneratedAt time.Time    `json:"generatedAt"`
	Packages    []IndexEntry `json:"packages"`
}

// IndexEntry 清单中的单个包条目。
type IndexEntry struct {
	Name               string    `json:"name"`
	Version            string    `json:"version"`
	Os                 string    `json:"os"`
	Arch               string    `json:"arch"`
	URL                string    `json:"url"` // 相对本清单文件的路径
	Sha256             string    `json:"sha256"`
	SizeBytes          int64     `json:"sizeBytes"`
	MinPlatformVersion string    `json:"minPlatformVersion,omitempty"`
	ReleasedAt         time.Time `json:"releasedAt,omitempty"`
	Notes              string    `json:"notes,omitempty"`
}

// ParseRootIndex 解析根索引。
func ParseRootIndex(data []byte) (*RootIndex, error) {
	var ri RootIndex
	if err := json.Unmarshal(data, &ri); err != nil {
		return nil, fmt.Errorf("根索引解析失败: %w", err)
	}
	if ri.Schema != SchemaVersion {
		return nil, fmt.Errorf("根索引 schema %d 不受支持", ri.Schema)
	}
	if ri.Domains.Services.Index == "" {
		return nil, fmt.Errorf("根索引缺少 services 域引用")
	}
	return &ri, nil
}

// ParseServicesIndex 解析服务清单。
func ParseServicesIndex(data []byte) (*ServicesIndex, error) {
	var si ServicesIndex
	if err := json.Unmarshal(data, &si); err != nil {
		return nil, fmt.Errorf("服务清单解析失败: %w", err)
	}
	if si.Schema != SchemaVersion {
		return nil, fmt.Errorf("服务清单 schema %d 不受支持", si.Schema)
	}
	return &si, nil
}

// LatestFor 取指定服务在本机平台（os/arch）下的最新版本条目。
// 预发布版低于同版本正式版（B5：OTA 只升最新，此处"最新"按 semver 取最大）。
// 没有匹配返回 nil。
func (si *ServicesIndex) LatestFor(name, osName, arch string) *IndexEntry {
	var best *IndexEntry
	for i := range si.Packages {
		e := &si.Packages[i]
		if e.Name != name || e.Os != osName || e.Arch != arch {
			continue
		}
		if best == nil || CompareSemVer(mustSemVer(e.Version), mustSemVer(best.Version)) > 0 {
			best = e
		}
	}
	return best
}

// FilterPlatform 过滤出本机平台的全部条目（按名称分组使用）。
func (si *ServicesIndex) FilterPlatform(osName, arch string) []IndexEntry {
	var out []IndexEntry
	for _, e := range si.Packages {
		if e.Os == osName && e.Arch == arch {
			out = append(out, e)
		}
	}
	return out
}

func mustSemVer(v string) SemVer {
	sv, _ := ParseSemVer(v)
	return sv
}
