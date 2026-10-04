package registry

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// recomputeRun 表示一次进行中的重算。
// 它持有工作副本，未完成前绝不修改已提交的 computeState；
// 进程重启时进行中的 run 直接丢弃（与已提交状态分离，见 store.go 恢复语义）。
type recomputeRun struct {
	baseVersion int64
	work        *computeState
	workSnap    *Snapshot
	pending     []string
	committed   bool
}

// computeState 保存基础数据与派生结果。所有访问由 Registry.mu 保护。
//   - baseData：原始单位的喂数值，重算时按定义单位归一；
//   - results：指标在其声明单位下的结果值；
//   - dataVersion：基础指标最后一次数据更新对应的事件序号；
//   - computedVersion：派生结果所基于的"定义版本 + 上游水位"指纹，
//     指纹未变即无需重算，因此未受影响的结果不会被改动。
type computeState struct {
	baseData        map[string]map[string]float64
	results         map[string]map[string]float64
	keyDims         map[string]map[string]map[string]string // id -> key -> 维度键值
	dataVersion     map[string]int64                        // base: 最后一条数据事件 seq（输入水位）
	baselineVersion map[string]int64                        // base: 上次重算已纳入的数据水位
	defVersion      map[string]int                          // derived: 结果基于的定义版本
	upstreamSig     map[string]string                       // derived: 结果基于的上游结构化签名
}

func newComputeState() *computeState {
	return &computeState{
		baseData:        map[string]map[string]float64{},
		results:         map[string]map[string]float64{},
		keyDims:         map[string]map[string]map[string]string{},
		dataVersion:     map[string]int64{},
		baselineVersion: map[string]int64{},
		defVersion:      map[string]int{},
		upstreamSig:     map[string]string{},
	}
}

func (c *computeState) clone() *computeState {
	cp := newComputeState()
	for id, m := range c.baseData {
		nm := make(map[string]float64, len(m))
		for k, v := range m {
			nm[k] = v
		}
		cp.baseData[id] = nm
	}
	for id, m := range c.results {
		nm := make(map[string]float64, len(m))
		for k, v := range m {
			nm[k] = v
		}
		cp.results[id] = nm
	}
	for id, m := range c.keyDims {
		nm := make(map[string]map[string]string, len(m))
		for k, d := range m {
			nd := make(map[string]string, len(d))
			for dk, dv := range d {
				nd[dk] = dv
			}
			nm[k] = nd
		}
		cp.keyDims[id] = nm
	}
	for k, v := range c.dataVersion {
		cp.dataVersion[k] = v
	}
	for k, v := range c.baselineVersion {
		cp.baselineVersion[k] = v
	}
	for k, v := range c.defVersion {
		cp.defVersion[k] = v
	}
	for k, v := range c.upstreamSig {
		cp.upstreamSig[k] = v
	}
	return cp
}

// dimKey 按 dimNames 顺序拼接维度键值。'\x00' 与 '=' 不出现在规范键里。
func dimKey(dims map[string]string, dimNames []string) string {
	if len(dimNames) == 0 {
		return ""
	}
	var b strings.Builder
	for i, n := range dimNames {
		if i > 0 {
			b.WriteByte(0x1f) // unit separator
		}
		b.WriteString(escapeDim(n))
		b.WriteByte('=')
		b.WriteString(escapeDim(dims[n]))
	}
	return b.String()
}

func escapeDim(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\x1f", "\\u")
	s = strings.ReplaceAll(s, "=", "\\e")
	return s
}

// putBase 写入一条基础数据（原始单位值）。
func (c *computeState) putBase(def *MetricDefinition, dims map[string]string, value float64, seq int64) string {
	key := dimKey(dims, def.Dimensions)
	if c.baseData[def.ID] == nil {
		c.baseData[def.ID] = map[string]float64{}
	}
	c.baseData[def.ID][key] = value
	if c.keyDims[def.ID] == nil {
		c.keyDims[def.ID] = map[string]map[string]string{}
	}
	nd := make(map[string]string, len(def.Dimensions))
	for _, n := range def.Dimensions {
		nd[n] = dims[n]
	}
	c.keyDims[def.ID][key] = nd
	c.dataVersion[def.ID] = seq
	return key
}

