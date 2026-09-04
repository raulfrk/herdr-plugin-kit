package interaction

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

func TestPickerConstructionAndCursorContracts(t *testing.T) {
	if _, err := NewPicker(PickerOptions{Load: func(context.Context, string, Cursor) (Page, error) { return Page{}, nil }}); err == nil {
		t.Fatal("accepted an empty title")
	}
	if _, err := NewPicker(PickerOptions{Title: "Commands"}); err == nil {
		t.Fatal("accepted a nil loader")
	}
	empty := NewCursor("")
	opaque := NewCursor("provider/page:2")
	if !empty.Empty() || empty.Token() != "" || opaque.Empty() || opaque.Token() != "provider/page:2" {
		t.Fatalf("cursor contracts: empty=%q/%t opaque=%q/%t", empty.Token(), empty.Empty(), opaque.Token(), opaque.Empty())
	}
}

func TestPickerReportsWhenPlainTextBelongsToTheQuery(t *testing.T) {
	picker := newPickerForTest(t)
	if picker.Editing() {
		t.Fatal("new picker reports active text editing")
	}
	picker.text(shell.EventContext{}, "/")
	if !picker.Editing() {
		t.Fatal("picker did not report active query editing")
	}
	picker.key(shell.EventContext{}, shell.KeyEscape)
	if picker.Editing() {
		t.Fatal("picker still reports editing after Escape")
	}
}

func TestPickerPreviousPageGuardsAndSingleStep(t *testing.T) {
	noHistory := newPickerForTest(t)
	noHistory.pageIndex = 1
	noHistory.previousPage()
	if noHistory.pageIndex != 1 || len(noHistory.pages) != 0 {
		t.Fatalf("empty-history guard changed index to %d", noHistory.pageIndex)
	}

	firstPage := newPickerForTest(t)
	if err := firstPage.applyPage(loadResult{page: Page{Items: []Item{{Key: "first", Label: "First"}}}}); err != nil {
		t.Fatal(err)
	}
	firstPage.previousPage()
	if firstPage.pageIndex != 0 || firstPage.selectedKey != "first" {
		t.Fatalf("first-page guard changed state: page=%d selected=%q", firstPage.pageIndex, firstPage.selectedKey)
	}

	history := newPickerForTest(t)
	_ = history.applyPage(loadResult{page: Page{Items: []Item{{Key: "first", Label: "First"}}}})
	_ = history.applyPage(loadResult{page: Page{Items: []Item{{Key: "second", Label: "Second"}}}, append: true})
	history.selectedKey = "first"
	history.previousPage()
	if history.pageIndex != 0 || history.selected != 0 || history.selectedKey != "first" || history.items()[0].Key != "first" {
		t.Fatalf("single previous step = page %d selection %d/%q items=%v", history.pageIndex, history.selected, history.selectedKey, history.items())
	}
}

func TestPickerResizeDoesNotDuplicateLoadsAndEveryHelpExitKeyReturns(t *testing.T) {
	for name, configure := range map[string]func(*Picker){
		"already loaded": func(picker *Picker) { picker.loaded = true },
		"load pending":   func(picker *Picker) { picker.pendingLoad = true },
	} {
		t.Run(name, func(t *testing.T) {
			picker := newPickerForTest(t)
			configure(picker)
			layout := responsive.Resolve(responsive.Size{Columns: 80, Rows: 19})
			if effects := picker.Update(shell.EventContext{}, shell.ResizeEvent{Layout: layout, Generation: 1}); len(effects) != 0 || picker.hasError {
				t.Fatalf("resize duplicated load: effects=%v error=%t", effects, picker.hasError)
			}
		})
	}
	for _, key := range []shell.KeyCode{shell.KeyEscape, shell.KeyBackspace, shell.KeyEnter} {
		picker := newPickerForTest(t)
		picker.screen, picker.returnScreen = helpScreen, detailScreen
		if effects := picker.key(shell.EventContext{}, key); len(effects) != 0 || picker.screen != detailScreen {
			t.Fatalf("help key %q left screen=%d effects=%v", key, picker.screen, effects)
		}
	}
}

