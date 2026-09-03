//go:build linux

package documentstore

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const ownerOnly = 0o600
const internalPrefix = ".documentstore-"

type systemHooks struct {
	syncFile    func(int) error
	write       func(int, []byte) (int, error)
	rename      func(int, string, int, string) error
	publishLock func(int, string, int, string, uint) error
	syncDir     func(int) error
	unlink      func(int, string) error
}

func defaultHooks() systemHooks {
	return systemHooks{
		syncFile:    unix.Fsync,
		write:       unix.Write,
		rename:      unix.Renameat,
		publishLock: unix.Renameat2,
		syncDir:     unix.Fsync,
		unlink:      func(directory int, name string) error { return unix.Unlinkat(directory, name, 0) },
	}
}

// Store confines document operations beneath one open directory. Close must
// be called when the store is no longer needed.
type Store struct {
	mu     sync.RWMutex
	rootFD int
	closed bool
	hooks  systemHooks
}

// Open opens an existing real directory as a document-store root. The root
// itself must not be a symlink.
func Open(root string) (*Store, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open document store root: %w", err)
	}
	return &Store{rootFD: fd, hooks: defaultHooks()}, nil
}

// Close releases the root directory. It waits for active reads and writes.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.closed = true
	if err := unix.Close(s.rootFD); err != nil {
		return fmt.Errorf("close document store: %w", err)
	}
	return nil
}

// Read returns a coherent snapshot and its content revision.
func (s *Store) Read(ctx context.Context, name string) (Document, error) {
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	parent, base, release, err := s.openParent(name)
	if err != nil {
		return Document{}, err
	}
	defer release()

	lock, err := openLock(parent, base, s.hooks)
	if err != nil {
		return Document{}, err
	}
	defer unix.Close(lock)
	if err := flockContext(ctx, lock, unix.LOCK_SH); err != nil {
		return Document{}, err
	}

	data, _, err := readRegular(ctx, parent, base)
	if err != nil {
		return Document{}, err
	}
	return Document{Bytes: data, Revision: revision(data)}, nil
}

// Write atomically replaces name when its current revision equals expected.
// A zero expected revision requires name not to exist. The returned revision
// is non-zero after rename, including when the following directory fsync fails.
func (s *Store) Write(ctx context.Context, name string, data []byte, expected Revision) (result Revision, resultErr error) {
	if err := ctx.Err(); err != nil {
		return Revision{}, err
	}
	parent, base, release, err := s.openParent(name)
	if err != nil {
		return Revision{}, err
	}
	defer release()

	lock, err := openLock(parent, base, s.hooks)
	if err != nil {
		return Revision{}, err
	}
	defer unix.Close(lock)
	if err := flockContext(ctx, lock, unix.LOCK_EX); err != nil {
		return Revision{}, err
	}

	current, metadata, err := readRegular(ctx, parent, base)
	if errors.Is(err, os.ErrNotExist) {
		if expected != (Revision{}) {
			return Revision{}, ErrConflict
		}
	} else if err != nil {
		return Revision{}, err
	} else if expected == (Revision{}) || revision(current) != expected {
		return Revision{}, ErrConflict
	}

	temporary, tempFD, err := createTemp(parent)
	if err != nil {
		return Revision{}, err
	}
	renamed := false
	defer func() {
		if err := unix.Close(tempFD); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close temporary document: %w", err))
		}
		if !renamed {
			if err := s.hooks.unlink(parent, temporary); err != nil && !errors.Is(err, unix.ENOENT) {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove temporary document: %w", err))
			}
		}
	}()

	if err := writeAll(tempFD, data, s.hooks.write); err != nil {
		return Revision{}, fmt.Errorf("write temporary document: %w", err)
	}
	if metadata != nil {
		if err := unix.Fchown(tempFD, int(metadata.Uid), int(metadata.Gid)); err != nil {
			return Revision{}, fmt.Errorf("preserve document ownership: %w", err)
		}
		if err := unix.Fchmod(tempFD, metadata.Mode&0o7777); err != nil {
			return Revision{}, fmt.Errorf("preserve document mode: %w", err)
		}
	} else if err := unix.Fchmod(tempFD, ownerOnly); err != nil {
		return Revision{}, fmt.Errorf("set owner-only document mode: %w", err)
	}
	if err := s.hooks.syncFile(tempFD); err != nil {
		return Revision{}, fmt.Errorf("sync temporary document: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Revision{}, err
	}
	if err := s.hooks.rename(parent, temporary, parent, base); err != nil {
		return Revision{}, fmt.Errorf("replace document: %w", err)
	}
	renamed = true
	next := revision(data)
	if err := s.hooks.syncDir(parent); err != nil {
		return next, fmt.Errorf("sync document directory: %w", err)
	}
	return next, nil
}

