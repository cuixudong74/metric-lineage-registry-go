package expr

import (
	"errors"
	"math"
	"testing"
)

func TestParseAndEval(t *testing.T) {
	cases := []struct {
		src  string
		in   map[string]float64
		want float64
	}{
		{"1 + 2*3", nil, 7},
		{"(1+2)*3", nil, 9},
		{"a/b", map[string]float64{"a": 10, "b": 4}, 2.5},
		{"-a + 2", map[string]float64{"a": 3}, -1},
		{"a*b+c", map[string]float64{"a": 2, "b": 3, "c": 1}, 7},
		{"10/(a-a)", map[string]float64{"a": 1}, 0},
	}
	for _, tc := range cases {
		ex, err := Parse(tc.src)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.src, err)
		}
		got, err := ex.Eval(tc.in)
		if tc.want == 0 && errors.Is(err, ErrExpr) {
			continue // division by zero case
		}
		if err != nil {
			t.Fatalf("eval %q: %v", tc.src, err)
		}
		if math.Abs(got-tc.want) > 1e-12 {
			t.Fatalf("%s = %v, want %v", tc.src, got, tc.want)
		}
	}
}

func TestInputsFirstUse(t *testing.T) {
	ex, err := Parse("b + a*b + c")
	if err != nil {
		t.Fatal(err)
	}
	got := ex.Inputs()
	want := []string{"b", "a", "c"}
	if len(got) != len(want) {
		t.Fatalf("inputs = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("inputs = %v, want %v", got, want)
		}
	}
	if ex.String() == "" {
		t.Fatal("empty normalized string")
	}
}

func TestParseErrors(t *testing.T) {
	for _, src := range []string{"", "   ", "1 +", "(1+2", "a $ b", "1 2"} {
		if _, err := Parse(src); err == nil {
			t.Fatalf("Parse(%q) succeeded", src)
		}
	}
}
