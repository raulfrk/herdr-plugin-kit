package debugui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
	"pgregory.net/rapid"
)

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

func terminalTheme(t testing.TB) theme.Palette {
	t.Helper()
	palette, err := theme.Builtin("terminal")
	if err != nil {
		t.Fatal(err)
	}
	return palette
}

func renderAt(t testing.TB, surface *Surface, size responsive.Size, generation uint64) *view.Frame {
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
			surface, err := New(Options{Recorder: recorder, PageSize: 7})
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
	surface, err := New(Options{Recorder: recorder, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	frame := renderAt(t, surface, responsive.Size{Columns: 40, Rows: 10}, 1)
	if text := frameText(frame); !strings.Contains(text, "h/t/g/r view") {
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

func TestRenderedViewsNeverContainRecorderPayloadPathOrError(t *testing.T) {
	recorder := uiRecorder(t)
	canary := "CANARY_/private/path_error-detail"
	if err := recorder.RecordSemantic(diagnostics.SemanticEvent{
		Level: diagnostics.LevelError, Kind: diagnostics.KindDiagnostic, Plugin: uiID(t, "plugin-a"), Code: uiID(t, "safe.failed"), Outcome: diagnostics.OutcomeFailed,
		Visual: &diagnostics.VisualState{Screen: uiID(t, "safe"), State: uiID(t, "failed")},
	}); err != nil {
		t.Fatal(err)
	}
	surface, _ := New(Options{Recorder: recorder})
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
	surface, err := New(Options{Recorder: recorder, MaxReportBytes: 2048, Exporter: func(_ context.Context, data []byte) error {
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
	failing, _ := New(Options{Recorder: recorder, Exporter: func(context.Context, []byte) error { return errors.New("SECRET exporter path") }})
	failed := failing.exportWork()(context.Background())
	if failed.Err == nil || failed.Err.Error() != "debug export failed" {
		t.Fatalf("unbounded export error escaped: %v", failed.Err)
	}
}

func TestProjectionFailureIsGuardedAndResizeHistoryIsBounded(t *testing.T) {
	recorder := uiRecorder(t)
	surface, _ := New(Options{Recorder: recorder})
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
	surface, _ := New(Options{Recorder: recorder})
	newest := responsive.Resolve(responsive.Size{Columns: 110, Rows: 24})
	older := responsive.Resolve(responsive.Size{Columns: 40, Rows: 10})
	surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: newest, Generation: 2})
	surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: older, Generation: 1})
	if surface.layout != newest || surface.resizeGeneration != 2 || len(surface.resizes) != 1 || surface.resizes[0].Generation != 2 {
		t.Fatalf("stale resize committed: layout=%+v g=%d history=%+v", surface.layout, surface.resizeGeneration, surface.resizes)
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
	surface, _ := New(Options{Recorder: recorder, Previews: previews, PageSize: 20})
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
		surface, _ := New(Options{Recorder: recorder, PageSize: 5})
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
	surface, _ := New(Options{Recorder: recorder, PageSize: 10})
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
	surface, _ := New(Options{Recorder: recorder})
	layout := responsive.Resolve(responsive.Size{Columns: 80, Rows: 18})
	surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: layout, Generation: 1})
	context := shell.RenderContext{Layout: layout, Theme: terminalTheme(t), ResizeGeneration: 1, Settled: true}
	first, _ := surface.Render(context)
	second, _ := surface.Render(context)
	if !bytes.Equal([]byte(frameText(first)), []byte(frameText(second))) {
		t.Fatal("identical state rendered differently")
	}
}
