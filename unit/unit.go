// Package unit models measurement units, dimension compatibility checks and
// explicit linear (plus affine, for temperature) conversion rules.
//
// Every unit has:
//
//   - a dimensional vector over base dimensions (length, time, mass, ...);
//
//   - Factor and Offset defining conversion to the dimension's base unit:
//
//     baseValue = value*Factor + Offset
//     value     = (baseValue - Offset)/Factor
//
// Two units are compatible iff their vectors are equal.  Arithmetic result
// dimensions follow the rules:
//
//	a + b / a - b : vectors must be equal; result keeps that vector
//	a * b         : vectors add
//	a / b         : vectors subtract
//
// Numeric literals are dimensionless.  Compound unit names such as "m/s" or
// "m^2" are parsed as products of registered simple units; the expression has
// the conventional form numerator*.../denominator*....
package unit

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Unit describes a measurement unit.
type Unit struct {
	Name   string
	Vector map[string]int // dimensional vector, e.g. {"length":1}
	Factor float64        // factor to a base unit of the same vector
	Offset float64        // affine offset to the base unit (temperature)
}

func (u *Unit) clone() *Unit {
	c := &Unit{Name: u.Name, Factor: u.Factor, Offset: u.Offset, Vector: map[string]int{}}
	for k, v := range u.Vector {
		c.Vector[k] = v
	}
	return c
}

// Errors.
var (
	ErrUnit          = errors.New("unit error")
	ErrUnknownUnit   = fmt.Errorf("%w: unknown unit", ErrUnit)
	ErrIncompatible  = fmt.Errorf("%w: incompatible units", ErrUnit)
	ErrDuplicateUnit = fmt.Errorf("%w: duplicate unit name", ErrUnit)
	ErrBadUnitDef    = fmt.Errorf("%w: bad unit definition", ErrUnit)
)

// System is a registry of known units.
type System struct {
	units     map[string]*Unit
	baseNames map[string]string // dimension -> canonical base unit name
}

// NewSystem returns a System preloaded with built-in units.
func NewSystem() *System {
	s := &System{
		units:     map[string]*Unit{},
		baseNames: map[string]string{},
	}
	s.builtins()
	return s
}

func (s *System) add(name string, factor, offset float64, vector map[string]int, base bool) {
	u := &Unit{Name: name, Factor: factor, Offset: offset, Vector: vector}
	s.units[name] = u
	if base {
		for dim := range vector {
			s.baseNames[dim] = name
		}
	}
}

func (s *System) linear(name string, factor float64, vector map[string]int) {
	if factor == 0 {
		factor = 1
	}
	s.add(name, factor, 0, vector, false)
}

func (s *System) base(name string, vector map[string]int) {
	s.add(name, 1, 0, vector, true)
}

func dimVec(dim string, exp int) map[string]int { return map[string]int{dim: exp} }

var emptyVec = map[string]int{}

func (s *System) builtins() {
	// Dimensionless units.
	s.base("", emptyVec)
	for _, n := range []string{"1", "count", "ratio", "times"} {
		s.linear(n, 1, emptyVec)
	}
	s.linear("percent", 0.01, emptyVec) // 1% = 0.01
	s.linear("pct", 0.01, emptyVec)

	// Length, base unit meter.
	s.base("m", dimVec("length", 1))
	s.linear("km", 1000, dimVec("length", 1))
	s.linear("cm", 0.01, dimVec("length", 1))
	s.linear("mm", 0.001, dimVec("length", 1))

	// Time, base unit second.
	s.base("s", dimVec("time", 1))
	s.linear("min", 60, dimVec("time", 1))
	s.linear("h", 3600, dimVec("time", 1))
	s.linear("day", 86400, dimVec("time", 1))
	s.linear("d", 86400, dimVec("time", 1))

	// Mass, base unit kilogram.
	s.base("kg", dimVec("mass", 1))
	s.linear("g", 0.001, dimVec("mass", 1))
	s.linear("t", 1000, dimVec("mass", 1))

	// Currency, explicit fixed conversion rules, base USD.
	s.base("USD", dimVec("currency", 1))
	s.linear("CNY", 0.14, dimVec("currency", 1))
	s.linear("EUR", 1.08, dimVec("currency", 1))

	// Temperature, affine units with base kelvin.
	s.add("K", 1, 0, dimVec("temperature", 1), true)
	s.add("C", 1, 273.15, dimVec("temperature", 1), false)
	s.add("F", 5.0/9.0, 273.15-32*5.0/9.0, dimVec("temperature", 1), false)
}

