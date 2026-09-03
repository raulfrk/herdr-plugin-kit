package catalogue

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

func TestLiveCataloguePublishesEveryReviewChoice(t *testing.T) {
	if got := ids(liveDesigns, func(value Design) string { return value.ID }); !reflect.DeepEqual(got, []string{"bento-air", "bento-command", "bento-flow"}) {
		t.Fatalf("designs = %v", got)
	}
	if got := ids(Treatments(), func(value Treatment) string { return value.ID }); !reflect.DeepEqual(got, []string{"structured", "quiet", "focus-rail"}) {
		t.Fatalf("treatments = %v", got)
	}
	if got := ids(PluginSurfaces(), func(value PluginSurface) string { return value.ID }); !reflect.DeepEqual(got, []string{"recall-search", "attention-switcher", "session-switcher", "plugin-configurator", "action-finder", "debug-ui"}) {
		t.Fatalf("plugin surfaces = %v", got)
	}
	for _, plugin := range PluginSurfaces() {
		states := ids(plugin.States, func(value FlowState) string { return value.ID })
		for _, required := range []string{"loading", "empty", "error"} {
			if !contains(states, required) {
				t.Errorf("%s states %v omit %s", plugin.ID, states, required)
			}
		}
	}
	fixtures := ViewportFixtures()
	for _, required := range []ViewportFixture{
		{ID: "recovery-width", Columns: 39, Rows: 10},
		{ID: "recovery-height", Columns: 40, Rows: 9},
		{ID: "minimum", Columns: 40, Rows: 10},
		{ID: "restricted", Columns: 70, Rows: 10},
		{ID: "compact-tall", Columns: 70, Rows: 30},
		{ID: "maximum", Columns: 500, Rows: 200},
		{ID: "projected", Columns: 520, Rows: 220},
	} {
		fixture, ok := fixtureByID(fixtures, required.ID)
		if !ok || fixture.Columns != required.Columns || fixture.Rows != required.Rows {
			t.Errorf("fixture %s = %+v, found=%t", required.ID, fixture, ok)
		}
	}
}

