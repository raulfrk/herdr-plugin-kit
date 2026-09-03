package catalogue

import (
	"fmt"
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
	"github.com/rivo/uniseg"
)

type FlowState struct{ ID, Name string }

type PluginSurface struct {
	ID, Name string
	States   []FlowState
}

var pluginSurfaces = []PluginSurface{
	{ID: "recall-search", Name: "Recall Search", States: flow("Query", "Results", "Detail")},
	{ID: "attention-switcher", Name: "Attention Switcher", States: flow("Agents", "Switching", "Switched")},
	{ID: "session-switcher", Name: "Session Switcher", States: flow("Sessions", "Opening", "Reopened")},
	{ID: "plugin-configurator", Name: "Plugin Configurator", States: flow("Editing", "Validation Error", "Applied")},
	{ID: "action-finder", Name: "Action Finder", States: flow("Actions", "Running", "Completed")},
	{ID: "debug-ui", Name: "Debug UI", States: flow("Health", "Timeline", "Event Detail")},
}

func flow(first, second, third string) []FlowState {
	return []FlowState{{ID: slug(first), Name: first}, {ID: slug(second), Name: second}, {ID: slug(third), Name: third}, {ID: "loading", Name: "Loading"}, {ID: "empty", Name: "Empty"}, {ID: "error", Name: "Recoverable Error"}}
}

func slug(value string) string { return strings.ToLower(strings.ReplaceAll(value, " ", "-")) }

func PluginSurfaces() []PluginSurface {
	result := make([]PluginSurface, len(pluginSurfaces))
	for index, surface := range pluginSurfaces {
		result[index] = surface
		result[index].States = append([]FlowState(nil), surface.States...)
	}
	return result
}

type liveAxis uint8

const (
	pluginAxis liveAxis = iota
	stateAxis
	themeAxis
	liveAxisCount
)

type LiveSurface struct {
	plugin, state, theme int
	active               liveAxis
	selected             int
	query                string
	editing, hud, help   bool
	layout               responsive.Layout
	resizeGeneration     uint64
}

func NewLiveSurface() *LiveSurface { return &LiveSurface{selected: 1} }

func (surface *LiveSurface) Update(_ shell.EventContext, event shell.Event) []shell.Effect {
	switch event := event.(type) {
	case shell.ResizeEvent:
		surface.layout, surface.resizeGeneration = event.Layout, event.Generation
	case shell.TextEvent:
		if surface.editing {
			surface.query += event.Text
			return nil
		}
		if event.Text == "q" {
			return []shell.Effect{shell.Quit()}
		}
		if len(event.Text) == 1 {
			surface.shortcut(event.Text)
		}
	case shell.KeyEvent:
		return surface.key(event.Code)
	}
	return nil
}

func (surface *LiveSurface) shortcut(key string) {
	switch key {
	case "p", "P":
		surface.plugin = cycle(surface.plugin, len(pluginSurfaces), key == "P")
		surface.state, surface.selected = 0, 1
	case "s", "S":
		surface.state = cycle(surface.state, len(surface.currentPlugin().States), key == "S")
	case "t", "T":
		surface.theme = cycle(surface.theme, len(theme.IDs()), key == "T")
	case "h", "H":
		surface.hud = !surface.hud
	case "?":
		surface.help = !surface.help
	case "/":
		surface.editing, surface.query = true, ""
	}
}

func (surface *LiveSurface) key(key shell.KeyCode) []shell.Effect {
	switch key {
	case shell.KeyCtrlC:
		return []shell.Effect{shell.Quit()}
	case shell.KeyEscape:
		if surface.help {
			surface.help = false
			return nil
		}
		if surface.editing {
			surface.editing = false
			return nil
		}
		return []shell.Effect{shell.Quit()}
	case shell.KeyEnter:
		if surface.editing {
			surface.editing = false
		} else {
			surface.state = cycle(surface.state, len(surface.currentPlugin().States), false)
		}
	case shell.KeyBackspace:
		if surface.editing {
			surface.query = trimLastGrapheme(surface.query)
		} else {
			surface.state = cycle(surface.state, len(surface.currentPlugin().States), true)
		}
	case shell.KeyTab:
		surface.active = liveAxis((int(surface.active) + 1) % int(liveAxisCount))
	case shell.KeyLeft:
		surface.moveActive(true)
	case shell.KeyRight:
		surface.moveActive(false)
	case shell.KeyUp:
		surface.selected = cycle(surface.selected, 6, true)
	case shell.KeyDown:
		surface.selected = cycle(surface.selected, 6, false)
	}
	return nil
}

func (surface *LiveSurface) moveActive(reverse bool) {
	switch surface.active {
	case pluginAxis:
		surface.plugin = cycle(surface.plugin, len(pluginSurfaces), reverse)
		surface.state, surface.selected = 0, 1
	case stateAxis:
		surface.state = cycle(surface.state, len(surface.currentPlugin().States), reverse)
	case themeAxis:
		surface.theme = cycle(surface.theme, len(theme.IDs()), reverse)
	}
}

func cycle(value, count int, reverse bool) int {
	if reverse {
		return (value + count - 1) % count
	}
	return (value + 1) % count
}

