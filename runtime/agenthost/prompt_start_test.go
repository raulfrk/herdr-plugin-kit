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

type fakeOpenCodePromptSessions struct {
	id          string
	createErr   error
	deleteErr   error
	creates     int
	deletes     int
	lastCreated OpenCodeServer
	lastDeleted string
	deleteWait  time.Duration
}

type fakeOpenCodePromptServerLauncher struct {
	server     OpenCodeServer
	startErr   error
	stopErr    error
	starts     int
	stops      int
	executable string
	directory  string
}

func (f *fakeOpenCodePromptServerLauncher) Start(_ context.Context, executable, directory string) (OpenCodeServer, func(context.Context) error, error) {
	f.starts++
	f.executable, f.directory = executable, directory
	if f.startErr != nil {
		return OpenCodeServer{}, nil, f.startErr
	}
	return f.server, func(context.Context) error {
		f.stops++
		return f.stopErr
	}, nil
}

func (f *fakeOpenCodePromptSessions) Create(_ context.Context, server OpenCodeServer) (string, error) {
	f.creates++
	f.lastCreated = server
	return f.id, f.createErr
}

func (f *fakeOpenCodePromptSessions) Delete(ctx context.Context, _ OpenCodeServer, id string) error {
	f.deletes++
	f.lastDeleted = id
	if deadline, ok := ctx.Deadline(); ok {
		f.deleteWait = time.Until(deadline)
	}
	return f.deleteErr
}

func TestPromptStarterOMPOrdersIdentityThenOneDelivery(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}, CreatedTab: true, DirectArgv: true}
	topology := &fakeTopology{created: created}
	agent := forkAgent("omp", created.PaneID, "/tmp/new.jsonl", "/tmp/project")
	agent.TabID = created.TabID
	runner := &promptRunner{agent: forkWire(agent), argv: []string{"omp", "--model", "openai/gpt"}}
	deliveries := 0
	starter := NewPromptStarter(Host{Runner: runner, Session: "named"})
	starter.launcher = topology
	starter.wait = func(context.Context, time.Duration) error { return nil }
	starter.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
		deliveries++
		return PromptResult{Target: clonePromptTarget(target), Agent: target.Agent, State: PromptAcknowledged}, nil
	}
	prompt, _ := PreparePrompt("exact\n🚀")
	result, err := starter.Start(context.Background(), prompt, PromptStartRequest{
		Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"omp", "--model", "openai/gpt"}, Directory: "/tmp/project",
		Placement: sessionhost.NewTab, OMPStatusSocket: "/tmp/omp.sock",
	})
	if err != nil || result.State != PromptAcknowledged || deliveries != 1 || result.Agent.Session.Value != "/tmp/new.jsonl" {
		t.Fatalf("result=%+v deliveries=%d error=%v", result, deliveries, err)
	}
	if len(topology.requests) != 1 || !reflect.DeepEqual(topology.requests[0].Argv, []string{"omp", "--model", "openai/gpt"}) || topology.requests[0].Focus {
		t.Fatalf("launches=%+v", topology.requests)
	}
	if len(topology.cleanupCalls) != 0 {
		t.Fatalf("cleanup=%+v", topology.cleanupCalls)
	}
}

