package pack

import (
	"encoding/json"
	"fmt"
	"time"
)

// IndexEntry 服务清单中的一个包。
type IndexEntry struct {
	Name               string    `json:"name"`
	Version            string    `json:"version"`
	Os                 string    `json:"os"`
	Arch               string    `json:"arch"`
	URL                string    `json:"url"`
	Sha256             string    `json:"sha256"`
	SizeBytes          int64     `json:"sizeBytes"`
	MinPlatformVersion string    `json:"minPlatformVersion,omitempty"`
	ReleasedAt         time.Time `json:"releasedAt"`
	Notes              string    `json:"notes,omitempty"`
	DisplayName        string    `json:"displayName,omitempty"`
	Author             string    `json:"author,omitempty"`
	Description        string    `json:"description,omitempty"`
	Tags               []string  `json:"tags,omitempty"`
	Deps               []string  `json:"deps,omitempty"`
	ServiceType        string    `json:"serviceType,omitempty"`
}

// ServicesIndex 服务域清单。
type ServicesIndex struct {
	Schema      int             `json:"schema"`
	GeneratedAt time.Time       `json:"generatedAt"`
	Packages    []IndexEntry    `json:"packages"`
	Signature   json.RawMessage `json:"signature,omitempty"`
}

// ParseServicesIndex 解析服务清单。
func ParseServicesIndex(raw []byte) (*ServicesIndex, error) {
	si := new(ServicesIndex)
	if err := json.Unmarshal(raw, si); err != nil {
		return nil, err
	}
	if si.Schema != SchemaVersion {
		return nil, fmt.Errorf("服务清单 schema %d 不受支持", si.Schema)
	}
	if si.Packages == nil {
		si.Packages = []IndexEntry{}
	}
	return si, nil
}

// Find 取指定服务、版本和平台的条目。没有则返回 nil。
func (si *ServicesIndex) Find(name, version, osName, arch string) *IndexEntry {
	if si == nil {
		return nil
	}
	for i := range si.Packages {
		e := &si.Packages[i]
		if e.Name == name && e.Version == version && e.Os == osName && e.Arch == arch {
			return e
		}
	}
	return nil
}

// LatestFor 取指定服务在本机平台上的最高版本。没有则返回 nil。
func (si *ServicesIndex) LatestFor(name, osName, arch string) *IndexEntry {
	if si == nil {
		return nil
	}
	var best *IndexEntry
	for i := range si.Packages {
		e := &si.Packages[i]
		if e.Name != name || e.Os != osName || e.Arch != arch {
			continue
		}
		if best == nil || MustCompareSemVer(e.Version, best.Version) > 0 {
			best = e
		}
	}
	return best
}

// FilterPlatform 返回本机平台的全部条目（含历史版本）。
func (si *ServicesIndex) FilterPlatform(osName, arch string) []IndexEntry {
	if si == nil {
		return nil
	}
	out := make([]IndexEntry, 0)
	for _, e := range si.Packages {
		if e.Os == osName && e.Arch == arch {
			out = append(out, e)
		}
	}
	return out
}

// ProductServiceRef 产品对某个服务版本的引用。
type ProductServiceRef struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ProductManifest 产品套件清单（包内唯一文件）。
type ProductManifest struct {
	Schema       int                 `json:"schema"`
	Name         string              `json:"name"`
	Version      string              `json:"version"`
	DisplayName  string              `json:"displayName,omitempty"`
	Author       string              `json:"author,omitempty"`
	Description  string              `json:"description,omitempty"`
	Tags         []string            `json:"tags,omitempty"`
	ReleaseNotes string              `json:"releaseNotes,omitempty"`
	Services     []ProductServiceRef `json:"services"`
	Signature    json.RawMessage     `json:"signature,omitempty"`
}

// Validate 校验产品清单。
func (m *ProductManifest) Validate() error {
	if m == nil {
		return fmt.Errorf("产品清单为空")
	}
	if m.Schema != SchemaVersion {
		return fmt.Errorf("不支持的 schema %d", m.Schema)
	}
	if !namePattern.MatchString(m.Name) {
		return fmt.Errorf("套件名不合法")
	}
	if !IsSemVer(m.Version) {
		return fmt.Errorf("版本不是语义化版本")
	}
	if len(m.Services) == 0 {
		return fmt.Errorf("套件至少引用一个服务")
	}
	seen := map[string]bool{}
	for _, r := range m.Services {
		if !namePattern.MatchString(r.Name) || !IsSemVer(r.Version) {
			return fmt.Errorf("服务引用不合法: %s@%s", r.Name, r.Version)
		}
		key := r.Name + "|" + r.Version
		if seen[key] {
			return fmt.Errorf("重复引用 %s@%s", r.Name, r.Version)
		}
		seen[key] = true
	}
	return nil
}

// ProductEntry 产品清单中的一个套件。
type ProductEntry struct {
	Name        string              `json:"name"`
	Version     string              `json:"version"`
	URL         string              `json:"url"`
	Sha256      string              `json:"sha256"`
	SizeBytes   int64               `json:"sizeBytes"`
	DisplayName string              `json:"displayName,omitempty"`
	Author      string              `json:"author,omitempty"`
	Description string              `json:"description,omitempty"`
	Tags        []string            `json:"tags,omitempty"`
	ReleasedAt  time.Time           `json:"releasedAt"`
	Notes       string              `json:"notes,omitempty"`
	Services    []ProductServiceRef `json:"services"`
}

// ProductsIndex 产品域清单。
type ProductsIndex struct {
	Schema      int             `json:"schema"`
	GeneratedAt time.Time       `json:"generatedAt"`
	Products    []ProductEntry  `json:"products"`
	Signature   json.RawMessage `json:"signature,omitempty"`
}

// ParseProductsIndex 解析产品清单。
func ParseProductsIndex(raw []byte) (*ProductsIndex, error) {
	pi := new(ProductsIndex)
	if err := json.Unmarshal(raw, pi); err != nil {
		return nil, err
	}
	if pi.Schema != SchemaVersion {
		return nil, fmt.Errorf("产品清单 schema %d 不受支持", pi.Schema)
	}
	if pi.Products == nil {
		pi.Products = []ProductEntry{}
	}
	return pi, nil
}

// DomainRef 根索引里一个域的清单位置。
type DomainRef struct {
	Index string `json:"index"`
}

// RootIndex 根索引。
type RootIndex struct {
	Schema      int       `json:"schema"`
	GeneratedAt time.Time `json:"generatedAt"`
	Domains     struct {
		Services DomainRef `json:"services"`
		Products DomainRef `json:"products"`
	} `json:"domains"`
}

// ParseRootIndex 解析根索引。
func ParseRootIndex(raw []byte) (*RootIndex, error) {
	root := new(RootIndex)
	if err := json.Unmarshal(raw, root); err != nil {
		return nil, err
	}
	if root.Schema != SchemaVersion {
		return nil, fmt.Errorf("根索引 schema %d 不受支持", root.Schema)
	}
	if root.Domains.Services.Index == "" || root.Domains.Products.Index == "" {
		return nil, fmt.Errorf("根索引缺少域清单路径")
	}
	return root, nil
}