func trimLastGrapheme(value string) string {
	graphemes, previous := uniseg.NewGraphemes(value), 0
	for graphemes.Next() {
		start, _ := graphemes.Positions()
		previous = start
	}
	return value[:previous]
}

func (surface *LiveSurface) Render(context shell.RenderContext) (*view.Frame, error) {
	plugin, state, themeID := surface.currentPlugin(), surface.currentPlugin().States[surface.state], theme.IDs()[surface.theme]
	if surface.help {
		return renderCatalogueHelp(context.Layout.Render, themeID)
	}
	data := liveSample(plugin.ID, state.ID)
	data.title = plugin.Name + " · Bento Command"
	data.selected, data.help = surface.selected, navigationHelp(context.Layout.Render.Columns)
	if surface.query != "" || surface.editing {
		data.query = surface.query
		if surface.editing {
			data.query += "▏"
		}
	}
	data.status = surface.status(context, state.Name, themeID)
	return renderSample(Spec{ThemeID: themeID, Viewport: Viewport{ID: "live", Name: "Live", Width: context.Layout.Render.Columns, Height: context.Layout.Render.Rows}, Scenario: Scenario{ID: state.ID, Name: state.Name, State: state.ID, Surfaces: []string{plugin.ID}}}, data)
}

func (surface *LiveSurface) status(context shell.RenderContext, state, themeID string) string {
	if surface.hud {
		return fmt.Sprintf("HUD %dx%d→%dx%d %s g%d", context.Layout.Reported.Columns, context.Layout.Reported.Rows, context.Layout.Render.Columns, context.Layout.Render.Rows, context.Layout.Class, context.ResizeGeneration)
	}
	if context.Layout.Render.Rows <= 18 {
		return fmt.Sprintf("%s · %s", state, themeID)
	}
	return fmt.Sprintf("%s · %s · Bento Command / Structured", state, themeID)
}

func navigationHelp(columns int) string {
	if columns < 70 {
		return "? help · Tab field · ←→ change"
	}
	return "? help · Tab field · ←/→ change · / search · Esc quit"
}

func renderCatalogueHelp(size responsive.Size, themeID string) (*view.Frame, error) {
	palette, err := theme.Builtin(themeID)
	if err != nil {
		return nil, err
	}
	frame, err := view.NewFrame(size.Columns, size.Rows)
	if err != nil {
		return nil, err
	}
	frame.Fill(0, 0, size.Columns, size.Rows, view.Style{Background: palette.Background})
	for row, line := range []string{"CATALOGUE HELP", "p/P surface · s/S state", "t/T theme · Tab field · ←→ change", "↑↓ select · / search · Backspace delete/back", "Enter next / commit search · h HUD", "? / Esc close help · q / Ctrl-C quit"} {
		style := view.Style{Foreground: palette.Text, Background: palette.Background}
		if row == 0 {
			style.Foreground, style.Background, style.Bold = palette.Accent, palette.PanelBackground, true
			frame.Fill(0, row, size.Columns, 1, style)
		}
		frame.PutText(1, row, fit(line, size.Columns-2), style)
	}
	return frame, nil
}

func (surface *LiveSurface) currentPlugin() PluginSurface { return pluginSurfaces[surface.plugin] }

func (surface *LiveSurface) DiagnosticState() diagnostics.VisualState {
	plugin, state := surface.currentPlugin(), surface.currentPlugin().States[surface.state]
	viewState := "plain"
	if surface.hud {
		viewState = "hud"
	}
	if surface.help {
		viewState = "help"
	}
	return diagnostics.VisualState{Screen: mustID("catalogue." + plugin.ID), Focus: mustID([]string{"surface", "state", "theme"}[surface.active]), Selection: mustID(theme.IDs()[surface.theme]), State: mustID(strings.Join([]string{state.ID, viewState}, ".")), Geometry: diagnostics.Geometry{ReportedColumns: surface.layout.Reported.Columns, ReportedRows: surface.layout.Reported.Rows, RenderColumns: surface.layout.Render.Columns, RenderRows: surface.layout.Render.Rows}, ResizeGeneration: surface.resizeGeneration, ItemCount: len(plugin.States), SelectedIndex: surface.selected, Pending: surface.editing, HasError: state.ID == "error" || state.ID == "validation-error"}
}

func mustID(value string) diagnostics.ID { id, _ := diagnostics.NewID(value); return id }

func liveSample(plugin, state string) sample {
	data := scenarioSample(Scenario{ID: "picker-search-selected", Name: "Live review"})
	data.title = plugin
	switch state {
	case "loading", "switching", "opening", "running":
		data.status = "Working · progress 2/3"
	case "empty":
		data.status, data.rows = "0 results · clear filters", []string{"No matches", "Try fewer terms", "Clear the current query"}
	case "error", "validation-error":
		data.status, data.rows = "! Recoverable error · retry available", []string{"! Request could not complete", "▌ Retry", "Open diagnostic event"}
	case "switched", "reopened", "applied", "completed":
		data.status = "OK Action completed"
	case "event-detail":
		data.status, data.detail = "Event detail · payload redacted", []string{"Diagnostic event", "payload: never recorded", "correlation: c-demo"}
	}
	return data
}
