package registry

import (
	"encoding/json"
	"fmt"

	"github.com/cuixudong74/metric-lineage-registry-go/compute"
	"github.com/cuixudong74/metric-lineage-registry-go/lineage"
	"github.com/cuixudong74/metric-lineage-registry-go/metric"
	"github.com/cuixudong74/metric-lineage-registry-go/store"
)

// replay reconstructs the exact committed state from the WAL.
//
// Definitions, retirements and base updates are applied in sequence order to
// rebuild the current definition set, statuses and base data.  A committed
// "results" record acts as a checkpoint: it replaces the derived result set
// and the dirty set with the values committed at that point.
//
// After the log is exhausted:
//   - if the last word on the log is a results checkpoint, its results and
//     dirty set are authoritative;
//   - otherwise derived results are rebuilt with one full recomputation over
//     the final definitions, while the accumulated dirty set (derived metrics
//     changed after the last checkpoint) is preserved.
//
// Because an in-flight recomputation never wrote a record, it is absent after
// recovery: it is neither re-applied nor able to mask a committed change.
func (r *Registry) replay() error {
	snap := &compute.Snapshot{
		Defs:    map[string]*metric.Definition{},
		States:  map[string]*metric.State{},
		Graph:   lineage.New(map[string]*metric.Definition{}),
		Results: map[string]map[string]float64{},
		Dirty:   map[string]struct{}{},
	}
	history := map[string][]*metric.Definition{}
	lastWasResults := false

	var err error
	replayErr := r.wal.Replay(func(e *store.Entry) error {
		snap.Seq = e.Seq
		lastWasResults = false
		switch e.Type {
		case store.EntryRegister:
			var p store.RegisterPayload
			if err = json.Unmarshal(e.Payload, &p); err != nil {
				return err
			}
			var def metric.Definition
			if err = json.Unmarshal(p.DefJSON, &def); err != nil {
				return err
			}
			d := def.Clone()
			snap.Defs[d.ID] = d
			st := snap.States[d.ID]
			if st == nil {
				st = &metric.State{Def: d, Status: metric.StatusActive}
				snap.States[d.ID] = st
			} else {
				st.Def = d
			}
			if d.Kind == metric.KindBase && st.Data == nil {
				st.Data = map[string]float64{}
			}
			snap.Graph = lineage.New(snap.Defs)
			history[d.ID] = append(history[d.ID], d.Clone())
			r.markImpactDirty(snap, d.ID)
			if d.Kind == metric.KindDerived {
				snap.Dirty[d.ID] = struct{}{}
			}
		case store.EntryRetire:
			var p store.RetirePayload
			if err = json.Unmarshal(e.Payload, &p); err != nil {
				return err
			}
			if st := snap.States[p.ID]; st != nil {
				st.Status = metric.StatusRetired
			}
			snap.Graph = lineage.New(snap.Defs)
			r.markImpactDirty(snap, p.ID)
		case store.EntryBaseUpdate:
			var p store.BaseUpdatePayload
			if err = json.Unmarshal(e.Payload, &p); err != nil {
				return err
			}
			st := snap.States[p.ID]
			if st == nil {
				return fmt.Errorf("corrupt WAL: data for unknown metric %q", p.ID)
			}
			if st.Data == nil {
				st.Data = map[string]float64{}
			}
			for k, v := range p.Points {
				st.Data[k] = v
			}
			r.markImpactDirty(snap, p.ID)
		case store.EntryResults:
			var p store.ResultsPayload
			if err = json.Unmarshal(e.Payload, &p); err != nil {
				return err
			}
			snap.Results = map[string]map[string]float64{}
			for id, m := range p.Results {
				cp := make(map[string]float64, len(m))
				for k, v := range m {
					cp[k] = v
				}
				snap.Results[id] = cp
			}
			snap.Dirty = map[string]struct{}{}
			for _, id := range p.Dirty {
				snap.Dirty[id] = struct{}{}
			}
			lastWasResults = true
		}
		return nil
	})
	if replayErr != nil {
		return replayErr
	}

	if !lastWasResults {
		finalResults, ferr := r.eng.Full(snap)
		if ferr != nil {
			return ferr
		}
		snap.Results = finalResults
	}

	r.historyMu.Lock()
	r.history = history
	r.historyMu.Unlock()
	r.snap.Store(snap)
	return nil
}

// markImpactDirty marks every active derived metric transitively impacted by
// a change to root.
func (r *Registry) markImpactDirty(snap *compute.Snapshot, root string) {
	for _, imp := range snap.Graph.Impact(root) {
		if st := snap.States[imp]; st != nil && st.Status == metric.StatusActive {
			snap.Dirty[imp] = struct{}{}
		}
	}
}

func dirtyList(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// Recompute performs an incremental recomputation over the latest committed
// snapshot and durably commits the new derived results.
//
// Recomputation is a pure function of a snapshot.  The commit mutex is held
// for the whole operation, so no definition/data change can interleave; if a
// future change path ever committed concurrently, the stale publish would be
// detected and the attempt retried against the freshest snapshot — a change
// arriving during recomputation can neither be lost nor produce stale results.
func (r *Registry) Recompute() ([]string, error) {
	r.commitMu.Lock()
	defer r.commitMu.Unlock()

	for {
		base := r.current()
		// Every stale (dirty) derived metric is a recomputation root, together
		// with its transitive impact: a newly registered metric depending on a
		// dirty ancestor is therefore always (re)computed from fresh inputs,
		// and unrelated downstream consumers of that ancestor are refreshed
		// too.  Unioning all dirty nodes makes the result independent of
		// registration ordering and equal to a full recomputation.
		rootSet := map[string]struct{}{}
		for id := range base.Dirty {
			rootSet[id] = struct{}{}
		}
		if len(rootSet) == 0 {
			return nil, nil
		}
		roots := dirtyList(rootSet)
		cp := cloneSnapshot(base)
		results, recomputed, err := r.eng.Incremental(cp, roots)
		if err != nil {
			return nil, err
		}
		cp.Results = results
		for _, id := range recomputed {
			delete(cp.Dirty, id)
		}
		payload, err := json.Marshal(store.ResultsPayload{
			Results: cp.Results,
			Dirty:   dirtyList(cp.Dirty),
		})
		if err != nil {
			return nil, err
		}
		if r.wal != nil {
			e, err := r.wal.Append(store.EntryResults, payload)
			if err != nil {
				return nil, err
			}
			cp.Seq = e.Seq
		} else {
			cp.Seq = base.Seq + 1
		}
		if r.current() != base {
			continue // raced with a newer commit; retry on the fresh snapshot
		}
		r.snap.Store(cp)
		return recomputed, nil
	}
}
