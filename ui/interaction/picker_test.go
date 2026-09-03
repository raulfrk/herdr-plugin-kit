package interaction

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
	"github.com/rivo/uniseg"
)

func newPickerForTest(t *testing.T) *Picker {
	t.Helper()
	picker, err := NewPicker(PickerOptions{Title: "Commands", Load: func(context.Context, string, Cursor) (Page, error) { return Page{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return picker
}

func TestPickerOpaquePagingHasNoKitCeiling(t *testing.T) {
	picker := newPickerForTest(t)
	const pageCount = 4097
	for page := 0; page < pageCount; page++ {
		cursor := NewCursor(fmt.Sprintf("provider/token/%d", page))
		next := NewCursor(fmt.Sprintf("provider/token/%d", page+1))
		if err := picker.applyPage(loadResult{cursor: cursor, page: Page{Items: []Item{{Key: fmt.Sprintf("item-%d", page), Label: "item"}}, Next: next}, append: page != 0}); err != nil {
			t.Fatal(err)
		}
	}
	if len(picker.pages) != pageCount || picker.pageIndex != pageCount-1 || picker.pages[pageCount-1].cursor.Token() != "provider/token/4096" {
		t.Fatalf("paging state pages=%d index=%d cursor=%q", len(picker.pages), picker.pageIndex, picker.pages[pageCount-1].cursor.Token())
	}
	for picker.pageIndex > 0 {
		picker.previousPage()
	}
	if picker.pageIndex != 0 {
		t.Fatalf("page history did not return to first page: %d", picker.pageIndex)
	}
}

func TestPickerStableSelectionAndStaleResults(t *testing.T) {
	picker := newPickerForTest(t)
	if err := picker.applyPage(loadResult{page: Page{Items: []Item{{Key: "a", Label: "Alpha"}, {Key: "b", Label: "Beta"}, {Key: "c", Label: "Charlie"}}}}); err != nil {
		t.Fatal(err)
	}
	picker.move(1)
	if picker.selectedKey != "b" {
		t.Fatalf("selected key = %q", picker.selectedKey)
	}
	if err := picker.applyPage(loadResult{page: Page{Items: []Item{{Key: "c", Label: "Charlie"}, {Key: "b", Label: "Beta"}, {Key: "a", Label: "Alpha"}}}}); err != nil {
		t.Fatal(err)
	}
	if picker.selected != 1 || picker.selectedKey != "b" {
		t.Fatalf("selection after reorder = %d/%q", picker.selected, picker.selectedKey)
	}

	picker.loadGeneration = 2
	picker.pendingLoad = true
	picker.result(shell.ResultEvent{Key: picker.loadKey, Generation: 1, Result: shell.WorkResult{Value: loadResult{page: Page{Items: []Item{{Key: "stale", Label: "stale"}}}}, Code: diagnostics.OutcomeApplied}})
	if !picker.pendingLoad || picker.items()[picker.selected].Key != "b" {
		t.Fatal("stale load completion changed picker")
	}
	picker.result(shell.ResultEvent{Key: picker.loadKey, Generation: 2, Result: shell.WorkResult{Value: loadResult{page: Page{Items: []Item{{Key: "fresh", Label: "fresh"}}}}, Code: diagnostics.OutcomeApplied}})
	if picker.pendingLoad || len(picker.items()) != 1 || picker.items()[0].Key != "fresh" {
		t.Fatalf("latest completion state pending=%t items=%v", picker.pendingLoad, picker.items())
	}
}

func TestPickerRejectsInvalidPagesAndSkipsDisabledItems(t *testing.T) {
	for _, items := range [][]Item{{{Label: "missing key"}}, {{Key: "same"}, {Key: "same"}}} {
		picker := newPickerForTest(t)
		if err := picker.applyPage(loadResult{page: Page{Items: items}}); err == nil {
			t.Fatalf("accepted invalid items %#v", items)
		}
	}
	picker := newPickerForTest(t)
	_ = picker.applyPage(loadResult{page: Page{Items: []Item{{Key: "disabled", Disabled: true}, {Key: "one"}, {Key: "also-disabled", Disabled: true}, {Key: "two"}}}})
	if picker.selected != 1 {
		t.Fatalf("first enabled selection = %d", picker.selected)
	}
	picker.move(1)
	if picker.selected != 3 {
		t.Fatalf("move did not skip disabled item: %d", picker.selected)
	}
	picker.selectBoundary(false)
	if picker.selected != 1 {
		t.Fatalf("home selection = %d", picker.selected)
	}
}

func TestPickerKeepsSelectionVisibleWithinTaskRows(t *testing.T) {
	picker := newPickerForTest(t)
	items := make([]Item, 20)
	for index := range items {
		items[index] = Item{Key: fmt.Sprintf("item-%d", index), Label: fmt.Sprintf("Item %d", index)}
	}
	_ = picker.applyPage(loadResult{page: Page{Items: items}})
	picker.selectBoundary(true)
	picker.layout = responsive.Resolve(responsive.Size{Columns: 40, Rows: 10})
	palette, _ := theme.Builtin("terminal")
	frame, err := picker.Render(shell.RenderContext{Layout: picker.layout, Theme: palette})
	if err != nil {
		t.Fatal(err)
	}
	if plain := view.ANSI(frame); !strings.Contains(plain, "▌ Item 19") {
		t.Fatalf("selected row was outside visible task window: %q", plain)
	}
}

func TestPickerIgnoresStaleResizeAndExplainsRecovery(t *testing.T) {
	picker := newPickerForTest(t)
	picker.query.Set("preserved")
	newest := responsive.Resolve(responsive.Size{Columns: 110, Rows: 24})
	older := responsive.Resolve(responsive.Size{Columns: 40, Rows: 10})
	picker.Update(shell.EventContext{}, shell.ResizeEvent{Layout: newest, Generation: 2})
	picker.Update(shell.EventContext{}, shell.ResizeEvent{Layout: older, Generation: 1})
	if picker.layout != newest || picker.resizeGeneration != 2 || picker.query.Text() != "preserved" {
		t.Fatalf("stale resize committed or state changed: layout=%+v g=%d query=%q", picker.layout, picker.resizeGeneration, picker.query.Text())
	}
	recovery := responsive.Resolve(responsive.Size{Columns: 20, Rows: 5})
	palette, _ := theme.Builtin("terminal")
	frame, err := picker.Render(shell.RenderContext{Layout: recovery, Theme: palette})
	if err != nil || !strings.Contains(view.ANSI(frame), "Need 40×10") || picker.query.Text() != "preserved" {
		t.Fatalf("recovery err=%v frame=%q query=%q", err, view.ANSI(frame), picker.query.Text())
	}
}

func TestPickerResponsiveFramesAtContractBoundariesAndThemes(t *testing.T) {
	sizes := []responsive.Size{{Columns: 40, Rows: 10}, {Columns: 48, Rows: 18}, {Columns: 48, Rows: 30}, {Columns: 78, Rows: 10}, {Columns: 78, Rows: 20}, {Columns: 80, Rows: 18}, {Columns: 110, Rows: 24}, {Columns: 500, Rows: 200}}
	for _, themeID := range theme.IDs() {
		palette, _ := theme.Builtin(themeID)
		for _, size := range sizes {
			picker := newPickerForTest(t)
			picker.layout = responsive.Resolve(size)
			_ = picker.applyPage(loadResult{page: Page{Items: []Item{{Key: "one", Label: strings.Repeat("選", 400), Description: "detail", Detail: []string{"line"}}, {Key: "two", Label: "Second"}}}})
			frame, err := picker.Render(shell.RenderContext{Layout: picker.layout, Theme: palette})
			if err != nil || frame.Width() != size.Columns || frame.Height() != size.Rows {
				t.Fatalf("%s %+v frame=%v err=%v", themeID, size, frame, err)
			}
			plain := view.ANSI(frame)
			for row, line := range strings.Split(stripANSI(plain), "\n") {
				if width := uniseg.StringWidth(line); width != size.Columns {
					t.Fatalf("%s %+v row %d width=%d", themeID, size, row, width)
				}
			}
			if !strings.Contains(plain, "▌") || !strings.Contains(plain, "? help") {
				t.Fatalf("%s %+v missing focus/basic-key cues", themeID, size)
			}
		}
	}
}

func TestPickerCompactDrillInAndEighteenRowRule(t *testing.T) {
	picker := newPickerForTest(t)
	_ = picker.applyPage(loadResult{page: Page{Items: []Item{{Key: "one", Label: "First", Description: "Selected detail"}}}})
	palette, _ := theme.Builtin("terminal")
	for _, size := range []responsive.Size{{Columns: 80, Rows: 18}, {Columns: 79, Rows: 30}} {
		picker.layout = responsive.Resolve(size)
		picker.openOrActivate(shell.EventContext{})
		if picker.screen != detailScreen {
			t.Fatalf("%+v did not drill into detail", size)
		}
		frame, _ := picker.Render(shell.RenderContext{Layout: picker.layout, Theme: palette})
		if plain := view.ANSI(frame); !strings.Contains(plain, "Detail · Commands") || strings.Contains(plain, "Search:") {
			t.Fatalf("%+v compact detail presentation incorrect", size)
		}
		picker.screen = rootScreen
	}
	picker.layout = responsive.Resolve(responsive.Size{Columns: 80, Rows: 19})
	frame, _ := picker.Render(shell.RenderContext{Layout: picker.layout, Theme: palette})
	if plain := view.ANSI(frame); !strings.Contains(plain, "Selected detail") || !strings.Contains(plain, "Search:") {
		t.Fatal("standard split did not show results and context")
	}
}

func TestPickerDiagnosticStateCannotLeakQueryItemOrValue(t *testing.T) {
	canary := "private-query_日本語_/secret/path"
	picker := newPickerForTest(t)
	picker.query.Set(canary)
	picker.layout = responsive.Resolve(responsive.Size{Columns: 5000, Rows: 2000})
	_ = picker.applyPage(loadResult{page: Page{Items: []Item{{Key: canary, Label: canary, Description: canary, Detail: []string{canary}, Value: canary}}}})
	picker.pendingActivation = true
	picker.resizeGeneration = 17
	encoded, err := json.Marshal(picker.DiagnosticState())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), canary) || strings.Contains(fmt.Sprintf("%+v", picker.DiagnosticState()), canary) {
		t.Fatalf("diagnostic projection leaked content: %s", encoded)
	}
	state := picker.DiagnosticState()
	if state.Geometry.RenderColumns != 500 || state.Geometry.RenderRows != 200 || state.ResizeGeneration != 17 || state.ItemCount != 1 || !state.Pending || state.State.String() != "activating" || state.Selection.String() != "available" {
		t.Fatalf("semantic diagnostic state = %+v", state)
	}
}

func TestPickerActivationCompletionUsesNewestGeneration(t *testing.T) {
	picker := newPickerForTest(t)
	_ = picker.applyPage(loadResult{page: Page{Items: []Item{{Key: "one", Label: "One"}}}})
	picker.activationGeneration = 2
	picker.pendingActivation = true
	picker.result(shell.ResultEvent{Key: picker.activationKey, Generation: 1, Result: shell.WorkResult{Err: context.Canceled, Code: diagnostics.OutcomeFailed}})
	if !picker.pendingActivation || picker.hasError || picker.activated {
		t.Fatal("stale activation completion changed picker")
	}
	picker.result(shell.ResultEvent{Key: picker.activationKey, Generation: 2, Result: shell.WorkResult{Value: activationResult{}, Code: diagnostics.OutcomeApplied}})
	if picker.pendingActivation || picker.hasError || !picker.activated {
		t.Fatal("latest activation completion was not applied")
	}
}

type signalSurface struct {
	picker  *Picker
	loaded  chan struct{}
	applied chan struct{}
}

func (surface *signalSurface) Update(eventContext shell.EventContext, event shell.Event) []shell.Effect {
	effects := surface.picker.Update(eventContext, event)
	if result, ok := event.(shell.ResultEvent); ok {
		switch result.Key.String() {
		case surface.picker.loadKey.String():
			select {
			case surface.loaded <- struct{}{}:
			default:
			}
		case surface.picker.activationKey.String():
			select {
			case surface.applied <- struct{}{}:
			default:
			}
		}
	}
	return effects
}

func (surface *signalSurface) Render(context shell.RenderContext) (*view.Frame, error) {
	return surface.picker.Render(context)
}

func (surface *signalSurface) DiagnosticState() diagnostics.VisualState {
	return surface.picker.DiagnosticState()
}

type discardSemanticSink struct{}

func (discardSemanticSink) RecordSemantic(diagnostics.SemanticEvent) error { return nil }

func TestPickerRunsLoadAndActivationThroughShellEffects(t *testing.T) {
	activated := make(chan Item, 1)
	requested := make(chan string, 1)
	picker, err := NewPicker(PickerOptions{
		Title: "Commands",
		Load: func(_ context.Context, query string, cursor Cursor) (Page, error) {
			requested <- query + "|" + cursor.Token()
			return Page{Items: []Item{{Key: "run", Label: "Run"}}}, nil
		},
		Activate: func(_ context.Context, item Item) error { activated <- item; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	surface := &signalSurface{picker: picker, loaded: make(chan struct{}, 1), applied: make(chan struct{}, 1)}
	palette, _ := theme.Builtin("terminal")
	input, inputWriter := io.Pipe()
	t.Cleanup(func() { _ = inputWriter.Close() })
	program, err := shell.NewProgram(shell.ProgramOptions{PluginID: semanticID("test-picker"), Theme: palette, Events: discardSemanticSink{}, Input: input, Output: io.Discard}, surface)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, runErr := program.Run(); done <- runErr }()
	program.Send(tea.WindowSizeMsg{Width: 80, Height: 19})
	select {
	case request := <-requested:
		if request != "|" {
			t.Fatalf("initial request = %q", request)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("load work did not run")
	}
	waitSignal(t, surface.loaded, "load result")
	program.Send(tea.KeyMsg{Type: tea.KeyEnter})
	select {
	case item := <-activated:
		if item.Key != "run" {
			t.Fatalf("activated item = %#v", item)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("activation work did not run")
	}
	waitSignal(t, surface.applied, "activation result")
	program.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatal(runErr)
		}
	case <-time.After(2 * time.Second):
		program.Kill()
		t.Fatal("picker program did not quit")
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func stripANSI(value string) string {
	result := strings.Builder{}
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
