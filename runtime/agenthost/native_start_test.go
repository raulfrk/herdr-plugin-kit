package agenthost

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func TestNativeStarterCreatesFreshBackgroundIdentityInExactSession(t *testing.T) {
	for _, session := range []string{"", "named"} {
		t.Run(sessionLabel(session), func(t *testing.T) {
			created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: session, WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}, CreatedTab: true, DirectArgv: true}
			prior := forkAgent("omp", "w1:p0", "/tmp/prior.jsonl", "/tmp/project")
			prior.SessionName = session
			child := forkAgent("omp", created.PaneID, "/tmp/new.jsonl", "/tmp/project")
			child.SessionName, child.TabID = session, created.TabID
			topology := &fakeTopology{created: created}
			runner := &scriptedRunner{results: []command.Result{listForkResult(t, prior), listForkResult(t, child), listForkResult(t, child)}}
			starter := NewNativeStarter(Host{Runner: runner, Session: session})
			starter.launcher = topology
			starter.wait = func(context.Context, time.Duration) error { return nil }
			request := NativeStartRequest{
				Origin: sessionhost.Origin{SessionName: session, WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
				Argv:   []string{"omp", "--model", "openai/gpt"}, Directory: "/tmp/project",
				Placement: sessionhost.NewTab, TabLabel: "fresh", OMPStatusSocket: "/tmp/omp.sock",
			}
			result, err := starter.Start(context.Background(), request)
			if err != nil || !sameImmutable(result.Agent, child) || result.Created != created || len(topology.cleanupCalls) != 0 || len(topology.requests) != 1 {
				t.Fatalf("result=%+v launches=%+v cleanup=%+v error=%v", result, topology.requests, topology.cleanupCalls, err)
			}
			launched := topology.requests[0]
			if !reflect.DeepEqual(launched.Argv, request.Argv) || launched.Origin.SessionName != session || launched.Focus || launched.OMPStatusSocket != request.OMPStatusSocket {
				t.Fatalf("launch=%+v", launched)
			}
		})
	}
}

