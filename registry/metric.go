package registry

import (
	"sort"
	"sync"
	"time"
)

// Kind 区分基础指标（外部喂数）与派生指标（由表达式计算）。
type Kind int

const (
	KindBase Kind = iota
	KindDerived
)

func (k Kind) String() string {
	switch k {
	case KindBase:
		return "base"
	case KindDerived:
		return "derived"
	default:
		return "unknown"
	}
}

// MetricDefinition 是不可变的指标定义版本。
// 一旦提交，任何字段都不会被原地修改；口径变化通过 Register 发布新版本。
type MetricDefinition struct {
	ID         string    `json:"id"`
	Version    int       `json:"version"` // 同一 ID 下从 1 递增
	Kind       Kind      `json:"kind"`
	Formula    string    `json:"formula,omitempty"`    // 派生指标：表达式原文；基础指标为空
	Expr       *Expr     `json:"-"`                    // 派生指标：解析后的 AST（由 Formula 重建）
	Unit       Unit      `json:"unit"`                 // 结果单位
	Dimensions []string  `json:"dimensions,omitempty"` // 业务维度名（已排序去重）
	Retired    bool      `json:"retired,omitempty"`    // 是否已下线
	CreatedAt  time.Time `json:"created_at"`
}

// Inputs 返回派生指标的直接输入标识（按表达式首次出现顺序）；基础指标返回 nil。
func (m *MetricDefinition) Inputs() []string {
	if m == nil || m.Kind != KindDerived || m.Expr == nil {
		return nil
	}
	return m.Expr.Refs()
}

// Snapshot 是某个已提交版本时刻注册表的完整只读视图。
//
// 并发查询拿到的永远是某个完整提交版本：注册事务在内部可变结构上完成
// 全部校验与结构更新后，才会原子替换当前快照指针；快照一经发布不再修改。
// Version 全局单调递增，每次成功注册/下线 +1。
type Snapshot struct {
	Version   int64
	Defs      map[string]*MetricDefinition // id -> 当前版本定义（含已下线，Retired 标记）
	LatestVer map[string]int               // id -> 当前版本号
	// Downstream[id] = 当前版本中直接依赖 id 的派生指标集合（下游邻接表）。
	Downstream map[string]map[string]struct{}
}

// Get 取指标当前版本定义；不存在或已下线（可选）均返回 ErrNotFound。
func (s *Snapshot) Get(id string) (*MetricDefinition, error) {
	if s == nil {
		return nil, ErrNotFound
	}
	d, ok := s.Defs[id]
	if !ok || d.Retired {
		return nil, ErrNotFound
	}
	return d, nil
}

// GetAny 与 Get 类似，但包含已下线指标。
func (s *Snapshot) GetAny(id string) (*MetricDefinition, error) {
	if s == nil {
		return nil, ErrNotFound
	}
	d, ok := s.Defs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return d, nil
}

// IDs 返回当前所有未下线指标标识（排序）。
func (s *Snapshot) IDs() []string {
	var out []string
	for id, d := range s.Defs {
		if !d.Retired {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Lineage 返回 id 的直接输入标识（即直接上游）。
func (s *Snapshot) Lineage(id string) []string {
	d, err := s.Get(id)
	if err != nil {
		return nil
	}
	return d.Inputs()
}

// Impacted 返回 id 发生变更/下线时全部受影响的下游派生指标，
// 包含直接与间接依赖，按拓扑顺序（下游排在其上游之后）返回。
// 结果不包含 id 自身。
func (s *Snapshot) Impacted(id string) []string {
	if s == nil {
		return nil
	}
	if _, ok := s.Defs[id]; !ok {
		return nil
	}
	// 由 id 沿下游边 BFS，收集全部可达节点。
	visited := map[string]bool{id: true}
	queue := []string{id}
	var reachable []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		dn := sortedKeys(s.Downstream[cur])
		for _, d := range dn {
			if !visited[d] {
				visited[d] = true
				reachable = append(reachable, d)
				queue = append(queue, d)
			}
		}
	}
	// 可达集合内做 Kahn 拓扑：边 input -> derived，保证依赖先于被依赖输出。
	// 起点 id 不在集合内，其发出的边不占入度；只统计"集合内节点之间"的边。
	indeg := map[string]int{}
	for _, n := range reachable {
		indeg[n] = 0
	}
	for _, n := range reachable {
		if d, ok := s.Defs[n]; ok {
			for _, in := range d.Inputs() {
				if _, ok := indeg[in]; ok {
					indeg[n]++
				}
			}
		}
	}
	ready := []string{}
	for _, n := range reachable {
		if indeg[n] == 0 {
			ready = append(ready, n)
		}
	}
	sort.Strings(ready)
	var ordered []string
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		ordered = append(ordered, n)
		for _, d := range sortedKeys(s.Downstream[n]) {
			if _, ok := indeg[d]; !ok {
				continue
			}
			indeg[d]--
			if indeg[d] == 0 {
				ready = append(ready, d)
				sort.Strings(ready)
			}
		}
	}
	return ordered
}

// clone 生成快照的深拷贝，供新事务在其上修改。
func (s *Snapshot) clone() *Snapshot {
	cp := &Snapshot{
		Version:    s.Version,
		Defs:       make(map[string]*MetricDefinition, len(s.Defs)),
		LatestVer:  make(map[string]int, len(s.LatestVer)),
		Downstream: make(map[string]map[string]struct{}, len(s.Downstream)),
	}
	for k, v := range s.Defs {
		cp.Defs[k] = v
	}
	for k, v := range s.LatestVer {
		cp.LatestVer[k] = v
	}
	for k, set := range s.Downstream {
		ns := make(map[string]struct{}, len(set))
		for x := range set {
			ns[x] = struct{}{}
		}
		cp.Downstream[k] = ns
	}
	return cp
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RegisterInput 是注册/更新指标定义的请求。
type RegisterInput struct {
	ID         string
	Formula    string // 派生指标必填；基础指标留空表示基础指标
	Unit       Unit
	Dimensions []string
}

// Registry 是注册、血缘、重算的统一入口。
type Registry struct {
	mu    sync.RWMutex
	store store
	units map[string]Unit

	snap    *Snapshot                      // 当前已提交快照（原子替换）
	history map[string][]*MetricDefinition // id -> 全部历史版本（不可变切片）
	compute *computeState
	opts    Options
	dirty   map[string]struct{} // 定义变更导致结果失效的派生指标
	nextSeq int64               // 已恢复/已提交的最大事件序号
	recomp  *recomputeRun       // 进行中的重算（与已提交状态分离）
}
