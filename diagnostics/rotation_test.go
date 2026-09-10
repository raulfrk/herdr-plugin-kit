package diagnostics

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rotationConfig(t *testing.T) Config {
	t.Helper()
	c := DefaultConfig(t.TempDir())
	c.Now = func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) }
	return c
}

func rotationRecord(t *testing.T, r *Recorder, message string) {
	t.Helper()
	if _, err := r.record(Event{Level: LevelInfo, Kind: KindDiagnostic, Message: message}); err != nil {
		t.Fatal(err)
	}
}

func TestSegmentsRotateOversizedAndRetainLoweredQuota(t *testing.T) {
	c := rotationConfig(t)
	r, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	rotationRecord(t, r, strings.Repeat("x", int(segmentTarget)+1))
	rotationRecord(t, r, "tail")
	if len(r.segments) != 2 || r.segments[0].count != 1 {
		t.Fatalf("segments: %+v", r.segments)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	c.MaxBytes = 4096
	r, err = Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if len(r.records) != 1 || r.records[0].event.Message != "tail" || r.health.CorruptRecords != 0 || r.health.Dropped != 1 {
		t.Fatalf("lowered quota: records=%d health=%+v", len(r.records), r.health)
	}
	rotationRecord(t, r, "equal time")
	if r.next != 3 || !r.records[0].event.Time.Equal(r.records[1].event.Time) {
		t.Fatal("sequence/time continuity lost")
	}
}

func TestSegmentsSequenceExhaustionDoesNotPublish(t *testing.T) {
	r, err := Open(rotationConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.next = ^uint64(0)
	before, err := os.ReadFile(r.file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.record(Event{Level: LevelInfo, Kind: KindDiagnostic, Message: "overflow"}); err == nil {
		t.Fatal("sequence wrapped")
	}
	after, err := os.ReadFile(r.file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || len(r.records) != 0 {
		t.Fatal("exhaustion published data")
	}
}

// Each hook still performs the actual OS operation. The ordinal selects a
// publication boundary, not an algorithm branch or a simulated filesystem.
func publicationHooks(hit func(string) (bool, bool)) storageHooks {
	h := defaultStorageHooks()
	fault := errors.New("injected publication failure")
	h.open = func(p string, flags int, mode os.FileMode) (*os.File, error) {
		fail, after := hit("open:" + filepath.Base(p))
		if fail && !after {
			return nil, fault
		}
		f, err := os.OpenFile(p, flags, mode)
		if fail && err == nil {
			err = fault
		}
		return f, err
	}
	h.createTemp = func(dir, pattern string) (*os.File, error) {
		fail, after := hit("temporary")
		if fail && !after {
			return nil, fault
		}
		f, err := os.CreateTemp(dir, pattern)
		if fail && err == nil {
			err = fault
		}
		return f, err
	}
	h.write = func(f *os.File, b []byte) (int, error) {
		fail, after := hit("write:" + filepath.Base(f.Name()))
		if fail && !after {
			return 0, fault
		}
		n, err := f.Write(b)
		if fail && err == nil {
			err = fault
		}
		return n, err
	}
	wrap := func(name string, op func(*os.File) error) func(*os.File) error {
		return func(f *os.File) error {
			fail, after := hit(name + ":" + filepath.Base(f.Name()))
			if fail && !after {
				return fault
			}
			err := op(f)
			if fail && err == nil {
				return fault
			}
			return err
		}
	}
	h.sync = wrap("sync", (*os.File).Sync)
	h.close = wrap("close", (*os.File).Close)
	h.rename = func(a, b string) error {
		fail, after := hit("rename:" + filepath.Base(b))
		if fail && !after {
			return fault
		}
		err := os.Rename(a, b)
		if fail && err == nil {
			return fault
		}
		return err
	}
	h.remove = func(p string) error {
		fail, after := hit("remove:" + filepath.Base(p))
		if fail && !after {
			return fault
		}
		err := os.Remove(p)
		if fail && err == nil {
			return fault
		}
		return err
	}
	h.mkdir = func(p string, mode os.FileMode) error {
		fail, after := hit("mkdir:" + filepath.Base(p))
		if fail && !after {
			return fault
		}
		err := os.Mkdir(p, mode)
		if fail && err == nil {
			return fault
		}
		return err
	}
	return h
}

func TestSegmentPublicationFailuresPreservePublishedMemory(t *testing.T) {
	for _, lane := range []string{"append", "rotation", "boundary", "unlink"} {
		t.Run(lane, func(t *testing.T) {
			setup := func(t *testing.T) (*Recorder, Config, string) {
				c := rotationConfig(t)
				if lane == "boundary" || lane == "unlink" {
					c.MaxEvents = 1
				}
				r, err := Open(c)
				if err != nil {
					t.Fatal(err)
				}
				message := "small"
				if lane == "rotation" || lane == "unlink" {
					message = strings.Repeat("x", int(segmentTarget))
				}
				rotationRecord(t, r, message)
				return r, c, "pending"
			}
			r, _, message := setup(t)
			var trace []string
			r.storage = publicationHooks(func(op string) (bool, bool) { trace = append(trace, op); return false, false })
			rotationRecord(t, r, message)
			r.storage = defaultStorageHooks()
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			for boundary, op := range trace {
				for _, after := range []bool{false, true} {
					t.Run(fmt.Sprintf("%02d-%s-after=%t", boundary, op, after), func(t *testing.T) {
						r, c, message := setup(t)
						prior, next, size, dropped := r.records[0], r.next, r.bytes, r.health.Dropped
						calls := 0
						r.storage = publicationHooks(func(string) (bool, bool) { fail := calls == boundary; calls++; return fail, after })
						if _, err := r.record(Event{Level: LevelInfo, Kind: KindDiagnostic, Message: message}); err == nil {
							t.Fatal("fault not returned")
						}
						if len(r.records) != 1 || r.records[0] != prior || r.next != next || r.bytes != size || r.health.Writable || r.health.Dropped != dropped+1 {
							t.Fatalf("failed publication changed memory: %+v", r.health)
						}
						if _, err := r.record(Event{Level: LevelInfo, Kind: KindDiagnostic, Message: "retry"}); !errors.Is(err, ErrStorage) {
							t.Fatalf("retry: %v", err)
						}
						if r.health.Dropped != dropped+1 {
							t.Fatal("failure counted twice")
						}
						r.storage = defaultStorageHooks()
						_ = r.Close()
						reopened, err := Open(c)
						if err != nil {
							t.Fatal(err)
						}
						defer reopened.Close()
						if len(reopened.records) == 0 || reopened.next < next || reopened.next > next+1 {
							t.Fatal("recovery lost acknowledged history or invented sequence")
						}
						if lane == "append" && after && reopened.next != next+1 {
							t.Fatal("complete failed append was not admitted")
						}
					})
				}
			}
		})
	}
}

func TestStorageHookDescriptorOwnership(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			h := defaultStorageHooks()
			var captured *os.File
			fault := errors.New("descriptor failure")
			h.open = func(p string, flags int, mode os.FileMode) (*os.File, error) {
				f, err := os.OpenFile(p, flags, mode)
				captured = f
				if err != nil {
					return f, err
				}
				return f, fault
			}
			if f, err := h.openFile(filepath.Join(t.TempDir(), "file"), os.O_CREATE|os.O_RDWR); err == nil || f != nil {
				t.Fatal("open leaked ownership")
			}
			if _, err := captured.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("open descriptor: %v", err)
			}
			h.createTemp = func(dir, pattern string) (*os.File, error) {
				f, err := os.CreateTemp(dir, pattern)
				captured = f
				if err != nil {
					return f, err
				}
				return f, fault
			}
			if f, err := h.temporary(t.TempDir()); err == nil || f != nil {
				t.Fatal("temporary leaked ownership")
			}
			if _, err := captured.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("temporary descriptor: %v", err)
			}
			f, err := os.CreateTemp(t.TempDir(), "close")
			if err != nil {
				t.Fatal(err)
			}
			captured = f
			h.close = func(f *os.File) error {
				if after {
					_ = f.Close()
				}
				return fault
			}
			if err := h.closeFile(&f); err == nil || f != nil {
				t.Fatal("close retained ownership")
			}
			if _, err := captured.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("close descriptor: %v", err)
			}
		})
	}
}

