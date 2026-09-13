package sessionhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"pgregory.net/rapid"
)

type apiCall struct {
	socket, id, method string
	params             []byte
}

type fakeAPI struct {
	result any
	err    error
	calls  []apiCall
}

func (f *fakeAPI) Call(_ context.Context, socket, id, method string, params, result any) error {
	encoded, err := json.Marshal(params)
	if err != nil {
		return err
	}
	f.calls = append(f.calls, apiCall{socket, id, method, encoded})
	if f.err != nil {
		return f.err
	}
	value, err := json.Marshal(f.result)
	if err != nil {
		return err
	}
	return json.Unmarshal(value, result)
}

func paneJSON(id, workspace, tab, cwd string, focused bool) string {
	return fmt.Sprintf(`{"id":"test","result":{"type":"pane_info","pane":{"pane_id":%q,"workspace_id":%q,"tab_id":%q,"cwd":%q,"focused":%t}}}`, id, workspace, tab, cwd, focused)
}

func sessionListJSON(name, socket string, named bool) string {
	return fmt.Sprintf(`{"sessions":[{"name":%q,"default":%t,"running":true,"session_dir":"/tmp/session","socket_path":%q}]}`, name, !named, socket)
}

func processInfoJSON(pane, cwd, shell string, ready bool) string {
	shellPID, foregroundPID := 101, 101
	name, argv := shell, []string{"/usr.substring.invalid"}
	if ready {
		argv = []string{"/bin/" + shell}
	} else {
		foregroundPID, name, argv = 102, "grep", nil
	}
	value, _ := json.Marshal(map[string]any{
		"id": "test", "result": map[string]any{"type": "pane_process_info", "process_info": map[string]any{
			"pane_id": pane, "shell_pid": shellPID, "foreground_process_group_id": foregroundPID,
			"foreground_processes": []any{map[string]any{"pid": foregroundPID, "name": name, "cwd": cwd, "argv": argv}},
		}},
	})
	return string(value)
}

func okJSON() string {
	return `{"id":"test","result":{"type":"ok"}}`
}

func layoutResult(argv []string) any {
	return map[string]any{
		"type": "layout_apply",
		"layout": map[string]any{
			"workspace_id": "w1", "tab_id": "w1:t2", "focused_pane_id": "w1:p2",
			"root": map[string]any{"type": "pane", "pane_id": "w1:p2", "cwd": "/tmp/project", "command": argv},
		},
	}
}

func paneResult(id, workspace, tab, cwd string, focused bool) any {
	return map[string]any{
		"type": "pane_info",
		"pane": map[string]any{"pane_id": id, "workspace_id": workspace, "tab_id": tab, "cwd": cwd, "focused": focused},
	}
}