func TestPromptStarterOpenCodePrecreatesOwnedSessionAndInjectsOnlyItsID(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	topology := &fakeTopology{created: created}
	wire := validWireAgent(created.PaneID)
	wire.Agent, wire.AgentStatus, wire.CWD, wire.Focused, wire.TabID = "opencode", "idle", "/tmp/project", false, created.TabID
	wire.AgentSession = &wireSession{Source: "herdr:opencode", Agent: "opencode", Kind: "id", Value: "ses_created"}
	sessions := &fakeOpenCodePromptSessions{id: "ses_created"}
	starter := NewPromptStarter(Host{Runner: &promptRunner{agent: wire}, Session: ""})
	starter.launcher, starter.openCodeSessions = topology, sessions
	starter.wait = func(context.Context, time.Duration) error { return nil }
	starter.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
		return PromptResult{Target: target, Agent: target.Agent, State: PromptAcknowledged}, nil
	}
	prompt, _ := PreparePrompt("hello")
	result, err := starter.Start(context.Background(), prompt, PromptStartRequest{
		Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"opencode", "--mini"}, Directory: "/tmp/project", Placement: sessionhost.NewTab,
		OpenCodeServer: OpenCodeServer{URL: "http://127.0.0.1:4096", Directory: "/tmp/project"},
	})
	if err != nil || result.State != PromptAcknowledged || sessions.creates != 1 || sessions.deletes != 0 {
		t.Fatalf("result=%+v sessions=%+v error=%v", result, sessions, err)
	}
	want := []string{"opencode", "--mini", "--session", "ses_created"}
	if len(topology.requests) != 1 || !reflect.DeepEqual(topology.requests[0].Argv, want) {
		t.Fatalf("launches=%+v", topology.requests)
	}
}

func TestPromptStarterOwnsShortLivedOpenCodeServerButPreservesStartedTarget(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	topology := &fakeTopology{created: created}
	wire := validWireAgent(created.PaneID)
	wire.Agent, wire.AgentStatus, wire.CWD, wire.Focused, wire.TabID = "opencode", "idle", "/tmp/project", false, created.TabID
	wire.AgentSession = &wireSession{Source: "herdr:opencode", Agent: "opencode", Kind: "id", Value: "ses_created"}
	sessions := &fakeOpenCodePromptSessions{id: "ses_created"}
	server := &fakeOpenCodePromptServerLauncher{server: OpenCodeServer{URL: "http://127.0.0.1:49152", Directory: "/tmp/project"}}
	starter := NewPromptStarter(Host{Runner: &promptRunner{agent: wire}})
	starter.launcher, starter.openCodeSessions, starter.openCodeServer = topology, sessions, server
	starter.wait = func(context.Context, time.Duration) error { return nil }
	starter.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
		return PromptResult{Target: target, Agent: target.Agent, State: PromptAcknowledged}, nil
	}
	prompt, _ := PreparePrompt("hello")
	result, err := starter.Start(context.Background(), prompt, PromptStartRequest{
		Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"/opt/opencode", "--mini"}, Directory: "/tmp/project", Placement: sessionhost.NewTab,
		StartOpenCodeServer: true,
	})
	if err != nil || result.State != PromptAcknowledged || server.starts != 1 || server.stops != 1 || server.executable != "/opt/opencode" || server.directory != "/tmp/project" {
		t.Fatalf("result=%+v server=%+v error=%v", result, server, err)
	}
	if sessions.lastCreated != server.server || sessions.deletes != 0 || len(topology.cleanupCalls) != 0 || result.Target.OpenCodeServer != server.server {
		t.Fatalf("sessions=%+v cleanup=%+v target=%+v", sessions, topology.cleanupCalls, result.Target)
	}
}

func TestPromptStarterOwnedOpenCodeServerFailureCreatesNothing(t *testing.T) {
	topology := &fakeTopology{}
	server := &fakeOpenCodePromptServerLauncher{startErr: errors.New("server unavailable")}
	starter := NewPromptStarter(Host{Runner: &promptRunner{}})
	starter.launcher, starter.openCodeServer = topology, server
	prompt, _ := PreparePrompt("hello")
	result, err := starter.Start(context.Background(), prompt, PromptStartRequest{
		Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"opencode"}, Directory: "/tmp/project", Placement: sessionhost.NewTab,
		StartOpenCodeServer: true,
	})
	if err == nil || result.State != PromptNotAttempted || server.starts != 1 || server.stops != 0 || len(topology.requests) != 0 {
		t.Fatalf("result=%+v server=%+v launches=%+v error=%v", result, server, topology.requests, err)
	}
}

