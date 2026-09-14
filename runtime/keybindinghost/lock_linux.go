//go:build linux

package keybindinghost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/documentstore"
	"golang.org/x/sys/unix"
)

const transactionLockName = ".herdr-plugin-kit-keybindings.lock"

func acquireTransactionLock(ctx context.Context, configPath string) (io.Closer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, err := unix.Open(filepath.Dir(configPath), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open config directory: %w", err)
	}
	defer unix.Close(directory)

	fd, err := unix.Openat(directory, transactionLockName, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("open transaction lock: %w", documentstore.ErrNotRegular)
		}
		return nil, fmt.Errorf("open transaction lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), transactionLockName)
	closeOnError := func(err error) (io.Closer, error) {
		return nil, errors.Join(err, file.Close())
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return closeOnError(fmt.Errorf("stat transaction lock: %w", err))
	}
	if !validTransactionLock(stat, uint32(os.Geteuid())) {
		return closeOnError(errors.New("transaction lock is not a private owner-controlled regular file"))
	}
	for {
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return file, nil
		} else if !errors.Is(err, unix.EWOULDBLOCK) {
			return closeOnError(fmt.Errorf("lock transaction: %w", err))
		}
		timer := time.NewTimer(time.Millisecond)
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

func validTransactionLock(stat unix.Stat_t, effectiveUID uint32) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Nlink == 1 && stat.Uid == effectiveUID && stat.Mode&0o077 == 0
}
