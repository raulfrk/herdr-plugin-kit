package manifest

import (
	"errors"
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

func TestParseValidatesDecodedManifest(t *testing.T) {
	input := `{"plugin_id":"INVALID","name":"Example","version":"1","executable":"example"}`
	if _, err := Parse(strings.NewReader(input)); err == nil {
		t.Fatal("Parse accepted a syntactically valid manifest with an invalid plugin ID")
	}
}

func TestParsePropagatesReaderErrors(t *testing.T) {
	want := errors.New("reader failed")
	_, err := Parse(errorReader{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("Parse error = %v, want wrapped reader error", err)
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
		{"nul executable", func(m *Manifest) { m.Executable = "bad\x00path" }},
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

func TestManifestAcceptsCollectionLimits(t *testing.T) {
	m := validManifest()
	m.Args = make([]string, MaxArgs)
	m.Capabilities = numberedIDs("cap", MaxCapabilities)
	m.Actions = numberedActions(MaxActions)
	m.Interfaces = numberedInterfaces(MaxInterfaces)
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest at collection limits rejected: %v", err)
	}
}

func TestManifestTextLengthBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		length int
		mutate func(*Manifest, string)
		valid  bool
	}{
		{"executable at limit", 4096, func(m *Manifest, value string) { m.Executable = value }, true},
		{"executable beyond limit", 4097, func(m *Manifest, value string) { m.Executable = value }, false},
		{"argument at limit", 4096, func(m *Manifest, value string) { m.Args = []string{value} }, true},
		{"argument beyond limit", 4097, func(m *Manifest, value string) { m.Args = []string{value} }, false},
		{"name at limit", 128, func(m *Manifest, value string) { m.Name = value }, true},
		{"name beyond limit", 129, func(m *Manifest, value string) { m.Name = value }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := validManifest()
			tc.mutate(&m, strings.Repeat("x", tc.length))
			err := m.Validate()
			if tc.valid != (err == nil) {
				t.Fatalf("Validate() error = %v, valid = %t", err, tc.valid)
			}
		})
	}
}

func TestManifestRejectsEmptyExecutable(t *testing.T) {
	m := validManifest()
	m.Executable = ""
	if err := m.Validate(); err == nil {
		t.Fatal("empty executable accepted")
	}
}

func TestParseRejectsOversizedManifest(t *testing.T) {
	prefix := `{"plugin_id":"example","name":"Example","version":"1","executable":"example"`
	manifestOfSize := func(size int) string {
		return prefix + strings.Repeat(" ", size-len(prefix)-1) + "}"
	}

	if _, err := Parse(strings.NewReader(manifestOfSize(MaxManifestBytes))); err != nil {
		t.Fatalf("manifest at byte limit rejected: %v", err)
	}
	if _, err := Parse(strings.NewReader(manifestOfSize(MaxManifestBytes + 1))); err == nil || !strings.Contains(err.Error(), "manifest exceeds") {
		t.Fatalf("oversized manifest error = %v, want size-limit error", err)
	}
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

func numberedIDs(prefix string, count int) []string {
	values := make([]string, count)
	for i := range values {
		values[i] = fmt.Sprintf("%s-%d", prefix, i)
	}
	return values
}

func numberedActions(count int) []Action {
	actions := make([]Action, count)
	for i := range actions {
		actions[i] = Action{ID: fmt.Sprintf("action-%d", i), Title: "Action"}
	}
	return actions
}

func numberedInterfaces(count int) []Interface {
	interfaces := make([]Interface, count)
	for i := range interfaces {
		interfaces[i] = Interface{ID: fmt.Sprintf("interface-%d", i), Version: 1, Direction: InterfaceProvides}
	}
	return interfaces
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
