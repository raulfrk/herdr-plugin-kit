package diagnostics_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"pgregory.net/rapid"
)

func testConfig(directory string) diagnostics.Config {
	config := diagnostics.DefaultConfig(directory)
	config.MaxEvents = 20
	config.MaxBytes = 32 << 10
	config.MaxAge = time.Hour
	config.MaxDetailBytes = 2 << 10
	config.MaxSnapshotBytes = 2 << 10
	config.MaxReportBytes = 16 << 10
	return config
}

func TestDefaultConfigKeepsUsefulDebuggingHistory(t *testing.T) {
	config := diagnostics.DefaultConfig("private")
	if config.MaxEvents != 100_000 || config.MaxBytes != 128<<20 || config.MaxAge != 14*24*time.Hour {
		t.Fatalf("default history budget = events %d, bytes %d, age %s", config.MaxEvents, config.MaxBytes, config.MaxAge)
	}
	if config.MaxDetailBytes != 1<<20 || config.MaxSnapshotBytes != 2<<20 || config.MaxReportBytes != 32<<20 {
		t.Fatalf("default artifact budget = details %d, snapshot %d, report %d", config.MaxDetailBytes, config.MaxSnapshotBytes, config.MaxReportBytes)
	}
}

func TestConfigValidationDocumentsEveryOperationalBoundary(t *testing.T) {
	valid := testConfig(filepath.Join(t.TempDir(), "private"))
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config error = %v", err)
	}
	tests := []struct {
		name   string
		change func(*diagnostics.Config)
		want   string
	}{
		{"empty directory", func(c *diagnostics.Config) { c.Directory = "" }, "directory"},
		{"zero events", func(c *diagnostics.Config) { c.MaxEvents = 0 }, "max events"},
		{"negative events", func(c *diagnostics.Config) { c.MaxEvents = -1 }, "max events"},
		{"bytes below minimum", func(c *diagnostics.Config) { c.MaxBytes = 1023 }, "max bytes"},
		{"bytes above maximum", func(c *diagnostics.Config) { c.MaxBytes = 1<<30 + 1 }, "max bytes"},
		{"zero age", func(c *diagnostics.Config) { c.MaxAge = 0 }, "max age"},
		{"zero details", func(c *diagnostics.Config) { c.MaxDetailBytes = 0 }, "max detail"},
		{"zero snapshot", func(c *diagnostics.Config) { c.MaxSnapshotBytes = 0 }, "max snapshot"},
		{"report below minimum", func(c *diagnostics.Config) { c.MaxReportBytes = 1023 }, "max report"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.change(&config)
			if err := config.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want context %q", err, test.want)
			}
		})
	}
	for _, maxBytes := range []int64{1024, 1 << 30} {
		config := valid
		config.MaxBytes = maxBytes
		if err := config.Validate(); err != nil {
			t.Fatalf("MaxBytes boundary %d error = %v", maxBytes, err)
		}
	}
	config := valid
	config.MaxReportBytes = 1024
	if err := config.Validate(); err != nil {
		t.Fatalf("MaxReportBytes boundary error = %v", err)
	}
}

func openRecorder(t *testing.T, config diagnostics.Config) *diagnostics.Recorder {
	t.Helper()
	recorder, err := diagnostics.Open(config)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := recorder.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return recorder
}

func TestRecordNormalizesSchemaAndRedactsEveryTextPath(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	config := testConfig(directory)
	config.Now = func() time.Time { return time.Date(2026, 9, 3, 10, 30, 0, 0, time.UTC) }
	recorder := openRecorder(t, config)

	event, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{
		Level:         diagnostics.LevelInfo,
		Kind:          diagnostics.KindInteraction,
		Plugin:        "search TOKEN=plugin-secret",
		Component:     "picker api_key=component-secret",
		Action:        "submit --password action-secret",
		CorrelationID: "request TOKEN=correlation-secret",
		Message:       "Authorization: Bearer message-secret",
		Details: map[string]any{
			"nested":     []any{"api_key=string-secret", map[string]any{"password": "map-secret", "safe": "visible"}},
			"auth.token": "dotted-secret",
		},
		UISnapshot: &diagnostics.UISnapshot{Name: "dialog TOKEN=name-secret", Text: "Cookie: session=snapshot-secret"},
		Screenshot: &diagnostics.ScreenshotRef{
			Name: "screen TOKEN=ref-secret", Path: "/tmp/view.png?token=path-secret",
			MediaType: "image/png; token=media-secret", SHA256: "TOKEN=sha-secret",
		},
	})
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if event.Version != diagnostics.EventSchemaVersion || event.Sequence != 1 || event.Time.IsZero() || event.Time.Location() != time.UTC {
		t.Fatalf("normalized identity = version %d, sequence %d, time %v", event.Version, event.Sequence, event.Time)
	}
	if event.Level != diagnostics.LevelInfo || event.Kind != diagnostics.KindInteraction || !strings.HasPrefix(event.Plugin, "search ") || !strings.HasPrefix(event.Component, "picker ") || !strings.HasPrefix(event.Action, "submit ") {
		t.Fatalf("diagnostic routing fields changed: %#v", event)
	}
	if event.Screenshot == nil || !strings.HasPrefix(event.Screenshot.Path, "/tmp/view.png?") || !strings.HasPrefix(event.Screenshot.MediaType, "image/png;") {
		t.Fatalf("safe screenshot context changed: %#v", event.Screenshot)
	}

	raw, err := os.ReadFile(filepath.Join(directory, diagnostics.EventLogName))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"plugin-secret", "component-secret", "action-secret", "correlation-secret", "message-secret", "string-secret", "map-secret", "dotted-secret", "name-secret", "snapshot-secret", "ref-secret", "path-secret", "media-secret", "sha-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("persisted event contains secret %q", secret)
		}
	}
	if !strings.Contains(string(raw), `"safe":"visible"`) || !strings.Contains(string(raw), `\u003credacted\u003e`) {
		t.Fatalf("persisted redaction lost safe data or marker: %s", raw)
	}
}

