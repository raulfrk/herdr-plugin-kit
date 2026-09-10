package diagnostics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/ui/snapshot"
)

func TestSyncDirectoryReportsMissingPath(t *testing.T) {
	err := syncDirectory(filepath.Join(t.TempDir(), "missing"))
	if err == nil || !os.IsNotExist(err) {
		t.Fatalf("syncDirectory() error = %v", err)
	}
}

func TestTextFitProjectionRejectsExplicitNullFields(t *testing.T) {
	for _, field := range []string{"omitted", "instance", "original_columns", "layout_rows", "available_columns", "available_rows", "wrapped", "truncated", "clipped", "allow_truncation"} {
		for _, alias := range []string{field, strings.ToUpper(field), strings.ToUpper(field[:1]) + field[1:]} {
			t.Run(alias, func(t *testing.T) {
				observation := map[string]any{"element": "field", "intent": "clip"}
				report := map[string]any{"observations": []any{observation}}
				if field == "omitted" {
					report[alias] = nil
				} else {
					observation[alias] = nil
				}
				event := DebugEvent{}
				projectSemanticDetails(&event, map[string]any{"text_fit": report})
				if event.TextFit != nil {
					t.Fatalf("explicit null %s projected as measured", alias)
				}
			})
		}
	}
	for _, report := range []any{
		map[string]any{"observations": []any{}},
		map[string]any{"observations": []any{map[string]any{"element": "field", "intent": "clip"}}},
	} {
		event := DebugEvent{}
		projectSemanticDetails(&event, map[string]any{"text_fit": report})
		if event.TextFit == nil {
			t.Fatal("valid missing optional fields rejected")
		}
	}
	event := DebugEvent{}
	projectSemanticDetails(&event, map[string]any{"text_fit": map[string]any{
		"observations": []any{map[string]any{"element": "field", "intent": "clip", "CLIPPED": true}},
	}})
	if event.TextFit == nil || len(event.TextFit.Observations) != 1 || !event.TextFit.Observations[0].Clipped {
		t.Fatal("valid aliased CLIPPED value was not preserved")
	}
}

func TestTextFitProjectionRejectsUnsafeIntegers(t *testing.T) {
	for _, field := range []string{"omitted", "instance", "original_columns", "layout_rows", "available_columns", "available_rows"} {
		for _, value := range []uint64{1 << 53, uint64(^uint(0) >> 1)} {
			if value <= 1<<53-1 {
				continue
			}
			observation := map[string]any{"element": "field", "intent": "clip"}
			report := map[string]any{"observations": []any{observation}}
			if field == "omitted" {
				report[field] = value
			} else {
				observation[field] = value
			}
			event := DebugEvent{}
			projectSemanticDetails(&event, map[string]any{"text_fit": report})
			if event.TextFit != nil {
				t.Fatalf("unsafe raw %s=%d admitted", field, value)
			}
		}
	}
}

func TestRedactRepeatedKeysPreservesValuesCollisionsAndCopies(t *testing.T) {
	input := map[string]any{
		"rows": []any{
			map[string]any{"safe": "token=one", "auth.token": "first-secret"},
			map[string]any{"safe": "visible", "auth.token": "second-secret"},
		},
		"TOKEN=one": "first",
		"TOKEN=two": "second",
	}
	before, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	got, err := redactMetadata(input)
	if err != nil {
		t.Fatal(err)
	}
	rows := got["rows"].([]any)
	if rows[0].(map[string]any)["safe"] != snapshot.Redact("token=one") || rows[1].(map[string]any)["safe"] != "visible" {
		t.Fatalf("repeated key reused a value: %+v", rows)
	}
	for _, row := range rows {
		if row.(map[string]any)["auth.token"] != snapshot.Replacement {
			t.Fatalf("dotted sensitive key not redacted: %+v", row)
		}
	}
	if len(got) != 2 || got[snapshot.Redact("TOKEN=one")] != "second" {
		t.Fatalf("sorted collision winner changed: %+v", got)
	}
	rows[0].(map[string]any)["safe"] = "mutated"
	rows[1] = nil
	got["extra"] = true
	after, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("redacted result aliases input")
	}
	input["rows"].([]any)[0].(map[string]any)["safe"] = "changed"
	again := redactValue(input).(map[string]any)
	if again["rows"].([]any)[0].(map[string]any)["safe"] != "changed" {
		t.Fatal("redaction retained values across calls")
	}
}

