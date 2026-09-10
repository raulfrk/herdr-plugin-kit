package interaction

import (
	"fmt"

	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
)

const (
	ActionOpen     = "picker.open"
	ActionActivate = "picker.activate"
)

func (picker *Picker) keymapPrefix() string {
	if picker.options.Namespace != "" {
		return picker.options.Namespace
	}
	return "interaction.picker"
}

func (picker *Picker) KeymapCatalog() []keymap.Action {
	p := picker.keymapPrefix()
	a := func(id, label, context string, quick bool, keys ...string) keymap.Action {
		defaults := make([]keymap.Sequence, len(keys))
		for i, k := range keys {
			defaults[i] = keymap.Sequence{k}
		}
		return keymap.Action{ID: id, Label: label, Context: p + "." + context, Editing: context == "editing", Quick: quick, Defaults: defaults}
	}
	return []keymap.Action{
		a("picker.quit", "Quit", "results", true, "q", "escape", "backspace"), a("picker.search", "Search", "results", true, "/", "tab"),
		a("picker.help", "Help", "results", true, "?"), a("picker.next", "Next item", "results", false, "j", "down"), a("picker.previous", "Previous item", "results", false, "k", "up"),
		a("picker.first", "First item", "results", false, "home"), a("picker.last", "Last item", "results", false, "end"), a("picker.next-page", "Next page", "results", true, "n", "page-down"), a("picker.previous-page", "Previous page", "results", true, "p", "page-up"), a(ActionOpen, "Open", "results", true, "o", "enter"),
		a("picker.edit-finish", "Finish search", "editing", true, "escape", "enter", "tab"), a("picker.edit-backspace", "Delete before cursor", "editing", false, "backspace"), a("picker.edit-delete", "Delete at cursor", "editing", false, "delete"), a("picker.edit-left", "Move cursor left", "editing", false, "left"), a("picker.edit-right", "Move cursor right", "editing", false, "right"), a("picker.edit-home", "Move to start", "editing", false, "home"), a("picker.edit-end", "Move to end", "editing", false, "end"),
		a("picker.detail-back", "Back to results", "detail", true, "escape", "backspace"), a(ActionActivate, "Activate", "detail", true, "a", "o", "enter"), a("picker.help", "Help", "detail", true, "?"),
		a("picker.help-back", "Close help", "help", true, "?", "escape", "backspace", "enter"), a("picker.quit", "Quit", "help", true, "q"),
	}
}

func (picker *Picker) KeymapContext() keymap.Context {
	name := "results"
	if picker.screen == helpScreen {
		name = "help"
	} else if picker.editing {
		name = "editing"
	} else if picker.screen == detailScreen {
		name = "detail"
	}
	target := fmt.Sprintf("%s:%d:%d", picker.selectedKey, picker.pageIndex, picker.targetRevision)
	return keymap.Context{ID: picker.keymapPrefix() + "." + name, Editing: name == "editing", Target: target}
}

func (picker *Picker) ActionState(id string) keymap.State {
	ctx := picker.KeymapContext().ID
	known := false
	for _, action := range picker.KeymapCatalog() {
		if action.ID == id && action.Context == ctx {
			known = true
			break
		}
	}
	if !known {
		return keymap.State{Reason: "unavailable in current view"}
	}
	switch id {
	case "picker.open", "picker.activate":
		_, ok := picker.selectedItem()
		enabled := ok && !picker.pendingActivation && !picker.queryDirty
		if id == ActionActivate {
			enabled = enabled && picker.options.Activate != nil
		} else {
			enabled = enabled && (picker.options.Activate != nil || compactPresentation(picker.layout))
		}
		return keymap.State{Enabled: enabled, Reason: "activation unavailable"}
	case "picker.next-page":
		enabled := false
		if picker.pageIndex >= 0 && picker.pageIndex < len(picker.pages) {
			enabled = !picker.pages[picker.pageIndex].page.Next.Empty()
		}
		return keymap.State{Enabled: !picker.pendingLoad && enabled, Reason: "no next page"}
	case "picker.previous-page":
		return keymap.State{Enabled: picker.pageIndex > 0, Reason: "no previous page"}
	}
	return keymap.State{Enabled: true}
}

func (picker *Picker) action(events shell.EventContext, id string) []shell.Effect {
	if !picker.ActionState(id).Enabled {
		return nil
	}
	switch id {
	case "picker.quit":
		return picker.text(events, "q")
	case "picker.search":
		return picker.text(events, "/")
	case "picker.help":
		return picker.text(events, "?")
	case "picker.help-back":
		return picker.key(events, shell.KeyEscape)
	case "picker.next":
		return picker.key(events, shell.KeyDown)
	case "picker.previous":
		return picker.key(events, shell.KeyUp)
	case "picker.first":
		return picker.key(events, shell.KeyHome)
	case "picker.last":
		return picker.key(events, shell.KeyEnd)
	case "picker.next-page":
		return picker.key(events, shell.KeyPageDown)
	case "picker.previous-page":
		return picker.key(events, shell.KeyPageUp)
	case "picker.open":
		return picker.key(events, shell.KeyEnter)
	case "picker.detail-back":
		return picker.key(events, shell.KeyEscape)
	case "picker.activate":
		return picker.key(events, shell.KeyEnter)
	case "picker.edit-finish":
		return picker.key(events, shell.KeyEnter)
	case "picker.edit-backspace":
		return picker.key(events, shell.KeyBackspace)
	case "picker.edit-delete":
		return picker.key(events, shell.KeyDelete)
	case "picker.edit-left":
		return picker.key(events, shell.KeyLeft)
	case "picker.edit-right":
		return picker.key(events, shell.KeyRight)
	case "picker.edit-home":
		return picker.key(events, shell.KeyHome)
	case "picker.edit-end":
		return picker.key(events, shell.KeyEnd)
	}
	return nil
}
