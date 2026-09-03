package responsive_test

import (
	"math"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"pgregory.net/rapid"
)

func TestResolveClassificationBoundaries(t *testing.T) {
	tests := []struct {
		name string
		size responsive.Size
		want responsive.Class
	}{
		{"recovery below columns", responsive.Size{Columns: 39, Rows: 24}, responsive.Recovery},
		{"recovery below rows", responsive.Size{Columns: 110, Rows: 9}, responsive.Recovery},
		{"compact lower boundary", responsive.Size{Columns: 40, Rows: 10}, responsive.Compact},
		{"compact below columns", responsive.Size{Columns: 79, Rows: 24}, responsive.Compact},
		{"compact below rows", responsive.Size{Columns: 110, Rows: 17}, responsive.Compact},
		{"standard lower boundary", responsive.Size{Columns: 80, Rows: 18}, responsive.Standard},
		{"standard below columns", responsive.Size{Columns: 109, Rows: 24}, responsive.Standard},
		{"standard below rows", responsive.Size{Columns: 110, Rows: 23}, responsive.Standard},
		{"wide lower boundary", responsive.Size{Columns: 110, Rows: 24}, responsive.Wide},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := responsive.Resolve(test.size).Class; got != test.want {
				t.Fatalf("Resolve(%+v).Class = %q, want %q", test.size, got, test.want)
			}
		})
	}
}

func TestResolveProjectionBoundaries(t *testing.T) {
	tests := []struct {
		name           string
		size           responsive.Size
		wantRender     responsive.Size
		wantProjected  bool
		wantFunctional bool
	}{
		{"zero columns", responsive.Size{Columns: 0, Rows: 24}, responsive.Size{Columns: 0, Rows: 24}, false, false},
		{"one column", responsive.Size{Columns: 1, Rows: 24}, responsive.Size{Columns: 1, Rows: 24}, false, false},
		{"maximum columns", responsive.Size{Columns: 500, Rows: 24}, responsive.Size{Columns: 500, Rows: 24}, false, true},
		{"projected columns", responsive.Size{Columns: 501, Rows: 24}, responsive.Size{Columns: 500, Rows: 24}, true, false},
		{"zero rows", responsive.Size{Columns: 110, Rows: 0}, responsive.Size{Columns: 110, Rows: 0}, false, false},
		{"one row", responsive.Size{Columns: 110, Rows: 1}, responsive.Size{Columns: 110, Rows: 1}, false, false},
		{"maximum rows", responsive.Size{Columns: 110, Rows: 200}, responsive.Size{Columns: 110, Rows: 200}, false, true},
		{"projected rows", responsive.Size{Columns: 110, Rows: 201}, responsive.Size{Columns: 110, Rows: 200}, true, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layout := responsive.Resolve(test.size)
			if layout.Render != test.wantRender {
				t.Fatalf("Resolve(%+v).Render = %+v, want %+v", test.size, layout.Render, test.wantRender)
			}
			if layout.Projected != test.wantProjected || layout.Functional != test.wantFunctional {
				t.Fatalf("Resolve(%+v) flags = functional %v projected %v, want functional %v projected %v", test.size, layout.Functional, layout.Projected, test.wantFunctional, test.wantProjected)
			}
		})
	}
}

func TestResolveExhaustiveSupportedEnvelope(t *testing.T) {
	for cols := 1; cols <= 500; cols++ {
		for rows := 1; rows <= 200; rows++ {
			size := responsive.Size{Columns: cols, Rows: rows}
			layout := responsive.Resolve(size)
			if layout.Class != referenceClass(size) {
				t.Fatalf("Resolve(%+v).Class = %q, want %q", size, layout.Class, referenceClass(size))
			}
			if layout.Render != size {
				t.Fatalf("Resolve(%+v).Render = %+v, want identity", size, layout.Render)
			}
			if layout.Reported != size {
				t.Fatalf("Resolve(%+v).Reported = %+v", size, layout.Reported)
			}
			wantFunctional := cols >= 40 && rows >= 10
			if layout.Functional != wantFunctional || layout.Projected {
				t.Fatalf("Resolve(%+v) flags = functional %v projected %v", size, layout.Functional, layout.Projected)
			}
		}
	}
}

func TestResolveBoundsExceptionalDimensions(t *testing.T) {
	tests := []struct {
		name string
		size responsive.Size
		want responsive.Size
	}{
		{"zero", responsive.Size{}, responsive.Size{}},
		{"negative", responsive.Size{Columns: -1, Rows: -2}, responsive.Size{}},
		{"minimum integers", responsive.Size{Columns: math.MinInt, Rows: math.MinInt}, responsive.Size{}},
		{"maximum integers", responsive.Size{Columns: math.MaxInt, Rows: math.MaxInt}, responsive.Size{Columns: 500, Rows: 200}},
		{"columns only", responsive.Size{Columns: 501, Rows: 199}, responsive.Size{Columns: 500, Rows: 199}},
		{"rows only", responsive.Size{Columns: 499, Rows: 201}, responsive.Size{Columns: 499, Rows: 200}},
		{"mixed signs", responsive.Size{Columns: math.MaxInt, Rows: math.MinInt}, responsive.Size{Columns: 500, Rows: 0}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layout := responsive.Resolve(test.size)
			if layout.Render != test.want {
				t.Fatalf("Resolve(%+v).Render = %+v, want %+v", test.size, layout.Render, test.want)
			}
			if layout.Class != referenceClass(test.size) {
				t.Fatalf("Resolve(%+v).Class = %q, want %q", test.size, layout.Class, referenceClass(test.size))
			}
			wantProjected := test.size.Columns > 500 || test.size.Rows > 200
			if layout.Projected != wantProjected {
				t.Fatalf("Resolve(%+v).Projected = %v", test.size, layout.Projected)
			}
		})
	}
}

func TestResolveAlwaysReturnsBoundedDeterministicLayout(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		size := responsive.Size{
			Columns: rapid.Int().Draw(t, "cols"),
			Rows:    rapid.Int().Draw(t, "rows"),
		}
		first := responsive.Resolve(size)
		second := responsive.Resolve(size)

		if first != second {
			t.Fatalf("Resolve(%+v) is nondeterministic: %+v then %+v", size, first, second)
		}
		if first.Class != referenceClass(size) {
			t.Fatalf("Resolve(%+v).Class = %q, want %q", size, first.Class, referenceClass(size))
		}
		if first.Render.Columns < 0 || first.Render.Columns > 500 || first.Render.Rows < 0 || first.Render.Rows > 200 {
			t.Fatalf("Resolve(%+v).Render is out of bounds: %+v", size, first.Render)
		}
		if first.Render.Columns != referenceDimension(size.Columns, 500) || first.Render.Rows != referenceDimension(size.Rows, 200) {
			t.Fatalf("Resolve(%+v).Render = %+v, want independent projection", size, first.Render)
		}
	})
}

func referenceClass(size responsive.Size) responsive.Class {
	if size.Columns < 40 || size.Rows < 10 {
		return responsive.Recovery
	}
	if size.Columns < 80 || size.Rows < 18 {
		return responsive.Compact
	}
	if size.Columns < 110 || size.Rows < 24 {
		return responsive.Standard
	}
	return responsive.Wide
}

func referenceDimension(value, maximum int) int {
	if value <= 0 {
		return 0
	}
	if value > maximum {
		return maximum
	}
	return value
}
