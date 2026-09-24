package agenthost

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

type fakeTopology struct {
	created       sessionhost.CreatedTarget
	launchErr     error
	cleanupErr    error
	requests      []sessionhost.LaunchRequest
	cleanupCalls  []sessionhost.CreatedTarget
	cleanupCtx    context.Context
	cleanupCtxErr error
}

func (f *fakeTopology) Launch(_ context.Context, request sessionhost.LaunchRequest) (sessionhost.CreatedTarget, error) {
	f.requests = append(f.requests, request)
	return f.created, f.launchErr
}

func (f *fakeTopology) Cleanup(ctx context.Context, target sessionhost.CreatedTarget) error {
	f.cleanupCtx = ctx
	f.cleanupCtxErr = ctx.Err()
	f.cleanupCalls = append(f.cleanupCalls, target)
	return f.cleanupErr
}

func forkAgent(kind, pane, sessionValue, cwd string) Agent {
	sessionKind := "id"
	if kind == "omp" {
		sessionKind = "path"
	}
	return Agent{
		SessionName: "named", Kind: kind, Status: "idle", CWD: cwd,
		WorkspaceID: "w1", TabID: "w1:t1", PaneID: pane,
		Session: &SessionRef{Source: "herdr:" + kind, Agent: kind, Kind: sessionKind, Value: sessionValue},
	}
}

func forkWire(agent Agent) wireAgent {
	return wireAgent{
		Agent: agent.Kind, AgentStatus: agent.Status, CWD: agent.CWD, Focused: agent.Focused,
		WorkspaceID: agent.WorkspaceID, TabID: agent.TabID, PaneID: agent.PaneID,
		AgentSession: &wireSession{Source: agent.Session.Source, Agent: agent.Session.Agent, Kind: agent.Session.Kind, Value: agent.Session.Value},
	}
}

func TestForkCodexPreservesSourceAndCreatesDistinctBackgroundChild(t *testing.T) {
	source := forkAgent("codex", "w1:p1", "thread-source", "/tmp/project")
	pending := forkAgent("codex", "w1:p2", "thread-source", "/tmp/project")
	child := forkAgent("codex", "w1:p2", "thread-child", "/tmp/project")
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}}
	topology := &fakeTopology{created: created}
	runner := &scriptedRunner{results: []command.Result{listForkResult(t, source), listForkResult(t, pending), listForkResult(t, child), listForkResult(t, source)}}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher = topology
	forker.wait = func(context.Context, time.Duration) error { return nil }
	result, err := forker.Fork(context.Background(), ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "/opt/bin/codex", Options: []string{"--model", "gpt-5", "-c", `model="gpt-5"`, "--config", `model_reasoning_effort="high"`}},
		Placement: sessionhost.NewPane, Direction: "right",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Source.Session.Value != "thread-source" || result.Child.Session.Value != "thread-child" || result.Child.PaneID == result.Source.PaneID {
		t.Fatalf("result = %+v", result)
	}
	if len(topology.requests) != 1 {
		t.Fatalf("requests = %+v", topology.requests)
	}
	request := topology.requests[0]
	wantArgv := []string{"/opt/bin/codex", "--model", "gpt-5", "-c", `model="gpt-5"`, "--config", `model_reasoning_effort="high"`, "-C", "/tmp/project", "fork", "thread-source"}
	if !reflect.DeepEqual(request.Argv, wantArgv) || request.Focus || request.Origin.SessionName != "named" {
		t.Fatalf("launch request = %+v", request)
	}
	if len(topology.cleanupCalls) != 0 {
		t.Fatalf("unexpected cleanup = %+v", topology.cleanupCalls)
	}
}

