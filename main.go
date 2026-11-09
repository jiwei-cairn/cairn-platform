// 积微 Cairn 微服务管理平台（Console）——单体二进制入口。
//
// 装配顺序：logx → store → fork 引擎 → adapt 注入 → api → web → http.Server。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/adapt"
	"github.com/jiwei-cairn/cairn-platform/internal/api"
	"github.com/jiwei-cairn/cairn-platform/internal/buildinfo"
	"github.com/jiwei-cairn/cairn-platform/internal/config"
	"github.com/jiwei-cairn/cairn-platform/internal/hostmetrics"
	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/runtime/fork"
	"github.com/jiwei-cairn/cairn-platform/internal/store"
	"github.com/jiwei-cairn/cairn-platform/internal/supervisor"
	"github.com/jiwei-cairn/cairn-platform/internal/web"
)

func main() {
	var (
		root   = flag.String("root", "", "数据根目录（空=自动：可执行文件所在目录 / CAIRN_ROOT / 当前目录）")
		addr   = flag.String("addr", "", "HTTP 监听地址（空=使用设置中的 web.port）")
		dbPath = flag.String("db", "", "SQLite 数据库路径（空=数据根下 data/cairn.db）")
	)
	flag.Parse()

	// 数据根必须最先定型：日志与数据库路径都由它派生。
	// Windows 服务形态无法传标志，故同时支持 CAIRN_ROOT 环境变量与
	// 「可执行文件所在目录」自动锚定（详见 config.InitRoot 文档）。
	config.InitRoot(*root)

	logx.Init(config.Default().LogOptions())
	logger := logx.Module("main")

	dbFile := *dbPath
	if dbFile == "" {
		dbFile = config.DataDir("cairn.db")
	}

	db, err := store.Open(dbFile)
	if err != nil {
		logger.Error("打开数据库失败", "err", err)
		os.Exit(1)
	}
	settings, err := store.OpenSettings(db)
	if err != nil {
		logger.Error("加载平台配置失败", "err", err)
		os.Exit(1)
	}
	logx.Init(settings.Get().LogOptions())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// fork 真引擎装配（登记目录锚定数据根，而非进程 CWD）。
	eng := supervisor.NewEngine(fork.New(config.RuntimeDir()), db)
	server := api.New(db, settings, time.Now())
	server.RuntimeName = "fork"
	server.Supervisor = adapt.SupervisorAdapter{Eng: eng}
	server.Defs = adapt.DefsHook{Eng: eng}
	server.Host = hostmetrics.HostMetrics{}
	// 磁盘探针锚定到数据根实际所在的卷：生产 exec-dir / --root 形态下 CWD
	// 可能是 System32 等无关位置，按 CWD 相对路径取卷会读错盘。
	hostmetrics.SetDataDir(config.DataDir())
	eng.Init()
	// 主机指标采样：供大盘 sparkline（近 1h 趋势）。节奏取设置项，
	// 采样器自身对误配做过下限保护，失败不影响服务监管。
	server.StartHostSampler(settings.Get().Collect.IntervalSec)

	listen := *addr
	if listen == "" {
		// 默认仅绑定回环地址：V1 无认证，裸绑 0.0.0.0 意味着同网段任何主机
		// 都能接管平台（安装/启停/上传）。需对外提供服务时须显式传 -addr，
		// 并在网络层自行收敛访问范围。
		listen = fmt.Sprintf("127.0.0.1:%d", settings.Get().Web.Port)
	}
	httpSrv := &http.Server{Addr: listen, Handler: web.Router(server)}

	// 监听失败经通道回传主流程统一收尾——goroutine 内直接 os.Exit 会跳过
	// 引擎关闭、DB 落盘与日志 flush（端口占用是现场高频错误，必须干净退出）。
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("管理平台启动", "addr", listen, "runtime", "fork", "version", buildinfo.Version)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		stop()
		logger.Info("正在关闭平台…")
	case err := <-serveErr:
		logger.Error("服务异常退出", "err", err)
	}

	// 关闭顺序很重要：必须先停 HTTP 入口，再关引擎与采样器。
	// 反过来的话，10s 优雅窗口内进来的 start/stop/restart 仍会被送进
	// 已无消费者的 cmdCh 缓冲，返回 200 假成功而实际不会执行。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("优雅关闭失败", "err", err)
	}

	eng.Close()
	api.StopHostSampler()
	if err := store.Close(db); err != nil {
		logger.Error("关闭数据库失败", "err", err)
	}
	logger.Info("平台已关闭", "version", buildinfo.Version)
	logx.Shutdown()
}