package catalogue

import (
	"fmt"
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
	"github.com/rivo/uniseg"
)

type canvas struct {
	frame   *view.Frame
	palette theme.Palette
}

type sample struct {
	title    string
	query    string
	status   string
	help     string
	rows     []string
	detail   []string
	selected int
}

func Render(spec Spec) (*view.Frame, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	return renderSample(spec, scenarioSample(spec.Scenario))
}

func renderSample(spec Spec, data sample) (*view.Frame, error) {
	palette, err := theme.Builtin(spec.ThemeID)
	if err != nil {
		return nil, err
	}
	frame, err := view.NewFrame(spec.Viewport.Width, spec.Viewport.Height)
	if err != nil {
		return nil, err
	}
	c := canvas{frame: frame, palette: palette}
	c.fill(0, 0, frame.Width(), frame.Height(), palette.Background)
	c.renderCommand(data)
	return frame, nil
}

func validateSpec(spec Spec) error {
	if _, err := theme.Builtin(spec.ThemeID); err != nil {
		return err
	}
	if spec.Viewport.Width <= 0 || spec.Viewport.Height <= 0 || !containsViewport(spec.Viewport.ID) {
		return fmt.Errorf("unknown or invalid viewport %q", spec.Viewport.ID)
	}
	if !containsScenario(spec.Scenario.ID) {
		return fmt.Errorf("unknown scenario %q", spec.Scenario.ID)
	}
	return nil
}

func containsViewport(id string) bool {
	for _, viewport := range viewports {
		if viewport.ID == id {
			return true
		}
	}
	return false
}

func containsScenario(id string) bool {
	for _, scenario := range scenarios {
		if scenario.ID == id {
			return true
		}
	}
	return false
}

func (c canvas) renderCommand(data sample) {
	w, h := c.frame.Width(), c.frame.Height()
	if h <= 18 {
		c.renderCompact(data)
		return
	}
	margin := 2
	if w >= 100 {
		margin = 4
	}
	c.fill(0, 0, w, 3, c.palette.PanelBackground)
	c.text(margin, 1, w-2*margin, data.title, view.Style{Foreground: c.palette.Text, Background: c.palette.PanelBackground, Bold: true})
	c.text(margin, 2, w-2*margin, data.status, view.Style{Foreground: c.palette.Muted, Background: c.palette.PanelBackground})
	c.fill(margin, 4, w-2*margin, 3, c.palette.PanelBackground)
	c.fill(margin, 4, 1, 3, c.palette.Accent)
	c.text(margin+3, 5, w-2*margin-5, "⌕  "+data.query, view.Style{Foreground: c.palette.Text, Background: c.palette.PanelBackground, Bold: true})
	contentTop := 8
	if w < 80 {
		listHeight := min(len(data.rows), max(3, h-contentTop-6))
		c.rows(margin, contentTop, w-2*margin, listHeight, data.rows, data.selected)
		detailY := contentTop + listHeight + 1
		c.fill(margin, detailY, w-2*margin, max(0, h-detailY-3), c.palette.Surface)
		c.details(margin+2, detailY+1, w-2*margin-4, max(0, h-detailY-5), data.detail)
	} else {
		gap := 2
		available := w - 2*margin - gap
		mainWidth := available * 2 / 3
		metaX := margin + mainWidth + gap
		c.rows(margin, contentTop, mainWidth, min(len(data.rows), h-contentTop-3), data.rows, data.selected)
		c.fill(metaX, contentTop, available-mainWidth, h-contentTop-3, c.palette.Surface)
		c.details(metaX+2, contentTop+2, available-mainWidth-4, h-contentTop-5, data.detail)
	}
	c.text(margin, h-2, w-2*margin, data.status, view.Style{Foreground: c.palette.Accent, Background: c.palette.Background, Bold: true})
	c.text(margin, h-1, w-2*margin, data.help, view.Style{Foreground: c.palette.Muted, Background: c.palette.Background})
}

func (c canvas) renderCompact(data sample) {
	w, h := c.frame.Width(), c.frame.Height()
	c.fill(0, 0, w, 1, c.palette.PanelBackground)
	c.text(1, 0, w-2, data.title, view.Style{Foreground: c.palette.Text, Background: c.palette.PanelBackground, Bold: true})
	c.fill(0, 1, w, 1, c.palette.PanelBackground)
	c.text(1, 1, w-2, "⌕ "+data.query, view.Style{Foreground: c.palette.Text, Background: c.palette.PanelBackground})
	c.rows(1, 2, w-2, h-4, data.rows, data.selected)
	c.fill(0, h-2, w, 2, c.palette.Background)
	c.text(1, h-2, w-2, data.status, view.Style{Foreground: c.palette.Accent, Background: c.palette.Background, Bold: true})
	c.text(1, h-1, w-2, data.help, view.Style{Foreground: c.palette.Muted, Background: c.palette.Background})
}

