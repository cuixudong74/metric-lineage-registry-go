package registry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cuixudong74/metric-lineage-registry-go/metric"
)

func buildRecoverable(t *testing.T, dir string) *Registry {
	t.Helper()
	r, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	must := func(d *metric.Definition) {
		t.Helper()
		if _, err := r.Register(d); err != nil {
			t.Fatalf("register %s: %v", d.ID, err)
		}
	}
	must(&metric.Definition{ID: "a", Kind: metric.KindBase, Spec: "v1", Unit: "CNY", Dims: []string{"d"}})
	must(&metric.Definition{ID: "b", Kind: metric.KindBase, Unit: "count", Dims: []string{"d"}})
	must(&metric.Definition{ID: "c", Kind: metric.KindDerived, Unit: "USD", Dims: []string{"d"},
		Formula: "x", Inputs: map[string]string{"x": "a"}})
	must(&metric.Definition{ID: "e", Kind: metric.KindDerived, Unit: "USD/count", Dims: []string{"d"},
		Formula: "x/y", Inputs: map[string]string{"x": "c", "y": "b"}})
	if err := r.UpsertBaseData("a", []metric.Point{{DimValues: []string{"1"}, Value: 700}}); err != nil {
		t.Fatal(err)
	}
	if err := r.UpsertBaseData("b", []metric.Point{{DimValues: []string{"1"}, Value: 10}}); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRestartRecoveryDefinitionsAndVersions(t *testing.T) {
	dir := t.TempDir()
	r1 := buildRecoverable(t, dir)
	if _, err := r1.Recompute(); err != nil {
		t.Fatal(err)
	}
	eBefore, err := r1.Result("e")
	if err != nil {
		t.Fatal(err)
	}
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate restart.
	r2, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()

	// Definitions and versions restored.
	if d, err := r2.Get("a"); err != nil || d.Spec != "v1" || d.Version != 1 {
		t.Fatalf("a after restart: %+v %v", d, err)
	}
	vs, err := r2.Versions("c")
	if err != nil || len(vs) != 1 || vs[0].Formula != "x" {
		t.Fatalf("c history after restart: %v %v", vs, err)
	}

	// Lineage restored.
	ins, err := r2.Inputs("e")
	if err != nil || len(ins) != 2 {
		t.Fatalf("e inputs after restart: %v %v", ins, err)
	}
	impact, err := r2.Impact("a")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, id := range impact {
		found[id] = true
	}
	if !found["c"] || !found["e"] {
		t.Fatalf("impact after restart: %v", impact)
	}

	// Base data and derived results restored.
	eAfter, err := r2.Result("e")
	if err != nil {
		t.Fatal(err)
	}
	if !sameFloatMap(eBefore, eAfter) {
		t.Fatalf("results changed across restart: before=%v after=%v", eBefore, eAfter)
	}
}

func TestRestartReplaysChangesAfterCheckpoint(t *testing.T) {
	dir := t.TempDir()
	r1 := buildRecoverable(t, dir)
	if _, err := r1.Recompute(); err != nil {
		t.Fatal(err)
	}
	// Committed definition change and data update after the last checkpoint;
	// dirty state must survive restart and be recomputable without losing the
	// change or applying it twice.
	if _, err := r1.Register(&metric.Definition{
		ID: "a", Kind: metric.KindBase, Spec: "v2", Unit: "CNY", Dims: []string{"d"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r1.UpsertBaseData("a", []metric.Point{{DimValues: []string{"1"}, Value: 1400}}); err != nil {
		t.Fatal(err)
	}
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}

	r2, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()

	// v2 definition restored, history has 2 versions.
	if d, _ := r2.Get("a"); d.Spec != "v2" || d.Version != 2 {
		t.Fatalf("a = %+v", d)
	}
	if vs, _ := r2.Versions("a"); len(vs) != 2 || vs[0].Spec != "v1" {
		t.Fatalf("history: %v", vs)
	}

	// Results rebuilt over final committed data: 1400 CNY = 196 USD; /10 = 19.6.
	e, err := r2.Result("e")
	if err != nil {
		t.Fatal(err)
	}
	if got := e[metric.Key([]string{"1"})]; abs(got-19.6) > 1e-9 {
		t.Fatalf("e after restart = %v, want 19.6", got)
	}
	// Dirty set preserved for the changed downstream metrics.
	dirty := r2.Dirty()
	dset := map[string]bool{}
	for _, id := range dirty {
		dset[id] = true
	}
	if !dset["c"] || !dset["e"] {
		t.Fatalf("dirty after restart = %v, want c,e", dirty)
	}

	// Recompute once after restart; results identical and dirty drains, and a
	// further restart does not re-apply anything.
	if _, err := r2.Recompute(); err != nil {
		t.Fatal(err)
	}
	if dirty := r2.Dirty(); len(dirty) != 0 {
		t.Fatalf("dirty = %v", dirty)
	}
	if err := r2.Close(); err != nil {
		t.Fatal(err)
	}
	r3, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer r3.Close()
	e3, _ := r3.Result("e")
	if got := e3[metric.Key([]string{"1"})]; abs(got-19.6) > 1e-9 {
		t.Fatalf("e after second restart = %v, want 19.6", got)
	}
	if dirty := r3.Dirty(); len(dirty) != 0 {
		t.Fatalf("dirty after second restart = %v", dirty)
	}
}

func TestWALTornTailIgnored(t *testing.T) {
	dir := t.TempDir()
	r1 := buildRecoverable(t, dir)
	if _, err := r1.Recompute(); err != nil {
		t.Fatal(err)
	}
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	// Append a garbage/torn record tail as a crash would leave.
	path := filepath.Join(dir, "wal.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"seq\":999,\"type\":\"register\",\"payload\":{broken"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	r2, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if _, err := r2.Get("e"); err != nil {
		t.Fatalf("committed state lost after torn tail: %v", err)
	}
	// New appends continue after the last intact sequence.
	if _, err := r2.Register(&metric.Definition{
		ID: "newone", Kind: metric.KindBase, Unit: "count", Dims: []string{"d"},
	}); err != nil {
		t.Fatal(err)
	}
}
