// Package health — L2 就绪探针（none | http | tcp），接口化便于单测注入。
package health

import (
	"fmt"
	"net"
	"net/http"
	"time"
)

// Prober 探针函数签名：目标 + 超时 → 错误（nil = 通过）。
type Prober func(target string, timeout time.Duration) error

// New 按类型构造探针；none 返回恒通过。
func New(kind string) Prober {
	switch kind {
	case "http":
		return HTTP
	case "tcp":
		return TCP
	default:
		return func(string, time.Duration) error { return nil }
	}
}

// HTTP GET 目标地址，2xx/3xx 视为通过。
func HTTP(target string, timeout time.Duration) error {
	if target == "" {
		return fmt.Errorf("探针目标为空")
	}
	cli := &http.Client{Timeout: timeout}
	resp, err := cli.Get(target)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// TCP 端口连通性。
func TCP(target string, timeout time.Duration) error {
	if target == "" {
		return fmt.Errorf("探针目标为空")
	}
	conn, err := net.DialTimeout("tcp", target, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}