package view

import (
	"math"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
)

func TestDimensionsOverflowExactBoundary(t *testing.T) {
	for _, test := range []struct {
		name                 string
		width, height, limit int
		want                 bool
	}{
		{name: "exact capacity", width: 4, height: 2, limit: 8, want: false},
		{name: "one cell over", width: 5, height: 2, limit: 8, want: true},
		{name: "zero height", width: 9, height: 0, limit: 8, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := dimensionsOverflow(test.width, test.height, test.limit); got != test.want {
				t.Fatalf("dimensionsOverflow(%d, %d, %d) = %v, want %v", test.width, test.height, test.limit, got, test.want)
			}
		})
	}
}

func TestOverwritingWideGlyphContinuationClearsItsLead(t *testing.T) {
	frame, err := NewFrame(4, 1)
	if err != nil {
		t.Fatal(err)
	}
	frame.PutText(1, 0, "界", Style{})
	frame.PutText(2, 0, "A", Style{})
	lead, _ := frame.CellAt(1, 0)
	replacement, _ := frame.CellAt(2, 0)
	if lead != (Cell{}) || replacement.Text != "A" || replacement.Continuation {
		t.Fatalf("overwritten glyph cells = lead %#v, replacement %#v", lead, replacement)
	}
}

func TestTruncateWithoutMarkerRejectsZeroWidthBudgetBeforeReadingGraphemes(t *testing.T) {
	if got := truncateWithoutMarker("\u0301secret", 0); got != "" {
		t.Fatalf("zero-width budget returned %q", got)
	}
}