func TestSegmentOpenPublicationFailures(t *testing.T) {
	for _, migration := range []bool{false, true} {
		t.Run(fmt.Sprintf("migration=%t", migration), func(t *testing.T) {
			setup := func(t *testing.T) Config {
				c := rotationConfig(t)
				r, err := Open(c)
				if err != nil {
					t.Fatal(err)
				}
				rotationRecord(t, r, "survivor")
				data := append([]byte(nil), r.records[0].data...)
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				if migration {
					if err := os.Remove(segmentPath(filepath.Join(c.Directory, segmentDirectory), 1)); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(filepath.Join(c.Directory, segmentDirectory)); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(c.Directory, EventLogName), data, 0o600); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(filepath.Join(c.Directory, segmentDirectory, ".boundary-123.tmp"), []byte("orphan"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return c
			}
			c := setup(t)
			var trace []string
			r, err := openRecorder(c, publicationHooks(func(op string) (bool, bool) { trace = append(trace, op); return false, false }))
			if err != nil {
				t.Fatal(err)
			}
			r.storage = defaultStorageHooks()
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			// The trace ends at successful Open, before Close's barrier.
			for boundary, op := range trace {
				for _, after := range []bool{false, true} {
					t.Run(fmt.Sprintf("%02d-%s-after=%t", boundary, op, after), func(t *testing.T) {
						c := setup(t)
						calls := 0
						r, err := openRecorder(c, publicationHooks(func(string) (bool, bool) { fail := calls == boundary; calls++; return fail, after }))
						if err == nil {
							r.Close()
							t.Fatal("Open admitted recorder after publication failure")
						}
						r, err = Open(c)
						if err != nil {
							t.Fatal(err)
						}
						defer r.Close()
						if len(r.records) != 1 || r.records[0].event.Message != "survivor" {
							t.Fatal("Open recovery lost source")
						}
					})
				}
			}
		})
	}
}

func TestSegmentRecoveryBarriersPrecedeDeletion(t *testing.T) {
	c := rotationConfig(t)
	r, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	rotationRecord(t, r, strings.Repeat("x", int(segmentTarget)))
	rotationRecord(t, r, "survivor")
	var legacy []byte
	for _, record := range r.records {
		legacy = append(legacy, record.data...)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Directory, EventLogName), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(c.Directory, segmentDirectory)
	if err := os.WriteFile(filepath.Join(live, ".boundary-123.tmp"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c.MaxEvents = 1
	synced := map[string]bool{}
	legacyRemoved := false
	h := defaultStorageHooks()
	h.sync = func(f *os.File) error {
		if legacyRemoved && f.Name() == c.Directory {
			return errors.New("post-unlink parent sync")
		}
		synced[f.Name()] = true
		return f.Sync()
	}
	h.remove = func(path string) error {
		for _, required := range []string{segmentPath(live, 1), segmentPath(live, 2), live, c.Directory} {
			if !synced[required] {
				t.Fatalf("removed %s before sync %s", path, required)
			}
		}
		if path == filepath.Join(c.Directory, EventLogName) {
			legacyRemoved = true
		}
		return os.Remove(path)
	}
	if r, err := openRecorder(c, h); err == nil {
		r.Close()
		t.Fatal("post-legacy-unlink failure admitted recorder")
	}
	if !legacyRemoved {
		t.Fatal("legacy unlink boundary not reached")
	}
	r, err = Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if len(r.records) != 1 || r.records[0].event.Message != "survivor" {
		t.Fatal("survivor lost")
	}
}

func TestSegmentRecoveryRejectsForeignEntriesAndSymlinks(t *testing.T) {
	for _, name := range []string{"foreign", "events-00000000000000000000.jsonl", "events-00000000000000000002.jsonl"} {
		t.Run(name, func(t *testing.T) {
			c := rotationConfig(t)
			r, err := Open(c)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(c.Directory, segmentDirectory, name)
			if strings.HasSuffix(name, "02.jsonl") {
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, []byte("untouched"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			if r, err := Open(c); err == nil {
				r.Close()
				t.Fatal("unsafe entry accepted")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "untouched" {
				t.Fatalf("foreign data changed: %q %v", data, err)
			}
		})
	}
}

func TestSegmentPartialTailRecovery(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			c := rotationConfig(t)
			r, err := Open(c)
			if err != nil {
				t.Fatal(err)
			}
			rotationRecord(t, r, "first")
			rotationRecord(t, r, "second")
			path := r.file.Name()
			data := append([]byte(nil), r.records[0].data...)
			last := r.records[1].data
			if complete {
				data = append(data, last[:len(last)-1]...)
			} else {
				data = append(data, last[:len(last)/2]...)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			r, err = Open(c)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			want, corrupt := uint64(1), uint64(1)
			if complete {
				want, corrupt = 2, 0
			}
			if r.next != want || r.health.CorruptRecords != corrupt {
				t.Fatalf("recovery next=%d health=%+v", r.next, r.health)
			}
			rotationRecord(t, r, "next")
			if r.next != want+1 {
				t.Fatal("recovery sequence")
			}
			persisted, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Count(persisted, []byte{'\n'}) != int(want+1) {
				t.Fatal("tail not repaired before append")
			}
		})
	}
}

func TestSegmentRetentionKeepsCapturedSnapshot(t *testing.T) {
	c := rotationConfig(t)
	c.MaxEvents = 1
	r, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	recordDebugSemantic(t, r, "session", "component", "captured", "", true)
	snapshot, err := r.DebugSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := snapshot.Select(context.Background(), DebugQuery{}, false)
	if err != nil {
		t.Fatal(err)
	}
	recordDebugSemantic(t, r, "session", "component", "replacement", "", true)
	if len(r.records) != 1 || r.next != 2 {
		t.Fatal("retention did not run")
	}
	window := view.Window(DebugWindowQuery{})
	if window.Total != 1 || len(window.Events) != 1 || window.Events[0].Sequence != 1 || window.Events[0].Code.String() != "captured" {
		t.Fatalf("snapshot changed after clearing dropped pointer: %+v", window)
	}
}

func TestSegmentHooksAreInstanceLocal(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			t.Parallel()
			r, err := Open(rotationConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			writes, syncs := 0, 0
			h := defaultStorageHooks()
			h.write = func(f *os.File, b []byte) (int, error) {
				writes++
				if fail {
					return 0, errors.New("local failure")
				}
				return f.Write(b)
			}
			h.sync = func(f *os.File) error { syncs++; return f.Sync() }
			r.storage = h
			_, err = r.record(Event{Level: LevelInfo, Kind: KindDiagnostic, Message: "ordinary"})
			if (err != nil) != fail || writes != 1 || syncs != 0 {
				t.Fatalf("instance fail=%t writes=%d syncs=%d err=%v", fail, writes, syncs, err)
			}
		})
	}
}

// Sized fixtures use canonical JSONL, including the delimiter in the byte count.
func rotationFixture(t *testing.T, sequence uint64, when time.Time, size int) []byte {
	t.Helper()
	event := Event{Version: EventSchemaVersion, Sequence: sequence, Time: when,
		Level: LevelInfo, Kind: KindDiagnostic, Message: "fixture"}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if size != 0 {
		padding := size - len(data) - 1
		if padding < 0 {
			t.Fatalf("fixture size %d below canonical size %d", size, len(data)+1)
		}
		event.Message += strings.Repeat("x", padding)
		data, err = json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
	}
	return append(data, '\n')
}

func rotationSeedSegments(t *testing.T, c Config, data ...[]byte) {
	t.Helper()
	dir := filepath.Join(c.Directory, segmentDirectory)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i, contents := range data {
		if err := os.WriteFile(segmentPath(dir, uint64(i+1)), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func rotationAssertDisk(t *testing.T, c Config, want map[uint64][]byte) {
	t.Helper()
	dir := filepath.Join(c.Directory, segmentDirectory)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want) {
		t.Fatalf("segment entries=%v, want %d files", entries, len(want))
	}
	for ordinal, expected := range want {
		data, err := os.ReadFile(segmentPath(dir, ordinal))
		if err != nil || !bytes.Equal(data, expected) {
			t.Fatalf("segment %d: bytes=%d want=%d equal=%t err=%v", ordinal, len(data), len(expected), bytes.Equal(data, expected), err)
		}
	}
}

func TestRotationContractNamesAndSuffix(t *testing.T) {
	if _, valid := segmentOrdinal("events-.jsonl"); valid {
		t.Fatal("short owned name accepted")
	}
	x, a, b := &storedEvent{data: []byte("X")}, &storedEvent{data: []byte("A")}, &storedEvent{data: []byte("B")}
	legacy := []*storedEvent{x, a, b}
	if !identicalSuffix([]*storedEvent{a, b}, legacy) {
		t.Fatal("two-record suffix rejected")
	}
	if identicalSuffix([]*storedEvent{a, x}, legacy) || identicalSuffix(legacy, []*storedEvent{a, b}) {
		t.Fatal("mismatching or longer suffix accepted")
	}
}

func TestRotationContractCloseDescriptor(t *testing.T) {
	for _, failSync := range []bool{false, true} {
		t.Run(fmt.Sprint(failSync), func(t *testing.T) {
			r, err := Open(rotationConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			captured := r.file
			t.Cleanup(func() { _ = captured.Close() })
			fault := errors.New("close sync sentinel")
			syncs, closes := 0, 0
			r.storage.sync = func(f *os.File) error {
				syncs++
				if f != captured {
					t.Fatal("synced wrong descriptor")
				}
				if failSync {
					return fault
				}
				return f.Sync()
			}
			r.storage.close = func(f *os.File) error { closes++; return f.Close() }
			err = r.Close()
			if (failSync && !errors.Is(err, fault)) || (!failSync && err != nil) {
				t.Fatalf("Close: %v", err)
			}
			if _, err := captured.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("descriptor not closed: %v", err)
			}
			if err := r.Close(); err != nil || syncs != 1 || closes != 1 {
				t.Fatalf("idempotent Close: syncs=%d closes=%d err=%v", syncs, closes, err)
			}
		})
	}
}

func TestRotationContractCanonicalRepair(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			c := rotationConfig(t)
			canonical := rotationFixture(t, 1, c.Now(), 0)
			raw := append([]byte("  "), canonical...)
			if corrupt {
				raw = append([]byte("not-json\n"), canonical...)
			}
			rotationSeedSegments(t, c, raw)
			r, err := Open(c)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			wantCorrupt := uint64(0)
			if corrupt {
				wantCorrupt = 1
			}
			if r.health.CorruptRecords != wantCorrupt || len(r.records) != 1 {
				t.Fatalf("repair health=%+v records=%d", r.health, len(r.records))
			}
			rotationAssertDisk(t, c, map[uint64][]byte{1: canonical})
		})
	}
}

func TestRotationContractRepairFailureDescriptors(t *testing.T) {
	for _, operation := range []string{"write", "sync"} {
		t.Run(operation, func(t *testing.T) {
			c := rotationConfig(t)
			raw := append([]byte("  "), rotationFixture(t, 1, c.Now(), 0)...)
			rotationSeedSegments(t, c, raw)
			var active, temporary *os.File
			t.Cleanup(func() {
				if active != nil {
					_ = active.Close()
				}
				if temporary != nil {
					_ = temporary.Close()
				}
			})
			h := defaultStorageHooks()
			h.open = func(path string, flags int, mode os.FileMode) (*os.File, error) {
				f, err := os.OpenFile(path, flags, mode)
				if flags&os.O_APPEND != 0 {
					active = f
				}
				return f, err
			}
			h.createTemp = func(dir, pattern string) (*os.File, error) {
				f, err := os.CreateTemp(dir, pattern)
				temporary = f
				return f, err
			}
			fault := errors.New("repair sentinel")
			h.write = func(f *os.File, data []byte) (int, error) {
				if f == temporary && operation == "write" {
					return 0, fault
				}
				return f.Write(data)
			}
			h.sync = func(f *os.File) error {
				if f == temporary && operation == "sync" {
					return fault
				}
				return f.Sync()
			}
			r, err := openRecorder(c, h)
			if r != nil {
				defer r.Close()
			}
			if !errors.Is(err, fault) {
				t.Fatalf("Open: %v", err)
			}
			for name, f := range map[string]*os.File{"active": active, "temporary": temporary} {
				if f == nil {
					t.Fatalf("%s descriptor never opened", name)
				}
				if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("%s not closed: %v", name, err)
				}
			}
			data, err := os.ReadFile(segmentPath(filepath.Join(c.Directory, segmentDirectory), 1))
			if err != nil || !bytes.Equal(data, raw) {
				t.Fatalf("failed repair changed original: %v", err)
			}
		})
	}
}

func TestRotationContractDiscardStage(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			c := rotationConfig(t)
			stage := filepath.Join(c.Directory, stagingDirectory)
			if err := os.Mkdir(stage, 0o700); err != nil {
				t.Fatal(err)
			}
			h := defaultStorageHooks()
			fault := errors.New("stage directory removal sentinel")
			removed, syncs := false, 0
			h.remove = func(path string) error {
				if path != stage {
					t.Fatalf("unexpected remove %s", path)
				}
				if fail {
					return fault
				}
				if err := os.Remove(path); err != nil {
					return err
				}
				removed = true
				return nil
			}
			h.sync = func(f *os.File) error {
				if f.Name() != c.Directory || !removed {
					t.Fatal("root sync before successful stage removal")
				}
				syncs++
				return f.Sync()
			}
			r := &Recorder{config: c, storage: h}
			err := r.discardStage(stage)
			if fail {
				if !errors.Is(err, fault) || syncs != 0 {
					t.Fatalf("discard: syncs=%d err=%v", syncs, err)
				}
			} else if err != nil || syncs != 1 {
				t.Fatalf("discard: syncs=%d err=%v", syncs, err)
			}
		})
	}
}

