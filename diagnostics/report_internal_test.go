package diagnostics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSyncDirectoryReportsMissingPath(t *testing.T) {
	err := syncDirectory(filepath.Join(t.TempDir(), "missing"))
	if err == nil || !os.IsNotExist(err) {
		t.Fatalf("syncDirectory() error = %v", err)
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