func textFitID(t *testing.T, value string) diagnostics.ID {
	t.Helper()
	id, err := diagnostics.NewID(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPutTextBoxCorrectedLayoutAndLossCases(t *testing.T) {
	cases := []struct {
		options                     TextBoxOptions
		lines                       []string
		wrapped, truncated, clipped bool
		originalColumns, layoutRows int
	}{
		{TextBoxOptions{Element: textFitID(t, "ascii"), Width: 2, Height: 1, Mode: TextClip}, []string{"abcd"}, false, false, true, 4, 1},
		{TextBoxOptions{Element: textFitID(t, "vertical"), X: 2, Width: 2, Height: 2, Mode: TextClip}, []string{"A", "B", "C"}, false, false, true, 1, 3},
		{TextBoxOptions{Element: textFitID(t, "unicode"), X: 4, Width: 2, Height: 2, Mode: TextWrap}, []string{"界a"}, true, false, false, 3, 2},
		{TextBoxOptions{Element: textFitID(t, "control"), X: 7, Width: 2, Height: 1, Mode: TextClip}, []string{"x\x00y"}, false, false, false, 2, 1},
		{TextBoxOptions{Element: textFitID(t, "zero"), Width: 0, Height: 1, Mode: TextWrap}, []string{"x"}, false, false, true, 1, 1},
		{TextBoxOptions{Element: textFitID(t, "truncate-zero"), Width: 0, Height: 1, Mode: TextTruncate}, []string{"x"}, false, true, true, 1, 1},
		{TextBoxOptions{Element: textFitID(t, "truncate-empty"), Width: 0, Height: 1, Mode: TextTruncate}, []string{""}, false, false, false, 0, 1},
		{TextBoxOptions{Element: textFitID(t, "wide"), Width: 1, Height: 1, Mode: TextWrap}, []string{"界"}, false, false, true, 2, 1},
		{TextBoxOptions{Element: textFitID(t, "empty"), Width: 1, Height: 1, Mode: TextClip}, []string{"", ""}, false, false, false, 0, 2},
		{TextBoxOptions{Element: textFitID(t, "mixed"), Width: 1, Height: 2, Mode: TextClip}, []string{"A", "", "B"}, false, false, true, 1, 3},
		{TextBoxOptions{Element: textFitID(t, "intentional"), X: 9, Width: 2, Height: 1, Mode: TextTruncate, AllowTruncation: true}, []string{"abcd"}, false, true, false, 4, 1},
	}
	for _, test := range cases {
		frame, err := NewFrame(12, 8)
		if err != nil {
			t.Fatal(err)
		}
		if err := frame.PutTextBox(test.options, test.lines, Style{}); err != nil {
			t.Fatalf("PutTextBox(%s): %v", test.options.Element.String(), err)
		}
		got := frame.TextFit().Observations[0]
		if got.Wrapped != test.wrapped || got.Truncated != test.truncated || got.Clipped != test.clipped ||
			got.OriginalColumns != test.originalColumns || got.LayoutRows != test.layoutRows ||
			got.AvailableColumns != test.options.Width || got.AvailableRows != test.options.Height ||
			got.Intent != diagnostics.TextFitIntent(test.options.Mode) || got.AllowTruncation != test.options.AllowTruncation {
			t.Fatalf("observation %s = %+v", test.options.Element.String(), got)
		}
	}

	frame, _ := NewFrame(8, 2)
	_ = frame.PutTextBox(TextBoxOptions{Element: textFitID(t, "rendered"), Width: 2, Height: 1, Mode: TextTruncate}, []string{"abcd"}, Style{})
	_ = frame.PutTextBox(TextBoxOptions{Element: textFitID(t, "rendered-wrap"), X: 3, Width: 2, Height: 2, Mode: TextWrap}, []string{"界a"}, Style{})
	_ = frame.PutTextBox(TextBoxOptions{Element: textFitID(t, "rendered-control"), X: 6, Width: 2, Height: 1, Mode: TextClip}, []string{"x\x00y"}, Style{})
	assertCellText(t, frame, 0, 0, "a")
	assertCellText(t, frame, 1, 0, "…")
	assertCellText(t, frame, 3, 0, "界")
	assertCellText(t, frame, 3, 1, "a")
	assertCellText(t, frame, 6, 0, "x")
	assertCellText(t, frame, 7, 0, "y")
}

func TestPutTextBoxFrameEdgesCoordinatesAndOverflow(t *testing.T) {
	frame, err := NewFrame(4, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id      string
		options TextBoxOptions
		lines   []string
	}{
		{"right", TextBoxOptions{X: 3, Width: 2, Height: 1, Mode: TextClip}, []string{"ab"}},
		{"negative", TextBoxOptions{X: -1, Y: 1, Width: 2, Height: 1, Mode: TextClip}, []string{"ab"}},
		{"x-overflow", TextBoxOptions{X: math.MaxInt, Width: math.MaxInt, Height: 1, Mode: TextClip}, []string{"x"}},
		{"y-overflow", TextBoxOptions{Y: math.MaxInt, Width: 1, Height: 2, Mode: TextClip}, []string{"", "x"}},
	} {
		test.options.Element = textFitID(t, test.id)
		if err := frame.PutTextBox(test.options, test.lines, Style{}); err != nil {
			t.Fatalf("PutTextBox(%s): %v", test.id, err)
		}
	}
	report := frame.TextFit()
	for index, observation := range report.Observations {
		if !observation.Clipped {
			t.Fatalf("observation %d did not report frame clipping: %+v", index, observation)
		}
	}
	assertCellText(t, frame, 3, 0, "a")
	assertCellText(t, frame, 0, 1, "b")
}

func TestPutTextBoxBoundsCopyValidationAndGraphemes(t *testing.T) {
	frame, err := NewFrame(4, 2)
	if err != nil {
		t.Fatal(err)
	}
	id := textFitID(t, "bounded")
	for instance := range diagnostics.MaxTextFitObservations + 1 {
		if err := frame.PutTextBox(TextBoxOptions{Element: id, Instance: instance, Width: 0, Height: 0, Mode: TextClip}, []string{""}, Style{}); err != nil {
			t.Fatal(err)
		}
	}
	report := frame.TextFit()
	if len(report.Observations) != diagnostics.MaxTextFitObservations || report.Omitted != 1 {
		t.Fatalf("bounded report = observations %d omitted %d", len(report.Observations), report.Omitted)
	}
	report.Observations[0].Instance = 999
	if frame.TextFit().Observations[0].Instance != 0 {
		t.Fatal("TextFit returned an aliased observation slice")
	}

	graphemeFrame, _ := NewFrame(2, 2)
	if err := graphemeFrame.PutTextBox(TextBoxOptions{Element: id, Width: 1, Height: 2, Mode: TextWrap}, []string{"e\u0301a"}, Style{}); err != nil {
		t.Fatal(err)
	}
	assertCellText(t, graphemeFrame, 0, 0, "e\u0301")
	assertCellText(t, graphemeFrame, 0, 1, "a")

	for _, options := range []TextBoxOptions{
		{Width: 1, Height: 1, Mode: TextClip},
		{Element: id, Instance: -1, Width: 1, Height: 1, Mode: TextClip},
		{Element: id, Width: -1, Height: 1, Mode: TextClip},
		{Element: id, Width: 1, Height: -1, Mode: TextClip},
		{Element: id, Width: 1, Height: 1, Mode: "scroll"},
	} {
		if err := frame.PutTextBox(options, []string{"x"}, Style{}); err == nil {
			t.Fatalf("PutTextBox accepted invalid options: %+v", options)
		}
	}
}

func assertCellText(t *testing.T, frame *Frame, x, y int, want string) {
	t.Helper()
	cell, ok := frame.CellAt(x, y)
	if !ok || cell.Text != want {
		t.Fatalf("CellAt(%d,%d) = %#v, %t; want %q", x, y, cell, ok, want)
	}
}
