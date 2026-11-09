// adopt.go — 孤儿认领（平台崩溃恢复，M1 方案 §3.3）。
//
// 平台启动时对每个 enabled 服务读登记并校验后零中断接管为 running：
//   - linux：/proc/{pid}/stat starttime（防 PID 复用）+ /proc/{pid}/exe 前缀匹配
//   - windows：GetProcessTimes 创建时间（防 PID 复用）+ QueryFullProcessImageNameW
//     映像路径匹配
//
// 两侧命中条件一致，均接管为 running；未命中则由编排层按 autoStart 重拉。
package fork

import (
	"errors"
	"time"

	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/runtime"
)

var errAdoptedExit = errors.New("认领进程已不在（探活发现）")

// Adopt 尝试认领一个单元：登记存在且校验通过则接管，返回 adopted。
func (r *Runtime) Adopt(def runtime.UnitDef) bool {
	name := runtime.UnitKeyFor(def.ServiceID)
	if err := r.EnsureUnit(def); err != nil {
		return false
	}
	reg, err := r.loadReg(name)
	if err != nil || reg == nil || reg.PID == 0 {
		return false
	}
	return r.adoptLive(name, def, reg)
}

// startAdoptWatch 认领进程探活：退出即发 UnitExit{Adopted:true}
//（认领进程非子进程无 Wait 事件，2s 对账轮询是唯一感知途径）。
func (r *Runtime) startAdoptWatch(u *unit, name string) {
	go func() {
		t := time.NewTicker(adoptProbeSec)
		defer t.Stop()
		for range t.C {
			if !u.alive() {
				return
			}
			u.mu.Lock()
			pid := u.pid
			u.mu.Unlock()
			if probeAlive(pid) {
				continue
			}
			u.mu.Lock()
			if !u.started {
				u.mu.Unlock()
				return
			}
			u.started = false
			u.exitCode = -1
			svcID := u.def.ServiceID
			u.mu.Unlock()
			logx.Module("fork").Info("认领进程退出", "unit", name, "pid", pid)
			select {
			case r.exits <- runtime.UnitExit{
				ServiceID: svcID, Name: name, ExitCode: -1,
				Err: errAdoptedExit, At: time.Now(), Adopted: true,
			}:
			default:
			}
			return
		}
	}()
}