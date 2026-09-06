package debugui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
	"pgregory.net/rapid"
)

func TestUIExportFallsBackByOmittingOversizedSessionIndex(t *testing.T) {
	recorder := uiRecorder(t)
	for index := range 40 {
		addDetailedUIEvent(t, recorder, fmt.Sprintf("session-%02d-with-bounded-padding", index), "results", "open", "event", "", diagnostics.LevelInfo, diagnostics.KindDiagnostic, nil)
	}
	exported := make(chan []byte, 1)
	surface, err := New(Options{Recorder: recorder, PageSize: 100, MaxReportBytes: 1024, Exporter: func(_ context.Context, data []byte) error {
		exported <- append([]byte(nil), data...)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	input, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	palette, _ := theme.Builtin("catppuccin")
	program, err := shell.NewProgram(shell.ProgramOptions{PluginID: uiID(t, "plugin-a"), Theme: palette, Events: recorder, Input: input, Output: io.Discard}, surface)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, runErr := program.Run(); done <- runErr }()
	t.Cleanup(func() { program.Kill(); <-done })
	program.Send(tea.WindowSizeMsg{Width: 80, Height: 18})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		page, pageErr := recorder.Debug(diagnostics.DebugQuery{PageSize: 100})
		if pageErr != nil {
			t.Fatal(pageErr)
		}
		ready := false
		for _, event := range page.Events {
			if event.Visual != nil && event.Visual.Screen.String() == "debug.health" && event.Visual.State.String() == "ready" {
				ready = true
				break
			}
		}
		if ready {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	select {
	case data := <-exported:
		var report diagnostics.DebugReport
		if len(data) > 1024 || !json.Valid(data) {
			t.Fatalf("export bytes=%d valid=%t", len(data), json.Valid(data))
		}
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatal(err)
		}
		if len(report.Sessions) != 0 || !slices.Contains(report.TruncationReasons, "session_index_omitted") {
			t.Fatalf("UI export fallback=%+v", report)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("UI export did not complete")
	}
}

func uiID(t testing.TB, value string) diagnostics.ID {
	t.Helper()
	id, err := diagnostics.NewID(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func uiRecorder(t testing.TB) *diagnostics.Recorder {
	t.Helper()
	recorder, err := diagnostics.Open(diagnostics.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	return recorder
}

type synchronousTestSurface struct{ *Surface }

func (surface *synchronousTestSurface) Update(events shell.EventContext, event shell.Event) []shell.Effect {
	effects := surface.Surface.Update(events, event)
	surface.refresh()
	return effects
}

type observedEffect struct {
	request    shell.RequestKey
	event      shell.EventCode
	suppressed bool
}

type observedUpdate struct {
	event         shell.Event
	effects       []observedEffect
	nonReservedOK bool
	observedAt    time.Time
}

type maintenanceObservingSurface struct {
	*Surface
	updates chan observedUpdate
}

func (surface *maintenanceObservingSurface) Update(events shell.EventContext, event shell.Event) []shell.Effect {
	effects := surface.Surface.Update(events, event)
	observation := observedUpdate{event: event, observedAt: time.Now(), nonReservedOK: true}
	for _, effect := range effects {
		observation.effects = append(observation.effects, observedEffect{
			request: effect.RequestKey(), event: effect.EventCode(), suppressed: surface.SuppressEffectDiagnostics(effect),
		})
	}
	if _, ok := event.(shell.ResizeEvent); ok {
		otherRequest, _ := shell.NewRequestKey("other.request")
		otherTimer, _ := shell.NewEventCode("other.timer")
		work, workErr := events.Start(otherRequest, func(context.Context) shell.WorkResult { return shell.WorkResult{Code: diagnostics.OutcomeApplied} })
		timer, timerErr := events.After(10*time.Second, otherTimer)
		observation.nonReservedOK = workErr == nil && timerErr == nil && !surface.SuppressEffectDiagnostics(work) && !surface.SuppressEffectDiagnostics(timer)
	}
	surface.updates <- observation
	return effects
}

func (surface *maintenanceObservingSurface) Render(context shell.RenderContext) (*view.Frame, error) {
	return surface.Surface.Render(context)
}

func (surface *maintenanceObservingSurface) DiagnosticState() diagnostics.VisualState {
	return surface.Surface.DiagnosticState()
}

func (surface *maintenanceObservingSurface) SuppressEventDiagnostics(event shell.Event) bool {
	return surface.Surface.SuppressEventDiagnostics(event)
}

func (surface *maintenanceObservingSurface) SuppressEffectDiagnostics(effect shell.Effect) bool {
	return surface.Surface.SuppressEffectDiagnostics(effect)
}

func newTestSurface(t interface{ Helper() }, options Options) (*synchronousTestSurface, error) {
	t.Helper()
	surface, err := New(options)
	if err == nil {
		surface.refresh()
	}
	return &synchronousTestSurface{Surface: surface}, err
}

func addUIEvent(t testing.TB, recorder *diagnostics.Recorder, code, correlation string, visual bool) {
	t.Helper()
	event := diagnostics.SemanticEvent{
		Level: diagnostics.LevelInfo, Kind: diagnostics.KindInteraction,
		Plugin: uiID(t, "plugin-a"), Component: uiID(t, "results"), Action: uiID(t, "open"),
		Code: uiID(t, code), Outcome: diagnostics.OutcomeApplied,
		Geometry: diagnostics.Geometry{ReportedColumns: 70, ReportedRows: 30, RenderColumns: 70, RenderRows: 30},
	}
	if correlation != "" {
		event.Correlation = uiID(t, correlation)
	}
	if visual {
		event.Visual = &diagnostics.VisualState{Screen: uiID(t, "search"), Focus: uiID(t, "results"), State: uiID(t, "ready"), ItemCount: 2, SelectedIndex: 1}
	}
	if err := recorder.RecordSemantic(event); err != nil {
		t.Fatal(err)
	}
}

func addDetailedUIEvent(t testing.TB, recorder *diagnostics.Recorder, session, component, action, code, correlation string, level diagnostics.Level, kind diagnostics.Kind, visual *diagnostics.VisualState) {
	t.Helper()
	event := diagnostics.SemanticEvent{
		Level: level, Kind: kind, Plugin: uiID(t, session), Component: uiID(t, component), Action: uiID(t, action),
		Code: uiID(t, code), Outcome: diagnostics.OutcomeApplied, Generation: 7, RelatedGeneration: 6,
		Geometry: diagnostics.Geometry{ReportedColumns: 120, ReportedRows: 40, RenderColumns: 110, RenderRows: 24},
		Visual:   visual,
	}
	if correlation != "" {
		event.Correlation = uiID(t, correlation)
	}
	if err := recorder.RecordSemantic(event); err != nil {
		t.Fatal(err)
	}
}

func terminalTheme(t testing.TB) theme.Palette {
	t.Helper()
	palette, err := theme.Builtin("terminal")
	if err != nil {
		t.Fatal(err)
	}
	return palette
}

func renderAt(t testing.TB, surface interface {
	Update(shell.EventContext, shell.Event) []shell.Effect
	Render(shell.RenderContext) (*view.Frame, error)
}, size responsive.Size, generation uint64) *view.Frame {
	t.Helper()
	layout := responsive.Resolve(size)
	surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: layout, Generation: generation})
	frame, err := surface.Render(shell.RenderContext{Layout: layout, Theme: terminalTheme(t), ResizeGeneration: generation, Settled: true})
	if err != nil {
		t.Fatal(err)
	}
	if frame.Width() != layout.Render.Columns || frame.Height() != layout.Render.Rows {
		t.Fatalf("frame=%dx%d layout=%+v", frame.Width(), frame.Height(), layout)
	}
	return frame
}

func frameText(frame *view.Frame) string {
	var output strings.Builder
	for row := range frame.Height() {
		for column := range frame.Width() {
			cell, _ := frame.CellAt(column, row)
			if cell.Continuation {
				continue
			}
			if cell.Text == "" {
				output.WriteByte(' ')
			} else {
				output.WriteString(cell.Text)
			}
		}
		output.WriteByte('\n')
	}
	return output.String()
}

func TestSurfaceRendersRequiredResponsiveEnvelopeAcrossThemes(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "request.completed", "request-1", true)
	sizes := []responsive.Size{{Columns: 40, Rows: 10}, {Columns: 48, Rows: 18}, {Columns: 48, Rows: 30}, {Columns: 78, Rows: 10}, {Columns: 78, Rows: 20}, {Columns: 80, Rows: 18}, {Columns: 110, Rows: 24}, {Columns: 500, Rows: 200}, {Columns: 900, Rows: 300}}
	for _, themeID := range theme.IDs() {
		palette, err := theme.Builtin(themeID)
		if err != nil {
			t.Fatal(err)
		}
		for generation, size := range sizes {
			surface, err := newTestSurface(t, Options{Recorder: recorder, PageSize: 7})
			if err != nil {
				t.Fatal(err)
			}
			layout := responsive.Resolve(size)
			surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: layout, Generation: uint64(generation + 1)})
			frame, renderErr := surface.Render(shell.RenderContext{Layout: layout, Theme: palette, ResizeGeneration: uint64(generation + 1), Settled: true})
			if renderErr != nil || frame.Width() != layout.Render.Columns || frame.Height() != layout.Render.Rows {
				t.Fatalf("theme=%s size=%+v frame=%v err=%v", themeID, size, frame, renderErr)
			}
		}
	}
}

func TestCompactNavigationDrillInFiltersAndResizeState(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "first", "request-1", false)
	addUIEvent(t, recorder, "second", "request-2", true)
	surface, err := newTestSurface(t, Options{Recorder: recorder, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	frame := renderAt(t, surface, responsive.Size{Columns: 40, Rows: 10}, 1)
	if text := frameText(frame); !strings.Contains(text, "h/t/g/r") || !strings.Contains(text, "p/v") {
		t.Fatalf("compact basic-key navigation missing: %q", text)
	}
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "t"})
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "/"})
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "second/PRIVATE"})
	surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: responsive.Resolve(responsive.Size{Columns: 48, Rows: 18}), Generation: 2})
	if !surface.editing || surface.draft != "second" {
		t.Fatalf("bounded filter draft lost or accepted invalid text: %q", surface.draft)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
	if surface.query.Code.String() != "second" || surface.page.Total != 1 {
		t.Fatalf("code filter not applied: query=%+v page=%+v", surface.query, surface.page)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
	if surface.screen != screenDetail || surface.selectedEvent().Code.String() != "second" {
		t.Fatalf("detail state=%v event=%+v", surface.screen, surface.selectedEvent())
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyBackspace})
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "c"})
	if surface.query.Correlation.String() != "request-2" || surface.page.Total != 1 {
		t.Fatalf("correlation filter query=%+v page=%+v", surface.query, surface.page)
	}
	surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: responsive.Resolve(responsive.Size{Columns: 20, Rows: 5}), Generation: 3})
	surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: responsive.Resolve(responsive.Size{Columns: 110, Rows: 24}), Generation: 4})
	if surface.screen != screenTimeline || surface.query.Correlation.String() != "request-2" || surface.selected != 0 {
		t.Fatalf("state lost across resize: %+v", surface)
	}
}

