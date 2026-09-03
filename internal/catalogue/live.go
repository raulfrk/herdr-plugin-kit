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

type FlowState struct {
	ID   string
	Name string
}

type PluginSurface struct {
	ID        string
	Name      string
	States    []FlowState
	shortName string
}

type ViewportFixture struct {
	ID      string
	Name    string
	Columns int
	Rows    int
}

var pluginSurfaces = []PluginSurface{
	{ID: "recall-search", Name: "Recall Search", States: flow("Query", "Results", "Detail"), shortName: "Recall"},
	{ID: "attention-switcher", Name: "Attention Switcher", States: flow("Agents", "Switching", "Switched"), shortName: "Attention"},
	{ID: "session-switcher", Name: "Session Switcher", States: flow("Sessions", "Opening", "Reopened"), shortName: "Session"},
	{ID: "plugin-configurator", Name: "Plugin Configurator", States: flow("Editing", "Validation Error", "Applied"), shortName: "Config"},
	{ID: "action-finder", Name: "Action Finder", States: flow("Actions", "Running", "Completed"), shortName: "Actions"},
	{ID: "debug-ui", Name: "Debug UI", States: flow("Health", "Timeline", "Event Detail"), shortName: "Debug"},
}

// liveDesigns are temporary review candidates. The first-round designs remain
// available to the deterministic static catalogue until a candidate is chosen.
var liveDesigns = []Design{
	{ID: "bento-air", Name: "Bento Air", Description: "Open composition with quiet surfaces and a slim focus rail.", LayoutSignature: "open-header/search/results-context", shortName: "Air"},
	{ID: "bento-command", Name: "Bento Command", Description: "Search-led composition with a high-contrast active result.", LayoutSignature: "command-search/results-context", shortName: "Command"},
	{ID: "bento-flow", Name: "Bento Flow", Description: "Rhythmic grouped rows with compact contextual metadata.", LayoutSignature: "compact-header/grouped-results/context", shortName: "Flow"},
}

func flow(first, second, third string) []FlowState {
	return []FlowState{
		{ID: strings.ToLower(strings.ReplaceAll(first, " ", "-")), Name: first},
		{ID: strings.ToLower(strings.ReplaceAll(second, " ", "-")), Name: second},
		{ID: strings.ToLower(strings.ReplaceAll(third, " ", "-")), Name: third},
		{ID: "loading", Name: "Loading"},
		{ID: "empty", Name: "Empty"},
		{ID: "error", Name: "Recoverable Error"},
	}
}

var viewportFixtures = []ViewportFixture{
	{ID: "live", Name: "Live terminal"},
	{ID: "recovery-width", Name: "Recovery width", Columns: 39, Rows: 10},
	{ID: "recovery-height", Name: "Recovery height", Columns: 40, Rows: 9},
	{ID: "minimum", Name: "Minimum Compact", Columns: 40, Rows: 10},
	{ID: "restricted", Name: "Restricted Compact", Columns: 70, Rows: 10},
	{ID: "compact-tall", Name: "Tall Compact", Columns: 70, Rows: 30},
	{ID: "compact-width-minus", Name: "Compact width -1", Columns: 79, Rows: 24},
	{ID: "standard-width", Name: "Standard width", Columns: 80, Rows: 24},
	{ID: "standard-width-plus", Name: "Standard width +1", Columns: 81, Rows: 24},
	{ID: "compact-row-minus", Name: "Compact row -1", Columns: 110, Rows: 17},
	{ID: "standard-row", Name: "Standard row", Columns: 110, Rows: 18},
	{ID: "standard-row-plus", Name: "Standard row +1", Columns: 110, Rows: 19},
	{ID: "standard-wide-minus", Name: "Wide width -1", Columns: 109, Rows: 24},
	{ID: "wide", Name: "Wide threshold", Columns: 110, Rows: 24},
	{ID: "wide-plus", Name: "Wide width +1", Columns: 111, Rows: 24},
	{ID: "wide-row-minus", Name: "Wide row -1", Columns: 110, Rows: 23},
	{ID: "wide-row", Name: "Wide row threshold", Columns: 110, Rows: 24},
	{ID: "wide-row-plus", Name: "Wide row +1", Columns: 110, Rows: 25},
	{ID: "phone", Name: "Phone portrait", Columns: 48, Rows: 30},
	{ID: "phone-keyboard", Name: "Phone + keyboard", Columns: 48, Rows: 18},
	{ID: "phone-landscape", Name: "Phone landscape", Columns: 78, Rows: 20},
	{ID: "phone-landscape-keyboard", Name: "Phone landscape + keyboard", Columns: 78, Rows: 10},
	{ID: "tablet", Name: "Tablet", Columns: 90, Rows: 32},
	{ID: "laptop", Name: "Laptop", Columns: 120, Rows: 38},
	{ID: "ultrawide", Name: "Ultrawide", Columns: 220, Rows: 55},
	{ID: "maximum", Name: "Maximum", Columns: 500, Rows: 200},
	{ID: "projected", Name: "Projected oversize", Columns: 520, Rows: 220},
}

