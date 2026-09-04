//go:build !linux

package scaffold

import (
	"errors"
	"os"
)

func publishNoReplace(_ *os.File, _, _ string) error {
	return errors.New("atomic plugin generation is supported only on Linux")
}

func openReadOnlyNoFollow(_ *os.Root, _ string) (*os.File, error) {
	return nil, errors.New("plugin validation is supported only on Linux")
}
