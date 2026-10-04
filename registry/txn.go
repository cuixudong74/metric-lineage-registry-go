package registry

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/cuixudong74/metric-lineage-registry-go/compute"
	"github.com/cuixudong74/metric-lineage-registry-go/lineage"
	"github.com/cuixudong74/metric-lineage-registry-go/metric"
	"github.com/cuixudong74/metric-lineage-registry-go/store"
)

// Register creates a new metric or registers a new immutable version.
//
// Re-registering the same ID with unchanged content is idempotent (returns the
// existing version).  Changed content is validated (lineage cycle, units,
// dimensions) and committed as a new version; previous versions remain
// readable through GetVersion/Versions and are never overwritten.
func (r *Registry) Register(in *metric.Definition) (*metric.Definition, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: nil definition", metric.ErrBadDefinition)
	}
	r.commitMu.Lock()
	defer r.commitMu.Unlock()

	snap := r.current()
	def, roots, err := r.applyRegister(snap, in)
	if err != nil {
		return nil, err
	}
	if roots == nil {
		return def, nil // idempotent no-op
	}
	payload, err := json.Marshal(store.RegisterPayload{
		DefJSON: mustMarshalDef(def),
	})
	if err != nil {
		return nil, err
	}
	_, err = r.commitOn(snap, store.EntryRegister, payload, roots, func(cp *compute.Snapshot) {
		cp.Defs[def.ID] = def.Clone()
		st := cp.States[def.ID]
		if st == nil {
			st = &metric.State{Def: def, Status: metric.StatusActive}
			cp.States[def.ID] = st
		} else {
			st.Def = def
			st.Status = metric.StatusActive // re-registering revives
		}
		if def.Kind == metric.KindBase && st.Data == nil {
			st.Data = map[string]float64{}
		}
		cp.Graph = lineage.New(cp.Defs)
	}, true)
	if err != nil {
		return nil, err
	}
	r.appendHistory(def)
	return r.current().Defs[def.ID].Clone(), nil
}

// Retire takes a metric offline.  Its definition history is preserved and its
// downstream derived metrics become dirty; new definitions cannot reference a
// retired metric.
func (r *Registry) Retire(id string) error {
	r.commitMu.Lock()
	defer r.commitMu.Unlock()
	snap := r.current()
	if snap.Defs[id] == nil {
		return fmt.Errorf("%w: %s", metric.ErrNotFound, id)
	}
	if snap.States[id].Status == metric.StatusRetired {
		return nil
	}
	payload, _ := json.Marshal(store.RetirePayload{ID: id})
	_, err := r.commitOn(snap, store.EntryRetire, payload, []string{id}, func(cp *compute.Snapshot) {
		cp.States[id].Status = metric.StatusRetired
		cp.Graph = lineage.New(cp.Defs)
	}, true)
	return err
}

// UpsertBaseData commits base data points (dimension tuple -> value upsert).
func (r *Registry) UpsertBaseData(id string, points []metric.Point) error {
	r.commitMu.Lock()
	defer r.commitMu.Unlock()
	snap := r.current()
	def := snap.Defs[id]
	if def == nil {
		return fmt.Errorf("%w: %s", metric.ErrNotFound, id)
	}
	if def.Kind != metric.KindBase {
		return fmt.Errorf("%w: %s is not a base metric", metric.ErrBadDefinition, id)
	}
	if snap.States[id].Status == metric.StatusRetired {
		return fmt.Errorf("%w: %s", metric.ErrRetired, id)
	}
	pointsMap := make(map[string]float64, len(points))
	for _, p := range points {
		if len(p.DimValues) != len(def.Dims) {
			return fmt.Errorf("%w: point for %q has %d dim values, metric has %d",
				metric.ErrDimMismatch, id, len(p.DimValues), len(def.Dims))
		}
		pointsMap[metric.Key(p.DimValues)] = p.Value
	}
	payload, _ := json.Marshal(store.BaseUpdatePayload{ID: id, Points: pointsMap})
	_, err := r.commitOn(snap, store.EntryBaseUpdate, payload, []string{id}, func(cp *compute.Snapshot) {
		if cp.States[id].Data == nil {
			cp.States[id].Data = map[string]float64{}
		}
		for k, v := range pointsMap {
			cp.States[id].Data[k] = v
		}
	}, true)
	return err
}