func TestRotationContractRecoveryPressure(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, condition := range []string{"clean", "expired", "corrupt"} {
			t.Run(fmt.Sprintf("legacy=%t/%s", legacy, condition), func(t *testing.T) {
				c := rotationConfig(t)
				fresh := rotationFixture(t, 2, c.Now(), 0)
				data := fresh
				if condition == "expired" {
					data = append(rotationFixture(t, 1, c.Now().Add(-2*c.MaxAge), 0), fresh...)
				}
				if condition == "corrupt" {
					data = append([]byte("bad-json\n"), fresh...)
				}
				if legacy {
					if err := os.WriteFile(filepath.Join(c.Directory, EventLogName), data, 0o600); err != nil {
						t.Fatal(err)
					}
				} else {
					rotationSeedSegments(t, c, data)
				}
				r, err := Open(c)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				dropped, corrupt := uint64(0), uint64(0)
				if condition == "expired" {
					dropped = 1
				}
				if condition == "corrupt" {
					corrupt = 1
				}
				if r.health.Dropped != dropped || r.health.CorruptRecords != corrupt || r.health.Pressure != (condition != "clean") || r.health.Events != 1 || r.health.Bytes != int64(len(fresh)) {
					t.Fatalf("recovery health=%+v", r.health)
				}
				rotationAssertDisk(t, c, map[uint64][]byte{1: fresh})
			})
		}
	}
}

