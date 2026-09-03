package catalogue

import (
	"fmt"
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

func TestCompactFamiliesKeepSemanticRegionsAtSupportedSmallSizes(t *testing.T) {
	palette, err := theme.Builtin("catppuccin")
	if err != nil {
		t.Fatal(err)
	}
	data := compactContractSample()

	for _, size := range []struct {
		width  int
		height int
	}{{40, 10}, {48, 18}} {
		for _, family := range []struct {
			name          string
			design        Design
			rowTextColumn int
			header        theme.Color
			query         theme.Color
			leftTaskEdge  theme.Color
			rightTaskEdge theme.Color
		}{
			{"dense", designByID("dense-palette"), 3, palette.SidebarBackground, palette.PanelBackground, palette.Background, palette.Background},
			{"split", designByID("split-inspector"), 3, palette.SidebarBackground, palette.Surface, palette.PanelBackground, palette.PanelBackground},
			{"cards", designByID("calm-cards"), 4, palette.Background, palette.Surface, palette.Background, palette.Background},
		} {
			t.Run(family.name+"/"+sizeName(size.width, size.height), func(t *testing.T) {
				frame := renderCompactContract(t, family.design, treatments[0], size.width, size.height, data)

				assertTextAt(t, frame, 1, 0, data.title)
				assertTextAt(t, frame, 1, 1, "> "+data.query)
				assertBackground(t, frame, 0, 0, family.header)
				assertBackground(t, frame, size.width-1, 0, family.header)
				assertBackground(t, frame, 0, 1, family.query)
				assertBackground(t, frame, size.width-1, 1, family.query)
				assertStyle(t, frame, 1, 0, palette.Text, family.header, true, false)
				assertStyle(t, frame, 1, 1, palette.Text, family.query, false, false)

				for index, row := range data.rows {
					assertTextAt(t, frame, family.rowTextColumn, 2+index, row)
				}
				assertBackground(t, frame, 0, 2, family.leftTaskEdge)
				assertBackground(t, frame, size.width-1, 2, family.rightTaskEdge)
				assertBackground(t, frame, 1, 2, palette.PanelBackground)
				assertBackground(t, frame, size.width-2, 2, palette.PanelBackground)

				assertTextAt(t, frame, 1, size.height-2, data.status)
				assertTextAt(t, frame, 1, size.height-1, "d/e/p/s/t/f · / type · enter · esc")
				assertStyle(t, frame, 1, size.height-2, palette.Accent, palette.Background, true, false)
				assertStyle(t, frame, 1, size.height-1, palette.Muted, palette.Background, false, false)
				for _, row := range []int{size.height - 2, size.height - 1} {
					assertBackground(t, frame, 0, row, palette.Background)
					assertBackground(t, frame, size.width-1, row, palette.Background)
				}
			})
		}
	}
}

func TestCompactTextPreservesHorizontalSafeInsets(t *testing.T) {
	data := compactContractSample()
	data.title = strings.Repeat("T", 80)
	data.query = strings.Repeat("Q", 80)
	data.status = strings.Repeat("S", 80)

	for _, design := range designs {
		t.Run(design.ID, func(t *testing.T) {
			frame := renderCompactContract(t, design, treatments[0], 40, 10, data)
			for _, row := range []int{0, 1, 8, 9} {
				if got := mustCell(t, frame, 39, row).Text; got != "" {
					t.Errorf("right safe inset at row %d contains %q", row, got)
				}
			}
		})
	}
}

func TestCompactCardsAlternateTaskRowBackgrounds(t *testing.T) {
	palette, err := theme.Builtin("catppuccin")
	if err != nil {
		t.Fatal(err)
	}
	data := compactContractSample()
	data.selected = -1

	for _, size := range [][2]int{{40, 10}, {48, 18}} {
		frame := renderCompactContract(t, designByID("calm-cards"), treatments[0], size[0], size[1], data)
		for row := 2; row < size[1]-2; row++ {
			want := palette.PanelBackground
			if (row-2)%2 == 1 {
				want = palette.Surface
			}
			assertBackground(t, frame, 1, row, want)
			assertBackground(t, frame, size[0]-2, row, want)
			assertBackground(t, frame, 0, row, palette.Background)
			assertBackground(t, frame, size[0]-1, row, palette.Background)
		}
	}
}

func TestCompactSelectedRowTreatmentCues(t *testing.T) {
	palette, err := theme.Builtin("catppuccin")
	if err != nil {
		t.Fatal(err)
	}
	data := compactContractSample()

	for _, design := range designs {
		for _, treatment := range treatments {
			for _, size := range [][2]int{{40, 10}, {48, 18}} {
				t.Run(design.ID+"/"+treatment.ID+"/"+sizeName(size[0], size[1]), func(t *testing.T) {
					frame := renderCompactContract(t, design, treatment, size[0], size[1], data)
					markerColumn := 1
					baseBackground := palette.PanelBackground
					if design.ID == "calm-cards" {
						markerColumn = 2
						baseBackground = palette.Surface // Selected index 1 is on the second card row.
					}
					marker := mustCell(t, frame, markerColumn, 3)
					if marker.Text != ">" || !marker.Style.Bold {
						t.Fatalf("selected marker = %+v, want bold > at (%d,3)", marker, markerColumn)
					}
					wantBackground := baseBackground
					wantUnderline := false
					switch treatment.ID {
					case "structured":
						wantBackground = palette.SelectionBackground
					case "quiet":
						wantUnderline = true
					}
					if marker.Style.Background != wantBackground || marker.Style.Underline != wantUnderline {
						t.Errorf("selected marker style = %+v, want background %q underline=%t", marker.Style, wantBackground, wantUnderline)
					}

					nonSelected := mustCell(t, frame, markerColumn, 2)
					if nonSelected.Text != " " || nonSelected.Style.Bold || nonSelected.Style.Underline {
						t.Errorf("non-selected row inherited selection cue: %+v", nonSelected)
					}
				})
			}
		}
	}
}

func TestCompactSplitRevealsDetailOnlyWhenRowsAreAvailable(t *testing.T) {
	palette, err := theme.Builtin("catppuccin")
	if err != nil {
		t.Fatal(err)
	}
	data := compactContractSample()
	design := designByID("split-inspector")

	short := renderCompactContract(t, design, treatments[0], 40, 10, data)
	if got := frameText(short); strings.Contains(got, data.detail[0]) {
		t.Fatalf("40x10 split rendered detail without room: %q", got)
	}
	for row := 2; row <= 7; row++ {
		assertBackground(t, short, 0, row, palette.PanelBackground)
		assertBackground(t, short, 39, row, palette.PanelBackground)
	}

	tall := renderCompactContract(t, design, treatments[0], 48, 18, data)
	assertBackground(t, tall, 0, 8, palette.PanelBackground)
	assertBackground(t, tall, 47, 8, palette.PanelBackground)
	assertTextAt(t, tall, 1, 9, data.detail[0])
	assertTextAt(t, tall, 1, 10, data.detail[1])
	assertStyle(t, tall, 1, 9, palette.Accent, palette.Surface, true, false)
	assertStyle(t, tall, 1, 10, palette.Text, palette.Surface, false, false)
	for row := 9; row <= 15; row++ {
		assertBackground(t, tall, 0, row, palette.Surface)
		assertBackground(t, tall, 47, row, palette.Surface)
	}
}

func TestBoxTreatmentsExposeQuietDividerAndStructuredBoundary(t *testing.T) {
	palette, err := theme.Builtin("catppuccin")
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name      string
		treatment Treatment
		top       string
		middle    string
		bottom    string
	}{
		{"structured", Treatment{ID: "structured"}, "+----+", "|    |", "+----+"},
		{"quiet", Treatment{ID: "quiet"}, "──────", "      ", "      "},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame, err := view.NewFrame(8, 6)
			if err != nil {
				t.Fatal(err)
			}
			c := canvas{frame: frame, palette: palette, treatment: test.treatment}
			c.box(1, 1, 6, 4, palette.PanelBackground, palette.Border)

			if got := rowText(frame, 1, 1, 6); got != test.top {
				t.Errorf("top boundary = %q, want %q", got, test.top)
			}
			if got := rowText(frame, 1, 2, 6); got != test.middle {
				t.Errorf("middle boundary = %q, want %q", got, test.middle)
			}
			if got := rowText(frame, 1, 4, 6); got != test.bottom {
				t.Errorf("bottom boundary = %q, want %q", got, test.bottom)
			}
			for _, point := range [][2]int{{1, 1}, {6, 1}, {1, 2}, {6, 2}, {1, 4}, {6, 4}} {
				assertBackground(t, frame, point[0], point[1], palette.PanelBackground)
			}
			if cell := mustCell(t, frame, 1, 1); cell.Style.Foreground != palette.Border {
				t.Errorf("divider foreground = %q, want border %q", cell.Style.Foreground, palette.Border)
			}
		})
	}
}