func TestPromptStarterOwnedOpenCodeStopFailureKeepsAcknowledgedTarget(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	topology := &fakeTopology{created: created}
	wire := validWireAgent(created.PaneID)
	wire.Agent, wire.AgentStatus, wire.CWD, wire.Focused, wire.TabID = "opencode", "idle", "/tmp/project", false, created.TabID
	wire.AgentSession = &wireSession{Source: "herdr:opencode", Agent: "opencode", Kind: "id", Value: "ses_created"}
	sessions := &fakeOpenCodePromptSessions{id: "ses_created"}
	server := &fakeOpenCodePromptServerLauncher{server: OpenCodeServer{URL: "http://127.0.0.1:49152", Directory: "/tmp/project"}, stopErr: errors.New("stop failed")}
	starter := NewPromptStarter(Host{Runner: &promptRunner{agent: wire}})
	starter.launcher, starter.openCodeSessions, starter.openCodeServer = topology, sessions, server
	starter.wait = func(context.Context, time.Duration) error { return nil }
	starter.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
		return PromptResult{Target: target, Agent: target.Agent, State: PromptAcknowledged}, nil
	}
	prompt, _ := PreparePrompt("hello")
	result, err := starter.Start(context.Background(), prompt, PromptStartRequest{
		Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"opencode"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, StartOpenCodeServer: true,
	})
	if err == nil || result.State != PromptAcknowledged || server.stops != 1 || sessions.deletes != 0 || len(topology.cleanupCalls) != 0 {
		t.Fatalf("result=%+v server=%+v sessions=%+v cleanup=%+v error=%v", result, server, sessions, topology.cleanupCalls, err)
	}
}

func TestPromptStarterRejectsOpenCodeDirectoryMismatchBeforeCreation(t *testing.T) {
	topology := &fakeTopology{}
	sessions := &fakeOpenCodePromptSessions{id: "ses_created"}
	starter := NewPromptStarter(Host{Runner: &promptRunner{}})
	starter.launcher, starter.openCodeSessions = topology, sessions
	prompt, _ := PreparePrompt("hello")
	_, err := starter.Start(context.Background(), prompt, PromptStartRequest{
		Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"opencode"}, Directory: "/tmp/project", Placement: sessionhost.NewTab,
		OpenCodeServer: OpenCodeServer{URL: "http://127.0.0.1:4096", Directory: "/tmp/other"},
	})
	if !errors.Is(err, ErrPromptInvalid) || sessions.creates != 0 || len(topology.requests) != 0 {
		t.Fatalf("sessions=%+v launches=%+v error=%v", sessions, topology.requests, err)
	}
}

func TestPromptStarterFailureOwnershipDistinguishesReadinessAndTransport(t *testing.T) {
	for _, test := range []struct {
		name        string
		delivery    PromptState
		wantCleanup int
	}{
		{"readiness", PromptNotAttempted, 1},
		{"pre-write transport", CreatedPromptNotSent, 0},
		{"ambiguous transport", CreatedPromptOutcomeUnknown, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
			topology := &fakeTopology{created: created}
			agent := forkAgent("omp", created.PaneID, "/tmp/new.jsonl", "/tmp/project")
			agent.TabID = created.TabID
			starter := NewPromptStarter(Host{Runner: &promptRunner{agent: forkWire(agent)}, Session: "named"})
			starter.launcher = topology
			starter.wait = func(context.Context, time.Duration) error { return nil }
			starter.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
				return PromptResult{Target: target, Agent: target.Agent, State: test.delivery}, errors.New("delivery failed")
			}
			prompt, _ := PreparePrompt("hello")
			result, err := starter.Start(context.Background(), prompt, PromptStartRequest{
				Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
				Argv:   []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, OMPStatusSocket: "/tmp/omp.sock",
			})
			if err == nil || result.State != test.delivery || len(topology.cleanupCalls) != test.wantCleanup {
				t.Fatalf("result=%+v cleanup=%+v error=%v", result, topology.cleanupCalls, err)
			}
		})
	}
}

