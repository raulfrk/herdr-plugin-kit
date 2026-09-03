package diagnostics_test

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
)

var _ diagnostics.SemanticSink = (*diagnostics.Recorder)(nil)

func mustID(t *testing.T, value string) diagnostics.ID {
	t.Helper()
	id, err := diagnostics.NewID(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSemanticRecordPersistsCompleteAllowlistedProjection(t *testing.T) {
	directory := t.TempDir()
	recorder := openRecorder(t, testConfig(directory))
	err := recorder.RecordSemantic(diagnostics.SemanticEvent{
		Level: diagnostics.LevelInfo, Kind: diagnostics.KindInteraction,
		Plugin: mustID(t, "sample"), Component: mustID(t, "results"),
		Action: mustID(t, "resize"), Correlation: mustID(t, "request-7"),
		Code: mustID(t, "resize.settled"), Outcome: diagnostics.OutcomeApplied,
		Before: mustID(t, "loading"), After: mustID(t, "ready"),
		Generation: 4, RelatedGeneration: 3,
		Count: 12, Bytes: 144, Graphemes: 36,
		Alt: true, Paste: true, Duration: 25 * time.Millisecond,
		Geometry: diagnostics.Geometry{ReportedColumns: 80, ReportedRows: 24, RenderColumns: 78, RenderRows: 22},
		Visual: &diagnostics.VisualState{
			Screen: mustID(t, "search"), Focus: mustID(t, "query"),
			Selection: mustID(t, "result-2"), State: mustID(t, "ready"),
			Geometry:          diagnostics.Geometry{ReportedColumns: 70, ReportedRows: 10, RenderColumns: 68, RenderRows: 8},
			ResizeGeneration:  4,
			RequestGeneration: 7,
			ItemCount:         12,
			SelectedIndex:     2,
			Pending:           true,
			HasError:          true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, filepath.Join(directory, diagnostics.EventLogName))
	if len(events) != 1 {
		t.Fatalf("semantic events = %#v", events)
	}
	event := events[0]
	if event.Level != diagnostics.LevelInfo || event.Kind != diagnostics.KindInteraction ||
		event.Plugin != "sample" || event.Component != "results" || event.Action != "resize" ||
		event.CorrelationID != "request-7" || event.Message != "resize.settled" {
		t.Fatalf("semantic event IDs = %#v", event)
	}
	wantDetails := map[string]any{
		"semantic_schema": float64(1),
		"outcome":         "applied", "generation": float64(4), "related_generation": float64(3),
		"count": float64(12), "bytes": float64(144), "graphemes": float64(36),
		"alt": true, "paste": true, "duration_ns": float64(25 * time.Millisecond),
		"reported_columns": float64(80), "reported_rows": float64(24),
		"render_columns": float64(78), "render_rows": float64(22),
		"state_before": "loading", "state_after": "ready",
	}
	if !reflect.DeepEqual(event.Details, wantDetails) {
		t.Fatalf("semantic details = %#v, want %#v", event.Details, wantDetails)
	}
	if event.UISnapshot == nil || event.UISnapshot.Name != "semantic-ui" {
		t.Fatalf("semantic UI snapshot = %#v", event.UISnapshot)
	}
	var visual struct {
		Screen    string `json:"screen"`
		Focus     string `json:"focus"`
		Selection string `json:"selection"`
		State     string `json:"state"`
		Geometry  struct {
			ReportedColumns int `json:"reported_columns"`
			ReportedRows    int `json:"reported_rows"`
			RenderColumns   int `json:"render_columns"`
			RenderRows      int `json:"render_rows"`
		} `json:"geometry"`
		ResizeGeneration  uint64 `json:"resize_generation"`
		RequestGeneration uint64 `json:"request_generation"`
		ItemCount         int    `json:"item_count"`
		SelectedIndex     int    `json:"selected_index"`
		Pending           bool   `json:"pending"`
		HasError          bool   `json:"has_error"`
	}
	if err := json.Unmarshal([]byte(event.UISnapshot.Text), &visual); err != nil {
		t.Fatalf("decode visual projection: %v", err)
	}
	if visual.Screen != "search" || visual.Focus != "query" || visual.Selection != "result-2" || visual.State != "ready" ||
		visual.Geometry.ReportedColumns != 70 || visual.Geometry.ReportedRows != 10 ||
		visual.Geometry.RenderColumns != 68 || visual.Geometry.RenderRows != 8 ||
		visual.ResizeGeneration != 4 || visual.RequestGeneration != 7 ||
		visual.ItemCount != 12 || visual.SelectedIndex != 2 || !visual.Pending || !visual.HasError {
		t.Fatalf("visual projection = %#v", visual)
	}
	for _, forbidden := range []string{"query text", "/private/path", "secret value"} {
		if strings.Contains(event.UISnapshot.Text, forbidden) {
			t.Fatalf("projection contains %q", forbidden)
		}
	}
}

func TestSemanticRecordAcceptsEventWithoutVisualState(t *testing.T) {
	directory := t.TempDir()
	recorder := openRecorder(t, testConfig(directory))
	if err := recorder.RecordSemantic(diagnostics.SemanticEvent{
		Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic,
		Code: mustID(t, "render.completed"), Outcome: diagnostics.OutcomeApplied,
	}); err != nil {
		t.Fatalf("RecordSemantic() rejected event without visual state: %v", err)
	}
	events := readEvents(t, filepath.Join(directory, diagnostics.EventLogName))
	if len(events) != 1 || events[0].UISnapshot != nil {
		t.Fatalf("semantic events = %#v", events)
	}
}

func TestSemanticIdentifiersEnforceFormatAndBounds(t *testing.T) {
	for _, value := range []string{"", "Query text", "/private/path", "contains secret", strings.Repeat("a", 65)} {
		if _, err := diagnostics.NewID(value); err == nil {
			t.Fatalf("NewID(%q) succeeded", value)
		}
	}
	if _, err := diagnostics.NewID("private-path"); err != nil {
		t.Fatalf("grammar-valid static ID rejected: %v", err)
	}
}

func TestSemanticRecordRejectsInvalidMeasurements(t *testing.T) {
	code := mustID(t, "render.completed")
	tests := []struct {
		name    string
		value   int
		wantErr bool
		set     func(*diagnostics.SemanticEvent, int)
	}{
		{name: "count negative", value: -1, wantErr: true, set: func(event *diagnostics.SemanticEvent, value int) { event.Count = value }},
		{name: "count zero", set: func(event *diagnostics.SemanticEvent, value int) { event.Count = value }},
		{name: "count valid", value: 1, set: func(event *diagnostics.SemanticEvent, value int) { event.Count = value }},
		{name: "bytes negative", value: -1, wantErr: true, set: func(event *diagnostics.SemanticEvent, value int) { event.Bytes = value }},
		{name: "bytes zero", set: func(event *diagnostics.SemanticEvent, value int) { event.Bytes = value }},
		{name: "bytes valid", value: 1, set: func(event *diagnostics.SemanticEvent, value int) { event.Bytes = value }},
		{name: "graphemes negative", value: -1, wantErr: true, set: func(event *diagnostics.SemanticEvent, value int) { event.Graphemes = value }},
		{name: "graphemes zero", set: func(event *diagnostics.SemanticEvent, value int) { event.Graphemes = value }},
		{name: "graphemes valid", value: 1, set: func(event *diagnostics.SemanticEvent, value int) { event.Graphemes = value }},
		{name: "duration negative", value: -1, wantErr: true, set: func(event *diagnostics.SemanticEvent, value int) { event.Duration = time.Duration(value) }},
		{name: "duration zero", set: func(event *diagnostics.SemanticEvent, value int) { event.Duration = time.Duration(value) }},
		{name: "duration valid", value: 1, set: func(event *diagnostics.SemanticEvent, value int) { event.Duration = time.Duration(value) }},
		{name: "visual item count negative", value: -1, wantErr: true, set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.ItemCount = value }},
		{name: "visual item count zero", set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.ItemCount = value }},
		{name: "visual item count valid", value: 1, set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.ItemCount = value }},
		{name: "visual selected index negative", value: -1, wantErr: true, set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.SelectedIndex = value }},
		{name: "visual selected index zero", set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.SelectedIndex = value }},
		{name: "visual selected index valid", value: 1, set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.SelectedIndex = value }},
		{name: "visual reported columns negative", value: -1, wantErr: true, set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.Geometry.ReportedColumns = value }},
		{name: "visual reported columns zero", set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.Geometry.ReportedColumns = value }},
		{name: "visual reported columns valid", value: 1, set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.Geometry.ReportedColumns = value }},
		{name: "visual reported rows negative", value: -1, wantErr: true, set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.Geometry.ReportedRows = value }},
		{name: "visual reported rows zero", set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.Geometry.ReportedRows = value }},
		{name: "visual reported rows valid", value: 1, set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.Geometry.ReportedRows = value }},
		{name: "visual render columns negative", value: -1, wantErr: true, set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.Geometry.RenderColumns = value }},
		{name: "visual render columns zero", set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.Geometry.RenderColumns = value }},
		{name: "visual render columns valid", value: 1, set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.Geometry.RenderColumns = value }},
		{name: "visual render rows negative", value: -1, wantErr: true, set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.Geometry.RenderRows = value }},
		{name: "visual render rows zero", set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.Geometry.RenderRows = value }},
		{name: "visual render rows valid", value: 1, set: func(event *diagnostics.SemanticEvent, value int) { event.Visual.Geometry.RenderRows = value }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := openRecorder(t, testConfig(t.TempDir()))
			event := diagnostics.SemanticEvent{
				Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic,
				Code: code, Outcome: diagnostics.OutcomeApplied, Visual: &diagnostics.VisualState{},
			}
			test.set(&event, test.value)
			err := recorder.RecordSemantic(event)
			if test.wantErr {
				if err == nil {
					t.Fatal("RecordSemantic() succeeded")
				}
				if health := recorder.Health(); health.Events != 0 || health.Bytes != 0 {
					t.Fatalf("rejected event changed recorder health: %#v", health)
				}
				return
			}
			if err != nil {
				t.Fatalf("RecordSemantic() rejected boundary value: %v", err)
			}
			if health := recorder.Health(); health.Events != 1 || health.Bytes == 0 {
				t.Fatalf("accepted event recorder health: %#v", health)
			}
		})
	}
}