func TestNativeStarterIgnoresBaselineAgentWithoutSessionIdentity(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "p2"}}
	pending := forkWire(forkAgent("omp", "p0", "/tmp/pending.jsonl", "/tmp/project"))
	pending.AgentSession = nil
	child := forkAgent("omp", created.PaneID, "/tmp/new.jsonl", "/tmp/project")
	child.TabID = created.TabID
	topology := &fakeTopology{created: created}
	starter := NewNativeStarter(Host{Runner: &scriptedRunner{results: []command.Result{jsonResult(t, listResponse(pending)), listForkResult(t, child), listForkResult(t, child)}}, Session: "named"})
	starter.launcher = topology
	starter.wait = func(context.Context, time.Duration) error { return nil }
	result, err := starter.Start(context.Background(), NativeStartRequest{
		Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "p1"},
		Argv:   []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewTab,
	})
	if err != nil || !sameImmutable(result.Agent, child) {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestNativeStarterRejectsUnsupportedAndOwnedSelectorsBeforeObservation(t *testing.T) {
	tests := []NativeStartRequest{
		{Argv: []string{"codex"}, Directory: "/tmp/project", Placement: sessionhost.NewTab},
		{Argv: []string{"omp", "--resume", "/tmp/prior.jsonl"}, Directory: "/tmp/project", Placement: sessionhost.NewTab},
		{Argv: []string{"omp", "--session-dir", "/tmp/sessions"}, Directory: "/tmp/project", Placement: sessionhost.NewTab},
		{Argv: []string{"opencode", "--session", "ses_prior"}, Directory: "/tmp/project", Placement: sessionhost.NewTab},
		{Argv: []string{"opencode", "--fork"}, Directory: "/tmp/project", Placement: sessionhost.NewTab},
		{Argv: []string{"unsupported"}, Directory: "/tmp/project", Placement: sessionhost.NewTab},
		{Argv: []string{"opencode"}, Directory: "relative", Placement: sessionhost.NewTab},
		{Argv: []string{"opencode"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, OMPStatusSocket: "/tmp/omp.sock"},
	}
	for _, request := range tests {
		runner := &scriptedRunner{}
		topology := &fakeTopology{}
		starter := NewNativeStarter(Host{Runner: runner})
		starter.launcher = topology
		_, err := starter.Start(context.Background(), request)
		if err == nil || len(runner.calls) != 0 || len(topology.requests) != 0 {
			t.Fatalf("request=%+v calls=%v launches=%v error=%v", request, runner.calls, topology.requests, err)
		}
	}
}

func TestNativeStarterRejectsReusedOrDriftedIdentityAndCleansOnlyCreatedTarget(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	prior := forkAgent("omp", "w1:p0", "/tmp/prior.jsonl", "/tmp/project")
	reused := forkAgent("omp", created.PaneID, "/tmp/prior.jsonl", "/tmp/project")
	reused.TabID = created.TabID
	fresh := forkAgent("omp", created.PaneID, "/tmp/new.jsonl", "/tmp/project")
	fresh.TabID = created.TabID
	for _, test := range []struct {
		name       string
		results    []command.Result
		cleanupErr error
		wantTarget bool
	}{
		{name: "reused", results: []command.Result{listForkResult(t, prior), listForkResult(t, reused)}},
		{name: "cleanup failure retains target", results: []command.Result{listForkResult(t, prior), listForkResult(t, reused)}, cleanupErr: errors.New("close failed"), wantTarget: true},
		{name: "stable read drift", results: []command.Result{listForkResult(t, prior), listForkResult(t, fresh), listForkResult(t, reused)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			topology := &fakeTopology{created: created, cleanupErr: test.cleanupErr}
			starter := NewNativeStarter(Host{Runner: &scriptedRunner{results: test.results}, Session: "named"})
			starter.launcher = topology
			starter.wait = func(context.Context, time.Duration) error { return nil }
			result, err := starter.Start(context.Background(), NativeStartRequest{
				Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
				Argv:   []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewTab,
			})
			if !errors.Is(err, ErrStaleReport) || !reflect.DeepEqual(topology.cleanupCalls, []sessionhost.CreatedTarget{created}) || (result.Created.PaneID != "") != test.wantTarget {
				t.Fatalf("result=%+v cleanup=%+v error=%v", result, topology.cleanupCalls, err)
			}
		})
	}
}

func TestNativeStarterAcceptsStatusChangeWithStableFreshIdentity(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	first := forkAgent("omp", created.PaneID, "/tmp/new.jsonl", "/tmp/project")
	first.TabID = created.TabID
	second := first
	second.Status, second.Revision, second.StateChangeSeq = "done", first.Revision+1, first.StateChangeSeq+1
	topology := &fakeTopology{created: created}
	starter := NewNativeStarter(Host{Runner: &scriptedRunner{results: []command.Result{listForkResult(t), listForkResult(t, first), listForkResult(t, second)}}, Session: "named"})
	starter.launcher = topology
	starter.wait = func(context.Context, time.Duration) error { return nil }
	result, err := starter.Start(context.Background(), NativeStartRequest{
		Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewTab,
	})
	if err != nil || !sameImmutable(result.Agent, second) || len(topology.cleanupCalls) != 0 {
		t.Fatalf("result=%+v cleanup=%+v error=%v", result, topology.cleanupCalls, err)
	}
}

func TestNativeStarterCreatesFreshOpenCodeSessionThroughOwnedServer(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}, CreatedTab: true, DirectArgv: true}
	child := forkAgent("opencode", created.PaneID, "ses_new", "/tmp/project")
	child.TabID = created.TabID
	server := &fakeOpenCodePromptServerLauncher{server: OpenCodeServer{URL: "http://127.0.0.1:4321", Directory: "/tmp/project"}}
	sessions := &fakeOpenCodePromptSessions{id: "ses_new"}
	topology := &fakeTopology{created: created}
	starter := NewNativeStarter(Host{Runner: &scriptedRunner{results: []command.Result{listForkResult(t), listForkResult(t, child), listForkResult(t, child)}}, Session: "named"})
	starter.launcher = topology
	starter.wait = func(context.Context, time.Duration) error { return nil }
	starter.openCodeServer, starter.openCodeSessions = server, sessions
	result, err := starter.Start(context.Background(), NativeStartRequest{
		Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"opencode", "--model", "provider/model"}, Directory: "/tmp/project", Placement: sessionhost.NewTab,
		StartOpenCodeServer: true,
	})
	if err != nil || !sameImmutable(result.Agent, child) || result.Created != created || server.starts != 1 || server.stops != 1 || sessions.creates != 1 || sessions.deletes != 0 {
		t.Fatalf("result=%+v server=%+v sessions=%+v error=%v", result, server, sessions, err)
	}
	wantArgv := []string{"opencode", "--model", "provider/model", "--session", "ses_new"}
	if len(topology.requests) != 1 || !reflect.DeepEqual(topology.requests[0].Argv, wantArgv) {
		t.Fatalf("launch=%+v", topology.requests)
	}
}

