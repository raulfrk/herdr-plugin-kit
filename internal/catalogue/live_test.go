package catalogue

import (
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

func TestLiveCatalogueKeepsSurfacesStatesAndFinalPresentation(t *testing.T) {
	if len(PluginSurfaces()) != 6 {
		t.Fatalf("surfaces = %d", len(PluginSurfaces()))
	}
	for _, surface := range PluginSurfaces() {
		if len(surface.States) != 6 {
			t.Fatalf("%s states = %d", surface.ID, len(surface.States))
		}
	}
	live := NewLiveSurface()
	live.Update(shell.EventContext{}, shell.TextEvent{Text: "p"})
	live.Update(shell.EventContext{}, shell.TextEvent{Text: "s"})
	live.Update(shell.EventContext{}, shell.TextEvent{Text: "t"})
	if got := live.DiagnosticState().Selection.String(); got != theme.IDs()[1] {
		t.Fatalf("theme = %s", got)
	}
	context := shell.RenderContext{Layout: responsive.Resolve(responsive.Size{Columns: 48, Rows: 18}), ResizeGeneration: 7}
	frame, err := live.Render(context)
	if err != nil {
		t.Fatal(err)
	}
	plain := view.ANSI(frame)
	if frame.Width() != 48 || frame.Height() != 18 || !strings.Contains(plain, "Bento Command") || !strings.Contains(plain, "Search sessions") || !strings.Contains(plain, "▌") {
		t.Fatalf("final compact command frame missing required cues")
	}
}

func TestLiveCatalogueEditingHUDAndHelp(t *testing.T) {
	live := NewLiveSurface()
	live.Update(shell.EventContext{}, shell.TextEvent{Text: "/"})
	live.Update(shell.EventContext{}, shell.TextEvent{Text: "東京e\u0301🧭", Paste: true})
	live.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyBackspace})
	if live.query != "東京e\u0301" {
		t.Fatal(live.query)
	}
	live.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
	live.Update(shell.EventContext{}, shell.TextEvent{Text: "h"})
	frame, err := live.Render(shell.RenderContext{Layout: responsive.Resolve(responsive.Size{Columns: 110, Rows: 24}), ResizeGeneration: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.ANSI(frame), "HUD 110x24") {
		t.Fatal("HUD missing")
	}
	live.editing = false
	live.Update(shell.EventContext{}, shell.TextEvent{Text: "?"})
	frame, err = live.Render(shell.RenderContext{Layout: responsive.Resolve(responsive.Size{Columns: 40, Rows: 10})})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.ANSI(frame), "CATALOGUE HELP") {
		t.Fatal("help missing")
	}
}

func TestLiveCatalogueIgnoresStaleAndDuplicateResizeGenerations(t *testing.T) {
	live := NewLiveSurface()
	newest := responsive.Resolve(responsive.Size{Columns: 110, Rows: 24})
	older := responsive.Resolve(responsive.Size{Columns: 40, Rows: 10})
	live.Update(shell.EventContext{}, shell.ResizeEvent{Layout: newest, Generation: 2})
	live.Update(shell.EventContext{}, shell.ResizeEvent{Layout: older, Generation: 1})
	live.Update(shell.EventContext{}, shell.ResizeEvent{Layout: older, Generation: 2})
	if live.layout != newest || live.resizeGeneration != 2 {
		t.Fatalf("stale or duplicate resize committed: layout=%+v generation=%d", live.layout, live.resizeGeneration)
	}
}