func TestCompactHelpIsCompleteAndUnclipped(t *testing.T) {
	recorder := uiRecorder(t)
	surface, err := newTestSurface(t, Options{Recorder: recorder})
	if err != nil {
		t.Fatal(err)
	}
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "?"})
	text := frameText(renderAt(t, surface, responsive.Size{Columns: 40, Rows: 10}, 1))
	for _, line := range helpLines() {
		if !strings.Contains(text, line) {
			t.Fatalf("compact help omitted %q: %q", line, text)
		}
	}
	if strings.Contains(text, "...") {
		t.Fatalf("compact help was clipped: %q", text)
	}
}

func TestStatusIncludesRecordingProgressAndFailures(t *testing.T) {
	recorder := uiRecorder(t)
	surface, err := newTestSurface(t, Options{Recorder: recorder, RecordingStatus: func() RecordingStatus {
		return RecordingStatus{Pending: 3, Persisted: 17, OverflowRejected: 2, PersistenceFailed: 1}
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := surface.status(shell.RenderContext{Layout: responsive.Resolve(responsive.Size{Columns: 110, Rows: 24})})
	for _, want := range []string{"Q!", "p3", "ok17", "r2", "f1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status %q omitted %q", got, want)
		}
	}
}

func TestStatusReportsEitherRecordingFailureCounter(t *testing.T) {
	recorder := uiRecorder(t)
	for _, status := range []RecordingStatus{
		{Pending: 1, Persisted: 2, OverflowRejected: 3},
		{Pending: 1, Persisted: 2, PersistenceFailed: 4},
	} {
		surface, err := newTestSurface(t, Options{Recorder: recorder, RecordingStatus: func() RecordingStatus { return status }})
		if err != nil {
			t.Fatal(err)
		}
		if got := surface.status(shell.RenderContext{Layout: responsive.Resolve(responsive.Size{Columns: 80, Rows: 18})}); !strings.Contains(got, "Q!") {
			t.Fatalf("failure status not highlighted: %q", got)
		}
	}
}

func TestMaintenanceDiagnosticSuppressionIsExact(t *testing.T) {
	surface, err := newTestSurface(t, Options{Recorder: uiRecorder(t)})
	if err != nil {
		t.Fatal(err)
	}
	otherCode, _ := shell.NewEventCode("other.timer")
	otherRequest, _ := shell.NewRequestKey("other.request")
	for _, test := range []struct {
		event shell.Event
		want  bool
	}{
		{shell.TimerEvent{Code: maintenanceCode}, true},
		{shell.TimerEvent{Code: otherCode}, false},
		{shell.ResultEvent{Key: projectionRequest}, true},
		{shell.ResultEvent{Key: otherRequest}, false},
		{shell.TextEvent{Text: "e"}, false},
		{shell.FocusEvent{Focused: true}, false},
	} {
		if got := surface.SuppressEventDiagnostics(test.event); got != test.want {
			t.Fatalf("SuppressEventDiagnostics(%T) = %t, want %t", test.event, got, test.want)
		}
	}
}

func TestMaintenanceTimerUsesRealShellContextAndRearmsLiveOrFrozen(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		t.Run(fmt.Sprintf("frozen=%t", frozen), func(t *testing.T) {
			recorder := uiRecorder(t)
			addUIEvent(t, recorder, "initial", "", false)
			base := loadedProjectionSurface(t, recorder)
			base.frozen = frozen
			observed := &maintenanceObservingSurface{Surface: base, updates: make(chan observedUpdate, 32)}
			input, writer := io.Pipe()
			t.Cleanup(func() { _ = writer.Close() })
			program, err := shell.NewProgram(shell.ProgramOptions{PluginID: uiID(t, "plugin-a"), Theme: terminalTheme(t), Events: recorder, Input: input, Output: io.Discard}, observed)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, runErr := program.Run(); done <- runErr }()
			t.Cleanup(func() {
				program.Kill()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("debug program did not stop")
				}
			})

			program.Send(tea.WindowSizeMsg{Width: 80, Height: 18})
			var resized observedUpdate
			deadline := time.After(2 * time.Second)
		resizeLoop:
			for {
				select {
				case update := <-observed.updates:
					if _, ok := update.event.(shell.ResizeEvent); ok {
						resized = update
						break resizeLoop
					}
				case <-deadline:
					t.Fatal("resize did not reach debug surface")
				}
			}
			if !resized.nonReservedOK {
				t.Fatal("non-reserved work or timer effect was suppressed")
			}

			var maintenance observedUpdate
			deadline = time.After(2 * time.Second)
		maintenanceLoop:
			for {
				select {
				case update := <-observed.updates:
					if timer, ok := update.event.(shell.TimerEvent); ok && timer.Code == maintenanceCode {
						maintenance = update
						break maintenanceLoop
					}
				case <-deadline:
					t.Fatal("maintenance timer did not fire")
				}
			}
			elapsed := maintenance.observedAt.Sub(resized.observedAt)
			if elapsed < 350*time.Millisecond || elapsed > 1500*time.Millisecond {
				t.Fatalf("maintenance delay = %v, want approximately 500ms", elapsed)
			}
			requests, timers := 0, 0
			for _, effect := range maintenance.effects {
				if !effect.suppressed {
					t.Fatalf("reserved maintenance effect was not suppressed: %+v", effect)
				}
				if effect.request == projectionRequest {
					requests++
				}
				if effect.event == maintenanceCode {
					timers++
				}
			}
			wantRequests := 1
			if frozen {
				wantRequests = 0
			}
			if requests != wantRequests || timers != 1 {
				t.Fatalf("maintenance effects: requests=%d timers=%d frozen=%t", requests, timers, frozen)
			}
		})
	}
}

