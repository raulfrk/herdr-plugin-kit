package agenthost

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func TestValidManagedServiceSnapshotRequiresEveryLaunchField(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CodexServiceSnapshot)
	}{
		{"state", func(snapshot *CodexServiceSnapshot) { snapshot.State = CodexServiceDegraded }},
		{"executable", func(snapshot *CodexServiceSnapshot) { snapshot.Executable = "codex" }},
		{"socket", func(snapshot *CodexServiceSnapshot) { snapshot.SocketPath = "shared.sock" }},
		{"CLI version", func(snapshot *CodexServiceSnapshot) { snapshot.CLIVersion = "0.155.0" }},
		{"App Server version", func(snapshot *CodexServiceSnapshot) { snapshot.AppServerVersion = "0.155.0" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := managedServiceSnapshot()
			test.mutate(&snapshot)
			if validManagedServiceSnapshot(snapshot) {
				t.Fatalf("invalid snapshot accepted: %+v", snapshot)
			}
		})
	}
}

func TestAwaitSafeSessionChangeAcceptsStableDonePaneWithoutInput(t *testing.T) {
	source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
	source.Status = "done"
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")},
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")},
	}}
	service := &fakeManagedService{snapshot: managedServiceSnapshot()}
	topology := &fakeManagedTopology{inspected: sessionProcess(source.CWD)}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	launcher.service, launcher.topology = service, topology
	snapshot, err := launcher.AwaitSafeSessionChange(context.Background(), source, 0)
	if err != nil || snapshot.Agent.Status != "done" || snapshot.Process.PID != 77 || snapshot.Service != service.snapshot {
		t.Fatalf("snapshot=%+v error=%v", snapshot, err)
	}
	if service.callCount() != 1 || len(runner.calls) != 4 || hasPaneInput(runner.calls) {
		t.Fatalf("service=%d calls=%#v", service.callCount(), runner.calls)
	}
}

func TestAwaitSafeOverlaySessionChangeAcceptsStableFocusedPaneWithoutInput(t *testing.T) {
	source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, true)
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")},
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")},
	}}
	service := &fakeManagedService{snapshot: managedServiceSnapshot()}
	topology := &fakeManagedTopology{inspected: sessionProcess(source.CWD)}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	launcher.service, launcher.topology = service, topology
	snapshot, err := launcher.AwaitSafeOverlaySessionChange(context.Background(), source, 0)
	if err != nil || !snapshot.Agent.Focused || snapshot.Process.PID != 77 || snapshot.Service != service.snapshot {
		t.Fatalf("snapshot=%+v error=%v", snapshot, err)
	}
	if service.callCount() != 1 || len(runner.calls) != 4 || hasPaneInput(runner.calls) {
		t.Fatalf("service=%d calls=%#v", service.callCount(), runner.calls)
	}
}

func TestAwaitSafeOverlaySessionChangeRejectsDraftWithoutInput(t *testing.T) {
	source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, true)
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, source), {Stdout: []byte("answer\n› user draft\n status")},
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	launcher.service = &fakeManagedService{snapshot: managedServiceSnapshot()}
	launcher.topology = &fakeManagedTopology{inspected: sessionProcess(source.CWD)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := launcher.AwaitSafeOverlaySessionChange(ctx, source, 0); !errors.Is(err, ErrManagedPaneBusy) || hasPaneInput(runner.calls) {
		t.Fatalf("error=%v calls=%#v", err, runner.calls)
	}
}

