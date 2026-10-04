// Package metric holds core metric definition and version types.
package metric

import "errors"

// Kind distinguishes base metrics from derived metrics.
type Kind string

const (
	KindBase    Kind = "base"
	KindDerived Kind = "derived"
)

// Status marks whether a metric is active or retired (offline).
type Status string

const (
	StatusActive  Status = "active"
	StatusRetired Status = "retired"
)

// Definition is a single immutable version of a metric definition.
//
// A Definition value never changes once committed: registering changed
// content under the same ID produces a new Definition with Version+1.
type Definition struct {
	ID      string   `json:"id"`      // stable identifier across versions
	Version int      `json:"version"` // 1-based version number
	Kind    Kind     `json:"kind"`
	Spec    string   `json:"spec"` // caliber / human-readable specification
	Unit    string   `json:"unit"` // declared unit name
	Dims    []string `json:"dims"` // business dimensions
	// Formula holds the derived expression; empty for base metrics.
	Formula string `json:"formula,omitempty"`
	// Inputs maps expression input slots ("a","b",...) to metric IDs.
	Inputs map[string]string `json:"inputs,omitempty"`
}

// Clone returns a deep copy of the definition.
func (d *Definition) Clone() *Definition {
	if d == nil {
		return nil
	}
	cp := *d
	cp.Dims = append([]string(nil), d.Dims...)
	if d.Inputs != nil {
		cp.Inputs = make(map[string]string, len(d.Inputs))
		for k, v := range d.Inputs {
			cp.Inputs[k] = v
		}
	}
	return &cp
}

// Point is a single dimension-keyed data value.
type Point struct {
	DimValues []string `json:"dim_values"` // aligned with the metric dimensions
	Value     float64  `json:"value"`
}

// State is the committed state of a metric (current version).
type State struct {
	Def    *Definition        `json:"def"`
	Status Status             `json:"status"`
	Data   map[string]float64 `json:"data,omitempty"` // base data keyed by dim tuple
}

// Key builds the canonical dimension-tuple key from dimension values.
func Key(dimValues []string) string {
	// Values are joined with NUL, which never occurs in ordinary dim values.
	key := ""
	for i, v := range dimValues {
		if i > 0 {
			key += "\x00"
		}
		key += v
	}
	return key
}

var (
	// ErrNotFound means the metric or version does not exist.
	ErrNotFound = errors.New("metric not found")
	// ErrAlreadyExists means an identical-content guard rejected a duplicate.
	ErrAlreadyExists = errors.New("metric already exists")
	// ErrBadDefinition means the definition payload is structurally invalid.
	ErrBadDefinition = errors.New("bad metric definition")
	// ErrCycle means registering a derived metric would create a cycle.
	ErrCycle = errors.New("cycle in metric lineage")
	// ErrUnitIncompatible means two units cannot participate in the operation.
	ErrUnitIncompatible = errors.New("incompatible units")
	// ErrDimMismatch means derived dims are not covered by every input.
	ErrDimMismatch = errors.New("dimension mismatch")
	// ErrUnknownInput means a formula references an unregistered metric.
	ErrUnknownInput = errors.New("unknown input metric")
	// ErrRetired means an operation referenced a retired (offline) metric.
	ErrRetired = errors.New("metric is retired")
)
