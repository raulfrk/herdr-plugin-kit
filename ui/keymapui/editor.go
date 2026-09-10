package keymapui

import (
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
)

func actionKey(action keymap.Action) string { return action.Context + "/" + action.ID }

func draftSequences(document keymap.Document, action keymap.Action) ([]keymap.Sequence, string) {
	sequences, state := action.Defaults, "Default"
	for _, binding := range document.Bindings {
		if binding.Context == action.Context && binding.Action == action.ID {
			sequences, state = binding.Sequences, "Custom"
			break
		}
	}
	if len(sequences) == 0 {
		state = "Unbound"
	}
	return sequences, state
}

func setBinding(document *keymap.Document, action keymap.Action, sequences []keymap.Sequence) {
	for index, binding := range document.Bindings {
		if binding.Context == action.Context && binding.Action == action.ID {
			document.Bindings[index].Sequences = sequences
			return
		}
	}
	document.Bindings = append(document.Bindings, keymap.Binding{Context: action.Context, Action: action.ID, Sequences: sequences})
}

func resetBinding(document *keymap.Document, action keymap.Action) {
	for index, binding := range document.Bindings {
		if binding.Context == action.Context && binding.Action == action.ID {
			document.Bindings = append(document.Bindings[:index], document.Bindings[index+1:]...)
			return
		}
	}
}

func sequenceLabel(sequences []keymap.Sequence) string {
	if len(sequences) == 0 {
		return "Unbound"
	}
	labels := make([]string, len(sequences))
	for i, sequence := range sequences {
		labels[i] = strings.Join(sequence, " ")
	}
	return strings.Join(labels, " / ")
}
