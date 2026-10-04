// Package compute evaluates derived metrics over immutable committed
// snapshots and supports incremental recomputation of affected subgraphs.
//
// Evaluation semantics:
//
//   - A derived metric's formula references inputs by slot name ("a", "b",
//     ...); each slot maps to one registered metric ID.
//   - Values of each input are projected onto the output metric's dimensions:
//     input dimensions must cover all output dimensions; extra input
//     dimensions are rolled up with SUM.  Output keys are the inner join of
//     all projected inputs (a key missing from any input is skipped), so
//     results are deterministic and reproducible.
//   - Operands are normalized to their dimension's canonical base unit before
//     arithmetic; the result is converted to the declared output unit.
//   - Full and incremental recomputation are algebraically identical:
//     incremental replaces exactly the impacted subgraph and leaves every
//     other result map untouched.
package compute

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cuixudong74/metric-lineage-registry-go/expr"
	"github.com/cuixudong74/metric-lineage-registry-go/lineage"
	"github.com/cuixudong74/metric-lineage-registry-go/metric"
	"github.com/cuixudong74/metric-lineage-registry-go/unit"
)

// Snapshot is an immutable committed view used by the compute engine.
type Snapshot struct {
	Defs    map[string]*metric.Definition // current version per id
	States  map[string]*metric.State      // current state per id
	Graph   *lineage.Graph
	Results map[string]map[string]float64 // derived id -> dim key -> value
	Dirty   map[string]struct{}           // derived ids needing recompute
	Seq     int64
}

// Engine evaluates derived metrics.
type Engine struct {
	units *unit.System
}

// NewEngine creates an Engine with the given unit system.
func NewEngine(units *unit.System) *Engine {
	return &Engine{units: units}
}

// Units exposes the unit system.
func (e *Engine) Units() *unit.System { return e.units }

// Errors.
var (
	ErrValidate = errors.New("compute: validation failed")
	ErrCompute  = errors.New("compute: evaluation failed")
)

// Validate checks kind, formula, input references, dimensions and units for a
// definition against the snapshot the definition would be committed into.
// Inputs must already exist and be active (ordering rule).
func (e *Engine) Validate(snap *Snapshot, def *metric.Definition) error {
	if def == nil {
		return fmt.Errorf("%w: nil definition", ErrValidate)
	}
	if def.ID == "" {
		return fmt.Errorf("%w: id required", ErrValidate)
	}
	if len(def.Dims) != len(uniqueSorted(def.Dims)) {
		return fmt.Errorf("%w: metric %q has duplicate dimensions", ErrValidate, def.ID)
	}
	if _, err := e.units.Lookup(def.Unit); err != nil {
		return fmt.Errorf("%w: metric %q declares unknown unit %q", ErrValidate, def.ID, def.Unit)
	}
	switch def.Kind {
	case metric.KindBase:
		return nil
	case metric.KindDerived:
	default:
		return fmt.Errorf("%w: metric %q has unknown kind %q", ErrValidate, def.ID, def.Kind)
	}

	ex, err := expr.Parse(def.Formula)
	if err != nil {
		return fmt.Errorf("%w: metric %q formula: %v", ErrValidate, def.ID, err)
	}
	slots := ex.Inputs()
	if len(slots) == 0 {
		return fmt.Errorf("%w: metric %q formula references no inputs", ErrValidate, def.ID)
	}
	slotSet := map[string]bool{}
	for _, s := range slots {
		slotSet[s] = true
	}
	for slot := range def.Inputs {
		if !slotSet[slot] {
			return fmt.Errorf("%w: metric %q binds unused slot %q", ErrValidate, def.ID, slot)
		}
	}

	inputDefs := make(map[string]*metric.Definition, len(slots))
	outDimSet := map[string]struct{}{}
	for _, d := range def.Dims {
		outDimSet[d] = struct{}{}
	}
	for _, slot := range slots {
		inID, ok := def.Inputs[slot]
		if !ok {
			return fmt.Errorf("%w: metric %q does not bind slot %q", ErrValidate, def.ID, slot)
		}
		inDef := snap.Defs[inID]
		if inDef == nil {
			return fmt.Errorf("%w: metric %q input %q: %w", ErrValidate, def.ID, inID, metric.ErrUnknownInput)
		}
		if st := snap.States[inID]; st != nil && st.Status == metric.StatusRetired {
			return fmt.Errorf("%w: metric %q input %q: %w", ErrValidate, def.ID, inID, metric.ErrRetired)
		}
		// Every output dimension must be covered by every input.
		inDimSet := map[string]struct{}{}
		for _, d := range inDef.Dims {
			inDimSet[d] = struct{}{}
		}
		for _, d := range def.Dims {
			if _, ok := inDimSet[d]; !ok {
				return fmt.Errorf("%w: metric %q dim %q missing in input %q: %w",
					ErrValidate, def.ID, d, inID, metric.ErrDimMismatch)
			}
		}
		if _, err := e.units.Lookup(inDef.Unit); err != nil {
			return fmt.Errorf("%w: input %q unknown unit %q", ErrValidate, inID, inDef.Unit)
		}
		inputDefs[slot] = inDef
	}

	// Unit inference over the AST.
	inferredVec, err := e.inferExpr(ex, inputDefs)
	if err != nil {
		return fmt.Errorf("%w: metric %q: %w", ErrValidate, def.ID, err)
	}
	declared, err := e.units.Lookup(def.Unit)
	if err != nil {
		return fmt.Errorf("%w: metric %q: %v", ErrValidate, def.ID, err)
	}
	if !sameVec(inferredVec, declared.Vector) {
		return fmt.Errorf("%w: metric %q declares unit %q %v but formula yields %v: %w",
			ErrValidate, def.ID, def.Unit, declared.Vector, inferredVec, metric.ErrUnitIncompatible)
	}
	return nil
}

