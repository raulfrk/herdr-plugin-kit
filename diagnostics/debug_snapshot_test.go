package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

type cancelAtErrCheck struct {
	context.Context
	cancel   context.CancelFunc
	calls    atomic.Int64
	cancelAt int64
}

func (ctx *cancelAtErrCheck) Err() error {
	if ctx.calls.Add(1) == ctx.cancelAt {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func newCancelAtErrCheck(t *testing.T, cancelAt int64) *cancelAtErrCheck {
	t.Helper()
	base, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &cancelAtErrCheck{Context: base, cancel: cancel, cancelAt: cancelAt}
}

func requireValidCancellation(t *testing.T, ctx context.Context) {
	t.Helper()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("context error = %v", ctx.Err())
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("context returned cancellation before Done closed")
	}
}

func TestDebugSnapshotSelectionValidationAndNilBoundaries(t *testing.T) {
	var nilSnapshot *DebugSnapshot
	if got := nilSnapshot.Health(); got != (DebugHealth{}) {
		t.Fatalf("nil snapshot health = %+v", got)
	}
	var nilView *DebugView
	if got := nilView.Health(); got != (DebugHealth{}) {
		t.Fatalf("nil view health = %+v", got)
	}
	if nilView.PreviewEligible(1) {
		t.Fatal("nil view reported preview eligibility")
	}
	if position, exact := nilView.Locate(1); position != -1 || exact {
		t.Fatalf("nil view locate = %d/%t", position, exact)
	}
	if position, exact := (&DebugView{}).Locate(1); position != -1 || exact {
		t.Fatalf("empty view locate = %d/%t", position, exact)
	}

	snapshot := &DebugSnapshot{health: DebugHealth{Writable: true}, maxReportBytes: 64 << 10}
	for _, test := range []struct {
		name  string
		query DebugQuery
		valid bool
	}{
		{"default", DebugQuery{}, true},
		{"minimum page", DebugQuery{PageSize: 1}, true},
		{"maximum page", DebugQuery{PageSize: MaxDebugPageSize}, true},
		{"page too small", DebugQuery{PageSize: -1}, false},
		{"page too large", DebugQuery{PageSize: MaxDebugPageSize + 1}, false},
		{"debug level", DebugQuery{Level: LevelDebug}, true},
		{"info level", DebugQuery{Level: LevelInfo}, true},
		{"warn level", DebugQuery{Level: LevelWarn}, true},
		{"error level", DebugQuery{Level: LevelError}, true},
		{"invalid level", DebugQuery{Level: Level("trace")}, false},
		{"lifecycle kind", DebugQuery{Kind: KindLifecycle}, true},
		{"interaction kind", DebugQuery{Kind: KindInteraction}, true},
		{"diagnostic kind", DebugQuery{Kind: KindDiagnostic}, true},
		{"invalid kind", DebugQuery{Kind: Kind("trace")}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			view, err := snapshot.Select(context.Background(), test.query, false)
			if test.valid && (err != nil || view == nil) {
				t.Fatalf("valid query failed: view=%v err=%v", view, err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid query succeeded")
			}
		})
	}
	view, err := snapshot.Select(context.Background(), DebugQuery{}, false)
	if err != nil || !view.Health().Writable {
		t.Fatalf("captured health = %+v, err=%v", view.Health(), err)
	}
}

func TestDebugSnapshotCancellationCheckpoints(t *testing.T) {
	refs := make([]*DebugEvent, 1025)
	for index := range refs {
		refs[index] = &DebugEvent{Sequence: uint64(index + 1), Session: debugID(t, "session")}
	}
	ctx := newCancelAtErrCheck(t, 1)
	if sessions := debugSessionsFromRefs(ctx, refs[:1]); sessions != nil {
		t.Fatalf("cancelled session indexing returned %+v", sessions)
	}
	requireValidCancellation(t, ctx)

	snapshot := &DebugSnapshot{events: refs, previewEntries: map[uint64]PreviewEntry{}, maxReportBytes: 64 << 10}
	ctx = newCancelAtErrCheck(t, 1)
	if _, err := snapshot.Select(ctx, DebugQuery{PageSize: 10}, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("selection cancellation error = %v", err)
	}
	requireValidCancellation(t, ctx)

	recorder := debugRecorder(t)
	recorder.records = []*storedEvent{{debug: refs[0]}}
	previews, err := OpenPreviewStore(t.TempDir(), DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = previews.Close() })
	ctx = newCancelAtErrCheck(t, 3)
	if _, err := recorder.DebugSnapshot(ctx, previews); !errors.Is(err, context.Canceled) {
		t.Fatalf("preview indexing cancellation error = %v", err)
	}
	requireValidCancellation(t, ctx)
}

