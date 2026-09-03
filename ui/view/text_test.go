package view_test

import (
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/ui/view"
	"github.com/rivo/uniseg"
)

func TestTruncatePreservesGraphemesAndCellBudget(t *testing.T) {
	for _, test := range []struct {
		text   string
		width  int
		marker string
		want   string
	}{
		{text: "short", width: 8, marker: "…", want: "short"},
		{text: "abcdef", width: 4, marker: "…", want: "abc…"},
		{text: "選択肢", width: 5, marker: "…", want: "選択…"},
		{text: "e\u0301clair", width: 2, marker: "…", want: "e\u0301…"},
		{text: "abcdef", width: 2, marker: "...", want: ".."},
		{text: "abcdef", width: 0, marker: "…", want: ""},
	} {
		got := view.Truncate(test.text, test.width, test.marker)
		if got != test.want || uniseg.StringWidth(got) > test.width {
			t.Fatalf("Truncate(%q,%d,%q)=%q width=%d, want %q", test.text, test.width, test.marker, got, uniseg.StringWidth(got), test.want)
		}
	}
}
