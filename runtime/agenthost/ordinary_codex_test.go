package agenthost

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func TestOrdinaryCodexBindingRejectsOperationAndPolicyIndependently(t *testing.T) {
	for _, tc := range []struct {
		name      string
		operation ManagedOperation
		policy    ManagedCodexPolicy
		observe   bool
	}{
		{"unsupported operation", ManagedOperation("invalid"), managedPolicy(), false},
		{"invalid policy", ManagedNew, ManagedCodexPolicy{}, false},
		{"observed with explicit policy", ManagedNew, managedPolicy(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &scriptedRunner{}
			service := &fakeManagedService{}
			launcher := NewManagedCodexLauncher(Host{Runner: runner})
			launcher.service = service
			binding, err := beginOrdinaryCodexBinding(context.Background(), launcher, tc.operation, Agent{}, "/tmp/project", tc.policy, tc.observe, 0)
			if binding != nil || !errors.Is(err, ErrManagedUnsupported) {
				t.Fatalf("binding=%+v error=%v", binding, err)
			}
			if len(runner.calls) != 0 || service.callCount() != 0 {
				t.Fatal("invalid launch caused external calls")
			}
		})
	}
}

func TestOrdinaryCodexBindingAcceptsObservedPolicyWithoutExternalCalls(t *testing.T) {
	launcher := NewManagedCodexLauncher(Host{Runner: &scriptedRunner{}})
	binding, err := beginOrdinaryCodexBinding(context.Background(), launcher, ManagedNew, Agent{}, "/tmp/project", ManagedCodexPolicy{}, true, 0)
	if err != nil || binding == nil || binding.policy != (ManagedCodexPolicy{}) {
		t.Fatalf("binding=%+v error=%v", binding, err)
	}
}

func TestOrdinaryCodexBindingRejectsInvalidOrSourceIdentityBeforeReporting(t *testing.T) {
	for _, id := range []string{"", "truncated", managedSourceID} {
		launcher := NewManagedCodexLauncher(Host{})
		launcher.topology = &fakeManagedTopology{}
		launcher.discover = func(context.Context, sessionhost.Origin, string, []string, CodexServiceSnapshot, ManagedCodexPolicy) (Agent, string, error) {
			return Agent{}, id, nil
		}
		binding := &ordinaryCodexBinding{launcher: launcher, operation: ManagedFork, sourceID: managedSourceID, readyTimeout: DefaultPromptReadyTimeout}
		if _, err := binding.Finish(context.Background(), sessionhost.CreatedTarget{}, "/tmp/project", []string{"codex", "fork", managedSourceID}); !errors.Is(err, ErrManagedStale) {
			t.Fatalf("identity %q error=%v", id, err)
		}
	}
}

