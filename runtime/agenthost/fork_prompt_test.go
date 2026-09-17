package agenthost

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func TestNativeForkPromptTargetsDistinctChildAfterSourceRecheck(t *testing.T) {
	source := forkAgent("omp", "w1:p1", "/tmp/source.jsonl", "/tmp/project")
	child := forkAgent("omp", "w1:p2", "/tmp/child.jsonl", "/tmp/project")
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: child.PaneID}}
	topology := &fakeTopology{created: created}
	runner := &scriptedRunner{results: []command.Result{listForkResult(t, source), listForkResult(t, child), listForkResult(t, source)}}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher = topology
	forker.wait = func(context.Context, time.Duration) error { return nil }
	deliveries := 0
	forker.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
		deliveries++
		if target.Source == nil || target.Source.Session.Value != source.Session.Value || target.Agent.Session.Value != child.Session.Value || target.Source.PaneID == target.Agent.PaneID {
			t.Fatalf("target=%+v", target)
		}
		return PromptResult{Target: target, Agent: target.Agent, State: PromptAcknowledged}, nil
	}
	prompt, _ := PreparePrompt("fork prompt")
	result, err := forker.Fork(context.Background(), ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "omp"}, Placement: sessionhost.NewPane,
		Direction: "right", OMPStatusSocket: "/tmp/omp.sock", Prompt: &prompt,
	})
	if err != nil || result.Prompt.State != PromptAcknowledged || deliveries != 1 || len(topology.cleanupCalls) != 0 {
		t.Fatalf("result=%+v deliveries=%d cleanup=%v error=%v", result, deliveries, topology.cleanupCalls, err)
	}
}

func TestNativeForkPromptFailureOwnership(t *testing.T) {
	for _, test := range []struct {
		state       PromptState
		wantCleanup int
	}{
		{PromptNotAttempted, 1},
		{CreatedPromptNotSent, 0},
		{CreatedPromptOutcomeUnknown, 0},
	} {
		t.Run(string(test.state), func(t *testing.T) {
			source := forkAgent("omp", "w1:p1", "/tmp/source.jsonl", "/tmp/project")
			child := forkAgent("omp", "w1:p2", "/tmp/child.jsonl", "/tmp/project")
			created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: child.PaneID}}
			topology := &fakeTopology{created: created}
			runner := &scriptedRunner{results: []command.Result{listForkResult(t, source), listForkResult(t, child), listForkResult(t, source)}}
			forker := NewForker(Host{Runner: runner, Session: "named"})
			forker.launcher = topology
			forker.wait = func(context.Context, time.Duration) error { return nil }
			forker.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
				return PromptResult{Target: target, Agent: target.Agent, State: test.state}, errors.New("delivery failed")
			}
			prompt, _ := PreparePrompt("fork prompt")
			result, err := forker.Fork(context.Background(), ForkRequest{
				Source: source, Profile: ForkProfile{Executable: "omp"}, Placement: sessionhost.NewPane,
				Direction: "right", OMPStatusSocket: "/tmp/omp.sock", Prompt: &prompt,
			})
			if err == nil || len(topology.cleanupCalls) != test.wantCleanup {
				t.Fatalf("result=%+v cleanup=%v error=%v", result, topology.cleanupCalls, err)
			}
			if test.state != PromptNotAttempted && result.Prompt.State != test.state {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestNativeForkPromptValidationPrecedesSourceReadAndLaunch(t *testing.T) {
	source := forkAgent("omp", "w1:p1", "/tmp/source.jsonl", "/tmp/project")
	topology := &fakeTopology{}
	runner := &scriptedRunner{}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher = topology
	_, err := forker.Fork(context.Background(), ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "omp"}, Placement: sessionhost.NewPane,
		Direction: "right", OMPStatusSocket: "/tmp/omp.sock", Prompt: &Prompt{},
	})
	if !errors.Is(err, ErrPromptInvalid) || len(runner.calls) != 0 || len(topology.requests) != 0 {
		t.Fatalf("calls=%v launches=%v error=%v", runner.calls, topology.requests, err)
	}
}

func TestOpenCodeForkPromptValidationPrecedesSourceReadAndLaunch(t *testing.T) {
	source := forkAgent("opencode", "w1:p1", "ses_source", "/tmp/project")
	prompt, _ := PreparePrompt("fork prompt")
	for _, server := range []OpenCodeServer{
		{},
		{URL: "http://127.0.0.1:4096", Directory: "/tmp/other"},
	} {
		topology := &fakeTopology{}
		runner := &scriptedRunner{}
		forker := NewForker(Host{Runner: runner, Session: "named"})
		forker.launcher = topology
		_, err := forker.Fork(context.Background(), ForkRequest{
			Source: source, Profile: ForkProfile{Executable: "opencode"}, Placement: sessionhost.NewTab,
			OpenCodeServer: server, Prompt: &prompt,
		})
		if err == nil || len(runner.calls) != 0 || len(topology.requests) != 0 {
			t.Fatalf("server=%+v calls=%v launches=%v error=%v", server, runner.calls, topology.requests, err)
		}
	}
}

