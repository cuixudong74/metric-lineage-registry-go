package registry

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Options 是注册表的持久化与重算配置项。
type Options struct {
	// StorePath 为事件日志（JSON Lines）路径；留空表示纯内存模式（重启不恢复）。
	StorePath string
	// StoreSync 为 true 时每个已提交事件 fsync 后才返回成功（崩溃不丢提交）。
	StoreSync bool
	// MaxRecomputeRetries 限制并发冲突时重算的最大重试次数（默认 64）。
	MaxRecomputeRetries int
	// Clock 用于定义版本时间戳，测试可注入。
	Clock func() time.Time
	// BeforeCompute 仅用于测试：在重算取到基线快照、实际计算前回调，
	// 可用来制造"计算期间到达的并发变更"，验证 CAS 重试不丢变更。
	BeforeCompute func()
}

// DefaultOptions 返回默认配置：内存存储、fsync、64 次重试。
func DefaultOptions() Options {
	return Options{
		StorePath:           "",
		StoreSync:           true,
		MaxRecomputeRetries: 64,
		Clock:               time.Now,
	}
}

var idPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// RecomputeResult 是一次重算的结果摘要。
type RecomputeResult struct {
	// Recomputed 本次实际重算（重写结果）的派生指标，拓扑序。
	Recomputed []string
	// Skipped 未受影响、结果保持原样的派生指标。
	Skipped []string
	// Version 重算基于的注册表提交版本号。
	Version int64
	// Elapsed 重算耗时。
	Elapsed time.Duration
}

// New 创建注册表；opts.StorePath 非空时从事件日志恢复全部已提交状态。
//
// 恢复语义：
//   - 定义、版本号、血缘、基础数据通过重放事件逐条还原；
//   - 事件序号连续、半行日志视为未提交直接截断；
//   - 派生结果不持久化，恢复后按最终快照做一次全量重算（确定性重建），
//     因此"未完成的重算"天然与已提交状态分离，不会重复应用或漏掉变更；
//   - 默认单位与用户自定义单位不同，不写事件日志（可通过 RegisterUnit 扩展）。
func New(opts Options) (*Registry, error) {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.MaxRecomputeRetries <= 0 {
		opts.MaxRecomputeRetries = 64
	}
	st, err := newFileStore(opts.StorePath, opts.StoreSync)
	if err != nil {
		return nil, err
	}
	r := &Registry{
		store:   st,
		units:   map[string]Unit{},
		snap:    &Snapshot{Defs: map[string]*MetricDefinition{}, LatestVer: map[string]int{}, Downstream: map[string]map[string]struct{}{}},
		history: map[string][]*MetricDefinition{},
		compute: newComputeState(),
		opts:    opts,
	}
	for _, u := range defaultUnits {
		r.units[u.Name] = u
	}
	// 重放已提交事件。apply* 不做二次持久化。
	var replayErr error
	err = st.Replay(func(ev *event) error {
		switch ev.Kind {
		case "register":
			if ev.Def == nil {
				return fmt.Errorf("%w: register event %d missing definition", ErrStore, ev.Seq)
			}
			if err := r.restoreDefinition(ev.Def); err != nil {
				return err
			}
			r.snap.Version++
			r.nextSeq = ev.Seq
		case "retire":
			if err := r.applyRetire(ev.ID, ev.Version, opts.Clock()); err != nil {
				return err
			}
			r.snap.Version++
			r.nextSeq = ev.Seq
		case "data":
			def, ok := r.snap.Defs[ev.ID]
			if !ok {
				return fmt.Errorf("%w: data event %d for unknown metric %q", ErrStore, ev.Seq, ev.ID)
			}
			r.compute.putBase(def, ev.Dims, ev.Value, ev.Seq)
			r.nextSeq = ev.Seq
		default:
			return fmt.Errorf("%w: unknown event kind %q at seq %d", ErrStore, ev.Kind, ev.Seq)
		}
		return nil
	})
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	if replayErr != nil {
		_ = st.Close()
		return nil, replayErr
	}
	// 结果层确定性重建：只依赖已提交的定义与基础数据。
	if err := r.compute.fullRecompute(r.snap); err != nil {
		_ = st.Close()
		return nil, err
	}
	return r, nil
}

// Close 刷新并关闭持久化存储。
func (r *Registry) Close() error { return r.store.Close() }