func TestNativeStarterOpenCodeIdentityFailureDeletesOnlyCreatedSessionAndTarget(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	wrong := forkAgent("opencode", created.PaneID, "ses_other", "/tmp/project")
	wrong.TabID = created.TabID
	server := &fakeOpenCodePromptServerLauncher{server: OpenCodeServer{URL: "http://127.0.0.1:4321", Directory: "/tmp/project"}}
	sessions := &fakeOpenCodePromptSessions{id: "ses_expected"}
	topology := &fakeTopology{created: created}
	starter := NewNativeStarter(Host{Runner: &scriptedRunner{results: []command.Result{listForkResult(t), listForkResult(t, wrong)}}, Session: "named"})
	starter.launcher, starter.openCodeServer, starter.openCodeSessions = topology, server, sessions
	result, err := starter.Start(context.Background(), NativeStartRequest{
		Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"opencode"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, StartOpenCodeServer: true,
	})
	if !errors.Is(err, ErrStaleReport) || result.Created.PaneID != "" || server.stops != 1 || sessions.deletes != 1 || sessions.lastDeleted != "ses_expected" || !reflect.DeepEqual(topology.cleanupCalls, []sessionhost.CreatedTarget{created}) {
		t.Fatalf("result=%+v server=%+v sessions=%+v cleanup=%+v error=%v", result, server, sessions, topology.cleanupCalls, err)
	}
}

func TestNativeStarterCancellationUsesUncancelledCleanup(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	topology := &fakeTopology{created: created}
	starter := NewNativeStarter(Host{Runner: &scriptedRunner{results: []command.Result{listForkResult(t), listForkResult(t)}}, Session: "named"})
	starter.launcher = topology
	starter.wait = func(context.Context, time.Duration) error { return context.Canceled }
	_, err := starter.Start(context.Background(), NativeStartRequest{
		Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewPane, Direction: "right",
	})
	if !errors.Is(err, context.Canceled) || len(topology.cleanupCalls) != 1 || topology.cleanupCtxErr != nil {
		t.Fatalf("cleanup=%+v cleanup context=%v error=%v", topology.cleanupCalls, topology.cleanupCtxErr, err)
	}
}

