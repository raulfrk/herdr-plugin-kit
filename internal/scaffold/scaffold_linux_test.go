//go:build linux

package scaffold

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestValidateRejectsExtraSpecialFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugin")
	if err := Generate(Options{ID: "example.special", Name: "Special", Output: root}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "runtime.fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	if err := Validate(root); err == nil {
		t.Fatal("special file passed validation")
	}
}
