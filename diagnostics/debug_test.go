package diagnostics

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"
)

func debugID(t *testing.T, value string) ID {
	t.Helper()
	id, err := NewID(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func debugRecorder(t *testing.T) *Recorder {
	t.Helper()
	config := DefaultConfig(t.TempDir())
	config.Now = func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) }
	recorder, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	return recorder
}

func recordDebugSemantic(t *testing.T, recorder *Recorder, plugin, component, code, correlation string, visual bool) {
	t.Helper()
	event := SemanticEvent{
		Level: LevelInfo, Kind: KindInteraction, Plugin: debugID(t, plugin),
		Component: debugID(t, component), Action: debugID(t, "open"), Code: debugID(t, code),
		Outcome: OutcomeApplied, Generation: 7, RelatedGeneration: 6, Count: 3,
		Geometry: Geometry{ReportedColumns: 80, ReportedRows: 24, RenderColumns: 80, RenderRows: 24},
	}
	if correlation != "" {
		event.Correlation = debugID(t, correlation)
	}
	if visual {
		event.Visual = &VisualState{Screen: debugID(t, "timeline"), Focus: debugID(t, "events"), State: debugID(t, "ready"), ItemCount: 3, SelectedIndex: 1}
	}
	if err := recorder.RecordSemantic(event); err != nil {
		t.Fatal(err)
	}
}

type debugTestReporter interface {
	Helper()
	Fatalf(string, ...any)
}

func cachedStoredEvent(t debugTestReporter, event Event) *storedEvent {
	t.Helper()
	projected, ok := projectDebugEvent(event)
	if !ok {
		t.Fatalf("event did not produce a safe debug projection: %+v", event)
	}
	return &storedEvent{event: event, debug: &projected}
}

