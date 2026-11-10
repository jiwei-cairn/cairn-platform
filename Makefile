# 积微 Cairn 控制台 —— 目标运行环境 Linux（amd64/arm64）与 Windows（amd64）
# 开发机（任意 OS）可直接运行真 fork 监管引擎。
#
# 跨平台说明：`VAR=x cmd` 的 env 前缀语法只有 POSIX shell 支持，cmd.exe 不支持，
# 且 Makefile 无「按 shell 切语法」的可移植写法。故此处统一用 `go env -w`
# （Go 自带、两种 shell 通用）设置交叉编译目标，并在每个目标「开头先复位/
# 结尾再复位」——开头复位使上一次中断的残留自动痊愈，不污染后续构建。
# Windows 用户也可不用 make，直接：
#   set GOOS=windows&& set GOARCH=amd64&& go build -o dist\cairn.exe .

BINARY  := cairn
VERSION := $(shell git describe --tags --always --dirty || echo dev)
BUILTAT := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X github.com/jiwei-cairn/cairn-platform/internal/buildinfo.Version=$(VERSION) \
	-X github.com/jiwei-cairn/cairn-platform/internal/buildinfo.BuildTime=$(BUILTAT)
DIST    := dist
GO      := go
BUILD   := $(GO) build -trimpath -ldflags "$(LDFLAGS)"

# clean 的删除命令按宿主 OS 选择（$(OS) 在 Windows 上为 Windows_NT）。
ifeq ($(OS),Windows_NT)
RMDIST := cmd /c if exist $(DIST) rmdir /s /q $(DIST)
else
RMDIST := rm -rf $(DIST)
endif

.PHONY: linux windows all run vet clean

# 交叉编译 Linux 双架构（生产交付物）
linux:
	$(GO) env -u GOOS GOARCH CGO_ENABLED
	$(GO) env -w GOOS=linux GOARCH=amd64 CGO_ENABLED=0
	$(BUILD) -o $(DIST)/$(BINARY)-linux-amd64/$(BINARY) .
	$(GO) env -w GOARCH=arm64
	$(BUILD) -o $(DIST)/$(BINARY)-linux-arm64/$(BINARY) .
	$(GO) env -u GOOS GOARCH CGO_ENABLED
	@echo "Linux 双架构产物完成：$(DIST)/$(BINARY)-linux-{amd64,arm64}/"

# 交叉编译 Windows amd64（生产交付物）
windows:
	$(GO) env -u GOOS GOARCH CGO_ENABLED
	$(GO) env -w GOOS=windows GOARCH=amd64 CGO_ENABLED=0
	$(BUILD) -o $(DIST)/$(BINARY)-windows-amd64/$(BINARY).exe .
	$(GO) env -u GOOS GOARCH CGO_ENABLED
	@echo "Windows 产物完成：$(DIST)/$(BINARY)-windows-amd64/$(BINARY).exe"

# 全平台产物
all: linux windows

# 本机直跑（真 fork 监管引擎；数据根自动锚定，可加 -root 覆盖）
# 开发者本机联调用回环地址即可；需对外演示时自行改 -addr（V1 无认证，注意范围）。
run:
	$(GO) run . -addr 127.0.0.1:8090

# OTA 验证服务器（docs/upgrade/ota-server.md §8）。
# 注意：Ctrl+C 后若端口未释放（go run 孤儿实例），按启动日志里的
# netstat/taskkill 指引清理；生产替换为任意静态托管。
devserver:
	$(GO) run ./cmd/devserver -root ./ota-root -addr :8090

devserver-build:
	$(GO) build -o dist/devserver.exe ./cmd/devserver

# 质量门禁：vet + 双平台编译 + 单元测试（任一失败即中断）
vet:
	$(GO) env -u GOOS GOARCH CGO_ENABLED
	$(GO) vet ./...
	$(GO) env -w GOOS=windows GOARCH=amd64 CGO_ENABLED=0
	$(BUILD) ./...
	$(GO) env -w GOOS=linux
	$(BUILD) ./...
	$(GO) env -u GOOS GOARCH CGO_ENABLED
	$(GO) test ./...
	@echo "质量门禁通过（vet + windows/linux 编译 + 单元测试）"

clean:
	$(RMDIST)
	@echo "已清理 $(DIST)"