func TestLaunchNewTabUsesExactDirectArgvInDefaultSession(t *testing.T) {
	argv := []string{"agent", "literal space", "$(touch /tmp/bad)", "", "quote\"value"}
	runner := &scriptedRunner{outputs: []string{
		paneJSON("w1:p1", "w1", "w1:t1", "/tmp/origin", true),
		sessionListJSON("default", "/tmp/session/herdr.sock", false),
		paneJSON("w1:p1", "w1", "w1:t1", "/tmp/origin", true),
	}}
	api := &fakeAPI{result: layoutResult(argv)}
	host := Host{Runner: runner, api: api}
	created, err := host.Launch(context.Background(), LaunchRequest{
		Origin: Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   argv, Directory: "/tmp/project", Placement: NewTab, TabLabel: "profile", Focus: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created != (CreatedTarget{Origin: Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}, CreatedTab: true, DirectArgv: true}) {
		t.Fatalf("created = %+v", created)
	}
	if len(api.calls) != 1 || api.calls[0].socket != "/tmp/session/herdr.sock" || api.calls[0].method != "layout.apply" {
		t.Fatalf("API calls = %+v", api.calls)
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(api.calls[0].params, &params); err != nil {
		t.Fatal(err)
	}
	if _, present := params["env"]; present {
		t.Fatalf("layout params injected env: %s", api.calls[0].params)
	}
	var root struct {
		Command []string `json:"command"`
		CWD     string   `json:"cwd"`
	}
	if err := json.Unmarshal(params["root"], &root); err != nil || !reflect.DeepEqual(root.Command, argv) || root.CWD != "/tmp/project" {
		t.Fatalf("root = %+v, error = %v", root, err)
	}
	wantCalls := [][]string{{"herdr", "pane", "get", "w1:p1"}, {"herdr", "session", "list", "--json"}, {"herdr", "pane", "get", "w1:p1"}}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("calls = %q, want %q", runner.calls, wantCalls)
	}
}

func TestLaunchNewPaneUsesStrictExecAndNamedSession(t *testing.T) {
	argv := []string{"my agent", "it's $(touch /tmp/bad)", "semi;colon", "line\nbreak", ""}
	runner := &scriptedRunner{outputs: []string{
		paneJSON("w1:p1", "w1", "w1:t1", "/tmp/origin", true),
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false),
		processInfoJSON("w1:p2", "/tmp/project", "zsh", false),
		processInfoJSON("w1:p2", "/tmp/project", "zsh", true),
		"true",
		paneJSON("w1:p1", "w1", "w1:t1", "/tmp/origin", true),
	}}
	host := Host{Runner: runner, Session: "named", wait: func(context.Context, time.Duration) error { return nil }}
	created, err := host.Launch(context.Background(), LaunchRequest{
		Origin: Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   argv, Directory: "/tmp/project", Placement: NewPane, Direction: "right",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Origin != (Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}) || created.CreatedTab || created.DirectArgv {
		t.Fatalf("created = %+v", created)
	}
	encoded := `exec 'my agent' 'it'"'"'s $(touch /tmp/bad)' 'semi;colon' 'line
break' ''`
	want := [][]string{
		{"herdr", "--session", "named", "pane", "get", "w1:p1"},
		{"herdr", "--session", "named", "pane", "split", "--pane", "w1:p1", "--direction", "right", "--cwd", "/tmp/project", "--no-focus"},
		{"herdr", "--session", "named", "pane", "process-info", "--pane", "w1:p2"},
		{"herdr", "--session", "named", "pane", "process-info", "--pane", "w1:p2"},
		{"herdr", "--session", "named", "pane", "run", "w1:p2", encoded},
		{"herdr", "--session", "named", "pane", "get", "w1:p1"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestLaunchAllowsOnlyFixedOMPStatusEnvironmentDelta(t *testing.T) {
	runner := &scriptedRunner{outputs: []string{
		paneJSON("w1:p1", "w1", "w1:t1", "/tmp/origin", true),
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false), processInfoJSON("w1:p2", "/tmp/project", "zsh", true), "true",
		paneJSON("w1:p1", "w1", "w1:t1", "/tmp/origin", true),
	}}
	_, err := (Host{Runner: runner}).Launch(context.Background(), LaunchRequest{
		Origin: Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"/opt/bin/omp", "--model", "demo"}, Directory: "/tmp/project",
		Placement: NewPane, Direction: "right", OMPStatusSocket: "/tmp/plugin/status.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := runner.calls[1]; !reflect.DeepEqual(got, []string{"herdr", "pane", "split", "--pane", "w1:p1", "--direction", "right", "--cwd", "/tmp/project", "--env", "HERDR_PLUGIN_KIT_OMP_STATUS_SOCKET=/tmp/plugin/status.sock", "--no-focus"}) {
		t.Fatalf("split call = %q", got)
	}

	for _, request := range []LaunchRequest{
		{Origin: Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"}, Argv: []string{"codex"}, Directory: "/tmp/project", Placement: NewTab, OMPStatusSocket: "/tmp/status.sock"},
		{Origin: Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"}, Argv: []string{"omp"}, Directory: "/tmp/project", Placement: NewTab, OMPStatusSocket: "relative"},
	} {
		cleanRunner := &scriptedRunner{}
		if _, err := (Host{Runner: cleanRunner}).Launch(context.Background(), request); err == nil || len(cleanRunner.calls) != 0 {
			t.Fatalf("unsafe OMP environment request accepted: %+v, calls=%q", request, cleanRunner.calls)
		}
	}
}

func TestLaunchFocusesOnlyAfterCreationAndRechecksTarget(t *testing.T) {
	runner := &scriptedRunner{outputs: []string{
		paneJSON("w1:p1", "w1", "w1:t1", "/tmp/origin", true),
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false), processInfoJSON("w1:p2", "/tmp/project", "zsh", true), "true",
		paneJSON("w1:p1", "w1", "w1:t1", "/tmp/origin", true),
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false),
		sessionListJSON("named", "/tmp/session/herdr.sock", true),
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", true),
	}}
	api := &fakeAPI{result: paneResult("w1:p2", "w1", "w1:t1", "/tmp/project", true)}
	_, err := (Host{Runner: runner, Session: "named", api: api}).Launch(context.Background(), LaunchRequest{
		Origin: Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"agent"}, Directory: "/tmp/project", Placement: NewPane, Direction: "down", Focus: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(api.calls) != 1 || api.calls[0].socket != "/tmp/session/herdr.sock" || api.calls[0].method != "pane.focus" {
		t.Fatalf("API calls = %+v", api.calls)
	}
	var params struct {
		PaneID string `json:"pane_id"`
	}
	if err := json.Unmarshal(api.calls[0].params, &params); err != nil || params.PaneID != "w1:p2" {
		t.Fatalf("focus params = %+v, error = %v", params, err)
	}
}

func TestLaunchRejectsCrossSessionAndUnsafeInputBeforeExecution(t *testing.T) {
	base := LaunchRequest{Origin: Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"}, Argv: []string{"agent"}, Directory: "/tmp/project", Placement: NewTab}
	for _, mutate := range []func(*LaunchRequest){
		func(r *LaunchRequest) { r.Origin.SessionName = "other" },
		func(r *LaunchRequest) { r.Argv = nil },
		func(r *LaunchRequest) { r.Argv = []string{""} },
		func(r *LaunchRequest) { r.Argv = []string{"-a", "changed", "/bin/printf"} },
		func(r *LaunchRequest) { r.Argv = []string{"agent", "bad\x00arg"} },
		func(r *LaunchRequest) {
			r.Argv = []string{"agent", "bad\x1barg"}
			r.Placement = NewPane
			r.Direction = "right"
		},
		func(r *LaunchRequest) { r.Argv = []string{"agent", string([]byte{0xff})} },
		func(r *LaunchRequest) { r.Directory = "relative" },
		func(r *LaunchRequest) { r.Placement = "unknown" },
		func(r *LaunchRequest) { r.Direction = "right" },
	} {
		request := base
		mutate(&request)
		runner := &scriptedRunner{}
		if _, err := (Host{Runner: runner}).Launch(context.Background(), request); err == nil {
			t.Fatalf("request accepted: %+v", request)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("unsafe request executed: %q", runner.calls)
		}
	}
}

func TestLaunchFailureCleansOnlyCreatedPane(t *testing.T) {
	runner := &scriptedRunner{outputs: []string{
		paneJSON("w1:p1", "w1", "w1:t1", "/tmp/origin", true),
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false),
		processInfoJSON("w1:p2", "/tmp/project", "zsh", true),
		"false",
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false),
		okJSON(),
	}}
	_, err := (Host{Runner: runner}).Launch(context.Background(), LaunchRequest{
		Origin: Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"agent"}, Directory: "/tmp/project", Placement: NewPane, Direction: "right",
	})
	if err == nil || !strings.Contains(err.Error(), "did not acknowledge") {
		t.Fatalf("error = %v", err)
	}
	if got := runner.calls[len(runner.calls)-1]; !reflect.DeepEqual(got, []string{"herdr", "pane", "close", "w1:p2"}) {
		t.Fatalf("cleanup call = %q", got)
	}
}

func TestLaunchRejectsUnsupportedShellBeforeCommandAndCleansPane(t *testing.T) {
	runner := &scriptedRunner{outputs: []string{
		paneJSON("w1:p1", "w1", "w1:t1", "/tmp/origin", true),
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false),
		processInfoJSON("w1:p2", "/tmp/project", "fish", true),
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false), okJSON(),
	}}
	_, err := (Host{Runner: runner}).Launch(context.Background(), LaunchRequest{
		Origin: Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"agent"}, Directory: "/tmp/project", Placement: NewPane, Direction: "right",
	})
	if !errors.Is(err, ErrUnsupportedShell) {
		t.Fatalf("error = %v", err)
	}
	for _, call := range runner.calls {
		if len(call) >= 3 && call[1] == "pane" && call[2] == "run" {
			t.Fatalf("command delivered to unsupported shell: %q", call)
		}
	}
	if got := runner.calls[len(runner.calls)-1]; !reflect.DeepEqual(got, []string{"herdr", "pane", "close", "w1:p2"}) {
		t.Fatalf("cleanup call = %q", got)
	}
}

