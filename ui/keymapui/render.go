package keymapui

import (
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/interaction"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
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
		if overview := s.options.HelpOverview; overview != nil && overview.composed() && overview.Footer != "" {
			footer = overview.Footer
			compactFooter = overview.CompactFooter
			if compactFooter == "" {
				compactFooter = footer
			}
		}
	}
	footer = fittingFooter(w, footer, compactFooter)
	frame.Fill(0, h-1, w, 1, muted)
	put(frame, "keymap-footer", 0, h-1, w, 1, []string{footer}, muted, false)
	if s.options.CompactNormalWeight && w < 64 {
		frame.ClearBold()
	}
	return frame, nil
}

func (s *Surface) renderHelpOverview(frame *view.Frame, context shell.RenderContext, width, height int) {
	overview := s.options.HelpOverview
	if overview == nil || width <= 0 || height <= 0 {
		return
	}
	if overview.composed() {
		s.renderComposedHelpOverview(frame, context, width, height)
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

func (s *Surface) renderComposedHelpOverview(frame *view.Frame, context shell.RenderContext, width, height int) {
	overview := s.options.HelpOverview
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
		if width >= 100 && len(overview.Views) != 0 {
			s.renderHelpOverviewViews(frame, context, width)
		} else {
			navigation := view.Style{Foreground: context.Theme.Muted, Background: context.Theme.PanelBackground}
			current := navigation
			current.Foreground, current.Bold = context.Theme.Accent, true
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
	}

	layout := s.composedHelpLayout(width, height)
	if overview.Description != "" && height > 2 {
		description := view.Style{Foreground: context.Theme.Muted, Background: context.Theme.Background}
		put(frame, "keymap-overview-description", 1, 2, max(0, width-2), 1, []string{overview.Description}, description, false)
	}
	if layout.asideWidth > 0 {
		s.renderHelpOverviewAside(frame, context, layout.asideX, layout.contentTop, layout.asideWidth, height-layout.contentTop-1)
	}
	for offset, index := 0, layout.start; offset < layout.visible && index < len(overview.Rows); offset, index = offset+1, index+1 {
		y := layout.contentTop + offset
		row := overview.Rows[index]
		rowStyle := view.Style{Foreground: context.Theme.Text, Background: context.Theme.Background}
		marker := " "
		if index == layout.selected {
			marker = "▏"
			rowStyle.Background, rowStyle.Bold = context.Theme.ActiveRowBackground, true
			frame.Fill(0, y, layout.mainWidth, 1, rowStyle)
		}
		put(frame, "keymap-overview-marker", 0, y, 1, 1, []string{marker}, view.Style{Foreground: context.Theme.Accent, Background: rowStyle.Background, Bold: rowStyle.Bold}, false)
		put(frame, "keymap-overview-label", 2, y, max(0, layout.keyX-3), 1, []string{row.Label}, rowStyle, false)
		keyStyle := view.Style{Foreground: context.Theme.Text, Background: context.Theme.Surface}
		if index == layout.selected {
			keyStyle.Foreground, keyStyle.Background, keyStyle.Bold = context.Theme.Accent, context.Theme.SelectionBackground, true
		}
		frame.Fill(layout.keyX, y, min(layout.keyWidth, max(0, layout.mainWidth-layout.keyX)), 1, keyStyle)
		sequences := sequenceLabel(s.helpOverviewEffective(row.Context, row.Action))
		put(frame, "keymap-overview-binding", layout.keyX+1, y, max(0, min(layout.keyWidth-2, layout.mainWidth-layout.keyX-1)), 1, []string{sequences}, keyStyle, false)
		if layout.showSummary {
			summaryX := layout.keyX + layout.keyWidth + 1
			put(frame, "keymap-overview-summary", summaryX, y, max(0, layout.mainWidth-summaryX), 1, []string{row.Summary}, view.Style{Foreground: context.Theme.Muted, Background: rowStyle.Background}, false)
		}
	}
}

type helpCompositionLayout struct {
	mainWidth, asideX, asideWidth int
	contentTop, visible           int
	selected, start               int
	keyX, keyWidth                int
	showSummary                   bool
}

func (s *Surface) composedHelpLayout(width, height int) helpCompositionLayout {
	overview := s.options.HelpOverview
	layout := helpCompositionLayout{mainWidth: width, contentTop: 2}
	if overview.Aside != nil && width >= 100 {
		layout.asideWidth = min(30, max(24, width*28/100))
		layout.asideX = width - layout.asideWidth
		layout.mainWidth = max(1, layout.asideX-1)
	}
	if overview.Description != "" && height > layout.contentTop {
		layout.contentTop++
	}
	layout.visible = max(1, height-layout.contentTop-1)
	layout.selected = min(max(0, s.helpSelected), len(overview.Rows)-1)
	layout.start = max(0, layout.selected-layout.visible+1)
	layout.start = min(layout.start, max(0, len(overview.Rows)-layout.visible))
	layout.keyX = min(max(3, layout.mainWidth*56/100), max(3, layout.mainWidth-1))
	layout.showSummary = layout.mainWidth >= 64
	layout.keyWidth = max(0, layout.mainWidth-layout.keyX)
	if layout.showSummary {
		layout.keyWidth = 5
		for _, row := range overview.Rows {
			layout.keyWidth = max(layout.keyWidth, uniseg.StringWidth(sequenceLabel(s.helpOverviewEffective(row.Context, row.Action)))+2)
		}
		layout.keyWidth = min(14, layout.keyWidth)
	}
	return layout
}

func (s *Surface) renderHelpOverviewViews(frame *view.Frame, context shell.RenderContext, width int) {
	inactive := view.Style{Foreground: context.Theme.Muted, Background: context.Theme.PanelBackground}
	active := view.Style{Foreground: context.Theme.Accent, Background: context.Theme.SelectionBackground, Bold: true}
	frame.Fill(0, 1, width, 1, inactive)
	for _, item := range s.helpOverviewViewPlacements(width) {
		style := inactive
		if item.current {
			style = active
		}
		frame.Fill(item.x, 1, item.width, 1, style)
		put(frame, "keymap-overview-view", item.x, 1, item.width, 1, []string{item.label}, style, false)
	}
}

type helpOverviewViewPlacement struct {
	x, width int
	label    string
	current  bool
}

func (s *Surface) helpOverviewViewPlacements(width int) []helpOverviewViewPlacement {
	x := 1
	var placements []helpOverviewViewPlacement
	for _, item := range s.options.HelpOverview.Views {
		binding := sequenceLabel(s.helpOverviewEffective(item.Context, item.Action))
		if item.Current {
			binding = sequenceLabel(s.runtime.Effective(s.helpContext.ID, "keymap.help"))
		}
		label := " " + binding + " " + item.Label + " "
		itemWidth := uniseg.StringWidth(label)
		if itemWidth == 0 || x+itemWidth > width {
			break
		}
		placements = append(placements, helpOverviewViewPlacement{x: x, width: itemWidth, label: label, current: item.Current})
		x += itemWidth
	}
	return placements
}

func (s *Surface) renderHelpOverviewAside(frame *view.Frame, context shell.RenderContext, x, y, width, height int) {
	if width <= 1 || height <= 0 {
		return
	}
	border := view.Style{Foreground: context.Theme.Border, Background: context.Theme.Background}
	borderLines := make([]string, height)
	for index := range borderLines {
		borderLines[index] = "│"
	}
	put(frame, "keymap-overview-aside-border", x-1, y, 1, height, borderLines, border, false)
	aside := s.options.HelpOverview.Aside
	header := view.Style{Foreground: context.Theme.Text, Background: context.Theme.Background, Bold: true}
	put(frame, "keymap-overview-aside-title", x+1, y, width-2, 1, []string{aside.Title}, header, false)
	row := y + 2
	for _, step := range aside.Steps {
		if row >= y+height {
			return
		}
		stepStyle := view.Style{Foreground: context.Theme.Text, Background: context.Theme.Surface}
		frame.Fill(x+1, row, width-2, 1, stepStyle)
		put(frame, "keymap-overview-aside-step-label", x+2, row, 2, 1, []string{step.Label}, view.Style{Foreground: context.Theme.Accent, Background: stepStyle.Background, Bold: true}, false)
		put(frame, "keymap-overview-aside-step-text", x+4, row, width-5, 1, []string{step.Text}, stepStyle, false)
		row += 2
	}
	row++
	for _, detail := range aside.Details {
		if row >= y+height {
			return
		}
		labelWidth, textWidth, textHeight := helpAsideDetailSize(width, y+height-row, detail.Text)
		put(frame, "keymap-overview-aside-detail-label", x+1, row, labelWidth, 1, []string{detail.Label}, view.Style{Foreground: context.Theme.Muted, Background: context.Theme.Background}, false)
		put(frame, "keymap-overview-aside-detail-text", x+2+labelWidth, row, textWidth, textHeight, []string{detail.Text}, view.Style{Foreground: context.Theme.Text, Background: context.Theme.Background}, true)
		row += max(1, textHeight)
	}
}

func helpAsideDetailSize(width, remaining int, text string) (labelWidth, textWidth, textHeight int) {
	labelWidth = min(10, max(4, width/3))
	textWidth = width - labelWidth - 3
	textHeight = min(len(view.Wrap(text, textWidth)), remaining)
	return labelWidth, textWidth, textHeight
}

func (s *Surface) helpOverviewEffective(context, action string) []keymap.Sequence {
	if action == "picker.help" || action == "debug.help" {
		action = "keymap.help"
	}
	return s.runtime.Effective(context, action)
}

func fittingFooter(width int, full, compact string) string {
	if uniseg.StringWidth(full) <= width {
		return full
	}
	if uniseg.StringWidth(compact) <= width {
		return compact
	}
	const fallback = "Keys active · Ctrl-C quit"
	if uniseg.StringWidth(fallback) <= width {
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
