package agenthost

import (
	"context"
	"reflect"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func TestOrdinaryCodexBindingReusesSharedDiscoveryAndBindsExactPane(t *testing.T) {
	target := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	service := &fakeManagedService{snapshot: managedServiceSnapshot()}
	identity := codexThreadIdentity{ID: managedNewID, CWD: "/tmp/project", ModelProvider: "openai"}
	verifier := &fakeManagedVerifier{preflight: managedPreflight("/tmp/project"), confirm: identity, recheck: identity, audit: managedPreflight("/tmp/project").EffectiveCodexPolicy}
	runner := &scriptedRunner{results: []command.Result{
		{},
		managedAgentResult(t, managedAgent(target.Origin, "/tmp/project", managedNewID, false)),
		managedAgentResult(t, managedAgent(target.Origin, "/tmp/project", managedNewID, false)),
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	launcher.service, launcher.verifiers = service, fakeManagedVerifierFactory{verifier}
	discoveries := 0
	launcher.discover = func(_ context.Context, got sessionhost.Origin, cwd string, argv []string, gotService CodexServiceSnapshot, policy ManagedCodexPolicy) (Agent, string, error) {
		discoveries++
		if got != target.Origin || cwd != "/tmp/project" || !reflect.DeepEqual(argv, []string{"codex"}) || gotService != service.snapshot || policy != managedPolicy() {
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
	if err != nil || agent.Session == nil || agent.Session.Value != managedNewID || discoveries != 1 || verifier.confirmOp != ManagedNew || verifier.confirmID != managedNewID || verifier.auditCalls != 0 {
		t.Fatalf("agent=%+v discoveries=%d verifier=%+v error=%v", agent, discoveries, verifier, err)
	}
	wantReport := []string{"herdr", "--session", "named", "pane", "report-agent-session", target.PaneID, "--source", "herdr:codex", "--agent", "codex", "--agent-session-id", managedNewID}
	if len(runner.calls) != 3 || !reflect.DeepEqual(runner.calls[0], wantReport) {
		t.Fatalf("calls=%q", runner.calls)
	}
	if err := binding.Close(); err != nil || verifier.closed != 1 || service.callCount() != 1 {
		t.Fatalf("closed=%d service=%d error=%v", verifier.closed, service.callCount(), err)
	}
}

func TestOrdinaryCodexBindingCleanupThreadOwnership(t *testing.T) {
	verifier := &fakeManagedVerifier{}
	var nilBinding *ordinaryCodexBinding
	if err := nilBinding.CleanupThread(context.Background()); err != nil {
		t.Fatal(err)
	}
	binding := &ordinaryCodexBinding{verifier: verifier}
	if err := binding.CleanupThread(context.Background()); err != nil || len(verifier.deleted) != 0 {
		t.Fatalf("deleted=%v error=%v", verifier.deleted, err)
	}
	binding.createdThreadID = managedNewID
	if err := binding.CleanupThread(context.Background()); err != nil || !reflect.DeepEqual(verifier.deleted, []string{managedNewID}) {
		t.Fatalf("deleted=%v error=%v", verifier.deleted, err)
	}
}