func TestUISnapshotBudgetCoversNameAndText(t *testing.T) {
	config := testConfig(filepath.Join(t.TempDir(), "private"))
	config.MaxSnapshotBytes = 32
	recorder := openRecorder(t, config)
	event, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{
		Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "snapshot",
		UISnapshot: &diagnostics.UISnapshot{Name: strings.Repeat("名", 20), Text: strings.Repeat("screen", 20)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if event.UISnapshot == nil || !event.UISnapshot.Truncated {
		t.Fatalf("snapshot truncation = %#v", event.UISnapshot)
	}
	if size := len(event.UISnapshot.Name) + len(event.UISnapshot.Text); size > config.MaxSnapshotBytes {
		t.Fatalf("snapshot string bytes = %d, limit = %d", size, config.MaxSnapshotBytes)
	}
	if !utf8.ValidString(event.UISnapshot.Name) || !utf8.ValidString(event.UISnapshot.Text) {
		t.Fatalf("snapshot truncation split UTF-8: %#v", event.UISnapshot)
	}
	exact, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{
		Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "exact snapshot",
		UISnapshot: &diagnostics.UISnapshot{Name: strings.Repeat("n", config.MaxSnapshotBytes)},
	})
	if err != nil || exact.UISnapshot == nil || exact.UISnapshot.Truncated {
		t.Fatalf("exact snapshot budget = (%#v, %v)", exact.UISnapshot, err)
	}
}

func TestDetailBudgetSmallerThanMarkerStillBoundsPayload(t *testing.T) {
	config := testConfig(filepath.Join(t.TempDir(), "private"))
	config.MaxDetailBytes = 1
	recorder := openRecorder(t, config)
	event, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{
		Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "details",
		Details: map[string]any{"safe": "value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !event.DetailsTruncated || event.Details != nil {
		t.Fatalf("one-byte details budget = %#v", event)
	}
	raw, err := os.ReadFile(filepath.Join(config.Directory, diagnostics.EventLogName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"details":`) || !strings.Contains(string(raw), `"details_truncated":true`) {
		t.Fatalf("persisted tiny-budget event = %s", raw)
	}
}

func TestDetailTruncationMarkerFitsItsExactBudget(t *testing.T) {
	const marker = `{"_truncated":true}`
	config := testConfig(filepath.Join(t.TempDir(), "private"))
	config.MaxDetailBytes = len(marker)
	recorder := openRecorder(t, config)
	event, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{
		Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "details",
		Details: map[string]any{"safe": strings.Repeat("x", 100)},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(event.Details)
	if err != nil || string(encoded) != marker || !event.DetailsTruncated {
		t.Fatalf("exact marker budget = details %s, truncated %v, error %v", encoded, event.DetailsTruncated, err)
	}
}

func TestDetailsAtExactBudgetRemainAvailable(t *testing.T) {
	details := map[string]any{"context": "visible"}
	encoded, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	config := testConfig(filepath.Join(t.TempDir(), "private"))
	config.MaxDetailBytes = len(encoded)
	recorder := openRecorder(t, config)
	event, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{
		Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic,
		Message: "exact details", Details: details,
	})
	if err != nil {
		t.Fatal(err)
	}
	if event.DetailsTruncated || event.Details["context"] != "visible" {
		t.Fatalf("exact-budget details = %#v", event)
	}
}

func TestOpenUsesPrivateModesAndRecoversCorruptRecords(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	valid := `{"version":1,"sequence":7,"time":"2026-09-03T10:00:00Z","level":"info","kind":"lifecycle","message":"started"}` + "\n"
	if err := os.WriteFile(filepath.Join(directory, diagnostics.EventLogName), []byte(valid+`{"version":1`), 0o644); err != nil {
		t.Fatal(err)
	}

	recorder := openRecorder(t, testConfig(directory))
	health := recorder.Health()
	if !health.Writable || health.CorruptRecords != 1 || health.LastError == "" {
		t.Fatalf("recovery health = %#v", health)
	}
	event, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindLifecycle, Message: "ready"})
	if err != nil || event.Sequence != 8 {
		t.Fatalf("post-recovery Record() = (%#v, %v)", event, err)
	}
	if mode := mustMode(t, directory); mode.Perm() != 0o700 {
		t.Fatalf("directory mode = %o", mode.Perm())
	}
	if mode := mustMode(t, filepath.Join(directory, diagnostics.EventLogName)); mode.Perm() != 0o600 {
		t.Fatalf("event log mode = %o", mode.Perm())
	}
	for _, line := range nonemptyLines(t, filepath.Join(directory, diagnostics.EventLogName)) {
		var decoded diagnostics.Event
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("recovered log still contains corrupt record %q: %v", line, err)
		}
	}
}

func TestOpenRejectsEachInvalidStoredEventIndependently(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"version":1,"sequence":1,"time":"2026-09-03T10:00:00Z","level":"info","kind":"lifecycle","message":"first"}`,
		`{"version":2,"sequence":2,"time":"2026-09-03T10:01:00Z","level":"info","kind":"lifecycle","message":"bad-version"}`,
		`{"version":1,"sequence":1,"time":"2026-09-03T10:02:00Z","level":"info","kind":"lifecycle","message":"duplicate"}`,
		`{"version":1,"sequence":3,"time":"0001-01-01T00:00:00Z","level":"info","kind":"lifecycle","message":"zero-time"}`,
		`{"version":1,"sequence":4,"time":"2026-09-03T09:59:00Z","level":"info","kind":"lifecycle","message":"backward-time"}`,
		`{"version":1,"sequence":5,"time":"2026-09-03T10:03:00Z","level":"trace","kind":"lifecycle","message":"bad-level"}`,
		`{"version":1,"sequence":6,"time":"2026-09-03T10:04:00Z","level":"info","kind":"unknown","message":"bad-kind"}`,
		`{"version":1,"sequence":7,"time":"2026-09-03T10:05:00Z","level":"info","kind":"lifecycle","message":""}`,
		`{"version":1,"sequence":8,"time":"2026-09-03T10:06:00Z","level":"info","kind":"lifecycle","message":"last"}`,
	}
	if err := os.WriteFile(filepath.Join(directory, diagnostics.EventLogName), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := testConfig(directory)
	config.Now = func() time.Time { return time.Date(2026, 9, 3, 10, 7, 0, 0, time.UTC) }
	recorder := openRecorder(t, config)
	if health := recorder.Health(); health.CorruptRecords != 7 {
		t.Fatalf("independent corrupt record count = %d, want 7", health.CorruptRecords)
	}
	events := readEvents(t, filepath.Join(directory, diagnostics.EventLogName))
	if len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 8 {
		t.Fatalf("valid records around corruption = %#v", events)
	}
}

func TestOpenRepairsMissingDelimiterBeforeAppending(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	prior := `{"version":1,"sequence":7,"time":"2026-09-03T10:00:00Z","level":"info","kind":"lifecycle","message":"started"}`
	if err := os.WriteFile(filepath.Join(directory, diagnostics.EventLogName), []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}
	config := testConfig(directory)
	config.Now = func() time.Time { return time.Date(2026, 9, 3, 10, 30, 0, 0, time.UTC) }
	recorder := openRecorder(t, config)
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindLifecycle, Message: "next"}); err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, filepath.Join(directory, diagnostics.EventLogName))
	if len(events) != 2 || events[0].Sequence != 7 || events[1].Sequence != 8 {
		t.Fatalf("repaired events = %#v", events)
	}
}

func TestOpenRetainsNewestRecordsWhenExistingLogExceedsNewByteBudget(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	for sequence := 1; sequence <= 30; sequence++ {
		fmt.Fprintf(&log, `{"version":1,"sequence":%d,"time":"2026-09-03T10:%02d:00Z","level":"info","kind":"lifecycle","message":"event-%02d-%s"}`+"\n", sequence, sequence, sequence, strings.Repeat("x", 40))
	}
	path := filepath.Join(directory, diagnostics.EventLogName)
	if err := os.WriteFile(path, []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	config := testConfig(directory)
	config.MaxBytes = 1400
	config.Now = func() time.Time { return time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC) }
	recorder := openRecorder(t, config)
	event, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindLifecycle, Message: "newest"})
	if err != nil {
		t.Fatal(err)
	}
	if event.Sequence != 31 {
		t.Fatalf("next sequence = %d, want 31", event.Sequence)
	}
	events := readEvents(t, path)
	if len(events) == 0 || events[len(events)-1].Sequence != 31 || events[0].Sequence == 1 {
		t.Fatalf("oversized recovery did not preserve newest tail: %#v", events)
	}
	if info, err := os.Stat(path); err != nil || info.Size() > config.MaxBytes {
		t.Fatalf("recovered log size = %v, error = %v", info, err)
	}
}

