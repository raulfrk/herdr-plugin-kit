//go:build !linux

package catalogue

import "errors"

func publishNoReplace(_, _ string) error {
	return errors.New("atomic no-replace catalogue publication is unsupported on this platform")
}