func TestLiveCatalogueNavigationTaskFlowAndUnicodeEditing(t *testing.T) {
	surface := NewLiveSurface()
	initial := surface.DiagnosticState()
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "d"})
	if surface.design != 1 || surface.DiagnosticState().Selection == initial.Selection {
		t.Fatalf("design did not advance: %+v", surface.DiagnosticState())
	}
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "P"})
	if surface.plugin != len(pluginSurfaces)-1 || surface.state != 0 {
		t.Fatalf("reverse plugin navigation = plugin %d state %d", surface.plugin, surface.state)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
	if surface.state != 1 {
		t.Fatalf("Enter state = %d", surface.state)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyBackspace})
	if surface.state != 0 {
		t.Fatalf("Backspace state = %d", surface.state)
	}

	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "/"})
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "東京e\u0301🧭", Paste: true})
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyBackspace})
	if surface.query != "東京e\u0301" || !surface.editing {
		t.Fatalf("grapheme edit = %q, editing=%t", surface.query, surface.editing)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
	if surface.editing || surface.state != 0 {
		t.Fatalf("query commit changed flow: editing=%t state=%d", surface.editing, surface.state)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
	if surface.state != 1 {
		t.Fatalf("post-commit task advance = %d", surface.state)
	}

	if effects := surface.Update(shell.EventContext{}, shell.TextEvent{Text: "q"}); len(effects) != 1 {
		t.Fatalf("q effects = %d", len(effects))
	}
}

func TestLiveCatalogueRendersAllAxesAndCompactTaskBudget(t *testing.T) {
	sizes := []responsive.Size{{Columns: 40, Rows: 10}, {Columns: 70, Rows: 10}, {Columns: 48, Rows: 18}, {Columns: 70, Rows: 30}, {Columns: 110, Rows: 24}, {Columns: 500, Rows: 200}}
	surface := NewLiveSurface()
	assertRender := func(label string, size responsive.Size) {
		t.Helper()
		layout := responsive.Resolve(size)
		frame, err := surface.Render(shell.RenderContext{Layout: layout, ResizeGeneration: 7, Settled: true})
		if err != nil {
			t.Fatalf("%s render: %v", label, err)
		}
		if frame.Width() != layout.Render.Columns || frame.Height() != layout.Render.Rows {
			t.Fatalf("%s frame = %dx%d, want %dx%d", label, frame.Width(), frame.Height(), layout.Render.Columns, layout.Render.Rows)
		}
		if size.Rows <= 18 {
			plain := stripSGR(view.ANSI(frame))
			rows := strings.Split(plain, "\n")
			if len(rows) != size.Rows || !strings.Contains(plain, "▌") {
				t.Fatalf("%s compact frame lacks exact rows or non-colour focus cue", label)
			}
			populated := 0
			for row := 2; row < 8; row++ {
				if strings.TrimSpace(rows[row]) != "" {
					populated++
				}
			}
			if populated < 6 {
				t.Fatalf("%s compact task rows = %d, want at least 6", label, populated)
			}
		}
	}
	for _, size := range sizes {
		assertRender("size", size)
	}
	for surface.design = range liveDesigns {
		assertRender("design", responsive.Size{Columns: 48, Rows: 18})
	}
	for surface.treatment = range treatments {
		assertRender("treatment", responsive.Size{Columns: 48, Rows: 18})
	}
	for surface.plugin = range pluginSurfaces {
		for surface.state = range pluginSurfaces[surface.plugin].States {
			assertRender("plugin-state", responsive.Size{Columns: 48, Rows: 18})
		}
	}
	for surface.theme = range theme.IDs() {
		assertRender("theme", responsive.Size{Columns: 80, Rows: 24})
	}
}

func TestEveryNamedViewportFixtureRendersAtItsResolvedSize(t *testing.T) {
	surface := NewLiveSurface()
	for fixtureIndex, fixture := range ViewportFixtures() {
		if fixture.ID == "live" {
			continue
		}
		for surface.fixture != fixtureIndex {
			surface.Update(shell.EventContext{}, shell.TextEvent{Text: "f"})
		}
		layout := responsive.Resolve(responsive.Size{Columns: fixture.Columns, Rows: fixture.Rows})
		frame, err := surface.Render(shell.RenderContext{Layout: layout, ResizeGeneration: 1, Settled: true})
		if err != nil {
			t.Fatalf("%s: %v", fixture.ID, err)
		}
		if frame.Width() != layout.Render.Columns || frame.Height() != layout.Render.Rows {
			t.Errorf("%s frame = %dx%d, want %dx%d", fixture.ID, frame.Width(), frame.Height(), layout.Render.Columns, layout.Render.Rows)
		}
	}
}

func TestCommandCompactSelectionRemainsVisibleForEveryResult(t *testing.T) {
	surface := NewLiveSurface()
	surface.design = 1
	context := shell.RenderContext{Layout: responsive.Resolve(responsive.Size{Columns: 40, Rows: 10})}
	for selected, row := range surfaceBase("recall-search").rows {
		surface.selected = selected
		frame, err := surface.Render(context)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stripSGR(view.ANSI(frame)), "▌ "+strings.Fields(row)[0]) {
			t.Errorf("selection %d (%s) is not visibly marked", selected, row)
		}
	}
}

func TestCompactProgressAndErrorCuesRemainVisible(t *testing.T) {
	surface := NewLiveSurface()
	context := shell.RenderContext{Layout: responsive.Resolve(responsive.Size{Columns: 40, Rows: 10})}
	for designIndex := range liveDesigns {
		surface.design = designIndex
		for _, check := range []struct{ state, cue string }{{"loading", "progress 2/3"}, {"error", "!"}} {
			for index, state := range surface.currentPlugin().States {
				if state.ID == check.state {
					surface.state = index
				}
			}
			frame, err := surface.Render(context)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stripSGR(view.ANSI(frame)), check.cue) {
				t.Errorf("design %s state %s omits cue %q", liveDesigns[designIndex].ID, check.state, check.cue)
			}
		}
	}
}

func TestDiagnosticStateCoversReviewAxesWithoutQueryText(t *testing.T) {
	surface := NewLiveSurface()
	surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: responsive.Resolve(responsive.Size{Columns: 48, Rows: 18}), Generation: 42})
	states := []string{diagnosticKey(surface.DiagnosticState())}
	for _, key := range []string{"d", "e", "p", "s", "t", "f", "h"} {
		surface.Update(shell.EventContext{}, shell.TextEvent{Text: key})
		states = append(states, diagnosticKey(surface.DiagnosticState()))
	}
	for index := 1; index < len(states); index++ {
		if states[index] == states[index-1] {
			t.Errorf("axis %d did not change diagnostic state", index)
		}
	}
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "/"})
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "private-canary"})
	state := surface.DiagnosticState()
	if !state.Pending || state.ResizeGeneration != 42 || strings.Contains(diagnosticKey(state), "private-canary") {
		t.Fatalf("diagnostic state leaked query or lost semantic state: %+v", state)
	}
}

func diagnosticKey(state diagnostics.VisualState) string {
	return strings.Join([]string{state.Screen.String(), state.Focus.String(), state.Selection.String(), state.State.String()}, "|")
}

