package compute

import (
	"math"
	"reflect"
	"testing"

	"github.com/cuixudong74/metric-lineage-registry-go/lineage"
	"github.com/cuixudong74/metric-lineage-registry-go/metric"
	"github.com/cuixudong74/metric-lineage-registry-go/unit"
)

func derivedDef(id, u, formula string, dims []string, ins map[string]string) *metric.Definition {
	return &metric.Definition{ID: id, Kind: metric.KindDerived, Unit: u,
		Dims: dims, Formula: formula, Inputs: ins}
}

func TestProjectionRollupAndInnerJoin(t *testing.T) {
	units := unit.NewSystem()
	eng := NewEngine(units)

	// sales[region,product] -> rollup to region; target[region] inner join.
	salesDef := &metric.Definition{ID: "sales", Kind: metric.KindBase, Unit: "count", Dims: []string{"region", "product"}}
	targetDef := &metric.Definition{ID: "target", Kind: metric.KindBase, Unit: "count", Dims: []string{"region"}}
	defs := map[string]*metric.Definition{"sales": salesDef, "target": targetDef}
	defs["ratio"] = derivedDef("ratio", "ratio", "a/b", []string{"region"}, map[string]string{"a": "sales", "b": "target"})

	states := map[string]*metric.State{
		"sales": {Def: salesDef, Status: metric.StatusActive, Data: map[string]float64{
			metric.Key([]string{"CN", "p1"}): 30,
			metric.Key([]string{"CN", "p2"}): 20, // rolls up CN -> 50
			metric.Key([]string{"US", "p1"}): 40,
		}},
		"target": {Def: targetDef, Status: metric.StatusActive, Data: map[string]float64{
			metric.Key([]string{"CN"}): 100,
			// US missing target -> dropped by inner join
		}},
	}
	snap := &Snapshot{Defs: defs, States: states, Graph: lineage.New(defs),
		Results: map[string]map[string]float64{}}

	if err := eng.Validate(snap, defs["ratio"]); err != nil {
		t.Fatal(err)
	}
	res, err := eng.Full(snap)
	if err != nil {
		t.Fatal(err)
	}
	r := res["ratio"]
	if _, hasUS := r[metric.Key([]string{"US"})]; hasUS {
		t.Fatalf("US key must be dropped by inner join, got %v", r)
	}
	if got := r[metric.Key([]string{"CN"})]; math.Abs(got-0.5) > 1e-12 {
		t.Fatalf("ratio CN = %v, want 0.5 (50/100)", got)
	}
}

func TestUnitNormalizationInCompute(t *testing.T) {
	units := unit.NewSystem()
	eng := NewEngine(units)
	kmDef := &metric.Definition{ID: "kmdist", Kind: metric.KindBase, Unit: "km", Dims: []string{"d"}}
	mDef := &metric.Definition{ID: "mdist", Kind: metric.KindBase, Unit: "m", Dims: []string{"d"}}
	defs := map[string]*metric.Definition{"kmdist": kmDef, "mdist": mDef}
	// Sum compatible units; declare output in meters.
	defs["tot"] = derivedDef("tot", "m", "a+b", []string{"d"}, map[string]string{"a": "kmdist", "b": "mdist"})
	states := map[string]*metric.State{
		"kmdist": {Def: kmDef, Status: metric.StatusActive, Data: map[string]float64{
			metric.Key([]string{"1"}): 2, // 2000 m
		}},
		"mdist": {Def: mDef, Status: metric.StatusActive, Data: map[string]float64{
			metric.Key([]string{"1"}): 500,
		}},
	}
	snap := &Snapshot{Defs: defs, States: states, Graph: lineage.New(defs),
		Results: map[string]map[string]float64{}}
	res, err := eng.Full(snap)
	if err != nil {
		t.Fatal(err)
	}
	if got := res["tot"][metric.Key([]string{"1"})]; math.Abs(got-2500) > 1e-9 {
		t.Fatalf("tot = %v, want 2500 m", got)
	}
}

func TestIncrementalLeavesUntouched(t *testing.T) {
	units := unit.NewSystem()
	eng := NewEngine(units)
	aDef := &metric.Definition{ID: "a", Kind: metric.KindBase, Unit: "count", Dims: []string{"d"}}
	bDef := &metric.Definition{ID: "b", Kind: metric.KindBase, Unit: "count", Dims: []string{"d"}}
	defs := map[string]*metric.Definition{"a": aDef, "b": bDef}
	defs["c"] = derivedDef("c", "count", "x", []string{"d"},
		map[string]string{"x": "a"})
	defs["e"] = derivedDef("e", "count", "x", []string{"d"},
		map[string]string{"x": "b"})
	states := map[string]*metric.State{
		"a": {Def: aDef, Status: metric.StatusActive, Data: map[string]float64{"1": 1}},
		"b": {Def: bDef, Status: metric.StatusActive, Data: map[string]float64{"1": 2}},
	}
	snap := &Snapshot{Defs: defs, States: states, Graph: lineage.New(defs),
		Results: map[string]map[string]float64{}}
	full, err := eng.Full(snap)
	if err != nil {
		t.Fatal(err)
	}
	snap.Results = full
	snap.Dirty = map[string]struct{}{"c": {}}

	// Change a; only c recomputed, e's result map must be the same object.
	eMapBefore := snap.Results["e"]
	// Simulate the registry: dirty derived c plus data root a.
	inc, changed, err := eng.Incremental(snap, []string{"a", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != "c" {
		t.Fatalf("changed = %v, want [c]", changed)
	}
	if mapPtr(inc["e"]) != mapPtr(eMapBefore) {
		t.Fatal("unaffected result map replaced")
	}
	full2, err := eng.Full(snap)
	if err != nil {
		t.Fatal(err)
	}
	for id, m := range full2 {
		if len(m) != len(inc[id]) {
			t.Fatalf("%s length mismatch", id)
		}
		for k, v := range m {
			if math.Abs(inc[id][k]-v) > 1e-12 {
				t.Fatalf("%s[%s] inc=%v full=%v", id, k, inc[id][k], v)
			}
		}
	}
}

func mapPtr(m map[string]float64) uintptr {
	return reflect.ValueOf(m).Pointer()
}
