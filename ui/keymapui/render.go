package keymapui

import (
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/interaction"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
	"github.com/rivo/uniseg"
)

func (s *Surface) Render(context shell.RenderContext) (*view.Frame, error) {
	var frame *view.Frame
	var err error
	childContext := context
	childContext.ReservedBottomRows = min(max(0, context.ReservedBottomRows)+1, context.Layout.Render.Rows)
	if picker := s.activePicker(); picker != nil {
		frame, err = picker.Render(childContext)
	} else if s.view == baseScreen {
		frame, err = s.base().Render(childContext)
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
	customHelp := s.view == helpScreen && s.options.HelpOverview != nil
	if customHelp {
		frame.Fill(0, 0, w, h, base)
		s.renderHelpOverview(frame, context, w, h)
	}
	if !customHelp && (s.view == checkerScreen || s.view == captureScreen || s.view == textScreen || s.view == helpScreen) {
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
			if s.options.InteractiveHelp {
				all := s.helpLines()
				visible := max(1, h-3)
				selectedLine := 0
				for index, line := range all {
					if strings.HasPrefix(line, "> ") {
						selectedLine = index
						break
					}
				}
				offset := max(0, selectedLine-visible+1)
				lines = all[offset:min(len(all), offset+visible)]
			} else {
				all := view.Wrap(strings.Join(s.helpLines(), "\n"), max(0, w-2))
				offset := min(s.helpOffset, max(0, len(all)-max(1, h-3)))
				lines = all[offset:]
			}
		}
		frame.Fill(0, 0, w, 1, header)
		put(frame, "keymap-title", 0, 0, w, 1, []string{title}, header, false)
		put(frame, "keymap-content", 1, 2, max(0, w-2), max(0, h-3), lines, base, !(s.view == helpScreen && s.options.InteractiveHelp))
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
	compactFooter := "Ctrl-C quit · " + s.bindingLabel("keymap.actions") + " Actions · " + s.bindingLabel("keymap.focus-next") + " nav"
	if s.childOwnsTab(s.contentContext()) && !s.actionsFocused {
		footer = "Ctrl-C quit · Actions " + s.bindingLabel("keymap.actions")
		compactFooter = footer
	}
	if s.actionsFocused {
		footer = "[Actions] " + s.bindingLabel("keymap.actions") + " open · " + s.bindingLabel("keymap.focus-next") + " return"
		compactFooter = "[Actions] " + s.bindingLabel("keymap.actions") + " · " + s.bindingLabel("keymap.focus-next") + " return"
	}
	if pending := s.runtime.Pending(); len(pending) > 0 {
		footer = strings.Join(pending, " ") + " → " + strings.Join(s.runtime.Continuations(), " / ")
		compactFooter = strings.Join(pending, " ") + " → " + strings.Join(s.runtime.Continuations(), "/")
	}
	if s.view == checkerScreen {
		footer = "Esc Esc within 1s return · Ctrl-C quit"
		compactFooter = footer
	}
	if s.view == captureScreen {
		footer = "Enter finish · Esc cancel · Ctrl-C quit"
		compactFooter = footer
	}
	if s.view == helpScreen && s.options.InteractiveHelp {
		parts := []string{"Enter edit"}
		for _, command := range s.options.HelpCommands {
			parts = append(parts, sequenceLabel(s.runtime.Effective("keymap.help", helpCommandID(command)))+" "+command.Label)
		}
		footer = strings.Join(parts, " · ")
		compactFooter = "Enter edit · Help commands active"
	}
	footer = fittingFooter(w, footer, compactFooter)
	frame.Fill(0, h-1, w, 1, muted)
	put(frame, "keymap-footer", 0, h-1, w, 1, []string{footer}, muted, false)
	return frame, nil
}

func (s *Surface) renderHelpOverview(frame *view.Frame, context shell.RenderContext, width, height int) {
	overview := s.options.HelpOverview
	if overview == nil || width <= 0 || height <= 0 {
		return
	}
	header := view.Style{Foreground: context.Theme.Accent, Background: context.Theme.PanelBackground, Bold: true}
	frame.Fill(0, 0, width, min(2, height), header)
	status := HelpOverviewStatus{}
	if overview.Status != nil {
		status = overview.Status(width)
	}
	status.Text = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(status.Text))
	statusStyle := view.Style{Foreground: context.Theme.Muted, Background: context.Theme.PanelBackground, Bold: true}
	switch status.Tone {
	case HelpStatusSuccess:
		statusStyle.Foreground = context.Theme.Green
	case HelpStatusWarning:
		statusStyle.Foreground = context.Theme.Yellow
	case HelpStatusError:
		statusStyle.Foreground = context.Theme.Red
	}
	statusWidth := min(width, uniseg.StringWidth(status.Text))
	put(frame, "keymap-overview-title", 0, 0, max(0, width-statusWidth-1), 1, []string{overview.Title}, header, false)
	if statusWidth > 0 {
		put(frame, "keymap-overview-status", width-statusWidth, 0, statusWidth, 1, []string{status.Text}, statusStyle, false)
	}
	if height > 1 {
		navigation := view.Style{Foreground: context.Theme.Muted, Background: context.Theme.PanelBackground}
		current := navigation
		current.Foreground = context.Theme.Accent
		put(frame, "keymap-overview-previous", 1, 1, 1, 1, []string{"‹"}, navigation, false)
		currentWidth := uniseg.StringWidth(overview.Current)
		currentX := max(3, (width-currentWidth)/2)
		put(frame, "keymap-overview-current", currentX, 1, currentWidth, 1, []string{overview.Current}, current, false)
		menuWidth := uniseg.StringWidth(overview.ViewMenu)
		if menuWidth > 0 {
			put(frame, "keymap-overview-next", max(currentX+currentWidth+1, width-menuWidth-3), 1, 1, 1, []string{"›"}, navigation, false)
			put(frame, "keymap-overview-menu", max(0, width-menuWidth), 1, menuWidth, 1, []string{overview.ViewMenu}, navigation, false)
		}
	}
	visible := max(1, height-3)
	selected := min(max(0, s.helpSelected), len(overview.Rows)-1)
	start := max(0, selected-visible+1)
	start = min(start, max(0, len(overview.Rows)-visible))
	labelWidth := min(22, max(12, width*38/100))
	keyWidth := min(16, max(8, width*22/100))
	for offset, index := 0, start; offset < visible && index < len(overview.Rows); offset, index = offset+1, index+1 {
		y := 2 + offset
		row := overview.Rows[index]
		rowStyle := view.Style{Foreground: context.Theme.Text, Background: context.Theme.Background}
		marker := " "
		if index == selected {
			marker = "▏"
			rowStyle.Background, rowStyle.Bold = context.Theme.ActiveRowBackground, true
			frame.Fill(0, y, width, 1, rowStyle)
		}
		put(frame, "keymap-overview-marker", 0, y, 1, 1, []string{marker}, view.Style{Foreground: context.Theme.Accent, Background: rowStyle.Background, Bold: rowStyle.Bold}, false)
		put(frame, "keymap-overview-label", 2, y, max(0, labelWidth-1), 1, []string{row.Label}, rowStyle, false)
		keyX := labelWidth + 1
		keyStyle := view.Style{Foreground: context.Theme.Text, Background: context.Theme.Surface}
		if index == selected {
			keyStyle.Foreground, keyStyle.Background, keyStyle.Bold = context.Theme.Accent, context.Theme.SelectionBackground, true
		}
		frame.Fill(keyX, y, min(keyWidth, max(0, width-keyX)), 1, keyStyle)
		sequences := sequenceLabel(s.runtime.Effective(row.Context, row.Action))
		put(frame, "keymap-overview-binding", keyX+1, y, max(0, min(keyWidth-2, width-keyX-1)), 1, []string{sequences}, keyStyle, false)
		summaryX := keyX + keyWidth + 1
		put(frame, "keymap-overview-summary", summaryX, y, max(0, width-summaryX), 1, []string{row.Summary}, view.Style{Foreground: context.Theme.Muted, Background: rowStyle.Background}, false)
	}
}

func fittingFooter(width int, full, compact string) string {
	if len([]rune(full)) <= width {
		return full
	}
	if len([]rune(compact)) <= width {
		return compact
	}
	const fallback = "Keys active · Ctrl-C quit"
	if len([]rune(fallback)) <= width {
		return fallback
	}
	return ""
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
	actions := s.helpActions()
	for index, action := range actions {
		prefix := ""
		if s.options.InteractiveHelp {
			prefix = "  "
			if index == min(max(0, s.helpSelected), max(0, len(actions)-1)) {
				prefix = "> "
			}
		}
		lines = append(lines, prefix+action.Label+": "+sequenceLabel(s.runtime.Effective(action.Context, action.ID)))
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
