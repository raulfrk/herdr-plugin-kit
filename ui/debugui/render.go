package debugui

import (
	"fmt"
	"strings"

	"github.com/rivo/uniseg"

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
	putLines(frame, 1, 0, size.Columns-2, 1, []string{"DEBUG · " + strings.ToUpper(surface.screen.String())}, header)
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
	if surface.screen == screenHelp {
		putLines(frame, 1, 1, 38, 9, helpLines(), base)
		return frame, nil
	}
	status := surface.status(context)
	putLines(frame, 1, 1, size.Columns-2, 1, []string{status}, muted)
	bottom := size.Rows - 1
	if bottom > 1 {
		putLines(frame, 1, bottom, size.Columns-2, 1, []string{surface.footer(size.Columns)}, muted)
	}
	if surface.projectionFailed {
		putLines(frame, 1, 3, size.Columns-2, bottom-3, []string{"! Debug data unavailable", "Previous safe projection retained.", "r retry"}, base)
		return frame, nil
	}

	filter := "Filter code: " + valueOrAll(surface.query.Code)
	if surface.editing {
		filter = "▌ Filter code: " + surface.draft + "▏"
	}
	putLines(frame, 1, 2, size.Columns-2, 1, []string{filter}, view.Style{Foreground: palette.Text, Background: palette.Background})
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
		lines := surface.detailRows(surface.selectedEvent(), size.Columns-2)
		surface.detailOffset = min(max(0, surface.detailOffset), max(0, len(lines)-contentHeight))
		end := min(len(lines), surface.detailOffset+contentHeight)
		putLines(frame, 1, contentTop, size.Columns-2, contentHeight, lines[surface.detailOffset:end], base)
	case screenHelp:
		putLines(frame, 1, contentTop, size.Columns-2, contentHeight, helpLines(), base)
	}
	return frame, nil
}

func (surface *Surface) status(context shell.RenderContext) string {
	if !surface.loaded {
		return "Loading debug snapshot…"
	}
	session := "all sessions"
	if !surface.query.Session.IsZero() {
		session = surface.query.Session.String()
	}
	settled := "settling"
	if context.Settled {
		settled = "settled"
	}
	mode := "Live"
	if surface.frozen {
		mode = "Frozen"
	}
	if surface.pending == projectionSelect {
		mode += "/filtering"
	}
	if surface.pending == projectionAcquire {
		mode += "/loading"
	}
	if surface.evicted {
		mode += "/anchor-evicted"
	}
	visibility := "debug hidden"
	if !surface.hideDebugger {
		visibility = "debug shown"
	}
	pressure := "Q:p0 ok0 "
	if surface.options.RecordingStatus != nil {
		status := surface.options.RecordingStatus()
		pressure = fmt.Sprintf("Q:p%d ok%d ", status.Pending, status.Persisted)
		if status.OverflowRejected != 0 || status.PersistenceFailed != 0 {
			pressure = fmt.Sprintf("Q! p%d ok%d r%d f%d ", status.Pending, status.Persisted, status.OverflowRejected, status.PersistenceFailed)
		}
	}
	return fmt.Sprintf("%s%s · %s · %s · %d events · page %d · %s · %dx%d g%d %s", pressure, mode, visibility, session, surface.page.Total, surface.page.Page+1, exportLabel(surface.export), context.Layout.Reported.Columns, context.Layout.Reported.Rows, context.ResizeGeneration, settled)
}

