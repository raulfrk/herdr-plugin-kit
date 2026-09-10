package shell

import (
	"time"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
)

type ActionSurface interface {
	KeymapCatalog() []keymap.Action
	KeymapContext() keymap.Context
	ActionState(string) keymap.State
}

// CapturedKey carries normalized input only to the active in-memory capture UI.
// Paste has no content. Capture does not pass through action resolution.
type CapturedKey struct {
	Stroke string
	Paste  bool
}
type KeyCapture interface {
	CapturingKeys() bool
	CaptureKey(EventContext, CapturedKey) []Effect
}

var prefixTimer, _ = NewEventCode("keymap.prefix")

func (model *model) dispatchKeymap(events EventContext, event Event, now time.Time) []Effect {
	runtime := model.options.Keymap
	surface := model.surface.(ActionSurface)
	runtime.Sync(surface.KeymapContext(), now)
	defer func() { runtime.Sync(surface.KeymapContext(), now) }()
	var strokes []string
	var text *TextEvent
	switch input := event.(type) {
	case FocusEvent:
		if !input.Focused {
			runtime.Cancel()
		}
	case TimerEvent:
		if input.Code == prefixTimer {
			return nil
		}
		return model.surface.Update(events, event)
	case TextEvent:
		text = &input
		if input.Paste {
			runtime.Cancel()
			if capture, ok := model.surface.(KeyCapture); ok && capture.CapturingKeys() {
				return capture.CaptureKey(events, CapturedKey{Paste: true})
			}
			if surface.KeymapContext().Editing {
				return model.surface.Update(events, TextEvent{Text: input.Text, Paste: true})
			}
			return nil
		}
		for _, r := range input.Text {
			value := string(r)
			if input.Alt {
				value = "alt+" + value
			}
			if normalized, err := keymap.Normalize(value); err == nil {
				strokes = append(strokes, normalized)
			}
		}
	case KeyEvent:
		if (input.Code == KeyCtrlC || input.Code == "ctrl+c") && !input.Alt {
			runtime.Cancel()
			return []Effect{Quit()}
		}
		value := string(input.Code)
		if input.Code == KeyCtrlC {
			value = "ctrl+c"
		}
		if input.Alt {
			value = "alt+" + value
		}
		if normalized, err := keymap.Normalize(value); err == nil {
			strokes = []string{normalized}
		}
	default:
		effects := model.surface.Update(events, event)
		runtime.Sync(surface.KeymapContext(), now)
		return effects
	}
	if _, ok := event.(FocusEvent); ok {
		return model.surface.Update(events, event)
	}
	if capture, ok := model.surface.(KeyCapture); ok && capture.CapturingKeys() {
		runtime.Cancel()
		var effects []Effect
		for _, stroke := range strokes {
			if !capture.CapturingKeys() {
				break
			}
			effects = append(effects, capture.CaptureKey(events, CapturedKey{Stroke: stroke})...)
		}
		return effects
	}
	if text != nil && !text.Alt && surface.KeymapContext().Editing && len(runtime.Pending()) == 0 {
		return model.surface.Update(events, *text)
	}
	var effects []Effect
	for index, stroke := range strokes {
		if capture, ok := model.surface.(KeyCapture); ok && capture.CapturingKeys() {
			runtime.Cancel()
			for _, remaining := range strokes[index:] {
				if !capture.CapturingKeys() {
					break
				}
				effects = append(effects, capture.CaptureKey(events, CapturedKey{Stroke: remaining})...)
			}
			return effects
		}
		context := surface.KeymapContext()
		// Preserve the rest of a rune batch once an action enters text entry.
		if text != nil && !text.Alt && context.Editing && len(runtime.Pending()) == 0 {
			runes := []rune(text.Text)
			effects = append(effects, model.surface.Update(events, TextEvent{Text: string(runes[index:])})...)
			break
		}
		action, consumed := runtime.Step(context, stroke, now)
		if action != "" && surface.ActionState(action).Enabled {
			before := model.surface.DiagnosticState()
			invocation := ActionEvent{ID: action}
			result := model.surface.Update(events, invocation)
			effects = append(effects, result...)
			model.record("action.invoked", diagnostics.OutcomeApplied, before, model.surface.DiagnosticState(), invocation, 0)
			runtime.Sync(surface.KeymapContext(), now)
			for _, effect := range result {
				if effect.kind == effectQuit {
					return effects
				}
			}
		} else if !consumed && context.Editing {
			value := stroke
			if value == "space" {
				value = " "
			}
			effects = append(effects, model.surface.Update(events, TextEvent{Text: value})...)
		}
	}
	if deadline := runtime.Deadline(); !deadline.IsZero() {
		if effect, err := events.After(deadline.Sub(now), prefixTimer); err == nil {
			effects = append(effects, effect)
		}
	}
	return effects
}