func TestForkOpenCodeAddsOwnedLoopbackEndpointAndPreservesSource(t *testing.T) {
	source := forkAgent("opencode", "w1:p1", "ses_source", "/tmp/project")
	child := forkAgent("opencode", "w1:p2", "ses_child", "/tmp/project")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.Method != http.MethodPost || request.URL.Path != "/session/ses_source/fork" || request.URL.Query().Get("directory") != source.CWD {
			t.Errorf("unexpected OpenCode fork request: %s %s", request.Method, request.URL)
		}
		_, _ = response.Write([]byte(`{"id":"ses_child","directory":"/tmp/project","parentID":null}`))
	}))
	defer server.Close()
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: child.PaneID}}
	topology := &fakeTopology{created: created}
	runner := &scriptedRunner{results: []command.Result{listForkResult(t, source), listForkResult(t, source), listForkResult(t, child), listForkResult(t, source)}}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher = topology
	forker.wait = func(context.Context, time.Duration) error { return nil }
	forker.openCodeTUIReserve = fixedOpenCodeTUIReserve(49154)

	result, err := forker.Fork(context.Background(), ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "opencode", Options: []string{"--mini"}}, Placement: sessionhost.NewPane, Direction: "right",
		OpenCodeServer: OpenCodeServer{URL: server.URL, Directory: source.CWD},
	})
	if err != nil || !sameImmutable(result.Source, source) || !sameImmutable(result.Child, child) || result.Created != created {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	want := []string{"opencode", "/tmp/project", "--mini", "--session", "ses_child", "--hostname", "127.0.0.1", "--port", "49154"}
	if requests != 1 || len(topology.requests) != 1 || !reflect.DeepEqual(topology.requests[0].Argv, want) || len(topology.cleanupCalls) != 0 {
		t.Fatalf("API requests=%d launch=%+v cleanup=%+v", requests, topology.requests, topology.cleanupCalls)
	}
}

func TestForkOpenCodePostNativeFailuresRemainUncertainWithChildIdentity(t *testing.T) {
	source := forkAgent("opencode", "w1:p1", "ses_source", "/tmp/project")
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}}
	for _, test := range []struct {
		name        string
		observed    Agent
		launchErr   error
		wantCleanup int
	}{
		{name: "launch failed after API fork", launchErr: errors.New("launch failed")},
		{name: "wrong Herdr child identity", observed: forkAgent("opencode", "w1:p2", "ses_other", "/tmp/project"), wantCleanup: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				requests++
				if request.Method != http.MethodPost {
					t.Errorf("unexpected native API method: %s", request.Method)
				}
				_, _ = response.Write([]byte(`{"id":"ses_child","directory":"/tmp/project"}`))
			}))
			defer server.Close()
			topology := &fakeTopology{created: created, launchErr: test.launchErr}
			results := []command.Result{listForkResult(t, source), listForkResult(t, source)}
			if test.observed.PaneID != "" {
				results = append(results, listForkResult(t, test.observed))
			}
			forker := NewForker(Host{Runner: &scriptedRunner{results: results}, Session: "named"})
			forker.launcher = topology
			forker.wait = func(context.Context, time.Duration) error { return nil }
			forker.openCodeTUIReserve = fixedOpenCodeTUIReserve(49154)
			_, err := forker.Fork(context.Background(), ForkRequest{
				Source: source, Profile: ForkProfile{Executable: "opencode"}, Placement: sessionhost.NewPane, Direction: "right",
				OpenCodeServer: OpenCodeServer{URL: server.URL, Directory: source.CWD},
			})
			if !errors.Is(err, ErrForkOutcomeUncertain) || !strings.Contains(err.Error(), "ses_child") || requests != 1 || len(topology.cleanupCalls) != test.wantCleanup {
				t.Fatalf("error=%v requests=%d cleanup=%v", err, requests, topology.cleanupCalls)
			}
		})
	}
}

func TestForkOpenCodeSourceDriftAfterNativeForkNeverLaunches(t *testing.T) {
	source := forkAgent("opencode", "w1:p1", "ses_source", "/tmp/project")
	drifted := forkAgent("opencode", "w1:p1", "ses_other", "/tmp/project")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(`{"id":"ses_child","directory":"/tmp/project"}`))
	}))
	defer server.Close()
	topology := &fakeTopology{}
	forker := NewForker(Host{Runner: &scriptedRunner{results: []command.Result{listForkResult(t, source), listForkResult(t, drifted)}}, Session: "named"})
	forker.launcher = topology
	forker.openCodeTUIReserve = fixedOpenCodeTUIReserve(49154)
	_, err := forker.Fork(context.Background(), ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "opencode"}, Placement: sessionhost.NewPane, Direction: "right",
		OpenCodeServer: OpenCodeServer{URL: server.URL, Directory: source.CWD},
	})
	if !errors.Is(err, ErrForkOutcomeUncertain) || !strings.Contains(err.Error(), "ses_child") || len(topology.requests) != 0 {
		t.Fatalf("error=%v launches=%v", err, topology.requests)
	}
}

