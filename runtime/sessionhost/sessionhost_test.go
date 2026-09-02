package sessionhost

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"pgregory.net/rapid"
)

type scriptedRunner struct {
	outputs []string
	calls   [][]string
}

type runnerFunc func(context.Context, string, []string) (command.Result, error)

func (f runnerFunc) Run(ctx context.Context, executable string, args []string) (command.Result, error) {
	return f(ctx, executable, args)
}

func (r *scriptedRunner) Run(_ context.Context, executable string, args []string) (command.Result, error) {
	r.calls = append(r.calls, append([]string{executable}, args...))
	if len(r.outputs) == 0 {
		return command.Result{}, fmt.Errorf("unexpected call")
	}
	out := r.outputs[0]
	r.outputs = r.outputs[1:]
	return command.Result{Stdout: []byte(out), ExitCode: 0}, nil
}

func TestListStopDeleteCommands(t *testing.T) {
	runner := &scriptedRunner{outputs: []string{
		`{"sessions":[{"name":"demo","default":false,"running":true,"session_dir":"/tmp/s","socket_path":"/tmp/s/sock"}]}`,
		`{"status":"stopped","session":"demo"}`,
		`{"status":"deleted","session":"demo","directory":"/tmp/s"}`,
	}}
	host := Host{Runner: runner}
	sessions, err := host.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Name != "demo" {
		t.Fatalf("sessions = %+v", sessions)
	}
	if err := host.Stop(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if err := host.Delete(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"herdr", "session", "list", "--json"},
		{"herdr", "session", "stop", "demo", "--json"},
		{"herdr", "session", "delete", "demo", "--json"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %q, want %q", runner.calls, want)
	}
}

func TestOpenAtDirectoryUsesProvenComposition(t *testing.T) {
	runner := &scriptedRunner{outputs: []string{"", `{"id":"cli:workspace:create","result":{"type":"workspace_created","workspace":{"workspace_id":"ws-1","number":1,"label":"repo","focused":true,"pane_count":1,"tab_count":1,"active_tab_id":"tab-1","agent_status":"unknown"},"tab":{},"root_pane":{}}}`}}
	workspace, err := (Host{Runner: runner, Herdr: "/bin/herdr"}).OpenAtDirectory(context.Background(), "named", "/tmp/project")
	if err != nil {
		t.Fatal(err)
	}
	if workspace.WorkspaceID != "ws-1" {
		t.Fatalf("workspace = %+v", workspace)
	}
	want := [][]string{
		{"/bin/herdr", "--session", "named"},
		{"/bin/herdr", "--session", "named", "workspace", "create", "--cwd", "/tmp/project"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %q, want %q", runner.calls, want)
	}
}

func TestOpenAtDirectoryRejectsUnsafeInputBeforeExecution(t *testing.T) {
	runner := &scriptedRunner{}
	for _, tc := range []struct{ name, path string }{{"../bad", "/tmp"}, {"good", "relative"}, {"good", "/tmp/../tmp"}, {"bad\x00name", "/tmp"}, {"good", "/tmp/bad\x00path"}, {strings.Repeat("a", 65), "/tmp"}} {
		if _, err := (Host{Runner: runner}).OpenAtDirectory(context.Background(), tc.name, tc.path); err == nil {
			t.Fatalf("accepted name=%q path=%q", tc.name, tc.path)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("executed calls: %q", runner.calls)
	}
}

func TestSessionHostRejectsBoundedOutputFailures(t *testing.T) {
	tests := []command.Result{
		{Stdout: []byte(strings.Repeat(" ", MaxJSONBytes+1))},
		{Stdout: []byte(`{"sessions":[]}`), StdoutTruncated: true},
	}
	for _, result := range tests {
		runner := runnerFunc(func(context.Context, string, []string) (command.Result, error) { return result, nil })
		if _, err := (Host{Runner: runner}).List(context.Background()); err == nil {
			t.Fatalf("accepted result %+v", result)
		}
	}
}

func TestCapabilitiesExposeUnsupportedOperations(t *testing.T) {
	got := SupportedCapabilities()
	if !got.List || !got.Stop || !got.Delete || !got.OpenAtDirectory || got.OneCommandCWD || got.ShortcutBinding {
		t.Fatalf("capabilities = %+v", got)
	}
	if err := (Host{}).Delete(context.Background(), "default"); err == nil {
		t.Fatal("default deletion accepted")
	}
}

func TestPropertySessionCommandComposition(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		name := rapid.StringMatching(`[A-Za-z][A-Za-z0-9_-]{0,12}`).Draw(t, "name")
		component := rapid.StringMatching(`[a-z][a-z0-9_-]{0,12}`).Draw(t, "directory")
		directory := "/tmp/" + component
		runner := &scriptedRunner{outputs: []string{"", `{"id":"cli:workspace:create","result":{"type":"workspace_created","workspace":{"workspace_id":"ws-1","number":1,"label":"repo","focused":true,"pane_count":1,"tab_count":1,"active_tab_id":"tab-1","agent_status":"unknown"},"tab":{},"root_pane":{}}}`}}
		_, err := (Host{Runner: runner}).OpenAtDirectory(context.Background(), name, directory)
		if err != nil {
			t.Fatal(err)
		}
		if len(runner.calls) != 2 || !reflect.DeepEqual(runner.calls[0], []string{"herdr", "--session", name}) || !reflect.DeepEqual(runner.calls[1], []string{"herdr", "--session", name, "workspace", "create", "--cwd", directory}) {
			t.Fatalf("calls = %q", runner.calls)
		}
	})
}
