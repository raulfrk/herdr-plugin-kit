package form

import (
	"fmt"
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

func (model *Model) render(context shell.RenderContext) (*view.Frame, error) {
	size, palette := context.Layout.Render, context.Theme
	frame, err := view.NewFrame(size.Columns, size.Rows)
	if err != nil {
		return nil, err
	}
	base := view.Style{Foreground: palette.Text, Background: palette.Background}
	frame.Fill(0, 0, size.Columns, size.Rows, base)
	if size.Columns == 0 || size.Rows == 0 {
		return frame, nil
	}
	header := view.Style{Foreground: palette.Accent, Background: palette.PanelBackground, Bold: true}
	frame.Fill(0, 0, size.Columns, 1, header)
	frame.PutText(1, 0, fit("CONFIGURATION", size.Columns-2), header)
	if context.Layout.Class == responsive.Recovery {
		lines := []string{
			"Terminal is too small",
			fmt.Sprintf("Need 40×10 · received %d×%d", context.Layout.Reported.Columns, context.Layout.Reported.Rows),
			"Resize to continue; your draft is preserved.",
		}
		putLines(frame, 1, 2, size.Columns-2, size.Rows-2, lines, base)
		return frame, nil
	}
	status := string(model.state)
	if model.rollbackToken != "" {
		status += " · Alt-r rollback"
	}
	frame.PutText(1, 1, fit(status, size.Columns-2), view.Style{Foreground: palette.Muted, Background: palette.Background})
	bottom := size.Rows - 1
	frame.PutText(0, bottom, fit("Tab/↑↓ field · Enter apply · Alt-r rollback · Ctrl-C close", size.Columns), view.Style{Foreground: palette.Muted, Background: palette.PanelBackground})
	available := max(0, bottom-2)
	rowHeight := 2
	visible := max(1, available/rowHeight)
	start := 0
	if model.focus >= visible {
		start = model.focus - visible + 1
	}
	for row, index := 0, start; row < visible && index < len(model.fields); row, index = row+1, index+1 {
		y := 2 + row*rowHeight
		field := model.fields[index]
		style := base
		prefix := "  "
		if index == model.focus {
			prefix = "▌ "
			style.Background, style.Bold = palette.ActiveRowBackground, true
			frame.Fill(0, y, size.Columns, min(rowHeight, bottom-y), style)
		}
		value := model.buffers[index].Text()
		if field.Secret {
			value = strings.Repeat("•", model.buffers[index].GraphemeCount())
		}
		frame.PutText(0, y, fit(prefix+field.Label+": "+value, size.Columns), style)
		if message := model.errors[field.ID]; message != "" && y+1 < bottom {
			frame.PutText(2, y+1, fit("! "+message, size.Columns-2), view.Style{Foreground: palette.Red, Background: style.Background})
		}
	}
	return frame, nil
}

func fit(text string, width int) string {
	return view.Truncate(text, width, "…")
}

func putLines(frame *view.Frame, x, y, width, height int, lines []string, style view.Style) {
	if width <= 0 || height <= 0 {
		return
	}
	for row := range min(height, len(lines)) {
		frame.PutText(x, y+row, fit(lines[row], width), style)
	}
}