func TestAwaitSafeOverlaySessionChangeRejectsUnfocusedPaneWithoutInput(t *testing.T) {
	source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
	runner := &scriptedRunner{results: []command.Result{managedAgentResult(t, source)}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	launcher.service = &fakeManagedService{snapshot: managedServiceSnapshot()}
	launcher.topology = &fakeManagedTopology{inspected: sessionProcess(source.CWD)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := launcher.AwaitSafeOverlaySessionChange(ctx, source, 0); !errors.Is(err, ErrManagedPaneBusy) || hasPaneInput(runner.calls) {
		t.Fatalf("error=%v calls=%#v", err, runner.calls)
	}
}

func TestAwaitSafeOverlaySessionChangeRejectsFocusDriftWithoutInput(t *testing.T) {
	source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, true)
	unfocused := source
	unfocused.Focused = false
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")},
		managedAgentResult(t, unfocused),
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	launcher.service = &fakeManagedService{snapshot: managedServiceSnapshot()}
	launcher.topology = &fakeManagedTopology{inspected: sessionProcess(source.CWD)}
	if _, err := launcher.AwaitSafeOverlaySessionChange(context.Background(), source, 0); !errors.Is(err, ErrManagedStale) || hasPaneInput(runner.calls) {
		t.Fatalf("error=%v calls=%#v", err, runner.calls)
	}
}

func TestAwaitSafeSessionChangeWaitsForDraftToClear(t *testing.T) {
	source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, source), {Stdout: []byte("answer\n› first draft line\n  second line\n status")},
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")},
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")},
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	launcher.service = &fakeManagedService{snapshot: managedServiceSnapshot()}
	launcher.topology = &fakeManagedTopology{inspected: sessionProcess(source.CWD)}
	if _, err := launcher.AwaitSafeSessionChange(context.Background(), source, 0); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 6 || hasPaneInput(runner.calls) {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestAwaitSafeSessionChangeCancellationNeverSendsInput(t *testing.T) {
	for _, state := range []string{"focused", "working", "pending", "draft"} {
		t.Run(state, func(t *testing.T) {
			source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, state == "focused")
			wire := wireFromAgent(source)
			if state == "working" {
				wire.AgentStatus = "working"
			}
			if state == "pending" {
				wire.LaunchPending = true
			}
			results := []command.Result{jsonResult(t, map[string]any{"id": "cli:agent:get", "result": map[string]any{"type": "agent_info", "agent": wire}})}
			if state == "draft" {
				results = append(results, command.Result{Stdout: []byte("answer\n› user draft\n status")})
			}
			runner := &scriptedRunner{results: results}
			launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
			launcher.service = &fakeManagedService{snapshot: managedServiceSnapshot()}
			launcher.topology = &fakeManagedTopology{inspected: sessionProcess(source.CWD)}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := launcher.AwaitSafeSessionChange(ctx, source, 0)
			if !errors.Is(err, ErrManagedPaneBusy) || hasPaneInput(runner.calls) {
				t.Fatalf("error=%v calls=%#v", err, runner.calls)
			}
		})
	}
}

func TestStopForSessionChangeStopsOnlyStableExactProcess(t *testing.T) {
	source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")},
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")},
	}}
	topology := &fakeManagedTopology{inspected: sessionProcess(source.CWD)}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	launcher.service = &fakeManagedService{snapshot: managedServiceSnapshot()}
	launcher.topology = topology
	snapshot, err := launcher.StopForSessionChange(context.Background(), source, 0)
	if err != nil || len(topology.stops) != 1 || topology.stops[0].PID != snapshot.Process.PID || hasPaneInput(runner.calls) {
		t.Fatalf("snapshot=%+v error=%v stops=%+v calls=%#v", snapshot, err, topology.stops, runner.calls)
	}
}

func TestStopForSessionChangeRefusesVisibleComposerBeforeStop(t *testing.T) {
	source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
	source.Status = "done"
	clear := command.Result{Stdout: managedEmptyComposer("answer")}
	draft := command.Result{Stdout: []byte("answer\n› unsent draft\n status")}
	for _, test := range []struct {
		name    string
		results []command.Result
	}{
		{"first read", []command.Result{managedAgentResult(t, source), draft}},
		{"final read", []command.Result{managedAgentResult(t, source), clear, managedAgentResult(t, source), draft}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &scriptedRunner{results: test.results}
			topology := &fakeManagedTopology{inspected: sessionProcess(source.CWD)}
			launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
			launcher.service = &fakeManagedService{snapshot: managedServiceSnapshot()}
			launcher.topology = topology
			_, err := launcher.StopForSessionChange(context.Background(), source, 0)
			if !errors.Is(err, ErrCodexConversionComposer) || !errors.Is(err, ErrManagedPaneBusy) || len(topology.stops) != 0 || hasPaneInput(runner.calls) || len(runner.results) != 0 {
				t.Fatalf("error=%v stops=%+v calls=%+v unused=%+v", err, topology.stops, runner.calls, runner.results)
			}
		})
	}
}