func TestPromptStarterInvalidOrCancelledInputCreatesNothing(t *testing.T) {
	for _, test := range []struct {
		name   string
		ctx    context.Context
		prompt Prompt
	}{
		{"invalid", context.Background(), Prompt{}},
		{"cancelled", cancelledContext(), func() Prompt { prompt, _ := PreparePrompt("hello"); return prompt }()},
	} {
		t.Run(test.name, func(t *testing.T) {
			topology := &fakeTopology{}
			starter := NewPromptStarter(Host{Runner: &promptRunner{}})
			starter.launcher = topology
			_, err := starter.Start(test.ctx, test.prompt, PromptStartRequest{})
			if err == nil || len(topology.requests) != 0 {
				t.Fatalf("launches=%+v error=%v", topology.requests, err)
			}
		})
	}
}

func TestPromptStarterOrdinaryCodexUsesPaneBindingWithoutService(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	topology := &fakeManagedTopology{target: created}
	service := &fakeManagedService{snapshot: managedServiceSnapshot()}
	verifier := &fakeManagedVerifier{}
	runner := &scriptedRunner{results: []command.Result{
		{},
		managedAgentResult(t, managedAgent(created.Origin, "/tmp/project", managedNewID, false)),
		managedAgentResult(t, managedAgent(created.Origin, "/tmp/project", managedNewID, false)),
	}}
	managed := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	managed.service, managed.topology, managed.verifiers = service, topology, fakeManagedVerifierFactory{verifier}
	managed.discover = func(_ context.Context, target sessionhost.Origin, cwd string, argv []string, service CodexServiceSnapshot, policy ManagedCodexPolicy) (Agent, string, error) {
		if !reflect.DeepEqual(argv, []string{"codex", "--yolo"}) || service != (CodexServiceSnapshot{}) || policy != (ManagedCodexPolicy{}) {
			t.Fatalf("argv=%q service=%+v policy=%+v", argv, service, policy)
		}
		agent := managedAgent(target, cwd, managedNewID, false)
		agent.Session = nil
		return agent, managedNewID, nil
	}
	starter := NewPromptStarter(Host{Runner: runner, Session: "named"})
	starter.launcher, starter.managed = topology, managed
	starter.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
		return PromptResult{Target: target, Agent: target.Agent, State: PromptAcknowledged}, nil
	}
	prompt, _ := PreparePrompt("hello")
	result, err := starter.Start(context.Background(), prompt, PromptStartRequest{
		Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"codex", "--yolo"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, ObserveCodexPolicy: true,
	})
	if err != nil || result.State != PromptAcknowledged || result.Agent.Session == nil || result.Agent.Session.Value != managedNewID || service.callCount() != 0 || verifier.confirmID != "" || result.Target.CodexService != (CodexServiceSnapshot{}) {
		t.Fatalf("result=%+v service=%d verifier=%+v error=%v", result, service.callCount(), verifier, err)
	}
	if len(topology.launches) != 1 || !reflect.DeepEqual(topology.launches[0].Argv, []string{"codex", "--yolo"}) || len(topology.cleanups) != 0 || len(verifier.deleted) != 0 {
		t.Fatalf("launches=%v cleanup=%v deleted=%v", topology.launches, topology.cleanups, verifier.deleted)
	}
}

