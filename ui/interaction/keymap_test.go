package interaction

import (
	"context"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"strings"
	"testing"
)

func TestPickerKeymapAndNamespace(t *testing.T) {
	loader := func(context.Context, string, Cursor) (Page, error) { return Page{}, nil }
	picker, err := NewPicker(PickerOptions{Title: "Pick", Namespace: "actions.picker", Load: loader})
	if err != nil {
		t.Fatal(err)
	}
	if picker.loadKey.String() != "actions.picker.load" || picker.queryDebounceCode.String() != "actions.picker.query" {
		t.Fatal("namespace did not disjoin request identifiers")
	}
	if picker.KeymapContext().ID != "actions.picker.results" {
		t.Fatalf("context=%q", picker.KeymapContext().ID)
	}
	if _, err = keymap.New(picker.KeymapCatalog()); err != nil {
		t.Fatal(err)
	}
	picker.pages = []loadedPage{{page: Page{Items: []Item{{Key: "a", Label: "A"}, {Key: "b", Label: "B"}}}}}
	picker.loaded = true
	picker.Update(shell.EventContext{}, shell.ActionEvent{ID: "picker.next"})
	if picker.selected != 1 {
		t.Fatalf("action selection=%d", picker.selected)
	}
	picker.selected = 0
	picker.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyDown})
	if picker.selected != 1 {
		t.Fatalf("key selection=%d", picker.selected)
	}
	if NewPickerResult, err := NewPicker(PickerOptions{Title: "Pick", Namespace: "bad namespace", Load: loader}); err == nil || NewPickerResult != nil {
		t.Fatal("invalid namespace accepted")
	}
	if tooLong, err := NewPicker(PickerOptions{Title: "Pick", Namespace: strings.Repeat("a", 64), Load: loader}); err == nil || tooLong != nil {
		t.Fatal("namespace with invalid derived IDs accepted")
	}
	picker.queryDirty = true
	if picker.ActionState(ActionOpen).Enabled {
		t.Fatal("open enabled with dirty query")
	}
}

func TestPickerKeymapContextAndAvailability(t *testing.T) {
	loader := func(context.Context, string, Cursor) (Page, error) { return Page{}, nil }
	picker, err := NewPicker(PickerOptions{Title: "Pick", Load: loader})
	if err != nil {
		t.Fatal(err)
	}
	if picker.KeymapContext().Editing {
		t.Fatal("results context reports editing")
	}
	picker.editing = true
	if context := picker.KeymapContext(); context.ID != "interaction.picker.editing" || !context.Editing {
		t.Fatalf("editing context = %+v", context)
	}
	for _, action := range picker.KeymapCatalog() {
		if (action.Context == "interaction.picker.editing") != action.Editing {
			t.Fatalf("action %s editing=%t in %s", action.ID, action.Editing, action.Context)
		}
	}
	if picker.ActionState(ActionOpen).Enabled || picker.ActionState("picker.next").Enabled {
		t.Fatal("wrong-context actions enabled while editing")
	}

	picker.editing = false
	if picker.ActionState("unknown").Enabled || picker.ActionState(ActionOpen).Enabled {
		t.Fatal("unknown or selection-dependent action enabled")
	}
	picker.pages = []loadedPage{{page: Page{Items: []Item{{Key: "a", Label: "A"}}, Next: NewCursor("next")}}, {page: Page{Items: []Item{{Key: "b", Label: "B"}}}}}
	picker.loaded, picker.selected, picker.selectedKey = true, 0, "a"
	if !picker.ActionState("picker.next-page").Enabled || picker.ActionState("picker.previous-page").Enabled {
		t.Fatal("first-page availability is incorrect")
	}
	picker.pendingLoad = true
	if picker.ActionState("picker.next-page").Enabled {
		t.Fatal("next page enabled during load")
	}
	picker.pendingLoad, picker.pageIndex, picker.selectedKey = false, 1, "b"
	if !picker.ActionState("picker.previous-page").Enabled || picker.ActionState("picker.next-page").Enabled {
		t.Fatal("last-page availability is incorrect")
	}

	picker.layout = responsive.Resolve(responsive.Size{Columns: 60, Rows: 20})
	if !picker.ActionState(ActionOpen).Enabled {
		t.Fatal("compact detail open disabled without activator")
	}
	picker.layout = responsive.Resolve(responsive.Size{Columns: 120, Rows: 30})
	if picker.ActionState(ActionOpen).Enabled {
		t.Fatal("wide activation enabled without activator")
	}
	picker.options.Activate = func(context.Context, Item) error { return nil }
	if !picker.ActionState(ActionOpen).Enabled {
		t.Fatal("open disabled with activator")
	}
	picker.screen = detailScreen
	if !picker.ActionState(ActionActivate).Enabled || picker.ActionState(ActionOpen).Enabled {
		t.Fatal("detail action context is incorrect")
	}
	picker.pendingActivation = true
	if picker.ActionState(ActionActivate).Enabled {
		t.Fatal("activate enabled while pending")
	}
	picker.pendingActivation, picker.queryDirty = false, true
	if picker.ActionState(ActionActivate).Enabled {
		t.Fatal("activate enabled with dirty query")
	}
}
