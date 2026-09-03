package debugui

import (
	"fmt"
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

func (surface *Surface) render(context shell.RenderContext) (*view.Frame, error) {
	size, palette := context.Layout.Render, context.Theme
	frame, err := view.NewFrame(size.Columns, size.Rows)
	if err != nil {
		return nil, err
	}
	base := view.Style{Foreground: palette.Text, Background: palette.Background}
	muted := view.Style{Foreground: palette.Muted, Background: palette.Background}
	header := view.Style{Foreground: palette.Accent, Background: palette.PanelBackground, Bold: true}
	frame.Fill(0, 0, size.Columns, size.Rows, base)
	frame.Fill(0, 0, size.Columns, 1, header)
	frame.PutText(1, 0, fit("DEBUG · "+strings.ToUpper(surface.screen.String()), size.Columns-2), header)
	if context.Layout.Class == responsive.Recovery {
		putLines(frame, 1, 2, size.Columns-2, max(0, size.Rows-2), []string{
			"Terminal is too small",
			fmt.Sprintf("Need 40×10 · received %d×%d", context.Layout.Reported.Columns, context.Layout.Reported.Rows),
			"Resize to continue; debug state is preserved.",
		}, base)
		return frame, nil
	}
	if size.Rows < 2 {
		return frame, nil
	}
	status := surface.status(context)
	frame.PutText(1, 1, fit(status, size.Columns-2), muted)
	bottom := size.Rows - 1
	if bottom > 1 {
		frame.PutText(1, bottom, fit(surface.footer(size.Columns), size.Columns-2), muted)
	}
	if surface.projectionFailed {
		putLines(frame, 1, 3, size.Columns-2, bottom-3, []string{"! Debug data unavailable", "Previous safe projection retained.", "r retry"}, base)
		return frame, nil
	}

	filter := "Filter code: " + valueOrAll(surface.query.Code)
	if surface.editing {
		filter = "▌ Filter code: " + surface.draft + "▏"
	}
	frame.PutText(1, 2, fit(filter, size.Columns-2), view.Style{Foreground: palette.Text, Background: palette.Background})
	contentTop, contentHeight := 3, max(0, bottom-3)
	switch surface.screen {
	case screenHealth:
		putLines(frame, 1, contentTop, size.Columns-2, contentHeight, surface.healthLines(), base)
	case screenTimeline:
		surface.renderEvents(frame, palette, contentTop, contentHeight, false, context.Layout.Class != responsive.Compact && size.Rows > 18)
	case screenGallery:
		surface.renderEvents(frame, palette, contentTop, contentHeight, true, context.Layout.Class != responsive.Compact && size.Rows > 18)
	case screenHUD:
		putLines(frame, 1, contentTop, size.Columns-2, contentHeight, surface.hudLines(context), base)
	case screenDetail:
		putLines(frame, 1, contentTop, size.Columns-2, contentHeight, surface.detailLines(surface.selectedEvent()), base)
	case screenHelp:
		putLines(frame, 1, contentTop, size.Columns-2, contentHeight, helpLines(), base)
	}
	return frame, nil
}

func (surface *Surface) status(context shell.RenderContext) string {
	session := "all sessions"
	if !surface.query.Session.IsZero() {
		session = surface.query.Session.String()
	}
	settled := "settling"
	if context.Settled {
		settled = "settled"
	}
	return fmt.Sprintf("%s · %d events · page %d · %s · %dx%d g%d %s", session, surface.page.Total, surface.query.Page+1, exportLabel(surface.export), context.Layout.Reported.Columns, context.Layout.Reported.Rows, context.ResizeGeneration, settled)
}

func (surface *Surface) footer(columns int) string {
	if columns < 70 {
		return "? help · h/t/g/r view · s session · e export"
	}
	return "? help · h health · t timeline · g gallery · r HUD · s/←→ session · e export"
}

func (surface *Surface) healthLines() []string {
	health := surface.page.Health
	writable := "OK writable"
	if !health.Writable {
		writable = "! unavailable"
	}
	pressure := "OK normal"
	if health.Pressure {
		pressure = "! pressure"
	}
	return []string{
		"Recorder health", writable, pressure,
		fmt.Sprintf("Usage %.1f%% · %d/%d bytes", health.UsageRatio*100, health.Bytes, health.MaxBytes),
		fmt.Sprintf("Events %d/%d · dropped %d · recovered %d", health.Events, health.MaxEvents, health.Dropped, health.CorruptRecords),
		"Export " + exportLabel(surface.export),
	}
}

func (surface *Surface) renderEvents(frame *view.Frame, palette theme.Palette, top, height int, gallery, split bool) {
	events := surface.page.Events
	indices := make([]int, len(events))
	for index := range indices {
		indices[index] = index
	}
	if gallery {
		filtered := make([]diagnostics.DebugEvent, 0, len(events))
		filteredIndices := make([]int, 0, len(events))
		for index, event := range events {
			if event.Visual != nil && surface.options.Previews != nil {
				if _, ok := surface.options.Previews.Entry(event.Sequence, *event.Visual); !ok {
					continue
				}
				filtered = append(filtered, event)
				filteredIndices = append(filteredIndices, index)
			}
		}
		events = filtered
		indices = filteredIndices
	}
	if len(events) == 0 {
		frame.PutText(1, top+1, "No matching safe events · 0 clear filters", view.Style{Foreground: palette.Text, Background: palette.Background})
		return
	}
	listWidth := frame.Width() - 2
	if split {
		listWidth = max(20, (frame.Width()-3)*2/3)
	}
	rows := min(height, len(events))
	selectedPosition := 0
	for position, index := range indices {
		if index == surface.selected {
			selectedPosition = position
			break
		}
	}
	start := 0
	if rows > 0 && selectedPosition >= rows {
		start = selectedPosition - rows + 1
	}
	for row := range rows {
		position := start + row
		event := events[position]
		style := view.Style{Foreground: palette.Text, Background: palette.Background}
		prefix := "  "
		if indices[position] == surface.selected {
			prefix = "▌ "
			style.Background, style.Bold = palette.SelectionBackground, true
			frame.Fill(0, top+row, listWidth+1, 1, style)
		}
		line := fmt.Sprintf("%s%06d %-5s %s", prefix, event.Sequence, event.Level, event.Code.String())
		frame.PutText(0, top+row, fit(line, listWidth), style)
	}
	if split {
		putLines(frame, listWidth+2, top, frame.Width()-listWidth-3, height, surface.detailLines(surface.selectedEvent()), view.Style{Foreground: palette.Text, Background: palette.PanelBackground})
	}
}

func (surface *Surface) detailLines(event *diagnostics.DebugEvent) []string {
	if event == nil {
		return []string{"No event selected"}
	}
	lines := []string{
		"Event " + event.Code.String(),
		fmt.Sprintf("sequence %d · %s · %s", event.Sequence, event.Level, event.Kind),
		"session " + valueOrDash(event.Session), "component " + valueOrDash(event.Component),
		"action " + valueOrDash(event.Action), "correlation " + valueOrDash(event.Correlation),
		fmt.Sprintf("outcome %s · generation %d/%d", valueOrDashOutcome(event.Outcome), event.Generation, event.RelatedGeneration),
		fmt.Sprintf("geometry %dx%d→%dx%d", event.Geometry.ReportedColumns, event.Geometry.ReportedRows, event.Geometry.RenderColumns, event.Geometry.RenderRows),
		"Payload and raw snapshot were never projected.",
	}
	if event.Visual != nil {
		lines = append(lines,
			"Semantic visual", "screen "+valueOrDash(event.Visual.Screen)+" · focus "+valueOrDash(event.Visual.Focus),
			"selection "+valueOrDash(event.Visual.Selection)+" · state "+valueOrDash(event.Visual.State),
			fmt.Sprintf("items %d · selected %d · pending %t · error %t", event.Visual.ItemCount, event.Visual.SelectedIndex, event.Visual.Pending, event.Visual.HasError),
		)
	}
	return lines
}

func (surface *Surface) hudLines(context shell.RenderContext) []string {
	lines := []string{fmt.Sprintf("Geometry HUD · reported %dx%d · render %dx%d · %s", context.Layout.Reported.Columns, context.Layout.Reported.Rows, context.Layout.Render.Columns, context.Layout.Render.Rows, context.Layout.Class)}
	if context.Layout.Projected {
		lines = append(lines, "! Oversized viewport projected to 500x200 maximum")
	}
	lines = append(lines, "Resize flight recorder · newest last")
	for _, record := range surface.resizes {
		lines = append(lines, fmt.Sprintf("g%d %dx%d→%dx%d %s", record.Generation, record.Layout.Reported.Columns, record.Layout.Reported.Rows, record.Layout.Render.Columns, record.Layout.Render.Rows, record.Layout.Class))
	}
	return lines
}

func helpLines() []string {
	return []string{
		"DEBUG HELP", "h health · t timeline · g semantic gallery · r geometry HUD",
		"s or Left/Right select current-recorder session", "Up/Down/Home/End select · Enter drill in",
		"PageUp/PageDown page · Backspace/Escape back", "l level · k kind · 0 clear all filters",
		"/ edit event-code filter · Enter apply · Escape cancel", "1 component · 2 action · 3 event code · 4 or c correlation",
		"e export sanitized bounded report · q/Ctrl-C quit",
	}
}

func exportLabel(status exportStatus) string {
	return []string{"ready", "* exporting", "OK exported", "! failed", "disabled"}[status]
}

func valueOrDash(value diagnostics.ID) string {
	if value.IsZero() {
		return "-"
	}
	return value.String()
}

func valueOrAll(value diagnostics.ID) string {
	if value.IsZero() {
		return "all"
	}
	return value.String()
}

func valueOrDashOutcome(value diagnostics.OutcomeCode) string {
	if value == "" {
		return "-"
	}
	return string(value)
}

func putLines(frame *view.Frame, x, y, width, height int, lines []string, style view.Style) {
	if width <= 0 || height <= 0 {
		return
	}
	for row := range min(height, len(lines)) {
		frame.Fill(x, y+row, width, 1, style)
		frame.PutText(x, y+row, fit(lines[row], width), style)
	}
}

func fit(value string, width int) string {
	return view.Truncate(value, width, "...")
}
