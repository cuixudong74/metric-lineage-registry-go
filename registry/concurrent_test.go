package registry

import (
	"fmt"
	"sync"
	"testing"

	"github.com/cuixudong74/metric-lineage-registry-go/metric"
)

// TestConcurrentRegisterAndQuery runs many writers (new metrics and new
// versions) against continuous readers.  Every observed snapshot must be a
// fully committed one: present definitions are complete (id/version/kind set),
// the history version returned matches the current pointer or an older one,
// and no error from torn state ever surfaces.
func TestConcurrentRegisterAndQuery(t *testing.T) {
	r := newTestRegistry(t)

	// Seed a metric that writers will version repeatedly.
	if _, err := r.Register(&metric.Definition{
		ID: "hot", Kind: metric.KindBase, Unit: "count", Dims: []string{"d"}, Spec: "v0",
	}); err != nil {
		t.Fatal(err)
	}

	const writers = 8
	const rounds = 40
	var readerWg, writerWg sync.WaitGroup
	stop := make(chan struct{})

	// Readers: snapshot must always be internally consistent.
	for i := 0; i < 6; i++ {
		readerWg.Add(1)
		go func() {
			defer readerWg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := r.Snapshot()
				if snap == nil || snap.Defs == nil || snap.Graph == nil {
					t.Errorf("observed nil/incomplete snapshot")
					return
				}
				if d := snap.Defs["hot"]; d != nil {
					if d.ID != "hot" || d.Version < 1 || d.Kind == "" {
						t.Errorf("observed torn definition: %+v", d)
						return
					}
					for _, in := range snap.Graph.Inputs(d.ID) {
						if snap.Defs[in] == nil {
							t.Errorf("graph references missing input %s", in)
							return
						}
					}
				}
			}
		}()
	}

	// Version writer for "hot".
	writerWg.Add(1)
	go func() {
		defer writerWg.Done()
		for i := 1; i <= rounds; i++ {
			_, err := r.Register(&metric.Definition{
				ID: "hot", Kind: metric.KindBase, Unit: "count", Dims: []string{"d"},
				Spec: fmt.Sprintf("v%d", i),
			})
			if err != nil {
				t.Errorf("hot v%d: %v", i, err)
				return
			}
		}
	}()

	// Independent new-metric writers.
	for w := 0; w < writers; w++ {
		writerWg.Add(1)
		go func(w int) {
			defer writerWg.Done()
			for i := 0; i < rounds; i++ {
				id := fmt.Sprintf("w%d_m%d", w, i)
				_, err := r.Register(&metric.Definition{
					ID: id, Kind: metric.KindBase, Unit: "count", Dims: []string{"d"},
				})
				if err != nil {
					t.Errorf("register %s: %v", id, err)
					return
				}
				if _, err := r.Get(id); err != nil {
					t.Errorf("get %s: %v", id, err)
					return
				}
			}
		}(w)
	}

	writerWg.Wait()
	close(stop)
	readerWg.Wait()

	cur, err := r.Get("hot")
	if err != nil {
		t.Fatal(err)
	}
	if cur.Version != rounds+1 {
		t.Fatalf("hot final version = %d, want %d", cur.Version, rounds+1)
	}
	versions, err := r.Versions("hot")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != rounds+1 {
		t.Fatalf("hot history = %d versions, want %d", len(versions), rounds+1)
	}
	for i, v := range versions {
		if v.Version != i+1 {
			t.Fatalf("history version gap at %d: %d", i, v.Version)
		}
	}
}

// TestConcurrentRecomputeAndData races base-data commits against repeated
// recomputations.  Final derived results must equal a fresh full recompute:
// no committed change may be lost or double-applied.
func TestConcurrentRecomputeAndData(t *testing.T) {
	r := newTestRegistry(t)
	must := func(d *metric.Definition) {
		t.Helper()
		if _, err := r.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	must(&metric.Definition{ID: "a", Kind: metric.KindBase, Unit: "count", Dims: []string{"d"}})
	must(&metric.Definition{ID: "b", Kind: metric.KindBase, Unit: "count", Dims: []string{"d"}})
	must(&metric.Definition{ID: "c", Kind: metric.KindDerived, Unit: "count", Dims: []string{"d"},
		Formula: "x+y", Inputs: map[string]string{"x": "a", "y": "b"}})

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if err := r.UpsertBaseData("a", []metric.Point{{DimValues: []string{"1"}, Value: float64(i)}}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if err := r.UpsertBaseData("b", []metric.Point{{DimValues: []string{"1"}, Value: float64(i * 2)}}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if _, err := r.Recompute(); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()

	if _, err := r.Recompute(); err != nil {
		t.Fatal(err)
	}
	m, err := r.Result("c")
	if err != nil {
		t.Fatal(err)
	}
	// Last committed base values: a=99, b=198 -> c=297.
	if got := m[metric.Key([]string{"1"})]; got != 297 {
		t.Fatalf("c = %v, want 297 (a change lost or double applied)", got)
	}
	full, err := r.eng.Full(r.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if full["c"][metric.Key([]string{"1"})] != 297 {
		t.Fatalf("full recompute mismatch: %v", full["c"])
	}
}