func (surface *Surface) footer(columns int) string {
	if columns < 70 {
		return "? all keys · h/t/g/r · p/v · s · e"
	}
	return "? help · h health · t timeline · g gallery · r HUD · p freeze · v debug · s/←→ session · e export"
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
	omitted := 0
	for index := range indices {
		indices[index] = index
	}
	if gallery {
		filtered := make([]diagnostics.DebugEvent, 0, len(events))
		filteredIndices := make([]int, 0, len(events))
		for index, event := range events {
			if event.Visual == nil {
				continue
			}
			if surface.view == nil || !surface.view.PreviewEligible(event.Sequence) {
				omitted++
				continue
			}
			filtered = append(filtered, event)
			filteredIndices = append(filteredIndices, index)
		}
		events = filtered
		indices = filteredIndices
		if omitted != 0 {
			frame.PutText(1, top, fit(fmt.Sprintf("! %d semantic previews omitted", omitted), frame.Width()-2), view.Style{Foreground: palette.Yellow, Background: palette.Background})
			top++
			height--
		}
	}
	if len(events) == 0 {
		if surface.displayedQuery == surface.query && surface.displayedList == surface.list {
			index := surface.anchorIndex()
			surface.visibleTop[index], surface.anchors[index].top = 0, 0
		}
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
	if len(events) > 0 && surface.displayedQuery == surface.query && surface.displayedList == surface.list {
		index := surface.anchorIndex()
		surface.visibleTop[index] = events[start].Sequence
		surface.anchors[index].top = events[start].Sequence
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
		putLines(frame, listWidth+2, top, frame.Width()-listWidth-3, height, surface.detailRows(surface.selectedEvent(), frame.Width()-listWidth-3), view.Style{Foreground: palette.Text, Background: palette.PanelBackground})
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

func (surface *Surface) detailRows(event *diagnostics.DebugEvent, width int) []string {
	lines := surface.detailLines(event)
	metadata := []string{"Text fit: unmeasured; other content unchecked."}
	if event != nil && event.TextFit != nil {
		report := event.TextFit
		metadata = []string{fmt.Sprintf("Text fit: %d measured, %d omitted; other content unchecked.", len(report.Observations), report.Omitted)}
		for _, observation := range report.Observations {
			outcome := "no measured loss"
			if observation.Clipped || observation.Truncated && !observation.AllowTruncation {
				outcome = "UNEXPECTED loss"
			} else if observation.Truncated {
				outcome = "intentional truncation"
			}
			metadata = append(metadata,
				fmt.Sprintf("%s[%d]: %s", observation.Element.String(), observation.Instance, outcome),
				fmt.Sprintf("original %d columns; layout %d rows; available %dx%d; intent %s", observation.OriginalColumns, observation.LayoutRows, observation.AvailableColumns, observation.AvailableRows, observation.Intent),
				fmt.Sprintf("wrapped %t; truncated %t; clipped %t; allow truncation %t", observation.Wrapped, observation.Truncated, observation.Clipped, observation.AllowTruncation))
		}
	}
	// Only the new metadata wraps; preserve the existing event presentation.
	// Detail scrolling makes every metadata row reachable, so offscreen rows
	// are not counted as clipped content in the current frame.
	for _, line := range metadata {
		row, columns := "", 0
		graphemes := uniseg.NewGraphemes(line)
		for graphemes.Next() {
			part := graphemes.Str()
			cells := uniseg.StringWidth(part)
			if columns+cells > width && row != "" {
				lines = append(lines, row)
				row, columns = "", 0
			}
			row += part
			columns += cells
		}
		lines = append(lines, row)
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
		"All controls",
		"h/t/g views · r HUD",
		"p/Space Live/Frozen · v debug",
		"s/Left/Right session · Back/Esc back",
		"Up/Down/Home/End/PgUp/PgDn · Enter",
		"l level · k kind · 0 clear filters",
		"/ code · Enter apply · Esc cancel",
		"1 comp · 2 action · 3 code · 4/c corr",
		"e export · q/Ctrl-C quit",
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
	// Measure complete blocks before viewport limits, then retain the established
	// three-dot presentation (PutTextBox uses a single-cell ellipsis).
	_ = frame.PutTextBox(view.TextBoxOptions{Element: id("debug.content"), Instance: max(0, y), X: x, Y: y, Width: max(0, width), Height: max(0, height), Mode: view.TextTruncate}, lines, style)
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
