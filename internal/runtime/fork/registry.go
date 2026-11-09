// registry.go — 受管单元的落盘登记（孤儿认领与平台重启对齐的依据）。
package fork

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/runtime"
)

// regEntry 一个单元的磁盘登记：pid + starttime（防 PID 复用）+ 解析后 exe。
type regEntry struct {
	ServiceID int       `json:"serviceId"`
	Name      string    `json:"name"`
	PID       int       `json:"pid"`
	StartTime string    `json:"startTime"`
	Exe       string    `json:"exe"`
	StartedAt time.Time `json:"startedAt"`
}

func (r *Runtime) regPath(name string) string {
	return filepath.Join(r.root, name+".json")
}

// saveReg 落盘登记（尽力而为：失败仅告警，不阻断监管主链路）。
func (r *Runtime) saveReg(u *unit) {
	u.mu.Lock()
	e := regEntry{
		ServiceID: u.def.ServiceID,
		Name:      runtime.UnitKeyFor(u.def.ServiceID),
		PID:       u.pid,
		StartTime: u.starttime,
		Exe:       resolveExe(u.def.ExePath),
		StartedAt: u.since,
	}
	exe := u.def.ExePath
	u.mu.Unlock()
	_ = e.Name
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(r.root, 0o755); err != nil {
		logx.Module("fork").Warn("登记目录创建失败", "dir", r.root, "err", err)
		return
	}
	if err := os.WriteFile(r.regPath(e.Name), b, 0o644); err != nil {
		logx.Module("fork").Warn("单元登记落盘失败", "unit", e.Name, "exe", exe, "err", err)
	}
}

// loadReg 读取登记；不存在或损坏返回错误（调用方按未登记处理）。
func (r *Runtime) loadReg(name string) (*regEntry, error) {
	b, err := os.ReadFile(r.regPath(name))
	if err != nil {
		return nil, err
	}
	var e regEntry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// clearReg 清除登记（RemoveUnit 时）。
func (r *Runtime) clearReg(name string) { _ = os.Remove(r.regPath(name)) }

// resolveExe 解析符号链接后的绝对路径（current 软链 → 真实版本目录，
// 认领时与 /proc/{pid}/exe 前缀比对用）。
func resolveExe(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		if rp, err2 := filepath.EvalSymlinks(abs); err2 == nil {
			return rp
		}
		return abs
	}
	return p
}