func TestFitReportKeepsSingleMetadataSourceUntilExactOmissionBoundary(t *testing.T) {
	for _, test := range []struct {
		name string
		set  func(*Report)
	}{
		{"config metadata", func(report *Report) { report.ConfigMetadata = map[string]any{"padding": strings.Repeat("c", 400)} }},
		{"manifest metadata", func(report *Report) { report.ManifestMetadata = map[string]any{"padding": strings.Repeat("m", 400)} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := Report{
				Version: ReportSchemaVersion, GeneratedAt: "2026-09-03T10:00:00.000000000Z",
				Health: Health{Writable: true, MaxEvents: 20, MaxBytes: 32 << 10},
				Events: []Event{{Version: EventSchemaVersion, Sequence: 1, Time: time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC), Level: LevelInfo, Kind: KindDiagnostic, Message: ""}},
			}
			addReason(&report, "metadata_omitted")
			padReportToSize(t, &report, 1024, func(value string) { report.Events[0].Message = value })
			report.Truncated = false
			report.TruncationReasons = nil
			test.set(&report)

			encoded, err := fitReport(&report, 1024)
			if err != nil {
				t.Fatalf("fitReport() error = %v", err)
			}
			if len(encoded) != 1024 || report.ConfigMetadata != nil || report.ManifestMetadata != nil || len(report.Events) != 1 || !hasOnlyReason(report.TruncationReasons, "metadata_omitted") {
				t.Fatalf("exact metadata omission = size %d report %#v", len(encoded), report)
			}
		})
	}
}

func TestFitReportAcceptsExactMinimumEnvelopeAfterLastEventOmission(t *testing.T) {
	report := Report{
		Version: ReportSchemaVersion, GeneratedAt: "2026-09-03T10:00:00.000000000Z",
		Health: Health{Writable: false, MaxEvents: 20, MaxBytes: 32 << 10},
	}
	addReason(&report, "oversized_last_event_omitted")
	padReportToSize(t, &report, 1024, func(value string) { report.Health.LastError = value })
	report.Truncated = false
	report.TruncationReasons = nil
	report.Events = []Event{{
		Version: EventSchemaVersion, Sequence: 1, Time: time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC),
		Level: LevelError, Kind: KindDiagnostic, Message: strings.Repeat("event", 300),
	}}

	encoded, err := fitReport(&report, 1024)
	if err != nil {
		t.Fatalf("fitReport() error = %v", err)
	}
	if len(encoded) != 1024 || len(report.Events) != 0 || !hasOnlyReason(report.TruncationReasons, "oversized_last_event_omitted") {
		t.Fatalf("exact event-free envelope = size %d report %#v", len(encoded), report)
	}
}

func padReportToSize(t *testing.T, report *Report, target int, set func(string)) {
	t.Helper()
	padding := 0
	for range 3 {
		set(strings.Repeat("x", padding))
		encoded, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) == target {
			return
		}
		padding += target - len(encoded)
		if padding < 0 {
			t.Fatalf("report envelope size %d exceeds target %d", len(encoded), target)
		}
	}
	t.Fatalf("report padding did not converge on %d bytes", target)
}

func hasOnlyReason(reasons []string, wanted string) bool {
	return len(reasons) == 1 && reasons[0] == wanted
}

func TestProjectTextFitTreatsMalformedWireAsUnmeasured(t *testing.T) {
	validID, err := NewID("field")
	if err != nil {
		t.Fatal(err)
	}
	valid := map[string]any{"observations": []any{map[string]any{"element": "field", "instance": 0.0, "original_columns": 4.0, "layout_rows": 1.0, "available_columns": 3.0, "available_rows": 1.0, "intent": "truncate", "truncated": true}}}
	for _, test := range []struct {
		name string
		raw  any
		want bool
	}{
		{"valid", valid, true},
		{"missing observations", map[string]any{}, false},
		{"null observations", map[string]any{"observations": nil}, false},
		{"unknown field", map[string]any{"observations": []any{}, "content": "private"}, false},
		{"bad identifier", map[string]any{"observations": []any{map[string]any{"element": "PRIVATE", "intent": "clip"}}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := DebugEvent{}
			projectSemanticDetails(&event, map[string]any{"text_fit": test.raw})
			if (event.TextFit != nil) != test.want {
				t.Fatalf("TextFit = %+v, want measured %t", event.TextFit, test.want)
			}
		})
	}
	report := &TextFitReport{Observations: []TextFitObservation{{Element: validID, Intent: TextFitClip}}}
	clone := CloneTextFit(report)
	clone.Observations[0].AvailableRows = 9
	if report.Observations[0].AvailableRows != 0 {
		t.Fatal("CloneTextFit aliases observations")
	}
}
