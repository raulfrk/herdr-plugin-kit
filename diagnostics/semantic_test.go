package diagnostics_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
)

var _ diagnostics.SemanticSink = (*diagnostics.Recorder)(nil)

func TestTextFitEmptyCapAndZeroMeasurementsRoundTrip(t *testing.T) {
	if diagnostics.CloneTextFit(nil) != nil {
		t.Fatal("nil clone became measured")
	}
	empty := &diagnostics.TextFitReport{Observations: []diagnostics.TextFitObservation{}}
	cloned := diagnostics.CloneTextFit(empty)
	if cloned == nil || cloned.Observations == nil || len(cloned.Observations) != 0 {
		t.Fatal("empty clone lost measured distinction")
	}
	capped := &diagnostics.TextFitReport{Observations: make([]diagnostics.TextFitObservation, 256)}
	for i := range capped.Observations {
		capped.Observations[i] = diagnostics.TextFitObservation{Element: mustID(t, "field"), Instance: i, Intent: diagnostics.TextFitClip}
	}
	for _, report := range []*diagnostics.TextFitReport{empty, capped} {
		recorder := openRecorder(t, diagnostics.DefaultConfig(t.TempDir()))
		if err := recorder.RecordSemantic(diagnostics.SemanticEvent{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Code: mustID(t, "render.completed"), Outcome: diagnostics.OutcomeApplied, TextFit: report}); err != nil {
			t.Fatal(err)
		}
		page, err := recorder.Debug(diagnostics.DebugQuery{PageSize: 1})
		if err != nil || len(page.Events) != 1 || !reflect.DeepEqual(page.Events[0].TextFit, report) {
			t.Fatalf("roundtrip mismatch: %+v err=%v", page, err)
		}
	}
}

func TestTextFitIntegerSafeBoundaries(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("all platform ints already fit the exact JSON range")
	}
	safe := int64(1<<53 - 1)
	for _, field := range []string{"omitted", "instance", "original_columns", "layout_rows", "available_columns", "available_rows"} {
		t.Run(field, func(t *testing.T) {
			recorder := openRecorder(t, testConfig(t.TempDir()))
			for _, value := range []int64{safe, safe + 1, int64(^uint(0) >> 1)} {
				report := &diagnostics.TextFitReport{Observations: []diagnostics.TextFitObservation{{Element: mustID(t, "field"), Intent: diagnostics.TextFitClip}}}
				observation := &report.Observations[0]
				switch field {
				case "omitted":
					report.Omitted = int(value)
				case "instance":
					observation.Instance = int(value)
				case "original_columns":
					observation.OriginalColumns = int(value)
				case "layout_rows":
					observation.LayoutRows = int(value)
				case "available_columns":
					observation.AvailableColumns = int(value)
				case "available_rows":
					observation.AvailableRows = int(value)
				}
				err := recorder.RecordSemantic(diagnostics.SemanticEvent{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Code: mustID(t, "render.completed"), Outcome: diagnostics.OutcomeApplied, TextFit: report})
				if value == safe && err != nil || value > safe && err == nil {
					t.Fatalf("value %d admission err=%v", value, err)
				}
				page, pageErr := recorder.Debug(diagnostics.DebugQuery{PageSize: 10})
				if pageErr != nil || len(page.Events) != 1 {
					t.Fatalf("invalid admission count=%d err=%v", len(page.Events), pageErr)
				}
				if value == safe && !reflect.DeepEqual(page.Events[0].TextFit, report) {
					t.Fatal("max safe integer rounded")
				}
			}
		})
	}
}

