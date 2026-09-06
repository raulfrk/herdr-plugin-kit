// Package debugui provides the standard privacy-safe diagnostics surface.
package debugui

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

const resizeHistoryLimit = 16

type Exporter func(context.Context, []byte) error

type Options struct {
	Recorder        *diagnostics.Recorder
	Previews        *diagnostics.PreviewStore
	Exporter        Exporter
	MaxReportBytes  int
	PageSize        int
	RecordingStatus func() RecordingStatus
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
	snapshot         *diagnostics.DebugSnapshot
	view             *diagnostics.DebugView
	window           diagnostics.DebugWindow
	query            diagnostics.DebugQuery
	screen           screen
	previous         screen
	list             screen
	selected         int
	session          int
	level            int
	kind             int
	export           exportStatus
	projectionFailed bool
	editing          bool
	draft            string
	resizes          []resizeRecord
	frozen           bool
	loaded           bool
	hideDebugger     bool
	pending          projectionKind
	queued           projectionKind
	pendingRequest   projectionIdentity
	anchors          [2]inspectionAnchor
	detail           *diagnostics.DebugEvent
	evicted          bool
	visibleTop       [2]uint64
	displayedQuery   diagnostics.DebugQuery
	displayedList    screen
	projectionEpoch  uint64
}

type projectionKind uint8

const (
	projectionNone projectionKind = iota
	projectionSelect
	projectionAcquire
)

type projectionResult struct {
	snapshot *diagnostics.DebugSnapshot
	view     *diagnostics.DebugView
	query    diagnostics.DebugQuery
	kind     projectionKind
	list     screen
	epoch    uint64
}
type projectionIdentity struct {
	query diagnostics.DebugQuery
	kind  projectionKind
	list  screen
	epoch uint64
}
type inspectionAnchor struct {
	selected, top uint64
	start         int
	query         diagnostics.DebugQuery
	queryOwned    bool
}

var exportRequest, _ = shell.NewRequestKey("debug.export")
var projectionRequest, _ = shell.NewRequestKey("debug.snapshot")
var maintenanceCode, _ = shell.NewEventCode("debug.maintenance")

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
	surface := &Surface{options: options, screen: screenHealth, list: screenTimeline, hideDebugger: true, query: diagnostics.DebugQuery{PageSize: options.PageSize, HideDebugger: true}}
	if options.Exporter == nil {
		surface.export = exportDisabled
	}
	return surface, nil
}