func TestDebugProjectsCurrentRecorderNewestFirstAndPagesExactly(t *testing.T) {
	recorder := debugRecorder(t)
	for _, item := range []struct{ plugin, component, code string }{
		{"alpha", "one", "first"}, {"beta", "two", "second"}, {"alpha", "two", "third"},
	} {
		recordDebugSemantic(t, recorder, item.plugin, item.component, item.code, "request-1", item.code == "third")
	}

	page, err := recorder.Debug(DebugQuery{Session: debugID(t, "alpha"), PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Events) != 1 || page.Events[0].Code.String() != "third" || page.HasPrev || !page.HasNext {
		t.Fatalf("first page = %+v", page)
	}
	if page.Events[0].Visual == nil || page.Events[0].Visual.Screen.String() != "timeline" {
		t.Fatalf("semantic visual = %+v", page.Events[0].Visual)
	}
	second, err := recorder.Debug(DebugQuery{Session: debugID(t, "alpha"), Page: 1, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 1 || second.Events[0].Code.String() != "first" || !second.HasPrev || second.HasNext {
		t.Fatalf("second page = %+v", second)
	}
	if len(page.Sessions) != 2 || page.Sessions[0].ID.String() != "alpha" || page.Sessions[0].Events != 2 || page.Sessions[1].ID.String() != "beta" {
		t.Fatalf("sessions = %+v", page.Sessions)
	}
}

func TestDebugProjectionHasNoFreeFormOrRawFields(t *testing.T) {
	recorder := debugRecorder(t)
	canary := "SECRET_/private/path_raw-error"
	_, err := recorder.record(Event{
		Level: LevelError, Kind: KindDiagnostic, Plugin: "safe", Component: "component", Action: "open",
		CorrelationID: "request-1", Message: "event.failed",
		Details:    map[string]any{"outcome": "failed", "secret": canary, "duration_ns": 5},
		UISnapshot: &UISnapshot{Name: "unsafe", Text: canary},
		Screenshot: &ScreenshotRef{Name: canary, Path: canary, MediaType: canary, SHA256: canary},
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := recorder.Debug(DebugQuery{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), canary) || len(page.Events) != 0 {
		t.Fatalf("unsafe projection: %s", encoded)
	}
	if len(page.Sessions) != 0 {
		t.Fatalf("raw record entered session index: %+v", page.Sessions)
	}
	report, err := recorder.ExportDebug(DebugExportOptions{MaxBytes: 4096})
	if err != nil || strings.Contains(string(report), "safe") || strings.Contains(string(report), canary) {
		t.Fatalf("raw record entered safe export: %s, %v", report, err)
	}
	forbidden := map[string]bool{"Details": true, "UISnapshot": true, "Screenshot": true, "Path": true, "Error": true, "LastError": true, "Message": true}
	for _, typ := range []reflect.Type{reflect.TypeOf(DebugEvent{}), reflect.TypeOf(DebugHealth{})} {
		for index := range typ.NumField() {
			if forbidden[typ.Field(index).Name] {
				t.Fatalf("privacy-forbidden field %s exists on %s", typ.Field(index).Name, typ.Name())
			}
		}
	}
}

func TestDebugDropsNonStaticEventsAndInvalidSemanticVisual(t *testing.T) {
	recorder := debugRecorder(t)
	for _, event := range []Event{
		{Level: LevelInfo, Kind: KindDiagnostic, Plugin: "safe", Message: "contains user text"},
		{Level: LevelInfo, Kind: KindDiagnostic, Plugin: "safe", Message: "safe.code", UISnapshot: &UISnapshot{Name: "semantic-ui", Text: `{"screen":"/private/path"}`}},
	} {
		if _, err := recorder.record(event); err != nil {
			t.Fatal(err)
		}
	}
	page, err := recorder.Debug(DebugQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 || len(page.Events) != 0 {
		t.Fatalf("page = %+v", page)
	}
}

func TestDebugFilterAndPageBoundaries(t *testing.T) {
	recorder := debugRecorder(t)
	recordDebugSemantic(t, recorder, "alpha", "results", "request.completed", "request-7", false)
	tests := []DebugQuery{
		{Page: -1}, {PageSize: -1}, {PageSize: MaxDebugPageSize + 1},
		{Level: "fatal"}, {Kind: "raw"},
	}
	for _, query := range tests {
		if _, err := recorder.Debug(query); err == nil {
			t.Fatalf("Debug(%+v) succeeded", query)
		}
	}
	page, err := recorder.Debug(DebugQuery{
		Component: debugID(t, "results"), Action: debugID(t, "open"), Code: debugID(t, "request.completed"),
		Correlation: debugID(t, "request-7"), Level: LevelInfo, Kind: KindInteraction, Page: math.MaxInt, PageSize: MaxDebugPageSize,
	})
	if err != nil || page.Total != 1 || len(page.Events) != 0 || !page.HasPrev || page.HasNext {
		t.Fatalf("oversize page = %+v, %v", page, err)
	}
}

func TestDebugExactPageAndExportBoundaries(t *testing.T) {
	recorder := debugRecorder(t)
	visual := previewState(t, "timeline")
	for _, code := range []string{"first", "second", "third"} {
		if err := recorder.RecordSemantic(SemanticEvent{
			Level: LevelInfo, Kind: KindInteraction, Plugin: debugID(t, "session"),
			Code: debugID(t, code), Outcome: OutcomeApplied, Visual: &visual,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for pageNumber, wantNext := range []bool{true, true, false, false} {
		page, err := recorder.Debug(DebugQuery{Page: pageNumber, PageSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		wantEvents := 1
		if pageNumber == 3 {
			wantEvents = 0
		}
		if page.HasNext != wantNext || len(page.Events) != wantEvents {
			t.Fatalf("page %d = %+v", pageNumber, page)
		}
	}

	full, err := recorder.ExportDebug(DebugExportOptions{MaxBytes: recorder.config.MaxReportBytes})
	if err != nil {
		t.Fatal(err)
	}
	exact, err := recorder.ExportDebug(DebugExportOptions{MaxBytes: len(full)})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(exact, full) {
		t.Fatal("an exact byte limit changed the report")
	}
	if _, err := recorder.ExportDebug(DebugExportOptions{MaxBytes: 1023}); err == nil {
		t.Fatal("report accepted a limit below the documented minimum")
	}
	if _, err := recorder.ExportDebug(DebugExportOptions{MaxBytes: recorder.config.MaxReportBytes + 1}); err == nil {
		t.Fatal("report accepted a limit above the recorder maximum")
	}
	var report DebugReport
	if data, err := recorder.ExportDebug(DebugExportOptions{MaxBytes: recorder.config.MaxReportBytes}); err != nil {
		t.Fatal(err)
	} else if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.PreviewsOmitted != 3 {
		t.Fatalf("previews omitted = %d, want 3", report.PreviewsOmitted)
	}
}

func TestDebugReportJSONPreservesNestedSemanticIDs(t *testing.T) {
	recorder := debugRecorder(t)
	event := SemanticEvent{
		Level: LevelInfo, Kind: KindInteraction,
		Plugin: debugID(t, "session-a"), Component: debugID(t, "results"),
		Action: debugID(t, "open"), Correlation: debugID(t, "request-7"),
		Code: debugID(t, "request.completed"), Outcome: OutcomeApplied,
		Visual: &VisualState{
			Screen: debugID(t, "timeline"), Focus: debugID(t, "events"),
			Selection: debugID(t, "event-3"), State: debugID(t, "ready"),
		},
	}
	if err := recorder.RecordSemantic(event); err != nil {
		t.Fatal(err)
	}

	encoded, err := recorder.ExportDebug(DebugExportOptions{MaxBytes: recorder.config.MaxReportBytes})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Sessions []struct {
			ID json.RawMessage `json:"ID"`
		} `json:"sessions"`
		Events []struct {
			Session, Component, Action, Code, Correlation json.RawMessage
			Visual                                        map[string]json.RawMessage
		} `json:"events"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Sessions) != 1 || len(wire.Events) != 1 {
		t.Fatalf("export shape = sessions %d events %d", len(wire.Sessions), len(wire.Events))
	}
	want := map[string]string{
		"session ID": string(wire.Sessions[0].ID),
		"session":    string(wire.Events[0].Session), "component": string(wire.Events[0].Component),
		"action": string(wire.Events[0].Action), "code": string(wire.Events[0].Code),
		"correlation":      string(wire.Events[0].Correlation),
		"visual screen":    string(wire.Events[0].Visual["Screen"]),
		"visual focus":     string(wire.Events[0].Visual["Focus"]),
		"visual selection": string(wire.Events[0].Visual["Selection"]),
		"visual state":     string(wire.Events[0].Visual["State"]),
	}
	for name, got := range want {
		if got == "{}" || len(got) < 2 || got[0] != '"' || got[len(got)-1] != '"' {
			t.Errorf("%s JSON = %s, want string", name, got)
		}
	}

	var decoded DebugReport
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	gotEvent := decoded.Events[0]
	if decoded.Sessions[0].ID.String() != "session-a" || gotEvent.Session.String() != "session-a" ||
		gotEvent.Component.String() != "results" || gotEvent.Action.String() != "open" ||
		gotEvent.Code.String() != "request.completed" || gotEvent.Correlation.String() != "request-7" ||
		gotEvent.Visual == nil || gotEvent.Visual.Screen.String() != "timeline" ||
		gotEvent.Visual.Focus.String() != "events" || gotEvent.Visual.Selection.String() != "event-3" ||
		gotEvent.Visual.State.String() != "ready" {
		t.Fatalf("decoded report IDs = %#v", decoded)
	}
}

func TestDebugSessionsTrackFirstLastAndStableOrder(t *testing.T) {
	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	semantic := map[string]any{"semantic_schema": semanticSchemaVersion, "outcome": "applied"}
	recorder := &Recorder{health: Health{Writable: true}, records: []*storedEvent{
		cachedStoredEvent(t, Event{Sequence: 1, Time: base.Add(2 * time.Minute), Plugin: "beta", Message: "event", Level: LevelInfo, Kind: KindDiagnostic, Details: semantic}),
		cachedStoredEvent(t, Event{Sequence: 2, Time: base, Plugin: "alpha", Message: "event", Level: LevelInfo, Kind: KindDiagnostic, Details: semantic}),
		cachedStoredEvent(t, Event{Sequence: 3, Time: base.Add(2 * time.Minute), Plugin: "alpha", Message: "event", Level: LevelInfo, Kind: KindDiagnostic, Details: semantic}),
	}}
	page, err := recorder.Debug(DebugQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 2 || page.Sessions[0].ID.String() != "alpha" || page.Sessions[1].ID.String() != "beta" {
		t.Fatalf("stable session order = %+v", page.Sessions)
	}
	alpha := page.Sessions[0]
	if alpha.Events != 2 || !alpha.First.Equal(base) || !alpha.Last.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("alpha session bounds = %+v", alpha)
	}
}

func TestDebugSessionIndexOmitsZeroSessionAndDropsBeforeOversizeExport(t *testing.T) {
	semantic := map[string]any{"semantic_schema": semanticSchemaVersion, "outcome": "applied"}
	recorder := &Recorder{
		config:   Config{MaxReportBytes: 1 << 20},
		reportAt: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
		health:   Health{Writable: true},
		records: []*storedEvent{cachedStoredEvent(t, Event{
			Sequence: 1, Time: time.Now(), Message: "event", Level: LevelInfo, Kind: KindDiagnostic, Details: semantic,
		})},
	}
	page, err := recorder.Debug(DebugQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Sessions) != 0 {
		t.Fatalf("zero-session projection = %+v", page)
	}

	recorder.records = nil
	for index := range 40 {
		recorder.records = append(recorder.records, cachedStoredEvent(t, Event{
			Sequence: uint64(index + 1), Time: time.Unix(int64(index), 0),
			Plugin: fmt.Sprintf("session-%02d-with-bounded-padding", index), Message: "event",
			Level: LevelInfo, Kind: KindDiagnostic, Details: semantic,
		}))
	}
	data, err := recorder.ExportDebug(DebugExportOptions{Query: DebugQuery{PageSize: 100}, MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	var report DebugReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if !report.Truncated || len(report.Sessions) != 0 || !slices.Contains(report.TruncationReasons, "session_index_omitted") {
		t.Fatalf("bounded report retained oversized session index: %+v", report)
	}
}

func TestDebugNumericProjectionRejectsLossAndNegatives(t *testing.T) {
	tests := []struct {
		value any
		want  int64
		ok    bool
	}{
		{int(0), 0, true}, {int64(-1), -1, true}, {uint64(math.MaxInt64), math.MaxInt64, true},
		{uint64(math.MaxInt64) + 1, 0, false}, {float64(42), 42, true}, {42.5, 0, false}, {"42", 0, false},
	}
	for _, test := range tests {
		got, ok := int64Detail(test.value)
		if got != test.want || ok != test.ok {
			t.Fatalf("int64Detail(%T(%v)) = %d/%t, want %d/%t", test.value, test.value, got, ok, test.want, test.ok)
		}
	}
	if value, ok := unsignedDetail(int64(-1)); ok || value != 0 {
		t.Fatalf("negative unsigned detail = %d/%t", value, ok)
	}
	if value, ok := unsignedDetail(int64(0)); !ok || value != 0 {
		t.Fatalf("zero unsigned detail = %d/%t", value, ok)
	}
	if value, ok := intDetail(int64(-1)); ok || value != -1 {
		t.Fatalf("negative int detail = %d/%t", value, ok)
	}
	if value, ok := intDetail(uint64(math.MaxInt64) + 1); ok || value != 0 {
		t.Fatalf("overflowing int detail = %d/%t", value, ok)
	}
	if value, ok := intDetail(int64(0)); !ok || value != 0 {
		t.Fatalf("zero int detail = %d/%t", value, ok)
	}

	event := DebugEvent{}
	projectSemanticDetails(&event, map[string]any{"duration_ns": int64(-1)})
	if event.Duration != 0 {
		t.Fatalf("negative duration = %s", event.Duration)
	}
	projectSemanticDetails(&event, map[string]any{"duration_ns": int64(17)})
	if event.Duration != 17*time.Nanosecond {
		t.Fatalf("duration = %s", event.Duration)
	}
	projectSemanticDetails(&event, map[string]any{"duration_ns": int64(0)})
	if event.Duration != 0 {
		t.Fatalf("zero duration = %s", event.Duration)
	}
}

func TestDebugPagingMatchesSliceModel(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		count := rapid.IntRange(0, 80).Draw(t, "count")
		pageSize := rapid.IntRange(1, MaxDebugPageSize).Draw(t, "page-size")
		pageNumber := rapid.IntRange(0, 100).Draw(t, "page")
		recorder := &Recorder{health: Health{Writable: true, MaxEvents: 100, MaxBytes: 1 << 20}}
		for index := range count {
			recorder.records = append(recorder.records, cachedStoredEvent(t, Event{
				Version: EventSchemaVersion, Sequence: uint64(index + 1), Time: time.Unix(int64(index), 0),
				Level: LevelInfo, Kind: KindDiagnostic, Plugin: "session", Message: "event-" + string(rune('a'+index%26)),
				Details: map[string]any{"semantic_schema": semanticSchemaVersion, "outcome": "applied"},
			}))
		}
		page, err := recorder.Debug(DebugQuery{Page: pageNumber, PageSize: pageSize})
		if err != nil {
			t.Fatal(err)
		}
		start := min(pageNumber*pageSize, count)
		end := min(start+pageSize, count)
		if page.Total != count || len(page.Events) != end-start || page.HasNext != (end < count) || page.HasPrev != (pageNumber > 0) {
			t.Fatalf("count=%d query=%d/%d page=%+v", count, pageNumber, pageSize, page)
		}
	})
}

func TestDebugPageStartClampsWithoutOverflow(t *testing.T) {
	for _, test := range []struct {
		page, size, total int
		want              int64
	}{
		{page: 0, size: 10, total: 5, want: 0},
		{page: 2, size: 2, total: 5, want: 4},
		{page: 3, size: 2, total: 5, want: 5},
		{page: math.MaxInt, size: math.MaxInt, total: 5, want: 5},
	} {
		if got := debugPageStart(test.page, test.size, test.total); got != test.want {
			t.Fatalf("debugPageStart(%d, %d, %d) = %d, want %d", test.page, test.size, test.total, got, test.want)
		}
	}
}

func TestDebugConcurrentRecordProjectionHealthAndExport(t *testing.T) {
	recorder := debugRecorder(t)
	const workers = 4
	const events = 25
	var wait sync.WaitGroup
	for worker := range workers {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			plugin, _ := NewID("session")
			code, _ := NewID("worker")
			for range events {
				if err := recorder.RecordSemantic(SemanticEvent{Level: LevelInfo, Kind: KindDiagnostic, Plugin: plugin, Code: code, Outcome: OutcomeApplied, Count: worker}); err != nil {
					t.Error(err)
				}
			}
		}(worker)
	}
	for range events {
		if _, err := recorder.Debug(DebugQuery{PageSize: 7}); err != nil {
			t.Fatal(err)
		}
		_ = recorder.Health()
		if _, err := recorder.Export(ReportOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	wait.Wait()
	page, err := recorder.Debug(DebugQuery{})
	if err != nil || page.Total != workers*events {
		t.Fatalf("final page total=%d err=%v", page.Total, err)
	}
}
