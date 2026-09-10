package debugui

import (
	"context"
	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"testing"
)

func TestDebugKeymapActionsMatchKeys(t *testing.T) {
	surface := &Surface{loaded: true, screen: screenTimeline, list: screenTimeline, page: diagnostics.DebugPage{Events: []diagnostics.DebugEvent{{Sequence: 1}, {Sequence: 2}}}}
	if _, err := keymap.New(surface.KeymapCatalog()); err != nil {
		t.Fatal(err)
	}
	surface.Update(shell.EventContext{}, shell.ActionEvent{ID: "debug.next"})
	if surface.selected != 1 {
		t.Fatalf("action selection=%d", surface.selected)
	}
	surface.selected = 0
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyDown})
	if surface.selected != 1 {
		t.Fatalf("key selection=%d", surface.selected)
	}
	before := surface.KeymapContext().Target
	surface.selected = 0
	if after := surface.KeymapContext().Target; after == before {
		t.Fatal("target did not change with selection")
	}
	if surface.ActionState("missing").Enabled {
		t.Fatal("unknown action enabled")
	}
	defaults := map[string]bool{}
	for _, action := range surface.KeymapCatalog() {
		if action.Context == "debug.timeline" {
			for _, sequence := range action.Defaults {
				defaults[action.ID+":"+sequence[0]] = true
			}
		}
	}
	if !defaults["debug.freeze:p"] || !defaults["debug.hide-debugger:v"] {
		t.Fatal("legacy debug toggles missing")
	}
	unloaded := &Surface{screen: screenHealth}
	if !unloaded.ActionState("debug.freeze").Enabled {
		t.Fatal("freeze disabled before first load")
	}
	detail := &Surface{loaded: true, screen: screenDetail, detail: &diagnostics.DebugEvent{Sequence: 1}}
	if !detail.ActionState("debug.next-page").Enabled || !detail.ActionState("debug.previous-page").Enabled {
		t.Fatal("detail scrolling disabled by page availability")
	}
	health := &Surface{loaded: true, screen: screenHealth, page: surface.page}
	if health.ActionState("debug.open").Enabled {
		t.Fatal("open enabled outside timeline/gallery")
	}
}

func TestDebugKeymapContextAndAvailability(t *testing.T) {
	surface := &Surface{screen: screenTimeline, list: screenTimeline}
	if surface.KeymapContext().Editing {
		t.Fatal("timeline context reports editing")
	}
	surface.editing = true
	if context := surface.KeymapContext(); context.ID != "debug.editing" || !context.Editing {
		t.Fatalf("editing context = %+v", context)
	}
	for _, action := range surface.KeymapCatalog() {
		if (action.Context == "debug.editing") != action.Editing {
			t.Fatalf("action %s editing=%t in %s", action.ID, action.Editing, action.Context)
		}
	}
	if surface.ActionState("debug.open").Enabled || surface.ActionState("debug.freeze").Enabled {
		t.Fatal("wrong-context debug action enabled")
	}
	surface.editing = false
	for _, id := range []string{"debug.open", "debug.filter-component", "debug.filter-action", "debug.filter-code", "debug.filter-correlation"} {
		if surface.ActionState(id).Enabled {
			t.Fatalf("%s enabled without selection", id)
		}
	}
	if surface.ActionState("debug.export").Enabled {
		t.Fatal("export enabled without exporter and view")
	}
	surface.page.Events = []diagnostics.DebugEvent{{Sequence: 1}}
	for _, id := range []string{"debug.open", "debug.filter-component", "debug.filter-action", "debug.filter-code", "debug.filter-correlation"} {
		if !surface.ActionState(id).Enabled {
			t.Fatalf("%s disabled with timeline selection", id)
		}
	}
	surface.options.Exporter = func(context.Context, []byte) error { return nil }
	if surface.ActionState("debug.export").Enabled {
		t.Fatal("export enabled without selected view")
	}
	surface.view = &diagnostics.DebugView{}
	if !surface.ActionState("debug.export").Enabled {
		t.Fatal("export disabled with exporter and view")
	}
	surface.export = exportPending
	if surface.ActionState("debug.export").Enabled {
		t.Fatal("export enabled while pending")
	}
	surface.export = exportReady
	surface.screen = screenHealth
	if surface.ActionState("debug.open").Enabled {
		t.Fatal("open enabled in health context")
	}
	if surface.ActionState("debug.edit-apply").Enabled || surface.ActionState("missing").Enabled {
		t.Fatal("wrong-context or unknown action enabled")
	}
}