func TestOpenRetainsCompleteTailStartingAtExactRecordBoundary(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	const maxBytes = 1024
	tail := diagnostics.Event{
		Version: diagnostics.EventSchemaVersion, Sequence: 2,
		Time:  time.Date(2026, 9, 3, 10, 1, 0, 0, time.UTC),
		Level: diagnostics.LevelInfo, Kind: diagnostics.KindLifecycle,
	}
	base, err := json.Marshal(tail)
	if err != nil {
		t.Fatal(err)
	}
	tail.Message = strings.Repeat("x", maxBytes-len(base)-1)
	tailLine, err := json.Marshal(tail)
	if err != nil {
		t.Fatal(err)
	}
	tailLine = append(tailLine, '\n')
	if len(tailLine) != maxBytes {
		t.Fatalf("tail fixture bytes = %d, want %d", len(tailLine), maxBytes)
	}
	older := []byte(`{"version":1,"sequence":1,"time":"2026-09-03T10:00:00Z","level":"info","kind":"lifecycle","message":"older"}` + "\n")
	path := filepath.Join(directory, diagnostics.EventLogName)
	if err := os.WriteFile(path, append(older, tailLine...), 0o600); err != nil {
		t.Fatal(err)
	}
	config := testConfig(directory)
	config.MaxBytes = maxBytes
	config.Now = func() time.Time { return time.Date(2026, 9, 3, 10, 2, 0, 0, time.UTC) }
	recorder := openRecorder(t, config)
	events := readEvents(t, path)
	if len(events) != 1 || events[0].Sequence != 2 || recorder.Health().Bytes != maxBytes {
		t.Fatalf("exact bounded tail = events %#v, health %#v", events, recorder.Health())
	}
	if event, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindLifecycle, Message: "new"}); err != nil || event.Sequence != 3 {
		t.Fatalf("record after exact bounded tail = (%#v, %v)", event, err)
	}
}