func TestRenderOwnershipAndGalleryOmissionAreObservable(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "visual", "", true)
	surface := loadedProjectionSurface(t, recorder)
	surface.list, surface.screen = screenGallery, screenGallery
	surface.displayedList = screenTimeline
	surface.anchors[1] = inspectionAnchor{top: 99}
	frame := renderAt(t, surface, responsive.Size{Columns: 80, Rows: 18}, 1)
	if surface.anchors[1].top != 99 {
		t.Fatalf("stale displayed list changed gallery anchor: %+v", surface.anchors[1])
	}
	if text := frameText(frame); !strings.Contains(text, "semantic previews omitted") {
		t.Fatalf("gallery omission was not rendered: %q", text)
	}

	surface.list, surface.screen, surface.displayedList = screenTimeline, screenTimeline, screenTimeline
	surface.displayedQuery = surface.query
	surface.anchors[0] = inspectionAnchor{}
	renderAt(t, surface, responsive.Size{Columns: 80, Rows: 18}, 2)
	if surface.anchors[0].top == 0 || surface.visibleTop[0] == 0 {
		t.Fatalf("current non-empty projection did not publish visible top: anchor=%+v visible=%d", surface.anchors[0], surface.visibleTop[0])
	}

	surface.page.Events = nil
	surface.anchors[0].top, surface.visibleTop[0] = 77, 77
	renderAt(t, surface, responsive.Size{Columns: 80, Rows: 18}, 3)
	if surface.anchors[0].top != 0 || surface.visibleTop[0] != 0 {
		t.Fatalf("current empty projection retained visible top: anchor=%+v visible=%d", surface.anchors[0], surface.visibleTop[0])
	}
}

func TestRenderedViewsNeverContainRecorderPayloadPathOrError(t *testing.T) {
	recorder := uiRecorder(t)
	canary := "CANARY_/private/path_error-detail"
	if err := recorder.RecordSemantic(diagnostics.SemanticEvent{
		Level: diagnostics.LevelError, Kind: diagnostics.KindDiagnostic, Plugin: uiID(t, "plugin-a"), Code: uiID(t, "safe.failed"), Outcome: diagnostics.OutcomeFailed,
		Visual: &diagnostics.VisualState{Screen: uiID(t, "safe"), State: uiID(t, "failed")},
	}); err != nil {
		t.Fatal(err)
	}
	surface, _ := newTestSurface(t, Options{Recorder: recorder})
	for _, command := range []string{"h", "t", "g", "r", "?"} {
		surface.Update(shell.EventContext{}, shell.TextEvent{Text: command})
		text := frameText(renderAt(t, surface, responsive.Size{Columns: 110, Rows: 24}, 1))
		if strings.Contains(text, canary) || strings.Contains(strings.ToLower(text), "/private/path") {
			t.Fatalf("%s view leaked unsafe content: %q", command, text)
		}
	}
}