func TestPromptStarterResolvesObservedCodexPrefixAfterOnePrompt(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	unbound := managedAgent(created.Origin, "/tmp/project", "unused", false)
	unbound.Session = nil
	bound := managedAgent(created.Origin, "/tmp/project", managedNewID, false)
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, unbound), managedAgentResult(t, unbound), {},
		managedAgentResult(t, bound), managedAgentResult(t, bound),
	}}
	topology := &fakeManagedTopology{target: created}
	service := &fakeManagedService{snapshot: managedServiceSnapshot()}
	resolved := codexThreadIdentity{
		ID: managedNewID, CWD: "/tmp/project", ModelProvider: "openai", Model: "gpt-5.6-sol", ReasoningEffort: "medium",
	}
	verifier := &fakeManagedVerifier{listed: []codexThreadIdentity{resolved}, read: []codexThreadIdentity{resolved}}
	managed := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	managed.topology, managed.service, managed.verifiers = topology, service, fakeManagedVerifierFactory{verifier}
	managed.discoverObserved = func(_ context.Context, target sessionhost.Origin, cwd string, argv []string, _ CodexServiceSnapshot, _ ManagedCodexPolicy) (Agent, managedStatusObservation, error) {
		if target != created.Origin || cwd != "/tmp/project" || !reflect.DeepEqual(argv, []string{"codex", "--yolo"}) {
			t.Fatalf("target=%+v cwd=%q argv=%q", target, cwd, argv)
		}
		return unbound, managedStatusObservation{
			identity: "01a09f5c-37cc-77f1-9409-e9aa070a",
			policy:   ManagedCodexPolicy{Model: "gpt-5.6-sol", ReasoningEffort: "medium"},
		}, nil
	}
	deliveries := 0
	starter := NewPromptStarter(Host{Runner: runner, Session: "named"})
	starter.launcher, starter.managed = topology, managed
	starter.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
		deliveries++
		if target.Agent.Session != nil || target.codexSessionPrefix != "01a09f5c-37cc-77f1-9409-e9aa070a" {
			t.Fatalf("prompt target=%+v", target)
		}
		return PromptResult{Target: target, Agent: target.Agent, State: PromptAcknowledged}, nil
	}
	prompt, _ := PreparePrompt("hello")
	result, err := starter.Start(context.Background(), prompt, PromptStartRequest{
		Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"codex", "--yolo"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, ObserveCodexPolicy: true,
	})
	if err != nil || deliveries != 1 || result.State != PromptAcknowledged || result.Agent.Session == nil || result.Agent.Session.Value != managedNewID || result.Target.codexSessionPrefix != "" {
		t.Fatalf("result=%+v deliveries=%d error=%v", result, deliveries, err)
	}
	if service.callCount() != 1 || verifier.closed != 1 || len(topology.cleanups) != 0 {
		t.Fatalf("service=%d closed=%d cleanup=%v", service.callCount(), verifier.closed, topology.cleanups)
	}
}

func TestPromptStarterKeepsAcknowledgedTargetWhenPrefixResolutionFails(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	unbound := managedAgent(created.Origin, "/tmp/project", "unused", false)
	unbound.Session = nil
	runner := &scriptedRunner{results: []command.Result{managedAgentResult(t, unbound)}}
	topology := &fakeManagedTopology{target: created}
	verifier := &fakeManagedVerifier{listErr: errors.New("list failed")}
	managed := NewManagedCodexLauncher(Host{Runner: runner})
	managed.topology = topology
	managed.service = &fakeManagedService{snapshot: managedServiceSnapshot()}
	managed.verifiers = fakeManagedVerifierFactory{verifier}
	managed.discoverObserved = func(context.Context, sessionhost.Origin, string, []string, CodexServiceSnapshot, ManagedCodexPolicy) (Agent, managedStatusObservation, error) {
		return unbound, managedStatusObservation{identity: "01a09f5c-37cc-77f1-9409-e9aa070a", policy: ManagedCodexPolicy{Model: "gpt-5.6-sol", ReasoningEffort: "medium"}}, nil
	}
	starter := NewPromptStarter(Host{Runner: runner})
	starter.launcher, starter.managed = topology, managed
	starter.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
		return PromptResult{Target: target, Agent: target.Agent, State: PromptAcknowledged}, nil
	}
	prompt, _ := PreparePrompt("hello")
	result, err := starter.Start(context.Background(), prompt, PromptStartRequest{
		Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"codex", "--yolo"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, ObserveCodexPolicy: true,
	})
	if err == nil || result.State != PromptAcknowledged || len(topology.cleanups) != 0 || verifier.closed != 1 {
		t.Fatalf("result=%+v cleanup=%v closed=%d error=%v", result, topology.cleanups, verifier.closed, err)
	}
}

