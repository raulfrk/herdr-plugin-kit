package keymapui

import (
	"fmt"
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/ui/interaction"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
)

type row struct {
	operation string
	action    keymap.Action
	index     int
}

func commandRow(id, label string) interaction.Item {
	return interaction.Item{Key: id, Label: label, Value: row{operation: id}}
}

func (s *Surface) openActions(events shell.EventContext) []shell.Effect {
	if s.view == actionsScreen {
		return s.back(events)
	}
	s.menuOrigin = s.view
	s.menuContext = s.contentContext()
	s.menuFocused = s.actionsFocused
	s.actionsFocused = false
	var items []interaction.Item
	for _, action := range s.runtime.Catalog() {
		if action.Context != s.menuContext.ID || !action.Quick {
			continue
		}
		state := s.ActionState(action.ID)
		description := sequenceLabel(s.runtime.Effective(action.Context, action.ID))
		if !state.Enabled {
			description = state.Reason
		}
		items = append(items, interaction.Item{Key: action.ID, Label: action.Label, Description: description, Disabled: !state.Enabled, Value: row{operation: "invoke", action: action}})
	}
	s.view = actionsScreen
	s.actionsFocused = false
	s.status = ""
	effects := s.menu.replace(events, items)
	return append(effects, s.resizeActive(events)...)
}

func (s *Surface) activateRow(events shell.EventContext) []shell.Effect {
	if s.saving && s.draftOpen {
		s.status = "Saving changes; please wait."
		return nil
	}
	picker := s.activePicker()
	if picker == nil {
		return nil
	}
	item, ok := picker.Selected()
	if !ok || picker.DiagnosticState().Pending {
		return nil
	}
	selection, ok := item.Value.(row)
	if !ok {
		return nil
	}
	switch selection.operation {
	case "invoke":
		s.view = s.menuOrigin
		s.actionsFocused = false
		if s.contentContext() != s.menuContext {
			s.view = actionsScreen
			s.status = "Target changed; reopen Actions."
			return nil
		}
		state := s.ActionState(selection.action.ID)
		if !state.Enabled {
			s.view = actionsScreen
			s.status = state.Reason
			return nil
		}
		return s.action(events, selection.action.ID)
	case "edit":
		s.selected = selection.action
		s.view = entryScreen
		return s.refreshEntry(events)
	case "keymap.save":
		return s.save(events)
	case "keymap.discard":
		return s.closeEditor(events)
	case "keymap.reload":
		s.reloadRequested = true
		return s.load(events)
	case "keymap.reset-all":
		s.draft = keymap.Document{Version: 1}
		s.status = "Defaults restored in draft; save to apply."
		return s.refreshEditor(events)
	case "keymap.conflicts":
		return s.action(events, "keymap.help")
	case "keymap.checker":
		s.openChecker()
		return nil
	case "record", "replace-record":
		s.alias = selection.index
		s.captured = nil
		s.view = captureScreen
		s.status = ""
		s.runtime.Cancel()
		return nil
	case "type", "replace-type":
		s.alias = selection.index
		s.text = interaction.NewBuffer("")
		if selection.index >= 0 {
			sequences, _ := draftSequences(s.draft, s.selected)
			if selection.index < len(sequences) {
				s.text.Set(strings.Join(sequences[selection.index], " "))
			}
		}
		s.view = textScreen
		s.status = ""
		return nil
	case "remove":
		sequences, _ := draftSequences(s.draft, s.selected)
		if selection.index >= 0 && selection.index < len(sequences) {
			next := append([]keymap.Sequence(nil), sequences[:selection.index]...)
			next = append(next, sequences[selection.index+1:]...)
			setBinding(&s.draft, s.selected, next)
		}
		return s.refreshEntry(events)
	case "clear":
		setBinding(&s.draft, s.selected, []keymap.Sequence{})
		return s.refreshEntry(events)
	case "reset":
		resetBinding(&s.draft, s.selected)
		return s.refreshEntry(events)
	case "back":
		return s.back(events)
	}
	return nil
}

