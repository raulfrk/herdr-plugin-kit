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
	"syscall"
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
		"id": "cli:pane:process_info", "result": map[string]any{"type": "pane_process_info", "process_info": map[string]any{
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

func TestRunInShellUsesExactNamedSessionAndStrictLiteralEncoding(t *testing.T) {
	argv := []string{"codex", "resume", "--remote", "unix:///tmp/shared.sock", "--cd", "/tmp/project", "thread'id"}
	runner := &scriptedRunner{outputs: []string{
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false),
		processInfoJSON("w1:p2", "/tmp/project", "bash", true),
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false),
		"",
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false),
	}}
	host := Host{Runner: runner, Session: "named"}
	attempted, err := host.RunInShell(context.Background(), Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}, "/tmp/project", argv)
	if err != nil {
		t.Fatal(err)
	}
	if !attempted {
		t.Fatal("command submission was not reported")
	}
	wantCommand, _ := encodeCommand(argv)
	if len(runner.calls) != 5 || !reflect.DeepEqual(runner.calls[3], []string{"herdr", "--session", "named", "pane", "run", "w1:p2", wantCommand}) {
		t.Fatalf("runner calls = %#v", runner.calls)
	}
}

func TestRunInShellReportsNoAttemptWhenReadinessFails(t *testing.T) {
	target := Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	runner := runnerFunc(func(_ context.Context, executable string, args []string) (command.Result, error) {
		calls++
		switch calls {
		case 1:
			return command.Result{Stdout: []byte(paneJSON(target.PaneID, target.WorkspaceID, target.TabID, "/tmp/project", false))}, nil
		case 2:
			cancel()
			return command.Result{Stdout: []byte(processInfoJSON(target.PaneID, "/tmp/project", "bash", false))}, nil
		default:
			return command.Result{}, fmt.Errorf("unexpected command %s %v", executable, args)
		}
	})
	attempted, err := (Host{Runner: runner}).RunInShell(ctx, target, "/tmp/project", []string{"codex"})
	if attempted || !errors.Is(err, context.Canceled) || calls != 2 {
		t.Fatalf("attempted=%t error=%v calls=%d", attempted, err, calls)
	}
}

func TestVerifyForegroundProcessRequiresExactUniqueGroupLeader(t *testing.T) {
	argv := []string{"codex", "--remote", "unix:///tmp/private.sock", "--cd", "/tmp/project"}
	value, _ := json.Marshal(map[string]any{
		"id": "cli:pane:process_info", "result": map[string]any{"type": "pane_process_info", "process_info": map[string]any{
			"pane_id": "w1:p2", "foreground_process_group_id": 202,
			"foreground_processes": []any{
				map[string]any{"pid": 202, "name": "codex", "cwd": "/tmp/project", "argv": argv},
				map[string]any{"pid": 203, "name": "lsb_release", "cwd": "/tmp/project", "argv": []string{"/usr/bin/lsb_release", "-a"}},
			},
		}},
	})
	host := Host{Runner: &scriptedRunner{outputs: []string{
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false),
		string(value),
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false),
	}}}
	process, err := host.VerifyForegroundProcess(context.Background(), Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}, "/tmp/project", argv)
	if err != nil || process.PID != 202 || !reflect.DeepEqual(process.Argv, argv) {
		t.Fatalf("process=%+v error=%v", process, err)
	}
}

func TestInspectForegroundProcessRejectsDuplicateGroupLeaders(t *testing.T) {
	value, _ := json.Marshal(map[string]any{
		"id": "cli:pane:process_info", "result": map[string]any{"type": "pane_process_info", "process_info": map[string]any{
			"pane_id": "w1:p2", "foreground_process_group_id": 202,
			"foreground_processes": []any{
				map[string]any{"pid": 202, "name": "codex", "cwd": "/tmp/project", "argv": []string{"codex"}},
				map[string]any{"pid": 202, "name": "other", "cwd": "/tmp/project", "argv": []string{"other"}},
			},
		}},
	})
	host := Host{Runner: &scriptedRunner{outputs: []string{
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false), string(value),
	}}}
	_, err := host.InspectForegroundProcess(context.Background(), Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"})
	if !errors.Is(err, ErrAmbiguousTarget) {
		t.Fatalf("error=%v", err)
	}
}