func PluginSurfaces() []PluginSurface {
	result := make([]PluginSurface, len(pluginSurfaces))
	for index, surface := range pluginSurfaces {
		result[index] = surface
		result[index].States = append([]FlowState(nil), surface.States...)
	}
	return result
}

func ViewportFixtures() []ViewportFixture {
	return append([]ViewportFixture(nil), viewportFixtures...)
}

type axis uint8

const (
	designAxis axis = iota
	treatmentAxis
	pluginAxis
	stateAxis
	themeAxis
	fixtureAxis
	axisCount
)

type LiveSurface struct {
	design           int
	treatment        int
	plugin           int
	state            int
	theme            int
	fixture          int
	active           axis
	selected         int
	query            string
	editing          bool
	hud              bool
	help             bool
	layout           responsive.Layout
	resizeGeneration uint64
}

func NewLiveSurface() *LiveSurface { return &LiveSurface{selected: 1} }

func (surface *LiveSurface) Update(_ shell.EventContext, event shell.Event) []shell.Effect {
	switch event := event.(type) {
	case shell.ResizeEvent:
		surface.layout = event.Layout
		surface.resizeGeneration = event.Generation
	case shell.TextEvent:
		if surface.editing {
			surface.query += event.Text
			return nil
		}
		if event.Text == "q" {
			return []shell.Effect{shell.Quit()}
		}
		if len(event.Text) == 1 {
			surface.handleShortcut(event.Text)
		}
	case shell.KeyEvent:
		return surface.handleKey(event.Code)
	}
	return nil
}

func (surface *LiveSurface) handleShortcut(key string) {
	switch key {
	case "d", "D":
		surface.design = cycle(surface.design, len(liveDesigns), key == "D")
	case "e", "E":
		surface.treatment = cycle(surface.treatment, len(treatments), key == "E")
	case "p", "P":
		surface.plugin = cycle(surface.plugin, len(pluginSurfaces), key == "P")
		surface.resetPluginState()
	case "s", "S":
		surface.state = cycle(surface.state, len(surface.currentPlugin().States), key == "S")
	case "t", "T":
		surface.theme = cycle(surface.theme, len(theme.IDs()), key == "T")
	case "f", "F":
		surface.fixture = cycle(surface.fixture, len(viewportFixtures), key == "F")
	case "h", "H":
		surface.hud = !surface.hud
	case "?":
		surface.help = !surface.help
	case "/":
		surface.editing = true
		surface.query = ""
	}
}