// lookupValue 取指标 id 在 keyDims 描述的维度组合下已归一到量纲基准单位的值。
// 派生指标结果以声明单位存储，读取时先归一到 canonical。
func (c *computeState) lookupValue(snap *Snapshot, id string, dims map[string]string) (evalResult, bool, error) {
	def, err := snap.GetAny(id)
	if err != nil {
		return evalResult{}, false, err
	}
	if def.Retired {
		return evalResult{}, false, nil
	}
	key := dimKey(dims, def.Dimensions)
	var raw float64
	var ok bool
	if def.Kind == KindBase {
		m := c.baseData[id]
		raw, ok = m[key]
	} else {
		m := c.results[id]
		raw, ok = m[key]
	}
	if !ok {
		return evalResult{}, false, nil
	}
	return evalResult{value: def.Unit.Canonical(raw), dimension: def.Unit.Dimension}, true, nil
}

// computeDerived 对单个派生指标在给定维度组合下求值：
// 输入值统一归一到各自量纲基准单位；推断量纲必须与声明单位量纲一致；
// 结果再换算回该指标声明的单位。
func (c *computeState) computeDerived(snap *Snapshot, id string, dims map[string]string) (float64, error) {
	def, err := snap.GetAny(id)
	if err != nil {
		return 0, err
	}
	if def.Kind != KindDerived || def.Expr == nil {
		return 0, fmt.Errorf("%w: %q is not a derived metric", ErrInvalidDefinition, id)
	}
	inputs := map[string]evalResult{}
	for _, ref := range def.Inputs() {
		v, ok, err := c.lookupValue(snap, ref, dims)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, fmt.Errorf("%w: input %q of %q has no data at this dimension key",
				ErrUnknownInput, ref, id)
		}
		inputs[ref] = v
	}
	res, err := def.Expr.eval(inputs)
	if err != nil {
		return 0, err
	}
	if res.dimension != def.Unit.Dimension {
		return 0, fmt.Errorf("%w: derived %q formula yields dimension %q but declared unit %q has %q",
			ErrIncompatibleUnit, id, res.dimension, def.Unit.Name, def.Unit.Dimension)
	}
	return def.Unit.FromCanonical(res.value), nil
}

// signature 计算派生指标当前上游的结构化签名（确定性、无哈希碰撞）：
// 自身定义版本 + 每个输入的（定义版本、基础数据水位或派生上游签名）。
// 任一上游口径或数据变化都会改变签名，从而使该指标在增量重算中被识别为脏。
func (c *computeState) signature(snap *Snapshot, id string) string {
	def, ok := snap.Defs[id]
	if !ok {
		return ""
	}
	var b strings.Builder
	b.WriteString("v")
	b.WriteString(strconv.Itoa(def.Version))
	for _, in := range def.Inputs() {
		inDef, ok := snap.Defs[in]
		if !ok {
			continue
		}
		b.WriteString("|")
		b.WriteString(in)
		b.WriteString("@v")
		b.WriteString(strconv.Itoa(inDef.Version))
		b.WriteString("#")
		if inDef.Kind == KindBase {
			b.WriteString(strconv.FormatInt(c.dataVersion[in], 10))
		} else {
			b.WriteString(c.upstreamSig[in])
		}
	}
	return b.String()
}