func TestPromptStartRejectsOwnedSelectorsAndPromptArgumentsBeforeCreation(t *testing.T) {
	for _, test := range []struct {
		name    string
		request PromptStartRequest
	}{
		{"Codex remote", PromptStartRequest{Argv: []string{"codex", "--remote", "unix:///tmp/socket"}, Directory: "/tmp/project", CodexPolicy: managedPolicy()}},
		{"Codex subcommand", PromptStartRequest{Argv: []string{"codex", "resume", managedNewID}, Directory: "/tmp/project", CodexPolicy: managedPolicy()}},
		{"Codex observed plus explicit policy", PromptStartRequest{Argv: []string{"codex"}, Directory: "/tmp/project", ObserveCodexPolicy: true, CodexPolicy: managedPolicy()}},
		{"Codex yolo without observed policy", PromptStartRequest{Argv: []string{"codex", "--yolo"}, Directory: "/tmp/project", CodexPolicy: managedPolicy()}},
		{"Codex duplicate yolo", PromptStartRequest{Argv: []string{"codex", "--yolo", "--yolo"}, Directory: "/tmp/project", ObserveCodexPolicy: true}},
		{"OMP positional prompt", PromptStartRequest{Argv: []string{"omp", "hello"}, Directory: "/tmp/project", OMPStatusSocket: "/tmp/omp.sock"}},
		{"observed policy for OMP", PromptStartRequest{Argv: []string{"omp"}, Directory: "/tmp/project", OMPStatusSocket: "/tmp/omp.sock", ObserveCodexPolicy: true}},
		{"OpenCode session", PromptStartRequest{Argv: []string{"opencode", "--session", "ses_other"}, Directory: "/tmp/project", OpenCodeServer: OpenCodeServer{URL: "http://127.0.0.1:4096", Directory: "/tmp/project"}}},
		{"OpenCode prompt", PromptStartRequest{Argv: []string{"opencode", "--prompt", "hello"}, Directory: "/tmp/project", OpenCodeServer: OpenCodeServer{URL: "http://127.0.0.1:4096", Directory: "/tmp/project"}}},
		{"owned server for OMP", PromptStartRequest{Argv: []string{"omp"}, Directory: "/tmp/project", StartOpenCodeServer: true}},
		{"owned and external server", PromptStartRequest{Argv: []string{"opencode"}, Directory: "/tmp/project", StartOpenCodeServer: true, OpenCodeServer: OpenCodeServer{URL: "http://127.0.0.1:4096", Directory: "/tmp/project"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			topology := &fakeTopology{}
			starter := NewPromptStarter(Host{Runner: &promptRunner{}})
			starter.launcher = topology
			prompt, _ := PreparePrompt("transient")
			if _, err := starter.Start(context.Background(), prompt, test.request); err == nil || len(topology.requests) != 0 {
				t.Fatalf("launches=%v error=%v", topology.requests, err)
			}
		})
	}
}

func TestPromptStarterOpenCodeLaunchFailureDeletesOnlyPrecreatedSession(t *testing.T) {
	topology := &fakeTopology{launchErr: errors.New("launch failed")}
	sessions := &fakeOpenCodePromptSessions{id: "ses_created"}
	starter := NewPromptStarter(Host{Runner: &promptRunner{}})
	starter.launcher, starter.openCodeSessions = topology, sessions
	prompt, _ := PreparePrompt("hello")
	_, err := starter.Start(context.Background(), prompt, PromptStartRequest{
		Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"opencode"}, Directory: "/tmp/project", Placement: sessionhost.NewTab,
		OpenCodeServer: OpenCodeServer{URL: "http://127.0.0.1:4096", Directory: "/tmp/project"},
	})
	if err == nil || sessions.creates != 1 || sessions.deletes != 1 || sessions.lastDeleted != "ses_created" || sessions.deleteWait < 4*time.Second || sessions.deleteWait > promptCleanupTimeout {
		t.Fatalf("sessions=%+v error=%v", sessions, err)
	}
}

