package keymap

import (
	"reflect"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"pgregory.net/rapid"
)

func actions() []Action {
	return []Action{
		{ID: "refresh", Label: "Refresh", Context: "results", Defaults: []Sequence{{"g", "r"}, {"f5"}}},
		{ID: "open", Label: "Open", Context: "results", Defaults: []Sequence{{"enter"}}},
		{ID: "finish", Label: "Finish", Context: "editing", Editing: true, Defaults: []Sequence{{"enter"}}},
		{ID: "command", Label: "Command", Context: "editing", Editing: true, Defaults: []Sequence{{"ctrl+k", "r"}}},
	}
}
func runtimeForTest(t *testing.T) *Runtime {
	t.Helper()
	r, err := New(actions())
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestNormalize(t *testing.T) {
	for input, want := range map[string]string{"Alt+d": "alt+d", "alt+D": "alt+D", "alt+i": "alt+i", "alt++": "alt++", "ctrl+?": "backspace", "ctrl+i": "tab", "Ctrl+M": "enter", "esc": "escape", "pgdown": "page-down", "shift+tab": "shift+tab", "ctrl+alt+k": "ctrl+alt+k", "ctrl+shift+left": "ctrl+shift+left", "ctrl+page-up": "ctrl+page-up", "作": "作", "D": "D", " ": "space"} {
		got, err := Normalize(input)
		if err != nil || got != want {
			t.Errorf("%q = %q, %v; want %q", input, got, err, want)
		}
	}
}

func TestCatalogRejectsDuplicateDeclarationsAndMissingLabels(t *testing.T) {
	for _, catalog := range [][]Action{
		{{ID: "one", Label: "One", Context: "results"}, {ID: "one", Label: "Another", Context: "results"}},
		{{ID: "one", Context: "results"}},
	} {
		if _, err := New(catalog); err == nil {
			t.Fatalf("accepted ambiguous or unlabelled actions: %v", catalog)
		}
	}
}

func TestContinuationsAndUnmatchedEditingInput(t *testing.T) {
	r, err := New([]Action{
		{ID: "first", Label: "First", Context: "editing", Editing: true, Defaults: []Sequence{{"ctrl+k", "r"}}},
		{ID: "second", Label: "Second", Context: "editing", Editing: true, Defaults: []Sequence{{"ctrl+g", "q"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	context := Context{ID: "editing", Editing: true}
	r.Step(context, "ctrl+k", now)
	if got := r.Continuations(); !reflect.DeepEqual(got, []string{"r"}) {
		t.Fatalf("continuations include another prefix: %v", got)
	}
	if action, consumed := r.Step(context, "x", now); action != "" || consumed || len(r.Pending()) != 0 {
		t.Fatal("mismatching printable input was not returned as fresh text", action, consumed)
	}
	if action, consumed := r.Step(context, "ctrl+f", now); action != "" || !consumed {
		t.Fatal("unbound modified input escaped command handling", action, consumed)
	}
}
func TestEffectiveValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		binding Binding
	}{
		{"same action prefix", Binding{"results", "refresh", []Sequence{{"g"}, {"g", "r"}}}},
		{"same action duplicate", Binding{"results", "refresh", []Sequence{{"g"}, {"g"}}}},
		{"other action", Binding{"results", "refresh", []Sequence{{"enter"}}}},
		{"alias", Binding{"editing", "command", []Sequence{{"ctrl+m"}}}},
		{"reserved", Binding{"results", "refresh", []Sequence{{"ctrl+c"}}}},
		{"printable editing", Binding{"editing", "command", []Sequence{{"g", "r"}}}},
		{"unknown", Binding{"other", "refresh", nil}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := runtimeForTest(t)
			before := r.Document()
			if err := r.Apply(Document{Version: 1, Bindings: []Binding{test.binding}}); err == nil {
				t.Fatal("accepted invalid map")
			}
			if !reflect.DeepEqual(before, r.Document()) {
				t.Fatal("failed update changed active map")
			}
		})
	}
	r := runtimeForTest(t)
	if err := r.Apply(Document{Version: 1, Bindings: []Binding{{"results", "refresh", []Sequence{}}}}); err != nil {
		t.Fatal(err)
	}
	if len(r.Effective("results", "refresh")) != 0 {
		t.Fatal("clearing retained defaults")
	}
	if err := r.Apply(Document{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if len(r.Effective("results", "refresh")) != 2 {
		t.Fatal("reset failed")
	}
}
func TestSequenceLifecycle(t *testing.T) {
	r := runtimeForTest(t)
	now := time.Unix(100, 0)
	context := Context{ID: "results", Target: "one"}
	if action, consumed := r.Step(context, "g", now); action != "" || !consumed {
		t.Fatal(action, consumed)
	}
	if !reflect.DeepEqual(r.Continuations(), []string{"r"}) {
		t.Fatal(r.Continuations())
	}
	if action, _ := r.Step(context, "r", now.Add(time.Second/2)); action != "refresh" {
		t.Fatal(action)
	}
	r.Step(context, "g", now)
	if action, _ := r.Step(context, "enter", now); action != "open" {
		t.Fatal("mismatch did not process once", action)
	}
	r.Step(context, "g", now)
	if action, _ := r.Step(context, "r", now.Add(time.Second)); action != "" {
		t.Fatal("expired prefix invoked", action)
	}
	r.Step(context, "g", now)
	context.Target = "two"
	if action, _ := r.Step(context, "r", now); action != "" {
		t.Fatal("stale target invoked", action)
	}
	editing := Context{ID: "editing", Editing: true}
	r.Step(editing, "ctrl+k", now)
	if action, consumed := r.Step(editing, "x", now); action != "" || consumed {
		t.Fatal("mismatch should insert fresh printable stroke")
	}
}
func TestPropertySequenceInvokesExactlyOnce(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r, _ := New(actions())
		now := time.Unix(100, 0)
		context := Context{ID: "results"}
		n := rapid.IntRange(1, 80).Draw(t, "repetitions")
		count := 0
		for i := 0; i < n; i++ {
			for _, stroke := range []string{"g", "r"} {
				action, _ := r.Step(context, stroke, now)
				if action != "" {
					count++
				}
				now = now.Add(time.Millisecond)
			}
		}
		if count != n || len(r.Pending()) != 0 {
			t.Fatalf("%d invocations for %d sequences", count, n)
		}
	})
}

func TestBubbleTeaKeyNamesAndUnsupportedCombinations(t *testing.T) {
	for code := tea.KeyF20; code <= tea.KeyBackspace; code++ {
		if code == tea.KeyRunes {
			continue
		}
		for _, alt := range []bool{false, true} {
			name := (tea.Key{Type: code, Alt: alt}).String()
			if name == "" {
				continue
			}
			if _, err := Normalize(name); err != nil {
				t.Errorf("received key %q unsupported: %v", name, err)
			}
		}
	}
	for _, name := range []string{"ctrl+f5", "ctrl+ab", "ctrl+作", "shift+1", "shift+escape", "meta+a", "ctrl+ctrl+a", "f01", "f21", ""} {
		if _, err := Normalize(name); err == nil {
			t.Errorf("accepted unsupported %q", name)
		}
	}
}
