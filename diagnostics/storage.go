package diagnostics

import (
	"errors"
	"io"
	"os"
)

// storageHooks belongs to one Recorder. Tests wrap real operations to exercise
// failures before and after publication; there is no process-global switch.
type storageHooks struct {
	open       func(string, int, os.FileMode) (*os.File, error)
	createTemp func(string, string) (*os.File, error)
	write      func(*os.File, []byte) (int, error)
	sync       func(*os.File) error
	close      func(*os.File) error
	rename     func(string, string) error
	remove     func(string) error
	mkdir      func(string, os.FileMode) error
}

func defaultStorageHooks() storageHooks {
	return storageHooks{open: os.OpenFile, createTemp: os.CreateTemp,
		write: (*os.File).Write, sync: (*os.File).Sync, close: (*os.File).Close,
		rename: os.Rename, remove: os.Remove, mkdir: os.Mkdir}
}

// A hook may return a descriptor together with an injected after-open error.
// In that case the caller never owns it; release it directly on the error path.
func (h storageHooks) openFile(path string, flags int) (*os.File, error) {
	f, err := h.open(path, flags, 0o600)
	if err != nil && f != nil {
		_ = f.Close()
		f = nil
	}
	return f, err
}

func (h storageHooks) temporary(dir string) (*os.File, error) {
	f, err := h.createTemp(dir, ".boundary-*.tmp")
	if err != nil && f != nil {
		_ = f.Close()
		f = nil
	}
	return f, err
}

// Consume ownership even on failure. Direct best-effort cleanup covers a hook
// that failed before close; os.File also makes an after-close retry harmless.
func (h storageHooks) closeFile(owned **os.File) error {
	f := *owned
	*owned = nil
	if f == nil {
		return nil
	}
	err := h.close(f)
	if err != nil {
		_ = f.Close()
	}
	return err
}

func (h storageHooks) writeAll(f *os.File, data []byte) error {
	n, err := h.write(f, data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

func (h storageHooks) syncDirectory(path string) error {
	f, err := h.openFile(path, os.O_RDONLY)
	if err != nil {
		return err
	}
	syncErr := h.sync(f)
	return errors.Join(syncErr, h.closeFile(&f))
}
