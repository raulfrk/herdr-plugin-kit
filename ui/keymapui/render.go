package keymapui

import (
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/interaction"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

func (s *Surface) Render(context shell.RenderContext) (*view.Frame, error) {
	var frame *view.Frame
	var err error
	if picker := s.activePicker(); picker != nil {
		frame, err = picker.Render(context)
	} else if s.view == baseScreen {
		frame, err = s.base().Render(context)
	} else {
		frame, err = view.NewFrame(context.Layout.Render.Columns, context.Layout.Render.Rows)
	}
	if err != nil || frame == nil {
		return frame, err
	}
	w, h := context.Layout.Render.Columns, context.Layout.Render.Rows
	if w == 0 || h == 0 {
		return frame, nil
	}
	base := view.Style{Foreground: context.Theme.Text, Background: context.Theme.Background}
	muted := view.Style{Foreground: context.Theme.Muted, Background: context.Theme.PanelBackground}
	header := view.Style{Foreground: context.Theme.Accent, Background: context.Theme.PanelBackground, Bold: true}
	if s.view == checkerScreen || s.view == captureScreen || s.view == textScreen || s.view == helpScreen {
		frame.Fill(0, 0, w, h, base)
		title := "KEY CHECKER"
		var lines []string
		switch s.view {
		case checkerScreen:
			lines = []string{"Press a key or combination.", ""}
			if s.checkerPaste {
				lines = append(lines, "Paste received (contents hidden)")
			} else if len(s.checkerKeys) > 0 {
				latest := s.checkerKeys[len(s.checkerKeys)-1]
				lines = append(lines, "Received: "+latest, "Modifiers: "+modifiers(latest), "Recent: "+strings.Join(s.checkerKeys, " → "))
			}
			lines = append(lines, "", "Keys intercepted by your terminal or Herdr cannot be detected. Modifier-only presses may not arrive.")
		case captureScreen:
			title = "RECORD SHORTCUT"
			lines = []string{s.selected.Label, strings.Join(s.captured, " "), "Enter finishes; Escape cancels.", "Use typed entry to bind those keys."}
		case textScreen:
			title = "TYPE SHORTCUT"
			lines = []string{s.selected.Label, s.text.Text(), "Separate strokes with spaces: g r", "Names: enter, escape, ctrl+k, alt+d"}
		case helpScreen:
			title = "EFFECTIVE SHORTCUTS"
			all := view.Wrap(strings.Join(s.helpLines(), "\n"), max(0, w-2))
			offset := min(s.helpOffset, max(0, len(all)-max(1, h-3)))
			lines = all[offset:]
		}
		frame.Fill(0, 0, w, 1, header)
		put(frame, "keymap-title", 0, 0, w, 1, []string{title}, header, false)
		put(frame, "keymap-content", 1, 2, max(0, w-2), max(0, h-3), lines, base, true)
	}
	message := s.status
	if s.warning != "" {
		message = s.warning
	}
	if s.options.DefaultKeymap {
		message = "Recovery: default keymap for this run"
		if s.status != "" {
			message += " · " + s.status
		}
	}
	if message != "" && h > 2 {
		row := 1
		_, mainPicker := s.base().(*interaction.Picker)
		if s.activePicker() != nil || s.view == baseScreen && mainPicker {
			row = 2
		}
		frame.Fill(0, row, w, 1, muted)
		put(frame, "keymap-status", 0, row, w, 1, []string{message}, muted, false)
	}
	footer := "Ctrl-C quit · Actions " + s.bindingLabel("keymap.actions") + " · " + s.bindingLabel("keymap.focus-next") + " controls"
	if s.actionsFocused {
		footer = "[Actions] " + s.bindingLabel("keymap.actions") + " open · " + s.bindingLabel("keymap.focus-next") + " return"
	}
	if pending := s.runtime.Pending(); len(pending) > 0 {
		footer = strings.Join(pending, " ") + " → " + strings.Join(s.runtime.Continuations(), " / ")
	}
	if s.view == checkerScreen {
		footer = "Esc Esc within 1s return · Ctrl-C quit"
	}
	if s.view == captureScreen {
		footer = "Enter finish · Esc cancel · Ctrl-C quit"
	}
	frame.Fill(0, h-1, w, 1, muted)
	put(frame, "keymap-footer", 0, h-1, w, 1, []string{footer}, muted, false)
	return frame, nil
}

func modifiers(stroke string) string {
	var names []string
	for _, modifier := range []string{"ctrl", "alt", "shift"} {
		if strings.Contains(stroke, modifier+"+") {
			names = append(names, modifier)
		}
	}
	if len(names) == 0 {
		return "none reported"
	}
	return strings.Join(names, " + ")
}
func (s *Surface) bindingLabel(id string) string {
	sequences := s.runtime.Effective(s.KeymapContext().ID, id)
	if len(sequences) == 0 {
		return "unbound"
	}
	if s.actionsFocused && id == "keymap.actions" {
		return strings.Join(sequences[len(sequences)-1], " ")
	}
	return strings.Join(sequences[0], " ")
}
func (s *Surface) helpLines() []string {
	lines := []string{"Context: " + s.helpContext.ID, "Ctrl-C: Quit (fixed)"}
	if s.draftOpen {
		if err := s.runtime.Validate(s.draft); err != nil {
			lines = append([]string{"Draft conflict: " + err.Error(), ""}, lines...)
		}
	}
	for _, action := range s.runtime.Catalog() {
		if action.Context == s.helpContext.ID {
			lines = append(lines, action.Label+": "+sequenceLabel(s.runtime.Effective(action.Context, action.ID)))
		}
	}
	return lines
}
func put(frame *view.Frame, id string, x, y, width, height int, lines []string, style view.Style, wrap bool) {
	element, _ := diagnostics.NewID(id)
	mode := view.TextTruncate
	if wrap {
		mode = view.TextWrap
	}
	_ = frame.PutTextBox(view.TextBoxOptions{Element: element, X: x, Y: y, Width: max(0, width), Height: max(0, height), Mode: mode, AllowTruncation: true}, lines, style)
}