// Register registers a custom linear unit under a new name.
func (s *System) Register(u *Unit) error {
	if u == nil || u.Name == "" {
		return fmt.Errorf("%w: name required", ErrBadUnitDef)
	}
	if _, ok := s.units[u.Name]; ok {
		return fmt.Errorf("%w: %s", ErrDuplicateUnit, u.Name)
	}
	if u.Factor <= 0 {
		u.Factor = 1
	}
	if u.Vector == nil {
		u.Vector = map[string]int{}
	}
	s.units[u.Name] = u.clone()
	return nil
}

// Lookup returns a defensive copy of the named unit.  The empty string and
// compound expressions such as "m/s" are accepted.
func (s *System) Lookup(name string) (*Unit, error) {
	if u, ok := s.units[name]; ok {
		return u.clone(), nil
	}
	u, err := s.evalCompound(name)
	if err != nil {
		return nil, err
	}
	return u.clone(), nil
}

// Compatible reports whether two units share the same dimensional vector.
func (s *System) Compatible(a, b string) bool {
	ua, err := s.Lookup(a)
	if err != nil {
		return false
	}
	ub, err := s.Lookup(b)
	if err != nil {
		return false
	}
	return sameVector(ua.Vector, ub.Vector)
}

// Convert converts a value from one unit to another compatible unit.
func (s *System) Convert(v float64, from, to string) (float64, error) {
	ua, err := s.Lookup(from)
	if err != nil {
		return 0, err
	}
	ub, err := s.Lookup(to)
	if err != nil {
		return 0, err
	}
	if !sameVector(ua.Vector, ub.Vector) {
		return 0, fmt.Errorf("%w: %s vs %s", ErrIncompatible, from, to)
	}
	base := v*ua.Factor + ua.Offset
	return (base - ub.Offset) / ub.Factor, nil
}

// VectorOf returns the dimensional vector of a unit.
func (s *System) VectorOf(name string) (map[string]int, error) {
	u, err := s.Lookup(name)
	if err != nil {
		return nil, err
	}
	return u.Vector, nil
}

// InferUnit infers the canonical result unit name of an arithmetic operation.
// op is one of '+', '-', '*', '/'.
func (s *System) InferUnit(op rune, a, b string) (string, error) {
	ua, err := s.Lookup(a)
	if err != nil {
		return "", err
	}
	ub, err := s.Lookup(b)
	if err != nil {
		return "", err
	}
	va, vb := ua.Vector, ub.Vector
	switch op {
	case '+', '-':
		if !sameVector(va, vb) {
			return "", fmt.Errorf("%w: cannot %s %q and %q", ErrIncompatible, string(op), a, b)
		}
		return s.canonical(va), nil
	case '*':
		return s.canonical(addVector(va, vb, 1)), nil
	case '/':
		return s.canonical(addVector(va, vb, -1)), nil
	default:
		return "", fmt.Errorf("%w: unknown operator %q", ErrBadUnitDef, string(op))
	}
}

// Normalize returns the canonical base-unit name for a unit's vector.
func (s *System) Normalize(name string) (string, error) {
	u, err := s.Lookup(name)
	if err != nil {
		return "", err
	}
	return s.CanonicalForVector(u.Vector), nil
}

// CanonicalForVector returns the canonical base-unit name for an explicit
// dimensional vector (e.g. {length:1,time:-1} -> "m/s").
func (s *System) CanonicalForVector(v map[string]int) string {
	return s.canonical(v)
}