func TestLiveCatalogueHUDReportsDeliveredAndFinalGeometry(t *testing.T) {
	surface := NewLiveSurface()
	surface.hud = true
	surface.fixture = len(viewportFixtures) - 1
	layout := responsive.Resolve(responsive.Size{Columns: 520, Rows: 220})
	frame, err := surface.Render(shell.RenderContext{Layout: layout, ResizeGeneration: 84, Settled: true})
	if err != nil {
		t.Fatal(err)
	}
	plain := stripSGR(view.ANSI(frame))
	for _, want := range []string{"520x220", "500x200", "gen:84", "settled:true", "Projected oversize"} {
		if !strings.Contains(plain, want) {
			t.Errorf("HUD omits %q", want)
		}
	}
}

func TestCompactHUDRetainsEveryDiagnosticField(t *testing.T) {
	surface := NewLiveSurface()
	surface.hud = true
	for designIndex := range liveDesigns {
		surface.design = designIndex
		for _, size := range []responsive.Size{{Columns: 40, Rows: 10}, {Columns: 48, Rows: 30}} {
			layout := responsive.Resolve(size)
			frame, err := surface.Render(shell.RenderContext{Layout: layout, ResizeGeneration: 7, Settled: true})
			if err != nil {
				t.Fatal(err)
			}
			plain := stripSGR(view.ANSI(frame))
			geometry := fmt.Sprintf("%dx%d>%dx%d", size.Columns, size.Rows, size.Columns, size.Rows)
			for _, want := range []string{geometry, "compact", "g7", "set:1", "F1"} {
				if !strings.Contains(plain, want) {
					t.Errorf("%s %dx%d HUD omits %q", liveDesigns[designIndex].ID, size.Columns, size.Rows, want)
				}
			}
		}
	}
}

func TestLiveCatalogueComponentTreatmentsAreVisiblyDistinct(t *testing.T) {
	surface := NewLiveSurface()
	palette, err := theme.Builtin(theme.IDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []responsive.Size{{Columns: 48, Rows: 18}, {Columns: 80, Rows: 24}} {
		context := shell.RenderContext{Layout: responsive.Resolve(size)}
		for designIndex := range liveDesigns {
			surface.design = designIndex
			for treatmentIndex := range treatments {
				surface.treatment = treatmentIndex
				frame, err := surface.Render(context)
				if err != nil {
					t.Fatal(err)
				}
				marker, ok := findCell(frame, "▌")
				if !ok {
					t.Fatalf("%dx%d %s/%s has no focus rail", size.Columns, size.Rows, liveDesigns[designIndex].ID, treatments[treatmentIndex].ID)
				}
				if marker.Style.Foreground != palette.Accent || !marker.Style.Bold {
					t.Errorf("%dx%d %s/%s focus rail style = %+v", size.Columns, size.Rows, liveDesigns[designIndex].ID, treatments[treatmentIndex].ID, marker.Style)
				}
				switch treatments[treatmentIndex].ID {
				case "structured":
					if marker.Style.Background != palette.SelectionBackground || marker.Style.Underline {
						t.Errorf("structured selection style = %+v", marker.Style)
					}
				case "quiet":
					if marker.Style.Background == palette.SelectionBackground || !marker.Style.Underline {
						t.Errorf("quiet selection style = %+v", marker.Style)
					}
				case "focus-rail":
					if marker.Style.Background == palette.SelectionBackground || marker.Style.Underline {
						t.Errorf("focus-rail selection style = %+v", marker.Style)
					}
				}
			}
		}
	}
}

func findCell(frame *view.Frame, text string) (view.Cell, bool) {
	for row := range frame.Height() {
		for column := range frame.Width() {
			cell, _ := frame.CellAt(column, row)
			if cell.Text == text {
				return cell, true
			}
		}
	}
	return view.Cell{}, false
}

func ids[T any](values []T, id func(T) string) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = id(value)
	}
	return result
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func fixtureByID(fixtures []ViewportFixture, id string) (ViewportFixture, bool) {
	for _, fixture := range fixtures {
		if fixture.ID == id {
			return fixture, true
		}
	}
	return ViewportFixture{}, false
}

func stripSGR(value string) string {
	var result strings.Builder
	for index := 0; index < len(value); {
		if value[index] == '\x1b' && index+1 < len(value) && value[index+1] == '[' {
			index += 2
			for index < len(value) && value[index] != 'm' {
				index++
			}
			if index < len(value) {
				index++
			}
			continue
		}
		result.WriteByte(value[index])
		index++
	}
	return result.String()
}
