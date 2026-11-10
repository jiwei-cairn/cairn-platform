// embedded.go — 内置默认清单（决策 I1/I2/I3）。
//
// 用途：仅展示。设备离线（拉不到远程清单）时，升级页面仍能呈现出厂
// 可用的服务与版本信息；不含二进制包，不能做离线安装。
// 优先级：远程 services/index.json > 内置清单（ota-server.md §7）。
package pack

import (
	_ "embed"
)

//go:embed embedded-services-index.json
var embeddedServicesIndexRaw []byte

// EmbeddedServicesIndex 内置默认清单（与 ServicesIndex 同构）。
func EmbeddedServicesIndex() *ServicesIndex {
	si, err := ParseServicesIndex(embeddedServicesIndexRaw)
	if err != nil {
		// 编译期资产，不该发生；兜底返回空清单。
		return &ServicesIndex{Schema: SchemaVersion, Packages: []IndexEntry{}}
	}
	return si
}
