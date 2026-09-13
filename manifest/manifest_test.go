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
	m = validManifest()
	m.Events = []EventHook{{On: "pane.focused", Command: []string{"plugin"}}, {On: "pane.focused", Command: []string{"plugin"}}}
	if err := m.Validate(); err == nil {
		t.Fatal("duplicate event hook accepted")
	}
}

func TestManifestAcceptsExactHerdrEventHookAllowlist(t *testing.T) {
	names := []string{
		"workspace.created", "workspace.updated", "workspace.closed", "workspace.renamed",
		"workspace.moved", "workspace.reordered", "workspace.focused", "worktree.created",
		"worktree.opened", "worktree.removed", "tab.created", "tab.closed", "tab.renamed",
		"tab.moved", "tab.focused", "pane.created", "pane.closed", "pane.focused",
		"pane.moved", "pane.exited", "pane.agent_detected", "pane.agent_status_changed",
	}
	if len(names) != 22 || len(eventHookNames) != len(names) {
		t.Fatalf("event allowlist size = %d, want 22", len(eventHookNames))
	}
	m := validManifest()
	for _, name := range names {
		m.Events = append(m.Events, EventHook{On: name, Command: []string{"plugin", "event", name}})
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("exact Herdr event allowlist rejected: %v", err)
	}
	for _, name := range []string{
		"workspace.metadata_updated", "pane.updated", "pane.output_changed",
		"pane.scroll_changed", "layout.updated", "unknown.event",
	} {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			m.Events = []EventHook{{On: name, Command: []string{"plugin"}}}
			if err := m.Validate(); err == nil {
				t.Fatalf("unsupported event %q accepted", name)
			}
		})
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
		{"too many events", func(m *Manifest) { m.Events = make([]EventHook, MaxEvents+1) }},
		{"too many interfaces", func(m *Manifest) { m.Interfaces = make([]Interface, MaxInterfaces+1) }},
		{"empty action contexts", func(m *Manifest) { m.Actions[0].Contexts = []string{} }},
		{"duplicate action context", func(m *Manifest) { m.Actions[0].Contexts = []string{"global", "global"} }},
		{"unsupported action context", func(m *Manifest) { m.Actions[0].Contexts = []string{"window"} }},
		{"empty action command", func(m *Manifest) { m.Actions[0].Command = []string{} }},
		{"empty action executable", func(m *Manifest) { m.Actions[0].Command = []string{""} }},
		{"too many action args", func(m *Manifest) { m.Actions[0].Command = make([]string, MaxArgs+1) }},
		{"empty event command", func(m *Manifest) { m.Events = []EventHook{{On: "pane.focused"}} }},
		{"empty event executable", func(m *Manifest) { m.Events = []EventHook{{On: "pane.focused", Command: []string{""}}} }},
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
		{"action title at limit", 128, func(m *Manifest, value string) { m.Actions[0].Title = value }, true},
		{"action title beyond limit", 129, func(m *Manifest, value string) { m.Actions[0].Title = value }, false},
		{"action description at limit", 1024, func(m *Manifest, value string) { m.Actions[0].Description = value }, true},
		{"action description beyond limit", 1025, func(m *Manifest, value string) { m.Actions[0].Description = value }, false},
		{"action command at limit", 4096, func(m *Manifest, value string) { m.Actions[0].Command = []string{value} }, true},
		{"action command beyond limit", 4097, func(m *Manifest, value string) { m.Actions[0].Command = []string{value} }, false},
		{"event command at limit", 4096, func(m *Manifest, value string) {
			m.Events = []EventHook{{On: "pane.focused", Command: []string{value}}}
		}, true},
		{"event command beyond limit", 4097, func(m *Manifest, value string) {
			m.Events = []EventHook{{On: "pane.focused", Command: []string{value}}}
		}, false},
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
