package registry

import "sort"

// detectCycle 检查：把 newID 按 deps 作为直接输入加入（或更新）当前血缘后是否形成环。
//
// 当前快照 cur 本身无环，因此新环必然经过 newID：从 newID 出发沿"输入"边
// 深度优先遍历，若能再次回到 newID（或遇到遍历栈上的灰色节点）即为环。
// 支持自引用与任意深度的间接循环。
func detectCycle(cur *Snapshot, newID string, deps []string) error {
	// adj(n)：n 的直接输入。新节点用 deps，其余用当前快照定义。
	adj := func(n string) []string {
		if n == newID {
			return deps
		}
		if d, ok := cur.Defs[n]; ok {
			return d.Inputs()
		}
		return nil
	}

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}

	// dfs 返回 true 表示发现环；ring 为环上节点序列（起点出现两次收尾）。
	var dfs func(n string, stack []string) ([]string, bool)
	dfs = func(n string, stack []string) ([]string, bool) {
		color[n] = gray
		stack = append(stack, n)
		for _, in := range adj(n) {
			if in == newID {
				return append(stack, newID), true
			}
			switch color[in] {
			case gray:
				// 理论上只有经过 newID 的环可达；防御性处理。
				idx := indexOf(stack, in)
				if idx >= 0 {
					return append(stack[idx:], in), true
				}
				return append(stack, in), true
			case black:
				continue
			default:
				// 输入尚不存在由注册流程单独报 ErrUnknownInput。
				if _, exists := cur.Defs[in]; !exists {
					continue
				}
				if ring, cyc := dfs(in, stack); cyc {
					return ring, true
				}
			}
		}
		color[n] = black
		return nil, false
	}

	if ring, cyc := dfs(newID, nil); cyc {
		return &CycleError{Path: ring}
	}
	return nil
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// CycleError 携带构成环的节点路径。
type CycleError struct{ Path []string }

func (e *CycleError) Error() string {
	out := "cycle detected in lineage graph: "
	for i, p := range e.Path {
		if i > 0 {
			out += " -> "
		}
		out += p
	}
	return out
}

// Is 支持 errors.Is(err, ErrCycleDetected)。
func (e *CycleError) Is(target error) bool { return target == ErrCycleDetected }

// topoDownstream 从 starts 出发沿下游边传播，返回受影响节点的拓扑顺序，
// 保证派生指标排在其全部上游之后（可安全按序重算）。已下线节点不传播。
func topoDownstream(cur *Snapshot, starts []string) []string {
	indeg := map[string]int{}
	queue := append([]string(nil), starts...)
	seen := map[string]struct{}{}
	for _, s := range starts {
		seen[s] = struct{}{}
	}

	// 收集可达集合并统计"集合内入边"数。
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for d := range cur.Downstream[n] {
			def, ok := cur.Defs[d]
			if !ok || def.Retired {
				continue
			}
			indeg[d]++
			if _, dup := seen[d]; !dup {
				seen[d] = struct{}{}
				queue = append(queue, d)
			}
		}
	}

	// Kahn：初始节点为 starts（它们的变更不依赖集合内其他节点）。
	ready := append([]string(nil), starts...)
	sort.Strings(ready)
	var ordered []string
	done := map[string]struct{}{}
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		if _, ok := done[n]; ok {
			continue
		}
		done[n] = struct{}{}
		ordered = append(ordered, n)

		var dn []string
		for d := range cur.Downstream[n] {
			if _, ok := seen[d]; ok {
				dn = append(dn, d)
			}
		}
		sort.Strings(dn)
		for _, d := range dn {
			indeg[d]--
			if indeg[d] <= 0 {
				ready = append(ready, d)
			}
		}
	}
	return ordered
}