func TestRotationContractOrphanRemovalBarrier(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			c := rotationConfig(t)
			rotationSeedSegments(t, c, rotationFixture(t, 1, c.Now(), 0))
			live := filepath.Join(c.Directory, segmentDirectory)
			orphan := filepath.Join(live, ".boundary-123.tmp")
			if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
				t.Fatal(err)
			}
			h := defaultStorageHooks()
			removed, postSyncs := false, 0
			fault := errors.New("post orphan removal sync sentinel")
			h.remove = func(path string) error {
				if err := os.Remove(path); err != nil {
					return err
				}
				if path == orphan {
					removed = true
				}
				return nil
			}
			h.sync = func(f *os.File) error {
				if f.Name() == live && removed {
					postSyncs++
					if fail {
						return fault
					}
				}
				return f.Sync()
			}
			r, err := openRecorder(c, h)
			if r != nil {
				defer r.Close()
			}
			if !removed || postSyncs != 1 || (fail && !errors.Is(err, fault)) || (!fail && err != nil) {
				t.Fatalf("orphan barrier: removed=%t syncs=%d err=%v", removed, postSyncs, err)
			}
			if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("orphan remains: %v", err)
			}
		})
	}
}

func TestRotationContractMigrationPacking(t *testing.T) {
	for _, shape := range []string{"small", "exact", "split", "oversized"} {
		t.Run(shape, func(t *testing.T) {
			c := rotationConfig(t)
			firstSize, secondSize := 0, 0
			switch shape {
			case "exact":
				firstSize, secondSize = int(segmentTarget)/2, int(segmentTarget)/2
			case "split":
				firstSize, secondSize = int(segmentTarget)/2, int(segmentTarget)/2+1
			case "oversized":
				firstSize = int(segmentTarget) + 1
			}
			first := rotationFixture(t, 1, c.Now(), firstSize)
			second := rotationFixture(t, 2, c.Now(), secondSize)
			data := append(append([]byte(nil), first...), second...)
			want := map[uint64][]byte{1: data}
			if shape == "split" {
				want = map[uint64][]byte{1: first, 2: second}
			}
			if shape == "oversized" {
				data, want = first, map[uint64][]byte{1: first}
			}
			if err := os.WriteFile(filepath.Join(c.Directory, EventLogName), data, 0o600); err != nil {
				t.Fatal(err)
			}
			h := defaultStorageHooks()
			creations := 0
			extra := errors.New("unexpected extra staged segment creation")
			h.open = func(path string, flags int, mode os.FileMode) (*os.File, error) {
				if flags&os.O_CREATE != 0 {
					creations++
					if creations > len(want) {
						return nil, extra
					}
				}
				return os.OpenFile(path, flags, mode)
			}
			r, err := openRecorder(c, h)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if creations != len(want) || len(r.segments) != len(want) {
				t.Fatalf("creations=%d segments=%d want=%d", creations, len(r.segments), len(want))
			}
			rotationAssertDisk(t, c, want)
			if _, err := os.Stat(filepath.Join(c.Directory, EventLogName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("legacy remains: %v", err)
			}
		})
	}
}

func TestRotationContractMigrationFailureDescriptors(t *testing.T) {
	for _, operation := range []string{"write", "sync", "close-before", "close-after"} {
		t.Run(operation, func(t *testing.T) {
			c := rotationConfig(t)
			first := rotationFixture(t, 1, c.Now(), int(segmentTarget)/2)
			data := append(first, rotationFixture(t, 2, c.Now(), int(segmentTarget)/2+1)...)
			legacy := filepath.Join(c.Directory, EventLogName)
			if err := os.WriteFile(legacy, data, 0o600); err != nil {
				t.Fatal(err)
			}
			h := defaultStorageHooks()
			fault := errors.New("staged segment sentinel")
			var captured *os.File
			t.Cleanup(func() {
				if captured != nil {
					_ = captured.Close()
				}
			})
			creations, renames, syncs, writes := 0, 0, 0, 0
			h.open = func(path string, flags int, mode os.FileMode) (*os.File, error) {
				f, err := os.OpenFile(path, flags, mode)
				if flags&os.O_CREATE != 0 {
					creations++
					if captured == nil {
						captured = f
					}
				}
				return f, err
			}
			h.write = func(f *os.File, b []byte) (int, error) {
				if f == captured {
					writes++
					if operation == "write" {
						return 0, fault
					}
				}
				return f.Write(b)
			}
			h.sync = func(f *os.File) error {
				if f == captured {
					syncs++
					if writes != 1 {
						t.Fatalf("intermediate sync after %d writes", writes)
					}
					if operation == "sync" {
						return fault
					}
				}
				return f.Sync()
			}
			h.close = func(f *os.File) error {
				if f == captured && strings.HasPrefix(operation, "close-") {
					if syncs != 1 {
						t.Fatalf("intermediate close before sync: %d", syncs)
					}
					if operation == "close-after" {
						if err := f.Close(); err != nil {
							return err
						}
					}
					return fault
				}
				return f.Close()
			}
			h.rename = func(a, b string) error { renames++; return os.Rename(a, b) }
			r, err := openRecorder(c, h)
			if r != nil {
				defer r.Close()
			}
			if !errors.Is(err, fault) || creations != 1 || renames != 0 {
				t.Fatalf("migration: creations=%d renames=%d err=%v", creations, renames, err)
			}
			if captured == nil {
				t.Fatal("staged descriptor not opened")
			}
			if _, err := captured.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("staged descriptor not closed: %v", err)
			}
			persisted, err := os.ReadFile(legacy)
			if err != nil || !bytes.Equal(persisted, data) {
				t.Fatalf("legacy changed: %v", err)
			}
			if _, err := os.Stat(filepath.Join(c.Directory, segmentDirectory)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("live published: %v", err)
			}
		})
	}
}

