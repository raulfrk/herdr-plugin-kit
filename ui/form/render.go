package form

import (
	"fmt"
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
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
	putTextBox(frame, "form-header", 0, 1, 0, size.Columns-2, 1, false, []string{"CONFIGURATION"}, header)
	if context.Layout.Class == responsive.Recovery {
		lines := []string{
			"Terminal is too small",
			fmt.Sprintf("Need 40×10 · received %d×%d", context.Layout.Reported.Columns, context.Layout.Reported.Rows),
			"Resize to continue; your draft is preserved.",
		}
		putTextBox(frame, "form-recovery", 0, 1, 2, size.Columns-2, size.Rows-2, false, lines, base)
		return frame, nil
	}
	status := string(model.state)
	if model.rollbackToken != "" {
		status += " · Alt-r rollback"
	}
	putTextBox(frame, "form-status", 0, 1, 1, size.Columns-2, 1, false, []string{status}, view.Style{Foreground: palette.Muted, Background: palette.Background})
	bottom := size.Rows - 1
	putTextBox(frame, "form-footer", 0, 0, bottom, size.Columns, 1, false, []string{"Tab/↑↓ field · Enter apply · Alt-r rollback · Ctrl-C close"}, view.Style{Foreground: palette.Muted, Background: palette.PanelBackground})
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
		putTextBox(frame, "form-field", index, 0, y, size.Columns, 1, false, []string{prefix + field.Label + ": " + value}, style)
		if message := model.errors[field.ID]; message != "" {
			putTextBox(frame, "form-field-error", index, 2, y+1, size.Columns-2, min(1, max(0, bottom-y-1)), false, []string{"! " + message}, view.Style{Foreground: palette.Red, Background: style.Background})
		}
	}
	return frame, nil
}

func putTextBox(frame *view.Frame, element string, instance, x, y, width, height int, allowTruncation bool, lines []string, style view.Style) {
	id, _ := diagnostics.NewID(element)
	_ = frame.PutTextBox(view.TextBoxOptions{
		Element: id, Instance: instance, X: x, Y: y, Width: max(0, width), Height: max(0, height),
		Mode: view.TextTruncate, AllowTruncation: allowTruncation,
	}, lines, style)
}