func TestForkOpenCodeInvalidLaunchNeverCreatesNativeChild(t *testing.T) {
	source := forkAgent("opencode", "w1:p1", "ses_source", "/tmp/project")
	for _, test := range []struct {
		name      string
		placement sessionhost.Placement
		direction string
		tabLabel  string
	}{
		{name: "missing pane direction", placement: sessionhost.NewPane},
		{name: "direction on new tab", placement: sessionhost.NewTab, direction: "right"},
		{name: "oversized tab label", placement: sessionhost.NewTab, tabLabel: strings.Repeat("x", 257)},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				requests++
				_, _ = response.Write([]byte(`{"id":"ses_child","directory":"/tmp/project"}`))
			}))
			defer server.Close()
			topology := &fakeTopology{}
			forker := NewForker(Host{Runner: &scriptedRunner{results: []command.Result{listForkResult(t, source)}}, Session: "named"})
			forker.launcher = topology
			forker.openCodeTUIReserve = fixedOpenCodeTUIReserve(49154)
			_, err := forker.Fork(context.Background(), ForkRequest{
				Source: source, Profile: ForkProfile{Executable: "opencode"}, Placement: test.placement,
				Direction: test.direction, TabLabel: test.tabLabel,
				OpenCodeServer: OpenCodeServer{URL: server.URL, Directory: source.CWD},
			})
			if err == nil || errors.Is(err, ErrForkOutcomeUncertain) || requests != 0 || len(topology.requests) != 0 {
				t.Fatalf("error=%v API requests=%d launches=%v", err, requests, topology.requests)
			}
		})
	}
}

func TestForkOpenCodeTUIReservationFailureCreatesNothing(t *testing.T) {
	source := forkAgent("opencode", "w1:p1", "ses_source", "/tmp/project")
	topology := &fakeTopology{}
	forker := NewForker(Host{Runner: &scriptedRunner{results: []command.Result{listForkResult(t, source)}}, Session: "named"})
	forker.launcher = topology
	forker.openCodeTUIReserve = func() (int, error) { return 0, errors.New("unavailable") }
	_, err := forker.Fork(context.Background(), ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "opencode"}, Placement: sessionhost.NewTab,
	})
	if err == nil || len(topology.requests) != 0 || len(topology.cleanupCalls) != 0 {
		t.Fatalf("launch=%+v cleanup=%+v error=%v", topology.requests, topology.cleanupCalls, err)
	}
}

func listForkResult(t *testing.T, agents ...Agent) command.Result {
	t.Helper()
	wires := make([]wireAgent, len(agents))
	for i := range agents {
		wires[i] = forkWire(agents[i])
	}
	return jsonResult(t, listResponse(wires...))
}

func TestForkComposesSupportedNativeArgv(t *testing.T) {
	tests := []struct {
		kind, source, executable, socket string
		options                          []string
		want                             []string
	}{
		{"codex", "thread-1", "codex", "", []string{"--sandbox=workspace-write", "--search"}, []string{"codex", "--sandbox=workspace-write", "--search", "-C", "/tmp/project", "fork", "thread-1"}},
		{"omp", "/tmp/source.jsonl", "/opt/omp", "/tmp/status.sock", []string{"--model", "openai/gpt", "--thinking=max"}, []string{"/opt/omp", "--model", "openai/gpt", "--thinking=max", "--cwd", "/tmp/project", "--fork", "/tmp/source.jsonl"}},
		{"opencode", "session-1", "opencode", "", []string{"--model=provider/model", "--mini"}, []string{"opencode", "/tmp/project", "--model=provider/model", "--mini"}},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			source := forkAgent(tc.kind, "w1:p1", tc.source, "/tmp/project")
			argv, kind, err := composeForkArgv(ForkRequest{Source: source, Profile: ForkProfile{Executable: tc.executable, Options: tc.options}, OMPStatusSocket: tc.socket})
			if err != nil || kind != tc.kind || !reflect.DeepEqual(argv, tc.want) {
				t.Fatalf("argv=%q kind=%q error=%v, want %q", argv, kind, err, tc.want)
			}
		})
	}
}

func TestComposeForkArgvRejectsRelativeOMPSourceSession(t *testing.T) {
	source := forkAgent("omp", "w1:p1", "relative-session.jsonl", "/tmp/project")
	if _, _, err := composeForkArgv(ForkRequest{Source: source, Profile: ForkProfile{Executable: "omp"}}); !errors.Is(err, ErrUnsupportedFork) {
		t.Fatalf("relative OMP source error=%v", err)
	}
}