func TestInspectForegroundProcessRejectsEveryMalformedEnvelopeAndLeaderField(t *testing.T) {
	target := Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}
	validProcess := map[string]any{"pid": 202, "name": "codex", "cwd": "/tmp/project", "argv": []string{"codex"}}
	tests := []struct {
		name, id, kind, pane string
		group                int
		process              map[string]any
	}{
		{"response id", "wrong", "pane_process_info", target.PaneID, 202, validProcess},
		{"result type", "cli:pane:process_info", "wrong", target.PaneID, 202, validProcess},
		{"pane id", "cli:pane:process_info", "pane_process_info", "w1:other", 202, validProcess},
		{"zero group", "cli:pane:process_info", "pane_process_info", target.PaneID, 0, map[string]any{"pid": 0, "name": "codex", "cwd": "/tmp/project", "argv": []string{"codex"}}},
		{"empty name", "cli:pane:process_info", "pane_process_info", target.PaneID, 202, map[string]any{"pid": 202, "name": "", "cwd": "/tmp/project", "argv": []string{"codex"}}},
		{"empty argv", "cli:pane:process_info", "pane_process_info", target.PaneID, 202, map[string]any{"pid": 202, "name": "codex", "cwd": "/tmp/project", "argv": []string{}}},
		{"argv name", "cli:pane:process_info", "pane_process_info", target.PaneID, 202, map[string]any{"pid": 202, "name": "codex", "cwd": "/tmp/project", "argv": []string{"other"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, _ := json.Marshal(map[string]any{"id": test.id, "result": map[string]any{"type": test.kind, "process_info": map[string]any{
				"pane_id": test.pane, "foreground_process_group_id": test.group, "foreground_processes": []any{test.process},
			}}})
			runner := &scriptedRunner{outputs: []string{paneJSON(target.PaneID, target.WorkspaceID, target.TabID, "/tmp/project", false), string(value)}}
			host := Host{Runner: runner}
			if _, err := host.InspectForegroundProcess(context.Background(), target); !errors.Is(err, ErrStaleTarget) {
				t.Fatalf("error=%v", err)
			}
			if len(runner.calls) != 2 {
				t.Fatalf("calls=%v", runner.calls)
			}
		})
	}
}

func TestVerifyForegroundProcessRejectsEachExactContractDrift(t *testing.T) {
	target := Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}
	wantArgv := []string{"codex", "--remote", "unix:///tmp/shared.sock"}
	tests := []ForegroundProcess{
		{PID: 202, Name: "codex", CWD: "/tmp/other", Argv: wantArgv},
		{PID: 202, Name: "other", CWD: "/tmp/project", Argv: []string{"other", "--remote", "unix:///tmp/shared.sock"}},
		{PID: 202, Name: "codex", CWD: "/tmp/project", Argv: []string{"codex", "--remote", "unix:///tmp/other.sock"}},
	}
	for i, process := range tests {
		value, _ := json.Marshal(map[string]any{"id": "cli:pane:process_info", "result": map[string]any{"type": "pane_process_info", "process_info": map[string]any{
			"pane_id": target.PaneID, "foreground_process_group_id": process.PID, "foreground_processes": []ForegroundProcess{process},
		}}})
		host := Host{Runner: &scriptedRunner{outputs: []string{
			paneJSON(target.PaneID, target.WorkspaceID, target.TabID, "/tmp/project", false), string(value), paneJSON(target.PaneID, target.WorkspaceID, target.TabID, "/tmp/project", false),
		}}}
		if _, err := host.VerifyForegroundProcess(context.Background(), target, "/tmp/project", wantArgv); !errors.Is(err, ErrStaleTarget) {
			t.Fatalf("case %d error=%v", i, err)
		}
	}
}

func TestStopForegroundProcessRejectsNonpositivePID(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if err := (Host{}).StopForegroundProcess(context.Background(), Origin{}, "/tmp/project", []string{"codex"}, pid); err == nil || err.Error() != "expected process PID is invalid" {
			t.Fatalf("pid %d error=%v", pid, err)
		}
	}
}