// keysForDerived 汇总派生指标所有输入上出现过的维度键，还原为维度键值 map。
// 只有所有输入在同一维度键值上都有值时，该组合才可计算。
func (c *computeState) keysForDerived(snap *Snapshot, def *MetricDefinition) []map[string]string {
	union := map[string]map[string]string{}
	refs := def.Inputs()
	if len(refs) == 0 {
		return nil
	}
	// 以第一个输入出现的键集合为基准，交集语义：每个可计算键都必须所有输入齐备。
	baseRef := refs[0]
	baseKeys := c.keyDims[baseRef]
	for key, dims := range baseKeys {
		full := true
		for _, ref := range refs[1:] {
			if _, ok := c.keyDims[ref][key]; !ok {
				// 该输入可能是派生指标，其 keyDims 在全量推进中已注册。
				if _, ok2 := c.results[ref][key]; !ok2 {
					full = false
					break
				}
			}
		}
		if full {
			union[key] = dims
		}
	}
	out := make([]map[string]string, 0, len(union))
	for _, d := range union {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		return dimKey(out[i], def.Dimensions) < dimKey(out[j], def.Dimensions)
	})
	return out
}

// recomputeOne 在工作副本上重算单个派生指标的所有维度键。
// 返回计算出的键数量；无数据的键被删除（旧结果不再有效）。
func (c *computeState) recomputeOne(snap *Snapshot, id string) (int, error) {
	def, err := snap.GetAny(id)
	if err != nil {
		return 0, err
	}
	newVals := map[string]float64{}
	newKeys := map[string]map[string]string{}
	for _, dims := range c.keysForDerived(snap, def) {
		val, err := c.computeDerived(snap, id, dims)
		if err != nil {
			return 0, err
		}
		key := dimKey(dims, def.Dimensions)
		newVals[key] = val
		newKeys[key] = dims
	}
	c.results[id] = newVals
	c.keyDims[id] = newKeys
	c.defVersion[id] = def.Version
	// 拓扑序保证此刻上游派生指标已更新指纹；先记录纳入的基础数据水位，
	// 再计算本指标指纹，使下一轮增量规划能精确识别"没有新变更"。
	for _, in := range def.Inputs() {
		if inDef, ok := snap.Defs[in]; ok && inDef.Kind == KindBase {
			c.baselineVersion[in] = c.dataVersion[in]
		}
	}
	c.upstreamSig[id] = c.signature(snap, id)
	return len(newVals), nil
}

// fullRecompute 按拓扑序全量重算所有派生指标。
// 这是增量重算的正确性基准：任何增量结果都必须与本函数结果一致。
func (c *computeState) fullRecompute(snap *Snapshot) error {
	derived := []string{}
	for id, d := range snap.Defs {
		if d.Kind == KindDerived && !d.Retired {
			derived = append(derived, id)
		}
	}
	order, err := topoOrder(snap, derived)
	if err != nil {
		return err
	}
	for _, id := range order {
		if _, err := c.recomputeOne(snap, id); err != nil {
			return err
		}
	}
	return nil
}

// topoOrder 对给定节点按"输入先于派生"做拓扑排序（Kahn）。
func topoOrder(snap *Snapshot, nodes []string) ([]string, error) {
	set := map[string]struct{}{}
	for _, n := range nodes {
		set[n] = struct{}{}
	}
	indeg := map[string]int{}
	for n := range set {
		def := snap.Defs[n]
		cnt := 0
		for _, in := range def.Inputs() {
			if _, ok := set[in]; ok {
				cnt++
			}
		}
		indeg[n] = cnt
	}
	var ready []string
	for n := range set {
		if indeg[n] == 0 {
			ready = append(ready, n)
		}
	}
	sort.Strings(ready)
	var out []string
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		out = append(out, n)
		for d := range snap.Downstream[n] {
			if _, ok := set[d]; !ok {
				continue
			}
			indeg[d]--
			if indeg[d] == 0 {
				ready = append(ready, d)
				sort.Strings(ready)
			}
		}
	}
	if len(out) != len(set) {
		return nil, &CycleError{}
	}
	return out, nil
}