func TestOpenDoesNotRewriteAValidLogAtItsExactByteBudget(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	config := testConfig(directory)
	config.MaxBytes = 1024
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	config.Now = func() time.Time { return now }
	recorder, err := diagnostics.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	normalized := diagnostics.Event{Version: diagnostics.EventSchemaVersion, Sequence: 1, Time: now, Level: diagnostics.LevelInfo, Kind: diagnostics.KindLifecycle}
	base, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindLifecycle, Message: strings.Repeat("x", int(config.MaxBytes)-len(base)-1)}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, diagnostics.EventLogName)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := diagnostics.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	health := reopened.Health()
	if !os.SameFile(before, after) || health.Dropped != 0 || health.CorruptRecords != 0 || health.LastError != "" || health.Bytes != config.MaxBytes {
		t.Fatalf("exact-budget reopen rewrote or degraded log: same=%v health=%#v", os.SameFile(before, after), health)
	}
}

func TestOpenRetainsTailWhenByteWindowStartsOnRecordSeparator(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	const tailBytes = 1024
	tail := diagnostics.Event{
		Version: diagnostics.EventSchemaVersion, Sequence: 2,
		Time:  time.Date(2026, 9, 3, 10, 1, 0, 0, time.UTC),
		Level: diagnostics.LevelInfo, Kind: diagnostics.KindLifecycle,
	}
	base, err := json.Marshal(tail)
	if err != nil {
		t.Fatal(err)
	}
	tail.Message = strings.Repeat("x", tailBytes-len(base)-1)
	tailLine, err := json.Marshal(tail)
	if err != nil {
		t.Fatal(err)
	}
	tailLine = append(tailLine, '\n')
	older := []byte(`{"version":1,"sequence":1,"time":"2026-09-03T10:00:00Z","level":"info","kind":"lifecycle","message":"older"}` + "\n")
	path := filepath.Join(directory, diagnostics.EventLogName)
	if err := os.WriteFile(path, append(older, tailLine...), 0o600); err != nil {
		t.Fatal(err)
	}
	config := testConfig(directory)
	config.MaxBytes = tailBytes + 1
	config.Now = func() time.Time { return time.Date(2026, 9, 3, 10, 2, 0, 0, time.UTC) }
	recorder := openRecorder(t, config)
	events := readEvents(t, path)
	health := recorder.Health()
	if len(events) != 1 || events[0].Sequence != 2 || health.Bytes != tailBytes || health.CorruptRecords != 0 {
		t.Fatalf("separator-aligned tail = events %#v, health %#v", events, health)
	}
}

func TestOpenReportsOversizedUndelimitedTailAsCorrupt(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, diagnostics.EventLogName)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 2048)), 0o600); err != nil {
		t.Fatal(err)
	}
	config := testConfig(directory)
	config.MaxBytes = 1024
	recorder := openRecorder(t, config)
	health := recorder.Health()
	if health.CorruptRecords != 1 || health.Dropped != 1 || health.LastError == "" || health.Events != 0 || health.Bytes != 0 {
		t.Fatalf("oversized undelimited recovery health = %#v", health)
	}
	if data, err := os.ReadFile(path); err != nil || len(data) != 0 {
		t.Fatalf("oversized undelimited recovery log = %q, error = %v", data, err)
	}
}

func TestEventAtExactByteBudgetFitsAndOversizedEventIsDropped(t *testing.T) {
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	config := testConfig(filepath.Join(t.TempDir(), "private"))
	config.MaxBytes = 1024
	config.Now = func() time.Time { return now }
	recorder := openRecorder(t, config)
	normalized := diagnostics.Event{
		Version: diagnostics.EventSchemaVersion, Sequence: 1, Time: now,
		Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic,
	}
	base, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	messageBytes := int(config.MaxBytes) - len(base) - 1
	if messageBytes <= 0 {
		t.Fatalf("event envelope unexpectedly consumes byte budget: %d", len(base))
	}
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: strings.Repeat("x", messageBytes)}); err != nil {
		t.Fatalf("exact-budget event error = %v", err)
	}
	if health := recorder.Health(); health.Bytes != config.MaxBytes || health.Dropped != 0 {
		t.Fatalf("exact-budget health = %#v", health)
	}
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: strings.Repeat("x", int(config.MaxBytes))}); !errors.Is(err, diagnostics.ErrEventTooLarge) {
		t.Fatalf("oversized event error = %v", err)
	}
	if health := recorder.Health(); health.Dropped != 1 || !health.Pressure || health.Bytes != config.MaxBytes {
		t.Fatalf("oversized rejection health = %#v", health)
	}
}