func (e *Engine) inferExpr(ex *expr.Expr, inputDefs map[string]*metric.Definition) (map[string]int, error) {
	var walk func(expr.Node) (map[string]int, string, error)
	walk = func(n expr.Node) (map[string]int, string, error) {
		switch x := n.(type) {
		case *expr.NumberNode:
			return map[string]int{}, "", nil
		case *expr.VarNode:
			inDef := inputDefs[x.Name]
			u, err := e.units.Lookup(inDef.Unit)
			if err != nil {
				return nil, "", err
			}
			canon, err := e.units.Normalize(inDef.Unit)
			if err != nil {
				return nil, "", err
			}
			return u.Vector, canon, nil
		case *expr.UnaryNode:
			v, name, err := walk(x.Operand)
			return v, name, err
		case *expr.BinaryNode:
			va, _, err := walk(x.Lhs)
			if err != nil {
				return nil, "", err
			}
			vb, _, err := walk(x.Rhs)
			if err != nil {
				return nil, "", err
			}
			var res map[string]int
			switch x.Op {
			case "+", "-":
				if !sameVec(va, vb) {
					return nil, "", fmt.Errorf("%w: %q of %v and %v",
						metric.ErrUnitIncompatible, x.Op, va, vb)
				}
				res = va
			case "*":
				res = addVec(va, vb, 1)
			case "/":
				res = addVec(va, vb, -1)
			default:
				return nil, "", fmt.Errorf("unknown op %q", x.Op)
			}
			return res, e.units.CanonicalForVector(res), nil
		default:
			return nil, "", fmt.Errorf("unknown expression node")
		}
	}
	v, _, err := walk(ex.Root())
	return v, err
}

// Full recomputes every active derived metric from scratch.
func (e *Engine) Full(snap *Snapshot) (map[string]map[string]float64, error) {
	order, err := snap.Graph.TopoOrder()
	if err != nil {
		return nil, err
	}
	results := map[string]map[string]float64{}
	for _, id := range order {
		def := snap.Defs[id]
		if def == nil || def.Kind != metric.KindDerived {
			continue
		}
		if st := snap.States[id]; st != nil && st.Status == metric.StatusRetired {
			continue
		}
		vals, err := e.evalWith(snap, id, results)
		if err != nil {
			return nil, err
		}
		results[id] = vals
	}
	return results, nil
}