func TestRotationContractExactAppend(t *testing.T) {
	c := rotationConfig(t)
	first := rotationFixture(t, 1, c.Now(), int(segmentTarget)/2)
	second := rotationFixture(t, 2, c.Now(), int(segmentTarget)/2)
	r, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for i, data := range [][]byte{first, second} {
		var event Event
		if err := json.Unmarshal(data, &event); err != nil {
			t.Fatal(err)
		}
		before, err := r.file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		syncs := 0
		r.storage.sync = func(f *os.File) error { syncs++; return f.Sync() }
		rotationRecord(t, r, event.Message)
		after, err := r.file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before, after) || len(r.segments) != 1 || r.segments[0].ordinal != 1 || syncs != 0 {
			t.Fatalf("append %d rotated or synced: segments=%+v syncs=%d", i, r.segments, syncs)
		}
	}
	rotationAssertDisk(t, c, map[uint64][]byte{1: append(first, second...)})
}

func TestRotationContractAppendBarriers(t *testing.T) {
	for _, rotating := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("rotating=%t/fail=%t", rotating, fail), func(t *testing.T) {
				c := rotationConfig(t)
				if !rotating {
					c.MaxEvents = 1
				}
				r, err := Open(c)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { r.storage = defaultStorageHooks(); _ = r.Close() }()
				message := "old"
				if rotating {
					message = strings.Repeat("x", int(segmentTarget))
				}
				rotationRecord(t, r, message)
				var pending *os.File
				pendingSyncs, barriers := 0, 0
				fault := errors.New("pending data sync sentinel")
				r.storage.write = func(f *os.File, b []byte) (int, error) {
					if !strings.HasPrefix(filepath.Base(f.Name()), ".boundary-") {
						pending = f
					}
					return f.Write(b)
				}
				r.storage.sync = func(f *os.File) error {
					if f == pending {
						pendingSyncs++
						if fail {
							return fault
						}
					}
					if rotating && f.Name() == filepath.Join(c.Directory, segmentDirectory) {
						if pendingSyncs != 1 {
							t.Fatal("namespace barrier before pending data sync")
						}
						barriers++
					}
					return f.Sync()
				}
				r.storage.createTemp = func(dir, pattern string) (*os.File, error) {
					if rotating || pendingSyncs != 1 {
						t.Fatal("retention replacement before pending data sync")
					}
					barriers++
					return os.CreateTemp(dir, pattern)
				}
				_, err = r.record(Event{Level: LevelInfo, Kind: KindDiagnostic, Message: "pending"})
				wantBarriers := 1
				if fail {
					wantBarriers = 0
				}
				if pending == nil || pendingSyncs != 1 || barriers != wantBarriers || (fail && !errors.Is(err, fault)) || (!fail && err != nil) {
					t.Fatalf("pending syncs=%d barriers=%d err=%v", pendingSyncs, barriers, err)
				}
				if r.health.Writable == fail {
					t.Fatalf("writable after append: %+v", r.health)
				}
			})
		}
	}
}