func TestRetentionByCountBytesAndAge(t *testing.T) {
	base := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	now := base
	config := testConfig(filepath.Join(t.TempDir(), "private"))
	config.MaxEvents = 3
	config.MaxBytes = 1400
	config.MaxAge = 2 * time.Minute
	config.Now = func() time.Time { return now }
	recorder := openRecorder(t, config)

	for index := range 8 {
		now = base.Add(time.Duration(index) * time.Minute)
		_, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{
			Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic,
			Message: fmt.Sprintf("event-%d-%s", index, strings.Repeat("x", 180)),
		})
		if err != nil {
			t.Fatalf("Record(%d) error = %v", index, err)
		}
	}
	if err := recorder.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	health := recorder.Health()
	if health.Events != 3 || health.Bytes > config.MaxBytes || health.Dropped != 5 || !health.Pressure {
		t.Fatalf("retention health = %#v", health)
	}
	events := readEvents(t, filepath.Join(config.Directory, diagnostics.EventLogName))
	for _, event := range events {
		if event.Time.Before(now.Add(-config.MaxAge)) {
			t.Fatalf("expired event retained: %v", event.Time)
		}
	}
	if len(events) == 0 || !events[0].Time.Equal(now.Add(-config.MaxAge)) {
		t.Fatalf("event exactly at age cutoff was not retained: %#v", events)
	}
}

func TestHealthReportsExactUsageAndIndependentPressureSignals(t *testing.T) {
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		maxEvents int
		bytes     int
		pressure  bool
	}{
		{"below byte pressure", 20, 819, false},
		{"at byte pressure", 20, 820, true},
		{"at event pressure", 1, 200, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(filepath.Join(t.TempDir(), "private"))
			config.MaxEvents = test.maxEvents
			config.MaxBytes = 1025
			config.Now = func() time.Time { return now }
			recorder := openRecorder(t, config)
			envelope := diagnostics.Event{Version: diagnostics.EventSchemaVersion, Sequence: 1, Time: now, Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic}
			base, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			messageBytes := test.bytes - len(base) - 1
			if messageBytes < 1 {
				t.Fatalf("fixture byte target %d is too small", test.bytes)
			}
			if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: strings.Repeat("x", messageBytes)}); err != nil {
				t.Fatal(err)
			}
			health := recorder.Health()
			if health.Events != 1 || health.Bytes != int64(test.bytes) || health.UsageRatio != float64(test.bytes)/1025 || health.Pressure != test.pressure || health.Dropped != 0 {
				t.Fatalf("health = %#v", health)
			}
		})
	}
}

func TestConcurrentRecordingHasUniqueSequenceAndSettledQuota(t *testing.T) {
	config := testConfig(filepath.Join(t.TempDir(), "private"))
	config.MaxEvents = 75
	config.MaxBytes = 18 << 10
	recorder := openRecorder(t, config)

	const goroutines = 12
	const perGoroutine = 80
	var wait sync.WaitGroup
	errorsSeen := make(chan error, goroutines*perGoroutine)
	for worker := range goroutines {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range perGoroutine {
				_, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{
					Level: diagnostics.LevelDebug, Kind: diagnostics.KindInteraction,
					Plugin: fmt.Sprintf("plugin-%d", worker), Message: fmt.Sprintf("event-%d-%s", index, strings.Repeat("x", index%40)),
				})
				if err != nil {
					errorsSeen <- err
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("concurrent Record() error = %v", err)
	}
	if err := recorder.Cleanup(); err != nil {
		t.Fatal(err)
	}
	health := recorder.Health()
	if health.Bytes > config.MaxBytes || health.Events > config.MaxEvents {
		t.Fatalf("settled quota exceeded: %#v", health)
	}
	if info, err := os.Stat(filepath.Join(config.Directory, diagnostics.EventLogName)); err != nil || info.Size() > config.MaxBytes {
		t.Fatalf("settled event log exceeds quota: info=%v err=%v", info, err)
	}
	seen := make(map[uint64]bool)
	for _, event := range readEvents(t, filepath.Join(config.Directory, diagnostics.EventLogName)) {
		if seen[event.Sequence] {
			t.Fatalf("duplicate sequence %d", event.Sequence)
		}
		seen[event.Sequence] = true
	}
}

func TestPropertyRetentionNeverExceedsCountOrByteBudget(t *testing.T) {
	root := t.TempDir()
	caseNumber := 0
	rapid.Check(t, func(rt *rapid.T) {
		caseNumber++
		config := testConfig(filepath.Join(root, fmt.Sprintf("case-%d", caseNumber)))
		config.MaxEvents = rapid.IntRange(1, 20).Draw(rt, "max_events")
		config.MaxBytes = int64(rapid.IntRange(1024, 8192).Draw(rt, "max_bytes"))
		recorder, err := diagnostics.Open(config)
		if err != nil {
			rt.Fatalf("Open() error = %v", err)
		}
		defer recorder.Close()
		operations := rapid.IntRange(1, 60).Draw(rt, "operations")
		for index := range operations {
			length := rapid.IntRange(1, 2000).Draw(rt, fmt.Sprintf("length-%d", index))
			_, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: strings.Repeat("m", length)})
			if err != nil && !errors.Is(err, diagnostics.ErrEventTooLarge) {
				rt.Fatalf("Record() unexpected error = %v", err)
			}
			health := recorder.Health()
			if health.Events > config.MaxEvents || health.Bytes > config.MaxBytes {
				rt.Fatalf("quota exceeded after operation %d: %#v", index, health)
			}
		}
	})
}