func TestNativeForkDerivedOMPAddressFailureCleansOnlyCreatedTarget(t *testing.T) {
	source := forkAgent("omp", "w1:p1", "/tmp/source.jsonl", "/tmp/project")
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}}
	topology := &fakeTopology{created: created}
	runner := &scriptedRunner{
		results: []command.Result{listForkResult(t, source), {}},
		errs:    []error{nil, errors.New("session socket unavailable")},
	}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher = topology
	prompt, _ := PreparePrompt("fork prompt")
	result, err := forker.Fork(context.Background(), ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "omp"}, Placement: sessionhost.NewTab, Prompt: &prompt,
	})
	if err == nil || result.Created.PaneID != "" || result.Child.PaneID != "" || len(topology.cleanupCalls) != 1 || topology.cleanupCalls[0] != created {
		t.Fatalf("result=%+v cleanup=%v error=%v", result, topology.cleanupCalls, err)
	}
}

func TestNativeForkFocusFailureAfterAcknowledgementPreservesChild(t *testing.T) {
	source := forkAgent("omp", "w1:p1", "/tmp/source.jsonl", "/tmp/project")
	child := forkAgent("omp", "w1:p2", "/tmp/child.jsonl", "/tmp/project")
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: child.PaneID}}
	topology := &fakeTopology{created: created}
	runner := &scriptedRunner{
		results: []command.Result{listForkResult(t, source), listForkResult(t, child), listForkResult(t, source), {}},
		errs:    []error{nil, nil, nil, errors.New("focus failed")},
	}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher = topology
	forker.wait = func(context.Context, time.Duration) error { return nil }
	forker.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
		return PromptResult{Target: target, Agent: target.Agent, State: PromptAcknowledged}, nil
	}
	prompt, _ := PreparePrompt("fork prompt")
	result, err := forker.Fork(context.Background(), ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "omp"}, Placement: sessionhost.NewTab,
		OMPStatusSocket: "/tmp/omp.sock", Prompt: &prompt, Focus: true,
	})
	if err == nil || result.Prompt.State != PromptAcknowledged || result.Child.Session.Value != child.Session.Value || len(topology.cleanupCalls) != 0 {
		t.Fatalf("result=%+v cleanup=%v error=%v", result, topology.cleanupCalls, err)
	}
}

func TestForkPostPromptSourceDriftPreservesAcknowledgedChild(t *testing.T) {
	source := forkAgent("omp", "w1:p1", "/tmp/source.jsonl", "/tmp/project")
	child := forkAgent("omp", "w1:p2", "/tmp/child.jsonl", "/tmp/project")
	focused := child
	focused.Focused = true
	driftedSource := source
	driftedSource.CWD = "/tmp/other"
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: child.PaneID}}
	runner := &scriptedRunner{results: []command.Result{
		listForkResult(t, source), listForkResult(t, child), listForkResult(t, source),
		jsonResult(t, infoResponse(forkWire(focused))), listForkResult(t, driftedSource),
	}}
	topology := &fakeTopology{created: created}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher = topology
	forker.wait = func(context.Context, time.Duration) error { return nil }
	forker.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
		return PromptResult{Target: target, Agent: target.Agent, State: PromptAcknowledged}, nil
	}
	prompt, _ := PreparePrompt("hello")
	result, err := forker.Fork(context.Background(), ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "omp"}, Placement: sessionhost.NewPane,
		Direction: "right", Focus: true, OMPStatusSocket: "/tmp/omp.sock", Prompt: &prompt,
	})
	if !errors.Is(err, ErrStaleReport) || result.Prompt.State != PromptAcknowledged || !result.Child.Focused || len(topology.cleanupCalls) != 0 {
		t.Fatalf("result=%+v cleanup=%+v error=%v", result, topology.cleanupCalls, err)
	}
}