func (surface *Surface) Update(events shell.EventContext, event shell.Event) []shell.Effect {
	switch event := event.(type) {
	case shell.ResizeEvent:
		if event.Generation <= surface.resizeGeneration {
			return nil
		}
		surface.layout = event.Layout
		surface.resizeGeneration = event.Generation
		surface.resizes = append(surface.resizes, resizeRecord{Layout: event.Layout, Generation: event.Generation})
		if len(surface.resizes) > resizeHistoryLimit {
			copy(surface.resizes, surface.resizes[len(surface.resizes)-resizeHistoryLimit:])
			surface.resizes = surface.resizes[:resizeHistoryLimit]
		}
		var effects []shell.Effect
		if !surface.loaded {
			effects = append(effects, surface.requestProjection(events, projectionAcquire)...)
		}
		effect, err := events.After(500*time.Millisecond, maintenanceCode)
		if err == nil {
			effects = append(effects, effect)
		}
		return effects
	case shell.TimerEvent:
		if event.Code == maintenanceCode {
			effects := []shell.Effect{}
			if !surface.frozen {
				effects = append(effects, surface.requestProjection(events, projectionAcquire)...)
			}
			effect, err := events.After(500*time.Millisecond, maintenanceCode)
			if err == nil {
				effects = append(effects, effect)
			}
			return effects
		}
	case shell.ResultEvent:
		if event.Key == projectionRequest {
			return surface.applyProjection(events, event.Result)
		}
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
	return nil
}

func (surface *Surface) SuppressEventDiagnostics(event shell.Event) bool {
	switch event := event.(type) {
	case shell.TimerEvent:
		return event.Code == maintenanceCode
	case shell.ResultEvent:
		return event.Key == projectionRequest
	default:
		return false
	}
}
func (surface *Surface) SuppressEffectDiagnostics(effect shell.Effect) bool {
	return effect.RequestKey() == projectionRequest || effect.EventCode() == maintenanceCode
}

func (surface *Surface) key(events shell.EventContext, key shell.KeyCode) []shell.Effect {
	if surface.editing {
		changed := false
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
			surface.saveAnchor()
			if surface.draft == "" {
				surface.query.Code = diagnostics.ID{}
			} else if code, err := diagnostics.NewID(surface.draft); err == nil {
				surface.query.Code = code
			}
			surface.query.Page = 0
			surface.editing = false
			changed = true
		}
		if changed {
			return surface.requestProjection(events, projectionSelect)
		}
		return nil
	}
	if !surface.loaded && key != shell.KeyCtrlC && key != shell.KeySpace && key != shell.KeyEscape && key != shell.KeyBackspace {
		return nil
	}
	switch key {
	case shell.KeyCtrlC:
		return []shell.Effect{shell.Quit()}
	case shell.KeySpace:
		return surface.toggleFreeze(events)
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
		if indices := surface.galleryIndices(); surface.list == screenGallery && len(indices) > 0 {
			surface.selected = indices[0]
		} else {
			surface.selected = 0
		}
	case shell.KeyEnd:
		if indices := surface.galleryIndices(); surface.list == screenGallery && len(indices) > 0 {
			surface.selected = indices[len(indices)-1]
		} else if len(surface.page.Events) > 0 {
			surface.selected = len(surface.page.Events) - 1
		}
	case shell.KeyPageUp:
		if surface.page.HasPrev {
			surface.pageTo(max(0, surface.window.Start-surface.query.PageSize))
		}
	case shell.KeyPageDown:
		if surface.page.HasNext {
			surface.pageTo(surface.window.Start + surface.query.PageSize)
		}
	case shell.KeyLeft:
		surface.saveAnchor()
		surface.changeSession(-1)
		return surface.requestProjection(events, projectionSelect)
	case shell.KeyRight:
		surface.saveAnchor()
		surface.changeSession(1)
		return surface.requestProjection(events, projectionSelect)
	case shell.KeyEnter:
		if surface.screen == screenTimeline || surface.screen == screenGallery {
			surface.saveAnchor()
			surface.openDetail()
		}
	}
	surface.saveAnchor()
	return nil
}

func (surface *Surface) text(events shell.EventContext, value byte) []shell.Effect {
	if !surface.loaded && strings.ContainsRune("slk01234c/e", rune(value)) {
		return nil
	}
	filterChanged := false
	viewKindChanged := false
	if strings.ContainsRune("vslk01234c", rune(value)) {
		surface.saveAnchor()
	}
	switch value {
	case 'q':
		return []shell.Effect{shell.Quit()}
	case 'p':
		return surface.toggleFreeze(events)
	case 'v':
		surface.hideDebugger = !surface.hideDebugger
		surface.query.HideDebugger = surface.hideDebugger
		surface.query.Page = 0
		filterChanged = true
	case '?':
		if surface.screen == screenHelp {
			surface.screen = surface.previous
		} else {
			surface.previous, surface.screen = surface.screen, screenHelp
		}
	case 'h':
		surface.saveAnchor()
		surface.screen = screenHealth
	case 't':
		surface.saveAnchor()
		viewKindChanged = surface.list != screenTimeline
		surface.list = screenTimeline
		surface.screen = screenTimeline
	case 'g':
		surface.saveAnchor()
		viewKindChanged = surface.list != screenGallery
		surface.list = screenGallery
		surface.screen = screenGallery
	case 'r':
		if surface.projectionFailed {
			surface.projectionFailed = false
			return surface.requestProjection(events, projectionAcquire)
		} else {
			surface.screen = screenHUD
		}
	case 'e':
		return surface.startExport(events)
	case 's':
		surface.changeSession(1)
		filterChanged = true
	case 'l':
		surface.cycleLevel()
		filterChanged = true
	case 'k':
		surface.cycleKind()
		filterChanged = true
	case '0':
		surface.clearFilters()
		filterChanged = true
	case '1':
		surface.filterSelected(1)
		filterChanged = true
	case '2':
		surface.filterSelected(2)
		filterChanged = true
	case '3':
		surface.filterSelected(3)
		filterChanged = true
	case '4', 'c':
		surface.filterSelected(4)
		filterChanged = true
	case '/':
		surface.editing = true
		surface.draft = surface.query.Code.String()
	}
	if filterChanged {
		return surface.requestProjection(events, projectionSelect)
	}
	if viewKindChanged {
		return surface.requestProjection(events, projectionSelect)
	}
	surface.saveAnchor()
	return nil
}

