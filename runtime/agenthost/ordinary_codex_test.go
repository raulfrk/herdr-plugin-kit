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
	}{
		{"unsupported operation", ManagedOperation("invalid"), managedPolicy()},
		{"invalid policy", ManagedNew, ManagedCodexPolicy{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &scriptedRunner{}
			service := &fakeManagedService{}
			launcher := NewManagedCodexLauncher(Host{Runner: runner})
			launcher.service = service
			binding, err := beginOrdinaryCodexBinding(context.Background(), launcher, tc.operation, Agent{}, "/tmp/project", tc.policy, 0)
			if binding != nil || !errors.Is(err, ErrManagedUnsupported) {
				t.Fatalf("binding=%+v error=%v", binding, err)
			}
			if len(runner.calls) != 0 || service.callCount() != 0 {
				t.Fatal("invalid launch caused external calls")
			}
		})
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
	binding, err := beginOrdinaryCodexBinding(context.Background(), launcher, ManagedNew, Agent{}, "/tmp/project", managedPolicy(), 0)
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
