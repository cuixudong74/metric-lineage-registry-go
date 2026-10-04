package registry

import (
	"errors"
	"testing"

	"github.com/cuixudong74/metric-lineage-registry-go/metric"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestRegistrationVersioning(t *testing.T) {
	r := newTestRegistry(t)

	d1 := &metric.Definition{ID: "m1", Kind: metric.KindBase, Spec: "v1", Unit: "m", Dims: []string{"d"}}
	got, err := r.Register(d1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 {
		t.Fatalf("first version = %d, want 1", got.Version)
	}

	// Identical content is idempotent.
	got, err = r.Register(d1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 {
		t.Fatalf("identical re-register produced version %d, want 1", got.Version)
	}

	// Changed content creates a new immutable version.
	d2 := &metric.Definition{ID: "m1", Kind: metric.KindBase, Spec: "v2 caliber", Unit: "m", Dims: []string{"d"}}
	got, err = r.Register(d2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 {
		t.Fatalf("changed re-register version = %d, want 2", got.Version)
	}
	versions, err := r.Versions("m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(versions))
	}
	v1, err := r.GetVersion("m1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Spec != "v1" {
		t.Fatalf("historical version overwritten: spec=%q", v1.Spec)
	}
	cur, _ := r.Get("m1")
	if cur.Spec != "v2 caliber" {
		t.Fatalf("current spec = %q", cur.Spec)
	}

	// Changing kind is rejected.
	bad := &metric.Definition{ID: "m1", Kind: metric.KindDerived, Spec: "x", Unit: "m", Formula: "a",
		Inputs: map[string]string{"a": "m1"}, Dims: []string{"d"}}
	if _, err := r.Register(bad); !errors.Is(err, metric.ErrBadDefinition) {
		t.Fatalf("kind change err = %v, want ErrBadDefinition", err)
	}
}

func TestCycleDetection(t *testing.T) {
	r := newTestRegistry(t)
	mkBase := func(id string) {
		if _, err := r.Register(&metric.Definition{ID: id, Kind: metric.KindBase, Unit: "count", Dims: []string{"d"}}); err != nil {
			t.Fatal(err)
		}
	}
	mkDerived := func(id, formula string, ins map[string]string) {
		if _, err := r.Register(&metric.Definition{
			ID: id, Kind: metric.KindDerived, Unit: "count", Dims: []string{"d"},
			Formula: formula, Inputs: ins,
		}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	mkBase("a")
	mkDerived("b", "x", map[string]string{"x": "a"})
	mkDerived("c", "y", map[string]string{"y": "b"})

	// Diamond without a cycle is accepted.
	mkDerived("d", "x+y", map[string]string{"x": "a", "y": "b"})

	// Direct self reference.
	if _, err := r.Register(&metric.Definition{
		ID: "self", Kind: metric.KindDerived, Unit: "count", Dims: []string{"d"},
		Formula: "x", Inputs: map[string]string{"x": "self"},
	}); !errors.Is(err, metric.ErrCycle) {
		t.Fatalf("self cycle err = %v, want ErrCycle", err)
	}

	// Indirect cycle: update c to consume d, and d to consume c.
	// Existing: a -> b -> c, a -> d, b -> d. Make d -> c first (fine,
	// c is still downstream of b/a), then flip c to consume d: d -> c -> d.
	mkDerived("d", "x", map[string]string{"x": "c"})
	loop := &metric.Definition{
		ID: "c", Kind: metric.KindDerived, Unit: "count", Dims: []string{"d"},
		Formula: "x", Inputs: map[string]string{"x": "d"},
	}
	if _, err := r.Register(loop); !errors.Is(err, metric.ErrCycle) {
		t.Fatalf("indirect cycle err = %v, want ErrCycle", err)
	}

	// The rejected registration must not have changed committed state: c must
	// still consume b (its previous committed version).
	if ins, _ := r.Inputs("c"); len(ins) != 1 || ins[0] != "b" {
		t.Fatalf("rejected registration modified committed state: inputs=%v", ins)
	}

	// Impact analysis on the committed graph (a -> b -> c, b/c -> d after d's
	// successful update... d now consumes c, so impact of a is b,c,d).
	impact, err := r.Impact("a")
	if err != nil {
		t.Fatal(err)
	}
	wantSet := map[string]bool{"b": true, "c": true, "d": true}
	gotSet := map[string]bool{}
	for _, id := range impact {
		gotSet[id] = true
	}
	if len(gotSet) != len(wantSet) {
		t.Fatalf("impact = %v, want %v", impact, wantSet)
	}
	for id := range wantSet {
		if !gotSet[id] {
			t.Fatalf("impact missing %s: %v", id, impact)
		}
	}
	// Topological order: b before c before d.
	pos := map[string]int{}
	for i, id := range impact {
		pos[id] = i
	}
	if !(pos["b"] < pos["c"] && pos["c"] < pos["d"]) {
		t.Fatalf("impact not topologically ordered: %v", impact)
	}
}

func TestUnitAndDimensionValidation(t *testing.T) {
	r := newTestRegistry(t)
	reg := func(d *metric.Definition) error {
		_, err := r.Register(d)
		return err
	}
	base := func(id, unit string, dims []string) *metric.Definition {
		return &metric.Definition{ID: id, Kind: metric.KindBase, Unit: unit, Dims: dims}
	}
	if err := reg(base("len", "m", []string{"d"})); err != nil {
		t.Fatal(err)
	}
	if err := reg(base("dur", "s", []string{"d"})); err != nil {
		t.Fatal(err)
	}
	if err := reg(base("dist_km", "km", []string{"d"})); err != nil {
		t.Fatal(err)
	}
	if err := reg(base("only_region", "count", []string{"region"})); err != nil {
		t.Fatal(err)
	}
	if err := reg(base("temp", "K", []string{"d"})); err != nil {
		t.Fatal(err)
	}

	// Adding incompatible units: m + s.
	err := reg(&metric.Definition{
		ID: "badadd", Kind: metric.KindDerived, Unit: "m", Dims: []string{"d"},
		Formula: "a+b", Inputs: map[string]string{"a": "len", "b": "dur"},
	})
	if !errors.Is(err, metric.ErrUnitIncompatible) {
		t.Fatalf("m+s err = %v, want ErrUnitIncompatible", err)
	}

	// Declared unit incompatible with formula vector (a*b is m*s, declared m).
	err = reg(&metric.Definition{
		ID: "badmul", Kind: metric.KindDerived, Unit: "m", Dims: []string{"d"},
		Formula: "a*b", Inputs: map[string]string{"a": "len", "b": "dur"},
	})
	if !errors.Is(err, metric.ErrUnitIncompatible) {
		t.Fatalf("m*s declared m err = %v, want ErrUnitIncompatible", err)
	}

	// m*s declared as compound m*s is fine.
	if err := reg(&metric.Definition{
		ID: "ms", Kind: metric.KindDerived, Unit: "m*s", Dims: []string{"d"},
		Formula: "a*b", Inputs: map[string]string{"a": "len", "b": "dur"},
	}); err != nil {
		t.Fatalf("m*s compound rejected: %v", err)
	}

	// Division m/s is speed.
	if err := reg(&metric.Definition{
		ID: "speed", Kind: metric.KindDerived, Unit: "m/s", Dims: []string{"d"},
		Formula: "a/b", Inputs: map[string]string{"a": "len", "b": "dur"},
	}); err != nil {
		t.Fatalf("m/s rejected: %v", err)
	}

	// Compatible units may be mixed after normalization: km + m, output in cm.
	if err := reg(&metric.Definition{
		ID: "sumlen", Kind: metric.KindDerived, Unit: "cm", Dims: []string{"d"},
		Formula: "a+b", Inputs: map[string]string{"a": "dist_km", "b": "len"},
	}); err != nil {
		t.Fatalf("km+m -> cm rejected: %v", err)
	}

	// Dimension mismatch: output dim not present in every input.
	err = reg(&metric.Definition{
		ID: "baddim", Kind: metric.KindDerived, Unit: "count", Dims: []string{"day"},
		Formula: "a", Inputs: map[string]string{"a": "only_region"},
	})
	if !errors.Is(err, metric.ErrDimMismatch) {
		t.Fatalf("dim mismatch err = %v, want ErrDimMismatch", err)
	}

	// Unknown input metric.
	err = reg(&metric.Definition{
		ID: "ghost", Kind: metric.KindDerived, Unit: "count", Dims: []string{"d"},
		Formula: "a", Inputs: map[string]string{"a": "nope"},
	})
	if !errors.Is(err, metric.ErrUnknownInput) {
		t.Fatalf("unknown input err = %v, want ErrUnknownInput", err)
	}

	// Unbound / unused slots.
	err = reg(&metric.Definition{
		ID: "unbound", Kind: metric.KindDerived, Unit: "m", Dims: []string{"d"},
		Formula: "a+b", Inputs: map[string]string{"a": "len"},
	})
	if err == nil {
		t.Fatalf("unbound slot accepted")
	}
	err = reg(&metric.Definition{
		ID: "unused", Kind: metric.KindDerived, Unit: "m", Dims: []string{"d"},
		Formula: "a", Inputs: map[string]string{"a": "len", "b": "dur"},
	})
	if err == nil {
		t.Fatalf("unused slot accepted")
	}

	// Temperature affine conversion: C and K compatible, F incompatible
	// only in vector sense — here C and K share the temperature vector.
	if !r.units.Compatible("C", "K") {
		t.Fatalf("C and K should be compatible")
	}
	if r.units.Compatible("C", "m") {
		t.Fatalf("C and m must not be compatible")
	}
}
