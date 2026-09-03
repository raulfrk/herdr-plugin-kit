// Package debugui provides the standard privacy-safe diagnostics surface.
package debugui

import (
	"context"
	"errors"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

const resizeHistoryLimit = 16

type Exporter func(context.Context, []byte) error

type Options struct {
	Recorder       *diagnostics.Recorder
	Previews       *diagnostics.PreviewStore
	Exporter       Exporter
	MaxReportBytes int
	PageSize       int
}

type screen uint8

const (
	screenHealth screen = iota
	screenTimeline
	screenGallery
	screenHUD
	screenDetail
	screenHelp
)

type exportStatus uint8

const (
	exportReady exportStatus = iota
	exportPending
	exportSucceeded
	exportFailed
	exportDisabled
)

type resizeRecord struct {
	Layout     responsive.Layout
	Generation uint64
}

type Surface struct {
	options          Options
	layout           responsive.Layout
	resizeGeneration uint64
	page             diagnostics.DebugPage
	query            diagnostics.DebugQuery
	screen           screen
	previous         screen
	selected         int
	session          int
	level            int
	kind             int
	export           exportStatus
	projectionFailed bool
	editing          bool
	draft            string
	resizes          []resizeRecord
}

var exportRequest, _ = shell.NewRequestKey("debug.export")

var _ shell.Surface = (*Surface)(nil)

func New(options Options) (*Surface, error) {
	if options.Recorder == nil {
		return nil, errors.New("debug recorder is nil")
	}
	if options.PageSize == 0 {
		options.PageSize = diagnostics.DefaultDebugPageSize
	}
	if options.PageSize < 1 || options.PageSize > diagnostics.MaxDebugPageSize {
		return nil, errors.New("debug page size must be between 1 and 100")
	}
	surface := &Surface{options: options, screen: screenHealth, query: diagnostics.DebugQuery{PageSize: options.PageSize}}
	if options.Exporter == nil {
		surface.export = exportDisabled
	}
	surface.refresh()
	return surface, nil
}

func (surface *Surface) Update(events shell.EventContext, event shell.Event) []shell.Effect {
	switch event := event.(type) {
	case shell.ResizeEvent:
		if event.Generation < surface.resizeGeneration {
			return nil
		}
		surface.layout = event.Layout
		surface.resizeGeneration = event.Generation
		surface.resizes = append(surface.resizes, resizeRecord{Layout: event.Layout, Generation: event.Generation})
		if len(surface.resizes) > resizeHistoryLimit {
			copy(surface.resizes, surface.resizes[len(surface.resizes)-resizeHistoryLimit:])
			surface.resizes = surface.resizes[:resizeHistoryLimit]
		}
	case shell.ResultEvent:
		if event.Key == exportRequest {
			if event.Result.Err != nil || event.Result.Code != diagnostics.OutcomeApplied {
				surface.export = exportFailed
			} else {
				surface.export = exportSucceeded
			}
		}
	case shell.KeyEvent:
		return surface.key(events, event.Code)
	case shell.TextEvent:
		if surface.editing {
			surface.appendDraft(event.Text)
			return nil
		}
		if len(event.Text) == 1 {
			return surface.text(events, event.Text[0])
		}
	}
	if !surface.projectionFailed {
		surface.refresh()
	}
	return nil
}

func (surface *Surface) key(events shell.EventContext, key shell.KeyCode) []shell.Effect {
	if surface.editing {
		switch key {
		case shell.KeyCtrlC:
			return []shell.Effect{shell.Quit()}
		case shell.KeyEscape:
			surface.editing = false
			surface.draft = ""
		case shell.KeyBackspace:
			if len(surface.draft) > 0 {
				surface.draft = surface.draft[:len(surface.draft)-1]
			}
		case shell.KeyEnter:
			if surface.draft == "" {
				surface.query.Code = diagnostics.ID{}
			} else if code, err := diagnostics.NewID(surface.draft); err == nil {
				surface.query.Code = code
			}
			surface.query.Page, surface.selected = 0, 0
			surface.editing = false
		}
		if !surface.projectionFailed {
			surface.refresh()
		}
		return nil
	}
	switch key {
	case shell.KeyCtrlC:
		return []shell.Effect{shell.Quit()}
	case shell.KeyEscape, shell.KeyBackspace:
		if surface.screen == screenDetail || surface.screen == screenHelp {
			surface.screen = surface.previous
		} else {
			return []shell.Effect{shell.Quit()}
		}
	case shell.KeyUp:
		surface.move(-1)
	case shell.KeyDown:
		surface.move(1)
	case shell.KeyHome:
		if indices := surface.galleryIndices(); surface.screen == screenGallery && len(indices) > 0 {
			surface.selected = indices[0]
		} else {
			surface.selected = 0
		}
	case shell.KeyEnd:
		if indices := surface.galleryIndices(); surface.screen == screenGallery && len(indices) > 0 {
			surface.selected = indices[len(indices)-1]
		} else if len(surface.page.Events) > 0 {
			surface.selected = len(surface.page.Events) - 1
		}
	case shell.KeyPageUp:
		if surface.page.HasPrev {
			surface.query.Page--
			surface.selected = 0
		}
	case shell.KeyPageDown:
		if surface.page.HasNext {
			surface.query.Page++
			surface.selected = 0
		}
	case shell.KeyLeft:
		surface.changeSession(-1)
	case shell.KeyRight:
		surface.changeSession(1)
	case shell.KeyEnter:
		if surface.screen == screenTimeline || surface.screen == screenGallery {
			surface.openDetail()
		}
	}
	if !surface.projectionFailed {
		surface.refresh()
	}
	return nil
}

func (surface *Surface) text(events shell.EventContext, value byte) []shell.Effect {
	switch value {
	case 'q':
		return []shell.Effect{shell.Quit()}
	case '?':
		if surface.screen == screenHelp {
			surface.screen = surface.previous
		} else {
			surface.previous, surface.screen = surface.screen, screenHelp
		}
	case 'h':
		surface.screen = screenHealth
	case 't':
		surface.screen = screenTimeline
	case 'g':
		surface.screen = screenGallery
		surface.ensureGallerySelection()
	case 'r':
		if surface.projectionFailed {
			surface.projectionFailed = false
			surface.refresh()
			return nil
		} else {
			surface.screen = screenHUD
		}
	case 'e':
		return surface.startExport(events)
	case 's':
		surface.changeSession(1)
	case 'l':
		surface.cycleLevel()
	case 'k':
		surface.cycleKind()
	case '0':
		surface.clearFilters()
	case '1':
		surface.filterSelected(1)
	case '2':
		surface.filterSelected(2)
	case '3':
		surface.filterSelected(3)
	case '4', 'c':
		surface.filterSelected(4)
	case '/':
		surface.editing = true
		surface.draft = surface.query.Code.String()
	}
	if !surface.projectionFailed {
		surface.refresh()
	}
	return nil
}

func (surface *Surface) appendDraft(text string) {
	for _, value := range []byte(text) {
		if len(surface.draft) == 64 {
			return
		}
		first := len(surface.draft) == 0
		if (first && value >= 'a' && value <= 'z') || (!first && ((value >= 'a' && value <= 'z') || (value >= '0' && value <= '9') || value == '.' || value == '_' || value == '-')) {
			surface.draft += string(value)
		}
	}
}

func (surface *Surface) startExport(events shell.EventContext) []shell.Effect {
	if surface.options.Exporter == nil || surface.export == exportPending {
		return nil
	}
	surface.export = exportPending
	effect, err := events.Start(exportRequest, surface.exportWork())
	if err != nil {
		surface.export = exportFailed
		return nil
	}
	return []shell.Effect{effect}
}

func (surface *Surface) exportWork() shell.Work {
	options, recorder, exporter := surface.options, surface.options.Recorder, surface.options.Exporter
	return func(ctx context.Context) shell.WorkResult {
		data, exportErr := recorder.ExportDebug(diagnostics.DebugExportOptions{
			Query: surface.query, MaxBytes: options.MaxReportBytes, Previews: options.Previews,
		})
		if exportErr == nil {
			exportErr = exporter(ctx, data)
		}
		if exportErr != nil {
			return shell.WorkResult{Code: diagnostics.OutcomeFailed, Err: errors.New("debug export failed")}
		}
		return shell.WorkResult{Code: diagnostics.OutcomeApplied}
	}
}

func (surface *Surface) refresh() {
	page, err := surface.options.Recorder.Debug(surface.query)
	if err != nil {
		surface.projectionFailed = true
		return
	}
	surface.page = page
	if !surface.query.Session.IsZero() {
		found := false
		for index, session := range page.Sessions {
			if session.ID == surface.query.Session {
				surface.session, found = index+1, true
				break
			}
		}
		if !found {
			surface.session = 0
			surface.query.Session = diagnostics.ID{}
		}
	}
	if surface.selected >= len(page.Events) {
		surface.selected = max(0, len(page.Events)-1)
	}
	if surface.screen == screenGallery {
		surface.ensureGallerySelection()
	}
}

func (surface *Surface) move(delta int) {
	if surface.screen == screenGallery {
		indices := surface.galleryIndices()
		if len(indices) == 0 {
			surface.selected = 0
			return
		}
		position := 0
		for index, candidate := range indices {
			if candidate == surface.selected {
				position = index
				break
			}
		}
		position = min(max(position+delta, 0), len(indices)-1)
		surface.selected = indices[position]
		return
	}
	if len(surface.page.Events) == 0 {
		surface.selected = 0
		return
	}
	surface.selected = min(max(surface.selected+delta, 0), len(surface.page.Events)-1)
}

func (surface *Surface) galleryIndices() []int {
	indices := make([]int, 0, len(surface.page.Events))
	for index, event := range surface.page.Events {
		if event.Visual != nil && surface.options.Previews != nil {
			if _, ok := surface.options.Previews.Entry(event.Sequence, *event.Visual); !ok {
				continue
			}
			indices = append(indices, index)
		}
	}
	return indices
}

func (surface *Surface) ensureGallerySelection() {
	indices := surface.galleryIndices()
	if len(indices) == 0 {
		surface.selected = 0
		return
	}
	for _, index := range indices {
		if index == surface.selected {
			return
		}
	}
	surface.selected = indices[0]
}

func (surface *Surface) changeSession(delta int) {
	count := len(surface.page.Sessions) + 1
	if count == 1 {
		return
	}
	surface.session = (surface.session + delta + count) % count
	if surface.session == 0 {
		surface.query.Session = diagnostics.ID{}
	} else {
		surface.query.Session = surface.page.Sessions[surface.session-1].ID
	}
	surface.query.Page, surface.selected = 0, 0
}

func (surface *Surface) cycleLevel() {
	levels := []diagnostics.Level{"", diagnostics.LevelDebug, diagnostics.LevelInfo, diagnostics.LevelWarn, diagnostics.LevelError}
	surface.level = (surface.level + 1) % len(levels)
	surface.query.Level = levels[surface.level]
	surface.query.Page, surface.selected = 0, 0
}

func (surface *Surface) cycleKind() {
	kinds := []diagnostics.Kind{"", diagnostics.KindLifecycle, diagnostics.KindInteraction, diagnostics.KindDiagnostic}
	surface.kind = (surface.kind + 1) % len(kinds)
	surface.query.Kind = kinds[surface.kind]
	surface.query.Page, surface.selected = 0, 0
}

func (surface *Surface) filterSelected(field int) {
	event := surface.selectedEvent()
	if event == nil {
		return
	}
	switch field {
	case 1:
		surface.query.Component = event.Component
	case 2:
		surface.query.Action = event.Action
	case 3:
		surface.query.Code = event.Code
	case 4:
		surface.query.Correlation = event.Correlation
	}
	surface.query.Page, surface.selected = 0, 0
}

func (surface *Surface) clearFilters() {
	pageSize := surface.query.PageSize
	surface.query = diagnostics.DebugQuery{PageSize: pageSize}
	surface.session, surface.level, surface.kind, surface.selected = 0, 0, 0, 0
}

func (surface *Surface) openDetail() {
	if surface.selectedEvent() == nil {
		return
	}
	surface.previous, surface.screen = surface.screen, screenDetail
}

func (surface *Surface) selectedEvent() *diagnostics.DebugEvent {
	if surface.selected < 0 || surface.selected >= len(surface.page.Events) {
		return nil
	}
	return &surface.page.Events[surface.selected]
}

func (surface *Surface) Render(context shell.RenderContext) (*view.Frame, error) {
	return surface.render(context)
}

func (surface *Surface) DiagnosticState() diagnostics.VisualState {
	state := "ready"
	if surface.projectionFailed {
		state = "projection-failed"
	} else if surface.export == exportPending {
		state = "export-pending"
	}
	selection := diagnostics.ID{}
	if event := surface.selectedEvent(); event != nil {
		selection = event.Code
	}
	return diagnostics.VisualState{
		Screen: id("debug." + surface.screen.String()), Focus: id("debug.navigation"), Selection: selection, State: id(state),
		Geometry:  diagnostics.Geometry{ReportedColumns: surface.layout.Reported.Columns, ReportedRows: surface.layout.Reported.Rows, RenderColumns: surface.layout.Render.Columns, RenderRows: surface.layout.Render.Rows},
		ItemCount: len(surface.page.Events), SelectedIndex: surface.selected, Pending: surface.export == exportPending || surface.editing, HasError: surface.projectionFailed || surface.export == exportFailed,
	}
}

func (value screen) String() string {
	return []string{"health", "timeline", "gallery", "hud", "detail", "help"}[value]
}

func id(value string) diagnostics.ID {
	result, _ := diagnostics.NewID(value)
	return result
}
