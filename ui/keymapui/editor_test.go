package keymapui

import (
	"reflect"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
)

func TestDraftChangesAreScopedToActionAndContext(t *testing.T) {
	action := keymap.Action{ID: "refresh", Context: "main", Defaults: []keymap.Sequence{{"r"}}}
	neighbors := []keymap.Binding{
		{Context: "debug", Action: "refresh", Sequences: []keymap.Sequence{{"d"}}},
		{Context: "main", Action: "open", Sequences: []keymap.Sequence{{"o"}}},
	}
	document := keymap.Document{Version: 1, Bindings: append([]keymap.Binding(nil), neighbors...)}
	check := func(want []keymap.Sequence, state string) {
		t.Helper()
		got, label := draftSequences(document, action)
		if !reflect.DeepEqual(got, want) || label != state {
			t.Fatalf("sequences = %v (%s), want %v (%s)", got, label, want, state)
		}
		if !reflect.DeepEqual(document.Bindings[:2], neighbors) {
			t.Fatal("editing changed a different action or context")
		}
	}
	check(action.Defaults, "Default")
	setBinding(&document, action, []keymap.Sequence{{"g", "r"}, {"f5"}})
	check([]keymap.Sequence{{"g", "r"}, {"f5"}}, "Custom")
	setBinding(&document, action, nil)
	check(nil, "Unbound")
	resetBinding(&document, action)
	check(action.Defaults, "Default")
	if len(document.Bindings) != 2 {
		t.Fatal("reset retained an override")
	}
}