func (c canvas) rows(x, y, width, height int, rows []string, selected int) {
	for index := range min(height, len(rows)) {
		style := view.Style{Foreground: c.palette.Text, Background: c.palette.Background}
		prefix := "  "
		if index == selected {
			prefix = "▌ "
			style.Bold = true
			style.Foreground = c.palette.Accent
			style.Background = c.palette.SelectionBackground
		}
		c.fill(x, y+index, width, 1, style.Background)
		c.text(x+1, y+index, width-2, prefix+rows[index], style)
	}
}

func (c canvas) details(x, y, width, height int, lines []string) {
	for index := range min(height, len(lines)) {
		style := view.Style{Foreground: c.palette.Text, Background: c.palette.Surface}
		if index == 0 || strings.HasPrefix(lines[index], "!") || strings.HasPrefix(lines[index], "OK ") {
			style.Bold = true
		}
		if strings.HasPrefix(lines[index], "!") {
			style.Foreground = c.palette.Red
		}
		if strings.HasPrefix(lines[index], "OK ") {
			style.Foreground = c.palette.Green
		}
		c.text(x, y+index, width, lines[index], style)
	}
}

func (c canvas) fill(x, y, width, height int, background theme.Color) {
	c.frame.Fill(x, y, width, height, view.Style{Background: background})
}

func (c canvas) text(x, y, width int, text string, style view.Style) {
	if width > 0 {
		c.frame.PutText(x, y, fit(text, width), style)
	}
}

func fit(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if uniseg.StringWidth(value) <= width {
		return value
	}
	if width <= 3 {
		return strings.Repeat(".", width)
	}
	var result strings.Builder
	used := 0
	graphemes := uniseg.NewGraphemes(value)
	for graphemes.Next() {
		cluster := graphemes.Str()
		size := uniseg.StringWidth(cluster)
		if used+size > width-3 {
			break
		}
		result.WriteString(cluster)
		used += size
	}
	return result.String() + "..."
}

func scenarioSample(s Scenario) sample {
	base := sample{title: "Bento Command · " + s.Name, query: "Search sessions, actions, memories, and settings...", status: "Ready | static review data", help: "? help · / search · enter open · esc back", rows: []string{"Yesterday | Debug flaky pane resize", "11:42 | Recall terminal theme decision", "Action | Switch attention to build agent", "Session | diagnostics implementation", "Configure | snapshot retention", "Command | Copy selected identifier"}, detail: []string{"Selected detail", "Source: Codex Recall", "Workspace: herdr-plugin-kit", "Updated 2 minutes ago", "Tags: terminal, responsive", "Enter opens"}, selected: 1}
	switch s.ID {
	case "config-validation":
		base.status, base.rows, base.detail = "1 field needs attention", []string{"Theme catppuccin", "Refresh interval -5ms", "Storage path ~/.local/state", "Live reload enabled"}, []string{"Validation", "! Refresh interval must be positive", "Last valid value remains active", "Fix the field to apply changes"}
	case "config-live-applied":
		base.status, base.detail = "Applied live | 11:42:08", []string{"Live configuration", "OK Changes applied without restart", "Revision: cfg-84f2"}
	case "diagnostic-timeline-detail", "diagnostic-storage-health", "diagnostic-screenshot":
		base.query, base.status, base.rows, base.detail = "Filter diagnostic events", "Diagnostic review", []string{"11:42 action.started", "11:42 session.changed", "11:42 frame.rendered", "11:42 snapshot.stored"}, []string{"Diagnostic event", "payload: redacted", "correlation: req_28ac", "Export sanitized report"}
	case "loading":
		base.status, base.rows = "Working | 3 sources pending", []string{"* Recall index", "* Sessions", "* Available actions"}
	case "empty":
		base.status, base.rows = "0 results", []string{"No matches", "Try fewer terms", "Clear the current filters"}
	case "error":
		base.status, base.rows, base.detail = "Recoverable error | retry available", []string{"! Recall index did not respond", "▌ Retry", "Open diagnostic event"}, []string{"! Could not refresh results", "The previous data is unchanged.", "Retry now"}
	case "long-content":
		base.query, base.status = "A deliberately long query with Unicode 東京 café 🧭", "Long values truncate at grapheme boundaries"
		base.rows = longLines("Result", 220)
		base.detail = longLines("Detail", 220)
	}
	return base
}

func longLines(label string, count int) []string {
	lines := make([]string, count)
	for index := range lines {
		lines[index] = fmt.Sprintf("%s %03d · %s", label, index+1, strings.Repeat("Unicode 東京 café 🧭 · ", 30))
	}
	return lines
}