func TestReportIsDeterministicRedactedBoundedAndExplicitlyTruncated(t *testing.T) {
	base := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	config := testConfig(filepath.Join(t.TempDir(), "private"))
	config.Now = func() time.Time { return base }
	recorder := openRecorder(t, config)
	for index := range 12 {
		_, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{
			Level: diagnostics.LevelInfo, Kind: diagnostics.KindLifecycle,
			Message:    fmt.Sprintf("event-%02d %s", index, strings.Repeat("x", 180)),
			UISnapshot: &diagnostics.UISnapshot{Name: "main", Text: "TOKEN=snapshot-secret\n" + strings.Repeat("screen", 100)},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	options := diagnostics.ReportOptions{
		MaxBytes:         2200,
		ConfigMetadata:   map[string]any{"profile": "test", "api_key": "config-secret"},
		ManifestMetadata: map[string]any{"id": "example", "nested": map[string]any{"password": "manifest-secret"}},
		EmbedSnapshots:   true,
	}
	first, err := recorder.Export(options)
	if err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	base = base.Add(time.Hour)
	second, err := recorder.Export(options)
	if err != nil {
		t.Fatalf("second Export() error = %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("unchanged recorder exported nondeterministic reports")
	}
	if len(first) > options.MaxBytes {
		t.Fatalf("report size = %d, limit = %d", len(first), options.MaxBytes)
	}
	if strings.Contains(string(first), "config-secret") || strings.Contains(string(first), "manifest-secret") || strings.Contains(string(first), "snapshot-secret") {
		t.Fatalf("report contains a secret: %s", first)
	}
	var report diagnostics.Report
	if err := json.Unmarshal(first, &report); err != nil {
		t.Fatal(err)
	}
	if report.Version != diagnostics.ReportSchemaVersion || !report.Truncated || len(report.TruncationReasons) == 0 || len(report.Events) == 0 {
		t.Fatalf("report did not describe truncation: %#v", report)
	}
	if report.Events[len(report.Events)-1].Sequence != 12 {
		t.Fatal("report did not retain the most recent event")
	}
}

func TestReportCanOmitTheFinalOversizedEvent(t *testing.T) {
	config := testConfig(filepath.Join(t.TempDir(), "private"))
	recorder := openRecorder(t, config)
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: strings.Repeat("x", 4000)}); err != nil {
		t.Fatal(err)
	}
	encoded, err := recorder.Export(diagnostics.ReportOptions{MaxBytes: 1024})
	if err != nil {
		t.Fatalf("bounded Export() error = %v", err)
	}
	if len(encoded) > 1024 {
		t.Fatalf("bounded report size = %d", len(encoded))
	}
	var report diagnostics.Report
	if err := json.Unmarshal(encoded, &report); err != nil {
		t.Fatal(err)
	}
	if !report.Truncated || len(report.Events) != 0 || !contains(report.TruncationReasons, "oversized_last_event_omitted") || contains(report.TruncationReasons, "oldest_events_omitted") {
		t.Fatalf("event-free truncation not explicit: %#v", report)
	}
}

func TestReportLimitsAndEveryTruncationStage(t *testing.T) {
	config := testConfig(filepath.Join(t.TempDir(), "private"))
	config.MaxReportBytes = 16 << 10
	recorder := openRecorder(t, config)
	for sequence := range 3 {
		if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{
			Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic,
			Message:    fmt.Sprintf("event-%d-%s", sequence, strings.Repeat("m", 700)),
			UISnapshot: &diagnostics.UISnapshot{Name: "main", Text: strings.Repeat("screen", 200)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	options := diagnostics.ReportOptions{
		MaxBytes: 1024, EmbedSnapshots: true,
		ConfigMetadata:   map[string]any{"config": strings.Repeat("c", 700)},
		ManifestMetadata: map[string]any{"manifest": strings.Repeat("m", 700)},
	}
	exactStageSizes := map[string]int{}
	for limit := 1024; limit <= config.MaxReportBytes; limit += 64 {
		options.MaxBytes = limit
		encoded, err := recorder.Export(options)
		if err != nil {
			continue
		}
		if len(encoded) > limit {
			t.Fatalf("report size %d exceeds limit %d", len(encoded), limit)
		}
		var report diagnostics.Report
		if err := json.Unmarshal(encoded, &report); err != nil {
			t.Fatal(err)
		}
		if len(report.TruncationReasons) > 0 {
			stage := report.TruncationReasons[len(report.TruncationReasons)-1]
			if exactStageSizes[stage] == 0 {
				exactStageSizes[stage] = len(encoded)
			}
		}
	}
	for _, reason := range []string{"oldest_events_omitted", "embedded_snapshots_omitted", "metadata_omitted", "oversized_last_event_omitted"} {
		stageSize, ok := exactStageSizes[reason]
		if !ok {
			t.Fatalf("no report completed at truncation stage %q", reason)
		}
		if stageSize < 1024 {
			continue
		}
		exactOptions := options
		exactOptions.MaxBytes = stageSize
		encoded, err := recorder.Export(exactOptions)
		if err != nil {
			t.Fatalf("exact %s stage error = %v", reason, err)
		}
		var exactReport diagnostics.Report
		if err := json.Unmarshal(encoded, &exactReport); err != nil {
			t.Fatal(err)
		}
		if len(encoded) != stageSize || !contains(exactReport.TruncationReasons, reason) {
			t.Fatalf("exact %s stage = size %d reasons %v", reason, len(encoded), exactReport.TruncationReasons)
		}
	}
	full, err := recorder.Export(diagnostics.ReportOptions{EmbedSnapshots: true, ConfigMetadata: options.ConfigMetadata, ManifestMetadata: options.ManifestMetadata})
	if err != nil {
		t.Fatal(err)
	}
	exactOptions := options
	exactOptions.MaxBytes = len(full)
	exact, err := recorder.Export(exactOptions)
	if err != nil || string(exact) != string(full) {
		t.Fatalf("exact-fit report changed: size=%d error=%v", len(exact), err)
	}
	for _, limit := range []int{1023, config.MaxReportBytes + 1} {
		if _, err := recorder.Export(diagnostics.ReportOptions{MaxBytes: limit}); err == nil {
			t.Errorf("invalid report limit %d accepted", limit)
		}
	}
}

func TestOpenRejectsSymlinkedPrivateStorageWithoutTouchingTarget(t *testing.T) {
	root := t.TempDir()
	targetDirectory := filepath.Join(root, "target-directory")
	if err := os.Mkdir(targetDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedDirectory := filepath.Join(root, "linked-directory")
	if err := os.Symlink(targetDirectory, linkedDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := diagnostics.Open(testConfig(linkedDirectory)); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlinked directory error = %v", err)
	}
	if mode := mustMode(t, targetDirectory).Perm(); mode != 0o755 {
		t.Fatalf("symlink target directory mode changed to %o", mode)
	}
	regularPath := filepath.Join(root, "regular-directory-target")
	if err := os.WriteFile(regularPath, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := diagnostics.Open(testConfig(regularPath)); err == nil {
		t.Fatalf("regular-file directory error = %v", err)
	}

	directory := filepath.Join(root, "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	targetFile := filepath.Join(root, "target-file")
	if err := os.WriteFile(targetFile, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetFile, filepath.Join(directory, diagnostics.EventLogName)); err != nil {
		t.Fatal(err)
	}
	if _, err := diagnostics.Open(testConfig(directory)); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlinked event log error = %v", err)
	}
	data, err := os.ReadFile(targetFile)
	if err != nil || string(data) != "keep" || mustMode(t, targetFile).Perm() != 0o644 {
		t.Fatalf("symlink target changed: data=%q mode=%o error=%v", data, mustMode(t, targetFile).Perm(), err)
	}
	specialDirectory := filepath.Join(root, "special-log")
	if err := os.Mkdir(specialDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(specialDirectory, diagnostics.EventLogName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := diagnostics.Open(testConfig(specialDirectory)); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory event-log error = %v", err)
	}
}

func TestValidationAndIdempotentCloseCleanup(t *testing.T) {
	config := diagnostics.DefaultConfig("")
	if err := config.Validate(); err == nil {
		t.Fatal("empty directory accepted")
	}
	config = testConfig(filepath.Join(t.TempDir(), "private"))
	config.MaxEvents = 0
	if err := config.Validate(); err == nil {
		t.Fatal("zero event retention accepted")
	}

	recorder := openRecorder(t, testConfig(filepath.Join(t.TempDir(), "private")))
	if err := recorder.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "late"}); !errors.Is(err, diagnostics.ErrClosed) {
		t.Fatalf("Record() after close error = %v", err)
	}
	if err := recorder.Cleanup(); !errors.Is(err, diagnostics.ErrClosed) {
		t.Fatalf("Cleanup() after close error = %v", err)
	}
}

func TestCyclicDetailsAndMetadataFailBeforeRecursiveRedaction(t *testing.T) {
	recorder := openRecorder(t, testConfig(filepath.Join(t.TempDir(), "private")))
	cyclicMap := map[string]any{}
	cyclicMap["nested"] = cyclicMap
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "cycle", Details: cyclicMap}); err == nil || !strings.Contains(err.Error(), "details") || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cyclic map error = %v", err)
	}
	cyclicSlice := make([]any, 1)
	cyclicSlice[0] = cyclicSlice
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "cycle", Details: map[string]any{"items": cyclicSlice}}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cyclic slice error = %v", err)
	}
	deep := any("leaf")
	for range 18 {
		deep = map[string]any{"nested": deep}
	}
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "deep", Details: deep.(map[string]any)}); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("excessive depth error = %v", err)
	}
	if health := recorder.Health(); health.Events != 0 || health.Bytes != 0 {
		t.Fatalf("rejected values changed storage: %#v", health)
	}
	if _, err := recorder.Export(diagnostics.ReportOptions{ConfigMetadata: cyclicMap}); err == nil || !strings.Contains(err.Error(), "config metadata") || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cyclic report metadata error = %v", err)
	}
	event, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "still usable"})
	if err != nil || event.Sequence != 1 {
		t.Fatalf("Record() after rejected values = (%#v, %v)", event, err)
	}
}

