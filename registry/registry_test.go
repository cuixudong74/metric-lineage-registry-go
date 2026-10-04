package registry

import (
	"errors"
	"path/filepath"
	"testing"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := New(Options{StorePath: "", StoreSync: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func mustRegister(t *testing.T, r *Registry, in RegisterInput) *MetricDefinition {
	t.Helper()
	d, err := r.Register(in)
	if err != nil {
		t.Fatalf("Register %q: %v", in.ID, err)
	}
	return d
}

func TestSmokeRegisterAndCompute(t *testing.T) {
	r := newTestRegistry(t)
	cny := Unit{Name: "CNY"}
	ratio := Unit{Name: "ratio"}

	mustRegister(t, r, RegisterInput{ID: "gmv", Unit: cny, Dimensions: []string{"region"}})
	mustRegister(t, r, RegisterInput{ID: "refund", Unit: cny, Dimensions: []string{"region"}})
	mustRegister(t, r, RegisterInput{ID: "refund_rate", Formula: "refund / gmv", Unit: ratio, Dimensions: []string{"region"}})

	if _, err := r.PutBaseData("gmv", map[string]string{"region": "cn"}, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PutBaseData("refund", map[string]string{"region": "cn"}, 20); err != nil {
		t.Fatal(err)
	}

	// 重算前派生结果尚不存在。
	if _, _, ok := r.GetResult("refund_rate", map[string]string{"region": "cn"}); ok {
		t.Fatal("refund_rate should not exist before recompute")
	}
	res, err := r.Recompute(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recomputed) != 1 || res.Recomputed[0] != "refund_rate" {
		t.Fatalf("unexpected recomputed: %v", res.Recomputed)
	}
	v, _, ok := r.GetResult("refund_rate", map[string]string{"region": "cn"})
	if !ok || v != 0.2 {
		t.Fatalf("refund_rate = %v (ok=%v), want 0.2", v, ok)
	}
}

func TestVersioning(t *testing.T) {
	r := newTestRegistry(t)
	d1 := mustRegister(t, r, RegisterInput{ID: "gmv", Unit: Unit{Name: "CNY"}})
	if d1.Version != 1 {
		t.Fatalf("first version = %d", d1.Version)
	}
	// 完全重复注册被拒绝。
	if _, err := r.Register(RegisterInput{ID: "gmv", Unit: Unit{Name: "CNY"}}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("dup register err = %v, want ErrAlreadyExists", err)
	}
	// 改单位产生新版本。
	d2, err := r.Register(RegisterInput{ID: "gmv", Unit: Unit{Name: "USD"}})
	if err != nil {
		t.Fatalf("new version: %v", err)
	}
	if d2.Version != 2 {
		t.Fatalf("second version = %d", d2.Version)
	}
	// 历史版本不可变。
	v1, err := r.GetVersion("gmv", 1)
	if err != nil || v1.Unit.Name != "CNY" {
		t.Fatalf("v1 = %+v, err=%v", v1, err)
	}
	hs, err := r.History("gmv")
	if err != nil || len(hs) != 2 {
		t.Fatalf("history len = %d, err=%v", len(hs), err)
	}
}

func TestImpactAndRetire(t *testing.T) {
	r := newTestRegistry(t)
	mustRegister(t, r, RegisterInput{ID: "a", Unit: Unit{Name: "CNY"}})
	mustRegister(t, r, RegisterInput{ID: "b", Formula: "a * 2", Unit: Unit{Name: "CNY"}})
	mustRegister(t, r, RegisterInput{ID: "c", Formula: "b + a", Unit: Unit{Name: "CNY"}})

	got := r.CurrentSnapshot().Impacted("a")
	if len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("impact = %v, want [b c]", got)
	}
	if err := r.Retire("a"); !errors.Is(err, ErrDependentStillActive) {
		t.Fatalf("retire a err = %v, want ErrDependentStillActive", err)
	}
	if err := r.Retire("c"); err != nil {
		t.Fatalf("retire c: %v", err)
	}
	if err := r.Retire("b"); err != nil {
		t.Fatalf("retire b: %v", err)
	}
	if err := r.Retire("a"); err != nil {
		t.Fatalf("retire a: %v", err)
	}
}

func TestRestartRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	opts := Options{StorePath: path, StoreSync: true}
	r, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, r, RegisterInput{ID: "gmv", Unit: Unit{Name: "CNY"}, Dimensions: []string{"region"}})
	mustRegister(t, r, RegisterInput{ID: "tax", Formula: "gmv * 0.1", Unit: Unit{Name: "CNY"}, Dimensions: []string{"region"}})
	if _, err := r.PutBaseData("gmv", map[string]string{"region": "cn"}, 200); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Recompute(false); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// 重新打开：定义、血缘、数据恢复，结果确定性重建。
	r2, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	v, _, ok := r2.GetResult("tax", map[string]string{"region": "cn"})
	if !ok || v != 20 {
		t.Fatalf("after restart tax = %v ok=%v, want 20", v, ok)
	}
	hs, _ := r2.History("gmv")
	if len(hs) != 1 {
		t.Fatalf("history after restart = %d", len(hs))
	}
	if got := r2.CurrentSnapshot().Impacted("gmv"); len(got) != 1 || got[0] != "tax" {
		t.Fatalf("lineage after restart = %v", got)
	}
}