func TestExportUsesBoundedRecorderReportAndFiniteStatuses(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "event", "request-1", true)
	var exported []byte
	surface, err := newTestSurface(t, Options{Recorder: recorder, MaxReportBytes: 2048, Exporter: func(_ context.Context, data []byte) error {
		exported = append([]byte(nil), data...)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	result := surface.exportWork()(context.Background())
	if result.Err != nil || result.Code != diagnostics.OutcomeApplied || len(exported) == 0 || len(exported) > 2048 || !json.Valid(exported) {
		t.Fatalf("export result=%+v bytes=%d valid=%t", result, len(exported), json.Valid(exported))
	}
	surface.Update(shell.EventContext{}, shell.ResultEvent{Key: exportRequest, Result: result})
	if surface.export != exportSucceeded || exportLabel(surface.export) != "OK exported" {
		t.Fatalf("export status=%v", surface.export)
	}
	failing, _ := newTestSurface(t, Options{Recorder: recorder, Exporter: func(context.Context, []byte) error { return errors.New("SECRET exporter path") }})
	failed := failing.exportWork()(context.Background())
	if failed.Err == nil || failed.Err.Error() != "debug export failed" {
		t.Fatalf("unbounded export error escaped: %v", failed.Err)
	}
}

func TestProjectionFailureIsGuardedAndResizeHistoryIsBounded(t *testing.T) {
	recorder := uiRecorder(t)
	surface, _ := newTestSurface(t, Options{Recorder: recorder})
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	for generation := 1; generation <= resizeHistoryLimit+20; generation++ {
		surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: responsive.Resolve(responsive.Size{Columns: 40 + generation, Rows: 10}), Generation: uint64(generation)})
	}
	if !surface.projectionFailed || len(surface.resizes) != resizeHistoryLimit || surface.resizes[0].Generation != 21 {
		t.Fatalf("guard=%t resize history=%+v", surface.projectionFailed, surface.resizes)
	}
	frame, err := surface.Render(shell.RenderContext{Layout: surface.layout, Theme: terminalTheme(t)})
	if err != nil || !strings.Contains(frameText(frame), "Previous safe projection retained") {
		t.Fatalf("guard render err=%v text=%q", err, frameText(frame))
	}
}

func TestDebugIgnoresStaleResizeAndExplainsRecovery(t *testing.T) {
	recorder := uiRecorder(t)
	surface, _ := newTestSurface(t, Options{Recorder: recorder})
	newest := responsive.Resolve(responsive.Size{Columns: 110, Rows: 24})
	older := responsive.Resolve(responsive.Size{Columns: 40, Rows: 10})
	surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: newest, Generation: 2})
	surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: older, Generation: 1})
	surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: older, Generation: 2})
	if surface.layout != newest || surface.resizeGeneration != 2 || len(surface.resizes) != 1 || surface.resizes[0].Generation != 2 {
		t.Fatalf("stale or duplicate resize committed: layout=%+v g=%d history=%+v", surface.layout, surface.resizeGeneration, surface.resizes)
	}
	recovery := responsive.Resolve(responsive.Size{Columns: 20, Rows: 5})
	frame, err := surface.Render(shell.RenderContext{Layout: recovery, Theme: terminalTheme(t)})
	if err != nil || !strings.Contains(frameText(frame), "Need 40×10") {
		t.Fatalf("recovery err=%v frame=%q", err, frameText(frame))
	}
}

func TestTimelineAndRegisteredGalleryKeepSelectionVisible(t *testing.T) {
	recorder := uiRecorder(t)
	for index := range 12 {
		addUIEvent(t, recorder, fmt.Sprintf("event-%d", index), "", true)
	}
	page, err := recorder.Debug(diagnostics.DebugQuery{PageSize: 20})
	if err != nil {
		t.Fatal(err)
	}
	previews, err := diagnostics.OpenPreviewStore(t.TempDir(), diagnostics.DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range page.Events {
		if _, err := previews.Put(event.Sequence, *event.Visual); err != nil {
			t.Fatal(err)
		}
	}
	surface, _ := newTestSurface(t, Options{Recorder: recorder, Previews: previews, PageSize: 20})
	surface.screen = screenTimeline
	surface.selected = len(surface.page.Events) - 1
	layout := responsive.Resolve(responsive.Size{Columns: 40, Rows: 10})
	frame, _ := surface.Render(shell.RenderContext{Layout: layout, Theme: terminalTheme(t)})
	if text := frameText(frame); !strings.Contains(text, "▌ 000001") {
		t.Fatalf("timeline selection is not visible: %q", text)
	}
	surface.screen = screenGallery
	frame, _ = surface.Render(shell.RenderContext{Layout: layout, Theme: terminalTheme(t)})
	if text := frameText(frame); !strings.Contains(text, "▌ 000001") {
		t.Fatalf("gallery selection is not visible: %q", text)
	}
}

func TestSurfaceNavigationModelMaintainsBounds(t *testing.T) {
	recorder := uiRecorder(t)
	for index := range 20 {
		addUIEvent(t, recorder, fmt.Sprintf("event-%d", index), fmt.Sprintf("request-%d", index%3), index%2 == 0)
	}
	commands := []byte{'h', 't', 'g', 'r', '?', '?', 's', 'l', 'k', '0', '1', '2', '3', '4'}
	rapid.Check(t, func(t *rapid.T) {
		surface, _ := newTestSurface(t, Options{Recorder: recorder, PageSize: 5})
		for range rapid.IntRange(1, 80).Draw(t, "steps") {
			if rapid.Bool().Draw(t, "text") {
				value := commands[rapid.IntRange(0, len(commands)-1).Draw(t, "command")]
				surface.Update(shell.EventContext{}, shell.TextEvent{Text: string(value)})
			} else {
				keys := []shell.KeyCode{shell.KeyUp, shell.KeyDown, shell.KeyHome, shell.KeyEnd, shell.KeyPageUp, shell.KeyPageDown, shell.KeyLeft, shell.KeyRight, shell.KeyEnter, shell.KeyBackspace}
				surface.Update(shell.EventContext{}, shell.KeyEvent{Code: keys[rapid.IntRange(0, len(keys)-1).Draw(t, "key")]})
			}
			if surface.selected < 0 || (len(surface.page.Events) == 0 && surface.selected != 0) || (len(surface.page.Events) > 0 && surface.selected >= len(surface.page.Events)) {
				t.Fatalf("selection out of bounds: selected=%d events=%d", surface.selected, len(surface.page.Events))
			}
			if len(surface.resizes) > resizeHistoryLimit || surface.query.Page < 0 || surface.query.PageSize != 5 {
				t.Fatalf("unbounded state: %+v", surface)
			}
		}
	})
}

func TestRecorderCanWriteWhileDebugSurfaceRefreshesAndRenders(t *testing.T) {
	recorder := uiRecorder(t)
	surface, _ := newTestSurface(t, Options{Recorder: recorder, PageSize: 10})
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		for index := range 100 {
			addUIEvent(t, recorder, fmt.Sprintf("event-%d", index), "request-1", index%2 == 0)
		}
	}()
	for generation := range 100 {
		layout := responsive.Resolve(responsive.Size{Columns: 40 + generation%80, Rows: 10 + generation%20})
		surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: layout, Generation: uint64(generation + 1)})
		frame, err := surface.Render(shell.RenderContext{Layout: layout, Theme: terminalTheme(t), ResizeGeneration: uint64(generation + 1)})
		if err != nil || frame.Width() != layout.Render.Columns {
			t.Fatalf("render error=%v frame=%v", err, frame)
		}
	}
	wait.Wait()
}