func TestJSONDetailComplexityBoundariesAndNumbers(t *testing.T) {
	recorder := openRecorder(t, testConfig(filepath.Join(t.TempDir(), "private")))
	for _, value := range []any{math.NaN(), math.Inf(1), math.Inf(-1), float32(math.Inf(1)), float32(math.NaN())} {
		if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "number", Details: map[string]any{"value": value}}); err == nil || !strings.Contains(err.Error(), "non-finite") {
			t.Errorf("non-finite value %v error = %v", value, err)
		}
	}
	nested := func(depth int) map[string]any {
		var value any = "leaf"
		for range depth {
			value = map[string]any{"nested": value}
		}
		return value.(map[string]any)
	}
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "depth-16", Details: nested(16)}); err != nil {
		t.Fatalf("depth 16 rejected: %v", err)
	}
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "depth-17", Details: nested(17)}); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("depth 17 error = %v", err)
	}
	nestedSlices := func(depth int) map[string]any {
		var value any = "leaf"
		for range depth {
			value = []any{value}
		}
		return map[string]any{"items": value}
	}
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "slice-depth-16", Details: nestedSlices(15)}); err != nil {
		t.Fatalf("slice depth 16 rejected: %v", err)
	}
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "slice-depth-17", Details: nestedSlices(16)}); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("slice depth 17 error = %v", err)
	}
	items := make([]any, 4094)
	for index := range items {
		items[index] = index
	}
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "nodes-4096", Details: map[string]any{"items": items}}); err != nil {
		t.Fatalf("4096 nodes rejected: %v", err)
	}
	items = append(items, 4094)
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "nodes-4097", Details: map[string]any{"items": items}}); err == nil || !strings.Contains(err.Error(), "node count") {
		t.Fatalf("4097 nodes error = %v", err)
	}
	longKey := strings.Repeat("k", 80)
	cycle := map[string]any{}
	cycle[longKey] = cycle
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "bounded path", Details: cycle}); err == nil || !strings.Contains(err.Error(), strings.Repeat("k", 64)+"...") || strings.Contains(err.Error(), longKey) {
		t.Fatalf("bounded validation path error = %v", err)
	}
	exactKey := strings.Repeat("e", 64)
	exactCycle := map[string]any{}
	exactCycle[exactKey] = exactCycle
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "exact path", Details: exactCycle}); err == nil || !strings.Contains(err.Error(), exactKey) || strings.Contains(err.Error(), exactKey+"...") {
		t.Fatalf("exact validation path error = %v", err)
	}
}