func TestLaunchReadinessCancellationCleansPaneWithUncancelledContext(t *testing.T) {
	runner := &scriptedRunner{outputs: []string{
		paneJSON("w1:p1", "w1", "w1:t1", "/tmp/origin", true),
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false),
		processInfoJSON("w1:p2", "/tmp/project", "zsh", false),
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false), okJSON(),
	}}
	host := Host{
		Runner: runner,
		wait: func(context.Context, time.Duration) error {
			return context.Canceled
		},
	}
	_, err := host.Launch(context.Background(), LaunchRequest{
		Origin: Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"agent"}, Directory: "/tmp/project", Placement: NewPane, Direction: "right",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if got := runner.calls[len(runner.calls)-1]; !reflect.DeepEqual(got, []string{"herdr", "pane", "close", "w1:p2"}) {
		t.Fatalf("cleanup call = %q", got)
	}
}

func TestExecEncodingRoundTripsThroughInstalledSupportedShells(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "not-run")
	literal := "it's $(touch " + marker + ")"
	argv := []string{"/usr/bin/printf", "[%s]\\n", "literal space", literal, "semi;colon", ""}
	command, err := encodeExec(argv)
	if err != nil {
		t.Fatal(err)
	}
	for _, shell := range []string{"sh", "bash", "zsh"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		t.Run(filepath.Base(path), func(t *testing.T) {
			output, err := exec.Command(path, "-c", command).CombinedOutput()
			want := "[literal space]\n[" + literal + "]\n[semi;colon]\n[]\n"
			if err != nil || string(output) != want {
				t.Fatalf("output=%q error=%v want=%q", output, err, want)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("argument syntax executed: marker error = %v", err)
			}
		})
	}
}

