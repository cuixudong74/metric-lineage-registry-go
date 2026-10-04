// Package lineage builds the immutable metric dependency graph and provides
// cycle detection, impact analysis and topological ordering.
//
// Semantics:
//
//   - Edges point from an *input* metric to the *derived* metric consuming it
//     ("upstream -> downstream"): edge a -> b means b consumes a.
//   - Graph values are immutable snapshots; the registry constructs a new one
//     on every committed change and never mutates a published graph.
//   - Impact(id) is id itself (when derived) plus every derived metric that
//     transitively depends on id, returned in topological (compute) order.
package lineage

import (
	"fmt"
	"sort"
	"strings"

	"github.com/cuixudong74/metric-lineage-registry-go/metric"
)

// Graph is an immutable snapshot of metric dependencies keyed by metric ID.
type Graph struct {
	// edges: input id -> set of downstream derived ids
	edges map[string]map[string]struct{}
	// inputs: derived id -> set of its direct input ids
	inputs map[string]map[string]struct{}
	// derived marks nodes that are derived metrics.
	derived map[string]struct{}
}

// New builds a graph from current definitions' explicit input sets.
func New(defs map[string]*metric.Definition) *Graph {
	g := &Graph{
		edges:   map[string]map[string]struct{}{},
		inputs:  map[string]map[string]struct{}{},
		derived: map[string]struct{}{},
	}
	ids := make([]string, 0, len(defs))
	for id := range defs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := defs[id]
		if g.edges[id] == nil {
			g.edges[id] = map[string]struct{}{} // ensure node exists
		}
		if d.Kind != metric.KindDerived {
			continue
		}
		g.derived[id] = struct{}{}
		ins := map[string]struct{}{}
		for slot, in := range d.Inputs {
			_ = slot
			ins[in] = struct{}{}
			if g.edges[in] == nil {
				g.edges[in] = map[string]struct{}{}
			}
			g.edges[in][id] = struct{}{}
		}
		g.inputs[id] = ins
	}
	return g
}

// Has reports whether the graph contains a metric node.
func (g *Graph) Has(id string) bool {
	_, ok := g.edges[id]
	return ok
}

// IsDerived reports whether id is a derived metric node.
func (g *Graph) IsDerived(id string) bool {
	_, ok := g.derived[id]
	return ok
}

// Inputs returns the sorted direct inputs of a derived metric.
func (g *Graph) Inputs(id string) []string {
	set := g.inputs[id]
	out := make([]string, 0, len(set))
	for in := range set {
		out = append(out, in)
	}
	sort.Strings(out)
	return out
}

// Downstream returns the sorted direct downstream derived metrics of id.
func (g *Graph) Downstream(id string) []string {
	set := g.edges[id]
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// Impact returns every derived metric transitively depending on id.
// When id is itself derived, id is included.  Results are returned in
// topological (input-before-output) order.
func (g *Graph) Impact(id string) []string {
	seen := map[string]struct{}{}
	var dfs func(cur string)
	dfs = func(cur string) {
		for _, d := range g.Downstream(cur) {
			if _, ok := seen[d]; ok {
				continue
			}
			seen[d] = struct{}{}
			dfs(d)
		}
	}
	dfs(id)
	if g.IsDerived(id) {
		seen[id] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	g.sortTopo(out)
	return out
}

// sortTopo sorts the given subset of derived ids in dependency order.
func (g *Graph) sortTopo(subset []string) {
	in := make(map[string]struct{}, len(subset))
	for _, id := range subset {
		in[id] = struct{}{}
	}
	done := map[string]struct{}{}
	var out []string
	var visit func(id string)
	visit = func(id string) {
		if _, ok := done[id]; ok {
			return
		}
		for _, dep := range g.Inputs(id) {
			if _, ok := in[dep]; ok {
				visit(dep)
			}
		}
		done[id] = struct{}{}
		out = append(out, id)
	}
	roots := append([]string(nil), subset...)
	sort.Strings(roots)
	for _, id := range roots {
		visit(id)
	}
	copy(subset, out)
}

// FindCycle reports whether adding/updating derivedID with the given input IDs
// would create a cycle.  Edges point input -> derived consumer, so adding
// edges newInput -> derivedID closes a cycle exactly when some newInput is
// reachable downstream of derivedID (derivedID -> ... -> newInput).
// The returned slice is a path starting at derivedID and ending at the
// conflicting input; nil means no cycle.
func (g *Graph) FindCycle(derivedID string, inputIDs []string) []string {
	targets := map[string]struct{}{}
	for _, in := range inputIDs {
		if in == derivedID {
			return []string{derivedID, derivedID}
		}
		targets[in] = struct{}{}
	}
	if len(targets) == 0 {
		return nil
	}
	var dfs func(cur string, path []string) []string
	dfs = func(cur string, path []string) []string {
		if cur != derivedID {
			if _, t := targets[cur]; t {
				return append(path, cur)
			}
		}
		for _, d := range g.Downstream(cur) {
			if containsString(path, d) {
				continue
			}
			if cyc := dfs(d, append(path, d)); cyc != nil {
				return cyc
			}
		}
		return nil
	}
	return dfs(derivedID, []string{derivedID})
}

// FormatCycle renders a cycle path produced by FindCycle as
// "a -> b -> c -> a".
func FormatCycle(cycle []string) string {
	if len(cycle) == 0 {
		return ""
	}
	parts := append([]string(nil), cycle...)
	if len(cycle) == 2 && cycle[0] == cycle[1] {
		return cycle[0] + " -> " + cycle[0]
	}
	parts = append(parts, cycle[0])
	return strings.Join(parts, " -> ")
}

func containsString(path []string, id string) bool {
	for _, p := range path {
		if p == id {
			return true
		}
	}
	return false
}

// TopoOrder returns all derived metrics in input-before-output order.
func (g *Graph) TopoOrder() ([]string, error) {
	indeg := map[string]int{}
	for id := range g.derived {
		indeg[id] = 0
	}
	for id := range g.derived {
		for in := range g.inputs[id] {
			if _, der := g.derived[in]; der {
				indeg[id]++
			}
		}
	}
	ready := make([]string, 0)
	for id, n := range indeg {
		if n == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	var out []string
	for len(ready) > 0 {
		cur := ready[0]
		ready = ready[1:]
		out = append(out, cur)
		for _, d := range g.Downstream(cur) {
			if _, der := g.derived[d]; !der {
				continue
			}
			indeg[d]--
			if indeg[d] == 0 {
				ready = insertSorted(ready, d)
			}
		}
	}
	if len(out) != len(g.derived) {
		return nil, fmt.Errorf("%w: lineage graph contains a cycle", metric.ErrCycle)
	}
	return out, nil
}

func insertSorted(s []string, v string) []string {
	i := sort.SearchStrings(s, v)
	s = append(s, "")
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}
