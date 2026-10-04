package registry

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
)

// 直接自引用循环必须被拒绝。
func TestCycleSelfReference(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, RegisterInput{ID: "a", Unit: Unit{Name: "CNY"}})
	_, err := r.Register(RegisterInput{ID: "x", Formula: "x + a", Unit: Unit{Name: "CNY"}})
	if !errors.Is(err, ErrCycleDetected) {
		t.Fatalf("self ref err = %v, want ErrCycleDetected", err)
	}
}

// 多跳间接循环 a->b->c->a 必须在最后一次注册时被拒绝，且不留中间状态。
func TestCycleIndirect(t *testing.T) {
	r := newTestRegistry(t)
	cny := Unit{Name: "CNY"}
	mustRegister(t, r, RegisterInput{ID: "a", Unit: cny})
	mustRegister(t, r, RegisterInput{ID: "b", Formula: "a * 2", Unit: cny})
	mustRegister(t, r, RegisterInput{ID: "c", Formula: "b * 3", Unit: cny})

	// 当前已提交版本仍是 3 个指标（a,b,c）。
	before := r.Version()
	_, err := r.Register(RegisterInput{ID: "a", Formula: "c + a", Unit: cny})
	if !errors.Is(err, ErrCycleDetected) {
		t.Fatalf("indirect cycle err = %v, want ErrCycleDetected", err)
	}
	if r.Version() != before {
		t.Fatalf("version changed %d -> %d despite rejected registration", before, r.Version())
	}
	// a 仍为基础指标，血缘未被污染。
	if a, _ := r.CurrentSnapshot().Get("a"); a.Kind != KindBase {
		t.Fatalf("a kind = %v after rejected cycle registration", a.Kind)
	}
}

// 环检测错误必须带上环路径。
func TestCycleErrorPath(t *testing.T) {
	r := newTestRegistry(t)
	cny := Unit{Name: "CNY"}
	mustRegister(t, r, RegisterInput{ID: "a", Unit: cny})
	mustRegister(t, r, RegisterInput{ID: "b", Formula: "a", Unit: cny})
	_, err := r.Register(RegisterInput{ID: "a", Formula: "b", Unit: cny})
	var ce *CycleError
	if !errors.As(err, &ce) || len(ce.Path) < 2 {
		t.Fatalf("expected CycleError with path, got %v", err)
	}
}

// 不同物理量纲相加减/相除必须被拒绝。
func TestIncompatibleUnits(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, RegisterInput{ID: "money", Unit: Unit{Name: "CNY"}})
	mustRegister(t, r, RegisterInput{ID: "seconds", Unit: Unit{Name: "s"}})
	mustRegister(t, r, RegisterInput{ID: "cnt", Unit: Unit{Name: "count"}})

	cases := []struct {
		id, formula string
		unit        Unit
	}{
		{"bad_add", "money + seconds", Unit{Name: "CNY"}},
		{"bad_div", "money / cnt", Unit{Name: "CNY"}},
		{"bad_decl", "money / money", Unit{Name: "CNY"}}, // 同量纲相除得到 ratio，声明 currency 不匹配
	}
	for _, c := range cases {
		_, err := r.Register(RegisterInput{ID: c.id, Formula: c.formula, Unit: c.unit})
		if !errors.Is(err, ErrIncompatibleUnit) {
			t.Errorf("case %s: err = %v, want ErrIncompatibleUnit", c.id, err)
		}
	}
}

// 引用未注册指标必须拒绝。
func TestUnknownInput(t *testing.T) {
	r := newTestRegistry(t)
	_, err := r.Register(RegisterInput{ID: "d", Formula: "ghost * 2", Unit: Unit{Name: "CNY"}})
	if !errors.Is(err, ErrUnknownInput) {
		t.Fatalf("err = %v, want ErrUnknownInput", err)
	}
}