func TestOrdinaryCodexBindingUsesPaneDiscoveryWithoutService(t *testing.T) {
	target := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	service := &fakeManagedService{snapshot: managedServiceSnapshot()}
	verifier := &fakeManagedVerifier{}
	runner := &scriptedRunner{results: []command.Result{
		{},
		managedAgentResult(t, managedAgent(target.Origin, "/tmp/project", managedNewID, false)),
		managedAgentResult(t, managedAgent(target.Origin, "/tmp/project", managedNewID, false)),
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	// A split first exposes the shell executing the submitted literal argv.
	topology := &fakeManagedTopology{verifyErrs: []error{sessionhost.ErrStaleTarget, nil}}
	launcher.topology = topology
	launcher.service, launcher.verifiers = service, fakeManagedVerifierFactory{verifier}
	discoveries := 0
	launcher.discover = func(_ context.Context, got sessionhost.Origin, cwd string, argv []string, gotService CodexServiceSnapshot, policy ManagedCodexPolicy) (Agent, string, error) {
		discoveries++
		if len(topology.verifications) != 2 {
			t.Fatal("identity discovery ran before the exact foreground process settled")
		}
		if got != target.Origin || cwd != "/tmp/project" || !reflect.DeepEqual(argv, []string{"codex"}) || gotService != (CodexServiceSnapshot{}) || policy != managedPolicy() {
			t.Fatalf("target=%+v cwd=%q argv=%q service=%+v policy=%+v", got, cwd, argv, gotService, policy)
		}
		agent := managedAgent(got, cwd, managedNewID, false)
		agent.Session = nil
		return agent, managedNewID, nil
	}
	binding, err := beginOrdinaryCodexBinding(context.Background(), launcher, ManagedNew, Agent{}, "/tmp/project", managedPolicy(), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := binding.Finish(context.Background(), target, "/tmp/project", []string{"codex"})
	if err != nil || agent.Session == nil || agent.Session.Value != managedNewID || discoveries != 1 || verifier.confirmID != "" || verifier.auditCalls != 0 {
		t.Fatalf("agent=%+v discoveries=%d verifier=%+v error=%v", agent, discoveries, verifier, err)
	}
	wantReport := []string{"herdr", "--session", "named", "pane", "report-agent-session", target.PaneID, "--source", "herdr:codex", "--agent", "codex", "--agent-session-id", managedNewID}
	if len(runner.calls) != 3 || !reflect.DeepEqual(runner.calls[0], wantReport) {
		t.Fatalf("calls=%q", runner.calls)
	}
	if verifier.closed != 0 || service.callCount() != 0 {
		t.Fatalf("closed=%d service=%d error=%v", verifier.closed, service.callCount(), err)
	}
}

func TestOrdinaryCodexBindingVerifiesExactForkParentBeforePrompt(t *testing.T) {
	target := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	source := managedAgent(sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"}, "/tmp/project", managedSourceID, false)
	child := managedAgent(target.Origin, "/tmp/project", managedChildID, false)
	process := sessionhost.ForegroundProcess{PID: 77, Name: "codex", CWD: "/tmp/project", Argv: []string{"codex"}}
	for _, test := range []struct {
		name, parent string
		wantErr      error
	}{
		{"matching parent", managedSourceID, nil},
		{"wrong parent", managedOriginalID, ErrManagedStale},
	} {
		t.Run(test.name, func(t *testing.T) {
			results := []command.Result{managedAgentResult(t, child)}
			if test.wantErr == nil {
				results = append(results, managedAgentResult(t, child), managedAgentResult(t, child), managedAgentResult(t, child), jsonResult(t, listResponse(wireFromAgent(source))))
			}
			runner := &scriptedRunner{results: results}
			topology := &fakeManagedTopology{inspected: process}
			resolved := codexThreadIdentity{
				ID: managedChildID, CWD: "/tmp/project", ForkedFromID: test.parent,
				ModelProvider: "openai", Model: "gpt-5.6-sol", ReasoningEffort: "medium",
			}
			verifier := &fakeManagedVerifier{read: []codexThreadIdentity{resolved}}
			launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
			launcher.topology = topology
			launcher.service = &fakeManagedService{snapshot: managedServiceSnapshot()}
			launcher.verifiers = fakeManagedVerifierFactory{verifier}
			launcher.discoverObserved = func(context.Context, sessionhost.Origin, string, []string, CodexServiceSnapshot, ManagedCodexPolicy) (Agent, managedStatusObservation, error) {
				return child, managedStatusObservation{identity: managedChildID, policy: managedPolicy()}, nil
			}
			binding := &ordinaryCodexBinding{
				launcher: launcher, operation: ManagedFork, policy: managedPolicy(), source: source,
				sourceID: managedSourceID, sourceProcess: process, readyTimeout: DefaultPromptReadyTimeout,
			}
			prepared, err := binding.Prepare(context.Background(), target, "/tmp/project", []string{"codex", "fork", managedSourceID})
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) || prepared.Agent.PaneID != "" || verifier.listCalls != 0 || hasAgentSessionReport(runner.calls) {
					t.Fatalf("prepared=%+v listCalls=%d calls=%q error=%v", prepared, verifier.listCalls, runner.calls, err)
				}
				return
			}
			if err != nil || prepared.Agent.Session == nil || prepared.Agent.Session.Value != managedChildID || verifier.listCalls != 0 {
				t.Fatalf("prepared=%+v listCalls=%d calls=%q verifications=%q error=%v", prepared, verifier.listCalls, runner.calls, topology.verifications, err)
			}
		})
	}
}

func TestOrdinaryCodexBindingRejectsAmbiguousDriftedAndCancelledPrefixResolution(t *testing.T) {
	target := sessionhost.CreatedTarget{Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	unbound := managedAgent(target.Origin, "/tmp/project", managedNewID, false)
	unbound.Session = nil
	prefix := "01a09f5c-37cc-77f1-9409-e9aa070a"
	base := codexThreadIdentity{ID: managedNewID, CWD: "/tmp/project", ModelProvider: "openai", Model: "gpt-5.6-sol", ReasoningEffort: "medium"}
	for _, test := range []struct {
		name       string
		candidates []codexThreadIdentity
		cancel     bool
		wantErr    error
	}{
		{"ambiguous", []codexThreadIdentity{base, {ID: managedSecondID, CWD: "/tmp/project", ModelProvider: "openai", Model: "gpt-5.6-sol", ReasoningEffort: "medium"}}, false, ErrManagedAmbiguous},
		{"wrong parent", []codexThreadIdentity{{ID: managedNewID, CWD: "/tmp/project", ForkedFromID: managedSourceID, ModelProvider: "openai", Model: "gpt-5.6-sol", ReasoningEffort: "medium"}}, false, ErrManagedStale},
		{"wrong model", []codexThreadIdentity{{ID: managedNewID, CWD: "/tmp/project", ModelProvider: "openai", Model: "gpt-5.6-terra", ReasoningEffort: "medium"}}, false, ErrManagedPolicyDrift},
		{"wrong reasoning", []codexThreadIdentity{{ID: managedNewID, CWD: "/tmp/project", ModelProvider: "openai", Model: "gpt-5.6-sol", ReasoningEffort: "high"}}, false, ErrManagedPolicyDrift},
		{"cancelled", nil, true, context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &scriptedRunner{results: []command.Result{managedAgentResult(t, unbound)}}
			verifier := &fakeManagedVerifier{listed: test.candidates}
			if len(test.candidates) == 1 {
				verifier.read = []codexThreadIdentity{test.candidates[0]}
			}
			launcher := NewManagedCodexLauncher(Host{Runner: runner})
			launcher.topology = &fakeManagedTopology{}
			launcher.service = &fakeManagedService{snapshot: managedServiceSnapshot()}
			launcher.verifiers = fakeManagedVerifierFactory{verifier}
			binding := &ordinaryCodexBinding{launcher: launcher, operation: ManagedNew, readyTimeout: DefaultPromptReadyTimeout}
			ctx := context.Background()
			if test.cancel {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			_, _, err := binding.resolvePersistedIdentity(ctx, target, "/tmp/project", []string{"codex"}, ordinaryCodexPreparation{Agent: unbound, Prefix: prefix, Policy: managedPolicy()})
			if !errors.Is(err, test.wantErr) || verifier.listCalls != 1 || hasAgentSessionReport(runner.calls) {
				t.Fatalf("listCalls=%d calls=%q error=%v", verifier.listCalls, runner.calls, err)
			}
		})
	}
}

func hasAgentSessionReport(calls [][]string) bool {
	for _, call := range calls {
		for _, arg := range call {
			if arg == "report-agent-session" {
				return true
			}
		}
	}
	return false
}