func TestPromptStarterFocusFailureAfterAcknowledgementLeavesConversation(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	topology := &fakeTopology{created: created}
	agent := forkAgent("omp", created.PaneID, "/tmp/new.jsonl", "/tmp/project")
	agent.TabID = created.TabID
	starter := NewPromptStarter(Host{Runner: &promptRunner{agent: forkWire(agent)}, Session: "named"})
	starter.launcher = topology
	starter.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
		return PromptResult{Target: target, Agent: target.Agent, State: PromptAcknowledged}, nil
	}
	prompt, _ := PreparePrompt("hello")
	result, err := starter.Start(context.Background(), prompt, PromptStartRequest{
		Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewTab,
		OMPStatusSocket: "/tmp/omp.sock", Focus: true,
	})
	if err == nil || result.State != PromptAcknowledged || len(topology.cleanupCalls) != 0 {
		t.Fatalf("result=%+v cleanup=%v error=%v", result, topology.cleanupCalls, err)
	}
}

func TestPromptStarterDerivedOMPAddressFailureCleansOnlyCreatedTarget(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	topology := &fakeTopology{created: created}
	runner := &scriptedRunner{results: []command.Result{{}}, errs: []error{errors.New("session socket unavailable")}}
	starter := NewPromptStarter(Host{Runner: runner, Session: "named"})
	starter.launcher = topology
	prompt, _ := PreparePrompt("hello")
	result, err := starter.Start(context.Background(), prompt, PromptStartRequest{
		Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewTab,
	})
	if err == nil || result.State != PromptNotAttempted || len(topology.cleanupCalls) != 1 || topology.cleanupCalls[0] != created {
		t.Fatalf("result=%+v cleanup=%v error=%v", result, topology.cleanupCalls, err)
	}
}

func TestPromptStarterWaitForAgentErrorsAreBoundedAndCleaned(t *testing.T) {
	for _, test := range []struct {
		name      string
		runner    command.Runner
		configure func(*PromptStarter)
	}{
		{
			name: "diagnostic failure", runner: &scriptedRunner{results: []command.Result{{}}, errs: []error{errors.New("diagnostic failed")}},
			configure: func(*PromptStarter) {},
		},
		{
			name: "pending identity", runner: &promptRunner{agent: wireAgent{Agent: "omp", AgentStatus: "idle", CWD: "/tmp/project", Focused: false, PaneID: "w1:p2", TabID: "w1:t2", WorkspaceID: "w1"}},
			configure: func(starter *PromptStarter) {
				starter.wait = func(_ context.Context, duration time.Duration) error {
					if duration != 50*time.Millisecond {
						t.Fatalf("wait=%s", duration)
					}
					return context.DeadlineExceeded
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
			topology := &fakeTopology{created: created}
			starter := NewPromptStarter(Host{Runner: test.runner})
			starter.launcher = topology
			test.configure(&starter)
			prompt, _ := PreparePrompt("hello")
			_, err := starter.Start(context.Background(), prompt, PromptStartRequest{
				Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
				Argv:   []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewTab, OMPStatusSocket: "/tmp/omp.sock",
			})
			if err == nil || len(topology.cleanupCalls) != 1 {
				t.Fatalf("cleanup=%v error=%v", topology.cleanupCalls, err)
			}
		})
	}
}

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