func TestAwaitSafeSessionChangeRejectsEachStableReadDriftWithoutInput(t *testing.T) {
	source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
	process := sessionProcess(source.CWD)
	otherSession := source
	otherSession.Session = &SessionRef{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: managedSecondID}
	changedStatus := source
	changedStatus.Status = "done"
	changedIdentity := source
	changedIdentity.TabID = "w:other"
	tests := []struct {
		name     string
		results  []command.Result
		topology *fakeManagedTopology
	}{
		{"immutable agent", []command.Result{managedAgentResult(t, otherSession)}, &fakeManagedTopology{inspected: process}},
		{"invalid process", []command.Result{managedAgentResult(t, source)}, &fakeManagedTopology{inspected: sessionhost.ForegroundProcess{PID: 77, Name: "other", CWD: source.CWD, Argv: []string{"other"}}}},
		{"second identity", []command.Result{managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")}, managedAgentResult(t, changedIdentity), {Stdout: managedEmptyComposer("answer")}}, &fakeManagedTopology{inspected: process}},
		{"second status", []command.Result{managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")}, managedAgentResult(t, changedStatus), {Stdout: managedEmptyComposer("answer")}}, &fakeManagedTopology{inspected: process}},
		{"second process", []command.Result{managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")}, managedAgentResult(t, source)}, &fakeManagedTopology{inspectedSeq: []sessionhost.ForegroundProcess{process, {PID: 88, Name: process.Name, CWD: process.CWD, Argv: process.Argv}}}},
		{"second composer", []command.Result{managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")}, managedAgentResult(t, source), {Stdout: []byte("\x1b[1m›\x1b[0m user draft")}}, &fakeManagedTopology{inspected: process}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &scriptedRunner{results: test.results}
			launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
			launcher.topology = test.topology
			if _, err := launcher.awaitSafeSessionChange(context.Background(), source, managedServiceSnapshot()); err == nil || hasPaneInput(runner.calls) {
				t.Fatalf("error=%v calls=%v", err, runner.calls)
			}
		})
	}
}

func TestAwaitSafeSessionChangeRejectsSecondForegroundProcessReplacement(t *testing.T) {
	source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
	process := sessionProcess(source.CWD)
	replacement := process
	replacement.PID++
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")},
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")},
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	launcher.topology = &fakeManagedTopology{inspectedSeq: []sessionhost.ForegroundProcess{process, replacement}}
	if _, err := launcher.awaitSafeSessionChange(context.Background(), source, managedServiceSnapshot()); !errors.Is(err, ErrManagedStale) {
		t.Fatalf("replacement process error=%v", err)
	}
}

func TestValidateSafeSourceRejectsCrossSessionSource(t *testing.T) {
	source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
	source.SessionName = "other"
	launcher := NewManagedCodexLauncher(Host{Runner: &scriptedRunner{}, Session: "named"})
	if err := launcher.validateSafeSource(source, 0); !errors.Is(err, ErrManagedUnsupported) {
		t.Fatalf("cross-session source error=%v", err)
	}
}

func TestManagedCodexGuardSafeSourceBoundsWorkingDirectory(t *testing.T) {
	longest := "/" + strings.Repeat("a", maxPathBytes-1)
	for _, test := range []struct {
		name string
		cwd  string
		want error
	}{
		{"longest allowed path", longest, nil},
		{"one byte too long", longest + "a", ErrManagedUnsupported},
		{"relative", "tmp/project", ErrManagedUnsupported},
		{"unclean", "/tmp/project/../project", ErrManagedUnsupported},
		{"NUL byte", "/tmp/pro\x00ject", ErrManagedUnsupported},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := managedAgent(managedOrigin(), test.cwd, managedSourceID, false)
			launcher := NewManagedCodexLauncher(Host{Runner: &scriptedRunner{}, Session: "named"})
			if err := launcher.validateSafeSource(source, 0); !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}
}

func TestManagedCodexGuardSafeSourceBoundsReadyTimeout(t *testing.T) {
	for _, test := range []struct {
		name    string
		timeout time.Duration
		want    error
	}{
		{"default", 0, nil},
		{"negative", -time.Second, ErrManagedUnsupported},
		{"shorter than three seconds", time.Second, ErrManagedUnsupported},
		{"exactly three seconds", 3 * time.Second, ErrManagedUnsupported},
		{"just over three seconds", 3*time.Second + time.Nanosecond, nil},
		{"maximum", MaxManagedReadyTimeout, nil},
		{"just over the maximum", MaxManagedReadyTimeout + time.Nanosecond, ErrManagedUnsupported},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
			launcher := NewManagedCodexLauncher(Host{Runner: &scriptedRunner{}, Session: "named"})
			if err := launcher.validateSafeSource(source, test.timeout); !errors.Is(err, test.want) {
				t.Fatalf("timeout=%v error=%v want=%v", test.timeout, err, test.want)
			}
		})
	}
}

func sessionProcess(cwd string) sessionhost.ForegroundProcess {
	return sessionhost.ForegroundProcess{PID: 77, Name: "codex", CWD: cwd, Argv: []string{"codex", "--remote", "unix:///tmp/shared.sock"}}
}

func hasPaneInput(calls [][]string) bool {
	for _, call := range calls {
		for _, arg := range call {
			if arg == "send-text" || arg == "send-keys" || arg == "run" {
				return true
			}
		}
	}
	return false
}