func TestFrameRenderingIsDeterministic(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "event", "request-1", true)
	surface, _ := newTestSurface(t, Options{Recorder: recorder})
	layout := responsive.Resolve(responsive.Size{Columns: 80, Rows: 18})
	surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: layout, Generation: 1})
	context := shell.RenderContext{Layout: layout, Theme: terminalTheme(t), ResizeGeneration: 1, Settled: true}
	first, _ := surface.Render(context)
	second, _ := surface.Render(context)
	if !bytes.Equal([]byte(frameText(first)), []byte(frameText(second))) {
		t.Fatal("identical state rendered differently")
	}
}

func TestApprovedDebugFramesRemainByteStable(t *testing.T) {
	visual := &diagnostics.VisualState{Screen: uiID(t, "search"), Focus: uiID(t, "results"), Selection: uiID(t, "item-2"), State: uiID(t, "ready"), ItemCount: 3, SelectedIndex: 1, Pending: true, HasError: true}
	palette := terminalTheme(t)
	tests := []struct {
		name string
		size responsive.Size
		set  func(*Surface)
	}{
		{name: "compact-health", size: responsive.Size{Columns: 40, Rows: 10}, set: func(*Surface) {}},
		{name: "standard-timeline", size: responsive.Size{Columns: 80, Rows: 18}, set: func(surface *Surface) { surface.screen = screenTimeline }},
		{name: "standard-split", size: responsive.Size{Columns: 80, Rows: 19}, set: func(surface *Surface) { surface.screen = screenTimeline }},
		{name: "wide-gallery", size: responsive.Size{Columns: 110, Rows: 24}, set: func(*Surface) {}},
		{name: "compact-detail", size: responsive.Size{Columns: 48, Rows: 18}, set: func(surface *Surface) { surface.screen = screenDetail }},
		{name: "help", size: responsive.Size{Columns: 40, Rows: 10}, set: func(surface *Surface) { surface.screen = screenHelp }},
		{name: "recovery", size: responsive.Size{Columns: 39, Rows: 9}, set: func(*Surface) {}},
		{name: "wide-editing", size: responsive.Size{Columns: 110, Rows: 24}, set: func(surface *Surface) {
			surface.screen = screenTimeline
			surface.editing = true
			surface.draft = strings.Repeat("a", 64)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := uiRecorder(t)
			addDetailedUIEvent(t, recorder, "alpha", "results", "open", "request.completed", "request-1", diagnostics.LevelInfo, diagnostics.KindInteraction, visual)
			page, err := recorder.Debug(diagnostics.DebugQuery{})
			if err != nil {
				t.Fatal(err)
			}
			previews, err := diagnostics.OpenPreviewStore(t.TempDir(), diagnostics.DefaultPreviewLimits())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = previews.Close() })
			if _, err := previews.Put(page.Events[0].Sequence, *page.Events[0].Visual); err != nil {
				t.Fatal(err)
			}
			if test.name == "wide-gallery" {
				addDetailedUIEvent(t, recorder, "alpha", "results", "open", "unregistered.visual", "request-2", diagnostics.LevelInfo, diagnostics.KindInteraction, visual)
			}
			surface, err := newTestSurface(t, Options{Recorder: recorder, Previews: previews})
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "wide-gallery" {
				surface.Surface.text(shell.EventContext{}, 'g')
				snapshot, snapshotErr := recorder.DebugSnapshot(context.Background(), previews)
				if snapshotErr != nil {
					t.Fatal(snapshotErr)
				}
				gallery, selectErr := snapshot.Select(context.Background(), surface.query, true)
				if selectErr != nil {
					t.Fatal(selectErr)
				}
				surface.pending = projectionSelect
				surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{
					snapshot: snapshot, view: gallery, query: surface.query, kind: projectionSelect, list: screenGallery, epoch: surface.projectionEpoch,
				}})
				selected := surface.selectedEvent()
				if surface.list != screenGallery || surface.screen != screenGallery || selected == nil || selected.Sequence != page.Events[0].Sequence || !surface.view.PreviewEligible(selected.Sequence) {
					t.Fatalf("gallery approval state is incoherent: list=%d screen=%d selected=%+v eligible=%t", surface.list, surface.screen, selected, selected != nil && surface.view.PreviewEligible(selected.Sequence))
				}
			} else {
				test.set(surface.Surface)
			}
			layout := responsive.Resolve(test.size)
			surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: layout, Generation: 7})
			surface.page.Health = diagnostics.DebugHealth{Writable: true, Events: 1, MaxEvents: 100, Bytes: 512, MaxBytes: 4096, UsageRatio: 0.125}
			frame, err := surface.Render(shell.RenderContext{Layout: layout, Theme: palette, ResizeGeneration: 7, Settled: true})
			if err != nil {
				t.Fatal(err)
			}
			got, err := view.PNG(frame)
			if err != nil {
				t.Fatal(err)
			}
			fixture := filepath.Join("testdata", "approved", test.name+".png")
			if os.Getenv("HERDR_UPDATE_GOLDENS") == "1" {
				if err := os.MkdirAll(filepath.Dir(fixture), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fixture, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				artifactRoot := filepath.Join("..", "..", ".artifacts", "test-failures")
				if err := os.MkdirAll(artifactRoot, 0o700); err != nil {
					t.Fatal(err)
				}
				artifactDir, err := os.MkdirTemp(artifactRoot, "debugui-"+test.name+"-")
				if err != nil {
					t.Fatal(err)
				}
				actual := filepath.Join(artifactDir, "actual.png")
				if err := os.WriteFile(actual, got, 0o600); err != nil {
					t.Fatal(err)
				}
				t.Fatalf("approved frame changed; compare expected %s with actual %s\nvisible frame:\n%s", fixture, actual, view.ANSI(frame))
			}
		})
	}
}

func TestSurfaceConstructionAndQuitBoundaries(t *testing.T) {
	if _, err := newTestSurface(t, Options{}); err == nil || err.Error() != "debug recorder is nil" {
		t.Fatalf("nil recorder error = %v", err)
	}
	recorder := uiRecorder(t)
	for _, size := range []int{-1, diagnostics.MaxDebugPageSize + 1} {
		if _, err := newTestSurface(t, Options{Recorder: recorder, PageSize: size}); err == nil {
			t.Fatalf("accepted invalid page size %d", size)
		}
	}
	for _, size := range []int{0, 1, diagnostics.MaxDebugPageSize} {
		surface, err := newTestSurface(t, Options{Recorder: recorder, PageSize: size})
		if err != nil {
			t.Fatalf("page size %d: %v", size, err)
		}
		want := size
		if want == 0 {
			want = diagnostics.DefaultDebugPageSize
		}
		if surface.query.PageSize != want || surface.export != exportDisabled {
			t.Fatalf("page size %d initialized as query=%+v export=%v", size, surface.query, surface.export)
		}
	}
	surface, _ := newTestSurface(t, Options{Recorder: recorder})
	for _, event := range []shell.Event{shell.TextEvent{Text: "q"}, shell.KeyEvent{Code: shell.KeyCtrlC}} {
		if effects := surface.Update(shell.EventContext{}, event); len(effects) != 1 {
			t.Fatalf("quit event %T returned %d effects", event, len(effects))
		}
	}
	if effects := surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEscape}); len(effects) != 1 {
		t.Fatalf("escape from primary screen returned %d effects", len(effects))
	}
}

