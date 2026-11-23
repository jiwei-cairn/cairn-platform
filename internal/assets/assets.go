// Package assets 持有平台构建期静态资产（go:embed）及其解析入口。
//
// 与 internal/api 的分层：api 是 HTTP 处理器层，本包是纯数据资产层——
// 资产被多处消费（升级页兜底、将来市场兜底等）时不引入 api 依赖。
//
// 当前资产：
//
//	embedded-services-index.json  内置默认服务清单（decisions.md I1/I2/I3）
package assets

import (
	_ "embed"

	"github.com/jiwei-cairn/cairn-pkg/pack"
)

//go:embed embedded-services-index.json
var embeddedServicesIndexRaw []byte

// EmbeddedServicesIndex 返回内置默认服务清单（出厂兜底，仅展示）。
//
// 用途（decisions.md I2/I3）：设备离线（拉不到远程清单）时，升级页面仍能
// 呈现出厂可用的服务与版本信息；不含二进制包，不能做离线安装。
// 优先级（ota-server.md §7）：远程 services/index.json > 本内置清单。
//
// 编译期资产，解析失败不应发生；兜底返回空清单（不阻断调用方）。
func EmbeddedServicesIndex() *pack.ServicesIndex {
	si, err := pack.ParseServicesIndex(embeddedServicesIndexRaw)
	if err != nil {
		return &pack.ServicesIndex{Schema: pack.SchemaVersion, Packages: []pack.IndexEntry{}}
	}
	return si
}
