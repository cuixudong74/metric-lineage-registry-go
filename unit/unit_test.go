package unit

import (
	"errors"
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestConversion(t *testing.T) {
	s := NewSystem()
	cases := []struct {
		v        float64
		from, to string
		want     float64
	}{
		{1, "km", "m", 1000},
		{1, "h", "s", 3600},
		{1, "day", "min", 1440},
		{100, "CNY", "USD", 14},
		{1, "m", "cm", 100},
		{0, "C", "K", 273.15}, // affine
		{32, "F", "C", 0},
		{1, "percent", "1", 0.01},
	}
	for _, tc := range cases {
		got, err := s.Convert(tc.v, tc.from, tc.to)
		if err != nil {
			t.Fatalf("convert %s->%s: %v", tc.from, tc.to, err)
		}
		if !approx(got, tc.want) {
			t.Fatalf("%g %s in %s = %g, want %g", tc.v, tc.from, tc.to, got, tc.want)
		}
	}
}

func TestIncompatible(t *testing.T) {
	s := NewSystem()
	if s.Compatible("m", "s") {
		t.Fatal("m and s must be incompatible")
	}
	if _, err := s.Convert(1, "m", "s"); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("convert m->s err = %v", err)
	}
	if _, err := s.InferUnit('+', "m", "s"); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("m+s infer err = %v", err)
	}
}

func TestInferAndCompound(t *testing.T) {
	s := NewSystem()
	v, err := s.InferUnit('/', "m", "s")
	if err != nil {
		t.Fatal(err)
	}
	if v != "m/s" {
		t.Fatalf("m/s inferred as %q", v)
	}
	v2, err := s.InferUnit('*', "m", "m")
	if err != nil {
		t.Fatal(err)
	}
	if v2 != "m^2" {
		t.Fatalf("m*m inferred as %q", v2)
	}
	// km/h compound lookup: 1 km/h = 1000/3600 m/s
	c, err := s.Convert(3.6, "km/h", "m/s")
	if err != nil {
		t.Fatal(err)
	}
	if !approx(c, 1) {
		t.Fatalf("3.6 km/h = %v m/s, want 1", c)
	}
	// USD/USD cancels to dimensionless.
	name, err := s.InferUnit('/', "USD", "USD")
	if err != nil {
		t.Fatal(err)
	}
	if name != "" {
		t.Fatalf("USD/USD = %q, want dimensionless \"\"", name)
	}
	norm, err := s.Normalize("km")
	if err != nil || norm != "m" {
		t.Fatalf("normalize km = %q %v", norm, err)
	}
}

func TestCustomUnit(t *testing.T) {
	s := NewSystem()
	if err := s.Register(&Unit{Name: "ft", Factor: 0.3048, Vector: map[string]int{"length": 1}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Convert(1, "ft", "m")
	if err != nil || !approx(got, 0.3048) {
		t.Fatalf("ft->m = %v %v", got, err)
	}
	if err := s.Register(&Unit{Name: "ft"}); !errors.Is(err, ErrDuplicateUnit) {
		t.Fatalf("dup unit err = %v", err)
	}
}
