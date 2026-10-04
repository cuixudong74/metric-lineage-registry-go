package lineage

import (
	"testing"

	"github.com/cuixudong74/metric-lineage-registry-go/metric"
)

func def(id string, kind metric.Kind, inputs map[string]string) *metric.Definition {
	return &metric.Definition{ID: id, Kind: kind, Inputs: inputs}
}

func TestGraphImpactAndTopo(t *testing.T) {
	defs := map[string]*metric.Definition{
		"a": def("a", metric.KindBase, nil),
		"b": def("b", metric.KindDerived, map[string]string{"x": "a"}),
		"c": def("c", metric.KindDerived, map[string]string{"x": "b"}),
		"d": def("d", metric.KindDerived, map[string]string{"x": "a", "y": "c"}),
	}
	g := New(defs)
	if !g.Has("a") || g.Has("zz") {
		t.Fatal("Has broken")
	}
	ins := g.Inputs("d")
	if len(ins) != 2 || ins[0] != "a" || ins[1] != "c" {
		t.Fatalf("inputs d = %v", ins)
	}
	impact := g.Impact("a")
	want := []string{"b", "c", "d"}
	if len(impact) != 3 {
		t.Fatalf("impact = %v", impact)
	}
	for i := range want {
		if impact[i] != want[i] {
			t.Fatalf("impact order = %v, want %v", impact, want)
		}
	}
	order, err := g.TopoOrder()
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	if !(pos["b"] < pos["c"] && pos["c"] < pos["d"]) {
		t.Fatalf("topo = %v", order)
	}
}

func TestFindCycle(t *testing.T) {
	defs := map[string]*metric.Definition{
		"a": def("a", metric.KindBase, nil),
		"b": def("b", metric.KindDerived, map[string]string{"x": "a"}),
		"c": def("c", metric.KindDerived, map[string]string{"x": "b"}),
	}
	g := New(defs)
	if cyc := g.FindCycle("d", []string{"a", "c"}); cyc != nil {
		t.Fatalf("new sink d seen as cycle: %v", FormatCycle(cyc))
	}
	// c consuming itself.
	if cyc := g.FindCycle("c", []string{"c"}); cyc == nil {
		t.Fatal("self cycle not detected")
	}
	// b consuming c: b -> c -> b.
	if cyc := g.FindCycle("b", []string{"c"}); cyc == nil {
		t.Fatal("indirect cycle not detected")
	}
	if s := FormatCycle([]string{"b", "c"}); s != "b -> c -> b" {
		t.Fatalf("format = %q", s)
	}
}
