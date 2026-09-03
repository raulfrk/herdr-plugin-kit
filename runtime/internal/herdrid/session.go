// Package herdrid validates identifiers passed to Herdr commands.
package herdrid

import (
	"errors"
	"regexp"
)

var sessionName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func ValidateSessionName(name string) error {
	if !sessionName.MatchString(name) || name == "." || name == ".." {
		return errors.New("invalid session name")
	}
	return nil
}
