package servicehost

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

var (
	ErrSocketActive    = errors.New("servicehost: socket is already active")
	ErrSocketOwnership = errors.New("servicehost: socket ownership mismatch")
)

const staleSocketProbeTimeout = 100 * time.Millisecond

type privateListener struct {
	net.Listener
	path      string
	stat      syscall.Stat_t
	lock      *socketLock
	closeOnce sync.Once
	closeErr  error
}

func listenPrivateUnix(path string, uid uint32) (*privateListener, bool, error) {
	if !cleanAbsolute(path) {
		return nil, false, errors.New("servicehost: socket path must be clean and absolute")
	}
	directory := filepath.Dir(path)
	if err := ensurePrivateDirectory(directory, uid); err != nil {
		return nil, false, err
	}
	lock, err := acquireSocketLock(path+".lock", uid)
	if err != nil {
		return nil, false, err
	}
	keepLock := false
	defer func() {
		if !keepLock {
			_ = lock.Close()
		}
	}()
	recovered := false
	info, err := os.Lstat(path)
	if err == nil {
		if err := validateSocketOwner(info, uid); err != nil {
			return nil, false, err
		}
		connection, dialErr := net.DialTimeout("unix", path, staleSocketProbeTimeout)
		if dialErr == nil {
			_ = connection.Close()
			return nil, false, ErrSocketActive
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, false, fmt.Errorf("servicehost: existing socket is not safely stale: %w", dialErr)
		}
		if err := os.Remove(path); err != nil {
			return nil, false, fmt.Errorf("servicehost: remove stale socket: %w", err)
		}
		recovered = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("servicehost: inspect socket: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, false, fmt.Errorf("servicehost: listen: %w", err)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		_ = listener.Close()
		return nil, false, fmt.Errorf("servicehost: unexpected listener %T", listener)
	}
	unixListener.SetUnlinkOnClose(false)
	info, err = os.Lstat(path)
	if err != nil {
		return nil, false, errors.Join(fmt.Errorf("servicehost: inspect created socket: %w", err), listener.Close())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, false, errors.Join(fmt.Errorf("servicehost: unexpected socket metadata %T", info.Sys()), listener.Close())
	}
	private := &privateListener{Listener: listener, path: path, stat: *stat, lock: lock}
	fail := func(cause error) (*privateListener, bool, error) {
		return nil, false, errors.Join(cause, private.Close())
	}
	if err := validateSocketOwner(info, uid); err != nil {
		return fail(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fail(fmt.Errorf("servicehost: protect socket: %w", err))
	}
	keepLock = true
	return private, recovered, nil
}

func (l *privateListener) Close() error {
	l.closeOnce.Do(func() {
		closeErr := l.Listener.Close()
		if errors.Is(closeErr, net.ErrClosed) {
			closeErr = nil
		}
		info, removeErr := os.Lstat(l.path)
		if removeErr == nil {
			if sameStatIdentity(info, l.stat) {
				removeErr = os.Remove(l.path)
			}
		}
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		l.closeErr = errors.Join(closeErr, removeErr, l.lock.Close())
	})
	return l.closeErr
}

func validateSocketOwner(info os.FileInfo, uid uint32) error {
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%w: existing path is not a socket", ErrSocketOwnership)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: unexpected metadata %T", ErrSocketOwnership, info.Sys())
	}
	if stat.Uid != uid {
		return fmt.Errorf("%w: socket UID %d, expected %d", ErrSocketOwnership, stat.Uid, uid)
	}
	return nil
}

func ensurePrivateDirectory(path string, uid uint32) error {
	if !cleanAbsolute(path) {
		return errors.New("servicehost: runtime directory must be clean and absolute")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("servicehost: create runtime directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return errors.New("servicehost: runtime directory must not traverse symlinks")
	}
	if resolved != path {
		return errors.New("servicehost: runtime directory must not traverse symlinks")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("servicehost: runtime path must be a directory")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("servicehost: runtime path must be a directory")
	}
	if !info.IsDir() {
		return errors.New("servicehost: runtime path must be a directory")
	}
	if !ownedByUID(info, uid) {
		return fmt.Errorf("servicehost: runtime directory is not owned by UID %d", uid)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("servicehost: protect runtime directory: %w", err)
	}
	return nil
}

func sameStatIdentity(info os.FileInfo, want syscall.Stat_t) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Dev == want.Dev && stat.Ino == want.Ino
}

func cleanAbsolute(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && !containsNUL(path)
}

func containsNUL(value string) bool {
	for _, char := range value {
		if char == 0 {
			return true
		}
	}
	return false
}