func TestStopForegroundProcessSignalsOnlyExactGroupAndReturnsToShell(t *testing.T) {
	argv := []string{"codex", "resume", "--remote", "unix:///tmp/private.sock", "--cd", "/tmp/project", "thread-original"}
	value, _ := json.Marshal(map[string]any{
		"id": "cli:pane:process_info", "result": map[string]any{"type": "pane_process_info", "process_info": map[string]any{
			"pane_id": "w1:p2", "foreground_process_group_id": 202,
			"foreground_processes": []any{map[string]any{"pid": 202, "name": "codex", "cwd": "/tmp/project", "argv": argv}},
		}},
	})
	runner := &scriptedRunner{outputs: []string{
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false), string(value),
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false),
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false),
		processInfoJSON("w1:p2", "/tmp/project", "zsh", true),
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false),
	}}
	var signalledPID int
	host := Host{Runner: runner, signal: func(pid int, signal syscall.Signal) error {
		if signal != syscall.SIGTERM {
			t.Fatalf("signal = %v", signal)
		}
		signalledPID = pid
		return nil
	}}
	err := host.StopForegroundProcess(context.Background(), Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}, "/tmp/project", argv, 202)
	if err != nil || signalledPID != -202 {
		t.Fatalf("error=%v signalled PID=%d calls=%q", err, signalledPID, runner.calls)
	}
}

func TestStopForegroundProcessRejectsPIDDriftWithoutSignal(t *testing.T) {
	argv := []string{"codex", "--remote", "unix:///tmp/private.sock"}
	value, _ := json.Marshal(map[string]any{
		"id": "cli:pane:process_info", "result": map[string]any{"type": "pane_process_info", "process_info": map[string]any{
			"pane_id": "w1:p2", "foreground_process_group_id": 203,
			"foreground_processes": []any{map[string]any{"pid": 203, "name": "codex", "cwd": "/tmp/project", "argv": argv}},
		}},
	})
	signalled := false
	host := Host{Runner: &scriptedRunner{outputs: []string{
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false), string(value),
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false),
	}}, signal: func(int, syscall.Signal) error { signalled = true; return nil }}
	err := host.StopForegroundProcess(context.Background(), Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}, "/tmp/project", argv, 202)
	if !errors.Is(err, ErrStaleTarget) || signalled {
		t.Fatalf("error=%v signalled=%t", err, signalled)
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
		"",
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
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false), processInfoJSON("w1:p2", "/tmp/project", "zsh", true), "",
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
		paneJSON("w1:p2", "w1", "w1:t1", "/tmp/project", false), processInfoJSON("w1:p2", "/tmp/project", "zsh", true), "",
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

func TestLaunchNonemptyRunOutputCleansOnlyCreatedPane(t *testing.T) {
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
	if !errors.Is(err, ErrStaleTarget) {
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

func TestCommandEncodingRoundTripsAndLeavesShellAlive(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "not-run")
	literal := "$(touch " + marker + ")"
	command, err := encodeCommand([]string{"/usr/bin/printf", "[%s]", literal})
	if err != nil || strings.HasPrefix(command, "exec ") {
		t.Fatalf("command=%q error=%v", command, err)
	}
	for _, shell := range []string{"sh", "bash", "zsh"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		t.Run(filepath.Base(path), func(t *testing.T) {
			output, err := exec.Command(path, "-c", command+`; /usr/bin/printf "[shell-alive]"`).CombinedOutput()
			want := "[" + literal + "][shell-alive]"
			if err != nil || string(output) != want {
				t.Fatalf("output=%q error=%v want=%q", output, err, want)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("argument syntax executed: marker error = %v", err)
			}
		})
	}
}

func TestEncodeCommandExactSizeLimit(t *testing.T) {
	exact, err := encodeCommand([]string{strings.Repeat("x", MaxCommandLen-2)})
	if err != nil || len(exact) != MaxCommandLen {
		t.Fatalf("exact-limit command length = %d, error = %v", len(exact), err)
	}
	if _, err := encodeCommand([]string{strings.Repeat("x", MaxCommandLen-1)}); err == nil {
		t.Fatal("oversized encoded command succeeded")
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