// 派生指标维度集合与输入不一致必须拒绝；注册时允许维度顺序不同。
func TestDimensionMismatch(t *testing.T) {
	r := newTestRegistry(t)
	cny := Unit{Name: "CNY"}
	mustRegister(t, r, RegisterInput{ID: "a", Unit: cny, Dimensions: []string{"region"}})
	mustRegister(t, r, RegisterInput{ID: "b", Unit: cny, Dimensions: []string{"region", "channel"}})

	if _, err := r.Register(RegisterInput{
		ID: "d1", Formula: "a + b", Unit: cny, Dimensions: []string{"region"},
	}); !errors.Is(err, ErrDimensionMismatch) {
		t.Fatalf("mismatch err = %v, want ErrDimensionMismatch", err)
	}
	// 顺序不同但集合相同，允许。
	d := mustRegister(t, r, RegisterInput{
		ID: "d2", Formula: "b", Unit: cny, Dimensions: []string{"channel", "region"},
	})
	if len(d.Dimensions) != 2 || d.Dimensions[0] != "channel" {
		t.Fatalf("normalized dims = %v", d.Dimensions)
	}
}

// 可换算单位按明确规则归一：USD 输入与 CNY 指标相加结果以 CNY 表示。
func TestUnitConversionNormalization(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, RegisterInput{ID: "usd_amt", Unit: Unit{Name: "USD"}})
	mustRegister(t, r, RegisterInput{ID: "cny_amt", Unit: Unit{Name: "CNY"}})
	mustRegister(t, r, RegisterInput{ID: "total", Formula: "usd_amt + cny_amt", Unit: Unit{Name: "CNY"}})

	// 10 USD * 7.1 + 5 CNY = 76 CNY
	if _, err := r.PutBaseData("usd_amt", nil, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PutBaseData("cny_amt", nil, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Recompute(false); err != nil {
		t.Fatal(err)
	}
	v, _, ok := r.GetResult("total", nil)
	if !ok || v != 76 {
		t.Fatalf("total = %v ok=%v, want 76", v, ok)
	}

	// 百分比与比例的归一：percent/ratio 同属 ratio 量纲。
	mustRegister(t, r, RegisterInput{ID: "p", Unit: Unit{Name: "percent"}})
	mustRegister(t, r, RegisterInput{ID: "share", Formula: "p", Unit: Unit{Name: "ratio"}})
	if _, err := r.PutBaseData("p", nil, 50); err != nil { // 50% = 0.5
		t.Fatal(err)
	}
	if _, err := r.Recompute(false); err != nil {
		t.Fatal(err)
	}
	if v, _, ok := r.GetResult("share", nil); !ok || v != 0.5 {
		t.Fatalf("share = %v ok=%v, want 0.5", v, ok)
	}
}

// 增量重算：只更新受影响分支；未受影响结果保持不变；增量结果与全量一致。
func TestIncrementalMatchesFull(t *testing.T) {
	r := buildLineageFixture(t)
	feedFixture(t, r, map[string]float64{
		"a": 100, "b": 10, "c": 4,
	})
	if _, err := r.Recompute(false); err != nil {
		t.Fatal(err)
	}

	// 血缘：
	//   x = a + b        (依赖 a,b)
	//   y = c * 2        (只依赖 c)
	//   z = x + a        (依赖 x,a，间接受 a/b 影响)
	// 更新与 y 无关的 a：增量重算应只动 x、z。
	if _, err := r.PutBaseData("a", nil, 200); err != nil {
		t.Fatal(err)
	}
	yBefore, _, _ := r.GetResult("y", nil)
	res, err := r.Recompute(false)
	if err != nil {
		t.Fatal(err)
	}
	got := append([]string(nil), res.Recomputed...)
	sort.Strings(got)
	if fmt.Sprint(got) != "[x z]" {
		t.Fatalf("incremental targets = %v, want [x z]", got)
	}
	yAfter, _, _ := r.GetResult("y", nil)
	if yBefore != yAfter {
		t.Fatalf("unaffected y changed: %v -> %v", yBefore, yAfter)
	}

	// 增量结果的期望值：x=200+10=210，z=210+200=410，y 保持 8。
	inc := snapshotResults(t, r)
	if inc["x"] != 210 || inc["z"] != 410 || inc["y"] != 8 {
		t.Fatalf("incremental results = %+v, want x=210 z=410 y=8", inc)
	}

	// 全量重算后逐指标必须与增量结果一致。
	if _, err := r.Recompute(true); err != nil {
		t.Fatal(err)
	}
	full := snapshotResults(t, r)
	for k, v := range inc {
		if full[k] != v {
			t.Fatalf("incremental/full mismatch at %s: %v vs %v", k, v, full[k])
		}
	}
}

// 定义变更（注册新版本）也应触发对应下游的增量重算。
func TestDefinitionChangeTriggersRecompute(t *testing.T) {
	r := newTestRegistry(t)
	cny := Unit{Name: "CNY"}
	mustRegister(t, r, RegisterInput{ID: "a", Unit: cny})
	mustRegister(t, r, RegisterInput{ID: "x", Formula: "a * 2", Unit: cny})
	if _, err := r.PutBaseData("a", nil, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Recompute(false); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := r.GetResult("x", nil); v != 20 {
		t.Fatalf("x = %v, want 20", v)
	}

	// 发布 x 的新版本口径 a*3。
	mustRegister(t, r, RegisterInput{ID: "x", Formula: "a * 3", Unit: cny})
	// 重算前旧结果不可读（定义已更新，结果失效）。
	if _, _, ok := r.GetResult("x", nil); ok {
		t.Fatal("stale result should be hidden after definition change")
	}
	res, err := r.Recompute(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recomputed) != 1 || res.Recomputed[0] != "x" {
		t.Fatalf("recompute targets = %v, want [x]", res.Recomputed)
	}
	if v, _, _ := r.GetResult("x", nil); v != 30 {
		t.Fatalf("x = %v, want 30", v)
	}
}

// 并发注册与并发查询：查询只能读到完整提交版本，不能读到中间状态。
func TestConcurrentRegisterAndQuery(t *testing.T) {
	r := newTestRegistry(t)
	cny := Unit{Name: "CNY"}

	// 先建一批基础指标。
	const n = 30
	for i := 0; i < n; i++ {
		mustRegister(t, r, RegisterInput{ID: fmt.Sprintf("base_%02d", i), Unit: cny})
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var readerWg sync.WaitGroup

	// 查询者：任意时刻读到的快照必须自洽——
	// 快照版本单调；快照中每个派生指标的输入都存在于同一快照。
	readerWg.Add(1)
	go func() {
		defer readerWg.Done()
		var lastVer int64
		for {
			select {
			case <-stop:
				return
			default:
			}
			s := r.CurrentSnapshot()
			if s.Version < lastVer {
				t.Errorf("snapshot version went backwards: %d -> %d", lastVer, s.Version)
				return
			}
			lastVer = s.Version
			for id, d := range s.Defs {
				if d.Retired {
					continue
				}
				for _, in := range d.Inputs() {
					ind, ok := s.Defs[in]
					if !ok || ind.Retired {
						t.Errorf("snapshot %d: %q references missing/retired input %q", s.Version, id, in)
						return
					}
				}
			}
			runtime.Gosched()
		}
	}()

	// 注册者：并发注册合法派生指标，全部应成功且版本唯一递增。
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("der_%02d", i)
			formula := fmt.Sprintf("base_%02d * 2", i)
			if _, err := r.Register(RegisterInput{ID: id, Formula: formula, Unit: cny}); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(stop)
	readerWg.Wait()
	select {
	case err := <-errCh:
		t.Fatalf("concurrent register: %v", err)
	default:
	}

	s := r.CurrentSnapshot()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("der_%02d", i)
		if _, err := s.Get(id); err != nil {
			t.Fatalf("committed derived %q not visible: %v", id, err)
		}
	}
}

// 并发喂数 + 并发重算：所有更新最终都被纳入；增量结果与全量一致；并发期间无数据竞争。
func TestConcurrentDataAndRecompute(t *testing.T) {
	r := newTestRegistry(t)
	cny := Unit{Name: "CNY"}
	mustRegister(t, r, RegisterInput{ID: "a", Unit: cny, Dimensions: []string{"k"}})
	mustRegister(t, r, RegisterInput{ID: "x", Formula: "a * 2", Unit: cny, Dimensions: []string{"k"}})

	const writers = 8
	const perWriter = 40
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				key := fmt.Sprintf("w%d-%02d", w, j)
				if _, err := r.PutBaseData("a", map[string]string{"k": key}, float64(j+1)); err != nil {
					t.Errorf("put: %v", returnErr(err))
					return
				}
			}
		}(w)
	}
	// 重算者持续跑，可能不断 CAS 重试；不得丢失任何数据更新，也不得崩。
	recomDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-recomDone:
				return
			default:
				_, _ = r.Recompute(false)
				runtime.Gosched()
			}
		}
	}()
	wg.Wait()
	close(recomDone)

	if _, err := r.Recompute(false); err != nil {
		t.Fatal(err)
	}
	inc := map[string]float64{}
	for w := 0; w < writers; w++ {
		for j := 0; j < perWriter; j++ {
			key := fmt.Sprintf("w%d-%02d", w, j)
			v, _, ok := r.GetResult("x", map[string]string{"k": key})
			if !ok {
				t.Fatalf("missing result for key %s (update lost)", key)
			}
			inc[key] = v
		}
	}
	// 全量重算必须逐键与增量结果一致。
	if _, err := r.Recompute(true); err != nil {
		t.Fatal(err)
	}
	for key, want := range inc {
		v, _, ok := r.GetResult("x", map[string]string{"k": key})
		if !ok || v != want {
			t.Fatalf("key %s: full=%v ok=%v, incremental=%v", key, v, ok, want)
		}
		if want != float64(2)*float64(parseKeyIndex(key)) {
			t.Fatalf("key %s: unexpected value %v", key, want)
		}
	}
}