func TestFilterGrammarAndEmptyEditingKeys(t *testing.T) {
	for _, test := range []struct {
		value byte
		first bool
		want  bool
	}{
		{value: 'a', first: true, want: true},
		{value: 'z', first: true, want: true},
		{value: '0', first: true, want: false},
		{value: '0', want: true},
		{value: '9', want: true},
		{value: '.', want: true},
		{value: '_', want: true},
		{value: '-', want: true},
		{value: '/', want: false},
		{value: 'A', want: false},
	} {
		if got := validDraftByte(test.value, test.first); got != test.want {
			t.Fatalf("validDraftByte(%q, first=%t) = %t, want %t", test.value, test.first, got, test.want)
		}
	}

	recorder := uiRecorder(t)
	surface, _ := newTestSurface(t, Options{Recorder: recorder})
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "/"})
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyBackspace})
	if !surface.editing || surface.draft != "" {
		t.Fatalf("empty Backspace changed editor: editing=%t draft=%q", surface.editing, surface.draft)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEscape})
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "?"})
	if surface.screen != screenHelp || surface.previous != screenHealth {
		t.Fatalf("help did not retain return screen: screen=%v previous=%v", surface.screen, surface.previous)
	}
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "?"})
	if surface.screen != screenHealth {
		t.Fatalf("help did not return to prior screen: %v", surface.screen)
	}
}

func TestSurfaceReportsFilterEditingState(t *testing.T) {
	surface, err := newTestSurface(t, Options{Recorder: uiRecorder(t)})
	if err != nil {
		t.Fatal(err)
	}
	if surface.Editing() {
		t.Fatal("new surface reports active filter editing")
	}
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "/"})
	if !surface.Editing() {
		t.Fatal("surface did not report active filter editing")
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEscape})
	if surface.Editing() {
		t.Fatal("surface still reports editing after Escape")
	}
}

func TestNavigationFilteringAndPagingStateMachine(t *testing.T) {
	recorder := uiRecorder(t)
	visual := &diagnostics.VisualState{Screen: uiID(t, "search"), Focus: uiID(t, "results"), State: uiID(t, "ready"), ItemCount: 3, SelectedIndex: 1}
	addDetailedUIEvent(t, recorder, "alpha", "results", "open", "alpha.first", "request-a", diagnostics.LevelInfo, diagnostics.KindInteraction, nil)
	addDetailedUIEvent(t, recorder, "beta", "worker", "retry", "beta.only", "request-b", diagnostics.LevelError, diagnostics.KindDiagnostic, nil)
	addDetailedUIEvent(t, recorder, "alpha", "results", "open", "alpha.latest", "request-a", diagnostics.LevelInfo, diagnostics.KindInteraction, visual)
	surface, _ := newTestSurface(t, Options{Recorder: recorder, PageSize: 1})

	if surface.page.Total != 3 || !surface.page.HasNext || surface.page.HasPrev {
		t.Fatalf("initial page = %+v", surface.page)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyPageDown})
	if surface.page.Page != 1 || surface.query.Page != 0 || surface.page.Events[0].Code.String() != "beta.only" || !surface.page.HasPrev || !surface.page.HasNext {
		t.Fatalf("middle page query=%+v page=%+v", surface.query, surface.page)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyPageDown})
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyPageDown})
	if surface.page.Page != 2 || surface.query.Page != 0 || surface.page.Events[0].Code.String() != "alpha.first" || surface.page.HasNext {
		t.Fatalf("last page advanced past boundary: query=%+v page=%+v", surface.query, surface.page)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyPageUp})
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyPageUp})
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyPageUp})
	if surface.page.Page != 0 || surface.query.Page != 0 || surface.page.Events[0].Code.String() != "alpha.latest" {
		t.Fatalf("first page moved past boundary: query=%+v page=%+v", surface.query, surface.page)
	}

	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyRight})
	if surface.query.Session.String() != "alpha" || surface.page.Total != 2 {
		t.Fatalf("right session = %q total=%d", surface.query.Session.String(), surface.page.Total)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyLeft})
	if !surface.query.Session.IsZero() || surface.page.Total != 3 {
		t.Fatalf("left did not wrap to all sessions: %+v", surface.query)
	}
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "s"})
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "s"})
	if surface.query.Session.String() != "beta" || surface.page.Total != 1 {
		t.Fatalf("session cycle = %q total=%d", surface.query.Session.String(), surface.page.Total)
	}

	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "0"})
	for _, command := range []string{"l", "l", "k", "k", "1", "2", "3", "4"} {
		surface.Update(shell.EventContext{}, shell.TextEvent{Text: command})
	}
	if surface.query.Level != diagnostics.LevelInfo || surface.query.Kind != diagnostics.KindInteraction ||
		surface.query.Component.String() != "results" || surface.query.Action.String() != "open" ||
		surface.query.Code.String() != "alpha.latest" || surface.query.Correlation.String() != "request-a" || surface.page.Total != 1 {
		t.Fatalf("composed filters = %+v page=%+v", surface.query, surface.page)
	}
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "0"})
	if surface.query.PageSize != 1 || surface.query.Page != 0 || !surface.query.Session.IsZero() || surface.level != 0 || surface.kind != 0 || surface.page.Total != 3 {
		t.Fatalf("clear filters did not restore all-events projection: query=%+v page=%+v", surface.query, surface.page)
	}
}

func TestSelectionDetailHelpAndEditorTransitions(t *testing.T) {
	recorder := uiRecorder(t)
	for _, code := range []string{"oldest", "middle", "newest"} {
		addUIEvent(t, recorder, code, "request-1", false)
	}
	surface, _ := newTestSurface(t, Options{Recorder: recorder, PageSize: 3})
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "t"})
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyDown})
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyDown})
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyDown})
	if surface.selected != 2 {
		t.Fatalf("down selection crossed end: %d", surface.selected)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyUp})
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyHome})
	if surface.selected != 0 {
		t.Fatalf("home selection = %d", surface.selected)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnd})
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
	if surface.screen != screenDetail || surface.previous != screenTimeline || surface.selectedEvent().Code.String() != "oldest" {
		t.Fatalf("detail transition screen=%v previous=%v event=%+v", surface.screen, surface.previous, surface.selectedEvent())
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEscape})
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "?"})
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "?"})
	if surface.screen != screenTimeline {
		t.Fatalf("help did not return to timeline: %v", surface.screen)
	}

	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "/"})
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "9INVALIDvalid.code_more-ok"})
	if surface.draft != "valid.code_more-ok" {
		t.Fatalf("editor accepted invalid identifier bytes: %q", surface.draft)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyBackspace})
	if surface.draft != "valid.code_more-o" {
		t.Fatalf("editor backspace = %q", surface.draft)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEscape})
	if surface.editing || surface.draft != "" {
		t.Fatalf("editor cancel retained state: editing=%t draft=%q", surface.editing, surface.draft)
	}
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "/"})
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: strings.Repeat("a", 80)})
	if len(surface.draft) != 64 {
		t.Fatalf("editor bound = %d", len(surface.draft))
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
	if len(surface.query.Code.String()) != 64 || surface.query.Page != 0 || surface.selected != 0 {
		t.Fatalf("editor apply = query=%+v selected=%d", surface.query, surface.selected)
	}
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "/"})
	for range 64 {
		surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyBackspace})
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
	if !surface.query.Code.IsZero() {
		t.Fatalf("empty editor did not clear code: %+v", surface.query)
	}
}