func TestCleanupStorageFailureRollsBackAndReportsOneDrop(t *testing.T) {
	base := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	now := base
	directory := filepath.Join(t.TempDir(), "private")
	config := testConfig(directory)
	config.MaxAge = time.Minute
	config.Now = func() time.Time { return now }
	recorder := openRecorder(t, config)
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindLifecycle, Message: "retained after rollback"}); err != nil {
		t.Fatal(err)
	}
	moved := directory + "-moved"
	if err := os.Rename(directory, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(directory, []byte("blocks directory recreation"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(directory)
		_ = os.Rename(moved, directory)
	})
	now = base.Add(2 * time.Minute)
	if err := recorder.Cleanup(); err == nil || !strings.Contains(err.Error(), "clean diagnostics event log") {
		t.Fatalf("Cleanup() error = %v", err)
	}
	health := recorder.Health()
	if health.Writable || health.Dropped != 1 || health.Events != 1 || health.LastError == "" {
		t.Fatalf("cleanup rollback health = %#v", health)
	}
}

func TestStorageFailureIsReportedWithoutRecursivePersistence(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	config := testConfig(directory)
	config.MaxEvents = 1
	recorder := openRecorder(t, config)
	if _, err := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	_, recordErr := diagnostics.RecordWireForTest(recorder, diagnostics.Event{Level: diagnostics.LevelInfo, Kind: diagnostics.KindDiagnostic, Message: "forces compaction"})
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if recordErr == nil {
		t.Skip("filesystem permissions do not prevent owner writes in this environment")
	}
	health := recorder.Health()
	if health.Writable || health.LastError == "" || health.Dropped == 0 {
		t.Fatalf("storage failure health = %#v", health)
	}
}

func mustMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

func nonemptyLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.FieldsFunc(string(data), func(r rune) bool { return r == '\n' })
}

func readEvents(t *testing.T, path string) []diagnostics.Event {
	t.Helper()
	lines := nonemptyLines(t, path)
	events := make([]diagnostics.Event, 0, len(lines))
	for _, line := range lines {
		var event diagnostics.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		events = append(events, event)
	}
	return events
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