// parseKeyIndex 从 "wX-YY" 键中取出 YY（数据值），仅测试用。
func parseKeyIndex(key string) int {
	var w, j int
	if n, _ := fmt.Sscanf(key, "w%d-%d", &w, &j); n == 2 {
		return j + 1
	}
	return 0
}

func returnErr(err error) error { return err }

// 重启恢复：多次更新后只恢复已提交事件，重放不重复应用，结果可重建。
func TestRestartIdempotentReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	opts := Options{StorePath: path, StoreSync: true}
	r, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	cny := Unit{Name: "CNY"}
	mustRegister(t, r, RegisterInput{ID: "a", Unit: cny})
	mustRegister(t, r, RegisterInput{ID: "x", Formula: "a * 2", Unit: cny})
	for i := 1; i <= 5; i++ {
		if _, err := r.PutBaseData("a", nil, float64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Recompute(false); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// 恢复两次（模拟反复重启）：每次从日志重建，再写入最新数据，
	// 验证事件序号持续递增、重放不重复应用、结果随最新数据正确更新。
	want := 10.0
	for i := 0; i < 2; i++ {
		r2, err := New(opts)
		if err != nil {
			t.Fatalf("restart %d: %v", i, err)
		}
		v, _, ok := r2.GetResult("x", nil)
		if !ok || v != want {
			t.Fatalf("restart %d: x = %v ok=%v, want %v", i, v, ok, want)
		}
		// 恢复后可以继续提交且序号不冲突。
		seq, err := r2.PutBaseData("a", nil, want)
		if err != nil {
			t.Fatalf("post-restart put: %v", err)
		}
		if seq <= 0 {
			t.Fatalf("bad seq %d after recovery", seq)
		}
		// 写入后结果在重算前过期；重算后等于 2*want。
		if _, err := r2.Recompute(false); err != nil {
			t.Fatal(err)
		}
		if err := r2.Close(); err != nil {
			t.Fatal(err)
		}
		want = 2 * want
	}
}

// 未完成的重算不持久化：崩溃后结果层从已提交事件确定性重建。
func TestRecoverySeparatesPendingRecompute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	opts := Options{StorePath: path, StoreSync: true}
	r, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	cny := Unit{Name: "CNY"}
	mustRegister(t, r, RegisterInput{ID: "a", Unit: cny})
	mustRegister(t, r, RegisterInput{ID: "x", Formula: "a * 2", Unit: cny})
	if _, err := r.PutBaseData("a", nil, 21); err != nil {
		t.Fatal(err)
	}
	// 故意不调用 Recompute 即"崩溃"：日志里只有定义与数据事件。
	_ = r.Close()

	r2, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	// 恢复过程自动确定性重建了派生结果。
	if v, _, ok := r2.GetResult("x", nil); !ok || v != 42 {
		t.Fatalf("x = %v ok=%v after recovery, want 42", v, ok)
	}
	// 再来一次增量重算应当是无操作（没有新变更）。
	res, err := r2.Recompute(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recomputed) != 0 {
		t.Fatalf("expected no-op recompute, got %v", res.Recomputed)
	}
}