func TestOrdinaryCodexForkPromptVerifiesPersistedParent(t *testing.T) {
	source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	process := sessionhost.ForegroundProcess{PID: 101, Name: "codex", CWD: source.CWD, Argv: []string{"codex"}}
	topology := &fakeManagedTopology{target: created, inspected: process}
	service := &fakeManagedService{snapshot: managedServiceSnapshot()}
	resolved := codexThreadIdentity{
		ID: managedChildID, CWD: source.CWD, ForkedFromID: managedSourceID,
		ModelProvider: "openai", Model: "gpt-5.6-sol", ReasoningEffort: "medium",
	}
	verifier := &fakeManagedVerifier{read: []codexThreadIdentity{resolved}}
	runner := &scriptedRunner{results: []command.Result{
		listForkResult(t, source),
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("header")},
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("header")},
		managedAgentResult(t, managedAgent(created.Origin, source.CWD, managedChildID, false)),
		managedAgentResult(t, managedAgent(created.Origin, source.CWD, managedChildID, false)),
		managedAgentResult(t, managedAgent(created.Origin, source.CWD, managedChildID, false)),
		managedAgentResult(t, managedAgent(created.Origin, source.CWD, managedChildID, false)),
		listForkResult(t, source), listForkResult(t, source),
	}}
	managed := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	managed.service, managed.topology, managed.verifiers = service, topology, fakeManagedVerifierFactory{verifier}
	managed.discover = func(_ context.Context, target sessionhost.Origin, cwd string, _ []string, _ CodexServiceSnapshot, _ ManagedCodexPolicy) (Agent, string, error) {
		agent := managedAgent(target, cwd, managedChildID, false)
		agent.Session = nil
		return agent, managedChildID, nil
	}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher, forker.managed = topology, managed
	forker.wait = func(context.Context, time.Duration) error { return nil }
	forker.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
		return PromptResult{Target: target, Agent: target.Agent, State: PromptAcknowledged}, nil
	}
	prompt, _ := PreparePrompt("hello")
	result, err := forker.Fork(context.Background(), ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "codex"}, Placement: sessionhost.NewTab,
		CodexPolicy: managedPolicy(), Prompt: &prompt,
	})
	if err != nil || result.Child.Session == nil || result.Child.Session.Value != managedChildID || result.Prompt.State != PromptAcknowledged || service.callCount() != 1 || verifier.listCalls != 0 || verifier.confirmID != "" {
		t.Fatalf("result=%+v service=%d verifier=%+v calls=%q error=%v", result, service.callCount(), verifier, runner.calls, err)
	}
	if len(topology.cleanups) != 0 || len(verifier.deleted) != 0 || verifier.closed != 1 {
		t.Fatalf("cleanup=%v deleted=%v closed=%d", topology.cleanups, verifier.deleted, verifier.closed)
	}
}

func TestOrdinaryCodexForkPrefixResolvesAfterOnePrompt(t *testing.T) {
	source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	process := sessionhost.ForegroundProcess{PID: 101, Name: "codex", CWD: source.CWD, Argv: []string{"codex"}}
	unbound := managedAgent(created.Origin, source.CWD, managedChildID, false)
	unbound.Session = nil
	bound := managedAgent(created.Origin, source.CWD, managedChildID, false)
	prefix := "01a09f5c-37cc-77f1-9409-e9aa070a"
	for _, test := range []struct {
		name, parent string
		wantErr      bool
	}{
		{"matching parent", managedSourceID, false},
		{"wrong parent", managedOriginalID, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			results := []command.Result{
				listForkResult(t, source),
				managedAgentResult(t, source), {Stdout: managedEmptyComposer("header")},
				managedAgentResult(t, source), {Stdout: managedEmptyComposer("header")},
				listForkResult(t, source), listForkResult(t, source), managedAgentResult(t, bound),
			}
			if !test.wantErr {
				results = append(results, managedAgentResult(t, bound), managedAgentResult(t, bound), managedAgentResult(t, bound), listForkResult(t, source))
			}
			runner := &scriptedRunner{results: results}
			topology := &fakeManagedTopology{target: created, inspected: process}
			resolved := codexThreadIdentity{
				ID: managedChildID, CWD: source.CWD, ForkedFromID: test.parent,
				ModelProvider: "openai", Model: "gpt-5.6-sol", ReasoningEffort: "medium",
			}
			verifier := &fakeManagedVerifier{listed: []codexThreadIdentity{resolved}, read: []codexThreadIdentity{resolved}}
			managed := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
			managed.service, managed.topology, managed.verifiers = &fakeManagedService{snapshot: managedServiceSnapshot()}, topology, fakeManagedVerifierFactory{verifier}
			managed.discoverObserved = func(context.Context, sessionhost.Origin, string, []string, CodexServiceSnapshot, ManagedCodexPolicy) (Agent, managedStatusObservation, error) {
				return unbound, managedStatusObservation{
					identity: prefix, forkedFromPrefix: managedSourceID[:len(prefix)], policy: managedPolicy(), fromStatus: true,
				}, nil
			}
			forker := NewForker(Host{Runner: runner, Session: "named"})
			forker.launcher, forker.managed = topology, managed
			forker.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
				if target.Agent.Session != nil || target.codexSessionPrefix != prefix {
					t.Fatalf("prompt target=%+v", target)
				}
				return PromptResult{Target: target, Agent: target.Agent, State: PromptAcknowledged}, nil
			}
			prompt, _ := PreparePrompt("hello")
			result, err := forker.Fork(context.Background(), ForkRequest{
				Source: source, Profile: ForkProfile{Executable: "codex"}, Placement: sessionhost.NewTab,
				ObserveCodexPolicy: true, Prompt: &prompt,
			})
			if test.wantErr {
				if !errors.Is(err, ErrManagedStale) || result.Prompt.State != PromptAcknowledged || len(topology.cleanups) != 0 || verifier.listCalls != 1 {
					t.Fatalf("result=%+v cleanup=%v listCalls=%d error=%v", result, topology.cleanups, verifier.listCalls, err)
				}
				return
			}
			if err != nil || result.Prompt.State != PromptAcknowledged || result.Child.Session == nil || result.Child.Session.Value != managedChildID || result.Prompt.Target.codexSessionPrefix != "" || verifier.listCalls != 1 || verifier.closed != 1 || len(topology.cleanups) != 0 {
				t.Fatalf("result=%+v cleanup=%v verifier=%+v error=%v", result, topology.cleanups, verifier, err)
			}
		})
	}
}