func TestValidateNativeStartCoversEveryOwnedBoundary(t *testing.T) {
	validOMP := NativeStartRequest{Argv: []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewTab}
	validOpenCode := NativeStartRequest{Argv: []string{"opencode"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, StartOpenCodeServer: true}
	server := OpenCodeServer{URL: "http://127.0.0.1:4321", Directory: "/tmp/project"}
	tests := []struct {
		name    string
		request NativeStartRequest
		valid   bool
	}{
		{name: "OMP valid", request: validOMP, valid: true},
		{name: "OpenCode owned server valid", request: validOpenCode, valid: true},
		{name: "OpenCode external server valid", request: NativeStartRequest{Argv: []string{"opencode"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, OpenCodeServer: server}, valid: true},
		{name: "empty directory", request: NativeStartRequest{Argv: []string{"omp"}, Placement: sessionhost.NewTab}},
		{name: "relative directory", request: NativeStartRequest{Argv: []string{"omp"}, Directory: "project", Placement: sessionhost.NewTab}},
		{name: "unclean directory", request: NativeStartRequest{Argv: []string{"omp"}, Directory: "/tmp/../tmp/project", Placement: sessionhost.NewTab}},
		{name: "NUL directory", request: NativeStartRequest{Argv: []string{"omp"}, Directory: "/tmp/project\x00x", Placement: sessionhost.NewTab}},
		{name: "negative timeout", request: NativeStartRequest{Argv: []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, ReadyTimeout: -1}},
		{name: "maximum timeout valid", request: NativeStartRequest{Argv: []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, ReadyTimeout: MaxForkReadyTimeout}, valid: true},
		{name: "excess timeout", request: NativeStartRequest{Argv: []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, ReadyTimeout: MaxForkReadyTimeout + time.Nanosecond}},
		{name: "OMP owned OpenCode server", request: NativeStartRequest{Argv: []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, StartOpenCodeServer: true}},
		{name: "OMP external OpenCode server", request: NativeStartRequest{Argv: []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, OpenCodeServer: server}},
		{name: "OMP relative socket", request: NativeStartRequest{Argv: []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, OMPStatusSocket: "relative"}},
		{name: "OMP unclean socket", request: NativeStartRequest{Argv: []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, OMPStatusSocket: "/tmp/../tmp/socket"}},
		{name: "OMP NUL socket", request: NativeStartRequest{Argv: []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, OMPStatusSocket: "/tmp/socket\x00x"}},
		{name: "OpenCode OMP socket", request: NativeStartRequest{Argv: []string{"opencode"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, StartOpenCodeServer: true, OMPStatusSocket: "/tmp/socket"}},
		{name: "OpenCode owned plus external server", request: NativeStartRequest{Argv: []string{"opencode"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, StartOpenCodeServer: true, OpenCodeServer: server}},
		{name: "OpenCode missing server", request: NativeStartRequest{Argv: []string{"opencode"}, Directory: "/tmp/project", Placement: sessionhost.NewTab}},
		{name: "OpenCode external directory mismatch", request: NativeStartRequest{Argv: []string{"opencode"}, Directory: "/tmp/other", Placement: sessionhost.NewTab, OpenCodeServer: server}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := validateNativeStart(test.request)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%t error=%v request=%+v", test.valid, err, test.request)
			}
		})
	}
}

func TestClassifyNativeNewCoversExactIdentityContract(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w", TabID: "t", PaneID: "p"}}
	base := Agent{
		SessionName: "named", Kind: "omp", Status: "idle", CWD: "/tmp/project",
		WorkspaceID: "w", TabID: "t", PaneID: "p",
		Session: &SessionRef{Source: "herdr:omp", Agent: "omp", Kind: "path", Value: "/tmp/new.jsonl"},
	}
	clone := func(agent Agent) Agent {
		copy := agent
		if agent.Session != nil {
			session := *agent.Session
			copy.Session = &session
		}
		return copy
	}
	tests := []struct {
		name    string
		change  func(*Agent)
		prior   map[SessionRef]struct{}
		ready   bool
		pending bool
	}{
		{name: "ready", ready: true},
		{name: "kind", change: func(a *Agent) { a.Kind = "opencode" }},
		{name: "session name", change: func(a *Agent) { a.SessionName = "other" }},
		{name: "workspace", change: func(a *Agent) { a.WorkspaceID = "other" }},
		{name: "tab", change: func(a *Agent) { a.TabID = "other" }},
		{name: "pane", change: func(a *Agent) { a.PaneID = "other" }},
		{name: "CWD", change: func(a *Agent) { a.CWD = "/tmp/other" }},
		{name: "focused", change: func(a *Agent) { a.Focused = true }},
		{name: "session pending", change: func(a *Agent) { a.Session = nil }, pending: true},
		{name: "status pending", change: func(a *Agent) { a.Status = "working" }, pending: true},
		{name: "relative OMP path", change: func(a *Agent) { a.Session.Value = "relative" }},
		{name: "unclean OMP path", change: func(a *Agent) { a.Session.Value = "/tmp/../tmp/new.jsonl" }},
		{name: "source", change: func(a *Agent) { a.Session.Source = "other" }},
		{name: "agent", change: func(a *Agent) { a.Session.Agent = "other" }},
		{name: "session kind", change: func(a *Agent) { a.Session.Kind = "id" }},
		{name: "empty session", change: func(a *Agent) { a.Session.Value = "" }},
		{name: "prior session", prior: map[SessionRef]struct{}{{Source: "herdr:omp", Agent: "omp", Kind: "path", Value: "/tmp/new.jsonl"}: {}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := clone(base)
			if test.change != nil {
				test.change(&candidate)
			}
			ready, pending := classifyNativeNew(candidate, created, "omp", "", "/tmp/project", test.prior)
			if ready != test.ready || pending != test.pending {
				t.Fatalf("ready=%t pending=%t candidate=%+v", ready, pending, candidate)
			}
		})
	}
	opencode := clone(base)
	opencode.Kind = "opencode"
	opencode.Session = &SessionRef{Source: "herdr:opencode", Agent: "opencode", Kind: "id", Value: "ses_exact"}
	if ready, pending := classifyNativeNew(opencode, created, "opencode", "ses_exact", "/tmp/project", nil); !ready || pending {
		t.Fatalf("exact OpenCode identity ready=%t pending=%t", ready, pending)
	}
	if ready, pending := classifyNativeNew(opencode, created, "opencode", "ses_other", "/tmp/project", nil); ready || pending {
		t.Fatalf("mismatched OpenCode identity ready=%t pending=%t", ready, pending)
	}
}

func TestNativeStarterUsesExactPollIntervalForPendingAndStableReads(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w", TabID: "t", PaneID: "p"}}
	agent := Agent{
		SessionName: "named", Kind: "omp", Status: "idle", CWD: "/tmp/project",
		WorkspaceID: "w", TabID: "t", PaneID: "p",
		Session: &SessionRef{Source: "herdr:omp", Agent: "omp", Kind: "path", Value: "/tmp/new.jsonl"},
	}
	for _, test := range []struct {
		name    string
		results []command.Result
		wantErr error
	}{
		{name: "pending", results: []command.Result{listForkResult(t)}, wantErr: context.Canceled},
		{name: "stable", results: []command.Result{listForkResult(t, agent), listForkResult(t, agent)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			starter := NativeStarter{Host: Host{Runner: &scriptedRunner{results: test.results}, Session: "named"}}
			waits := 0
			starter.wait = func(_ context.Context, duration time.Duration) error {
				waits++
				if duration != 50*time.Millisecond {
					t.Fatalf("poll interval=%s", duration)
				}
				if test.wantErr != nil {
					return test.wantErr
				}
				return nil
			}
			_, err := starter.waitForFresh(context.Background(), created, "omp", "", "/tmp/project", nil, time.Second)
			if !errors.Is(err, test.wantErr) || waits != 1 {
				t.Fatalf("waits=%d error=%v", waits, err)
			}
		})
	}
}

func TestNativeStarterSecondReadMustRemainReadySameIdentity(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w", TabID: "t", PaneID: "p"}}
	first := Agent{
		SessionName: "named", Kind: "omp", Status: "idle", CWD: "/tmp/project",
		WorkspaceID: "w", TabID: "t", PaneID: "p",
		Session: &SessionRef{Source: "herdr:omp", Agent: "omp", Kind: "path", Value: "/tmp/new.jsonl"},
	}
	pendingWire := forkWire(first)
	pendingWire.AgentSession = nil
	changed := first
	changed.Session = &SessionRef{Source: "herdr:omp", Agent: "omp", Kind: "path", Value: "/tmp/changed.jsonl"}
	for _, test := range []struct {
		name   string
		second command.Result
	}{
		{name: "missing", second: listForkResult(t)},
		{name: "pending", second: jsonResult(t, listResponse(pendingWire))},
		{name: "changed", second: listForkResult(t, changed)},
	} {
		t.Run(test.name, func(t *testing.T) {
			starter := NativeStarter{Host: Host{Runner: &scriptedRunner{results: []command.Result{listForkResult(t, first), test.second}}, Session: "named"}}
			starter.wait = func(context.Context, time.Duration) error { return nil }
			if _, err := starter.waitForFresh(context.Background(), created, "omp", "", "/tmp/project", nil, time.Second); !errors.Is(err, ErrStaleReport) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func sessionLabel(session string) string {
	if session == "" {
		return "default"
	}
	return session
}
