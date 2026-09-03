//go:build linux

package documentstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"pgregory.net/rapid"
)

func openTestStore(t *testing.T, root string) *Store {
	t.Helper()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		done := make(chan error, 1)
		go func() { done <- store.Close() }()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, ErrClosed) {
				t.Errorf("close store: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("close store timed out")
		}
	})
	return store
}

func expectedRevision(data []byte) Revision {
	return Revision(sha256.Sum256(data))
}

func TestCheckedCreateReadReplaceAndConflict(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	ctx := context.Background()

	first := []byte("theme = 'night'\n")
	firstRevision, err := store.Write(ctx, "plugin.toml", first, Revision{})
	if err != nil {
		t.Fatal(err)
	}
	if firstRevision != expectedRevision(first) {
		t.Fatalf("first revision = %v", firstRevision)
	}
	document, err := store.Read(ctx, "plugin.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(document.Bytes, first) || document.Revision != firstRevision {
		t.Fatalf("first document mismatch: bytes_equal=%t length=%d revision=%v", bytes.Equal(document.Bytes, first), len(document.Bytes), document.Revision)
	}

	second := []byte("theme = 'light'\n")
	secondRevision, err := store.Write(ctx, "plugin.toml", second, firstRevision)
	if err != nil {
		t.Fatal(err)
	}
	if secondRevision != expectedRevision(second) {
		t.Fatalf("second revision = %v", secondRevision)
	}
	for name, expected := range map[string]Revision{
		"stale revision": firstRevision,
		"checked create": {},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := store.Write(ctx, "plugin.toml", []byte("lost update"), expected); !errors.Is(err, ErrConflict) || got != (Revision{}) {
				t.Fatalf("write = (%v, %v)", got, err)
			}
		})
	}
	document, err = store.Read(ctx, "plugin.toml")
	if err != nil || !bytes.Equal(document.Bytes, second) || document.Revision != secondRevision {
		t.Fatalf("document after conflicts: err=%v bytes_equal=%t length=%d revision=%v", err, bytes.Equal(document.Bytes, second), len(document.Bytes), document.Revision)
	}
	if _, err := store.Write(ctx, "missing.toml", nil, firstRevision); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing checked replace error = %v", err)
	}
}

func TestPathConfinementAndRegularFiles(t *testing.T) {
	outer := t.TempDir()
	root := filepath.Join(outer, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, root)
	ctx := context.Background()

	if _, err := store.Write(ctx, "nested/config.toml", []byte("ok"), Revision{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", ".", "../escape", "nested/../escape", "nested//config", "/absolute", "\x00leading", "nested/\x00inside", internalPrefix + "lock-user"} {
		if _, err := store.Read(ctx, name); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Read(%q) error = %v", name, err)
		}
		if _, err := store.Write(ctx, name, nil, Revision{}); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Write(%q) error = %v", name, err)
		}
	}
	if err := os.Symlink(outer, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Write(ctx, "linked/escape", []byte("no"), Revision{}); err == nil {
		t.Fatal("write through symlinked parent succeeded")
	}
	if _, err := os.Stat(filepath.Join(outer, "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escaped target stat error = %v", err)
	}

	outside := filepath.Join(outer, "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "symlink.toml")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(ctx, "symlink.toml"); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("symlink read error = %v", err)
	}
	if _, err := store.Write(ctx, "symlink.toml", nil, Revision{}); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("symlink write error = %v", err)
	}
	fifo := filepath.Join(root, "pipe")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(ctx, "pipe"); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("FIFO read error = %v", err)
	}
}

func TestRootSymlinkRejected(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if store, err := Open(link); err == nil || store != nil {
		t.Fatalf("Open(symlink) = (%v, %v)", store, err)
	}
}

func TestModesAndOwnership(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	ctx := context.Background()

	if _, err := store.Write(ctx, "new.toml", []byte("new"), Revision{}); err != nil {
		t.Fatal(err)
	}
	newInfo, err := os.Stat(filepath.Join(root, "new.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := newInfo.Mode().Perm(); got != ownerOnly {
		t.Fatalf("new mode = %04o", got)
	}

	path := filepath.Join(root, "existing.toml")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o6750); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.Read(ctx, "existing.toml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Write(ctx, "existing.toml", []byte("replacement"), old.Revision); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeStat, afterStat := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if afterStat.Mode&0o7777 != beforeStat.Mode&0o7777 || afterStat.Uid != beforeStat.Uid || afterStat.Gid != beforeStat.Gid {
		t.Fatalf("metadata before=(%04o,%d,%d) after=(%04o,%d,%d)", beforeStat.Mode&0o7777, beforeStat.Uid, beforeStat.Gid, afterStat.Mode&0o7777, afterStat.Uid, afterStat.Gid)
	}
}

