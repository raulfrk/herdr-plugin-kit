package form

import (
	"fmt"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
)

func (model *Model) KeymapCatalog() []keymap.Action {
	a := func(id, label string, keys ...string) keymap.Action {
		d := make([]keymap.Sequence, len(keys))
		for i, k := range keys {
			d[i] = keymap.Sequence{k}
		}
		return keymap.Action{ID: id, Label: label, Context: "form.editing", Editing: true, Quick: id == "form.apply" || id == "form.rollback", Defaults: d}
	}
	return []keymap.Action{
		a("form.quit", "Quit", "escape"), a("form.next-field", "Next field", "tab", "down"), a("form.previous-field", "Previous field", "up"),
		a("form.cursor-left", "Move cursor left", "left"), a("form.cursor-right", "Move cursor right", "right"), a("form.cursor-home", "Move to start", "home"), a("form.cursor-end", "Move to end", "end"),
		a("form.backspace", "Delete before cursor", "backspace"), a("form.delete", "Delete at cursor", "delete"), a("form.apply", "Apply changes", "enter"), a("form.rollback", "Roll back", "alt+r"),
	}
}

func (model *Model) KeymapContext() keymap.Context {
	return keymap.Context{ID: "form.editing", Editing: true, Target: fmt.Sprintf("%s:%d", model.fields[model.focus].ID, model.targetRevision)}
}

func (model *Model) ActionState(id string) keymap.State {
	known := false
	for _, a := range model.KeymapCatalog() {
		if a.ID == id {
			known = true
			break
		}
	}
	if !known {
		return keymap.State{Reason: "unknown action"}
	}
	switch id {
	case "form.apply":
		return keymap.State{Enabled: model.options.Apply != nil && !model.pendingApply && !model.pendingRollback && !model.compensationNeeded, Reason: "apply unavailable"}
	case "form.rollback":
		return keymap.State{Enabled: model.rollbackToken != "" && !model.pendingApply && !model.pendingRollback, Reason: "rollback unavailable"}
	}
	return keymap.State{Enabled: true}
}

func (model *Model) action(events shell.EventContext, id string) []shell.Effect {
	if !model.ActionState(id).Enabled {
		return nil
	}
	switch id {
	case "form.quit":
		return model.key(events, shell.KeyEscape)
	case "form.next-field":
		return model.key(events, shell.KeyTab)
	case "form.previous-field":
		return model.key(events, shell.KeyUp)
	case "form.cursor-left":
		return model.key(events, shell.KeyLeft)
	case "form.cursor-right":
		return model.key(events, shell.KeyRight)
	case "form.cursor-home":
		return model.key(events, shell.KeyHome)
	case "form.cursor-end":
		return model.key(events, shell.KeyEnd)
	case "form.backspace":
		return model.key(events, shell.KeyBackspace)
	case "form.delete":
		return model.key(events, shell.KeyDelete)
	case "form.apply":
		return model.key(events, shell.KeyEnter)
	case "form.rollback":
		return model.Update(events, shell.TextEvent{Text: "r", Alt: true})
	}
	return nil
}
