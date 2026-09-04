//go:build linux

package scaffold

import (
	"os"

	"golang.org/x/sys/unix"
)

func publishNoReplace(parent *os.File, staging, output string) error {
	return unix.Renameat2(int(parent.Fd()), staging, int(parent.Fd()), output, unix.RENAME_NOREPLACE)
}

func openReadOnlyNoFollow(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW, 0)
}