func TestExportFailureRecoveryAndDiagnosticState(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "event", "request-1", true)
	disabled, _ := newTestSurface(t, Options{Recorder: recorder})
	if effects := disabled.Update(shell.EventContext{}, shell.TextEvent{Text: "e"}); len(effects) != 0 || disabled.export != exportDisabled {
		t.Fatalf("disabled export changed: effects=%d status=%v", len(effects), disabled.export)
	}

	surface, _ := newTestSurface(t, Options{Recorder: recorder, Exporter: func(context.Context, []byte) error { return nil }})
	if effects := surface.Update(shell.EventContext{}, shell.TextEvent{Text: "e"}); len(effects) != 0 || surface.export != exportFailed {
		t.Fatalf("failed scheduling did not surface failure: effects=%d status=%v", len(effects), surface.export)
	}
	surface.export = exportPending
	if effects := surface.Update(shell.EventContext{}, shell.TextEvent{Text: "e"}); len(effects) != 0 || surface.export != exportPending {
		t.Fatalf("pending export was restarted: effects=%d status=%v", len(effects), surface.export)
	}
	state := surface.DiagnosticState()
	if state.State.String() != "export-pending" || !state.Pending || state.HasError || state.Selection.String() != "event" || state.ItemCount != 1 {
		t.Fatalf("pending diagnostic state = %+v", state)
	}
	other, _ := shell.NewRequestKey("other.request")
	surface.Update(shell.EventContext{}, shell.ResultEvent{Key: other, Result: shell.WorkResult{Code: diagnostics.OutcomeFailed, Err: errors.New("ignored")}})
	if surface.export != exportPending {
		t.Fatalf("unrelated result changed export: %v", surface.export)
	}
	surface.Update(shell.EventContext{}, shell.ResultEvent{Key: exportRequest, Result: shell.WorkResult{Code: diagnostics.OutcomeFailed}})
	if surface.export != exportFailed || surface.DiagnosticState().State.String() != "ready" || !surface.DiagnosticState().HasError {
		t.Fatalf("failed export state=%v diagnostic=%+v", surface.export, surface.DiagnosticState())
	}
	surface.Update(shell.EventContext{}, shell.ResultEvent{Key: exportRequest, Result: shell.WorkResult{Code: diagnostics.OutcomeApplied}})
	if surface.export != exportSucceeded || surface.DiagnosticState().HasError {
		t.Fatalf("successful result did not recover: status=%v diagnostic=%+v", surface.export, surface.DiagnosticState())
	}

	layout := responsive.Resolve(responsive.Size{Columns: 80, Rows: 18})
	surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: layout, Generation: 9})
	state = surface.DiagnosticState()
	if state.Screen.String() != "debug.health" || state.Focus.String() != "debug.navigation" || state.Geometry.ReportedColumns != 80 || state.Geometry.RenderRows != 18 {
		t.Fatalf("diagnostic identity/geometry = %+v", state)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	surface.Update(shell.EventContext{}, shell.FocusEvent{Focused: true})
	if !surface.projectionFailed || surface.page.Total != 1 || surface.DiagnosticState().State.String() != "projection-failed" {
		t.Fatalf("projection failure did not retain safe page: %+v state=%+v", surface.page, surface.DiagnosticState())
	}
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "r"})
	if !surface.projectionFailed || surface.page.Total != 1 {
		t.Fatalf("retry against unavailable recorder lost guard/page: guard=%t page=%+v", surface.projectionFailed, surface.page)
	}
}

func TestGalleryNavigationUsesOnlyRegisteredSemanticPreviews(t *testing.T) {
	recorder := uiRecorder(t)
	for index := range 5 {
		addUIEvent(t, recorder, fmt.Sprintf("event-%d", index), "", index != 2)
	}
	page, err := recorder.Debug(diagnostics.DebugQuery{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	previews, err := diagnostics.OpenPreviewStore(t.TempDir(), diagnostics.DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{1, 4} {
		event := page.Events[index]
		if _, err := previews.Put(event.Sequence, *event.Visual); err != nil {
			t.Fatal(err)
		}
	}
	surface, _ := newTestSurface(t, Options{Recorder: recorder, Previews: previews, PageSize: 10})
	surface.selected = 0
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "g"})
	frame := renderAt(t, surface, responsive.Size{Columns: 40, Rows: 10}, 1)
	if surface.selected != 0 || len(surface.page.Events) != 2 {
		t.Fatalf("gallery did not select first registered preview: %d", surface.selected)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyDown})
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyDown})
	if surface.selected != 1 {
		t.Fatalf("gallery crossed final registered preview: %d", surface.selected)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyUp})
	if surface.selected != 0 {
		t.Fatalf("gallery previous preview = %d", surface.selected)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnd})
	if surface.selected != 1 {
		t.Fatalf("gallery end = %d", surface.selected)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyHome})
	if surface.selected != 0 {
		t.Fatalf("gallery home = %d", surface.selected)
	}
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
	if surface.screen != screenDetail || surface.previous != screenGallery {
		t.Fatalf("gallery drill-in screen=%v previous=%v", surface.screen, surface.previous)
	}

	empty, _ := newTestSurface(t, Options{Recorder: recorder, PageSize: 10})
	empty.Update(shell.EventContext{}, shell.TextEvent{Text: "g"})
	empty.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyDown})
	if empty.selected != 0 || empty.selectedEvent() != nil {
		t.Fatalf("empty gallery retained an ineligible selection: %d", empty.selected)
	}
	frame = renderAt(t, empty, responsive.Size{Columns: 40, Rows: 10}, 1)
	if text := frameText(frame); !strings.Contains(text, "No matching safe events") {
		t.Fatalf("empty gallery explanation missing: %q", frameText(frame))
	}
}

