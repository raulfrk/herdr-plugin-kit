//go:build linux

package servicehost

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

type socketLock struct {
	file *os.File
	once sync.Once
	err  error
}

func acquireSocketLock(path string, uid uint32) (*socketLock, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("servicehost: open socket lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	fail := func(cause error) (*socketLock, error) {
		return nil, errors.Join(cause, file.Close())
	}
	info, err := file.Stat()
	if err != nil {
		return fail(fmt.Errorf("servicehost: inspect socket lock: %w", err))
	}
	if !ownedRegularFile(info, uid) {
		return fail(fmt.Errorf("%w: invalid socket lock", ErrSocketOwnership))
	}
	if err := file.Chmod(0o600); err != nil {
		return fail(fmt.Errorf("servicehost: protect socket lock: %w", err))
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return fail(ErrSocketActive)
		}
		return fail(fmt.Errorf("servicehost: lock socket: %w", err))
	}
	return &socketLock{file: file}, nil
}

func (l *socketLock) Close() error {
	l.once.Do(func() {
		l.err = errors.Join(unix.Flock(int(l.file.Fd()), unix.LOCK_UN), l.file.Close())
	})
	return l.err
}