func (surface *LiveSurface) handleKey(key shell.KeyCode) []shell.Effect {
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
		surface.active = axis((int(surface.active) + 1) % int(axisCount))
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
	case designAxis:
		surface.design = cycle(surface.design, len(liveDesigns), reverse)
	case treatmentAxis:
		surface.treatment = cycle(surface.treatment, len(treatments), reverse)
	case pluginAxis:
		surface.plugin = cycle(surface.plugin, len(pluginSurfaces), reverse)
		surface.resetPluginState()
	case stateAxis:
		surface.state = cycle(surface.state, len(surface.currentPlugin().States), reverse)
	case themeAxis:
		surface.theme = cycle(surface.theme, len(theme.IDs()), reverse)
	case fixtureAxis:
		surface.fixture = cycle(surface.fixture, len(viewportFixtures), reverse)
	}
}

func (surface *LiveSurface) resetPluginState() {
	surface.state, surface.selected, surface.query = 0, 1, ""
	surface.editing = false
}

func cycle(value, count int, reverse bool) int {
	if reverse {
		return (value + count - 1) % count
	}
	return (value + 1) % count
}

func trimLastGrapheme(value string) string {
	graphemes := uniseg.NewGraphemes(value)
	end := 0
	previous := 0
	for graphemes.Next() {
		start, next := graphemes.Positions()
		previous, end = start, next
	}
	if end == 0 {
		return ""
	}
	return value[:previous]
}

func (surface *LiveSurface) Render(context shell.RenderContext) (*view.Frame, error) {
	plugin := surface.currentPlugin()
	state := plugin.States[surface.state]
	design := liveDesigns[surface.design]
	treatment := treatments[surface.treatment]
	themeID := theme.IDs()[surface.theme]
	if surface.help {
		return renderCatalogueHelp(context.Layout.Render, themeID)
	}
	data := liveSample(plugin.ID, state.ID)
	flowStatus := data.status
	if context.Layout.Render.Rows <= 18 {
		data.title = fmt.Sprintf("D%d %s · E%d %s · P%d %s", surface.design+1, design.shortName, surface.treatment+1, treatment.Name, surface.plugin+1, plugin.shortName)
	} else {
		data.title = plugin.Name + " · " + design.Name + " / " + treatment.Name
	}
	data.selected = surface.selected
	if surface.query != "" || surface.editing {
		data.query = surface.query
		if surface.editing {
			data.query += "▏"
		}
	}
	data.help = navigationHelp(context.Layout.Render.Columns)
	data.status = surface.status(context, state.Name, themeID)
	if flowStatus != "" && !surface.hud {
		data.status = flowStatus + " · " + data.status
	}
	spec := Spec{
		Design:   design,
		ThemeID:  themeID,
		Viewport: Viewport{ID: "live", Name: "Live", Width: context.Layout.Render.Columns, Height: context.Layout.Render.Rows},
		Scenario: Scenario{ID: state.ID, Name: state.Name, State: state.ID, Surfaces: []string{plugin.ID}},
	}
	return renderSample(spec, data, treatment)
}

func (surface *LiveSurface) status(context shell.RenderContext, state, themeID string) string {
	fixture := viewportFixtures[surface.fixture]
	if surface.hud {
		if context.Layout.Render.Columns < 100 || context.Layout.Render.Rows <= 18 {
			settled := 0
			if context.Settled {
				settled = 1
			}
			return fmt.Sprintf("%dx%d>%dx%d %s g%d set:%d F%d",
				context.Layout.Reported.Columns, context.Layout.Reported.Rows,
				context.Layout.Render.Columns, context.Layout.Render.Rows,
				context.Layout.Class, context.ResizeGeneration, settled, surface.fixture+1)
		}
		return fmt.Sprintf("HUD %dx%d→%dx%d %s gen:%d settled:%t · fixture:%s",
			context.Layout.Reported.Columns, context.Layout.Reported.Rows,
			context.Layout.Render.Columns, context.Layout.Render.Rows,
			context.Layout.Class, context.ResizeGeneration, context.Settled, fixture.Name)
	}
	fixtureLabel := fixture.Name
	if fixture.Columns > 0 {
		fixtureLabel = fmt.Sprintf("%s %dx%d", fixture.Name, fixture.Columns, fixture.Rows)
	}
	if context.Layout.Render.Rows <= 18 {
		return fmt.Sprintf("S%d %s · T%d %s · F%d %s", surface.state+1, state, surface.theme+1, themeID, surface.fixture+1, fixtureLabel)
	}
	return fmt.Sprintf("%s · %s · %s", state, themeID, fixtureLabel)
}