func TestLiveCatalogueNavigationAndExitSemantics(t *testing.T) {
	live := NewLiveSurface()
	update := func(event shell.Event) []shell.Effect {
		t.Helper()
		return live.Update(shell.EventContext{}, event)
	}

	update(shell.TextEvent{Text: "P"})
	if live.plugin != len(pluginSurfaces)-1 || live.state != 0 || live.selected != 1 {
		t.Fatalf("reverse surface = %+v", live)
	}
	update(shell.TextEvent{Text: "p"})
	update(shell.TextEvent{Text: "S"})
	if live.plugin != 0 || live.state != len(live.currentPlugin().States)-1 {
		t.Fatalf("surface/state = %d/%d", live.plugin, live.state)
	}
	update(shell.TextEvent{Text: "s"})
	update(shell.TextEvent{Text: "T"})
	if live.state != 0 || live.theme != len(theme.IDs())-1 {
		t.Fatalf("state/theme = %d/%d", live.state, live.theme)
	}
	update(shell.TextEvent{Text: "t"})

	update(shell.KeyEvent{Code: shell.KeyTab})
	update(shell.KeyEvent{Code: shell.KeyRight})
	if live.active != stateAxis || live.state != 1 {
		t.Fatalf("state field = %d/%d", live.active, live.state)
	}
	update(shell.KeyEvent{Code: shell.KeyTab})
	update(shell.KeyEvent{Code: shell.KeyLeft})
	if live.active != themeAxis || live.theme != len(theme.IDs())-1 {
		t.Fatalf("theme field = %d/%d", live.active, live.theme)
	}
	update(shell.KeyEvent{Code: shell.KeyTab})
	update(shell.KeyEvent{Code: shell.KeyLeft})
	if live.active != pluginAxis || live.plugin != len(pluginSurfaces)-1 || live.state != 0 || live.selected != 1 {
		t.Fatalf("plugin field reset = %+v", live)
	}

	update(shell.KeyEvent{Code: shell.KeyUp})
	if live.selected != 0 {
		t.Fatalf("selection up = %d", live.selected)
	}
	update(shell.KeyEvent{Code: shell.KeyUp})
	if live.selected != 5 {
		t.Fatalf("selection wrapped = %d", live.selected)
	}
	update(shell.KeyEvent{Code: shell.KeyDown})
	if live.selected != 0 {
		t.Fatalf("selection down = %d", live.selected)
	}

	update(shell.KeyEvent{Code: shell.KeyEnter})
	if live.state != 1 {
		t.Fatalf("enter state = %d", live.state)
	}
	update(shell.KeyEvent{Code: shell.KeyBackspace})
	if live.state != 0 {
		t.Fatalf("back state = %d", live.state)
	}

	update(shell.TextEvent{Text: "?"})
	if effects := update(shell.KeyEvent{Code: shell.KeyEscape}); len(effects) != 0 || live.help {
		t.Fatalf("help escape effects=%v help=%t", effects, live.help)
	}
	update(shell.TextEvent{Text: "/"})
	update(shell.TextEvent{Text: "query"})
	if effects := update(shell.KeyEvent{Code: shell.KeyEscape}); len(effects) != 0 || live.editing || live.query != "query" {
		t.Fatalf("editing escape effects=%v editing=%t query=%q", effects, live.editing, live.query)
	}
	if effects := update(shell.KeyEvent{Code: shell.KeyEscape}); len(effects) != 1 {
		t.Fatalf("root escape effects=%v", effects)
	}
	if effects := update(shell.TextEvent{Text: "q"}); len(effects) != 1 {
		t.Fatalf("q effects=%v", effects)
	}
	if effects := update(shell.KeyEvent{Code: shell.KeyCtrlC}); len(effects) != 1 {
		t.Fatalf("ctrl-c effects=%v", effects)
	}
}

