// policy.go — 服务定义的监管策略解析、默认值回落与差量调谐指纹（架构 §1.4 两档）。
package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jiwei-cairn/cairn-platform/internal/store"
)

// Policy worker 需要的监管参数（从 Service 的 JSON Blob 解析 + 默认值回落）。
type Policy struct {
	// RestartPolicy
	RestartMode  string // periodic（固定周期）| backoff（指数退避）；旧值 always/onFailure 归一为 backoff
	PeriodicSec  int    // periodic 模式：异常退出后固定拉起间隔（默认/最小 3s）
	MaxRetries   int    // 窗口内最大启动失败数（0=不限，熔断配额）
	WindowSec    int    // 滑动窗口长度（0=不限）
	BackoffBase  int    // backoff 模式：退避基数秒（默认 2）
	BackoffMax   int    // backoff 模式：退避封顶秒（默认 300）
	// HealthCheck
	HealthType          string // none | http | tcp
	HealthTarget        string
	IntervalSec         int
	TimeoutSec          int
	ReadyHoldSec        int
	FailuresThreshold   int // L2 连续失败判 degraded
	DegradedRestartSec  int
	RecoverThreshold    int // degraded 恢复需连续成功次数（supervisord 经验，默认 2）
}

// ParsePolicy 解析 Service 定义；缺省字段回落默认值（与 api.defaultPolicyBlobs 同源）。
func ParsePolicy(def store.Service) Policy {
	p := Policy{
		RestartMode: "backoff", PeriodicSec: 3, MaxRetries: 0, WindowSec: 0, BackoffBase: 2, BackoffMax: 300,
		HealthType: "none", IntervalSec: 10, TimeoutSec: 3, ReadyHoldSec: 5,
		FailuresThreshold: 3, DegradedRestartSec: 30, RecoverThreshold: 2,
	}
	var rp struct {
		Mode        string `json:"mode"`
		PeriodicSec int    `json:"periodicSec"`
		MaxRetries  int    `json:"maxRetries"`
		WindowSec   int    `json:"windowSec"`
		BackoffBase int    `json:"backoffBase"`
		BackoffMax  int    `json:"backoffMax"`
	}
	if json.Unmarshal(def.RestartPolicy, &rp) == nil {
		switch rp.Mode {
		case "periodic", "backoff":
			p.RestartMode = rp.Mode
		case "always", "onFailure":
			p.RestartMode = "backoff" // 旧值归一：异常退出均拉起、指数退避
		}
		if rp.PeriodicSec >= 3 {
			p.PeriodicSec = rp.PeriodicSec
		}
		if rp.MaxRetries > 0 {
			p.MaxRetries = rp.MaxRetries
		}
		if rp.WindowSec > 0 {
			p.WindowSec = rp.WindowSec
		}
		if rp.BackoffBase > 0 {
			p.BackoffBase = rp.BackoffBase
		}
		if rp.BackoffMax > 0 {
			p.BackoffMax = rp.BackoffMax
		}
	}
	var hc struct {
		Type               string `json:"type"`
		Target             string `json:"target"`
		IntervalSec        int    `json:"intervalSec"`
		TimeoutSec         int    `json:"timeoutSec"`
		ReadyHoldSec       int    `json:"readyHoldSec"`
		FailuresThreshold  int    `json:"failuresThreshold"`
		RecoverThreshold   int    `json:"recoverThreshold"`
		DegradedRestartSec int    `json:"degradedRestartSec"`
	}
	if json.Unmarshal(def.HealthCheck, &hc) == nil {
		if hc.Type == "http" || hc.Type == "tcp" {
			p.HealthType = hc.Type
		}
		p.HealthTarget = hc.Target
		if hc.IntervalSec > 0 {
			p.IntervalSec = hc.IntervalSec
		}
		if hc.TimeoutSec > 0 {
			p.TimeoutSec = hc.TimeoutSec
		}
		// ReadyHoldSec 显式 0 无法表达（空 JSON 全零会覆盖默认）——
		// "无需稳定期"是罕见场景，>0 才覆盖默认 5。
		if hc.ReadyHoldSec > 0 {
			p.ReadyHoldSec = hc.ReadyHoldSec
		}
		if hc.FailuresThreshold > 0 {
			p.FailuresThreshold = hc.FailuresThreshold
		}
		// RecoverThreshold 原缺 JSON 键、恒为常量 2，degraded 恢复阈值对用户
		// 不可配置（字段形同虚设）。补键后与 FailuresThreshold 一致地按 >0 覆盖。
		if hc.RecoverThreshold > 0 {
			p.RecoverThreshold = hc.RecoverThreshold
		}
		if hc.DegradedRestartSec > 0 {
			p.DegradedRestartSec = hc.DegradedRestartSec
		}
	}
	return p
}

// UnitFingerprint 进程指纹：变化 → 重写期望态并（运行中）重启进程。
// 字段集 = 架构 §1.4：exePath/args/workDir/env/name/autoStart/enabled。
func UnitFingerprint(def store.Service) string {
	h := sha256.New()
	w := func(s string) { _, _ = h.Write([]byte(s)); _, _ = h.Write([]byte{0}) }
	w(def.Name)
	w(def.ExePath)
	w(def.WorkDir)
	var args []string
	_ = json.Unmarshal(def.Args, &args)
	for _, a := range args {
		w(a)
	}
	var env map[string]string
	_ = json.Unmarshal(def.Env, &env)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w(k)
		w(env[k])
	}
	w(fmt.Sprintf("%t", def.AutoStart))
	w(fmt.Sprintf("%t", def.Enabled))
	return hex.EncodeToString(h.Sum(nil))
}

// SuperviseFingerprint 监管参数指纹：变化 → 仅热更新 worker 配置，
// 绝不触发进程重启（现场调探针阈值是高频操作，架构 §1.4 铁律）。
func SuperviseFingerprint(def store.Service) string {
	h := sha256.New()
	_, _ = h.Write(def.RestartPolicy)
	_, _ = h.Write(def.HealthCheck)
	_, _ = h.Write(def.LogPolicy)
	return hex.EncodeToString(h.Sum(nil))
}