func (s *Surface) openEditor(events shell.EventContext) []shell.Effect {
	if !s.draftOpen {
		s.editorOrigin = s.view
		s.draftOpen = true
		s.resetDraft()
	}
	s.view = editorScreen
	s.actionsFocused = false
	s.status = "Changes stay in the draft until saved."
	return s.refreshEditor(events)
}

func (s *Surface) resetDraft() {
	s.draft = s.runtime.Document()
	if s.options.DefaultKeymap && s.disk.valid {
		s.draft = keymap.Clone(s.disk.document)
	}
	s.draftRevision = s.disk.revision
}

func (s *Surface) refreshEditor(events shell.EventContext) []shell.Effect {
	items := []interaction.Item{commandRow("keymap.save", "Save changes"), commandRow("keymap.discard", "Discard and return"), commandRow("keymap.reset-all", "Reset all overrides"), commandRow("keymap.reload", "Reload saved shortcuts"), commandRow("keymap.checker", "Key checker")}
	for index := range items {
		if s.saving {
			items[index].Disabled = true
			items[index].Description = "Saving changes"
		}
	}
	if err := s.runtime.Validate(s.draft); err != nil {
		items[0].Disabled = true
		items[0].Description = err.Error()
		items = append(items, commandRow("keymap.conflicts", "View conflict details"))
	}
	if s.disk.revision != s.draftRevision {
		items[0].Disabled = true
		items[0].Description = "File changed; reload or discard"
	}
	for _, action := range s.runtime.Catalog() {
		sequences, state := draftSequences(s.draft, action)
		items = append(items, interaction.Item{Key: actionKey(action), Label: action.Label, Description: fmt.Sprintf("%s · %s · %s", action.Context, state, sequenceLabel(sequences)), Value: row{operation: "edit", action: action}})
	}
	effects := s.editor.replace(events, items)
	if s.view == editorScreen {
		effects = append(effects, s.resizeActive(events)...)
	}
	return effects
}

func (s *Surface) refreshEntry(events shell.EventContext) []shell.Effect {
	items := []interaction.Item{{Key: "record", Label: "Record a new sequence", Value: row{operation: "record", index: -1}}, {Key: "type", Label: "Type a new sequence", Description: "Use this to bind Enter or Escape", Value: row{operation: "type", index: -1}}}
	sequences, state := draftSequences(s.draft, s.selected)
	for index, sequence := range sequences {
		label := strings.Join(sequence, " ")
		for _, operation := range []struct{ id, label string }{{"replace-record", "Replace by recording"}, {"replace-type", "Replace by typing"}, {"remove", "Remove"}} {
			items = append(items, interaction.Item{Key: fmt.Sprintf("%s-%d", operation.id, index), Label: operation.label + ": " + label, Value: row{operation: operation.id, index: index}})
		}
	}
	items = append(items, commandRow("clear", "Clear all bindings"), commandRow("reset", "Restore defaults"), commandRow("keymap.checker", "Key checker"), commandRow("back", "Back to all shortcuts"))
	s.status = s.selected.Label + " · " + state
	if err := s.runtime.Validate(s.draft); err != nil {
		s.status = err.Error()
		items = append(items, commandRow("keymap.conflicts", "View conflict details"))
	}
	effects := s.entry.replace(events, items)
	return append(effects, s.resizeActive(events)...)
}

func (s *Surface) back(events shell.EventContext) []shell.Effect {
	s.actionsFocused = false
	switch s.view {
	case actionsScreen:
		s.view = s.menuOrigin
		s.actionsFocused = s.menuFocused
	case entryScreen:
		s.view = editorScreen
		return s.refreshEditor(events)
	case editorScreen:
		return s.closeEditor(events)
	case helpScreen:
		s.view = s.helpOrigin
	default:
		s.view = baseScreen
	}
	if !s.draftOpen {
		s.applyLatest()
	}
	return s.resizeActive(events)
}

func (s *Surface) closeEditor(events shell.EventContext) []shell.Effect {
	if s.saving {
		s.status = "Saving changes; please wait."
		return nil
	}
	s.view = s.editorOrigin
	s.draftOpen = false
	s.draft = keymap.Document{}
	s.captured = nil
	s.text.Set("")
	s.actionsFocused = false
	s.applyLatest()
	return s.resizeActive(events)
}
