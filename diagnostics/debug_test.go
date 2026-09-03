package diagnostics

import (
	"encoding/json"
	"math"
	"reflect"
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

func TestDebugPagingMatchesSliceModel(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		count := rapid.IntRange(0, 80).Draw(t, "count")
		pageSize := rapid.IntRange(1, MaxDebugPageSize).Draw(t, "page-size")
		pageNumber := rapid.IntRange(0, 100).Draw(t, "page")
		recorder := &Recorder{health: Health{Writable: true, MaxEvents: 100, MaxBytes: 1 << 20}}
		for index := range count {
			recorder.records = append(recorder.records, storedEvent{event: Event{
				Version: EventSchemaVersion, Sequence: uint64(index + 1), Time: time.Unix(int64(index), 0),
				Level: LevelInfo, Kind: KindDiagnostic, Plugin: "session", Message: "event-" + string(rune('a'+index%26)),
				Details: map[string]any{"semantic_schema": semanticSchemaVersion, "outcome": "applied"},
			}})
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