func TestLaunchCleansPlausibleCreatedTargetOnResponseDrift(t *testing.T) {
	t.Run("new tab", func(t *testing.T) {
		argv := []string{"agent"}
		result := layoutResult([]string{"drifted"})
		runner := &scriptedRunner{outputs: []string{
			paneJSON("w1:p1", "w1", "w1:t1", "/tmp/origin", true),
			sessionListJSON("default", "/tmp/session/herdr.sock", false),
			paneJSON("w1:p2", "w1", "w1:t2", "/tmp/project", false), okJSON(),
		}}
		_, err := (Host{Runner: runner, api: &fakeAPI{result: result}}).Launch(context.Background(), LaunchRequest{
			Origin: Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"}, Argv: argv, Directory: "/tmp/project", Placement: NewTab,
		})
		got := runner.calls[len(runner.calls)-1]
		if !errors.Is(err, ErrStaleTarget) || !reflect.DeepEqual(got, []string{"herdr", "pane", "close", "w1:p2"}) {
			t.Fatalf("error=%v calls=%q", err, runner.calls)
		}
	})
	t.Run("new pane", func(t *testing.T) {
		runner := &scriptedRunner{outputs: []string{
			paneJSON("w1:p1", "w1", "w1:t1", "/tmp/origin", true),
			paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", true),
			paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", true), okJSON(),
		}}
		_, err := (Host{Runner: runner}).Launch(context.Background(), LaunchRequest{
			Origin: Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"}, Argv: []string{"agent"}, Directory: "/tmp/project", Placement: NewPane, Direction: "right",
		})
		got := runner.calls[len(runner.calls)-1]
		if !errors.Is(err, ErrStaleTarget) || !reflect.DeepEqual(got, []string{"herdr", "pane", "close", "w1:p2"}) {
			t.Fatalf("error=%v calls=%q", err, runner.calls)
		}
	})
}