func TestForkOptionMatricesRejectOwnedAndAmbiguousArguments(t *testing.T) {
	tests := []struct {
		kind, value, executable, socket string
		options                         []string
	}{
		{"codex", "thread", "codex", "", []string{"fork"}},
		{"codex", "thread", "codex", "", []string{"-C", "/other"}},
		{"codex", "thread", "codex", "", []string{"--remote", "unix:///tmp/server"}},
		{"codex", "thread", "codex", "", []string{"-c", "shell_environment_policy.inherit=all"}},
		{"codex", "thread", "codex", "", []string{"--model", "one", "--model", "two"}},
		{"codex", "thread", "codex", "", []string{"-m", "one", "--model", "two"}},
		{"omp", "/tmp/source.jsonl", "omp", "/tmp/status", []string{"--fork", "/tmp/other"}},
		{"omp", "/tmp/source.jsonl", "omp", "/tmp/status", []string{"--resume", "id"}},
		{"omp", "/tmp/source.jsonl", "omp", "/tmp/status", []string{"--session-dir=/tmp/other"}},
		{"omp", "/tmp/source.jsonl", "omp", "/tmp/status", []string{"--no-session"}},
		{"opencode", "session", "opencode", "", []string{"/other/project"}},
		{"opencode", "session", "opencode", "", []string{"--session", "other"}},
		{"opencode", "session", "opencode", "", []string{"--continue"}},
		{"opencode", "session", "opencode", "", []string{"--fork"}},
		{"opencode", "session", "opencode", "", []string{"--prompt", "unexpected"}},
	}
	for _, tc := range tests {
		t.Run(tc.kind+strings.Join(tc.options, "_"), func(t *testing.T) {
			source := forkAgent(tc.kind, "w1:p1", tc.value, "/tmp/project")
			if _, _, err := composeForkArgv(ForkRequest{Source: source, Profile: ForkProfile{Executable: tc.executable, Options: tc.options}, OMPStatusSocket: tc.socket}); err == nil {
				t.Fatalf("options %q accepted", tc.options)
			}
		})
	}
}

func TestForkRejectsUnsupportedOrStaleSourceBeforeLaunch(t *testing.T) {
	for _, source := range []Agent{
		forkAgent("claude", "w1:p1", "session", "/tmp/project"),
		forkAgent("codex", "w1:p1", "thread", "relative"),
		forkAgent("omp", "w1:p1", "relative.jsonl", "/tmp/project"),
	} {
		topology := &fakeTopology{}
		forker := NewForker(Host{Runner: &scriptedRunner{}, Session: "named"})
		forker.launcher = topology
		_, err := forker.Fork(context.Background(), ForkRequest{Source: source, Profile: ForkProfile{Executable: source.Kind}, Placement: sessionhost.NewTab})
		if err == nil || len(topology.requests) != 0 {
			t.Fatalf("source accepted: %+v, error=%v", source, err)
		}
	}
}

func TestForkRejectsInvalidReadyTimeoutBeforeLaunch(t *testing.T) {
	source := forkAgent("codex", "w1:p1", "thread", "/tmp/project")
	for _, timeout := range []time.Duration{-time.Nanosecond, MaxForkReadyTimeout + time.Nanosecond} {
		topology := &fakeTopology{}
		forker := NewForker(Host{Runner: &scriptedRunner{}, Session: "named"})
		forker.launcher = topology
		if _, err := forker.Fork(context.Background(), ForkRequest{Source: source, Profile: ForkProfile{Executable: "codex"}, Placement: sessionhost.NewTab, ReadyTimeout: timeout}); err == nil || len(topology.requests) != 0 {
			t.Fatalf("timeout %v accepted", timeout)
		}
	}
}