func (s *Store) openParent(name string) (int, string, func(), error) {
	parentName, base, err := splitName(name)
	if err != nil {
		return 0, "", nil, err
	}
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return 0, "", nil, ErrClosed
	}
	how := &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	}
	fd, err := unix.Openat2(s.rootFD, parentName, how)
	if err != nil {
		s.mu.RUnlock()
		return 0, "", nil, pathError("open document parent", name, err)
	}
	return fd, base, func() { unix.Close(fd); s.mu.RUnlock() }, nil
}

func splitName(name string) (string, string, error) {
	if strings.IndexByte(name, 0) >= 0 || path.IsAbs(name) || path.Clean(name) != name || name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return "", "", ErrInvalidPath
	}
	parent, base := path.Dir(name), path.Base(name)
	if strings.HasPrefix(base, internalPrefix) {
		return "", "", ErrInvalidPath
	}
	return parent, base, nil
}

func openLock(parent int, base string, hooks systemHooks) (int, error) {
	name := lockName(base)
	fd, err := openExistingLock(parent, name, base)
	if err == nil {
		return fd, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	temporary, candidate, err := createPrivateTemp(parent, unix.O_RDWR, "lock")
	if err != nil {
		return 0, fmt.Errorf("prepare document lock: %w", err)
	}
	if err := unix.Fchmod(candidate, ownerOnly); err != nil {
		return 0, errors.Join(fmt.Errorf("set owner-only document lock mode: %w", err), discardCandidate(parent, temporary, candidate, hooks.unlink))
	}
	if err := hooks.publishLock(parent, temporary, parent, name, unix.RENAME_NOREPLACE); err == nil {
		return candidate, nil
	} else if !errors.Is(err, unix.EEXIST) {
		return 0, errors.Join(fmt.Errorf("publish document lock: %w", err), discardCandidate(parent, temporary, candidate, hooks.unlink))
	}
	if err := discardCandidate(parent, temporary, candidate, hooks.unlink); err != nil {
		return 0, err
	}
	return openExistingLock(parent, name, base)
}

func openExistingLock(parent int, name, base string) (int, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return 0, fmt.Errorf("open document lock: %w", ErrNotRegular)
		}
		return 0, pathError("open document lock", base, err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return 0, fmt.Errorf("stat document lock: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return 0, fmt.Errorf("document lock: %w", ErrNotRegular)
	}
	return fd, nil
}

func discardCandidate(parent int, name string, fd int, unlink func(int, string) error) error {
	var result error
	if err := unix.Close(fd); err != nil {
		result = errors.Join(result, fmt.Errorf("close unpublished document lock: %w", err))
	}
	if err := unlink(parent, name); err != nil && !errors.Is(err, unix.ENOENT) {
		result = errors.Join(result, fmt.Errorf("remove unpublished document lock: %w", err))
	}
	return result
}

func lockName(base string) string {
	return internalPrefix + "lock-" + revision([]byte(base)).String()
}

func flockContext(ctx context.Context, fd, operation int) error {
	for {
		err := unix.Flock(fd, operation|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("lock document: %w", err)
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func readRegular(ctx context.Context, parent int, base string) ([]byte, *unix.Stat_t, error) {
	fd, err := unix.Openat(parent, base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, nil, fmt.Errorf("read document: %w", ErrNotRegular)
		}
		return nil, nil, pathError("read document", base, err)
	}
	file := os.NewFile(uintptr(fd), base)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, nil, fmt.Errorf("stat document: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, nil, fmt.Errorf("read document: %w", ErrNotRegular)
	}
	data, err := readAllContext(ctx, file)
	if err != nil {
		return nil, nil, fmt.Errorf("read document: %w", err)
	}
	return data, &stat, nil
}

func readAllContext(ctx context.Context, file *os.File) ([]byte, error) {
	const bufferSize = 32768
	data := make([]byte, 0, bufferSize)
	buffer := make([]byte, bufferSize)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := file.Read(buffer)
		data = append(data, buffer[:n]...)
		if errors.Is(err, io.EOF) {
			return data, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func createTemp(parent int) (string, int, error) {
	return createPrivateTemp(parent, unix.O_WRONLY, "document")
}

func createPrivateTemp(parent, access int, kind string) (string, int, error) {
	for range 100 {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", 0, fmt.Errorf("name temporary document: %w", err)
		}
		name := fmt.Sprintf("%stmp-%s-%x", internalPrefix, kind, random[:])
		fd, err := unix.Openat(parent, name, access|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, ownerOnly)
		if err == nil {
			return name, fd, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return "", 0, fmt.Errorf("create temporary document: %w", err)
		}
	}
	return "", 0, errors.New("create temporary document: exhausted temporary names")
}

func writeAll(fd int, data []byte, write func(int, []byte) (int, error)) error {
	for len(data) > 0 {
		n, err := write(fd, data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func pathError(operation, name string, err error) error {
	return &os.PathError{Op: operation, Path: name, Err: err}
}
