package sessionhost

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"pgregory.net/rapid"
)

const workspaceCreatedJSON = `{"id":"cli:workspace:create","result":{"type":"workspace_created","workspace":{"workspace_id":"ws-1","number":1,"label":"repo","focused":true,"pane_count":1,"tab_count":1,"active_tab_id":"tab-1","agent_status":"unknown"},"tab":{},"root_pane":{}}}`

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

func TestListValidatesSessionNames(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantErr bool
	}{
		{name: "valid", output: `{"sessions":[{"name":"demo"}]}`},
		{name: "invalid", output: `{"sessions":[{"name":"demo"},{"name":"../other"}]}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &scriptedRunner{outputs: []string{tc.output}}
			sessions, err := (Host{Runner: runner}).List(context.Background())
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "sessions[1]: invalid session name") {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil || len(sessions) != 1 || sessions[0].Name != "demo" {
				t.Fatalf("sessions = %+v, error = %v", sessions, err)
			}
		})
	}
}

func TestStopAndDeleteRequireMatchingResponses(t *testing.T) {
	tests := []struct {
		name   string
		invoke func(Host) error
		output string
	}{
		{
			name:   "stop status",
			invoke: func(host Host) error { return host.Stop(context.Background(), "demo") },
			output: `{"status":"running","session":"demo"}`,
		},
		{
			name:   "stop session",
			invoke: func(host Host) error { return host.Stop(context.Background(), "demo") },
			output: `{"status":"stopped","session":"other"}`,
		},
		{
			name:   "delete status",
			invoke: func(host Host) error { return host.Delete(context.Background(), "demo") },
			output: `{"status":"present","session":"demo","directory":"/tmp/demo"}`,
		},
		{
			name:   "delete session",
			invoke: func(host Host) error { return host.Delete(context.Background(), "demo") },
			output: `{"status":"deleted","session":"other","directory":"/tmp/demo"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &scriptedRunner{outputs: []string{tc.output}}
			if err := tc.invoke(Host{Runner: runner}); err == nil {
				t.Fatal("mismatched response accepted")
			}
		})
	}
}

func TestCommandsPropagateRunnerErrors(t *testing.T) {
	wantErr := errors.New("runner failed")
	runner := runnerFunc(func(context.Context, string, []string) (command.Result, error) {
		return command.Result{}, wantErr
	})
	tests := []struct {
		name   string
		invoke func(Host) error
	}{
		{name: "stop", invoke: func(host Host) error { return host.Stop(context.Background(), "demo") }},
		{name: "delete", invoke: func(host Host) error { return host.Delete(context.Background(), "demo") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.invoke(Host{Runner: runner}); !errors.Is(err, wantErr) {
				t.Fatalf("error = %v, want wrapped %v", err, wantErr)
			}
		})
	}
}

func TestOpenAtDirectoryStopsAfterStartError(t *testing.T) {
	wantErr := errors.New("runner failed")
	calls := 0
	runner := runnerFunc(func(context.Context, string, []string) (command.Result, error) {
		calls++
		return command.Result{}, wantErr
	})
	_, err := (Host{Runner: runner}).OpenAtDirectory(context.Background(), "demo", "/tmp/demo")
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "start named session") {
		t.Fatalf("error = %v, want start error wrapping %v", err, wantErr)
	}
	if calls != 1 {
		t.Fatalf("runner calls = %d, want 1", calls)
	}
}

func TestOpenAtDirectoryUsesProvenComposition(t *testing.T) {
	runner := &scriptedRunner{outputs: []string{"", workspaceCreatedJSON}}
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

func TestOpenAtDirectoryRequiresCreatedWorkspace(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{name: "result type", output: `{"id":"cli:workspace:create","result":{"type":"workspace_present","workspace":{"workspace_id":"ws-1"},"tab":{},"root_pane":{}}}`},
		{name: "workspace ID", output: `{"id":"cli:workspace:create","result":{"type":"workspace_created","workspace":{"workspace_id":""},"tab":{},"root_pane":{}}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &scriptedRunner{outputs: []string{"", tc.output}}
			if _, err := (Host{Runner: runner}).OpenAtDirectory(context.Background(), "demo", "/tmp/demo"); err == nil {
				t.Fatal("invalid workspace response accepted")
			}
		})
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
		{Stdout: []byte(`{"sessions":[]}`), StderrTruncated: true},
	}
	for _, result := range tests {
		runner := runnerFunc(func(context.Context, string, []string) (command.Result, error) { return result, nil })
		if _, err := (Host{Runner: runner}).List(context.Background()); err == nil {
			t.Fatalf("accepted result %+v", result)
		}
	}
}

func TestSessionHostAcceptsJSONAtSizeLimit(t *testing.T) {
	prefix := `{"sessions":[]}`
	output := prefix + strings.Repeat(" ", MaxJSONBytes-len(prefix))
	runner := &scriptedRunner{outputs: []string{output}}
	sessions, err := (Host{Runner: runner}).List(context.Background())
	if err != nil || len(sessions) != 0 {
		t.Fatalf("sessions = %+v, error = %v", sessions, err)
	}
}

func TestOpenAtDirectoryRejectsEitherTruncatedStream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result command.Result
	}{
		{name: "stdout", result: command.Result{StdoutTruncated: true}},
		{name: "stderr", result: command.Result{StderrTruncated: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			runner := runnerFunc(func(context.Context, string, []string) (command.Result, error) {
				calls++
				return tc.result, nil
			})
			_, err := (Host{Runner: runner}).OpenAtDirectory(context.Background(), "demo", "/tmp/demo")
			if err == nil || !strings.Contains(err.Error(), "start named session") {
				t.Fatalf("error = %v, want start error", err)
			}
			if calls != 1 {
				t.Fatalf("runner calls = %d, want 1", calls)
			}
		})
	}
}

func TestDirectoryLengthBoundary(t *testing.T) {
	maximum := "/" + strings.Repeat("a", 4095)
	if got, err := validateDirectory(maximum); err != nil || got != maximum {
		t.Fatalf("4096-byte directory rejected: %v", err)
	}

	tooLong := maximum + "a"
	if _, err := validateDirectory(tooLong); err == nil {
		t.Fatal("4097-byte directory accepted")
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
		runner := &scriptedRunner{outputs: []string{"", workspaceCreatedJSON}}
		_, err := (Host{Runner: runner}).OpenAtDirectory(context.Background(), name, directory)
		if err != nil {
			t.Fatal(err)
		}
		if len(runner.calls) != 2 || !reflect.DeepEqual(runner.calls[0], []string{"herdr", "--session", name}) || !reflect.DeepEqual(runner.calls[1], []string{"herdr", "--session", name, "workspace", "create", "--cwd", directory}) {
			t.Fatalf("calls = %q", runner.calls)
		}
	})
}