func compactContractSample() sample {
	return sample{
		title:    "Catalogue Header",
		query:    "needle",
		status:   "Ready",
		rows:     []string{"Task zero", "Task one", "Task two", "Task three", "Task four", "Task five"},
		detail:   []string{"Detail title", "Detail body", "Detail third"},
		selected: 1,
	}
}

func renderCompactContract(t *testing.T, design Design, treatment Treatment, width, height int, data sample) *view.Frame {
	t.Helper()
	frame, err := renderSample(Spec{
		Design:   design,
		ThemeID:  "catppuccin",
		Viewport: Viewport{ID: "live", Name: "Live", Width: width, Height: height},
		Scenario: Scenario{ID: "query", Name: "Query"},
	}, data, treatment)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func designByID(id string) Design {
	for _, design := range designs {
		if design.ID == id {
			return design
		}
	}
	panic("unknown test design " + id)
}

func sizeName(width, height int) string {
	return fmt.Sprintf("%dx%d", width, height)
}

func assertTextAt(t *testing.T, frame *view.Frame, x, y int, want string) {
	t.Helper()
	if got := rowText(frame, x, y, len([]rune(want))); got != want {
		t.Errorf("text at (%d,%d) = %q, want %q", x, y, got, want)
	}
}

func assertBackground(t *testing.T, frame *view.Frame, x, y int, want theme.Color) {
	t.Helper()
	if got := mustCell(t, frame, x, y).Style.Background; got != want {
		t.Errorf("background at (%d,%d) = %q, want %q", x, y, got, want)
	}
}

func assertStyle(t *testing.T, frame *view.Frame, x, y int, foreground, background theme.Color, bold, underline bool) {
	t.Helper()
	got := mustCell(t, frame, x, y).Style
	if got.Foreground != foreground || got.Background != background || got.Bold != bold || got.Underline != underline {
		t.Errorf("style at (%d,%d) = %+v, want foreground %q background %q bold=%t underline=%t", x, y, got, foreground, background, bold, underline)
	}
}

func mustCell(t *testing.T, frame *view.Frame, x, y int) view.Cell {
	t.Helper()
	cell, ok := frame.CellAt(x, y)
	if !ok {
		t.Fatalf("missing cell at (%d,%d)", x, y)
	}
	return cell
}

func rowText(frame *view.Frame, x, y, width int) string {
	var result strings.Builder
	for column := x; column < x+width; column++ {
		cell, _ := frame.CellAt(column, y)
		if cell.Continuation {
			continue
		}
		if cell.Text == "" {
			result.WriteByte(' ')
		} else {
			result.WriteString(cell.Text)
		}
	}
	return result.String()
}

func frameText(frame *view.Frame) string {
	var result strings.Builder
	for row := 0; row < frame.Height(); row++ {
		result.WriteString(rowText(frame, 0, row, frame.Width()))
		result.WriteByte('\n')
	}
	return result.String()
}