// CurrentSnapshot 返回当前已提交版本的完整只读快照（并发安全）。
// 返回后即使注册表继续变更，该快照内容也不会被修改。
func (r *Registry) CurrentSnapshot() *Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snap
}

// Version 返回当前提交版本号。
func (r *Registry) Version() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snap.Version
}

// normalizeDims 排序去重维度名并校验非空。
func normalizeDims(dims []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(dims))
	for _, d := range dims {
		if d == "" {
			return nil, fmt.Errorf("%w: dimension name is empty", ErrInvalidDefinition)
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	sort.Strings(out)
	return out, nil
}

// resolveUnit 解析单位：必须已注册且 Scale 有效。
func (r *Registry) resolveUnit(u Unit) (Unit, error) {
	r.mu.RLock()
	registered, ok := r.units[u.Name]
	r.mu.RUnlock()
	if !ok {
		return Unit{}, fmt.Errorf("%w: unit %q is not registered", ErrIncompatibleUnit, u.Name)
	}
	if u.Dimension != "" && u.Dimension != registered.Dimension {
		return Unit{}, fmt.Errorf("%w: unit %q dimension declared %q but registered %q",
			ErrIncompatibleUnit, u.Name, u.Dimension, registered.Dimension)
	}
	return registered, nil
}

// Register 注册指标或发布新版本（线程安全）。
//
// 语义保证：
//   - 相同 ID 若口径/单位/维度与当前版本完全一致，拒绝（ErrAlreadyExists），不产生版本；
//   - 任一属性变化则生成不可变新版本，历史版本可通过 History/GetVersion 查询；
//   - 派生表达式引用不存在的指标、量纲不兼容、维度对不上、形成（含间接）环，均拒绝；
//   - 拒绝发生在任何状态与事件写入之前，外部不会观察到中间状态。
func (r *Registry) Register(in RegisterInput) (*MetricDefinition, error) {
	if !idPattern.MatchString(in.ID) {
		return nil, fmt.Errorf("%w: id %q must match %s",
			ErrInvalidDefinition, in.ID, idPattern.String())
	}
	dims, err := normalizeDims(in.Dimensions)
	if err != nil {
		return nil, err
	}
	unit, err := r.resolveUnit(in.Unit)
	if err != nil {
		return nil, err
	}
	var expr *Expr
	kind := KindBase
	if strings.TrimSpace(in.Formula) != "" {
		kind = KindDerived
		expr, err = ParseExpr(in.Formula)
		if err != nil {
			return nil, err
		}
	}

	// 构造新定义草案；随后在锁内做依赖快照的全部校验。
	r.mu.Lock()
	defer r.mu.Unlock()

	cur := r.snap
	existing, exists := cur.Defs[in.ID]
	if exists {
		// 已下线的 ID 不可复活，避免历史血缘语义被复用标识污染。
		if existing.Retired {
			return nil, fmt.Errorf("%w: %q has been retired; register a new id instead",
				ErrInvalidDefinition, in.ID)
		}
		// 派生指标不可降级为基础指标（其值只能由表达式产生）；
		// 基础指标允许升级为派生指标，环检测会拦截因此引入的循环。
		if existing.Kind == KindDerived && kind == KindBase {
			return nil, fmt.Errorf("%w: %q cannot change from derived to base",
				ErrInvalidDefinition, in.ID)
		}
		if kind == KindBase && existing.Unit == unit && equalStrings(existing.Dimensions, dims) {
			return nil, fmt.Errorf("%w: %q identical to version %d",
				ErrAlreadyExists, in.ID, existing.Version)
		}
		if kind == KindDerived && existing.Formula == in.Formula &&
			existing.Unit == unit && equalStrings(existing.Dimensions, dims) {
			return nil, fmt.Errorf("%w: %q identical to version %d",
				ErrAlreadyExists, in.ID, existing.Version)
		}
	}

	// 派生指标：引用、维度、量纲、环，全部基于当前已提交快照校验。
	if kind == KindDerived {
		if err := r.validateDerived(cur, in.ID, expr, unit, dims); err != nil {
			return nil, err
		}
	}

	version := 1
	if existing != nil {
		version = existing.Version + 1
	}
	def := &MetricDefinition{
		ID:         in.ID,
		Version:    version,
		Kind:       kind,
		Formula:    in.Formula,
		Expr:       expr,
		Unit:       unit,
		Dimensions: dims,
		CreatedAt:  r.opts.Clock(),
	}

	// 先持久化，再切换内存状态：持久化失败则调用方观察不到本次注册。
	r.nextSeq++
	ev := &event{Seq: r.nextSeq, Kind: evRegister.string(), ID: def.ID, Version: def.Version, Def: def}
	if err := r.store.Append(ev); err != nil {
		r.nextSeq--
		return nil, err
	}
	r.applyDefinition(def)
	r.snap.Version++
	return def, nil
}

// validateDerived 在当前快照上校验派生定义：引用存在、维度集合一致、量纲兼容、无环。
func (r *Registry) validateDerived(cur *Snapshot, id string, expr *Expr, unit Unit, dims []string) error {
	refs := expr.Refs()
	if len(refs) == 0 {
		return fmt.Errorf("%w: derived metric %q must reference at least one input",
			ErrInvalidDefinition, id)
	}
	refDims := map[string]string{}
	for _, ref := range refs {
		if ref == id {
			// 自引用在下方环检测中处理，这里先占位，避免被当作"未知输入"提前报错。
			if cur, ok := cur.Defs[id]; ok {
				refDims[ref] = cur.Unit.Dimension
			} else {
				refDims[ref] = unit.Dimension
			}
			continue
		}
		d, err := cur.Get(ref)
		if err != nil {
			return fmt.Errorf("%w: input %q referenced by %q", ErrUnknownInput, ref, id)
		}
		// 维度必须对得上：派生指标的维度集合与每个输入完全一致。
		// 这样同一维度键在整条血缘上语义一致，重算才能按键连接。
		if !equalStrings(d.Dimensions, dims) {
			return fmt.Errorf("%w: %q dimensions %v do not match input %q dimensions %v",
				ErrDimensionMismatch, id, dims, ref, d.Dimensions)
		}
		refDims[ref] = d.Unit.Dimension
	}
	inferred, err := expr.inferDim(refDims)
	if err != nil {
		return err
	}
	if inferred != unit.Dimension {
		return fmt.Errorf("%w: %q formula dimension %q conflicts with declared unit %q dimension %q",
			ErrIncompatibleUnit, id, inferred, unit.Name, unit.Dimension)
	}
	// 自引用与任意深度的间接环。
	if err := detectCycle(cur, id, refs); err != nil {
		return err
	}
	return nil
}

// applyDefinition 把已验证的定义版本应用到快照与历史（纯内存结构变更）。
func (r *Registry) applyDefinition(def *MetricDefinition) {
	next := r.snap.clone()
	old := next.Defs[def.ID]
	next.Defs[def.ID] = def
	next.LatestVer[def.ID] = def.Version
	// 维护下游邻接表：先移除旧版本的边，再加入新版本的边。
	if old != nil {
		for _, in := range old.Inputs() {
			delete(next.Downstream[in], def.ID)
		}
	}
	for _, in := range def.Inputs() {
		if next.Downstream[in] == nil {
			next.Downstream[in] = map[string]struct{}{}
		}
		next.Downstream[in][def.ID] = struct{}{}
	}
	r.history[def.ID] = append(r.history[def.ID], def)
	r.snap = next
}

// restoreDefinition 重放 register 事件时使用：定义已在日志中，直接重建结构。
func (r *Registry) restoreDefinition(def *MetricDefinition) error {
	if def.Expr == nil && def.Kind == KindDerived && def.Formula != "" {
		expr, err := ParseExpr(def.Formula)
		if err != nil {
			return fmt.Errorf("%w: corrupt log for %q v%d: %v",
				ErrStore, def.ID, def.Version, err)
		}
		def.Expr = expr
	}
	// 注册时单位来自单位表；恢复时以落盘定义为准，同时确保单位表包含它。
	if _, ok := r.units[def.Unit.Name]; !ok {
		r.units[def.Unit.Name] = def.Unit
	}
	r.applyDefinition(def)
	return nil
}

// Retire 下线指标。仍有未下线下游派生指标直接/间接依赖时拒绝（ErrDependentStillActive）。
// 下线本身也是一个不可变版本（Retired=true），并持久化。
func (r *Registry) Retire(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.snap
	def, ok := cur.Defs[id]
	if !ok || def.Retired {
		return ErrNotFound
	}
	if impacted := cur.Impacted(id); len(impacted) > 0 {
		return fmt.Errorf("%w: %q -> %v", ErrDependentStillActive, id, impacted)
	}
	r.nextSeq++
	ev := &event{Seq: r.nextSeq, Kind: evRetire.string(), ID: id, Version: def.Version + 1}
	if err := r.store.Append(ev); err != nil {
		r.nextSeq--
		return err
	}
	return r.applyRetire(id, def.Version+1, r.opts.Clock())
}

func (r *Registry) applyRetire(id string, version int, at time.Time) error {
	cur := r.snap
	def, ok := cur.Defs[id]
	if !ok {
		return ErrNotFound
	}
	retired := *def
	retired.Version = version
	retired.Retired = true
	retired.CreatedAt = at
	next := cur.clone()
	next.Defs[id] = &retired
	next.LatestVer[id] = version
	for _, in := range def.Inputs() {
		delete(next.Downstream[in], id)
	}
	r.history[id] = append(r.history[id], &retired)
	r.snap = next
	// 清理结果与数据。
	delete(r.compute.results, id)
	delete(r.compute.baseData, id)
	delete(r.compute.keyDims, id)
	delete(r.compute.dataVersion, id)
	delete(r.compute.baselineVersion, id)
	delete(r.compute.upstreamSig, id)
	delete(r.compute.defVersion, id)
	return nil
}

// GetVersion 查询指标的指定历史版本。历史版本不可变。
func (r *Registry) GetVersion(id string, version int) (*MetricDefinition, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, d := range r.history[id] {
		if d.Version == version {
			return d, nil
		}
	}
	return nil, fmt.Errorf("%w: %q v%d", ErrNotFound, id, version)
}

// History 返回指标全部历史版本（按版本号升序）。
func (r *Registry) History(id string) ([]*MetricDefinition, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	hs, ok := r.history[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	out := make([]*MetricDefinition, len(hs))
	copy(out, hs)
	return out, nil
}

// PutBaseData 写入/更新基础指标的一条数据（线程安全）。
// value 以该指标注册单位的原始数值给出；重算时统一归一。返回数据版本（事件序号）。
// 数据更新在 Recompute 期间到达也不会丢：它已持久化并体现在水位指纹中，
// 若未被进行中的重算纳入，下次（或本次重试）必然重算其下游。
func (r *Registry) PutBaseData(id string, dims map[string]string, value float64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	def, err := r.snap.Get(id)
	if err != nil {
		return 0, err
	}
	if def.Kind != KindBase {
		return 0, fmt.Errorf("%w: %q is derived; results are computed, not fed",
			ErrInvalidDefinition, id)
	}
	for _, d := range def.Dimensions {
		if _, ok := dims[d]; !ok {
			return 0, fmt.Errorf("%w: data for %q missing dimension %q",
				ErrDimensionMismatch, id, d)
		}
	}
	r.nextSeq++
	ev := &event{
		Seq:     r.nextSeq,
		Kind:    evData.string(),
		ID:      id,
		DataVer: r.nextSeq,
		Dims:    copyDims(dims, def.Dimensions),
		Value:   value,
	}
	if err := r.store.Append(ev); err != nil {
		r.nextSeq--
		return 0, err
	}
	r.compute.putBase(def, ev.Dims, value, r.nextSeq)
	return r.nextSeq, nil
}

func copyDims(dims map[string]string, names []string) map[string]string {
	out := make(map[string]string, len(names))
	for _, n := range names {
		out[n] = dims[n]
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Recompute 基于血缘做增量重算（线程安全），并保证与全量重算结果逐键一致。
//
// 增量判定：以每个指标记录的"定义版本 + 上游水位指纹"与当前快照比对，
// 找出脏的基础变更源（数据水位落后/定义更新）与脏派生节点，再沿下游边传播，
// 得到受影响集合。集合之外的指标结果绝不会被写入或改动。
//
// 并发语义：计算在快照与状态的工作副本上进行，期间注册/下线/喂数照常提交，
// 不阻塞查询；提交工作副本时若发现基线已落后，则基于最新状态重试，
// 因此重算期间到达的定义/数据变更不会丢失（被新一轮纳入）。
// force=true 时等价于全量重算。
func (r *Registry) Recompute(force bool) (*RecomputeResult, error) {
	start := time.Now()
	for attempt := 0; attempt < r.opts.MaxRecomputeRetries; attempt++ {
		// 阶段一：在锁内取一致基线（快照 + 状态副本），确定待重算集合。
		r.mu.Lock()
		baseSnap := r.snap
		baseSeq := r.nextSeq
		work := r.compute.clone()
		r.mu.Unlock()

		targets, skipped, err := planRecompute(baseSnap, work, force)
		if err != nil {
			return nil, err
		}

		// 测试钩子：模拟计算进行中外部提交了新变更。
		if r.opts.BeforeCompute != nil {
			r.opts.BeforeCompute()
		}

		// 阶段二：锁外在工作副本上做实际计算（耗时操作不阻塞注册与查询）。
		var recomputed []string
		for _, id := range targets {
			if _, err := work.recomputeOne(baseSnap, id); err != nil {
				return nil, err
			}
			recomputed = append(recomputed, id)
		}

		// 阶段三：CAS 提交。基线未变则原子替换结果层；否则带最新变更重试。
		r.mu.Lock()
		if r.snap == baseSnap && r.nextSeq == baseSeq {
			r.compute = work
			res := &RecomputeResult{
				Recomputed: recomputed,
				Skipped:    skipped,
				Version:    baseSnap.Version,
				Elapsed:    time.Since(start),
			}
			r.mu.Unlock()
			return res, nil
		}
		r.mu.Unlock()
		// 基线变化：丢弃工作副本，下一轮基于最新已提交状态重新规划。
	}
	return nil, fmt.Errorf("%w: recompute exceeded %d retries under contention",
		ErrStore, r.opts.MaxRecomputeRetries)
}

// planRecompute 计算需要重算的派生指标集合（拓扑序）以及无需重算的指标。
func planRecompute(snap *Snapshot, c *computeState, force bool) (targets, skipped []string, err error) {
	if force {
		var all []string
		for id, d := range snap.Defs {
			if d.Kind == KindDerived && !d.Retired {
				all = append(all, id)
			}
		}
		targets, err = topoOrder(snap, all)
		return targets, nil, err
	}

	// 脏源：基础指标的数据水位高于上次重算已纳入的基线；
	// 或派生指标的定义版本/上游指纹与已计算结果不一致。
	var roots []string
	for id, d := range snap.Defs {
		if d.Retired {
			continue
		}
		if d.Kind == KindBase {
			if c.dataVersion[id] != c.baselineVersion[id] {
				roots = append(roots, id)
			}
		} else if c.defVersion[id] != d.Version || c.upstreamSig[id] != c.signature(snap, id) {
			roots = append(roots, id)
		}
	}
	sort.Strings(roots)

	// 沿下游边传播；传播集合中的派生节点即受影响集合。
	propagated := topoDownstream(snap, roots)
	propSet := map[string]struct{}{}
	for _, p := range propagated {
		propSet[p] = struct{}{}
	}
	// roots 里的脏派生节点也要包含（topoDownstream 从 base 根未必覆盖它们）。
	var all []string
	for id, d := range snap.Defs {
		if d.Kind != KindDerived || d.Retired {
			continue
		}
		_, inSet := propSet[id]
		dirty := c.defVersion[id] != d.Version || c.upstreamSig[id] != c.signature(snap, id)
		if inSet || dirty {
			all = append(all, id)
		} else {
			skipped = append(skipped, id)
		}
	}
	targets, err = topoOrder(snap, all)
	sort.Strings(skipped)
	return targets, skipped, err
}

// GetResult 读取指标在维度键值下的结果值（声明单位）、结果所基于的快照版本，
// 以及是否存在。派生指标若尚未重算则 ok=false。
func (r *Registry) GetResult(id string, dims map[string]string) (value float64, snapVersion int64, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, err := r.snap.Get(id)
	if err != nil {
		return 0, r.snap.Version, false
	}
	key := dimKey(dims, def.Dimensions)
	// 派生结果必须与当前定义版本及上游水位一致；上游数据或定义一旦变更，
	// 旧结果在重算前视为不存在，避免调用方读到与当前已提交定义不一致的值。
	c := r.compute
	if def.Kind == KindDerived {
		if c.defVersion[id] != def.Version || c.upstreamSig[id] != c.signature(r.snap, id) {
			return 0, r.snap.Version, false
		}
	}
	var m map[string]float64
	if def.Kind == KindBase {
		m = r.compute.baseData[id]
	} else {
		m = r.compute.results[id]
	}
	v, exists := m[key]
	if !exists {
		return 0, r.snap.Version, false
	}
	return v, r.snap.Version, true
}