func TestSemanticFrameLayoutFingerprintsAtSplitBoundaries(t *testing.T) {
	recorder := uiRecorder(t)
	visual := &diagnostics.VisualState{Screen: uiID(t, "search"), Focus: uiID(t, "results"), Selection: uiID(t, "item-2"), State: uiID(t, "ready"), ItemCount: 3, SelectedIndex: 1, Pending: true, HasError: true}
	addDetailedUIEvent(t, recorder, "alpha", "results", "open", "request.completed", "request-1", diagnostics.LevelInfo, diagnostics.KindInteraction, visual)
	previews, err := diagnostics.OpenPreviewStore(t.TempDir(), diagnostics.DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	page, err := recorder.Debug(diagnostics.DebugQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := previews.Put(page.Events[0].Sequence, *page.Events[0].Visual); err != nil {
		t.Fatal(err)
	}
	palette := terminalTheme(t)

	tests := []struct {
		name        string
		size        responsive.Size
		wantClass   responsive.Class
		selectionTo int
		detailFrom  int
	}{
		{name: "compact", size: responsive.Size{Columns: 40, Rows: 10}, wantClass: responsive.Compact, selectionTo: 38, detailFrom: -1},
		{name: "standard-unsplit-at-18-rows", size: responsive.Size{Columns: 80, Rows: 18}, wantClass: responsive.Standard, selectionTo: 78, detailFrom: -1},
		{name: "standard-split-above-18-rows", size: responsive.Size{Columns: 80, Rows: 19}, wantClass: responsive.Standard, selectionTo: 51, detailFrom: 53},
		{name: "wide", size: responsive.Size{Columns: 110, Rows: 24}, wantClass: responsive.Wide, selectionTo: 71, detailFrom: 73},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			surface, _ := newTestSurface(t, Options{Recorder: recorder, Previews: previews})
			surface.screen = screenTimeline
			layout := responsive.Resolve(test.size)
			if layout.Class != test.wantClass {
				t.Fatalf("class = %s", layout.Class)
			}
			frame := renderAt(t, surface, test.size, 4)
			text := frameText(frame)
			if !strings.Contains(text, "DEBUG · TIMELINE") || !strings.Contains(text, "Filter code: all") || !strings.Contains(text, "▌ 000001") {
				t.Fatalf("primary semantic regions missing: %q", text)
			}
			for _, column := range []int{0, test.selectionTo} {
				cell, _ := frame.CellAt(column, 3)
				if cell.Style.Background != palette.SelectionBackground || !cell.Style.Bold {
					t.Fatalf("selection fingerprint column %d = %+v", column, cell.Style)
				}
			}
			after, _ := frame.CellAt(test.selectionTo+1, 3)
			if after.Style.Background == palette.SelectionBackground {
				t.Fatalf("selection crossed list boundary at column %d", test.selectionTo+1)
			}
			if test.detailFrom < 0 {
				if strings.Contains(text, "Event request.completed") {
					t.Fatalf("unsplit layout exposed detail pane: %q", text)
				}
			} else {
				detail, _ := frame.CellAt(test.detailFrom, 3)
				if detail.Style.Background != palette.PanelBackground || !strings.Contains(text, "Event request.completed") {
					t.Fatalf("split detail fingerprint start=%d style=%+v text=%q", test.detailFrom, detail.Style, text)
				}
			}
			footer, _ := frame.CellAt(1, frame.Height()-1)
			if footer.Style.Foreground != palette.Muted {
				t.Fatalf("footer semantic style = %+v", footer.Style)
			}
		})
	}
}

func TestRenderedHealthDetailHUDAndRecoverySemantics(t *testing.T) {
	recorder := uiRecorder(t)
	surface, _ := newTestSurface(t, Options{Recorder: recorder, Exporter: func(context.Context, []byte) error { return nil }})
	layout := responsive.Resolve(responsive.Size{Columns: 80, Rows: 18})
	surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: layout, Generation: 1})
	surface.page.Health = diagnostics.DebugHealth{Writable: false, Pressure: true, Events: 3, MaxEvents: 5, Bytes: 80, MaxBytes: 100, UsageRatio: .8, Dropped: 2, CorruptRecords: 1}
	frame, err := surface.Render(shell.RenderContext{Layout: layout, Theme: terminalTheme(t), ResizeGeneration: 1, Settled: true})
	if err != nil {
		t.Fatal(err)
	}
	text := frameText(frame)
	for _, want := range []string{"! unavailable", "! pressure", "Usage 80.0% · 80/100 bytes", "dropped 2 · recovered 1", "Export ready"} {
		if !strings.Contains(text, want) {
			t.Fatalf("health missing %q: %q", want, text)
		}
	}

	surface.screen = screenDetail
	surface.selected = -1
	frame = renderAt(t, surface, responsive.Size{Columns: 80, Rows: 18}, 2)
	if !strings.Contains(frameText(frame), "No event selected") {
		t.Fatalf("nil detail explanation missing: %q", frameText(frame))
	}

	surface.screen = screenHUD
	oversized := responsive.Resolve(responsive.Size{Columns: 700, Rows: 250})
	surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: oversized, Generation: 3})
	frame, err = surface.Render(shell.RenderContext{Layout: oversized, Theme: terminalTheme(t), ResizeGeneration: 3, Settled: false})
	if err != nil {
		t.Fatal(err)
	}
	text = frameText(frame)
	for _, want := range []string{"700x250 g3 settling", "render 500x200", "Oversized viewport projected to 500x200 maximum", "g3 700x250→500x200 wide"} {
		if !strings.Contains(text, want) {
			t.Fatalf("HUD missing %q", want)
		}
	}

	recovery := responsive.Resolve(responsive.Size{Columns: 39, Rows: 9})
	frame, err = surface.Render(shell.RenderContext{Layout: recovery, Theme: terminalTheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	text = frameText(frame)
	if !strings.Contains(text, "Terminal is too small") || !strings.Contains(text, "received 39×9") || strings.Contains(text, "Filter code") {
		t.Fatalf("recovery semantic fingerprint = %q", text)
	}
}

func TestRefreshDropsAStaleSessionSelection(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "event", "", false)
	surface, _ := newTestSurface(t, Options{Recorder: recorder})
	surface.query.Session = uiID(t, "missing-session")
	surface.session = 4
	surface.refresh()
	if surface.session != 0 || !surface.query.Session.IsZero() || surface.page.Total != 0 {
		t.Fatalf("stale session was not cleared after its empty projection: session=%d query=%+v page=%+v", surface.session, surface.query, surface.page)
	}
	surface.refresh()
	if surface.session != 0 || !surface.query.Session.IsZero() || surface.page.Total != 1 {
		t.Fatalf("stale session survived refresh: session=%d query=%+v page=%+v", surface.session, surface.query, surface.page)
	}
}

func TestRefreshClampsSelectionAtExactEventBoundary(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "event", "", false)
	surface, _ := newTestSurface(t, Options{Recorder: recorder})
	surface.selected = len(surface.page.Events)
	surface.refresh()
	if surface.selected != len(surface.page.Events)-1 || surface.selectedEvent() == nil {
		t.Fatalf("selection was not clamped: selected=%d events=%d", surface.selected, len(surface.page.Events))
	}
}