func TestCleanupRejectsChangedIdentityWithoutClosing(t *testing.T) {
	runner := &scriptedRunner{outputs: []string{paneJSON("w1:p2", "other", "w1:t1", "/tmp/project", false)}}
	err := (Host{Runner: runner}).Cleanup(context.Background(), CreatedTarget{Origin: Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}})
	if !errors.Is(err, ErrStaleTarget) || len(runner.calls) != 1 {
		t.Fatalf("error = %v, calls = %q", err, runner.calls)
	}
}

func TestSessionSocketRejectsAmbiguousAndStoppedSessions(t *testing.T) {
	for _, output := range []string{
		`{"sessions":[{"name":"default","default":true,"running":true,"socket_path":"/tmp/a"},{"name":"other","default":true,"running":true,"socket_path":"/tmp/b"}]}`,
		`{"sessions":[{"name":"default","default":true,"running":false,"socket_path":"/tmp/a"}]}`,
	} {
		runner := &scriptedRunner{outputs: []string{output}}
		if _, err := (Host{Runner: runner}).sessionSocket(context.Background()); err == nil {
			t.Fatalf("sessions accepted: %s", output)
		}
	}
}

func TestPropertyExecEncodingNeverExposesArgumentSyntax(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		args := rapid.SliceOfN(rapid.StringN(0, 32, 128), 1, 16).Draw(t, "args")
		for i := range args {
			args[i] = strings.ReplaceAll(args[i], "\x00", "")
		}
		command, err := encodeExec(args)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeExec(command)
		if err != nil || !reflect.DeepEqual(decoded, args) {
			t.Fatalf("decodeExec(%q) = %#v, %v; want %#v", command, decoded, err, args)
		}
	})
}

func decodeExec(command string) ([]string, error) {
	if !strings.HasPrefix(command, "exec ") {
		return nil, errors.New("missing exec prefix")
	}
	input := command[len("exec "):]
	var result []string
	for len(input) > 0 {
		if input[0] != '\'' {
			return nil, errors.New("missing opening quote")
		}
		input = input[1:]
		var value strings.Builder
		for {
			quote := strings.IndexByte(input, '\'')
			if quote < 0 {
				return nil, errors.New("missing closing quote")
			}
			value.WriteString(input[:quote])
			input = input[quote:]
			if strings.HasPrefix(input, `'"'"'`) {
				value.WriteByte('\'')
				input = input[len(`'"'"'`):]
				continue
			}
			input = input[1:]
			result = append(result, value.String())
			break
		}
		if input == "" {
			break
		}
		if input[0] != ' ' {
			return nil, errors.New("missing argument separator")
		}
		input = input[1:]
	}
	return result, nil
}

var _ command.Runner = (*scriptedRunner)(nil)
