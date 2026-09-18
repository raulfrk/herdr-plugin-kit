//go:build linux

package agenthost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const codexServiceLifecycleLockName = ".codex-service.lock"

type codexServiceLifecycleLock struct {
	file *os.File
}

func (l *codexServiceLifecycleLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// acquireCodexServiceLifecycleLock serializes mutating operations across Kit
// plugin and CLI processes. The persistent lock lives beside the descriptor,
// never under systemd's removable runtime directory. Existing-only acquisition
// leaves a never-installed configuration untouched.
func acquireCodexServiceLifecycleLock(ctx context.Context, paths codexServicePaths, create bool, uid int, interval time.Duration) (*codexServiceLifecycleLock, error) {
	if ctx == nil {
		panic("nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if create {
		if err := ensureCodexServiceDirectories(paths); err != nil {
			return nil, err
		}
	} else {
		if _, err := os.Lstat(paths.DescriptorPath); errors.Is(err, os.ErrNotExist) {
			return nil, ErrCodexServiceNotInstalled
		} else if err != nil {
			return nil, err
		}
		if err := ensureNoSymlinkDirectory(paths.KitConfigDir, false); err != nil {
			return nil, err
		}
	}
	path := filepath.Join(paths.KitConfigDir, codexServiceLifecycleLockName)
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: lifecycle lock: %v", ErrCodexServiceOwnership, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create lifecycle lock file")
	}
	closeOnError := func(err error) (*codexServiceLifecycleLock, error) {
		_ = file.Close()
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return closeOnError(err)
	}
	if stat.Uid != uint32(uid) || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o777 != 0o600 {
		return closeOnError(fmt.Errorf("%w: lifecycle lock ownership or mode", ErrCodexServiceOwnership))
	}
	interval = codexServiceLifecyclePollInterval(interval)
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &codexServiceLifecycleLock{file: file}, nil
		}
		// Linux defines EWOULDBLOCK as EAGAIN; one comparison is enough here.
		if err != unix.EAGAIN {
			return closeOnError(err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return closeOnError(ctx.Err())
		case <-timer.C:
		}
	}
}

func codexServiceLifecyclePollInterval(interval time.Duration) time.Duration {
	if interval <= 0 {
		return codexServicePollInterval
	}
	return interval
}