// commitOn is the single mutation funnel.  It clones the base snapshot,
// applies mutate, marks the entire transitive impact of every change root
// dirty on the *new* graph, durably appends a WAL record, optionally runs an
// incremental recomputation, and atomically publishes the new snapshot.
func (r *Registry) commitOn(base *compute.Snapshot, typ store.EntryType, payloadJSON json.RawMessage,
	roots []string, mutate func(cp *compute.Snapshot), durable bool) (*compute.Snapshot, error) {

	cp := cloneSnapshot(base)
	mutate(cp)
	for _, root := range roots {
		for _, imp := range cp.Graph.Impact(root) {
			st := cp.States[imp]
			if st == nil {
				continue
			}
			if st.Status == metric.StatusActive {
				cp.Dirty[imp] = struct{}{}
			} else {
				// A retired derived metric keeps no result and never blocks
				// the dirty set from draining.
				delete(cp.Results, imp)
				delete(cp.Dirty, imp)
			}
		}
	}

	if durable && r.wal != nil {
		e, err := r.wal.Append(typ, json.RawMessage(payloadJSON))
		if err != nil {
			return nil, err
		}
		cp.Seq = e.Seq
	} else {
		cp.Seq = base.Seq + 1
	}

	if r.cfg.AutoRecompute {
		// Recompute over the whole current dirty set (plus its impact), not
		// just this commit's roots: a commit may sit next to already-stale
		// derived metrics, and the post-commit state must have no dirty nodes.
		roots := make([]string, 0, len(cp.Dirty))
		for id := range cp.Dirty {
			roots = append(roots, id)
		}
		if err := r.recomputeInto(cp, roots); err != nil {
			return nil, err
		}
	}

	r.snap.Store(cp)
	return cp, nil
}

// recomputeInto runs an incremental recomputation on cp for the given roots
// and installs the results; cleared dirty markers are exactly the recomputed
// active metrics.
func (r *Registry) recomputeInto(cp *compute.Snapshot, roots []string) error {
	results, recomputed, err := r.eng.Incremental(cp, roots)
	if err != nil {
		return err
	}
	cp.Results = results
	for _, id := range recomputed {
		delete(cp.Dirty, id)
	}
	return nil
}

func mustMarshalDef(def *metric.Definition) json.RawMessage {
	b, err := json.Marshal(def)
	if err != nil {
		panic(err)
	}
	return b
}

func (r *Registry) appendHistory(def *metric.Definition) {
	r.historyMu.Lock()
	defer r.historyMu.Unlock()
	r.history[def.ID] = append(r.history[def.ID], def.Clone())
}

// ---- Read API ----------------------------------------------------------

// Snapshot returns the current committed immutable snapshot.
func (r *Registry) Snapshot() *compute.Snapshot { return r.current() }

// Get returns the current definition of a metric.
func (r *Registry) Get(id string) (*metric.Definition, error) {
	d := r.current().Defs[id]
	if d == nil {
		return nil, fmt.Errorf("%w: %s", metric.ErrNotFound, id)
	}
	return d.Clone(), nil
}

// GetVersion returns a specific historical version of a metric.
func (r *Registry) GetVersion(id string, version int) (*metric.Definition, error) {
	r.historyMu.RLock()
	defer r.historyMu.RUnlock()
	for _, d := range r.history[id] {
		if d.Version == version {
			return d.Clone(), nil
		}
	}
	return nil, fmt.Errorf("%w: %s v%d", metric.ErrNotFound, id, version)
}

// Versions lists all committed versions of a metric, oldest first.
func (r *Registry) Versions(id string) ([]metric.Definition, error) {
	r.historyMu.RLock()
	defer r.historyMu.RUnlock()
	defs := r.history[id]
	if defs == nil {
		return nil, fmt.Errorf("%w: %s", metric.ErrNotFound, id)
	}
	out := make([]metric.Definition, 0, len(defs))
	for _, d := range defs {
		out = append(out, *d.Clone())
	}
	return out, nil
}

// Inputs returns the direct input metric IDs of a derived metric.
func (r *Registry) Inputs(id string) ([]string, error) {
	snap := r.current()
	if !snap.Graph.Has(id) {
		return nil, fmt.Errorf("%w: %s", metric.ErrNotFound, id)
	}
	return snap.Graph.Inputs(id), nil
}

// Impact lists all derived metrics transitively affected by a change to id,
// in topological order.
func (r *Registry) Impact(id string) ([]string, error) {
	snap := r.current()
	if !snap.Graph.Has(id) {
		return nil, fmt.Errorf("%w: %s", metric.ErrNotFound, id)
	}
	return snap.Graph.Impact(id), nil
}

// Result returns a derived metric's current result keyed by dimension tuple.
func (r *Registry) Result(id string) (map[string]float64, error) {
	snap := r.current()
	if snap.Defs[id] == nil {
		return nil, fmt.Errorf("%w: %s", metric.ErrNotFound, id)
	}
	m := snap.Results[id]
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out, nil
}

// Dirty reports derived metrics waiting for recomputation.
func (r *Registry) Dirty() []string {
	snap := r.current()
	out := make([]string, 0, len(snap.Dirty))
	for id := range snap.Dirty {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Status reports active/retired status.
func (r *Registry) Status(id string) (metric.Status, error) {
	st := r.current().States[id]
	if st == nil {
		return "", fmt.Errorf("%w: %s", metric.ErrNotFound, id)
	}
	return st.Status, nil
}
