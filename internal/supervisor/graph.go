// graph.go — dependsOn DAG：环拒绝、就绪查询、拓扑/逆序与依赖方闭包。
//
// 与 api 层 detectDependsCycle 的分工：API 在保存时拒绝环（写路径防御）；
// 本图在引擎装配时再校验一次（读路径兜底，含历史脏数据），有环的边被
// 忽略并告警，绝不死锁（supervisord process_sort.go 的环死循环是反面教材）。
package supervisor

import (
	"encoding/json"
	"fmt"

	"github.com/jiwei-cairn/cairn-platform/internal/logx"
	"github.com/jiwei-cairn/cairn-platform/internal/store"
)

// Graph 依赖图（id → 依赖的 id 列表）。
type Graph struct {
	deps map[int][]int
}

// BuildGraph 从服务定义构建 DAG；忽略环边与悬空引用并告警。
func BuildGraph(defs []store.Service) *Graph {
	g := &Graph{deps: make(map[int][]int, len(defs))}
	ids := make(map[int]bool, len(defs))
	for _, d := range defs {
		ids[d.ID] = true
	}
	for _, d := range defs {
		var list []int
		_ = json.Unmarshal(d.DependsOn, &list)
		for _, dep := range list {
			if dep == d.ID {
				logx.Module("supervisor").Warn("忽略自依赖", "id", d.ID, "name", d.Name)
				continue
			}
			if !ids[dep] {
				logx.Module("supervisor").Warn("忽略悬空依赖引用", "id", d.ID, "dep", dep)
				continue
			}
			if path := g.detectCycle(d.ID, dep, ids); path != "" {
				logx.Module("supervisor").Warn("忽略环形依赖边", "from", d.ID, "to", dep, "cycle", path)
				continue
			}
			g.deps[d.ID] = append(g.deps[d.ID], dep)
		}
	}
	return g
}

// detectCycle 判断追加 from→to 是否成环：从 to 出发能否回到 from。
func (g *Graph) detectCycle(from, to int, ids map[int]bool) string {
	seen := map[int]bool{}
	var walk func(n int, path []int) string
	walk = func(n int, path []int) string {
		if n == from {
			p := ""
			for _, x := range append(path, from) {
				if p != "" {
					p += " → "
				}
				p += fmt.Sprintf("#%d", x)
			}
			return p
		}
		if seen[n] {
			return ""
		}
		seen[n] = true
		for _, next := range g.deps[n] {
			if p := walk(next, append(path, n)); p != "" {
				return p
			}
		}
		return ""
	}
	return walk(to, nil)
}

// Deps 返回 id 的直接依赖。
func (g *Graph) Deps(id int) []int { return g.deps[id] }

// Dependents 返回直接依赖 id 的服务（反向边）。
func (g *Graph) Dependents(id int) []int {
	var out []int
	for who, deps := range g.deps {
		for _, d := range deps {
			if d == id {
				out = append(out, who)
				break
			}
		}
	}
	return out
}

// DependentsClosure 反向闭包：所有直接/间接依赖 id 的服务（停止时逆序传播的对象）。
func (g *Graph) DependentsClosure(id int) []int {
	seen := map[int]bool{}
	out := []int{}
	var walk func(n int)
	walk = func(n int) {
		for _, who := range g.Dependents(n) {
			if !seen[who] {
				seen[who] = true
				out = append(out, who)
				walk(who)
			}
		}
	}
	walk(id)
	return out
}

// TopoOrder 拓扑序（依赖在前；同级按输入顺序稳定输出）。
func (g *Graph) TopoOrder(defs []store.Service) []int {
	order := make([]int, 0, len(defs))
	done := map[int]bool{}
	var emit func(id int)
	emit = func(id int) {
		if done[id] {
			return
		}
		done[id] = true
		for _, dep := range g.deps[id] {
			emit(dep)
		}
		order = append(order, id)
	}
	for _, d := range defs {
		emit(d.ID)
	}
	return order
}

// Ready 依赖就绪查询：全部依赖处于 running（由 isReady 回调判定）。
func (g *Graph) Ready(id int, isRunning func(int) bool) bool {
	for _, dep := range g.deps[id] {
		if !isRunning(dep) {
			return false
		}
	}
	return true
}