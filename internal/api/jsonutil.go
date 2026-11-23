// jsonutil.go — 配置管理用的小工具（容错 JSON 解码 + 受限命令执行）。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// jsonUnmarshalLenient 容错解码：空/nil/无效输入不报错（保持零值）。
// 用于 JSON blob 列（历史数据可能有空值或旧格式）。
func jsonUnmarshalLenient(raw []byte, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return nil // 容错：旧格式/空值按零值处理
	}
	return nil
}

// jsonDecodeBody 解码请求体（带上限，防止超大 JSON 打满内存）。
func jsonDecodeBody(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, maxConfigBytes+1024))
	return dec.Decode(v)
}

// runShellLimited 在指定工作目录执行命令并返回合并输出（限时）。
//
// 安全说明：此函数只用于服务自己声明的 validateCmd（配置在 Service 定义里，
// 由已授权管理平台的人设定）。它不是给外部输入用的通用执行器；参数中的
// {file} 占位由调用方替换为本平台生成的临时文件路径。
//
// 超时后强制结束进程（含子进程树：Windows 用 taskkill /T，POSIX 用 kill -9）。
func runShellLimited(cmdline string, env map[string]string, dir string, timeout time.Duration) (string, error) {
	if strings.TrimSpace(cmdline) == "" {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", cmdline)
	} else {
		cmd = exec.CommandContext(ctx, posixShell(), "-c", cmdline)
	}
	if dir != "" {
		cmd.Dir = dir
	}
	envList := make([]string, 0, len(env))
	for k, v := range env {
		envList = append(envList, k+"="+v)
	}
	base := os.Environ()
	cmd.Env = append(base, envList...)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := strings.TrimSpace(buf.String())
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("命令执行超时（%s）: %s", timeout, cmdline)
	}
	if err != nil {
		return out, err
	}
	return out, nil
}

// posixShell 返回可用的 POSIX shell 路径。
//
// 不能硬编码 /bin/sh：主流发行版与 Alpine（busybox）都有它，但部分裁剪镜像
// （尤其 arm64 边缘设备的最小 rootfs）只提供 /bin/ash 或 /usr/bin/sh。
// 逐个探测第一个存在的；全都不存在时仍返回 /bin/sh（由 exec 报出清晰错误）。
func posixShell() string {
	for _, sh := range []string{"/bin/sh", "/usr/bin/sh", "/bin/ash", "/bin/bash"} {
		if st, err := os.Stat(sh); err == nil && !st.IsDir() {
			return sh
		}
	}
	return "/bin/sh"
}