func sameVector(a, b map[string]int) bool {
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

func addVector(a, b map[string]int, signB int) map[string]int {
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

// canonical builds the base-unit name for a vector (e.g. length/time -> "m/s")
// and registers a synthetic unit with factor 1 so it can be looked up.
func (s *System) canonical(v map[string]int) string {
	if len(v) == 0 {
		return ""
	}
	dims := make([]string, 0, len(v))
	for d := range v {
		dims = append(dims, d)
	}
	sort.Strings(dims)
	var num, den []string
	for _, d := range dims {
		base, ok := s.baseNames[d]
		if !ok {
			base = d
		}
		e := v[d]
		if e > 0 {
			num = append(num, exponentName(base, e))
		} else {
			den = append(den, exponentName(base, -e))
		}
	}
	name := strings.Join(num, "*")
	if name == "" {
		name = "1"
	}
	if len(den) > 0 {
		name += "/" + strings.Join(den, "*")
	}
	if _, ok := s.units[name]; !ok {
		s.units[name] = &Unit{Name: name, Factor: 1, Vector: cloneVector(v)}
	}
	return name
}

func exponentName(base string, exp int) string {
	if exp == 1 {
		return base
	}
	return fmt.Sprintf("%s^%d", base, exp)
}

func cloneVector(v map[string]int) map[string]int {
	c := map[string]int{}
	for k, val := range v {
		c[k] = val
	}
	return c
}

// evalCompound parses names like "m/s", "m^2" or "USD/USD".  The resulting
// synthetic unit is cached under the requested spelling.
func (s *System) evalCompound(name string) (*Unit, error) {
	if name == "" {
		return s.units[""].clone(), nil
	}
	if u, ok := s.units[name]; ok {
		return u, nil
	}
	p := &compoundParser{src: name, s: s}
	u, err := p.parse()
	if err != nil {
		return nil, err
	}
	s.units[name] = u
	return u.clone(), nil
}

// compoundParser parses unit strings of the conventional form
// factor*factor/denominator*denominator, with optional ^n exponents.  After
// the first '/', every '*' term is a denominator until a second '/' returns
// to the numerator side.
type compoundParser struct {
	s   *System
	src string
	pos int
}

func (p *compoundParser) skipSpaces() {
	for p.pos < len(p.src) && p.src[p.pos] == ' ' {
		p.pos++
	}
}

func (p *compoundParser) readAtom() (string, int, error) {
	p.skipSpaces()
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '*' || c == '/' || c == '^' || c == ' ' {
			break
		}
		p.pos++
	}
	if p.pos == start {
		return "", 0, fmt.Errorf("%w: bad unit expression %q", ErrUnknownUnit, p.src)
	}
	name := p.src[start:p.pos]
	exp := 1
	p.skipSpaces()
	if p.pos < len(p.src) && p.src[p.pos] == '^' {
		p.pos++
		p.skipSpaces()
		ns := p.pos
		for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
			p.pos++
		}
		if ns == p.pos {
			return "", 0, fmt.Errorf("%w: bad exponent in %q", ErrUnknownUnit, p.src)
		}
		v := 0
		for _, c := range p.src[ns:p.pos] {
			v = v*10 + int(c-'0')
		}
		exp = v
	}
	return name, exp, nil
}

func (p *compoundParser) parse() (*Unit, error) {
	vector := map[string]int{}
	factor := 1.0
	denominator := false
	first := true
	for {
		p.skipSpaces()
		if p.pos >= len(p.src) {
			break
		}
		if !first {
			op := p.src[p.pos]
			if op != '*' && op != '/' {
				return nil, fmt.Errorf("%w: expected * or / in %q", ErrUnknownUnit, p.src)
			}
			p.pos++
			if op == '/' {
				denominator = !denominator
			}
		}
		first = false
		name, exp, err := p.readAtom()
		if err != nil {
			return nil, err
		}
		u, ok := p.s.units[name]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrUnknownUnit, name)
		}
		sign := 1
		if denominator {
			sign = -1
		}
		for d, e := range u.Vector {
			vector[d] += e * exp * sign
			if vector[d] == 0 {
				delete(vector, d)
			}
		}
		factor *= powN(u.Factor, exp*sign)
	}
	return &Unit{Name: p.src, Factor: factor, Offset: 0, Vector: vector}, nil
}

func powN(base float64, n int) float64 {
	if n < 0 {
		return 1 / powN(base, -n)
	}
	r := 1.0
	for i := 0; i < n; i++ {
		r *= base
	}
	return r
}
