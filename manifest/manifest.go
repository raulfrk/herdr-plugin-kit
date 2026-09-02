// Package manifest defines the reusable, transport-independent plugin manifest.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	MaxManifestBytes = 1 << 20
	MaxArgs          = 128
	MaxCapabilities  = 64
	MaxInterfaces    = 64
	MaxActions       = 128
)

var stableID = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

type Manifest struct {
	PluginID     string      `json:"plugin_id"`
	Name         string      `json:"name"`
	Version      string      `json:"version"`
	Description  string      `json:"description,omitempty"`
	Executable   string      `json:"executable"`
	Args         []string    `json:"args,omitempty"`
	Capabilities []string    `json:"capabilities,omitempty"`
	Actions      []Action    `json:"actions,omitempty"`
	Interfaces   []Interface `json:"interfaces,omitempty"`
}

type Action struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

type InterfaceDirection string

const (
	InterfaceProvides InterfaceDirection = "provides"
	InterfaceRequires InterfaceDirection = "requires"
)

type Interface struct {
	ID        string             `json:"id"`
	Version   uint32             `json:"version"`
	Direction InterfaceDirection `json:"direction"`
}

func Parse(r io.Reader) (Manifest, error) {
	var m Manifest
	data, err := io.ReadAll(io.LimitReader(r, MaxManifestBytes+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	if len(data) > MaxManifestBytes {
		return Manifest{}, fmt.Errorf("manifest exceeds %d bytes", MaxManifestBytes)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := ensureEOF(dec); err != nil {
		return Manifest{}, err
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (m Manifest) Validate() error {
	if err := ValidateID("plugin_id", m.PluginID); err != nil {
		return err
	}
	if err := display("name", m.Name, 128, true); err != nil {
		return err
	}
	if err := display("version", m.Version, 64, true); err != nil {
		return err
	}
	if err := display("description", m.Description, 1024, false); err != nil {
		return err
	}
	if m.Executable == "" || len(m.Executable) > 4096 || strings.ContainsRune(m.Executable, 0) {
		return errors.New("executable must be a non-empty NUL-free path of at most 4096 bytes")
	}
	if len(m.Args) > MaxArgs {
		return fmt.Errorf("args exceeds %d entries", MaxArgs)
	}
	for i, arg := range m.Args {
		if len(arg) > 4096 || strings.ContainsRune(arg, 0) {
			return fmt.Errorf("args[%d] is invalid", i)
		}
	}
	if len(m.Capabilities) > MaxCapabilities {
		return fmt.Errorf("capabilities exceeds %d entries", MaxCapabilities)
	}
	if err := uniqueIDs("capability", m.Capabilities); err != nil {
		return err
	}
	if len(m.Actions) > MaxActions {
		return fmt.Errorf("actions exceeds %d entries", MaxActions)
	}
	seenActions := map[string]bool{}
	for i, action := range m.Actions {
		if err := ValidateID("action id", action.ID); err != nil {
			return fmt.Errorf("actions[%d]: %w", i, err)
		}
		if seenActions[action.ID] {
			return fmt.Errorf("duplicate action id %q", action.ID)
		}
		seenActions[action.ID] = true
		if err := display("action title", action.Title, 128, true); err != nil {
			return fmt.Errorf("actions[%d]: %w", i, err)
		}
		if err := display("action description", action.Description, 1024, false); err != nil {
			return fmt.Errorf("actions[%d]: %w", i, err)
		}
	}
	if len(m.Interfaces) > MaxInterfaces {
		return fmt.Errorf("interfaces exceeds %d entries", MaxInterfaces)
	}
	seenInterfaces := map[string]bool{}
	for i, iface := range m.Interfaces {
		if err := ValidateID("interface id", iface.ID); err != nil {
			return fmt.Errorf("interfaces[%d]: %w", i, err)
		}
		if iface.Version == 0 {
			return fmt.Errorf("interfaces[%d]: version must be positive", i)
		}
		if iface.Direction != InterfaceProvides && iface.Direction != InterfaceRequires {
			return fmt.Errorf("interfaces[%d]: invalid direction %q", i, iface.Direction)
		}
		key := fmt.Sprintf("%s/%d/%s", iface.ID, iface.Version, iface.Direction)
		if seenInterfaces[key] {
			return fmt.Errorf("duplicate interface declaration %q", key)
		}
		seenInterfaces[key] = true
	}
	return nil
}

func ValidateID(field, value string) error {
	if len(value) == 0 || len(value) > 64 || !stableID.MatchString(value) {
		return fmt.Errorf("%s must be a stable lowercase identifier of at most 64 bytes", field)
	}
	return nil
}

func uniqueIDs(field string, values []string) error {
	seen := map[string]bool{}
	for i, value := range values {
		if err := ValidateID(field, value); err != nil {
			return fmt.Errorf("%s[%d]: %w", field, i, err)
		}
		if seen[value] {
			return fmt.Errorf("duplicate %s %q", field, value)
		}
		seen[value] = true
	}
	return nil
}

func display(field, value string, max int, required bool) error {
	if (required && strings.TrimSpace(value) == "") || len(value) > max || strings.ContainsRune(value, 0) {
		return fmt.Errorf("%s must be %sNUL-free text of at most %d bytes", field, map[bool]string{true: "non-empty ", false: ""}[required], max)
	}
	return nil
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("manifest must contain exactly one JSON value")
	}
	return nil
}