// Incremental recomputes only derived metrics impacted by the given changed
// root IDs (which may be base or derived metrics).  All other result maps are
// reused unchanged.
func (e *Engine) Incremental(snap *Snapshot, changed []string) (map[string]map[string]float64, []string, error) {
	affectedSet := map[string]struct{}{}
	for _, id := range changed {
		for _, d := range snap.Graph.Impact(id) {
			def := snap.Defs[d]
			if def == nil || def.Kind != metric.KindDerived {
				continue
			}
			if st := snap.States[d]; st != nil && st.Status == metric.StatusRetired {
				continue
			}
			affectedSet[d] = struct{}{}
		}
	}
	affected := make([]string, 0, len(affectedSet))
	for id := range affectedSet {
		affected = append(affected, id)
	}
	// sortTopo via graph: build result by walking full topo order.
	order, err := snap.Graph.TopoOrder()
	if err != nil {
		return nil, nil, err
	}
	results := make(map[string]map[string]float64, len(snap.Results))
	for k, v := range snap.Results {
		results[k] = v // untouched maps are shared, never mutated
	}
	var recomputed []string
	for _, id := range order {
		if _, ok := affectedSet[id]; !ok {
			continue
		}
		if st := snap.States[id]; st != nil && st.Status == metric.StatusRetired {
			delete(results, id)
			continue
		}
		vals, err := e.evalWith(snap, id, results)
		if err != nil {
			return nil, nil, err
		}
		results[id] = vals
		recomputed = append(recomputed, id)
	}
	return results, recomputed, nil
}

// EvalOne evaluates one derived metric using the snapshot's committed results
// for derived inputs.
func (e *Engine) EvalOne(snap *Snapshot, id string) (map[string]float64, error) {
	return e.evalWith(snap, id, snap.Results)
}

