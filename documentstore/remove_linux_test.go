//go:build linux

package documentstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRemoveBoundedRevisionAndLimit(t *testing.T) {
	for _, scenario := range []string{"exact", "empty", "zero", "stale", "missing", "oversize", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			store := openTestStore(t, root)
			data, expected, limit := []byte("document"), Revision{}, 8
			if scenario == "empty" {
				data = nil
			}
			if scenario != "missing" {
				var err error
				expected, err = store.Write(context.Background(), "config.toml", data, Revision{})
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			want, wantRemoved := error(nil), scenario == "exact" || scenario == "empty"
			switch scenario {
			case "zero":
				expected, want = Revision{}, ErrConflict
			case "stale":
				expected, want = Revision{1}, ErrConflict
			case "missing":
				expected, want = Revision{1}, ErrConflict
			case "oversize":
				limit, want = 7, ErrTooLarge
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			}
			removed, err := store.RemoveBounded(ctx, "config.toml", expected, limit)
			if removed != wantRemoved || !errors.Is(err, want) {
				t.Fatalf("removed=%t err=%v want=%t/%v", removed, err, wantRemoved, want)
			}
			actual, readErr := os.ReadFile(filepath.Join(root, "config.toml"))
			if wantRemoved || scenario == "missing" {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("document still exists: %v", readErr)
				}
			} else if readErr != nil || string(actual) != string(data) {
				t.Fatal("refused removal changed the document", readErr)
			}
		})
	}
}

func TestRemoveBoundedPublicationFailures(t *testing.T) {
	for _, afterUnlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "unlink", true: "directory fsync"}[afterUnlink], func(t *testing.T) {
			root := t.TempDir()
			store := openTestStore(t, root)
			current, err := store.Write(context.Background(), "config.toml", []byte("old"), Revision{})
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected failure")
			if afterUnlink {
				store.hooks.syncDir = func(int) error { return injected }
			} else {
				store.hooks.unlink = func(int, string) error { return injected }
			}
			removed, err := store.RemoveBounded(context.Background(), "config.toml", current, 3)
			if removed != afterUnlink || !errors.Is(err, injected) {
				t.Fatalf("removed=%t err=%v", removed, err)
			}
			data, readErr := os.ReadFile(filepath.Join(root, "config.toml"))
			if afterUnlink {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("published absence not reported: %v", readErr)
				}
			} else if readErr != nil || string(data) != "old" {
				t.Fatal("failed unlink changed document", readErr)
			}
		})
	}
}

func TestRemoveBoundedPreservesHardLinkAndRejectsUnsafePaths(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	current, err := store.Write(context.Background(), "config.toml", []byte("keep"), Revision{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "config.toml"), filepath.Join(root, "other.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other.toml", filepath.Join(root, "linked.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name string
		want error
	}{{"../other.toml", ErrInvalidPath}, {"/other.toml", ErrInvalidPath}, {".documentstore-lock-private", ErrInvalidPath}, {"linked.toml", ErrNotRegular}, {"directory", ErrNotRegular}} {
		if removed, err := store.RemoveBounded(context.Background(), scenario.name, current, 4); removed || !errors.Is(err, scenario.want) {
			t.Fatalf("%s removed=%t err=%v", scenario.name, removed, err)
		}
	}
	if removed, err := store.RemoveBounded(context.Background(), "config.toml", current, 4); !removed || err != nil {
		t.Fatalf("remove linked path: %t %v", removed, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "other.toml"))
	if err != nil || string(data) != "keep" {
		t.Fatal("other hard link changed", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if removed, err := store.RemoveBounded(context.Background(), "other.toml", current, 4); removed || !errors.Is(err, ErrClosed) {
		t.Fatalf("closed: %t %v", removed, err)
	}
}

func TestRemoveBoundedCooperatesWithWriter(t *testing.T) {
	root := t.TempDir()
	first, second := openTestStore(t, root), openTestStore(t, root)
	current, err := first.Write(context.Background(), "config.toml", []byte("old"), Revision{})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	group.Add(2)
	results := make(chan error, 2)
	go func() {
		defer group.Done()
		_, err := first.RemoveBounded(context.Background(), "config.toml", current, 3)
		results <- err
	}()
	go func() {
		defer group.Done()
		_, err := second.WriteBounded(context.Background(), "config.toml", []byte("new"), current, 3)
		results <- err
	}()
	group.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestRemoveBoundedCancellationWhileWaitingForLock(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	current, err := store.Write(context.Background(), "config.toml", []byte("old"), Revision{})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := unix.Open(filepath.Join(root, lockName("config.toml")), unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(lock)
	if err := unix.Flock(lock, unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if removed, err := store.RemoveBounded(ctx, "config.toml", current, 3); removed || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation removed=%t err=%v", removed, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "config.toml"))
	if err != nil || string(data) != "old" {
		t.Fatal("canceled deletion changed document", err)
	}
}
