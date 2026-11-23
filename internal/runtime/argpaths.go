// argpaths.go — 启动参数相对路径解析（方案 A）与预检探针。
//
// 背景：决策 P2 让服务工作目录固定为 data/{name}/（数据与产物分离），而随包
// 发行的配置文件留在产物目录 services/{os}/{arch}/{name}/config/ 下。nats-server
// 这类第三方服务的惯例是「配置跟程序走」，启动参数写作 -c ./config/x.conf ——
// 相对 workDir 解析就会找不到（实测报错：open ./config/nats-server.conf:
// The system cannot find the path specified）。
//
// 解析规则（对每个「看起来是路径」的参数）：
//  1. 绝对路径 → 不动
//  2. workDir 下该路径存在 → 保持原样（数据目录优先：服务自写/自改的配置在
//     data/ 下，尊重 P2 的数据留存语义）
//  3. 否则产物目录下存在 → 改写为产物目录下的绝对路径（发行时随包的配置）
//  4. 都不存在 → 保持原样，交由服务自身报错（平台不猜测用户意图）
//
// 「看起来是路径」的判据：以 ./ ../ .\ ..\ 开头，或含路径分隔符。
// 纯选项/数值/URL（-m、8222、nats://host:4222）一律不动。
package runtime

import (
	"os"
	"path/filepath"
	"strings"
)

// ArgRewrite 一次参数路径改写记录（日志与事件用）。
type ArgRewrite struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// looksLikePath 判断一个参数是否「像文件路径」。
func looksLikePath(arg string) bool {
	if arg == "" || strings.HasPrefix(arg, "-") {
		return false
	}
	// 明确以相对路径前缀开头。
	if strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../") ||
		strings.HasPrefix(arg, ".\\") || strings.HasPrefix(arg, "..\\") {
		return true
	}
	// URL 形态（nats://、http://…）不是本地路径。
	if strings.Contains(arg, "://") {
		return false
	}
	// 含分隔符即视为路径。
	return strings.ContainsAny(arg, `/\`)
}

// ResolveArgs 按方案 A 的规则解析参数中的相对路径。
//
//	args    启动参数
//	workDir 服务工作目录（data/{name}），可为空
//	prodDir 产物目录（services/{os}/{arch}/{name}），可为空
//
// 返回解析后的参数副本（不修改入参）与改写记录。任一目录为空时跳过对应规则。
func ResolveArgs(args []string, workDir, prodDir string) ([]string, []ArgRewrite) {
	if len(args) == 0 {
		return args, nil
	}
	var rewrites []ArgRewrite
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		if filepath.IsAbs(a) || !looksLikePath(a) {
			continue
		}
		// 规则 2：workDir 下存在 → 保持原样（数据目录优先）。
		if workDir != "" {
			if _, err := os.Stat(filepath.Join(workDir, filepath.FromSlash(a))); err == nil {
				continue
			}
		}
		// 规则 3：产物目录下存在 → 改写为绝对路径。
		if prodDir != "" {
			cand := filepath.Join(prodDir, filepath.FromSlash(a))
			if _, err := os.Stat(cand); err == nil {
				if abs, err := filepath.Abs(cand); err == nil {
					out[i] = abs
					rewrites = append(rewrites, ArgRewrite{From: a, To: abs})
				}
			}
		}
		// 规则 4：都不存在 → 保持原样（服务自己会报错）。
	}
	return out, rewrites
}

// MissingArgPaths 预检探针：返回参数中「哪条路径在 workDir 与产物目录下都不存在」。
//
// 判据与 ResolveArgs 完全一致（保证「预检说没问题」⇔「运行期能解析到」）。
//
// settleProductDir 为 false 表示安装落盘前调用（产物尚未就位）：此时**不对
// 产物目录下是否存在作断定**——无法区分「包里会带」与「真的没有」，据此报
// 缺失只会误伤首次安装。返回比 true 时更少的条目，绝不会更多。
func MissingArgPaths(args []string, workDir, prodDir string, settleProductDir bool) []string {
	var missing []string
	for _, a := range args {
		if filepath.IsAbs(a) || !looksLikePath(a) {
			continue
		}
		if workDir != "" {
			if _, err := os.Stat(filepath.Join(workDir, filepath.FromSlash(a))); err == nil {
				continue // 数据目录已能解析
			}
		}
		if !settleProductDir {
			continue // 安装前：产物目录未定稿，不据此判负
		}
		if prodDir != "" {
			if _, err := os.Stat(filepath.Join(prodDir, filepath.FromSlash(a))); err == nil {
				continue // 产物目录可解析
			}
		}
		missing = append(missing, a)
	}
	return missing
}

// ServiceRoot 从可执行文件路径反推服务根目录：向上找到名字等于服务名的祖先目录。
//
// 例：services/windows/amd64/nats-server/bin/nats-server.exe + name=nats-server
//
//	→ services/windows/amd64/nats-server
//
// 找不到同名祖先时回退为可执行文件所在目录（尽力而为：至少不比现状更差）。
func ServiceRoot(exePath, name string) string {
	dir := filepath.Dir(exePath)
	for {
		if name != "" && filepath.Base(dir) == name {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir { // 到达卷根
			break
		}
		dir = parent
	}
	return filepath.Dir(exePath)
}
