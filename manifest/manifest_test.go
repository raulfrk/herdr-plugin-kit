package manifest

import (
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func validManifest() Manifest {
	return Manifest{
		PluginID:     "example.plugin",
		Name:         "Example",
		Version:      "1.0.0",
		Executable:   "/usr/bin/example",
		Capabilities: []string{"workspace.read"},
		Actions:      []Action{{ID: "open-project", Title: "Open project"}},
		Interfaces:   []Interface{{ID: "document.open", Version: 1, Direction: InterfaceProvides}},
	}
}

func TestParseRejectsUnknownAndTrailingJSON(t *testing.T) {
	for _, input := range []string{
		`{"plugin_id":"example","name":"Example","version":"1","executable":"example","unknown":true}`,
		`{"plugin_id":"example","name":"Example","version":"1","executable":"example"} {}`,
	} {
		if _, err := Parse(strings.NewReader(input)); err == nil {
			t.Fatalf("Parse(%q) accepted malformed input", input)
		}
	}
}

func TestManifestRejectsDuplicateDeclarations(t *testing.T) {
	m := validManifest()
	m.Actions = append(m.Actions, m.Actions[0])
	if err := m.Validate(); err == nil {
		t.Fatal("duplicate action accepted")
	}
	m = validManifest()
	m.Interfaces = append(m.Interfaces, m.Interfaces[0])
	if err := m.Validate(); err == nil {
		t.Fatal("duplicate interface accepted")
	}
}

func TestManifestValidationBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"duplicate capability", func(m *Manifest) { m.Capabilities = []string{"workspace.read", "workspace.read"} }},
		{"zero interface version", func(m *Manifest) { m.Interfaces[0].Version = 0 }},
		{"invalid interface direction", func(m *Manifest) { m.Interfaces[0].Direction = "both" }},
		{"long metadata", func(m *Manifest) { m.Name = strings.Repeat("x", 129) }},
		{"nul executable", func(m *Manifest) { m.Executable = "bad\x00path" }},
		{"long argument", func(m *Manifest) { m.Args = []string{strings.Repeat("x", 4097)} }},
		{"too many capabilities", func(m *Manifest) { m.Capabilities = numberedIDs("cap", MaxCapabilities+1) }},
		{"too many actions", func(m *Manifest) { m.Actions = make([]Action, MaxActions+1) }},
		{"too many interfaces", func(m *Manifest) { m.Interfaces = make([]Interface, MaxInterfaces+1) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := validManifest()
			tc.mutate(&m)
			if err := m.Validate(); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
	if err := ValidateID("id", "a"+strings.Repeat("b", 63)); err != nil {
		t.Fatalf("64-byte ID rejected: %v", err)
	}
	if err := ValidateID("id", "a"+strings.Repeat("b", 64)); err == nil {
		t.Fatal("65-byte ID accepted")
	}
}

func TestParseRejectsOversizedManifest(t *testing.T) {
	if _, err := Parse(strings.NewReader(strings.Repeat(" ", MaxManifestBytes+1))); err == nil {
		t.Fatal("oversized manifest accepted")
	}
}

func numberedIDs(prefix string, count int) []string {
	values := make([]string, count)
	for i := range values {
		values[i] = fmt.Sprintf("%s-%d", prefix, i)
	}
	return values
}

func TestPropertyStableIdentifiers(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		parts := rapid.SliceOfN(rapid.StringMatching(`[a-z][a-z0-9]{0,7}`), 1, 5).Draw(t, "parts")
		id := strings.Join(parts, ".")
		m := validManifest()
		m.PluginID = id
		if err := m.Validate(); err != nil {
			t.Fatalf("generated stable ID %q rejected: %v", id, err)
		}
		m.PluginID = "-" + id
		if err := m.Validate(); err == nil {
			t.Fatalf("unstable ID %q accepted", m.PluginID)
		}
	})
}

func TestPropertyManifestArgumentBound(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, MaxArgs+2).Draw(t, "count")
		m := validManifest()
		m.Args = make([]string, n)
		err := m.Validate()
		if (n <= MaxArgs) != (err == nil) {
			t.Fatalf("count=%d err=%v", n, err)
		}
	})
}