func navigationHelp(columns int) string {
	if columns < 70 {
		return "?:help  Tab:field  ←→:change  ↑↓:pick"
	}
	return "? help   Tab field   ←/→ change   ↑/↓ select   / search   Esc quit"
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
	lines := []string{
		"CATALOGUE HELP",
		"d/D design · e/E treatment",
		"p/P plugin · s/S state",
		"t/T theme · f/F fixture",
		"Tab field · ←→ change",
		"↑↓ select result",
		"/ search · Backspace delete/back",
		"Enter next / commit search",
		"h HUD · ? / Esc close help",
		"q / Ctrl-C quit",
	}
	for row, line := range lines {
		if row >= size.Rows {
			break
		}
		style := view.Style{Foreground: palette.Text, Background: palette.Background}
		if row == 0 {
			style.Foreground, style.Background, style.Bold = palette.Accent, palette.PanelBackground, true
			frame.Fill(0, row, size.Columns, 1, style)
		}
		frame.PutText(1, row, fit(line, max(0, size.Columns-2)), style)
	}
	return frame, nil
}

func (surface *LiveSurface) currentPlugin() PluginSurface { return pluginSurfaces[surface.plugin] }

func (surface *LiveSurface) DiagnosticState() diagnostics.VisualState {
	plugin := surface.currentPlugin()
	state := plugin.States[surface.state]
	viewState := "plain"
	if surface.hud {
		viewState = "hud"
	}
	if surface.help {
		viewState = "help"
		if surface.hud {
			viewState = "help-hud"
		}
	}
	return diagnostics.VisualState{
		Screen: mustID("catalogue." + plugin.ID), Focus: mustID(axisName(surface.active)),
		Selection: mustID(strings.Join([]string{liveDesigns[surface.design].ID, treatments[surface.treatment].ID, theme.IDs()[surface.theme]}, ".")),
		State:     mustID(strings.Join([]string{state.ID, viewportFixtures[surface.fixture].ID, viewState}, ".")),
		Geometry: diagnostics.Geometry{
			ReportedColumns: surface.layout.Reported.Columns, ReportedRows: surface.layout.Reported.Rows,
			RenderColumns: surface.layout.Render.Columns, RenderRows: surface.layout.Render.Rows,
		},
		ResizeGeneration: surface.resizeGeneration,
		ItemCount:        len(plugin.States), SelectedIndex: surface.selected,
		Pending: surface.editing, HasError: state.ID == "error" || state.ID == "validation-error",
	}
}

func axisName(value axis) string {
	return [...]string{"design", "treatment", "plugin", "state", "theme", "fixture"}[value]
}

func mustID(value string) diagnostics.ID {
	id, _ := diagnostics.NewID(value)
	return id
}