func TestDebugSnapshotSelectRejectsPreCancelledSingleEvent(t *testing.T) {
	recorder := debugRecorder(t)
	recordDebugSemantic(t, recorder, "session", "component", "eligible", "", false)
	snapshot, err := recorder.DebugSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	query := DebugQuery{PageSize: 1}
	selectable, err := snapshot.Select(context.Background(), query, false)
	if err != nil || len(selectable.Window(DebugWindowQuery{}).Events) != 1 {
		t.Fatalf("background selection: view=%v err=%v", selectable, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	view, err := snapshot.Select(ctx, query, false)
	if !errors.Is(err, context.Canceled) || view != nil {
		t.Fatalf("pre-cancelled selection: view=%v err=%v", view, err)
	}
}

func TestDebugSnapshotWindowLocateAndPublicCopies(t *testing.T) {
	recorder := debugRecorder(t)
	for _, code := range []string{"oldest", "middle", "newest"} {
		recordDebugSemantic(t, recorder, "session", "component", code, "", true)
	}
	snapshot, err := recorder.DebugSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := snapshot.Select(context.Background(), DebugQuery{PageSize: 2}, false)
	if err != nil {
		t.Fatal(err)
	}
	window := view.Window(DebugWindowQuery{Start: 0})
	if window.Total != 3 || len(window.Events) != 2 || window.HasPrev || !window.HasNext || window.Events[0].Code.String() != "newest" {
		t.Fatalf("first window = %+v", window)
	}
	anchored := view.Window(DebugWindowQuery{TopSequence: 2})
	if anchored.Start != 1 || anchored.AnchorMissing || anchored.Events[0].Sequence != 2 {
		t.Fatalf("anchored window = %+v", anchored)
	}
	position, exact := view.Locate(4)
	if position != 0 || exact {
		t.Fatalf("nearest older for sequence 4 = %d/%t", position, exact)
	}
	position, exact = view.Locate(0)
	if position != 2 || exact {
		t.Fatalf("nearest newer for sequence 0 = %d/%t", position, exact)
	}
	window.Events[0].Sequence = 999
	window.Events[1].Visual.Screen = debugID(t, "mutated")
	again := view.Window(DebugWindowQuery{Start: 0})
	if again.Events[0].Sequence == 999 || again.Events[1].Visual.Screen.String() == "mutated" {
		t.Fatal("public window aliases the immutable projection")
	}
	sessions := snapshot.Sessions()
	sessions[0].Events = 0
	if snapshot.Sessions()[0].Events == 0 {
		t.Fatal("public sessions alias the immutable snapshot")
	}
}

func TestDebugSnapshotHidesOnlyDebuggerVisualsAndKeepsPageSize(t *testing.T) {
	recorder := debugRecorder(t)
	for _, item := range []struct{ code, screen string }{
		{"debugger", "debug.timeline"},
		{"plugin", "plugin.main"},
		{"plain", ""},
	} {
		event := SemanticEvent{Level: LevelInfo, Kind: KindDiagnostic, Plugin: debugID(t, "session"), Code: debugID(t, item.code), Outcome: OutcomeApplied}
		if item.screen != "" {
			event.Visual = &VisualState{Screen: debugID(t, item.screen), State: debugID(t, "ready")}
		}
		if err := recorder.RecordSemantic(event); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := recorder.DebugSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := snapshot.Select(context.Background(), DebugQuery{PageSize: 7, HideDebugger: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	window := view.Window(DebugWindowQuery{})
	if window.Total != 2 || window.Events[0].Code.String() != "plain" || window.Events[1].Code.String() != "plugin" {
		t.Fatalf("hidden-debugger view = %+v", window.Events)
	}
	page, err := recorder.Debug(DebugQuery{PageSize: 1, Page: 1})
	if err != nil || page.PageSize != 1 || page.Page != 1 || len(page.Events) != 1 {
		t.Fatalf("public paging changed: page=%+v err=%v", page, err)
	}
}

func TestDebugSnapshotSelectionCombinesVisibilityGalleryAndFilters(t *testing.T) {
	recorder := debugRecorder(t)
	previews, err := OpenPreviewStore(t.TempDir(), DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = previews.Close() })
	registered := previewState(t, "plugin.main")
	for _, event := range []SemanticEvent{
		{Level: LevelInfo, Kind: KindInteraction, Plugin: debugID(t, "session"), Component: debugID(t, "component"), Action: debugID(t, "open"), Code: debugID(t, "registered"), Correlation: debugID(t, "request"), Outcome: OutcomeApplied, Visual: &registered},
		{Level: LevelWarn, Kind: KindDiagnostic, Plugin: debugID(t, "other"), Code: debugID(t, "unregistered"), Outcome: OutcomeApplied, Visual: ptrVisual(previewState(t, "plugin.other"))},
		{Level: LevelError, Kind: KindLifecycle, Plugin: debugID(t, "session"), Code: debugID(t, "plain"), Outcome: OutcomeFailed},
		{Level: LevelInfo, Kind: KindInteraction, Plugin: debugID(t, "session"), Code: debugID(t, "debugger"), Outcome: OutcomeApplied, Visual: ptrVisual(previewState(t, "debug.timeline"))},
	} {
		sequence, recordErr := recorder.RecordSemanticWithSequence(event)
		if recordErr != nil {
			t.Fatal(recordErr)
		}
		if event.Code.String() == "registered" {
			if _, putErr := previews.Put(sequence, *event.Visual); putErr != nil {
				t.Fatal(putErr)
			}
		}
	}
	snapshot, err := recorder.DebugSnapshot(context.Background(), previews)
	if err != nil {
		t.Fatal(err)
	}
	query := DebugQuery{Session: debugID(t, "session"), Level: LevelInfo, Kind: KindInteraction, Component: debugID(t, "component"), Action: debugID(t, "open"), Code: debugID(t, "registered"), Correlation: debugID(t, "request"), PageSize: 10, HideDebugger: true}
	view, err := snapshot.Select(context.Background(), query, true)
	if err != nil {
		t.Fatal(err)
	}
	window := view.Window(DebugWindowQuery{})
	if len(window.Events) != 1 || window.Events[0].Code.String() != "registered" || !view.PreviewEligible(window.Events[0].Sequence) {
		t.Fatalf("combined selection = %+v", window.Events)
	}
	gallery, err := snapshot.Select(context.Background(), DebugQuery{PageSize: 10}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := gallery.Window(DebugWindowQuery{}).Events; len(got) != 1 || got[0].Code.String() != "registered" {
		t.Fatalf("gallery selection = %+v", got)
	}
}

func ptrVisual(value VisualState) *VisualState { return &value }

func TestDebugSnapshotExportUsesCapturedWindowAndRevalidatesPreviews(t *testing.T) {
	recorder := debugRecorder(t)
	previews, err := OpenPreviewStore(t.TempDir(), DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = previews.Close() })
	visual := previewState(t, "plugin.main")
	sequence, err := recorder.RecordSemanticWithSequence(SemanticEvent{Level: LevelInfo, Kind: KindInteraction, Plugin: debugID(t, "session"), Code: debugID(t, "captured"), Outcome: OutcomeApplied, Visual: &visual})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := previews.Put(sequence, visual); err != nil {
		t.Fatal(err)
	}
	snapshot, err := recorder.DebugSnapshot(context.Background(), previews)
	if err != nil {
		t.Fatal(err)
	}
	view, err := snapshot.Select(context.Background(), DebugQuery{PageSize: 10}, false)
	if err != nil {
		t.Fatal(err)
	}
	window := view.Window(DebugWindowQuery{})
	if err := recorder.RecordSemantic(SemanticEvent{Level: LevelInfo, Kind: KindDiagnostic, Plugin: debugID(t, "session"), Code: debugID(t, "later"), Outcome: OutcomeApplied}); err != nil {
		t.Fatal(err)
	}
	data, err := view.ExportWindow(context.Background(), window, 64<<10, previews)
	if err != nil {
		t.Fatal(err)
	}
	var report DebugReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Events) != 1 || report.Events[0].Code.String() != "captured" || len(report.Previews) != 1 {
		t.Fatalf("captured export = %+v", report)
	}
	if bytes.Contains(data, []byte("later")) {
		t.Fatal("export incorporated an event recorded after dispatch")
	}
	replacementStore, err := OpenPreviewStore(t.TempDir(), DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replacementStore.Close() })
	replacementVisual := previewState(t, "plugin.replacement")
	replacement, err := replacementStore.Put(sequence, replacementVisual)
	if err != nil {
		t.Fatal(err)
	}
	captured, ok := previews.Entry(sequence, visual)
	if !ok || replacement == captured {
		t.Fatalf("provenance fixture: captured=%+v replacement=%+v ok=%t", captured, replacement, ok)
	}
	changedWindow := window
	changedWindow.Events = append([]DebugEvent(nil), window.Events...)
	changedWindow.Events[0].Visual = ptrVisual(replacementVisual)
	changedData, err := view.ExportWindow(context.Background(), changedWindow, 64<<10, replacementStore)
	if err != nil {
		t.Fatal(err)
	}
	var changed DebugReport
	if err := json.Unmarshal(changedData, &changed); err != nil {
		t.Fatal(err)
	}
	if len(changed.Previews) != 0 || changed.PreviewsOmitted != 1 || len(changed.Events) != 1 || changed.Events[0].Visual == nil || changed.Events[0].Visual.Screen != replacementVisual.Screen {
		t.Fatalf("changed preview provenance export = %+v", changed)
	}
	if original := view.Window(DebugWindowQuery{}).Events[0].Visual; original == nil || original.Screen != visual.Screen {
		t.Fatalf("copied window mutation changed captured view: %+v", original)
	}
	defaultData, err := view.ExportWindow(context.Background(), window, 0, previews)
	if err != nil || !json.Valid(defaultData) {
		t.Fatalf("zero export limit did not use recorder default: bytes=%d err=%v", len(defaultData), err)
	}
	if _, err := view.ExportWindow(context.Background(), window, recorder.config.MaxReportBytes+1, previews); err == nil {
		t.Fatal("export accepted a limit above the recorder maximum")
	}
	if err := previews.Close(); err != nil {
		t.Fatal(err)
	}
	data, err = view.ExportWindow(context.Background(), window, 64<<10, previews)
	if err != nil {
		t.Fatal(err)
	}
	report = DebugReport{}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Previews) != 0 || report.PreviewsOmitted != 1 {
		t.Fatalf("stale preview was exported: %+v", report)
	}
}

func TestDebugViewExportOmitsOversizedSessionIndexAfterEvents(t *testing.T) {
	recorder := debugRecorder(t)
	semantic := map[string]any{"semantic_schema": semanticSchemaVersion, "outcome": string(OutcomeApplied)}
	recorder.records = nil
	for index := range 40 {
		recorder.records = append(recorder.records, cachedStoredEvent(t, Event{
			Sequence: uint64(index + 1), Time: time.Unix(int64(index), 0),
			Plugin: fmt.Sprintf("session-%02d-with-bounded-padding", index), Message: "event",
			Level: LevelInfo, Kind: KindDiagnostic, Details: semantic,
		}))
	}
	snapshot, err := recorder.DebugSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := snapshot.Select(context.Background(), DebugQuery{PageSize: 100}, false)
	if err != nil {
		t.Fatal(err)
	}
	window := view.Window(DebugWindowQuery{})
	wantFirst, wantLength := window.Events[0], len(window.Events)
	data, err := view.ExportWindow(context.Background(), window, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	var report DebugReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(data) > 1024 || !report.Truncated || len(report.Sessions) != 0 || !slices.Contains(report.TruncationReasons, "session_index_omitted") {
		t.Fatalf("bounded captured export retained session index: bytes=%d report=%+v", len(data), report)
	}
	if len(window.Events) != wantLength || window.Events[0] != wantFirst {
		t.Fatal("export mutated captured window")
	}
}

func TestDebugViewExportPreviewOmissionsTruncationAndInclusiveLimit(t *testing.T) {
	recorder := debugRecorder(t)
	previews, err := OpenPreviewStore(t.TempDir(), DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = previews.Close() })
	visuals := []VisualState{previewState(t, "plugin.newest"), previewState(t, "plugin.oldest")}
	for index := len(visuals) - 1; index >= 0; index-- {
		sequence, recordErr := recorder.RecordSemanticWithSequence(SemanticEvent{Level: LevelInfo, Kind: KindInteraction, Plugin: debugID(t, "session"), Code: debugID(t, fmt.Sprintf("event-%d", index)), Outcome: OutcomeApplied, Visual: &visuals[index]})
		if recordErr != nil {
			t.Fatal(recordErr)
		}
		if _, putErr := previews.Put(sequence, visuals[index]); putErr != nil {
			t.Fatal(putErr)
		}
	}
	snapshot, err := recorder.DebugSnapshot(context.Background(), previews)
	if err != nil {
		t.Fatal(err)
	}
	view, err := snapshot.Select(context.Background(), DebugQuery{PageSize: 10}, false)
	if err != nil {
		t.Fatal(err)
	}
	window := view.Window(DebugWindowQuery{})
	full, err := view.ExportWindow(context.Background(), window, 64<<10, previews)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) < 1024 {
		t.Fatalf("fixture too small for minimum export limit: %d", len(full))
	}
	exact, err := view.ExportWindow(context.Background(), window, len(full), previews)
	if err != nil || !bytes.Equal(exact, full) {
		t.Fatalf("inclusive exact-size export: bytes=%d err=%v", len(exact), err)
	}

	withoutStore, err := view.ExportWindow(context.Background(), window, 64<<10, nil)
	if err != nil {
		t.Fatal(err)
	}
	var omitted DebugReport
	if err := json.Unmarshal(withoutStore, &omitted); err != nil {
		t.Fatal(err)
	}
	if len(omitted.Previews) != 0 || omitted.PreviewsOmitted != len(window.Events) {
		t.Fatalf("nil-store omissions = %+v", omitted)
	}

	var truncated DebugReport
	found := false
	for limit := len(full) - 1; limit >= 1024; limit-- {
		data, exportErr := view.ExportWindow(context.Background(), window, limit, previews)
		if exportErr != nil || json.Unmarshal(data, &truncated) != nil {
			continue
		}
		if len(truncated.Events) == 1 {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no valid one-event truncation boundary found")
	}
	if len(truncated.Previews) != 1 || truncated.Previews[0].Sequence != truncated.Events[0].Sequence || !truncated.Truncated || !slices.Contains(truncated.TruncationReasons, "oldest_events_omitted") {
		t.Fatalf("truncated preview membership = %+v", truncated)
	}
}

func TestDebugCacheRollbackDoesNotPublishFailedRecord(t *testing.T) {
	recorder := debugRecorder(t)
	recorder.file.Close()
	err := recorder.RecordSemantic(SemanticEvent{Level: LevelInfo, Kind: KindDiagnostic, Plugin: debugID(t, "session"), Code: debugID(t, "failed"), Outcome: OutcomeApplied})
	if err == nil {
		t.Fatal("record unexpectedly succeeded after closing storage")
	}
	if len(recorder.records) != 0 {
		t.Fatalf("failed record published %d cached records", len(recorder.records))
	}
}