func TestOrdinaryCodexForkObservedPolicyConflictsRejectBeforeSourceRead(t *testing.T) {
	source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
	prompt, _ := PreparePrompt("hello")
	for _, request := range []ForkRequest{
		{Source: source, Profile: ForkProfile{Executable: "codex"}, Placement: sessionhost.NewTab, ObserveCodexPolicy: true},
		{Source: source, Profile: ForkProfile{Executable: "codex"}, Placement: sessionhost.NewTab, ObserveCodexPolicy: true, CodexPolicy: managedPolicy(), Prompt: &prompt},
		{Source: forkAgent("omp", "w1:p1", "/tmp/source.jsonl", "/tmp/project"), Profile: ForkProfile{Executable: "omp"}, Placement: sessionhost.NewTab, ObserveCodexPolicy: true, Prompt: &prompt},
	} {
		topology := &fakeTopology{}
		runner := &scriptedRunner{}
		forker := NewForker(Host{Runner: runner, Session: "named"})
		forker.launcher = topology
		if _, err := forker.Fork(context.Background(), request); !errors.Is(err, ErrUnsupportedFork) || len(runner.calls) != 0 || len(topology.requests) != 0 {
			t.Fatalf("request=%+v calls=%v launches=%v error=%v", request, runner.calls, topology.requests, err)
		}
	}
}

func TestOrdinaryCodexForkPrefixRejectsSourceProcessDriftBeforePrompt(t *testing.T) {
	source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	process := sessionhost.ForegroundProcess{PID: 101, Name: "codex", CWD: source.CWD, Argv: []string{"codex"}}
	drifted := process
	drifted.PID++
	topology := &fakeManagedTopology{target: created, inspectedSeq: []sessionhost.ForegroundProcess{process, process, drifted}}
	runner := &scriptedRunner{results: []command.Result{
		listForkResult(t, source),
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("header")},
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("header")},
		listForkResult(t, source),
	}}
	unbound := managedAgent(created.Origin, source.CWD, managedChildID, false)
	unbound.Session = nil
	managed := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	managed.topology = topology
	managed.discoverObserved = func(context.Context, sessionhost.Origin, string, []string, CodexServiceSnapshot, ManagedCodexPolicy) (Agent, managedStatusObservation, error) {
		return unbound, managedStatusObservation{
			identity: "01a09f5c-37cc-77f1-9409-e9aa070a", forkedFromPrefix: "01a09f5c-37cc-77f1-9409-e9aa070a",
			policy: managedPolicy(), fromStatus: true,
		}, nil
	}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher, forker.managed = topology, managed
	deliveries := 0
	forker.deliver = func(context.Context, Prompt, PromptTarget) (PromptResult, error) {
		deliveries++
		return PromptResult{}, nil
	}
	prompt, _ := PreparePrompt("hello")
	_, err := forker.Fork(context.Background(), ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "codex"}, Placement: sessionhost.NewTab,
		CodexPolicy: managedPolicy(), Prompt: &prompt,
	})
	if !errors.Is(err, ErrManagedStale) || deliveries != 0 || len(topology.cleanups) != 1 || topology.cleanups[0] != created {
		t.Fatalf("deliveries=%d cleanup=%v error=%v", deliveries, topology.cleanups, err)
	}
}
