package registry

import (
	"math"
	"testing"

	"github.com/cuixudong74/metric-lineage-registry-go/metric"
)

// chain builds two independent base metrics feeding independent branches that
// share a top metric:
//
//	a -> c -> e
//	b -> d -> e   (e = c + d)
func buildChain(t *testing.T) *Registry {
	t.Helper()
	r := newTestRegistry(t)
	reg := func(d *metric.Definition) {
		t.Helper()
		if _, err := r.Register(d); err != nil {
			t.Fatalf("register %s: %v", d.ID, err)
		}
	}
	reg(&metric.Definition{ID: "a", Kind: metric.KindBase, Unit: "count", Dims: []string{"d"}})
	reg(&metric.Definition{ID: "b", Kind: metric.KindBase, Unit: "count", Dims: []string{"d"}})
	reg(&metric.Definition{ID: "c", Kind: metric.KindDerived, Unit: "count", Dims: []string{"d"},
		Formula: "x", Inputs: map[string]string{"x": "a"}})
	reg(&metric.Definition{ID: "d", Kind: metric.KindDerived, Unit: "count", Dims: []string{"d"},
		Formula: "x", Inputs: map[string]string{"x": "b"}})
	reg(&metric.Definition{ID: "e", Kind: metric.KindDerived, Unit: "count", Dims: []string{"d"},
		Formula: "x+y", Inputs: map[string]string{"x": "c", "y": "d"}})
	if err := r.UpsertBaseData("a", []metric.Point{{DimValues: []string{"1"}, Value: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := r.UpsertBaseData("b", []metric.Point{{DimValues: []string{"1"}, Value: 10}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Recompute(); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestIncrementalRecomputeAffectedOnly(t *testing.T) {
	r := buildChain(t)

	// Initial: c=1, d=10, e=11.
	check := func(id string, want float64) {
		t.Helper()
		m, err := r.Result(id)
		if err != nil {
			t.Fatal(err)
		}
		if got := m[metric.Key([]string{"1"})]; math.Abs(got-want) > 1e-12 {
			t.Fatalf("%s = %v, want %v", id, got, want)
		}
	}
	check("c", 1)
	check("d", 10)
	check("e", 11)

	// Snapshot unaffected maps: changing a must not replace d's result map.
	dBefore, _ := r.Result("d")

	if err := r.UpsertBaseData("a", []metric.Point{{DimValues: []string{"1"}, Value: 5}}); err != nil {
		t.Fatal(err)
	}
	changed, err := r.Recompute()
	if err != nil {
		t.Fatal(err)
	}
	gotChanged := map[string]bool{}
	for _, id := range changed {
		gotChanged[id] = true
	}
	if !gotChanged["c"] || !gotChanged["e"] {
		t.Fatalf("changed = %v, must contain c and e", changed)
	}
	if gotChanged["d"] {
		t.Fatalf("unaffected d was recomputed: %v", changed)
	}

	check("c", 5)
	check("d", 10)
	check("e", 15)

	dAfter, _ := r.Result("d")
	if !sameFloatMap(dBefore, dAfter) {
		t.Fatalf("unaffected d result map was modified: before=%v after=%v", dBefore, dAfter)
	}

	// Nothing dirty after a successful recompute.
	if dirty := r.Dirty(); len(dirty) != 0 {
		t.Fatalf("dirty after recompute: %v", dirty)
	}

	// Recompute with no changes changes nothing.
	again, err := r.Recompute()
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("idle recompute returned %v", again)
	}
}

func TestIncrementalEqualsFull(t *testing.T) {
	r := buildChain(t)

	// Apply a series of updates incrementally ...
	updates := []struct {
		id string
		v  float64
	}{
		{"a", 3}, {"b", 7}, {"a", 8}, {"b", 2},
	}
	for _, u := range updates {
		if err := r.UpsertBaseData(u.id, []metric.Point{{DimValues: []string{"1"}, Value: u.v}}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Recompute(); err != nil {
			t.Fatal(err)
		}
	}
	inc, _ := r.Result("c")
	ind, _ := r.Result("d")
	ine, _ := r.Result("e")

	// ... and compare against a full recomputation over the same inputs.
	full, err := r.eng.Full(r.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for id, incMap := range map[string]map[string]float64{"c": inc, "d": ind, "e": ine} {
		if !sameFloatMap(incMap, full[id]) {
			t.Fatalf("%s incremental %v != full %v", id, incMap, full[id])
		}
	}
}

func TestDefinitionChangeRecompute(t *testing.T) {
	r := buildChain(t)
	// Change c's definition: c = a*2 (new version).
	if _, err := r.Register(&metric.Definition{
		ID: "c", Kind: metric.KindDerived, Unit: "count", Dims: []string{"d"},
		Formula: "x*2", Inputs: map[string]string{"x": "a"},
	}); err != nil {
		t.Fatal(err)
	}
	changed, err := r.Recompute()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, id := range changed {
		got[id] = true
	}
	if !got["c"] || !got["e"] {
		t.Fatalf("definition change recomputed %v, want c,e", changed)
	}
	if got["d"] {
		t.Fatalf("sibling d recomputed: %v", changed)
	}
	c, _ := r.Result("c")
	e, _ := r.Result("e")
	key := metric.Key([]string{"1"})
	if c[key] != 2 { // a=1
		t.Fatalf("c = %v, want 2", c[key])
	}
	if e[key] != 12 { // 2 + d(10)
		t.Fatalf("e = %v, want 12", e[key])
	}
}

func TestRetireImpactAndResults(t *testing.T) {
	r := buildChain(t)
	impact, err := r.Impact("a")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"c": true, "e": true}
	for _, id := range impact {
		if !want[id] {
			t.Fatalf("impact(a) = %v", impact)
		}
	}
	if err := r.Retire("c"); err != nil {
		t.Fatal(err)
	}
	st, err := r.Status("c")
	if err != nil {
		t.Fatal(err)
	}
	if st != metric.StatusRetired {
		t.Fatalf("status = %s", st)
	}
	if _, err := r.Recompute(); err != nil {
		t.Fatal(err)
	}
	if m, _ := r.Result("c"); len(m) != 0 {
		t.Fatalf("retired c still has results %v", m)
	}
	// History preserved.
	vs, err := r.Versions("c")
	if err != nil || len(vs) != 1 {
		t.Fatalf("history after retire: %v %v", vs, err)
	}
}

func TestAutoRecomputeMode(t *testing.T) {
	r, err := New(Config{AutoRecompute: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	must := func(d *metric.Definition) {
		if _, err := r.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	must(&metric.Definition{ID: "a", Kind: metric.KindBase, Unit: "count", Dims: []string{"d"}})
	must(&metric.Definition{ID: "c", Kind: metric.KindDerived, Unit: "count", Dims: []string{"d"},
		Formula: "x", Inputs: map[string]string{"x": "a"}})
	if err := r.UpsertBaseData("a", []metric.Point{{DimValues: []string{"1"}, Value: 42}}); err != nil {
		t.Fatal(err)
	}
	m, err := r.Result("c")
	if err != nil {
		t.Fatal(err)
	}
	if m[metric.Key([]string{"1"})] != 42 {
		t.Fatalf("auto recompute result = %v", m)
	}
	if dirty := r.Dirty(); len(dirty) != 0 {
		t.Fatalf("auto mode left dirty: %v", dirty)
	}
}

func sameFloatMap(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if math.Abs(b[k]-v) > 1e-12 {
			return false
		}
	}
	return true
}

// TestNewDerivedOnDirtyAncestor covers manual (non-auto) mode: register a new
// derived metric while its derived ancestor is already dirty (data changed but
// not yet recomputed).  A single Recompute must refresh everything, including
// unrelated downstream consumers of the stale ancestor, and equal a full run.
func TestNewDerivedOnDirtyAncestor(t *testing.T) {
	r := newTestRegistry(t)
	reg := func(d *metric.Definition) {
		t.Helper()
		if _, err := r.Register(d); err != nil {
			t.Fatalf("register %s: %v", d.ID, err)
		}
	}
	reg(&metric.Definition{ID: "a", Kind: metric.KindBase, Unit: "count", Dims: []string{"d"}})
	reg(&metric.Definition{ID: "b", Kind: metric.KindDerived, Unit: "count", Dims: []string{"d"},
		Formula: "x", Inputs: map[string]string{"x": "a"}})
	reg(&metric.Definition{ID: "c", Kind: metric.KindDerived, Unit: "count", Dims: []string{"d"},
		Formula: "x", Inputs: map[string]string{"x": "b"}})
	if err := r.UpsertBaseData("a", []metric.Point{{DimValues: []string{"1"}, Value: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Recompute(); err != nil {
		t.Fatal(err)
	}
	// Make b,c dirty via a data change without recomputing.
	if err := r.UpsertBaseData("a", []metric.Point{{DimValues: []string{"1"}, Value: 9}}); err != nil {
		t.Fatal(err)
	}
	// Register a brand new metric e consuming b while b is stale.
	reg(&metric.Definition{ID: "e", Kind: metric.KindDerived, Unit: "count", Dims: []string{"d"},
		Formula: "x", Inputs: map[string]string{"x": "b"}})

	changed, err := r.Recompute()
	if err != nil {
		t.Fatal(err)
	}
	ch := map[string]bool{}
	for _, id := range changed {
		ch[id] = true
	}
	for _, id := range []string{"b", "c", "e"} {
		if !ch[id] {
			t.Fatalf("recompute changed %v, missing %s", changed, id)
		}
	}
	for id, want := range map[string]float64{"b": 9, "c": 9, "e": 9} {
		m, err := r.Result(id)
		if err != nil {
			t.Fatal(err)
		}
		if got := m[metric.Key([]string{"1"})]; got != want {
			t.Fatalf("%s = %v, want %v", id, got, want)
		}
	}
	// Must equal a fresh full recomputation.
	full, err := r.eng.Full(r.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range full {
		m, _ := r.Result(id)
		if !sameFloatMap(m, want) {
			t.Fatalf("%s incremental %v != full %v", id, m, want)
		}
	}
}