func (e *Engine) evalWith(snap *Snapshot, id string, derivedResults map[string]map[string]float64) (map[string]float64, error) {
	def := snap.Defs[id]
	if def == nil {
		return nil, fmt.Errorf("%w: %w: %s", ErrCompute, metric.ErrNotFound, id)
	}
	if def.Kind != metric.KindDerived {
		return nil, fmt.Errorf("%w: %s is not a derived metric", ErrCompute, id)
	}
	ex, err := expr.Parse(def.Formula)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCompute, id, err)
	}
	slots := ex.Inputs()

	// Project every input onto the output dimensions.
	projected := make([]map[string]float64, len(slots))
	slotDefs := make([]*metric.Definition, len(slots))
	keySets := make([]map[string]struct{}, len(slots))
	for i, slot := range slots {
		inID := def.Inputs[slot]
		inDef := snap.Defs[inID]
		if inDef == nil {
			return nil, fmt.Errorf("%w: %s: %w: %s", ErrCompute, id, metric.ErrUnknownInput, inID)
		}
		slotDefs[i] = inDef
		var raw map[string]float64
		if inDef.Kind == metric.KindBase {
			if st := snap.States[inID]; st != nil {
				raw = st.Data
			}
		} else {
			raw = derivedResults[inID]
		}
		pj, err := project(raw, inDef.Dims, def.Dims)
		if err != nil {
			return nil, fmt.Errorf("%w: %s input %s: %v", ErrCompute, id, inID, err)
		}
		projected[i] = pj
		keySets[i] = map[string]struct{}{}
		for k := range pj {
			keySets[i][k] = struct{}{}
		}
	}

	// Inner join of projected key sets.
	var keys []string
	for k := range keySets[0] {
		present := true
		for i := 1; i < len(keySets); i++ {
			if _, ok := keySets[i][k]; !ok {
				present = false
				break
			}
		}
		if present {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	out := make(map[string]float64, len(keys))
	for _, key := range keys {
		values := make(map[string]float64, len(slots))
		canonical := make(map[string]string, len(slots))
		for i, slot := range slots {
			inDef := slotDefs[i]
			v := projected[i][key]
			canon, err := e.units.Normalize(inDef.Unit)
			if err != nil {
				return nil, err
			}
			baseV, err := e.units.Convert(v, inDef.Unit, canon)
			if err != nil {
				return nil, err
			}
			values[slot] = baseV
			canonical[slot] = canon
		}
		raw, err := evalExprUnits(e, ex, values)
		if err != nil {
			if errors.Is(err, expr.ErrExpr) {
				continue // e.g. division by zero on this key: skip deterministically
			}
			return nil, fmt.Errorf("%w: %s: %v", ErrCompute, id, err)
		}
		// raw is in the canonical base unit of the inferred result vector.
		inferredName, err := resultCanonical(e, ex, slotDefs, slots)
		if err != nil {
			return nil, err
		}
		finalV, err := e.units.Convert(raw, inferredName, def.Unit)
		if err != nil {
			return nil, fmt.Errorf("%w: %s result unit: %v", ErrCompute, id, err)
		}
		out[key] = finalV
	}
	return out, nil
}

// evalExprUnits evaluates with all slot values already expressed in canonical
// base units; arithmetic therefore composes dimensions directly.
func evalExprUnits(e *Engine, ex *expr.Expr, values map[string]float64) (float64, error) {
	var walk func(expr.Node) (float64, error)
	walk = func(n expr.Node) (float64, error) {
		switch x := n.(type) {
		case *expr.NumberNode:
			return x.Value, nil
		case *expr.VarNode:
			v, ok := values[x.Name]
			if !ok {
				return 0, fmt.Errorf("%w: missing input %q", expr.ErrExpr, x.Name)
			}
			return v, nil
		case *expr.UnaryNode:
			v, err := walk(x.Operand)
			return -v, err
		case *expr.BinaryNode:
			l, err := walk(x.Lhs)
			if err != nil {
				return 0, err
			}
			r, err := walk(x.Rhs)
			if err != nil {
				return 0, err
			}
			switch x.Op {
			case "+":
				return l + r, nil
			case "-":
				return l - r, nil
			case "*":
				return l * r, nil
			case "/":
				if r == 0 {
					return 0, fmt.Errorf("%w: division by zero", expr.ErrExpr)
				}
				return l / r, nil
			}
			return 0, fmt.Errorf("unknown op %q", x.Op)
		}
		return 0, fmt.Errorf("unknown node")
	}
	return walk(ex.Root())
}

// resultCanonical derives the canonical result unit name of a formula given
// the resolved slot definitions.
func resultCanonical(e *Engine, ex *expr.Expr, slotDefs []*metric.Definition, slots []string) (string, error) {
	bind := map[string]*metric.Definition{}
	for i, s := range slots {
		bind[s] = slotDefs[i]
	}
	var walk func(expr.Node) (string, error)
	walk = func(n expr.Node) (string, error) {
		switch x := n.(type) {
		case *expr.NumberNode:
			return "", nil
		case *expr.VarNode:
			return e.units.Normalize(bind[x.Name].Unit)
		case *expr.UnaryNode:
			return walk(x.Operand)
		case *expr.BinaryNode:
			a, err := walk(x.Lhs)
			if err != nil {
				return "", err
			}
			b, err := walk(x.Rhs)
			if err != nil {
				return "", err
			}
			return e.units.InferUnit(rune(x.Op[0]), a, b)
		}
		return "", fmt.Errorf("unknown node")
	}
	return walk(ex.Root())
}

// project rolls raw points keyed by inDims onto outDims (which must be a
// subset), summing values that collapse onto the same projected key.
func project(raw map[string]float64, inDims, outDims []string) (map[string]float64, error) {
	if equalDims(inDims, outDims) {
		out := make(map[string]float64, len(raw))
		for k, v := range raw {
			out[k] = v
		}
		return out, nil
	}
	idx := make([]int, len(outDims))
	for i, d := range outDims {
		j := indexOf(inDims, d)
		if j < 0 {
			return nil, fmt.Errorf("%w: dimension %q not covered", metric.ErrDimMismatch, d)
		}
		idx[i] = j
	}
	out := map[string]float64{}
	for key, v := range raw {
		parts := splitKey(key)
		proj := make([]string, len(outDims))
		for i, j := range idx {
			proj[i] = parts[j]
		}
		out[metric.Key(proj)] += v
	}
	return out, nil
}

func sameVec(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func addVec(a, b map[string]int, signB int) map[string]int {
	out := map[string]int{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] += v * signB
		if out[k] == 0 {
			delete(out, k)
		}
	}
	return out
}

func uniqueSorted(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func equalDims(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func splitKey(key string) []string {
	if key == "" {
		return []string{""}
	}
	return strings.Split(key, "\x00")
}