func TestForkRejectsWrongHarnessChildAndCleansCreatedPane(t *testing.T) {
	source := forkAgent("codex", "w1:p1", "thread-source", "/tmp/project")
	child := forkAgent("opencode", "w1:p2", "thread-child", "/tmp/project")
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}}
	topology := &fakeTopology{created: created}
	runner := &scriptedRunner{results: []command.Result{listForkResult(t, source), listForkResult(t, child)}}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher = topology
	forker.wait = func(context.Context, time.Duration) error { return nil }
	_, err := forker.Fork(context.Background(), ForkRequest{Source: source, Profile: ForkProfile{Executable: "codex"}, Placement: sessionhost.NewPane, Direction: "right"})
	if !errors.Is(err, ErrStaleReport) || !reflect.DeepEqual(topology.cleanupCalls, []sessionhost.CreatedTarget{created}) {
		t.Fatalf("error=%v cleanup=%+v", err, topology.cleanupCalls)
	}
}

func TestForkCancellationUsesUncancelledCleanupContext(t *testing.T) {
	source := forkAgent("codex", "w1:p1", "thread-source", "/tmp/project")
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}}
	topology := &fakeTopology{created: created}
	runner := &scriptedRunner{results: []command.Result{listForkResult(t, source), listForkResult(t)}}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher = topology
	forker.wait = func(ctx context.Context, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}
	forker.wait = func(context.Context, time.Duration) error { return context.Canceled }
	_, err := forker.Fork(context.Background(), ForkRequest{Source: source, Profile: ForkProfile{Executable: "codex"}, Placement: sessionhost.NewPane, Direction: "right", ReadyTimeout: time.Millisecond})
	if err == nil || len(topology.cleanupCalls) != 1 || topology.cleanupCtxErr != nil {
		t.Fatalf("error=%v cleanup=%+v cleanup context err=%v", err, topology.cleanupCalls, topology.cleanupCtxErr)
	}
}

func TestForkRejectsFocusedChildCWDDriftAndCleansCreatedPane(t *testing.T) {
	source := forkAgent("codex", "w1:p1", "thread-source", "/tmp/project")
	child := forkAgent("codex", "w1:p2", "thread-child", "/tmp/project")
	focusedDrift := child
	focusedDrift.Focused = true
	focusedDrift.CWD = "/tmp/other"
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}}
	topology := &fakeTopology{created: created}
	runner := &scriptedRunner{results: []command.Result{
		listForkResult(t, source), listForkResult(t, child), listForkResult(t, source),
		jsonResult(t, infoResponse(forkWire(focusedDrift))),
	}}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher = topology
	forker.wait = func(context.Context, time.Duration) error { return nil }
	_, err := forker.Fork(context.Background(), ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "codex"},
		Placement: sessionhost.NewPane, Direction: "right", Focus: true,
	})
	if !errors.Is(err, ErrStaleReport) || !reflect.DeepEqual(topology.cleanupCalls, []sessionhost.CreatedTarget{created}) {
		t.Fatalf("error=%v cleanup=%+v", err, topology.cleanupCalls)
	}
}

func TestForkRechecksSourceAfterSuccessfulFocus(t *testing.T) {
	for _, state := range []string{"unchanged", "changed", "missing"} {
		t.Run(state, func(t *testing.T) {
			source := forkAgent("codex", "w1:p1", "thread-source", "/tmp/project")
			child := forkAgent("codex", "w1:p2", "thread-child", "/tmp/project")
			child.Focused = true
			created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}}
			after := source
			if state == "changed" {
				after.CWD = "/other"
			}
			last := listForkResult(t, after)
			if state == "missing" {
				last = listForkResult(t)
			}
			runner := &scriptedRunner{results: []command.Result{listForkResult(t, source), listForkResult(t, child), listForkResult(t, source), jsonResult(t, infoResponse(forkWire(child))), last}}
			topology := &fakeTopology{created: created}
			forker := NewForker(Host{Runner: runner, Session: "named"})
			forker.launcher = topology
			result, err := forker.Fork(context.Background(), ForkRequest{Source: source, Profile: ForkProfile{Executable: "codex"}, Placement: sessionhost.NewPane, Direction: "right", Focus: true})
			if state == "unchanged" {
				if err != nil || !result.Child.Focused || len(topology.cleanupCalls) != 0 {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			} else if !errors.Is(err, ErrStaleReport) || len(topology.cleanupCalls) != 1 {
				t.Fatalf("err=%v cleanup=%v", err, topology.cleanupCalls)
			}
		})
	}
}

func TestForkReadinessDurationContract(t *testing.T) {
	if readyTimeout(0) != 30*time.Second || readyTimeout(time.Minute) != time.Minute || MaxForkReadyTimeout != 300*time.Second {
		t.Fatal("fork readiness duration contract changed")
	}
}
