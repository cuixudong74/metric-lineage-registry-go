// Package registry is the unified facade: MVCC registration, lineage,
// validation, recomputation, concurrency control and durable recovery.
//
// Concurrency model
//
//   - All state is held in an immutable *compute.Snapshot plus history maps.
//     Readers (Get / Snapshot / Impact / Result / Versions) always observe a
//     fully committed snapshot; they can never see a half-applied change.
//   - A single commitMu serializes mutating transactions.  Each transaction
//     validates against the latest committed snapshot, durably appends a WAL
//     record, builds a new snapshot and atomically publishes it.
//   - Recomputation commits through the same WAL + publish path.  A definition
//     change arriving while a recomputation is in flight is never lost: the
//     recomputation retries against the freshest snapshot until its sequence
//     matches, then commits.
//
// Recovery model
//
//   - On open, committed records are replayed from the write-ahead log in
//     sequence order.  Derived results are rebuilt through the same
//     incremental engine, and the per-record dirty bookkeeping is restored, so
//     in-flight (never committed) recomputations are simply absent: they are
//     neither re-applied nor silently completed under a committed change.
package registry

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/cuixudong74/metric-lineage-registry-go/compute"
	"github.com/cuixudong74/metric-lineage-registry-go/lineage"
	"github.com/cuixudong74/metric-lineage-registry-go/metric"
	"github.com/cuixudong74/metric-lineage-registry-go/store"
	"github.com/cuixudong74/metric-lineage-registry-go/unit"
)

// Config configures a Registry.
type Config struct {
	// Dir is the persistence directory; empty means in-memory only.
	Dir string
	// AutoRecompute runs an incremental recomputation synchronously inside
	// every mutating commit, so reads after Register/Upsert/Retire already
	// see fresh derived results.
	AutoRecompute bool
}

// Registry is the concurrent-safe metric lineage registry.
type Registry struct {
	cfg      Config
	units    *unit.System
	wal      *store.WAL
	eng      *compute.Engine
	commitMu sync.Mutex
	snap     atomic.Pointer[compute.Snapshot]

	// history is immutable after each publish (copy-on-write); readers may
	// access the published maps without locking.
	historyMu sync.RWMutex
	history   map[string][]*metric.Definition // id -> all versions
}

// Errors.
var (
	ErrRegistry = errors.New("registry error")
)

// New opens (or creates) a registry and replays committed state from Dir.
func New(cfg Config) (*Registry, error) {
	r := &Registry{
		cfg:     cfg,
		units:   unit.NewSystem(),
		history: map[string][]*metric.Definition{},
	}
	r.eng = compute.NewEngine(r.units)
	initial := &compute.Snapshot{
		Defs:    map[string]*metric.Definition{},
		States:  map[string]*metric.State{},
		Graph:   lineage.New(map[string]*metric.Definition{}),
		Results: map[string]map[string]float64{},
		Dirty:   map[string]struct{}{},
	}
	r.snap.Store(initial)
	if cfg.Dir != "" {
		wal, err := store.OpenWAL(cfg.Dir)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrRegistry, err)
		}
		r.wal = wal
		if err := r.replay(); err != nil {
			_ = wal.Close()
			return nil, err
		}
	}
	return r, nil
}

// RegisterUnit registers a custom linear unit usable by later definitions.
func (r *Registry) RegisterUnit(u *unit.Unit) error {
	return r.units.Register(u)
}

// Close flushes and closes persistence.
func (r *Registry) Close() error {
	if r.wal != nil {
		return r.wal.Close()
	}
	return nil
}

// current returns the latest committed snapshot.
func (r *Registry) current() *compute.Snapshot { return r.snap.Load() }