func TestRotationContractRetentionDisk(t *testing.T) {
	for _, boundary := range []string{"whole-first", "inside-second", "all"} {
		t.Run(boundary, func(t *testing.T) {
			c := rotationConfig(t)
			old := c.Now().Add(-2 * c.MaxAge)
			first := rotationFixture(t, 1, old, 0)
			fresh := rotationFixture(t, 2, c.Now(), 0)
			want := map[uint64][]byte{2: fresh}
			wantRecords, wantDropped := 1, uint64(1)
			switch boundary {
			case "whole-first":
				rotationSeedSegments(t, c, first, fresh)
			case "inside-second":
				fresh = rotationFixture(t, 3, c.Now(), 0)
				last := rotationFixture(t, 4, c.Now(), 0)
				rotationSeedSegments(t, c, first, append(rotationFixture(t, 2, old, 0), fresh...), last)
				want, wantRecords, wantDropped = map[uint64][]byte{2: fresh, 3: last}, 2, 2
			case "all":
				rotationSeedSegments(t, c, first, rotationFixture(t, 2, old, 0))
				want, wantRecords, wantDropped = map[uint64][]byte{2: nil}, 0, 2
			}
			lastOrdinal := uint64(2)
			if boundary == "inside-second" {
				lastOrdinal = 3
			}
			lastPath := segmentPath(filepath.Join(c.Directory, segmentDirectory), lastOrdinal)
			before, err := os.Stat(lastPath)
			if err != nil {
				t.Fatal(err)
			}
			r, err := Open(c)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if len(r.records) != wantRecords || r.health.Dropped != wantDropped || !r.health.Pressure {
				t.Fatalf("retention: records=%d health=%+v", len(r.records), r.health)
			}
			rotationAssertDisk(t, c, want)
			if _, err := os.Stat(segmentPath(filepath.Join(c.Directory, segmentDirectory), 1)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("expired prefix remains: %v", err)
			}
			after, err := r.file.Stat()
			if err != nil {
				t.Fatalf("active descriptor invalid: %v", err)
			}
			if boundary != "all" && !os.SameFile(before, after) {
				t.Fatal("untouched last segment replaced")
			}
			if boundary == "all" {
				rotationRecord(t, r, "after expiry")
				want[2] = append([]byte(nil), r.records[0].data...)
				wantRecords = 1
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(c)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if len(reopened.records) != wantRecords {
				t.Fatalf("reopen records=%d want=%d", len(reopened.records), wantRecords)
			}
			rotationAssertDisk(t, c, want)
			var expected, actual []byte
			for ordinal := uint64(2); ordinal <= lastOrdinal; ordinal++ {
				expected = append(expected, want[ordinal]...)
			}
			for _, record := range reopened.records {
				actual = append(actual, record.data...)
			}
			if !bytes.Equal(actual, expected) {
				t.Fatal("reopen suffix differs")
			}
		})
	}
}

func TestRotationContractCleanReopenNoRewrite(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			c := rotationConfig(t)
			var data []byte
			if !empty {
				data = rotationFixture(t, 1, c.Now(), 0)
			}
			rotationSeedSegments(t, c, data)
			live := filepath.Join(c.Directory, segmentDirectory)
			before, err := os.Stat(segmentPath(live, 1))
			if err != nil {
				t.Fatal(err)
			}
			h := defaultStorageHooks()
			unexpected := errors.New("unexpected clean-store mutation or extra sync")
			mutations, liveSyncs := 0, 0
			h.createTemp = func(string, string) (*os.File, error) { mutations++; return nil, unexpected }
			h.write = func(*os.File, []byte) (int, error) { mutations++; return 0, unexpected }
			h.rename = func(string, string) error { mutations++; return unexpected }
			h.sync = func(f *os.File) error {
				if f.Name() == live {
					liveSyncs++
					if liveSyncs > 1 {
						return unexpected
					}
				}
				return f.Sync()
			}
			r, err := openRecorder(c, h)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			after, err := r.file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) || mutations != 0 || liveSyncs != 1 {
				t.Fatalf("clean reopen: mutations=%d liveSyncs=%d sameFile=%t", mutations, liveSyncs, os.SameFile(before, after))
			}
			if r.health.Pressure || r.health.Dropped != 0 || r.health.CorruptRecords != 0 {
				t.Fatalf("clean health: %+v", r.health)
			}
			rotationAssertDisk(t, c, map[uint64][]byte{1: data})
		})
	}
}