func (surface *Surface) toggleFreeze(events shell.EventContext) []shell.Effect {
	if surface.loaded {
		surface.projectionEpoch++
	}
	surface.frozen = !surface.frozen
	if surface.frozen {
		if surface.queued == projectionAcquire {
			if surface.snapshot == nil {
				return nil
			} else if surface.displayedQuery != surface.query || surface.displayedList != surface.list {
				surface.queued = projectionSelect
			} else {
				surface.queued = projectionNone
			}
		}
		return nil
	}
	return surface.requestProjection(events, projectionAcquire)
}

func (surface *Surface) appendDraft(text string) {
	for _, value := range []byte(text) {
		if len(surface.draft) == 64 {
			return
		}
		if validDraftByte(value, len(surface.draft) == 0) {
			surface.draft += string(value)
		}
	}
}

func validDraftByte(value byte, first bool) bool {
	if value >= 'a' && value <= 'z' {
		return true
	}
	if first {
		return false
	}
	if value >= '0' && value <= '9' {
		return true
	}
	return value == '.' || value == '_' || value == '-'
}

func (surface *Surface) startExport(events shell.EventContext) []shell.Effect {
	if surface.options.Exporter == nil || surface.export == exportPending || surface.view == nil {
		return nil
	}
	surface.export = exportPending
	view, window, limit, previews, exporter := surface.view, surface.window, surface.options.MaxReportBytes, surface.options.Previews, surface.options.Exporter
	effect, err := events.Start(exportRequest, func(ctx context.Context) shell.WorkResult {
		data, exportErr := view.ExportWindow(ctx, window, limit, previews)
		if exportErr == nil {
			exportErr = exporter(ctx, data)
		}
		if exportErr != nil {
			return shell.WorkResult{Code: diagnostics.OutcomeFailed, Err: errors.New("debug export failed")}
		}
		return shell.WorkResult{Code: diagnostics.OutcomeApplied}
	})
	if err != nil {
		surface.export = exportFailed
		return nil
	}
	return []shell.Effect{effect}
}

func (surface *Surface) requestProjection(events shell.EventContext, kind projectionKind) []shell.Effect {
	if surface.pending != projectionNone {
		if kind > surface.queued {
			surface.queued = kind
		}
		return nil
	}
	if kind == projectionSelect && surface.snapshot == nil {
		kind = projectionAcquire
	}
	surface.pending = kind
	query, snapshot, recorder, previews, requestedList, epoch := surface.query, surface.snapshot, surface.options.Recorder, surface.options.Previews, surface.list, surface.projectionEpoch
	surface.pendingRequest = projectionIdentity{query: query, kind: kind, list: requestedList, epoch: epoch}
	effect, err := events.Start(projectionRequest, func(ctx context.Context) shell.WorkResult {
		if kind == projectionAcquire {
			var acquireErr error
			snapshot, acquireErr = recorder.DebugSnapshot(ctx, previews)
			if acquireErr != nil {
				return shell.WorkResult{Code: diagnostics.OutcomeFailed, Err: errors.New("debug snapshot failed")}
			}
		}
		view, selectErr := snapshot.Select(ctx, query, requestedList == screenGallery)
		if selectErr != nil {
			return shell.WorkResult{Code: diagnostics.OutcomeFailed, Err: errors.New("debug selection failed")}
		}
		return shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: snapshot, view: view, query: query, kind: kind, list: requestedList, epoch: epoch}}
	})
	if err != nil {
		surface.pending = projectionNone
		surface.pendingRequest = projectionIdentity{}
		surface.projectionFailed = true
		return nil
	}
	return []shell.Effect{effect}
}

