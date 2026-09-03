package diagnostics

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func previewState(t testing.TB, name string) VisualState {
	t.Helper()
	id := func(value string) ID {
		result, err := NewID(value)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	return VisualState{
		Screen: id(name), Focus: id("results"), State: id("ready"),
		Geometry:  Geometry{ReportedColumns: 70, ReportedRows: 37, RenderColumns: 70, RenderRows: 37},
		ItemCount: 8, SelectedIndex: 3,
	}
}

func TestPreviewStorePersistsAndRevalidatesRegisteredSemanticPNG(t *testing.T) {
	stateDirectory := t.TempDir()
	store, err := OpenPreviewStore(stateDirectory, DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	state := previewState(t, "search")
	entry, err := store.Put(41, state)
	if err != nil {
		t.Fatal(err)
	}
	data, err := store.Read(entry, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("stored preview is not PNG: %v", err)
	}
	path := filepath.Join(stateDirectory, filepath.FromSlash(entry.RelativePath))
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("preview mode = %v", info.Mode())
	}
	reopened, err := OpenPreviewStore(stateDirectory, DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, ok := reopened.Entry(41, state); !ok {
		t.Fatal("persisted registered preview was not revalidated")
	}

	tampered := append([]byte(nil), data...)
	tampered[len(tampered)-1] ^= 0xff
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Entry(41, state); ok {
		t.Fatal("tampered preview remained visible")
	}
	if _, err := reopened.Read(entry, state); err == nil {
		t.Fatal("tampered preview was readable")
	}
}

func TestPreviewStoreRejectsUnregisteredWrongStateSymlinkAndLimits(t *testing.T) {
	stateDirectory := t.TempDir()
	store, err := OpenPreviewStore(stateDirectory, PreviewLimits{MaxCount: 1, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	state := previewState(t, "search")
	entry, err := store.Put(1, state)
	if err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string]PreviewEntry{
		"unregistered": {Sequence: 2, VisualDigest: entry.VisualDigest, RelativePath: entry.RelativePath, PNGSHA256: entry.PNGSHA256, Bytes: entry.Bytes},
		"escape":       {Sequence: 1, VisualDigest: entry.VisualDigest, RelativePath: "../outside.png", PNGSHA256: entry.PNGSHA256, Bytes: entry.Bytes},
	} {
		if _, err := store.Read(candidate, state); err == nil {
			t.Fatalf("%s entry was accepted", name)
		}
	}
	if _, err := store.Read(entry, previewState(t, "other")); err == nil {
		t.Fatal("preview accepted the wrong visual state")
	}
	if _, err := store.Put(2, state); err == nil {
		t.Fatal("preview count limit was ignored")
	}

	path := filepath.Join(stateDirectory, filepath.FromSlash(entry.RelativePath))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(stateDirectory, "outside.png"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(entry, state); err == nil {
		t.Fatal("preview symlink was accepted")
	}
}

func TestPreviewStoreRejectsReplacedRootAndNoncanonicalPersistedPath(t *testing.T) {
	stateDirectory := t.TempDir()
	store, err := OpenPreviewStore(stateDirectory, DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	state := previewState(t, "search")
	entry, err := store.Put(7, state)
	if err != nil {
		t.Fatal(err)
	}
	snapshotRoot := filepath.Join(stateDirectory, "snapshots")
	realRoot := filepath.Join(stateDirectory, "snapshots-real")
	if err := os.Rename(snapshotRoot, realRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRoot, snapshotRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(8, state); err == nil {
		t.Fatal("preview write followed replaced snapshot root")
	}
	if _, err := os.Lstat(filepath.Join(realRoot, filepath.Base(canonicalPreviewPath(8, entry.VisualDigest)))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replaced root received preview file: %v", err)
	}
	if _, ok := store.Entry(7, state); ok {
		t.Fatal("preview remained visible after snapshot root became a symlink")
	}
	if err := os.Remove(snapshotRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(realRoot, snapshotRoot); err != nil {
		t.Fatal(err)
	}

	canonical := filepath.Join(stateDirectory, filepath.FromSlash(entry.RelativePath))
	noncanonical := filepath.Join(snapshotRoot, "private-project-name.png")
	if err := os.Rename(canonical, noncanonical); err != nil {
		t.Fatal(err)
	}
	entry.RelativePath = "snapshots/private-project-name.png"
	registry, err := json.Marshal([]PreviewEntry{entry})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshotRoot, previewRegistryName), registry, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPreviewStore(stateDirectory, DefaultPreviewLimits()); err == nil {
		t.Fatal("noncanonical persisted preview path was accepted")
	}
}

func TestPreviewStoreRejectsDifferentDirectoryAtCanonicalPath(t *testing.T) {
	stateDirectory := t.TempDir()
	store, err := OpenPreviewStore(stateDirectory, DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	state := previewState(t, "search")
	entry, err := store.Put(11, state)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(stateDirectory, "snapshots")
	original := filepath.Join(stateDirectory, "snapshots-original")
	if err := os.Rename(root, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	canaryPath := filepath.Join(root, filepath.Base(filepath.FromSlash(entry.RelativePath)))
	if err := os.WriteFile(canaryPath, []byte("untrusted image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Entry(11, state); ok {
		t.Fatal("preview remained visible through replacement directory")
	}
	if _, err := store.Read(entry, state); err == nil {
		t.Fatal("preview read accepted replacement directory")
	}
	if _, err := store.Put(12, state); err == nil {
		t.Fatal("preview write accepted replacement directory")
	}
	recorder := debugRecorder(t)
	for index := range 10 {
		code, _ := NewID(fmt.Sprintf("padding-%d", index))
		if err := recorder.RecordSemantic(SemanticEvent{Level: LevelInfo, Kind: KindDiagnostic, Plugin: debugID(t, "plugin"), Code: code, Outcome: OutcomeApplied}); err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.RecordSemantic(SemanticEvent{Level: LevelInfo, Kind: KindDiagnostic, Plugin: debugID(t, "plugin"), Code: debugID(t, "event"), Outcome: OutcomeApplied, Visual: &state}); err != nil {
		t.Fatal(err)
	}
	data, err := recorder.ExportDebug(DebugExportOptions{MaxBytes: 4096, Previews: store})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), entry.RelativePath) {
		t.Fatalf("replacement preview entered export: %s", data)
	}
}

func TestDebugExportContainsOnlyAllowlistedProjectionAndRegisteredPreviews(t *testing.T) {
	recorder := debugRecorder(t)
	canary := "PRIVATE_/home/person/query-value"
	if _, err := recorder.record(Event{
		Level: LevelError, Kind: KindDiagnostic, Plugin: "safe", Message: "valid-looking-id",
		Details:    map[string]any{"semantic_schema": 0, "path": canary},
		UISnapshot: &UISnapshot{Name: "semantic-ui", Text: `{"screen":"forged"}`},
		Screenshot: &ScreenshotRef{Name: canary, Path: canary, SHA256: strings.Repeat("a", 64)},
	}); err != nil {
		t.Fatal(err)
	}
	visual := previewState(t, "timeline")
	for _, code := range []string{"first", "second"} {
		if err := recorder.RecordSemantic(SemanticEvent{
			Level: LevelInfo, Kind: KindInteraction, Plugin: debugID(t, "plugin"),
			Code: debugID(t, code), Outcome: OutcomeApplied, Visual: &visual,
		}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := recorder.Debug(DebugQuery{})
	if err != nil || len(page.Events) != 2 {
		t.Fatalf("debug page = %+v, %v", page, err)
	}
	store, err := OpenPreviewStore(t.TempDir(), DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.Put(page.Events[0].Sequence, *page.Events[0].Visual); err != nil {
		t.Fatal(err)
	}
	data, err := recorder.ExportDebug(DebugExportOptions{Query: DebugQuery{}, MaxBytes: 8192, Previews: store})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) || strings.Contains(string(data), canary) || strings.Contains(string(data), "forged") {
		t.Fatalf("unsafe debug report: %s", data)
	}
	var report DebugReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Events) != 2 || len(report.Previews) != 1 || report.PreviewsOmitted != 1 {
		t.Fatalf("debug report = %+v", report)
	}
}

func TestDebugExportTruncatesEventsAndMatchingPreviewsTogether(t *testing.T) {
	recorder := debugRecorder(t)
	visual := previewState(t, "timeline")
	for index := range 30 {
		code, _ := NewID(fmt.Sprintf("event-%d", index))
		if err := recorder.RecordSemantic(SemanticEvent{Level: LevelInfo, Kind: KindInteraction, Plugin: debugID(t, "plugin"), Code: code, Outcome: OutcomeApplied, Visual: &visual}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := recorder.Debug(DebugQuery{PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenPreviewStore(t.TempDir(), DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, event := range page.Events {
		if _, err := store.Put(event.Sequence, *event.Visual); err != nil {
			t.Fatal(err)
		}
	}
	data, err := recorder.ExportDebug(DebugExportOptions{Query: DebugQuery{PageSize: 50}, MaxBytes: 2048, Previews: store})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 2048 {
		t.Fatalf("debug report length = %d", len(data))
	}
	var report DebugReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if !report.Truncated || len(report.TruncationReasons) == 0 || len(report.Events) >= len(page.Events) {
		t.Fatalf("report was not observably truncated: %+v", report)
	}
	retained := make(map[uint64]struct{}, len(report.Events))
	for _, event := range report.Events {
		retained[event.Sequence] = struct{}{}
	}
	for _, preview := range report.Previews {
		if _, ok := retained[preview.Sequence]; !ok {
			t.Fatalf("preview for omitted event %d remained", preview.Sequence)
		}
	}
}

func TestPreviewStoreAndDebugExportAreConcurrentSafe(t *testing.T) {
	recorder := debugRecorder(t)
	visual := previewState(t, "timeline")
	for index := range 40 {
		code, _ := NewID(fmt.Sprintf("event-%d", index))
		if err := recorder.RecordSemantic(SemanticEvent{Level: LevelInfo, Kind: KindInteraction, Plugin: debugID(t, "plugin"), Code: code, Outcome: OutcomeApplied, Visual: &visual}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := recorder.Debug(DebugQuery{PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenPreviewStore(t.TempDir(), DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	errorsSeen := make(chan error, len(page.Events)+80)
	var wait sync.WaitGroup
	wait.Add(4)
	go func() {
		defer wait.Done()
		for _, event := range page.Events {
			if _, err := store.Put(event.Sequence, *event.Visual); err != nil {
				errorsSeen <- err
			}
		}
	}()
	go func() {
		defer wait.Done()
		for index := range 40 {
			code, _ := NewID(fmt.Sprintf("concurrent-%d", index))
			if err := recorder.RecordSemantic(SemanticEvent{Level: LevelInfo, Kind: KindInteraction, Plugin: debugID(t, "plugin"), Code: code, Outcome: OutcomeApplied, Visual: &visual}); err != nil {
				errorsSeen <- err
			}
		}
	}()
	go func() {
		defer wait.Done()
		for range 40 {
			if _, err := recorder.ExportDebug(DebugExportOptions{Query: DebugQuery{PageSize: 50}, MaxBytes: 8192, Previews: store}); err != nil {
				errorsSeen <- err
			}
		}
	}()
	go func() {
		defer wait.Done()
		for range 40 {
			for _, event := range page.Events {
				store.Entry(event.Sequence, *event.Visual)
			}
		}
	}()
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Error(err)
	}
}