// applyRegister mutates cloned snapshot state to install a new definition.
// Returns the persisted definition and dirty roots (definition change itself
// impacts its downstream; when re-registered derived also includes itself).
func (r *Registry) applyRegister(snap *compute.Snapshot, in *metric.Definition) (*metric.Definition, []string, error) {
	if in.ID == "" {
		return nil, nil, fmt.Errorf("%w: id required", metric.ErrBadDefinition)
	}
	// An empty unit denotes a dimensionless metric ("", "1", "ratio" ...).
	existing := snap.Defs[in.ID]
	def := in.Clone()
	if def.Dims == nil {
		def.Dims = []string{}
	}
	if def.Inputs == nil {
		def.Inputs = map[string]string{}
	}
	dirtyRoots := []string{in.ID}
	if existing != nil {
		if existing.Kind != def.Kind {
			return nil, nil, fmt.Errorf("%w: cannot change kind of %q", metric.ErrBadDefinition, in.ID)
		}
		// Identical content: idempotent, no new version, no WAL write.
		if sameDefinition(existing, def) && snap.States[in.ID].Status == metric.StatusActive {
			return existing.Clone(), nil, nil
		}
		def.Version = existing.Version + 1
	} else {
		def.Version = 1
	}
	// Validate formula/inputs/dims/units against a view where the candidate
	// definition itself is visible (needed for derived-validity checks).
	view := cloneSnapshot(snap)
	if st := view.States[def.ID]; st != nil && st.Status == metric.StatusRetired {
		// Re-registering a retired metric revives it; content must validate.
		st.Status = metric.StatusActive
	}
	view.Defs[def.ID] = def
	if _, ok := view.States[def.ID]; !ok {
		view.States[def.ID] = &metric.State{Def: def, Status: metric.StatusActive}
	}
	view.Graph = lineage.New(view.Defs)
	if def.Kind == metric.KindDerived {
		if cyc := view.Graph.FindCycle(def.ID, inputValues(def.Inputs)); cyc != nil {
			return nil, nil, fmt.Errorf("%w: %s", metric.ErrCycle, lineage.FormatCycle(cyc))
		}
	}
	if err := r.eng.Validate(view, def); err != nil {
		return nil, nil, err
	}
	return def, dirtyRoots, nil
}

func inputValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// sameDefinition compares committable content (ignores Version).
func sameDefinition(a, b *metric.Definition) bool {
	if a.Kind != b.Kind || a.Spec != b.Spec || a.Unit != b.Unit || a.Formula != b.Formula {
		return false
	}
	if len(a.Dims) != len(b.Dims) {
		return false
	}
	for i := range a.Dims {
		if a.Dims[i] != b.Dims[i] {
			return false
		}
	}
	if len(a.Inputs) != len(b.Inputs) {
		return false
	}
	for k, v := range a.Inputs {
		if b.Inputs[k] != v {
			return false
		}
	}
	return true
}

// cloneSnapshot deep-copies the mutable parts of a snapshot (results maps are
// copied at top level; per-metric result maps are never mutated in place).
func cloneSnapshot(s *compute.Snapshot) *compute.Snapshot {
	cp := &compute.Snapshot{
		Defs:    make(map[string]*metric.Definition, len(s.Defs)),
		States:  make(map[string]*metric.State, len(s.States)),
		Results: make(map[string]map[string]float64, len(s.Results)),
		Dirty:   make(map[string]struct{}, len(s.Dirty)),
		Seq:     s.Seq,
	}
	for k, v := range s.Defs {
		cp.Defs[k] = v.Clone()
	}
	for k, v := range s.States {
		st := &metric.State{Def: cp.Defs[k], Status: v.Status}
		if v.Data != nil {
			st.Data = make(map[string]float64, len(v.Data))
			for dk, dv := range v.Data {
				st.Data[dk] = dv
			}
		}
		cp.States[k] = st
	}
	for k, v := range s.Results {
		m := make(map[string]float64, len(v))
		for dk, dv := range v {
			m[dk] = dv
		}
		cp.Results[k] = m
	}
	for k := range s.Dirty {
		cp.Dirty[k] = struct{}{}
	}
	cp.Graph = s.Graph
	return cp
}