func (surface *Surface) applyProjection(events shell.EventContext, result shell.WorkResult) []shell.Effect {
	kind := surface.pending
	request := surface.pendingRequest
	if request.kind == projectionNone {
		if value, ok := result.Value.(projectionResult); ok {
			request = projectionIdentity{query: value.query, kind: value.kind, list: value.list, epoch: value.epoch}
		} else {
			request = projectionIdentity{query: surface.query, kind: kind, list: surface.list, epoch: surface.projectionEpoch}
		}
	}
	surface.pending = projectionNone
	surface.pendingRequest = projectionIdentity{}
	validOwner := request.query == surface.query && request.list == surface.list && !(request.kind == projectionAcquire && request.epoch != surface.projectionEpoch)
	if (result.Err != nil || result.Code != diagnostics.OutcomeApplied) && validOwner {
		surface.projectionFailed = true
	} else if result.Err == nil && result.Code == diagnostics.OutcomeApplied {
		value, ok := result.Value.(projectionResult)
		if !ok {
			if validOwner {
				surface.projectionFailed = true
			}
			return surface.followProjection(events)
		}
		validOwner = validOwner && value.query == request.query && value.kind == request.kind && value.list == request.list && value.epoch == request.epoch
		if !validOwner {
			if !surface.frozen && kind > surface.queued {
				surface.queued = kind
			}
			return surface.followProjection(events)
		}
		if !value.query.Session.IsZero() {
			found := false
			for _, session := range value.view.Sessions() {
				if session.ID == value.query.Session {
					found = true
					break
				}
			}
			if !found {
				surface.query.Session = diagnostics.ID{}
				surface.session = 0
				surface.snapshot = value.snapshot
				if projectionSelect > surface.queued {
					surface.queued = projectionSelect
				}
				return surface.followProjection(events)
			}
		}
		anchor := &surface.anchors[surface.anchorIndex()]
		filterChanged := anchor.queryOwned && anchor.query != value.query
		surface.snapshot = value.snapshot
		surface.view = value.view
		surface.loaded = true
		surface.projectionFailed = false
		surface.displayedQuery = value.query
		surface.displayedList = value.list
		surface.rebuildWindow(filterChanged)
	}
	return surface.followProjection(events)
}

func (surface *Surface) followProjection(events shell.EventContext) []shell.Effect {
	next := surface.queued
	surface.queued = projectionNone
	if surface.frozen && next == projectionAcquire {
		if surface.snapshot == nil {
			// The first current projection is still required to finish loading.
		} else if surface.displayedQuery != surface.query || surface.displayedList != surface.list {
			next = projectionSelect
		} else {
			next = projectionNone
		}
	}
	if next != projectionNone {
		return surface.requestProjection(events, next)
	}
	return nil
}

func (surface *Surface) anchorIndex() int {
	if surface.list == screenGallery {
		return 1
	}
	return 0
}
func (surface *Surface) saveAnchor() {
	if surface.view == nil || surface.displayedQuery != surface.query || surface.displayedList != surface.list {
		return
	}
	index := surface.anchorIndex()
	anchor := &surface.anchors[index]
	if event := surface.listSelectedEvent(); event != nil {
		anchor.selected = event.Sequence
	}
	if surface.visibleTop[index] != 0 {
		anchor.top = surface.visibleTop[index]
	}
	anchor.start = surface.window.Start
	anchor.query = surface.displayedQuery
	anchor.queryOwned = true
}
func (surface *Surface) rebuildWindow(filterChanged bool) {
	if surface.view == nil {
		return
	}
	index := surface.anchorIndex()
	anchor := &surface.anchors[index]
	selectedPosition, selectedExact := surface.view.Locate(anchor.selected)
	topPosition, topExact := surface.view.Locate(anchor.top)
	anchorMissing := anchor.selected != 0 && !selectedExact || anchor.top != 0 && !topExact
	surface.evicted = !filterChanged && anchorMissing
	if filterChanged && anchor.selected != 0 && !selectedExact {
		anchor.selected, anchor.top, anchor.start = 0, 0, 0
		selectedPosition, topPosition = -1, -1
	}
	start := anchor.start
	if anchor.top != 0 && topPosition >= 0 {
		start = topPosition
	}
	if anchor.selected != 0 && selectedPosition >= 0 {
		if selectedPosition < start {
			start = selectedPosition
		} else if selectedPosition >= start+surface.query.PageSize {
			start = selectedPosition - surface.query.PageSize + 1
		}
	}
	window := surface.view.Window(diagnostics.DebugWindowQuery{Start: start})
	if len(window.Events) == 0 {
		surface.selected = 0
		anchor.selected = 0
		anchor.top = 0
		anchor.start = 0
	} else {
		position, _ := surface.view.Locate(anchor.selected)
		if anchor.selected == 0 {
			position = window.Start
		}
		if position < window.Start || position >= window.Start+len(window.Events) {
			position = window.Start
		}
		surface.selected = position - window.Start
		anchor.selected = window.Events[surface.selected].Sequence
		anchor.start = window.Start
	}
	surface.window = window
	surface.page = diagnostics.DebugPage{Sessions: surface.view.Sessions(), Health: surface.view.Health(), Events: window.Events, Page: window.Start / surface.query.PageSize, PageSize: surface.query.PageSize, Total: window.Total, HasPrev: window.HasPrev, HasNext: window.HasNext}
	if surface.list == screenGallery {
		surface.ensureGallerySelection()
	}
	anchor.query = surface.displayedQuery
	anchor.queryOwned = true
}