func TestTextFitPublicSnapshotCopiesRemainIndependent(t *testing.T) {
	recorder := openRecorder(t, testConfig(t.TempDir()))
	want := diagnostics.TextFitReport{Observations: []diagnostics.TextFitObservation{{
		Element: mustID(t, "field"), Instance: 2, OriginalColumns: 8,
		AvailableColumns: 4, AvailableRows: 1, LayoutRows: 1,
		Intent: diagnostics.TextFitClip, Clipped: true,
	}}, Omitted: 3}
	if err := recorder.RecordSemantic(diagnostics.SemanticEvent{
		Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic,
		Code: mustID(t, "render.completed"), Outcome: diagnostics.OutcomeApplied, TextFit: &want,
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snapshot, err := recorder.DebugSnapshot(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	query := diagnostics.DebugQuery{PageSize: 1}
	selected, err := snapshot.Select(ctx, query, false)
	if err != nil {
		t.Fatal(err)
	}
	window := selected.Window(diagnostics.DebugWindowQuery{})
	window.Events[0].TextFit.Omitted = 99
	window.Events[0].TextFit.Observations[0].OriginalColumns = 99
	fresh := selected.Window(diagnostics.DebugWindowQuery{})
	second, err := snapshot.Select(ctx, query, false)
	if err != nil {
		t.Fatal(err)
	}
	page, err := recorder.Debug(query)
	if err != nil {
		t.Fatal(err)
	}
	for _, events := range [][]diagnostics.DebugEvent{fresh.Events, second.Window(diagnostics.DebugWindowQuery{}).Events, page.Events} {
		if len(events) != 1 || !reflect.DeepEqual(events[0].TextFit, &want) {
			t.Fatalf("public mutation changed captured/cached report: %+v", events)
		}
	}
	data, err := selected.ExportWindow(ctx, fresh, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var exported diagnostics.DebugReport
	if err := json.Unmarshal(data, &exported); err != nil {
		t.Fatal(err)
	}
	if len(exported.Events) != 1 || !reflect.DeepEqual(exported.Events[0].TextFit, &want) {
		t.Fatalf("export changed original report: %+v", exported.Events)
	}
}

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
	events := readEvents(t, directory)
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
	events := readEvents(t, directory)
	if len(events) != 1 || events[0].UISnapshot != nil {
		t.Fatalf("semantic events = %#v", events)
	}
}

func TestSemanticTextFitRoundTripsWithoutContentOrAliasing(t *testing.T) {
	directory := t.TempDir()
	recorder := openRecorder(t, testConfig(directory))
	observations := []diagnostics.TextFitObservation{{
		Element: mustID(t, "picker-title"), Instance: 2, OriginalColumns: 12, LayoutRows: 2,
		AvailableColumns: 8, AvailableRows: 1, Intent: diagnostics.TextFitWrap, Wrapped: true, Clipped: true,
	}}
	report := &diagnostics.TextFitReport{Observations: observations, Omitted: 3}
	if err := recorder.RecordSemantic(diagnostics.SemanticEvent{
		Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Code: mustID(t, "render.completed"),
		Outcome: diagnostics.OutcomeApplied, TextFit: report,
	}); err != nil {
		t.Fatal(err)
	}
	observations[0].OriginalColumns = 999

	page, err := recorder.Debug(diagnostics.DebugQuery{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].TextFit == nil {
		t.Fatalf("text-fit projection = %+v", page.Events)
	}
	got := page.Events[0].TextFit
	if got.Omitted != 3 || len(got.Observations) != 1 || got.Observations[0].OriginalColumns != 12 {
		t.Fatalf("text-fit report = %+v", got)
	}
	got.Observations[0].OriginalColumns = 777
	second, err := recorder.Debug(diagnostics.DebugQuery{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if second.Events[0].TextFit.Observations[0].OriginalColumns != 12 {
		t.Fatal("mutating a debug page changed recorder-owned text-fit data")
	}
	encoded, err := json.Marshal(page.Events[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"PRIVATE-TEXT-FIT", "snippet", "hash", "content"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("text-fit projection contains forbidden field %q: %s", forbidden, encoded)
		}
	}
}

func TestSemanticTextFitRejectsInvalidBoundsAndMetadata(t *testing.T) {
	recorder := openRecorder(t, testConfig(t.TempDir()))
	valid := diagnostics.TextFitObservation{Element: mustID(t, "field"), Intent: diagnostics.TextFitClip}
	tooMany := make([]diagnostics.TextFitObservation, diagnostics.MaxTextFitObservations+1)
	for i := range tooMany {
		tooMany[i] = valid
	}
	for _, test := range []struct {
		name   string
		report diagnostics.TextFitReport
	}{
		{"missing observations", diagnostics.TextFitReport{}},
		{"negative omitted", diagnostics.TextFitReport{Observations: []diagnostics.TextFitObservation{}, Omitted: -1}},
		{"too many", diagnostics.TextFitReport{Observations: tooMany}},
		{"zero element", diagnostics.TextFitReport{Observations: []diagnostics.TextFitObservation{{Intent: diagnostics.TextFitClip}}}},
		{"negative dimension", diagnostics.TextFitReport{Observations: []diagnostics.TextFitObservation{{Element: valid.Element, Intent: diagnostics.TextFitClip, AvailableRows: -1}}}},
		{"invalid intent", diagnostics.TextFitReport{Observations: []diagnostics.TextFitObservation{{Element: valid.Element, Intent: "scroll"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := recorder.RecordSemantic(diagnostics.SemanticEvent{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Code: mustID(t, "render.completed"), Outcome: diagnostics.OutcomeApplied, TextFit: &test.report})
			if err == nil {
				t.Fatal("RecordSemantic() accepted invalid text-fit report")
			}
		})
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

func TestSemanticIDJSONRoundTripAndLegacyZero(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want string
	}{
		{name: "valid string", wire: `"request-7"`, want: "request-7"},
		{name: "empty string", wire: `""`},
		{name: "legacy empty object", wire: `{}`},
		{name: "legacy empty object with whitespace", wire: " \n { } \t"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			id := mustID(t, "unchanged")
			if err := json.Unmarshal([]byte(test.wire), &id); err != nil {
				t.Fatalf("Unmarshal(%s): %v", test.wire, err)
			}
			if id.String() != test.want {
				t.Fatalf("decoded ID = %q, want %q", id.String(), test.want)
			}
			encoded, err := json.Marshal(id)
			if err != nil {
				t.Fatal(err)
			}
			wantWire, err := json.Marshal(test.want)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != string(wantWire) {
				t.Fatalf("encoded ID = %s, want %s", encoded, wantWire)
			}
		})
	}
}

func TestSemanticIDJSONRejectsInvalidInputWithoutMutation(t *testing.T) {
	tests := []string{
		`"Invalid ID"`, `null`, `7`, `[]`, `["valid"]`, `{"value":"valid"}`,
		`{ "value": "valid" }`, `{`, `"valid" trailing`, ``,
	}
	for _, wire := range tests {
		t.Run(wire, func(t *testing.T) {
			id := mustID(t, "unchanged")
			err := id.UnmarshalJSON([]byte(wire))
			if err == nil {
				t.Fatalf("Unmarshal(%s) succeeded", wire)
			}
			if err.Error() != "invalid semantic identifier JSON" {
				t.Fatalf("Unmarshal(%s) error = %q", wire, err)
			}
			if id.String() != "unchanged" {
				t.Fatalf("rejected input changed ID to %q", id.String())
			}
		})
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