// 自定义单位注册：重复冲突定义被拒绝。
func TestRegisterUnitConflict(t *testing.T) {
	r := newTestRegistry(t)
	u := Unit{Name: "USD", Dimension: "currency", Scale: 7.1}
	// 与内置默认定义完全一致：允许幂等。
	if err := r.RegisterUnit(u); err != nil {
		t.Fatalf("identical re-register: %v", err)
	}
	// 换算规则变化：拒绝。
	if err := r.RegisterUnit(Unit{Name: "USD", Dimension: "currency", Scale: 8.0}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("conflict unit err = %v, want ErrAlreadyExists", err)
	}
}

// ---------------- fixtures ----------------

func buildLineageFixture(t *testing.T) *Registry {
	t.Helper()
	r := newTestRegistry(t)
	cny := Unit{Name: "CNY"}
	mustRegister(t, r, RegisterInput{ID: "a", Unit: cny})
	mustRegister(t, r, RegisterInput{ID: "b", Unit: cny})
	mustRegister(t, r, RegisterInput{ID: "c", Unit: cny})
	mustRegister(t, r, RegisterInput{ID: "x", Formula: "a + b", Unit: cny})
	mustRegister(t, r, RegisterInput{ID: "y", Formula: "c * 2", Unit: cny})
	mustRegister(t, r, RegisterInput{ID: "z", Formula: "x + a", Unit: cny})
	return r
}