func TestRotationContractShortWriteIdentity(t *testing.T) {
	fault := errors.New("short write sentinel")
	for _, writeErr := range []error{nil, fault} {
		t.Run(fmt.Sprint(writeErr), func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "short-write")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			h := defaultStorageHooks()
			h.write = func(f *os.File, data []byte) (int, error) {
				n, err := f.Write(data[:2])
				if err != nil {
					return n, err
				}
				return n, writeErr
			}
			want := writeErr
			if want == nil {
				want = io.ErrShortWrite
			}
			err = h.writeAll(f, []byte("short"))
			if !errors.Is(err, want) || (writeErr != nil && errors.Is(err, io.ErrShortWrite)) {
				t.Fatalf("short write identity: %v want=%v", err, want)
			}
			data, err := os.ReadFile(f.Name())
			if err != nil || string(data) != "sh" {
				t.Fatalf("physical short write=%q err=%v", data, err)
			}
		})
	}
}

func TestSegmentHistoricalLineLimit(t *testing.T) {
	if historicalLineLimit != 1073741825 {
		t.Fatalf("historical physical-line budget=%d, want 1073741825", historicalLineLimit)
	}
}

func TestSegmentReaderPhysicalLineBudget(t *testing.T) {
	c := rotationConfig(t)
	first := rotationFixture(t, 1, c.Now(), 0)
	second := rotationFixture(t, 2, c.Now(), 0)
	below := rotationFixture(t, 1, c.Now(), 255)
	exact := rotationFixture(t, 1, c.Now(), 256)
	above := rotationFixture(t, 1, c.Now(), 257)
	aboveEOF := rotationFixture(t, 1, c.Now(), 258)
	for _, test := range []struct {
		name    string
		input   []byte
		want    []byte
		count   int
		next    uint64
		corrupt uint64
		dirty   bool
	}{
		{"empty", nil, nil, 0, 0, 0, false},
		{"below", below, below, 1, 1, 0, false},
		{"exact", exact, exact, 1, 1, 0, false},
		{"above", above, nil, 0, 0, 1, true},
		{"oversized-then-valid", append(append([]byte(nil), above...), second...), second, 1, 2, 1, true},
		{"two-oversized-then-valid", append(append(append([]byte(nil), above...), above...), second...), second, 1, 2, 2, true},
		{"below-without-delimiter", exact[:len(exact)-1], exact, 1, 1, 0, true},
		{"exact-without-delimiter", above[:len(above)-1], above, 1, 1, 0, true},
		{"above-without-delimiter", aboveEOF[:len(aboveEOF)-1], nil, 0, 0, 1, true},
		{"blank", []byte("\n \t\n"), nil, 0, 0, 0, true},
		{"blank-without-delimiter", []byte(" \t"), nil, 0, 0, 0, true},
		{"noncanonical-then-clean", append(append([]byte("  "), first...), second...), append(append([]byte(nil), first...), second...), 2, 2, 0, true},
		{"corrupt-then-clean", append([]byte("bad-json\n"), first...), first, 1, 1, 1, true},
	} {
		for _, bufferSize := range []int{16, 512} {
			t.Run(fmt.Sprintf("%s/buffer=%d", test.name, bufferSize), func(t *testing.T) {
				// A small physical-line budget covers exact boundaries without a
				// giant fixture. The 16-byte buffer splits valid records into parts.
				r := &Recorder{config: c, reportAt: c.Now(), health: Health{Writable: true}}
				s := segment{bytes: int64(len(test.input))}
				reader := bufio.NewReaderSize(bytes.NewReader(test.input), bufferSize)
				if reader.Size() != bufferSize {
					t.Fatalf("reader buffer=%d, want %d", reader.Size(), bufferSize)
				}
				if err := r.readSegmentRecords(reader, &s, 256); err != nil {
					t.Fatal(err)
				}
				var got []byte
				for _, record := range r.records {
					got = append(got, record.data...)
				}
				if !bytes.Equal(got, test.want) || len(r.records) != test.count || r.next != test.next || r.bytes != int64(len(test.want)) {
					t.Fatalf("admitted records=%d next=%d bytes=%d equal=%t", len(r.records), r.next, r.bytes, bytes.Equal(got, test.want))
				}
				if s.count != test.count || s.bytes != int64(len(test.input)) || s.dirty != test.dirty {
					t.Fatalf("segment=%+v, want count=%d physicalBytes=%d dirty=%t", s, test.count, len(test.input), test.dirty)
				}
				if r.health.CorruptRecords != test.corrupt || r.health.Dropped != 0 || r.health.Pressure || !r.health.Writable {
					t.Fatalf("reader health=%+v, want corrupt=%d", r.health, test.corrupt)
				}
				r.reportCorruption()
				r.updateUsage()
				if r.health.CorruptRecords != test.corrupt || r.health.Pressure != (test.corrupt != 0) || r.health.Events != test.count || r.health.Bytes != int64(len(test.want)) {
					t.Fatalf("reported health=%+v", r.health)
				}
			})
		}
	}
}

