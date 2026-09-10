package diagnostics

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
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
	if err := store.Close(); err != nil {
		t.Fatal(err)
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

func TestPreviewStoreExclusivelyOwnsItsRootUntilClosed(t *testing.T) {
	stateDirectory := t.TempDir()
	first, err := OpenPreviewStore(stateDirectory, DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	legacyLock := filepath.Join(stateDirectory, "snapshots", ".store.lock")
	if err := os.WriteFile(legacyLock, []byte("replaceable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(legacyLock); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyLock, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPreviewStore(stateDirectory, DefaultPreviewLimits()); err == nil ||
		!strings.Contains(err.Error(), "already open") {
		t.Fatalf("second OpenPreviewStore() error = %v", err)
	}
	state := previewState(t, "exclusive")
	if _, err := first.Put(1, state); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenPreviewStore(stateDirectory, DefaultPreviewLimits())
	if err != nil {
		t.Fatalf("OpenPreviewStore() after close error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, ok := reopened.Entry(1, state); !ok {
		t.Fatal("exclusive owner did not persist its registered preview")
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

func TestPreviewStoreLimitAndIdempotencyBoundaries(t *testing.T) {
	state := previewState(t, "search")
	preview, err := renderSemanticPreview(state)
	if err != nil {
		t.Fatal(err)
	}
	for name, limits := range map[string]PreviewLimits{
		"zero count":         {MaxCount: 0, MaxBytes: int64(len(preview))},
		"negative count":     {MaxCount: -1, MaxBytes: int64(len(preview))},
		"zero bytes":         {MaxCount: 1, MaxBytes: 0},
		"negative bytes":     {MaxCount: 1, MaxBytes: -1},
		"bytes over maximum": {MaxCount: 1, MaxBytes: MaxPreviewBytes + 1},
	} {
		if _, err := OpenPreviewStore(filepath.Join(t.TempDir(), name), limits); err == nil {
			t.Fatalf("%s limits were accepted", name)
		}
	}
	maximum, err := OpenPreviewStore(t.TempDir(), PreviewLimits{MaxCount: 1, MaxBytes: MaxPreviewBytes})
	if err != nil {
		t.Fatalf("exact maximum preview byte limit rejected: %v", err)
	}
	if err := maximum.Close(); err != nil {
		t.Fatal(err)
	}

	exact, err := OpenPreviewStore(t.TempDir(), PreviewLimits{MaxCount: 1, MaxBytes: int64(len(preview))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exact.Close() })
	entry, err := exact.Put(1, state)
	if err != nil {
		t.Fatal(err)
	}
	again, err := exact.Put(1, state)
	if err != nil || again != entry {
		t.Fatalf("idempotent put = %+v, %v", again, err)
	}

	tooSmall, err := OpenPreviewStore(t.TempDir(), PreviewLimits{MaxCount: 1, MaxBytes: int64(len(preview) - 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tooSmall.Close() })
	if _, err := tooSmall.Put(1, state); err == nil {
		t.Fatal("preview larger than the byte limit was accepted")
	}
}

func TestPreviewStoreRejectsNonDirectoryRoots(t *testing.T) {
	for name, prepare := range map[string]func(string) error{
		"regular file": func(path string) error { return os.WriteFile(path, []byte("not a directory"), 0o600) },
		"symlink": func(path string) error {
			target := t.TempDir()
			return os.Symlink(target, path)
		},
	} {
		stateDirectory := t.TempDir()
		snapshots := filepath.Join(stateDirectory, "snapshots")
		if err := prepare(snapshots); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenPreviewStore(stateDirectory, DefaultPreviewLimits()); err == nil {
			t.Fatalf("%s snapshot root was accepted", name)
		}
	}
}

func TestPreviewStoreDoesNotChmodRejectedSymlinkTarget(t *testing.T) {
	stateDirectory := t.TempDir()
	target := t.TempDir()
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(stateDirectory, "snapshots")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPreviewStore(stateDirectory, DefaultPreviewLimits()); err == nil {
		t.Fatal("symlink snapshot root was accepted")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("rejected symlink target mode changed to %o", info.Mode().Perm())
	}
}

func TestPreviewStoreRejectsUnsafeRegistryKindsAndModes(t *testing.T) {
	for name, prepare := range map[string]func(string) error{
		"directory": func(path string) error { return os.Mkdir(path, 0o700) },
		"symlink": func(path string) error {
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("[]"), 0o600); err != nil {
				return err
			}
			return os.Symlink(target, path)
		},
		"group readable": func(path string) error {
			if err := os.WriteFile(path, []byte("[]"), 0o640); err != nil {
				return err
			}
			return os.Chmod(path, 0o640)
		},
	} {
		t.Run(name, func(t *testing.T) {
			stateDirectory := t.TempDir()
			root := filepath.Join(stateDirectory, "snapshots")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := prepare(filepath.Join(root, previewRegistryName)); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenPreviewStore(stateDirectory, DefaultPreviewLimits()); err == nil {
				t.Fatal("unsafe registry was accepted")
			}
		})
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

func TestPreviewRegistryRejectsInvalidBoundsAndIsDeterministicallyOrdered(t *testing.T) {
	state := previewState(t, "timeline")
	store, err := OpenPreviewStore(t.TempDir(), DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	root := store.root
	for _, sequence := range []uint64{9, 2} {
		if _, err := store.Put(sequence, state); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := os.ReadFile(filepath.Join(root, previewRegistryName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(registry), `"sequence":2`) || strings.Index(string(registry), `"sequence":2`) > strings.Index(string(registry), `"sequence":9`) {
		t.Fatalf("registry is not sequence ordered: %s", registry)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var persisted []PreviewEntry
	if err := json.Unmarshal(registry, &persisted); err != nil {
		t.Fatal(err)
	}
	exactBytes := persisted[0].Bytes + persisted[1].Bytes
	reopened, err := OpenPreviewStore(filepath.Dir(root), PreviewLimits{MaxCount: 2, MaxBytes: exactBytes})
	if err != nil {
		t.Fatalf("registry at exact count/byte limits: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	digest, err := visualDigest(state)
	if err != nil {
		t.Fatal(err)
	}
	valid := PreviewEntry{Sequence: 1, VisualDigest: digest, RelativePath: canonicalPreviewPath(1, digest), PNGSHA256: strings.Repeat("a", 64), Bytes: 1}
	for name, mutate := range map[string]func(*PreviewEntry){
		"zero sequence": func(entry *PreviewEntry) { entry.Sequence = 0 },
		"zero bytes":    func(entry *PreviewEntry) { entry.Bytes = 0 },
		"bad visual hash": func(entry *PreviewEntry) {
			entry.VisualDigest = "bad"
		},
		"bad png hash":  func(entry *PreviewEntry) { entry.PNGSHA256 = "bad" },
		"byte overflow": func(entry *PreviewEntry) { entry.Bytes = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			stateDirectory := t.TempDir()
			snapshotRoot := filepath.Join(stateDirectory, "snapshots")
			if err := os.Mkdir(snapshotRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			entry := valid
			mutate(&entry)
			data, err := json.Marshal([]PreviewEntry{entry})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(snapshotRoot, previewRegistryName), data, 0o600); err != nil {
				t.Fatal(err)
			}
			limits := DefaultPreviewLimits()
			if name == "byte overflow" {
				limits.MaxBytes = 1
			}
			_, err = OpenPreviewStore(stateDirectory, limits)
			if err == nil {
				t.Fatal("invalid registry entry was accepted")
			}
			if name == "zero bytes" && err.Error() != "preview registry contains an invalid entry" {
				t.Fatalf("zero-byte registry diagnosis = %q", err)
			}
		})
	}
}

func TestPreviewRegistryRejectsDeclaredSizeThatDoesNotMatchFile(t *testing.T) {
	stateDirectory := t.TempDir()
	store, err := OpenPreviewStore(stateDirectory, DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(1, previewState(t, "timeline")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(stateDirectory, "snapshots", previewRegistryName)
	registry, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	var entries []PreviewEntry
	if err := json.Unmarshal(registry, &entries); err != nil {
		t.Fatal(err)
	}
	entries[0].Bytes = 1
	registry, err = json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryPath, registry, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPreviewStore(stateDirectory, DefaultPreviewLimits()); err == nil {
		t.Fatal("registry with understated preview size was accepted")
	}
}

func TestPreviewRegistryWriteSizeBoundary(t *testing.T) {
	if err := validatePreviewRegistrySize(int(MaxPreviewRegistryBytes)); err != nil {
		t.Fatalf("exact registry byte limit rejected: %v", err)
	}
	if err := validatePreviewRegistrySize(int(MaxPreviewRegistryBytes) + 1); err == nil {
		t.Fatal("oversized registry accepted")
	}
}

func TestAnchoredRegularReadCannotSwitchToPathReplacement(t *testing.T) {
	for _, name := range []string{"preview.png", previewRegistryName} {
		t.Run(name, func(t *testing.T) {
			rootPath := t.TempDir()
			original := []byte("trusted-original")
			if err := os.WriteFile(filepath.Join(rootPath, name), original, 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = root.Close() })
			opened, err := root.Open(name)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = opened.Close() })
			if err := os.Rename(filepath.Join(rootPath, name), filepath.Join(rootPath, name+".old")); err != nil {
				t.Fatal(err)
			}
			replacement := filepath.Join(t.TempDir(), "replacement")
			if err := os.WriteFile(replacement, []byte("untrusted-replacement"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(replacement, filepath.Join(rootPath, name)); err != nil {
				t.Fatal(err)
			}
			data, err := readOpenedRegular(opened, 1024, int64(len(original)))
			if err != nil || !bytes.Equal(data, original) {
				t.Fatalf("descriptor read switched objects: data=%q err=%v", data, err)
			}
			if _, err := (&PreviewStore{rootFS: root}).readAnchoredRegular(name, 1024, int64(len(original))); err == nil {
				t.Fatal("no-follow open accepted replacement symlink")
			}
		})
	}
}

func TestOpenedRegularReadHonorsExactAndZeroExpectedSizes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preview.png")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := readOpenedRegular(file, 1, 1); err != nil || string(data) != "x" {
		t.Fatalf("read at exact limits = %q, %v", data, err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	file, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := readOpenedRegular(file, 1, 0); err == nil {
		t.Fatal("nonempty file accepted with zero expected bytes")
	}
}

func TestPreviewStoreRevalidatesFileTypeSizeModeAndCanonicalRoot(t *testing.T) {
	stateDirectory := t.TempDir()
	store, err := OpenPreviewStore(stateDirectory, DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	state := previewState(t, "search")
	entry, err := store.Put(3, state)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDirectory, filepath.FromSlash(entry.RelativePath))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, replace := range map[string]func() error{
		"directory":  func() error { return os.Mkdir(path, 0o700) },
		"short file": func() error { return os.WriteFile(path, original[:len(original)-1], 0o600) },
		"group readable": func() error {
			if err := os.WriteFile(path, original, 0o640); err != nil {
				return err
			}
			return os.Chmod(path, 0o640)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if err := replace(); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Read(entry, state); err == nil {
				t.Fatal("unsafe preview file was accepted")
			}
		})
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	store.root += string(os.PathSeparator)
	if _, err := store.Read(entry, state); err == nil {
		t.Fatal("noncanonical root spelling was accepted")
	}
}

func TestPreviewHelpersPreserveCanonicalIdentityAndAtomicBytes(t *testing.T) {
	if validSHA256("not-hex") || validSHA256(strings.Repeat("a", 62)) || !validSHA256(strings.Repeat("a", 64)) {
		t.Fatal("SHA-256 syntax validation changed")
	}
	if got := canonicalPreviewPath(7, "1234567890123456"); got != "snapshots/00000000000000000007-1234567890123456.png" {
		t.Fatalf("16-byte prefix path = %q", got)
	}
	if got := canonicalPreviewPath(7, "12345678901234567"); got != "snapshots/00000000000000000007-1234567890123456.png" {
		t.Fatalf("long prefix path = %q", got)
	}
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if err := writeAtomic(root, "value", []byte("complete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(rootPath, "value")); err != nil || string(got) != "complete" {
		t.Fatalf("atomic value = %q, %v", got, err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "blocked"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(root, "blocked", []byte("cannot replace a directory"), 0o600); err == nil {
		t.Fatal("atomic rename failure was accepted")
	}
	for _, test := range []struct {
		used, added, limit int64
		want               bool
	}{
		{used: 0, added: 5, limit: 5, want: false},
		{used: 4, added: 1, limit: 5, want: false},
		{used: 4, added: 2, limit: 5, want: true},
		{used: math.MaxInt64, added: 1, limit: math.MaxInt64, want: true},
	} {
		if got := exceedsByteLimit(test.used, test.added, test.limit); got != test.want {
			t.Fatalf("exceedsByteLimit(%d, %d, %d) = %t, want %t", test.used, test.added, test.limit, got, test.want)
		}
	}
}

func TestSemanticPreviewClampsSelectionToVisibleRows(t *testing.T) {
	state := previewState(t, "many-results")
	state.ItemCount = 8
	state.SelectedIndex = 7
	data, err := renderSemanticPreview(state)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if got := rgbaAt(decoded, 30, 142); got != ([4]uint32{0x5858, 0x5b5b, 0x7070, 0xffff}) {
		t.Fatalf("clamped selected row = %#v", got)
	}
	if got := rgbaAt(decoded, 30, 122); got != ([4]uint32{0x4545, 0x4747, 0x5a5a, 0xffff}) {
		t.Fatalf("row before clamped selection = %#v", got)
	}
}

func TestSemanticPreviewHasStableMeaningfulRegions(t *testing.T) {
	states := []struct {
		name   string
		state  VisualState
		accent [4]uint32
	}{
		{name: "ready", state: previewState(t, "ready"), accent: [4]uint32{0x8989, 0xb4b4, 0xfafa, 0xffff}},
		{name: "pending", state: func() VisualState { state := previewState(t, "pending"); state.Pending = true; return state }(), accent: [4]uint32{0xf9f9, 0xe2e2, 0xafaf, 0xffff}},
		{name: "error", state: func() VisualState { state := previewState(t, "error"); state.HasError = true; return state }(), accent: [4]uint32{0xf3f3, 0x8b8b, 0xa8a8, 0xffff}},
	}
	for _, test := range states {
		t.Run(test.name, func(t *testing.T) {
			data, err := renderSemanticPreview(test.state)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if decoded.Bounds() != (image.Rect(0, 0, 320, 180)) {
				t.Fatalf("bounds = %v", decoded.Bounds())
			}
			if got := rgbaAt(decoded, 13, 40); got != test.accent {
				t.Fatalf("accent = %#v, want %#v", got, test.accent)
			}
			if got := rgbaAt(decoded, 30, 102); got != ([4]uint32{0x5858, 0x5b5b, 0x7070, 0xffff}) {
				t.Fatalf("selected row = %#v", got)
			}
			if got := rgbaAt(decoded, 30, 82); got != ([4]uint32{0x4545, 0x4747, 0x5a5a, 0xffff}) {
				t.Fatalf("ordinary row = %#v", got)
			}
			if got := rgbaAt(decoded, 220, 40); got != ([4]uint32{0x2424, 0x2727, 0x3a3a, 0xffff}) {
				t.Fatalf("detail region = %#v", got)
			}
		})
	}
}

func rgbaAt(value image.Image, x, y int) [4]uint32 {
	r, g, b, a := value.At(x, y).RGBA()
	return [4]uint32{r, g, b, a}
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