func TestPickerNextPageBoundariesAndCachedNavigation(t *testing.T) {
	empty := newPickerForTest(t)
	if effects := empty.nextPage(shell.EventContext{}); len(effects) != 0 || empty.pageIndex != 0 {
		t.Fatalf("empty next page produced effects/state: %d/%d", len(effects), empty.pageIndex)
	}

	picker := newPickerForTest(t)
	_ = picker.applyPage(loadResult{page: Page{Items: []Item{{Key: "one", Label: "One"}}, Next: NewCursor("next")}})
	picker.pendingLoad = true
	if effects := picker.nextPage(shell.EventContext{}); len(effects) != 0 || picker.pageIndex != 0 || picker.hasError {
		t.Fatalf("pending-load next changed state: page=%d error=%t", picker.pageIndex, picker.hasError)
	}
	picker.pendingLoad = false
	picker.queryDirty = true
	if effects := picker.nextPage(shell.EventContext{}); len(effects) != 0 || picker.pageIndex != 0 || picker.hasError {
		t.Fatalf("dirty-query next changed state: page=%d error=%t", picker.pageIndex, picker.hasError)
	}
	picker.queryDirty = false
	picker.pages[0].page.Next = Cursor{}
	picker.nextPage(shell.EventContext{})
	if picker.pageIndex != 0 || picker.hasError {
		t.Fatalf("terminal page next changed state: page=%d error=%t", picker.pageIndex, picker.hasError)
	}

	_ = picker.applyPage(loadResult{page: Page{Items: []Item{{Key: "two", Label: "Two"}}}, append: true})
	picker.pageIndex = 0
	picker.selectedKey = "two"
	if effects := picker.nextPage(shell.EventContext{}); len(effects) != 0 || picker.pageIndex != 1 || picker.selectedKey != "two" {
		t.Fatalf("cached next = effects=%d page=%d selected=%q", len(effects), picker.pageIndex, picker.selectedKey)
	}

	picker.pages[1].page.Next = NewCursor("provider-next")
	if effects := picker.nextPage(shell.EventContext{}); len(effects) != 0 || !picker.hasError || picker.pendingLoad || picker.pageIndex != 1 {
		t.Fatalf("failed next request = effects=%d error=%t pending=%t page=%d", len(effects), picker.hasError, picker.pendingLoad, picker.pageIndex)
	}
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

func TestPickerRejectsPreviousQueryCompletionDuringDebounce(t *testing.T) {
	picker := newPickerForTest(t)
	picker.query.Set("old")
	picker.loadGeneration = 1
	picker.pendingLoad = true
	picker.queryRevision = 1
	picker.query.Set("new")
	picker.queryDirty = true
	picker.result(shell.ResultEvent{Key: picker.loadKey, Generation: 1, Result: shell.WorkResult{
		Value: loadResult{query: "old", revision: 1, page: Page{Items: []Item{{Key: "stale", Label: "Stale"}}}},
		Code:  diagnostics.OutcomeApplied,
	}})
	if picker.loaded || len(picker.items()) != 0 || picker.hasError {
		t.Fatalf("previous-query completion committed: loaded=%t items=%v error=%t", picker.loaded, picker.items(), picker.hasError)
	}
	if effects := picker.nextPage(shell.EventContext{}); len(effects) != 0 {
		t.Fatalf("dirty query paged with %d effects", len(effects))
	}
	if effects := picker.openOrActivate(shell.EventContext{}); len(effects) != 0 {
		t.Fatalf("dirty query activated with %d effects", len(effects))
	}
	if picker.status() != "Typing…" {
		t.Fatalf("dirty query status = %q", picker.status())
	}
	state := picker.DiagnosticState()
	if state.State.String() != "debouncing" || !state.Pending {
		t.Fatalf("dirty query diagnostics = %+v", state)
	}
}

func TestPickerRejectsSameTextFromAnOlderQueryRevision(t *testing.T) {
	picker := newPickerForTest(t)
	picker.query.Set("a")
	picker.queryRevision = 1
	picker.loadGeneration = 1
	picker.pendingLoad = true
	picker.query.Set("ab")
	picker.queryRevision++
	picker.query.Set("a")
	picker.queryRevision++
	picker.queryDirty = true
	picker.result(shell.ResultEvent{Key: picker.loadKey, Generation: 1, Result: shell.WorkResult{
		Value: loadResult{query: "a", revision: 1, page: Page{Items: []Item{{Key: "stale", Label: "Stale"}}}},
		Code:  diagnostics.OutcomeApplied,
	}})
	if picker.loaded || len(picker.items()) != 0 || picker.hasError {
		t.Fatalf("older same-text completion committed: loaded=%t items=%v error=%t", picker.loaded, picker.items(), picker.hasError)
	}
}

func TestPickerRejectsStaleProviderErrorsByQueryAndRevision(t *testing.T) {
	for name, result := range map[string]loadResult{
		"query":    {query: "old", revision: 2},
		"revision": {query: "current", revision: 1},
	} {
		t.Run(name, func(t *testing.T) {
			picker := newPickerForTest(t)
			picker.query.Set("current")
			picker.queryRevision = 2
			picker.loadGeneration = 1
			picker.pendingLoad = true
			picker.result(shell.ResultEvent{Key: picker.loadKey, Generation: 1, Result: shell.WorkResult{
				Value: result, Err: errors.New("stale failure"), Code: diagnostics.OutcomeFailed,
			}})
			if picker.hasError {
				t.Fatal("stale provider failure replaced the current query state")
			}
		})
	}
}

func TestPickerPreservesProviderOrderAndExposesEnabledSelection(t *testing.T) {
	picker := newPickerForTest(t)
	picker.query.Set("alpha")
	items := []Item{
		{Key: "provider-first", Label: "No textual match"},
		{Key: "disabled", Label: "Alpha", Disabled: true},
		{Key: "provider-last", Label: "Alpha exact"},
	}
	if err := picker.applyPage(loadResult{page: Page{Items: items}}); err != nil {
		t.Fatal(err)
	}
	if got := picker.items(); len(got) != 3 || got[0].Key != "provider-first" || got[2].Key != "provider-last" {
		t.Fatalf("provider order changed: %#v", got)
	}
	selected, ok := picker.Selected()
	if !ok || selected.Key != "provider-first" {
		t.Fatalf("selected item = %#v, %t", selected, ok)
	}
	picker.selected = 1
	if _, ok := picker.Selected(); ok {
		t.Fatal("disabled item was exposed as selected")
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
	picker.Update(shell.EventContext{}, shell.ResizeEvent{Layout: older, Generation: 2})
	if picker.layout != newest || picker.resizeGeneration != 2 || picker.query.Text() != "preserved" {
		t.Fatalf("stale or duplicate resize committed or state changed: layout=%+v g=%d query=%q", picker.layout, picker.resizeGeneration, picker.query.Text())
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

func TestPickerCompactStandardAndWideFrameFingerprints(t *testing.T) {
	palette, _ := theme.Builtin("terminal")
	tests := []struct {
		name    string
		size    responsive.Size
		class   responsive.Class
		splitAt int
	}{
		{name: "compact", size: responsive.Size{Columns: 60, Rows: 12}, class: responsive.Compact},
		{name: "standard", size: responsive.Size{Columns: 90, Rows: 20}, class: responsive.Standard, splitAt: 60},
		{name: "wide", size: responsive.Size{Columns: 120, Rows: 30}, class: responsive.Wide, splitAt: 80},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			picker := newPickerForTest(t)
			picker.layout = responsive.Resolve(test.size)
			_ = picker.applyPage(loadResult{page: Page{Items: []Item{
				{Key: "alpha", Label: "Alpha", Description: "Primary detail", Detail: []string{"Detail line"}},
				{Key: "disabled", Label: "Disabled", Disabled: true},
			}}})
			frame, err := picker.Render(shell.RenderContext{Layout: picker.layout, Theme: palette})
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(stripANSI(view.ANSI(frame)), "\n")
			if picker.layout.Class != test.class || strings.TrimRight(lines[0], " ") != " Commands" || !strings.HasPrefix(lines[1], "▌ Search: ") || !strings.Contains(lines[2], "Ready · page 1 · 2 items") || !strings.HasPrefix(lines[3], "▌ Alpha") || !strings.HasPrefix(lines[len(lines)-1], "? help · / search") {
				t.Fatalf("%s semantic frame fingerprint:\n%s", test.name, strings.Join(lines, "\n"))
			}
			header, _ := frame.CellAt(1, 0)
			selected, _ := frame.CellAt(0, 3)
			if !header.Style.Bold || header.Style.Background != palette.PanelBackground || !selected.Style.Bold || selected.Style.Background != palette.ActiveRowBackground {
				t.Fatalf("%s semantic styles header=%+v selected=%+v", test.name, header.Style, selected.Style)
			}
			if test.splitAt == 0 {
				if strings.Contains(strings.Join(lines, "\n"), "Primary detail") {
					t.Fatal("compact root unexpectedly rendered the detail pane")
				}
				last, _ := frame.CellAt(test.size.Columns-1, 3)
				if last.Style.Background != palette.ActiveRowBackground {
					t.Fatalf("compact selected row did not span list: %+v", last.Style)
				}
				return
			}

			detailLabel := strings.TrimRight(string([]rune(lines[2])[test.splitAt+1:]), " ")
			detailDescription := strings.TrimRight(string([]rune(lines[3])[test.splitAt+1:]), " ")
			panel, _ := frame.CellAt(test.splitAt, 3)
			if detailLabel != "Alpha" || detailDescription != "Primary detail" || panel.Style.Background != palette.PanelBackground {
				t.Fatalf("%s split fingerprint label=%q description=%q panel=%+v", test.name, detailLabel, detailDescription, panel.Style)
			}
		})
	}
}

func TestPickerApprovedFramesRemainByteStable(t *testing.T) {
	palette, _ := theme.Builtin("terminal")
	items := []Item{
		{Key: "alpha", Label: "Alpha", Description: "Primary detail", Detail: []string{"Detail line"}},
		{Key: "disabled", Label: "Disabled", Disabled: true},
	}
	tests := []struct {
		name string
		size responsive.Size
		set  func(*Picker)
	}{
		{name: "compact-root", size: responsive.Size{Columns: 48, Rows: 18}, set: func(picker *Picker) { _ = picker.applyPage(loadResult{page: Page{Items: items}}) }},
		{name: "standard-root", size: responsive.Size{Columns: 80, Rows: 19}, set: func(picker *Picker) { _ = picker.applyPage(loadResult{page: Page{Items: items}}) }},
		{name: "wide-root", size: responsive.Size{Columns: 110, Rows: 24}, set: func(picker *Picker) { _ = picker.applyPage(loadResult{page: Page{Items: items}}) }},
		{name: "compact-detail", size: responsive.Size{Columns: 48, Rows: 18}, set: func(picker *Picker) {
			_ = picker.applyPage(loadResult{page: Page{Items: items}})
			picker.screen = detailScreen
		}},
		{name: "help", size: responsive.Size{Columns: 40, Rows: 10}, set: func(picker *Picker) { picker.screen = helpScreen }},
		{name: "recovery", size: responsive.Size{Columns: 39, Rows: 9}, set: func(*Picker) {}},
		{name: "long-wide-editing", size: responsive.Size{Columns: 110, Rows: 24}, set: func(picker *Picker) {
			picker.options.Title = strings.Repeat("Long title ", 20)
			longItems := make([]Item, 30)
			for index := range longItems {
				longItems[index] = Item{Key: fmt.Sprintf("item-%02d", index), Label: strings.Repeat(fmt.Sprintf("Result %02d ", index), 20), Description: strings.Repeat("Long description ", 20), Detail: []string{strings.Repeat("Long detail ", 20)}}
			}
			_ = picker.applyPage(loadResult{page: Page{Items: longItems}})
			picker.selected = 20
			picker.query.Set(strings.Repeat("query", 30))
			picker.query.MoveLeft()
			picker.editing = true
		}},
		{name: "long-compact-detail", size: responsive.Size{Columns: 48, Rows: 18}, set: func(picker *Picker) {
			picker.options.Title = strings.Repeat("Long title ", 20)
			_ = picker.applyPage(loadResult{page: Page{Items: []Item{{Key: "long", Label: strings.Repeat("Long label ", 20), Description: strings.Repeat("Long description ", 20), Detail: []string{strings.Repeat("Long detail ", 20)}}}}})
			picker.screen = detailScreen
		}},
		{name: "long-recovery", size: responsive.Size{Columns: 39, Rows: 9}, set: func(picker *Picker) { picker.options.Title = strings.Repeat("Long title ", 20) }},
		{name: "empty", size: responsive.Size{Columns: 80, Rows: 19}, set: func(picker *Picker) {
			_ = picker.applyPage(loadResult{page: Page{}})
			picker.loaded = true
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			picker := newPickerForTest(t)
			picker.layout = responsive.Resolve(test.size)
			test.set(picker)
			frame, err := picker.Render(shell.RenderContext{Layout: picker.layout, Theme: palette})
			if err != nil {
				t.Fatal(err)
			}
			got, err := view.PNG(frame)
			if err != nil {
				t.Fatal(err)
			}
			fixture := filepath.Join("testdata", "approved", test.name+".png")
			if os.Getenv("HERDR_UPDATE_GOLDENS") == "1" {
				if err := os.MkdirAll(filepath.Dir(fixture), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fixture, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				artifactRoot := filepath.Join("..", "..", ".artifacts", "test-failures")
				if err := os.MkdirAll(artifactRoot, 0o700); err != nil {
					t.Fatal(err)
				}
				artifactDir, err := os.MkdirTemp(artifactRoot, "picker-"+test.name+"-")
				if err != nil {
					t.Fatal(err)
				}
				actual := filepath.Join(artifactDir, "actual.png")
				if err := os.WriteFile(actual, got, 0o600); err != nil {
					t.Fatal(err)
				}
				t.Fatalf("approved frame changed; compare expected %s with actual %s\nvisible frame:\n%s", fixture, actual, view.ANSI(frame))
			}
		})
	}
}

func TestPickerDetailBasicKeysAndEmptySemanticState(t *testing.T) {
	picker := newPickerForTest(t)
	picker.options.Activate = func(context.Context, Item) error { return nil }
	picker.layout = responsive.Resolve(responsive.Size{Columns: 48, Rows: 18})
	_ = picker.applyPage(loadResult{page: Page{Items: []Item{{Key: "one", Label: "One"}}}})
	picker.screen = detailScreen
	for _, key := range []string{"a", "o"} {
		picker.pendingActivation = false
		if effects := picker.text(shell.EventContext{}, key); len(effects) != 0 || !picker.hasError {
			t.Fatalf("detail key %q did not attempt activation without a shell: effects=%v error=%t", key, effects, picker.hasError)
		}
	}
	picker.hasError = false
	if effects := picker.text(shell.EventContext{}, "?"); len(effects) != 0 || picker.screen != helpScreen || picker.returnScreen != detailScreen {
		t.Fatalf("detail help transition = effects=%v screen=%d return=%d", effects, picker.screen, picker.returnScreen)
	}
	picker.screen = rootScreen
	picker.loaded, picker.pages = true, []loadedPage{{page: Page{}}}
	state := picker.DiagnosticState()
	if state.State.String() != "empty" || state.ItemCount != 0 {
		t.Fatalf("empty semantic state = %+v", state)
	}
}

func TestPickerHelpStatusAndCursorPresentation(t *testing.T) {
	picker := newPickerForTest(t)
	picker.layout = responsive.Resolve(responsive.Size{Columns: 80, Rows: 18})
	picker.query.Set("A界")
	picker.query.MoveLeft()
	picker.editing = true
	if got := picker.queryWithCursor(); got != "A▏界" {
		t.Fatalf("query cursor presentation = %q", got)
	}

	statusTests := []struct {
		name  string
		apply func()
		want  string
	}{
		{name: "ready", apply: func() {}, want: "Ready"},
		{name: "loading", apply: func() { picker.pendingLoad = true }, want: "Loading…"},
		{name: "activating wins", apply: func() { picker.pendingActivation = true }, want: "Working · activating"},
		{name: "error", apply: func() { picker.pendingActivation, picker.pendingLoad, picker.hasError = false, false, true }, want: "! Recoverable error"},
		{name: "activated", apply: func() { picker.hasError, picker.activated = false, true }, want: "OK Activated"},
	}
	for _, test := range statusTests {
		test.apply()
		if got := picker.status(); got != test.want {
			t.Fatalf("%s status = %q, want %q", test.name, got, test.want)
		}
	}

	picker.screen, picker.returnScreen = helpScreen, rootScreen
	palette, _ := theme.Builtin("terminal")
	frame, err := picker.Render(shell.RenderContext{Layout: picker.layout, Theme: palette})
	if err != nil {
		t.Fatal(err)
	}
	plain := stripANSI(view.ANSI(frame))
	if !strings.Contains(plain, "PICKER HELP") || !strings.Contains(plain, "Home/End first/last") || !strings.Contains(plain, "q/Ctrl-C quit") || strings.Contains(plain, "Search:") {
		t.Fatalf("help frame fingerprint:\n%s", plain)
	}
	state := picker.DiagnosticState()
	if state.Screen.String() != "picker.help" || state.Focus.String() != "help" || state.State.String() != "activated" {
		t.Fatalf("help diagnostic state = %+v", state)
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

func TestPickerCompletionErrorsAreRecoverableAndGenerationScoped(t *testing.T) {
	picker := newPickerForTest(t)
	_ = picker.applyPage(loadResult{page: Page{Items: []Item{{Key: "kept", Label: "Kept"}}}})
	picker.loadGeneration = 3

	picker.pendingLoad = true
	picker.result(shell.ResultEvent{Key: picker.loadKey, Generation: 3, Result: shell.WorkResult{Err: errors.New("provider failed"), Code: diagnostics.OutcomeFailed}})
	if picker.pendingLoad || !picker.hasError || picker.items()[0].Key != "kept" {
		t.Fatalf("load error state pending=%t error=%t items=%v", picker.pendingLoad, picker.hasError, picker.items())
	}

	picker.pendingLoad, picker.hasError = true, false
	picker.result(shell.ResultEvent{Key: picker.loadKey, Generation: 3, Result: shell.WorkResult{Value: "wrong result type", Code: diagnostics.OutcomeApplied}})
	if picker.pendingLoad || !picker.hasError || picker.items()[0].Key != "kept" {
		t.Fatalf("invalid result state pending=%t error=%t items=%v", picker.pendingLoad, picker.hasError, picker.items())
	}

	picker.pendingLoad, picker.hasError = true, false
	picker.result(shell.ResultEvent{Key: picker.loadKey, Generation: 3, Result: shell.WorkResult{Value: loadResult{page: Page{Items: []Item{{Label: "missing key"}}}}, Code: diagnostics.OutcomeApplied}})
	if picker.pendingLoad || !picker.hasError || picker.items()[0].Key != "kept" {
		t.Fatalf("invalid page state pending=%t error=%t items=%v", picker.pendingLoad, picker.hasError, picker.items())
	}

	picker.activationGeneration = 4
	picker.pendingActivation, picker.hasError, picker.activated = true, false, true
	picker.result(shell.ResultEvent{Key: picker.activationKey, Generation: 4, Result: shell.WorkResult{Err: errors.New("activation failed"), Code: diagnostics.OutcomeFailed}})
	if picker.pendingActivation || !picker.hasError || picker.activated {
		t.Fatalf("activation error state pending=%t error=%t activated=%t", picker.pendingActivation, picker.hasError, picker.activated)
	}

	unknown, _ := shell.NewRequestKey("interaction.picker.unknown")
	before := *picker
	picker.result(shell.ResultEvent{Key: unknown, Generation: 99, Result: shell.WorkResult{Code: diagnostics.OutcomeApplied}})
	if picker.pendingLoad != before.pendingLoad || picker.pendingActivation != before.pendingActivation || picker.hasError != before.hasError || picker.activated != before.activated {
		t.Fatal("unrelated completion changed picker status")
	}
}

func TestPickerActivationBoundaries(t *testing.T) {
	picker := newPickerForTest(t)
	if effects := picker.activate(shell.EventContext{}); len(effects) != 0 || picker.hasError {
		t.Fatalf("empty activation produced effects/error: %d/%t", len(effects), picker.hasError)
	}
	_ = picker.applyPage(loadResult{page: Page{Items: []Item{{Key: "disabled", Disabled: true}}}})
	if effects := picker.activate(shell.EventContext{}); len(effects) != 0 || picker.hasError {
		t.Fatalf("disabled activation produced effects/error: %d/%t", len(effects), picker.hasError)
	}
	_ = picker.applyPage(loadResult{page: Page{Items: []Item{{Key: "enabled", Label: "Enabled"}}}})
	if effects := picker.activate(shell.EventContext{}); len(effects) != 0 || picker.hasError {
		t.Fatalf("nil activator produced effects/error: %d/%t", len(effects), picker.hasError)
	}
	picker.options.Activate = func(context.Context, Item) error { return nil }
	picker.pendingActivation = true
	if effects := picker.activate(shell.EventContext{}); len(effects) != 0 || picker.hasError {
		t.Fatalf("pending activation produced effects/error: %d/%t", len(effects), picker.hasError)
	}
	picker.pendingActivation = false
	if effects := picker.activate(shell.EventContext{}); len(effects) != 0 || !picker.hasError || picker.pendingActivation {
		t.Fatalf("start failure state effects=%d error=%t pending=%t", len(effects), picker.hasError, picker.pendingActivation)
	}
}

func TestPickerTextAndKeyStateModel(t *testing.T) {
	picker := newPickerForTest(t)
	_ = picker.applyPage(loadResult{page: Page{Items: []Item{{Key: "one", Label: "One"}, {Key: "off", Label: "Off", Disabled: true}, {Key: "two", Label: "Two"}}}})

	picker.Update(shell.EventContext{}, shell.TextEvent{Text: "/"})
	if !picker.editing {
		t.Fatal("slash did not focus search")
	}
	picker.Update(shell.EventContext{}, shell.TextEvent{Text: "界"})
	if picker.query.Text() != "界" || !picker.hasError {
		t.Fatalf("text edit/start failure = query %q error=%t", picker.query.Text(), picker.hasError)
	}
	picker.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyLeft})
	picker.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyRight})
	picker.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyHome})
	picker.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnd})
	if picker.query.Cursor() != 1 {
		t.Fatalf("editing cursor = %d", picker.query.Cursor())
	}
	picker.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyBackspace})
	if picker.query.Text() != "" {
		t.Fatalf("backspace query = %q", picker.query.Text())
	}
	picker.query.Set("ab")
	picker.query.MoveHome()
	picker.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyDelete})
	if picker.query.Text() != "b" {
		t.Fatalf("delete query = %q", picker.query.Text())
	}
	picker.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyTab})
	if picker.editing {
		t.Fatal("tab did not leave search")
	}

	picker.query.Set("")
	picker.hasError = false
	picker.Update(shell.EventContext{}, shell.TextEvent{Text: "j"})
	if picker.selectedKey != "two" {
		t.Fatalf("j selection = %q", picker.selectedKey)
	}
	picker.Update(shell.EventContext{}, shell.TextEvent{Text: "k"})
	if picker.selectedKey != "one" {
		t.Fatalf("k selection = %q", picker.selectedKey)
	}
	picker.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnd})
	picker.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyHome})
	if picker.selectedKey != "one" {
		t.Fatalf("boundary key selection = %q", picker.selectedKey)
	}
	picker.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyTab})
	if !picker.editing {
		t.Fatal("tab did not enter search")
	}
	picker.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEscape})
	if picker.editing {
		t.Fatal("escape did not leave search")
	}

	picker.Update(shell.EventContext{}, shell.TextEvent{Text: "?"})
	if picker.screen != helpScreen || picker.returnScreen != rootScreen {
		t.Fatalf("help transition = screen %d return %d", picker.screen, picker.returnScreen)
	}
	picker.Update(shell.EventContext{}, shell.TextEvent{Text: "?"})
	if picker.screen != rootScreen {
		t.Fatalf("question mark did not close help: %d", picker.screen)
	}
	picker.screen, picker.returnScreen = helpScreen, detailScreen
	picker.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
	if picker.screen != detailScreen {
		t.Fatalf("enter did not return from help: %d", picker.screen)
	}
	picker.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyBackspace})
	if picker.screen != rootScreen {
		t.Fatalf("backspace did not leave detail: %d", picker.screen)
	}

	for _, event := range []shell.Event{shell.TextEvent{Text: "q"}, shell.KeyEvent{Code: shell.KeyEscape}, shell.KeyEvent{Code: shell.KeyBackspace}, shell.KeyEvent{Code: shell.KeyCtrlC}} {
		picker.screen = rootScreen
		if effects := picker.Update(shell.EventContext{}, event); len(effects) != 1 {
			t.Fatalf("quit event %#v produced %d effects", event, len(effects))
		}
	}
	picker.screen, picker.returnScreen = helpScreen, rootScreen
	if effects := picker.Update(shell.EventContext{}, shell.TextEvent{Text: "q"}); len(effects) != 1 {
		t.Fatalf("help q produced %d effects", len(effects))
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

func TestPickerDebouncesQueryBurstToFinalText(t *testing.T) {
	requested := make(chan string, 8)
	picker, err := NewPicker(PickerOptions{
		Title: "Commands",
		Load: func(_ context.Context, query string, _ Cursor) (Page, error) {
			requested <- query
			return Page{Items: []Item{{Key: "run", Label: "Run"}}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	surface := &signalSurface{picker: picker, loaded: make(chan struct{}, 8), applied: make(chan struct{}, 1)}
	palette, _ := theme.Builtin("terminal")
	input, inputWriter := io.Pipe()
	t.Cleanup(func() { _ = inputWriter.Close() })
	program, err := shell.NewProgram(shell.ProgramOptions{PluginID: semanticID("test-picker-debounce"), Theme: palette, Events: discardSemanticSink{}, Input: input, Output: io.Discard}, surface)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, runErr := program.Run(); done <- runErr }()
	t.Cleanup(func() { program.Kill(); <-done })

	program.Send(tea.WindowSizeMsg{Width: 80, Height: 19})
	if got := <-requested; got != "" {
		t.Fatalf("initial query = %q", got)
	}
	waitSignal(t, surface.loaded, "initial load")
	program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	for _, value := range []rune{'a', 'b', 'c'} {
		program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{value}})
	}
	select {
	case got := <-requested:
		if got != "abc" {
			t.Fatalf("debounced query = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("debounced load did not run")
	}
	select {
	case extra := <-requested:
		t.Fatalf("query burst produced extra load for %q", extra)
	case <-time.After(2 * queryDebounce):
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