func TestNewModeIgnoresRestrictiveUmask(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	previous := unix.Umask(0o777)
	defer unix.Umask(previous)
	if _, err := store.Write(context.Background(), "config.toml", []byte("private"), Revision{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != ownerOnly {
		t.Fatalf("new mode = %04o", info.Mode().Perm())
	}
	if document, err := store.Read(context.Background(), "config.toml"); err != nil || string(document.Bytes) != "private" {
		t.Fatalf("read after restrictive umask: err=%v bytes_match=%t length=%d", err, string(document.Bytes) == "private", len(document.Bytes))
	}
}

func TestMaximumBasenameWorks(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	name := strings.Repeat("a", 255)
	data := []byte("maximum basename")
	got, err := store.Write(context.Background(), name, data, Revision{})
	if err != nil {
		t.Fatal(err)
	}
	document, err := store.Read(context.Background(), name)
	if err != nil || !bytes.Equal(document.Bytes, data) || document.Revision != got {
		t.Fatalf("long-name document mismatch: err=%v bytes_equal=%t length=%d revision=%v", err, bytes.Equal(document.Bytes, data), len(document.Bytes), document.Revision)
	}
}

func TestConcurrentFirstLockPublicationUnderRestrictiveUmask(t *testing.T) {
	root := t.TempDir()
	firstStore := openTestStore(t, root)
	secondStore := openTestStore(t, root)
	previous := unix.Umask(0o777)
	defer unix.Umask(previous)

	prepared := make(chan struct{})
	release := make(chan struct{})
	releasePublisher := sync.OnceFunc(func() { close(release) })
	defer releasePublisher()
	firstStore.hooks.publishLock = func(oldDir int, old string, newDir int, new string, flags uint) error {
		close(prepared)
		<-release
		return unix.Renameat2(oldDir, old, newDir, new, flags)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := firstStore.Write(context.Background(), "config.toml", []byte("first"), Revision{})
		firstDone <- err
	}()
	select {
	case <-prepared:
	case <-time.After(time.Second):
		t.Fatal("first participant did not prepare its lock")
	}
	secondContext, cancelSecond := context.WithTimeout(context.Background(), time.Second)
	defer cancelSecond()
	secondDone := make(chan error, 1)
	go func() {
		_, err := secondStore.Write(secondContext, "config.toml", []byte("second"), Revision{})
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second participant could not publish and use a ready lock: %v", err)
		}
	case <-secondContext.Done():
		t.Fatalf("second participant did not finish: %v", secondContext.Err())
	}
	releasePublisher()
	select {
	case err := <-firstDone:
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("first participant error = %v, want conflict", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first participant did not join the published lock")
	}
	info, err := os.Stat(filepath.Join(root, lockName("config.toml")))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != ownerOnly {
		t.Fatalf("published lock mode = %04o", mode)
	}
	assertNoTemporaryFiles(t, root)
}

func TestExistingHardLinkedLockMetadataIsNotChanged(t *testing.T) {
	outer := t.TempDir()
	root := filepath.Join(outer, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(outer, "outside")
	if err := os.WriteFile(outside, []byte("shared inode"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(root, lockName("config.toml"))); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, root)
	if _, err := store.Write(context.Background(), "config.toml", []byte("document"), Revision{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("outside hard-link mode changed to %04o", info.Mode().Perm())
	}
}

func TestHardLinkedDocumentReplacementBreaksLink(t *testing.T) {
	outer := t.TempDir()
	root := filepath.Join(outer, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(outer, "outside")
	if err := os.WriteFile(outside, []byte("shared bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "config.toml")
	if err := os.Link(outside, inside); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, root)
	document, err := store.Read(context.Background(), "config.toml")
	if err != nil || string(document.Bytes) != "shared bytes" {
		t.Fatalf("hard-linked read identity/err = (%t, %v)", string(document.Bytes) == "shared bytes", err)
	}
	if _, err := store.Write(context.Background(), "config.toml", []byte("replacement"), document.Revision); err != nil {
		t.Fatal(err)
	}
	outsideBytes, outsideErr := os.ReadFile(outside)
	insideBytes, insideErr := os.ReadFile(inside)
	if outsideErr != nil || insideErr != nil || string(outsideBytes) != "shared bytes" || string(insideBytes) != "replacement" {
		t.Fatalf("hard-link replacement identities/errors = (outside:%t/%v, inside:%t/%v)", string(outsideBytes) == "shared bytes", outsideErr, string(insideBytes) == "replacement", insideErr)
	}
}

func TestSupplementaryGroupOwnershipIsPreserved(t *testing.T) {
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	wantedGID := -1
	for _, group := range groups {
		if group != os.Getegid() {
			wantedGID = group
			break
		}
	}
	if wantedGID < 0 {
		t.Skip("no supplementary group available for a distinct ownership fixture")
	}
	root := t.TempDir()
	target := filepath.Join(root, "config.toml")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(target, os.Geteuid(), wantedGID); err != nil {
		t.Skipf("cannot assign permitted supplementary group %d: %v", wantedGID, err)
	}
	store := openTestStore(t, root)
	current, err := store.Read(context.Background(), "config.toml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Write(context.Background(), "config.toml", []byte("new"), current.Revision); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := int(info.Sys().(*syscall.Stat_t).Gid); got != wantedGID {
		t.Fatalf("replacement gid = %d, want %d", got, wantedGID)
	}
}

func TestDurabilitySequenceAndLockHeldThroughRename(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	var steps []string
	store.hooks.syncFile = func(fd int) error {
		steps = append(steps, "file-fsync")
		return unix.Fsync(fd)
	}
	store.hooks.rename = func(oldDir int, old string, newDir int, new string) error {
		steps = append(steps, "rename")
		if err := unix.Renameat(oldDir, old, newDir, new); err != nil {
			return err
		}
		lock, err := unix.Openat(newDir, lockName("config.toml"), unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer unix.Close(lock)
		if err := unix.Flock(lock, unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("cooperative lock was not held through rename: %v", err)
		}
		return nil
	}
	store.hooks.syncDir = func(fd int) error {
		steps = append(steps, "directory-fsync")
		return unix.Fsync(fd)
	}
	data := []byte("durable")
	got, err := store.Write(context.Background(), "config.toml", data, Revision{})
	if err != nil {
		t.Fatal(err)
	}
	if got != expectedRevision(data) || !reflect.DeepEqual(steps, []string{"file-fsync", "rename", "directory-fsync"}) {
		t.Fatalf("revision/steps = (%v, %v)", got, steps)
	}
	assertNoTemporaryFiles(t, root)
}

func TestPrePublicationFailuresAndCancellationCleanUp(t *testing.T) {
	wantErr := errors.New("injected failure")
	for _, test := range []struct {
		name string
		set  func(*Store)
	}{
		{"lock publication", func(store *Store) {
			store.hooks.publishLock = func(int, string, int, string, uint) error { return wantErr }
		}},
		{"document write", func(store *Store) {
			store.hooks.write = func(int, []byte) (int, error) { return 0, wantErr }
		}},
		{"file fsync", func(store *Store) { store.hooks.syncFile = func(int) error { return wantErr } }},
		{"rename", func(store *Store) { store.hooks.rename = func(int, string, int, string) error { return wantErr } }},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store := openTestStore(t, root)
			test.set(store)
			if got, err := store.Write(context.Background(), "config.toml", []byte("new"), Revision{}); !errors.Is(err, wantErr) || got != (Revision{}) {
				t.Fatalf("Write = (%v, %v)", got, err)
			} else if strings.Contains(err.Error(), "remove temporary document") {
				t.Fatalf("successful cleanup reported as an error: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "config.toml")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("target stat error = %v", err)
			}
			assertNoTemporaryFiles(t, root)
		})
	}

	t.Run("canceled after file fsync", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		store := openTestStore(t, root)
		old, err := store.Read(context.Background(), "config.toml")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		store.hooks.syncFile = func(fd int) error {
			err := unix.Fsync(fd)
			cancel()
			return err
		}
		if got, err := store.Write(ctx, "config.toml", []byte("new"), old.Revision); !errors.Is(err, context.Canceled) || got != (Revision{}) {
			t.Fatalf("Write = (%v, %v)", got, err)
		}
		data, err := os.ReadFile(filepath.Join(root, "config.toml"))
		if err != nil || string(data) != "old" {
			t.Fatalf("canceled target: err=%v bytes_match=%t length=%d", err, string(data) == "old", len(data))
		}
		assertNoTemporaryFiles(t, root)
	})
}

func TestCleanupFailureIsReported(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	writeErr := errors.New("write protocol failed")
	cleanupErr := errors.New("cleanup failed")
	store.hooks.syncFile = func(int) error { return writeErr }
	store.hooks.unlink = func(int, string) error { return cleanupErr }
	if got, err := store.Write(context.Background(), "config.toml", []byte("secret"), Revision{}); got != (Revision{}) || !errors.Is(err, writeErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("Write = (%v, %v)", got, err)
	}
}

func TestLockCollisionCleanupFailureIsReported(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	cleanupErr := errors.New("lock candidate cleanup failed")
	store.hooks.publishLock = func(int, string, int, string, uint) error { return unix.EEXIST }
	store.hooks.unlink = func(int, string) error { return cleanupErr }
	if got, err := store.Write(context.Background(), "config.toml", []byte("content"), Revision{}); got != (Revision{}) || !errors.Is(err, cleanupErr) {
		t.Fatalf("Write = (%v, %v)", got, err)
	}
}

func TestDirectorySyncFailureReportsCommittedRevision(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	wantErr := errors.New("directory sync failed")
	store.hooks.syncDir = func(int) error { return wantErr }
	data := []byte("visible but durability uncertain")
	got, err := store.Write(context.Background(), "config.toml", data, Revision{})
	if !errors.Is(err, wantErr) || got != expectedRevision(data) {
		t.Fatalf("Write = (%v, %v)", got, err)
	}
	written, readErr := os.ReadFile(filepath.Join(root, "config.toml"))
	if readErr != nil || !bytes.Equal(written, data) {
		t.Fatalf("published target: err=%v bytes_equal=%t length=%d", readErr, bytes.Equal(written, data), len(written))
	}
	assertNoTemporaryFiles(t, root)
}

func TestCooperativeContentionWaitsThenConflicts(t *testing.T) {
	root := t.TempDir()
	firstStore := openTestStore(t, root)
	secondStore := openTestStore(t, root)
	initialRevision, err := firstStore.Write(context.Background(), "config.toml", []byte("initial"), Revision{})
	if err != nil {
		t.Fatal(err)
	}

	reachedRename := make(chan struct{})
	releaseRename := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseRename) })
	defer release()
	firstStore.hooks.rename = func(oldDir int, old string, newDir int, new string) error {
		close(reachedRename)
		<-releaseRename
		return unix.Renameat(oldDir, old, newDir, new)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := firstStore.Write(context.Background(), "config.toml", []byte("first"), initialRevision)
		firstDone <- err
	}()
	select {
	case <-reachedRename:
	case <-time.After(time.Second):
		t.Fatal("first writer did not reach rename")
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := secondStore.Write(context.Background(), "config.toml", []byte("second"), initialRevision)
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("contending write returned before lock release: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	release()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("first writer did not finish")
	}
	select {
	case err := <-secondDone:
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("contending write error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second writer did not finish")
	}
	document, err := firstStore.Read(context.Background(), "config.toml")
	if err != nil || string(document.Bytes) != "first" {
		t.Fatalf("final document: err=%v bytes_match=%t length=%d", err, string(document.Bytes) == "first", len(document.Bytes))
	}
}

func TestCancellationWhileWaitingForLock(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	lock, err := unix.Open(filepath.Join(root, lockName("config.toml")), unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, ownerOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(lock)
	if err := unix.Flock(lock, unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if got, err := store.Write(ctx, "config.toml", []byte("blocked"), Revision{}); !errors.Is(err, context.DeadlineExceeded) || got != (Revision{}) {
		t.Fatalf("Write = (%v, %v)", got, err)
	}
	if time.Since(started) > 300*time.Millisecond {
		t.Fatalf("cancellation took %v", time.Since(started))
	}
	assertNoTemporaryFiles(t, root)
}

func TestReadWaitsForCooperativeWriterLock(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	if _, err := store.Write(context.Background(), "config.toml", []byte("content"), Revision{}); err != nil {
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
	if _, err := store.Read(ctx, "config.toml"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contending Read error = %v", err)
	}
	if err := unix.Flock(lock, unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	document, err := store.Read(context.Background(), "config.toml")
	if err != nil || string(document.Bytes) != "content" {
		t.Fatalf("Read after unlock: err=%v bytes_match=%t length=%d", err, string(document.Bytes) == "content", len(document.Bytes))
	}
}

func TestCloseAndAlreadyCanceledContexts(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Read(ctx, "config.toml"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Read error = %v", err)
	}
	if _, err := store.Write(ctx, "config.toml", nil, Revision{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Write error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(context.Background(), "config.toml"); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Read error = %v", err)
	}
	if _, err := store.Write(context.Background(), "config.toml", nil, Revision{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Write error = %v", err)
	}
	if err := store.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close error = %v", err)
	}
}

func TestCloseWaitsForActiveWriteAndThenExcludesNewWork(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	reachedRename := make(chan struct{})
	releaseRename := make(chan struct{})
	store.hooks.rename = func(oldDir int, old string, newDir int, new string) error {
		close(reachedRename)
		<-releaseRename
		return unix.Renameat(oldDir, old, newDir, new)
	}
	writeDone := make(chan error, 1)
	go func() {
		_, err := store.Write(context.Background(), "config.toml", []byte("published"), Revision{})
		writeDone <- err
	}()
	select {
	case <-reachedRename:
	case <-time.After(time.Second):
		t.Fatal("active write did not reach rename")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- store.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned while a write held the root: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseRename)
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("active write did not finish")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after the active write")
	}
	if data, err := os.ReadFile(filepath.Join(root, "config.toml")); err != nil || string(data) != "published" {
		t.Fatalf("published document: err=%v bytes_match=%t length=%d", err, string(data) == "published", len(data))
	}
	if _, err := store.Write(context.Background(), "later.toml", nil, Revision{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("post-close Write error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "later.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("post-close target stat error = %v", err)
	}
}

func TestCheckedWriteModel(t *testing.T) {
	root := t.TempDir()
	var sequence int
	rapid.Check(t, func(rt *rapid.T) {
		sequence++
		directory := filepath.Join(root, fmt.Sprintf("case-%d", sequence))
		if err := os.Mkdir(directory, 0o700); err != nil {
			rt.Fatal(err)
		}
		store, err := Open(directory)
		if err != nil {
			rt.Fatal(err)
		}
		defer store.Close()

		var model []byte
		var modelRevision Revision
		exists := false
		operations := rapid.IntRange(1, 30).Draw(rt, "operations")
		for index := range operations {
			dataSeed := rapid.Uint64().Draw(rt, fmt.Sprintf("data-seed-%d", index))
			dataLength := rapid.IntRange(0, 256).Draw(rt, fmt.Sprintf("data-length-%d", index))
			data := generatedBytes(dataSeed, dataLength)
			stale := rapid.Bool().Draw(rt, fmt.Sprintf("stale-%d", index))
			expected := modelRevision
			if stale {
				if exists {
					expected[0] ^= 0xff
				} else {
					expected[0] = 1
				}
			}
			got, err := store.Write(context.Background(), "model.toml", data, expected)
			if stale {
				if !errors.Is(err, ErrConflict) || got != (Revision{}) {
					rt.Fatalf("operation %d conflict result = (%v, %v)", index, got, err)
				}
			} else {
				if err != nil || got != expectedRevision(data) {
					rt.Fatalf("operation %d write result = (%v, %v)", index, got, err)
				}
				model, modelRevision, exists = append([]byte(nil), data...), got, true
			}
			if exists {
				document, err := store.Read(context.Background(), "model.toml")
				if err != nil || !bytes.Equal(document.Bytes, model) || document.Revision != modelRevision {
					rt.Fatalf("operation %d document mismatch: err=%v bytes_equal=%t got_length=%d want_length=%d got_revision=%v want_revision=%v", index, err, bytes.Equal(document.Bytes, model), len(document.Bytes), len(model), document.Revision, modelRevision)
				}
			}
		}
	})
}

func generatedBytes(seed uint64, length int) []byte {
	data := make([]byte, length)
	state := seed
	for index := range data {
		state = state*6364136223846793005 + 1
		data[index] = byte(state >> 56)
	}
	return data
}

func TestConcurrentReadsAreCoherent(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	first := bytes.Repeat([]byte("a"), 32*1024)
	second := bytes.Repeat([]byte("b"), 32*1024)
	current, err := store.Write(context.Background(), "config.toml", first, Revision{})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			for range 30 {
				document, err := store.Read(context.Background(), "config.toml")
				if err != nil {
					t.Errorf("read: %v", err)
					return
				}
				matchesFirst := bytes.Equal(document.Bytes, first)
				matchesSecond := bytes.Equal(document.Bytes, second)
				if (!matchesFirst && !matchesSecond) || document.Revision != expectedRevision(document.Bytes) {
					t.Errorf("incoherent read: length=%d first=%t second=%t revision=%v", len(document.Bytes), matchesFirst, matchesSecond, document.Revision)
					return
				}
			}
		}()
	}
	for range 20 {
		next := second
		if current == expectedRevision(second) {
			next = first
		}
		current, err = store.Write(context.Background(), "config.toml", next, current)
		if err != nil {
			t.Fatal(err)
		}
	}
	group.Wait()
}

func assertNoTemporaryFiles(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), internalPrefix+"tmp-") {
			t.Errorf("temporary file remains: %s", entry.Name())
		}
	}
}