type segmentErrorReader struct {
	data []byte
	err  error
}

func (r *segmentErrorReader) Read(buffer []byte) (int, error) {
	n := copy(buffer, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

func TestSegmentReaderReadErrors(t *testing.T) {
	c := rotationConfig(t)
	first := rotationFixture(t, 1, c.Now(), 0)
	second := rotationFixture(t, 2, c.Now(), 0)
	oversized := rotationFixture(t, 1, c.Now(), 258)
	for _, test := range []struct {
		name  string
		input []byte
		want  []byte
		count int
	}{
		{"before-any-bytes", nil, nil, 0},
		{"before-delimiter", first[:len(first)-1], nil, 0},
		{"complete-then-partial", append(append([]byte(nil), first...), second[:len(second)/2]...), first, 1},
		{"oversized-with-read-error", oversized[:len(oversized)-1], nil, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			fault := errors.New("segment reader sentinel")
			reader := bufio.NewReaderSize(&segmentErrorReader{data: test.input, err: fault}, 16)
			r := &Recorder{config: c, health: Health{Writable: true}}
			s := segment{bytes: int64(len(test.input))}
			if err := r.readSegmentRecords(reader, &s, 256); !errors.Is(err, fault) {
				t.Fatalf("reader error identity: %v", err)
			}
			var got []byte
			for _, record := range r.records {
				got = append(got, record.data...)
			}
			if !bytes.Equal(got, test.want) || len(r.records) != test.count || r.next != uint64(test.count) || r.bytes != int64(len(test.want)) {
				t.Fatalf("admission after read error: records=%d next=%d bytes=%d equal=%t", len(r.records), r.next, r.bytes, bytes.Equal(got, test.want))
			}
			// Read errors precede committing corruption or dirty state for the
			// current physical line, including an oversized unterminated line.
			if s.count != test.count || s.bytes != int64(len(test.input)) || s.dirty || r.health.CorruptRecords != 0 || r.health.Dropped != 0 || r.health.Pressure || !r.health.Writable {
				t.Fatalf("state after read error: segment=%+v health=%+v", s, r.health)
			}
		})
	}
}