func TestLiveCatalogueResponsiveCopyAndDiagnosticState(t *testing.T) {
	live := NewLiveSurface()
	layout := responsive.Resolve(responsive.Size{Columns: 69, Rows: 18})
	live.Update(shell.EventContext{}, shell.ResizeEvent{Layout: layout, Generation: 9})
	live.Update(shell.EventContext{}, shell.TextEvent{Text: "h"})
	frame, err := live.Render(shell.RenderContext{Layout: layout, ResizeGeneration: 9})
	if err != nil {
		t.Fatal(err)
	}
	plain := view.ANSI(frame)
	if !strings.Contains(plain, "HUD 69x18") || !strings.Contains(plain, "? help · Tab field · ←→ change") {
		t.Fatalf("compact HUD/help missing: %q", plain)
	}
	plainStatus := NewLiveSurface()
	if got := plainStatus.status(shell.RenderContext{Layout: responsive.Resolve(responsive.Size{Columns: 110, Rows: 18})}, "Results", "catppuccin"); got != "Results · catppuccin" {
		t.Fatalf("18-row status = %q", got)
	}
	if got := plainStatus.status(shell.RenderContext{Layout: responsive.Resolve(responsive.Size{Columns: 110, Rows: 19})}, "Results", "catppuccin"); got != "Results · catppuccin · Bento Command / Structured" {
		t.Fatalf("19-row status = %q", got)
	}
	if navigationHelp(69) != "? help · Tab field · ←→ change" || navigationHelp(70) != "? help · Tab field · ←/→ change · / search · Esc quit" {
		t.Fatal("navigation copy boundary changed")
	}
	state := live.DiagnosticState()
	if state.Geometry.ReportedColumns != 69 || state.Geometry.ReportedRows != 18 || state.Geometry.RenderColumns != 69 || state.Geometry.RenderRows != 18 || state.ResizeGeneration != 9 || state.State.String() != "query.hud" {
		t.Fatalf("diagnostic state = %+v", state)
	}

	live.Update(shell.EventContext{}, shell.TextEvent{Text: "h"})
	wide := responsive.Resolve(responsive.Size{Columns: 70, Rows: 19})
	frame, err = live.Render(shell.RenderContext{Layout: wide})
	if err != nil {
		t.Fatal(err)
	}
	plain = view.ANSI(frame)
	if !strings.Contains(plain, "Bento Command / Structured") || !strings.Contains(plain, "←/→ change · / search · Esc quit") {
		t.Fatalf("standard status/help missing: %q", plain)
	}

	for range 5 {
		live.Update(shell.EventContext{}, shell.TextEvent{Text: "s"})
	}
	if got := live.DiagnosticState(); !got.HasError {
		t.Fatalf("error diagnostic = %+v", got)
	}
	live.Update(shell.EventContext{}, shell.TextEvent{Text: "/"})
	if got := live.DiagnosticState(); !got.Pending {
		t.Fatalf("editing diagnostic = %+v", got)
	}
	frame, err = live.Render(shell.RenderContext{Layout: wide})
	if err != nil || !strings.Contains(view.ANSI(frame), "▏") {
		t.Fatalf("editing cursor missing: %v", err)
	}
	configError := NewLiveSurface()
	configError.plugin, configError.state = 3, 1
	if got := configError.DiagnosticState(); !got.HasError || got.State.String() != "validation-error.plain" {
		t.Fatalf("validation diagnostic = %+v", got)
	}
}

func TestLiveCatalogueExactLayoutTransitions(t *testing.T) {
	palette, _ := theme.Builtin("catppuccin")
	data := sample{title: strings.Repeat("T", 200), query: strings.Repeat("Q", 200), status: strings.Repeat("S", 200), help: strings.Repeat("H", 200), rows: longLines("Row", 220), detail: longLines("Detail", 220), selected: 0}
	for _, size := range []responsive.Size{{Columns: 80, Rows: 19}, {Columns: 100, Rows: 24}} {
		frame, err := renderSample(Spec{ThemeID: "catppuccin", Viewport: Viewport{ID: "live", Width: size.Columns, Height: size.Rows}}, data)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Width() != size.Columns || frame.Height() != size.Rows {
			t.Fatalf("%+v frame = %dx%d", size, frame.Width(), frame.Height())
		}
		if size.Columns == 80 {
			cell, _ := frame.CellAt(53, 8)
			if cell.Style.Background != palette.Surface {
				t.Fatalf("80-column detail boundary = %+v", cell)
			}
		}
		if size.Columns == 100 {
			left, _ := frame.CellAt(2, 1)
			start, _ := frame.CellAt(4, 1)
			if left.Text != "" || start.Text != "T" {
				t.Fatalf("100-column margin = left %+v start %+v", left, start)
			}
		}
	}
}

func TestLongContentFixtureExercisesFullViewport(t *testing.T) {
	data := scenarioSample(Scenario{ID: "long-content"})
	if len(data.rows) != 220 || len(data.detail) != 220 || !strings.Contains(data.rows[219], "Result 220") || !strings.Contains(data.detail[219], "Detail 220") {
		t.Fatalf("long fixture rows/detail = %d/%d", len(data.rows), len(data.detail))
	}
}