func feedFixture(t *testing.T, r *Registry, vals map[string]float64) {
	t.Helper()
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := r.PutBaseData(k, nil, vals[k]); err != nil {
			t.Fatalf("feed %s: %v", k, err)
		}
	}
}

func snapshotResults(t *testing.T, r *Registry) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, id := range r.CurrentSnapshot().IDs() {
		if v, _, ok := r.GetResult(id, nil); ok {
			out[id] = v
		}
	}
	return out
}

// 崩溃产生的半行日志必须被当作未提交截断；之后仍可继续追加提交。
func TestTornLogTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	opts := Options{StorePath: path, StoreSync: false}
	r, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, r, RegisterInput{ID: "a", Unit: Unit{Name: "CNY"}})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// 手工追加一条完整事件和一条被截断的半行，模拟写到一半崩溃。
	st, err := newFileStore(path, false)
	if err != nil {
		t.Fatal(err)
	}
	before := st.LastSeq()
	if err := st.Append(&event{
		Seq: before + 1, Kind: evRegister.string(), ID: "b", Version: 1,
		Def: &MetricDefinition{ID: "b", Version: 1, Kind: KindBase, Unit: Unit{Name: "CNY", Dimension: "currency", Scale: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	f, err := openAppend(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":99,"kind":"register","id":"ghost"` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	// 重新打开：半行被忽略，b 已恢复，ghost 不存在，且可继续提交不撞序号。
	r2, err := New(opts)
	if err != nil {
		t.Fatalf("open with torn tail: %v", err)
	}
	defer r2.Close()
	if _, err := r2.CurrentSnapshot().Get("b"); err != nil {
		t.Fatalf("committed metric b missing: %v", err)
	}
	if _, err := r2.CurrentSnapshot().Get("ghost"); err == nil {
		t.Fatal("torn event must not be recovered")
	}
	if _, err := r2.Register(RegisterInput{ID: "c", Unit: Unit{Name: "CNY"}}); err != nil {
		t.Fatalf("register after torn tail: %v", err)
	}
}

// 多级血缘：更新最底层基础指标，传播路径上的全部派生都重算，路径外的不动。
func TestMultiHopPropagation(t *testing.T) {
	r := newTestRegistry(t)
	u := Unit{Name: "ratio"}
	// l1 = a ; l2 = l1 ; l3 = l2 ; side = a
	mustRegister(t, r, RegisterInput{ID: "a", Unit: u})
	mustRegister(t, r, RegisterInput{ID: "l1", Formula: "a", Unit: u})
	mustRegister(t, r, RegisterInput{ID: "l2", Formula: "l1", Unit: u})
	mustRegister(t, r, RegisterInput{ID: "l3", Formula: "l2", Unit: u})
	mustRegister(t, r, RegisterInput{ID: "side", Formula: "a", Unit: u})

	if _, err := r.PutBaseData("a", nil, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Recompute(false); err != nil {
		t.Fatal(err)
	}
	// 改 a：l1,l2,l3,side 都受影响（全部是 a 的下游）。
	if _, err := r.PutBaseData("a", nil, 2); err != nil {
		t.Fatal(err)
	}
	res, err := r.Recompute(false)
	if err != nil {
		t.Fatal(err)
	}
	got := append([]string(nil), res.Recomputed...)
	sort.Strings(got)
	if fmt.Sprint(got) != "[l1 l2 l3 side]" {
		t.Fatalf("propagation = %v, want [l1 l2 l3 side]", got)
	}
	for _, id := range []string{"l1", "l2", "l3", "side"} {
		if v, _, ok := r.GetResult(id, nil); !ok || v != 2 {
			t.Fatalf("%s = %v ok=%v, want 2", id, v, ok)
		}
	}
	// 再改一次：side 与链路都仍需重算（它们都直接/间接消费 a）。
	if _, err := r.PutBaseData("a", nil, 3); err != nil {
		t.Fatal(err)
	}
	res, err = r.Recompute(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recomputed) != 4 {
		t.Fatalf("second propagation = %v, want 4 nodes", res.Recomputed)
	}
	// 无新变更时增量重算为空操作。
	res, err = r.Recompute(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recomputed) != 0 {
		t.Fatalf("no-op recompute = %v, want empty", res.Recomputed)
	}
}

// 重算进行中到达的数据变更不能丢：CAS 检测到基线变化后重试，最终结果纳入新数据。
func TestChangeDuringRecomputeIsNotLost(t *testing.T) {
	injected := false
	opts := Options{
		StorePath:           "",
		StoreSync:           true,
		MaxRecomputeRetries: 8,
		BeforeCompute: func() {
			// 仅在第一次基线（数据水位=10）的计算窗口注入一次新数据。
			if !injected {
				injected = true
				if _, err := rPut(t).PutBaseData("a", nil, 11); err != nil {
					t.Errorf("concurrent put during recompute: %v", err)
				}
			}
		},
	}
	r, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	putHook.Store(r)

	cny := Unit{Name: "CNY"}
	mustRegister(t, r, RegisterInput{ID: "a", Unit: cny})
	mustRegister(t, r, RegisterInput{ID: "x", Formula: "a * 2", Unit: cny})
	if _, err := r.PutBaseData("a", nil, 10); err != nil {
		t.Fatal(err)
	}
	res, err := r.Recompute(false)
	if err != nil {
		t.Fatalf("recompute with concurrent change: %v", err)
	}
	// 最终结果必须是 22（纳入了计算期间到达的 11），而不是 20。
	v, _, ok := r.GetResult("x", nil)
	if !ok || v != 22 {
		t.Fatalf("x = %v ok=%v, want 22 (change during recompute lost?)", v, ok)
	}
	if len(res.Recomputed) != 1 || res.Recomputed[0] != "x" {
		t.Fatalf("recomputed = %v, want [x] after retry", res.Recomputed)
	}
}

// 定义变更在重算期间到达同样不能丢，且结果按新口径计算。
func TestDefinitionChangeDuringRecomputeIsNotLost(t *testing.T) {
	injected := false
	opts := Options{
		StorePath:           "",
		MaxRecomputeRetries: 8,
		BeforeCompute: func() {
			if !injected {
				injected = true
				rr := putHook.Load()
				if _, err := rr.Register(RegisterInput{
					ID: "x", Formula: "a * 3", Unit: Unit{Name: "CNY"},
				}); err != nil {
					t.Errorf("concurrent register during recompute: %v", err)
				}
			}
		},
	}
	r, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	putHook.Store(r)
	cny := Unit{Name: "CNY"}
	mustRegister(t, r, RegisterInput{ID: "a", Unit: cny})
	mustRegister(t, r, RegisterInput{ID: "x", Formula: "a * 2", Unit: cny})
	if _, err := r.PutBaseData("a", nil, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Recompute(false); err != nil {
		t.Fatalf("recompute with concurrent definition change: %v", err)
	}
	v, _, ok := r.GetResult("x", nil)
	if !ok || v != 30 {
		t.Fatalf("x = %v ok=%v, want 30 (new definition during recompute lost?)", v, ok)
	}
}
