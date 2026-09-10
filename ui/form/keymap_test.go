package form

import (
	"context"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"testing"
)

type keymapTestApplier struct{}

func (keymapTestApplier) Apply(context.Context, Values) (ApplyToken, error) { return "token", nil }
func (keymapTestApplier) Rollback(context.Context, ApplyToken) error        { return nil }

func TestFormKeymapActionsMatchKeys(t *testing.T) {
	model, err := New(Options{Fields: []Field{{ID: "one", Label: "One"}, {ID: "two", Label: "Two"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = keymap.New(model.KeymapCatalog()); err != nil {
		t.Fatal(err)
	}
	model.Update(shell.EventContext{}, shell.ActionEvent{ID: "form.next-field"})
	if model.focus != 1 {
		t.Fatalf("action focus=%d", model.focus)
	}
	model.focus = 0
	model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyDown})
	if model.focus != 1 {
		t.Fatalf("key focus=%d", model.focus)
	}
	if model.ActionState("form.apply").Enabled {
		t.Fatal("apply enabled without applier")
	}
	if model.ActionState("missing").Enabled {
		t.Fatal("unknown action enabled")
	}
	quick := map[string]bool{}
	for _, action := range model.KeymapCatalog() {
		quick[action.ID] = action.Quick
	}
	if !quick["form.apply"] || !quick["form.rollback"] {
		t.Fatal("form operations are absent from quick actions")
	}
	before := model.KeymapContext().Target
	model.rollbackValues = Values{"one": "restored", "two": ""}
	model.rollbackToken = "token"
	model.pendingRollback = true
	model.rollbackGeneration = 1
	model.resolve(shell.ResultEvent{Key: model.rollbackKey, Generation: 1})
	if model.KeymapContext().Target == before {
		t.Fatal("successful rollback did not change target")
	}
}

func TestFormKeymapContextAndAvailability(t *testing.T) {
	model, err := New(Options{Fields: []Field{{ID: "one", Label: "One"}, {ID: "two", Label: "Two"}}, Apply: keymapTestApplier{}})
	if err != nil {
		t.Fatal(err)
	}
	if context := model.KeymapContext(); context.ID != "form.editing" || !context.Editing {
		t.Fatalf("context = %+v", context)
	}
	for _, action := range model.KeymapCatalog() {
		if !action.Editing {
			t.Fatalf("form action %s is not marked editing", action.ID)
		}
	}
	for _, id := range []string{"form.next-field", "form.previous-field", "form.cursor-left"} {
		if !model.ActionState(id).Enabled {
			t.Fatalf("%s disabled", id)
		}
	}
	if !model.ActionState("form.apply").Enabled || model.ActionState("form.rollback").Enabled {
		t.Fatal("initial submit state is incorrect")
	}
	model.pendingApply = true
	if model.ActionState("form.apply").Enabled || model.ActionState("form.rollback").Enabled {
		t.Fatal("operation enabled during apply")
	}
	model.pendingApply, model.rollbackToken = false, "token"
	if !model.ActionState("form.rollback").Enabled {
		t.Fatal("rollback disabled with token")
	}
	model.pendingRollback = true
	if model.ActionState("form.apply").Enabled || model.ActionState("form.rollback").Enabled {
		t.Fatal("operation enabled during rollback")
	}
	model.pendingRollback, model.compensationNeeded = false, true
	if model.ActionState("form.apply").Enabled {
		t.Fatal("apply enabled while compensation is needed")
	}
	if !model.ActionState("form.rollback").Enabled {
		t.Fatal("rollback disabled while compensation is needed")
	}
}
