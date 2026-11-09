// Package buildinfo 暴露构建元数据，供 /api/version 与大盘平台只读条展示。
package buildinfo

// Version 发布构建时经
// -ldflags "-X github.com/jiwei-cairn/cairn-platform/internal/buildinfo.Version=<version>"
// 注入（见 Makefile LDFLAGS）。
var Version = "v0.4.0-skeleton"

// BuildTime 构建时间（ldflags 注入；空则前端显示占位符）。
var BuildTime = ""

// PlatformName 平台自身在 API 与大盘上的标识（自管条目不参与监管）。
const PlatformName = "cairn-console"
