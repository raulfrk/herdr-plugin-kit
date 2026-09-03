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
	title  string
	query  string
	status string
	rows   []string
	detail []string
}

func Render(spec Spec) (*view.Frame, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	palette, _ := theme.Builtin(spec.ThemeID)
	frame, err := view.NewFrame(spec.Viewport.Width, spec.Viewport.Height)
	if err != nil {
		return nil, err
	}
	c := canvas{frame: frame, palette: palette}
	c.fill(0, 0, frame.Width(), frame.Height(), palette.Background)
	data := scenarioSample(spec.Scenario)
	switch spec.Design.ID {
	case "dense-palette":
		c.renderDense(spec, data)
	case "split-inspector":
		c.renderSplit(spec, data)
	case "calm-cards":
		c.renderCards(spec, data)
	default:
		return nil, fmt.Errorf("unknown design %q", spec.Design.ID)
	}
	return frame, nil
}

func validateSpec(spec Spec) error {
	if !containsDesign(spec.Design.ID) {
		return fmt.Errorf("unknown design %q", spec.Design.ID)
	}
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

func containsDesign(id string) bool {
	for _, v := range designs {
		if v.ID == id {
			return true
		}
	}
	return false
}
func containsViewport(id string) bool {
	for _, v := range viewports {
		if v.ID == id {
			return true
		}
	}
	return false
}
func containsScenario(id string) bool {
	for _, v := range scenarios {
		if v.ID == id {
			return true
		}
	}
	return false
}

func (c canvas) renderDense(spec Spec, data sample) {
	w, h := c.frame.Width(), c.frame.Height()
	c.fill(0, 0, w, 2, c.palette.SidebarBackground)
	c.text(2, 0, w-4, "HERDR / "+strings.ToUpper(data.title), view.Style{Foreground: c.palette.Text, Background: c.palette.SidebarBackground, Bold: true})
	c.text(2, 1, w-4, spec.ThemeID+"  |  "+spec.Viewport.Name+"  |  "+data.status, view.Style{Foreground: c.palette.Muted, Background: c.palette.SidebarBackground})
	c.box(1, 3, w-2, 3, c.palette.PanelBackground, c.palette.Border)
	c.text(3, 4, w-6, "> "+data.query, view.Style{Foreground: c.palette.Text, Background: c.palette.PanelBackground})
	if w >= 80 {
		left := w * 3 / 5
		c.box(1, 7, left-1, h-9, c.palette.PanelBackground, c.palette.Border)
		c.box(left+1, 7, w-left-2, h-9, c.palette.Surface, c.palette.Border)
		c.rows(3, 8, left-5, h-11, data.rows, true)
		c.details(left+3, 8, w-left-6, h-11, data.detail)
	} else {
		detailHeight := max(4, (h-8)/3)
		listHeight := h - 9 - detailHeight
		c.box(1, 7, w-2, listHeight, c.palette.PanelBackground, c.palette.Border)
		c.rows(3, 8, w-6, max(1, listHeight-2), data.rows, true)
		c.box(1, 7+listHeight, w-2, detailHeight, c.palette.Surface, c.palette.Border)
		c.details(3, 8+listHeight, w-6, max(1, detailHeight-2), data.detail)
	}
	c.text(2, h-1, w-4, "j/k navigate   enter inspect   esc close", view.Style{Foreground: c.palette.Muted, Background: c.palette.Background})
}

func (c canvas) renderSplit(spec Spec, data sample) {
	w, h := c.frame.Width(), c.frame.Height()
	if w >= 80 {
		nav := max(18, w/6)
		master := max(32, w/3)
		c.fill(0, 0, nav, h, c.palette.SidebarBackground)
		c.text(2, 1, nav-4, "HERDR", view.Style{Foreground: c.palette.Accent, Background: c.palette.SidebarBackground, Bold: true})
		for i, item := range []string{"Search", "Switchers", "Configure", "Diagnostics"} {
			style := view.Style{Foreground: c.palette.Muted, Background: c.palette.SidebarBackground}
			if strings.Contains(strings.ToLower(data.title), strings.ToLower(item[:min(6, len(item))])) || i == 0 {
				style = view.Style{Foreground: c.palette.Text, Background: c.palette.ActiveRowBackground, Bold: true}
			}
			c.fill(1, 4+i*2, nav-2, 1, style.Background)
			c.text(3, 4+i*2, nav-6, item, style)
		}
		c.fill(nav, 0, master, h, c.palette.PanelBackground)
		c.text(nav+2, 1, master-4, data.title, view.Style{Foreground: c.palette.Text, Background: c.palette.PanelBackground, Bold: true})
		c.text(nav+2, 3, master-4, "> "+data.query, view.Style{Foreground: c.palette.Muted, Background: c.palette.Surface})
		c.rows(nav+2, 5, master-4, h-7, data.rows, true)
		c.fill(nav+master, 0, w-nav-master, h, c.palette.Background)
		c.details(nav+master+3, 2, w-nav-master-6, h-5, data.detail)
		c.text(nav+master+3, h-2, w-nav-master-6, data.status, view.Style{Foreground: c.palette.Green, Background: c.palette.Background, Bold: true})
		return
	}
	c.fill(0, 0, w, 3, c.palette.SidebarBackground)
	c.text(2, 0, w-4, "HERDR  "+data.title, view.Style{Foreground: c.palette.Text, Background: c.palette.SidebarBackground, Bold: true})
	c.text(2, 1, w-4, "Search | Configure | Debug", view.Style{Foreground: c.palette.Muted, Background: c.palette.SidebarBackground})
	c.text(2, 3, w-4, "> "+data.query, view.Style{Foreground: c.palette.Text, Background: c.palette.Surface})
	masterHeight := max(4, (h-5)*2/5)
	c.rows(2, 5, w-4, masterHeight, data.rows, true)
	c.fill(0, 5+masterHeight, w, h-5-masterHeight, c.palette.PanelBackground)
	c.details(2, 6+masterHeight, w-4, h-8-masterHeight, data.detail)
	c.text(2, h-1, w-4, data.status, view.Style{Foreground: c.palette.Green, Background: c.palette.PanelBackground, Bold: true})
}

func (c canvas) renderCards(spec Spec, data sample) {
	w, h := c.frame.Width(), c.frame.Height()
	margin := 2
	if w >= 100 {
		margin = 5
	}
	c.text(margin, 1, w-2*margin, data.title, view.Style{Foreground: c.palette.Text, Background: c.palette.Background, Bold: true})
	c.text(margin, 2, w-2*margin, "A calm workspace for "+strings.Join(spec.Scenario.Surfaces, ", "), view.Style{Foreground: c.palette.Muted, Background: c.palette.Background})
	c.box(margin, 4, w-2*margin, 4, c.palette.PanelBackground, c.palette.Border)
	c.text(margin+2, 5, w-2*margin-4, data.query, view.Style{Foreground: c.palette.Text, Background: c.palette.PanelBackground})
	c.text(margin+2, 6, w-2*margin-4, data.status, view.Style{Foreground: c.palette.Accent, Background: c.palette.PanelBackground})
	available := h - 11
	if w >= 80 {
		gap := 2
		cardW := (w - 2*margin - gap) / 2
		c.box(margin, 9, cardW, available, c.palette.PanelBackground, c.palette.Border)
		c.box(margin+cardW+gap, 9, w-margin-(margin+cardW+gap), available, c.palette.Surface, c.palette.Border)
		c.rows(margin+2, 10, cardW-4, available-2, data.rows, false)
		c.details(margin+cardW+gap+2, 10, cardW-4, available-2, data.detail)
	} else {
		firstH := max(3, available/2)
		c.box(margin, 9, w-2*margin, firstH, c.palette.PanelBackground, c.palette.Border)
		c.rows(margin+2, 10, w-2*margin-4, max(1, firstH-2), data.rows, false)
		secondY := 9 + firstH + 1
		secondH := max(2, h-secondY-1)
		c.box(margin, secondY, w-2*margin, secondH, c.palette.Surface, c.palette.Border)
		c.details(margin+2, secondY+1, w-2*margin-4, max(1, secondH-2), data.detail)
	}
	c.text(margin, h-1, w-2*margin, "Review sample · static data", view.Style{Foreground: c.palette.Muted, Background: c.palette.Background})
}

func (c canvas) rows(x, y, width, height int, rows []string, selected bool) {
	for i := 0; i < min(height, len(rows)); i++ {
		style := view.Style{Foreground: c.palette.Text, Background: c.palette.PanelBackground}
		prefix := "  "
		if selected && i == 1 {
			style.Background = c.palette.SelectionBackground
			style.Bold = true
			prefix = "> "
		}
		c.fill(x, y+i, width, 1, style.Background)
		c.text(x, y+i, width, prefix+rows[i], style)
	}
}

func (c canvas) details(x, y, width, height int, lines []string) {
	for i := 0; i < min(height, len(lines)); i++ {
		style := view.Style{Foreground: c.palette.Text, Background: c.palette.Surface}
		if i == 0 {
			style.Foreground = c.palette.Accent
			style.Bold = true
		}
		if strings.HasPrefix(lines[i], "!") {
			style.Foreground = c.palette.Red
			style.Bold = true
		}
		if strings.HasPrefix(lines[i], "OK ") {
			style.Foreground = c.palette.Green
			style.Bold = true
		}
		c.text(x, y+i, width, lines[i], style)
	}
}

func (c canvas) box(x, y, width, height int, background, border theme.Color) {
	if width <= 0 || height <= 0 {
		return
	}
	c.fill(x, y, width, height, background)
	style := view.Style{Foreground: border, Background: background}
	if width >= 2 {
		c.text(x, y, width, "+"+strings.Repeat("-", max(0, width-2))+"+", style)
		if height > 1 {
			c.text(x, y+height-1, width, "+"+strings.Repeat("-", max(0, width-2))+"+", style)
		}
	}
	for row := y + 1; row < y+height-1; row++ {
		c.text(x, row, 1, "|", style)
		c.text(x+width-1, row, 1, "|", style)
	}
}

func (c canvas) fill(x, y, width, height int, background theme.Color) {
	c.frame.Fill(x, y, width, height, view.Style{Background: background})
}
func (c canvas) text(x, y, width int, text string, style view.Style) {
	if width <= 0 {
		return
	}
	c.frame.PutText(x, y, fit(text, width), style)
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
	base := sample{title: s.Name, query: "Search sessions, actions, memories, and settings...", status: "Ready | static review data", rows: []string{"Yesterday | Debug flaky pane resize", "11:42 | Recall terminal theme decision", "Action | Switch attention to build agent", "Session | diagnostics implementation", "Configure | snapshot retention", "Command | Copy selected identifier"}, detail: []string{"Selected detail", "Source: Codex Recall", "Workspace: herdr-plugin-kit", "Updated 2 minutes ago", "Tags: terminal, responsive", "Enter opens | Ctrl-K actions"}}
	switch s.ID {
	case "config-validation":
		base.query = "Plugin configuration"
		base.status = "1 field needs attention"
		base.rows = []string{"Theme             catppuccin", "Refresh interval  -5ms", "Storage path      ~/.local/state", "Live reload       enabled"}
		base.detail = []string{"Validation", "! Refresh interval must be positive", "Last valid value remains active", "Fix the field to apply changes"}
	case "config-live-applied":
		base.query = "Plugin configuration"
		base.status = "Applied live | 11:42:08"
		base.rows = []string{"Theme             nord", "Refresh interval  250ms", "Storage path      ~/.local/state", "Live reload       enabled"}
		base.detail = []string{"Live configuration", "OK Changes applied without restart", "Revision: cfg-84f2", "Watching source file"}
	case "diagnostic-timeline-detail":
		base.query = "Filter events: session.changed"
		base.rows = []string{"11:42:08.120  action.started", "11:42:08.164  session.changed", "11:42:08.201  frame.rendered", "11:42:08.214  snapshot.stored", "11:42:08.231  action.finished"}
		base.detail = []string{"session.changed", "duration_ms: 37", "source: attention-switcher", "session_id: ses_demo_04", "payload: redacted", "correlation: req_28ac"}
	case "diagnostic-storage-health":
		base.query = "Diagnostic storage"
		base.status = "Healthy | last checked now"
		base.rows = []string{"Events       18.4 MiB / 128 MiB", "Screenshots  42 files", "Snapshots    186 records", "Oldest       7 days ago", "Write latency 2.1 ms"}
		base.detail = []string{"Storage health", "OK Writable and below quota", "Retention: 14 days", "Compaction: idle", "Next sweep: in 3 hours"}
	case "diagnostic-screenshot":
		base.query = "Screenshot capture · frame 184"
		base.rows = []string{"Capture 184  11:42:08", "Capture 183  11:41:52", "Capture 182  11:41:31", "Capture 181  11:40:57"}
		base.detail = []string{"Screenshot state", "+----------------------+", "|  pane preview        |", "|  [selected row]      |", "|  status: connected   |", "+----------------------+", "960 x 544 | PNG | 42 KiB"}
	case "loading":
		base.query = "Loading recent activity..."
		base.status = "Working | 3 sources pending"
		base.rows = []string{"* Recall index", "* Sessions", "* Available actions"}
		base.detail = []string{"Preparing workspace", "Results appear as sources respond", "You can close this view safely"}
	case "empty":
		base.query = "no matching archived sessions"
		base.status = "0 results"
		base.rows = []string{"No matches", "Try fewer terms", "or clear the current filters"}
		base.detail = []string{"Nothing here yet", "Saved sessions and actions", "will appear in this space."}
	case "error":
		base.query = "Search unavailable"
		base.status = "Recoverable error | retry available"
		base.rows = []string{"! Recall index did not respond", "Cached actions remain available", "Diagnostics captured event req_28ac"}
		base.detail = []string{"! Could not refresh results", "The previous data is unchanged.", "Retry now", "Open diagnostic event"}
	case "long-content":
		base.query = "A deliberately long query about a deeply nested workspace session whose title exceeds compact viewport width"
		base.status = "Long values truncate at grapheme boundaries"
		base.rows = []string{"Session | Investigate deterministic rendering across every supported terminal viewport and theme", "Memory | Decision record with emoji 🧭 and combining text é preserved", "Action | Reconfigure an unusually-long-plugin-identifier-without-breaking-layout", "Event | payload={nested:[many,values],message:content-remains-bounded}", "Path | /workspace/a/very/long/location/that/must/not/escape/the/frame"}
		base.detail = []string{"Long-content boundary", "This paragraph intentionally exceeds the available panel width so every family demonstrates deterministic ellipsis rather than drawing beyond its assigned region.", "Unicode: 東京 · café · 🧭", "Unbroken: abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz", "Final visible line"}
	}
	return base
}