func (surface *Surface) pageTo(start int) {
	if surface.view == nil || surface.displayedQuery != surface.query || surface.displayedList != surface.list {
		return
	}
	window := surface.view.Window(diagnostics.DebugWindowQuery{Start: start})
	anchor := &surface.anchors[surface.anchorIndex()]
	anchor.selected, anchor.top, anchor.start = 0, 0, start
	if len(window.Events) != 0 {
		anchor.selected, anchor.top, anchor.start = window.Events[0].Sequence, window.Events[0].Sequence, window.Start
	}
	surface.rebuildWindow(false)
}

func (surface *Surface) move(delta int) {
	if surface.list == screenGallery {
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
		if event.Visual != nil && surface.view != nil && surface.view.PreviewEligible(event.Sequence) {
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
	surface.query.Page = 0
}

func (surface *Surface) cycleLevel() {
	levels := []diagnostics.Level{"", diagnostics.LevelDebug, diagnostics.LevelInfo, diagnostics.LevelWarn, diagnostics.LevelError}
	surface.level = (surface.level + 1) % len(levels)
	surface.query.Level = levels[surface.level]
	surface.query.Page = 0
}

func (surface *Surface) cycleKind() {
	kinds := []diagnostics.Kind{"", diagnostics.KindLifecycle, diagnostics.KindInteraction, diagnostics.KindDiagnostic}
	surface.kind = (surface.kind + 1) % len(kinds)
	surface.query.Kind = kinds[surface.kind]
	surface.query.Page = 0
}

func (surface *Surface) filterSelected(field int) {
	event := surface.listSelectedEvent()
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
	surface.query.Page = 0
}

func (surface *Surface) clearFilters() {
	pageSize := surface.query.PageSize
	surface.query = diagnostics.DebugQuery{PageSize: pageSize, HideDebugger: surface.hideDebugger}
	surface.session, surface.level, surface.kind = 0, 0, 0
}

func (surface *Surface) openDetail() {
	event := surface.listSelectedEvent()
	if event == nil {
		return
	}
	copy := *event
	if event.Visual != nil {
		visual := *event.Visual
		copy.Visual = &visual
	}
	surface.detail = &copy
	surface.previous, surface.screen = surface.screen, screenDetail
}

func (surface *Surface) selectedEvent() *diagnostics.DebugEvent {
	if surface.screen == screenDetail && surface.detail != nil {
		return surface.detail
	}
	return surface.listSelectedEvent()
}

func (surface *Surface) listSelectedEvent() *diagnostics.DebugEvent {
	if surface.selected < 0 || surface.selected >= len(surface.page.Events) {
		return nil
	}
	return &surface.page.Events[surface.selected]
}

func (surface *Surface) Render(context shell.RenderContext) (*view.Frame, error) {
	return surface.render(context)
}

// Editing reports whether plain text is currently routed into the filter.
func (surface *Surface) Editing() bool { return surface.editing }

func (surface *Surface) DiagnosticState() diagnostics.VisualState {
	state := "ready"
	if !surface.loaded {
		state = "loading"
	} else if surface.projectionFailed {
		state = "projection-failed"
	} else if surface.pending == projectionAcquire {
		state = "loading"
	} else if surface.pending == projectionSelect {
		state = "filtering"
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
		ItemCount: len(surface.page.Events), SelectedIndex: surface.selected, Pending: surface.pending != projectionNone || surface.export == exportPending || surface.editing, HasError: surface.projectionFailed || surface.export == exportFailed,
	}
}

func (value screen) String() string {
	return []string{"health", "timeline", "gallery", "hud", "detail", "help"}[value]
}

func id(value string) diagnostics.ID {
	result, _ := diagnostics.NewID(value)
	return result
}
