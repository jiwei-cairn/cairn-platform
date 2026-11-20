// embeddedindex.go — 内置默认清单（决策 I1/I2/I3；随 internal/pack 迁出而平台私有化）。
//
// 用途：仅展示。设备离线（拉不到远程清单）时，升级页面仍能呈现出厂
// 可用的服务与版本信息；不含二进制包，不能做离线安装。
// 优先级：远程 services/index.json > 内置清单（ota-server.md §7）。
package api

import (
	_ "embed"

	"github.com/jiwei-cairn/cairn-pkg/pack"
)

//go:embed embedded-services-index.json
var embeddedServicesIndexRaw []byte

// embeddedServicesIndex 解析内置默认清单（编译期资产；解析失败兜底空清单）。
func embeddedServicesIndex() *pack.ServicesIndex {
	si, err := pack.ParseServicesIndex(embeddedServicesIndexRaw)
	if err != nil {
		return &pack.ServicesIndex{Schema: pack.SchemaVersion, Packages: []pack.IndexEntry{}}
	}
	return si
}
