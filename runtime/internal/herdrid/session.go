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

// Args selects Herdr's default session explicitly when session is empty. This
// prevents a plugin process from inheriting a named HERDR_SOCKET_PATH and
// silently routing an otherwise default-scoped command to the wrong server.
func Args(session string, args []string) ([]string, error) {
	selected := session
	if selected == "" {
		selected = "default"
	} else if selected == "default" {
		return nil, errors.New("the default Herdr session is represented by an empty session name")
	}
	if err := ValidateSessionName(selected); err != nil {
		return nil, err
	}
	return append([]string{"--session", selected}, args...), nil
}
