//go:build linux

package servicehost

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

type lifecycleLock struct {
	file *os.File
	once sync.Once
	err  error
}

func acquireLifecycleLock(path string, uid uint32) (*lifecycleLock, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("servicehost: open lifecycle lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	fail := func(cause error) (*lifecycleLock, error) { return nil, errors.Join(cause, file.Close()) }
	info, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	if !ownedRegularFile(info, uid) {
		return fail(fmt.Errorf("%w: invalid lifecycle lock", ErrLifecycleOwnership))
	}
	if err := file.Chmod(0o600); err != nil {
		return fail(err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return fail(ErrLifecycleBusy)
		}
		return fail(err)
	}
	return &lifecycleLock{file: file}, nil
}

func (l *lifecycleLock) Close() error {
	l.once.Do(func() { l.err = errors.Join(unix.Flock(int(l.file.Fd()), unix.LOCK_UN), l.file.Close()) })
	return l.err
}
