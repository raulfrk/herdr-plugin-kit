package view

import "testing"

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