func liveSample(plugin, state string) sample {
	data := surfaceBase(plugin)
	switch state {
	case "loading", "switching", "opening", "running":
		data.status = "Working · progress 2/3"
		data.rows = []string{"* Request accepted", "* Provider responding", "  Final state pending", "  Cancel remains available", "  Existing state preserved", "  Diagnostics linked"}
		data.detail = []string{"Progress", "2 of 3 steps complete", "The current view stays usable"}
	case "empty":
		data.status = "0 results · clear filters"
		data.rows = []string{"No matches", "Try fewer terms", "Clear the current query", "Open help", "Recent items remain available", "Esc returns"}
		data.detail = []string{"Empty state", "No result is selected", "Clear query to continue"}
	case "error", "validation-error":
		data.status = "! Recoverable error · retry available"
		data.rows = []string{"! Request could not complete", "> Retry", "  Keep previous state", "  Open diagnostic event", "  Copy correlation ID", "  Esc returns"}
		data.detail = []string{"! Action required", "Previous valid data is unchanged", "Retry or inspect diagnostics"}
	case "switched":
		data.status = "OK Attention moved"
		data.detail = []string{"OK Active agent changed", "Previous task remains running", "Shortcut returns here"}
	case "reopened":
		data.status = "OK Session reopened in requested directory"
		data.detail = []string{"OK Session active", "Directory: <workspace>", "Previous view closed cleanly"}
	case "applied":
		data.status = "OK Configuration applied live"
		data.detail = []string{"OK TOML validated and applied", "Rollback point retained", "No restart required"}
	case "completed":
		data.status = "OK Action completed"
		data.detail = []string{"OK Plugin action returned", "Receipt correlated", "Result remains bounded"}
	case "event-detail":
		data.status = "Event detail · payload redacted"
		data.detail = []string{"resize.settled", "outcome: applied", "generation: 84", "geometry: 48x18", "payload: never recorded", "correlation: c-demo"}
	}
	return data
}

func surfaceBase(plugin string) sample {
	data := sample{selected: 1}
	switch plugin {
	case "recall-search":
		data.query = "responsive plugin decisions"
		data.rows = []string{"Memory · UI must use delivered size", "Decision · Bubble Tea owns lifecycle", "Session · mutation gate review", "Note · catalogue stays internal", "Memory · latest generation wins", "Decision · no keyboard heuristic"}
		data.detail = []string{"Recall result", "Source: Codex Recall", "Workspace: plugin kit", "Matched terms: responsive, UI", "Open source session", "Copy stable reference"}
	case "attention-switcher":
		data.query = "agent or task"
		data.rows = []string{"Arendt · reviewing shell", "Beauvoir · catalogue tests", "Anscombe · idle", "Singer · diagnostics", "Ohm · generated plugin", "Hubble · research"}
		data.detail = []string{"Selected agent", "Task: catalogue tests", "State: working", "Enter switches attention", "No task is cancelled"}
	case "session-switcher":
		data.query = "session or directory"
		data.rows = []string{"Plugin kit · /projects/herdr-plugin-kit", "Recall rebuild · /projects/recall", "Herdr core · /projects/herdr", "Diagnostics · /projects/plugin-kit", "Planning · /home/raul/planning", "Recent shell · /home/raul"}
		data.detail = []string{"Selected session", "Agent directory supplied", "Enter closes current view", "Then reopens selected session", "Shortcut available"}
	case "plugin-configurator":
		data.query = "Filter settings"
		data.rows = []string{"Theme              catppuccin", "Refresh interval   250ms", "Diagnostics        enabled", "Retention          14 days", "Shortcut           ctrl+r", "Live reload        enabled"}
		data.detail = []string{"Configuration", "TOML / YAML source", "Validate before apply", "Apply changes live", "Rollback on failure"}
	case "action-finder":
		data.query = "open diagnostic"
		data.rows = []string{"Debug UI: open timeline", "Recall: search memory", "Session: reopen here", "Attention: switch agent", "Config: validate", "Plugin: show health"}
		data.detail = []string{"Runnable action", "Provider: debug-ui", "Shortcut: Enter", "Version: v1", "Receipt is correlated"}
	case "debug-ui":
		data.query = "Filter semantic events"
		data.rows = []string{"OK Runtime healthy", "Resize · generation 84 settled", "Request · recall.search applied", "Storage · 18 MiB / 128 MiB", "Snapshot · semantic placeholder", "Export · report ready"}
		data.detail = []string{"Diagnostic event", "Every event has an outcome", "No user payload persisted", "Open resize flight recorder", "Inspect placeholder frame", "Export sanitized report"}
	}
	return data
}
