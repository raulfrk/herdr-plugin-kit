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

func TestOrdinaryCodexBindingObservationRequiresEveryPaneGuard(t *testing.T) {
	target := sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"}
	unbound := managedAgent(target, "/tmp/project", managedNewID, false)
	unbound.Session = nil
	prepared := ordinaryCodexPreparation{Agent: unbound, Prefix: "01a09f5c-37cc-77f1-9409-e9aa070a"}
	validWire := wireFromAgent(unbound)
	validWire.AgentSession = nil
	otherID := "02b19f5c-37cc-77f1-9409-e9aa070ad8d0"
	tests := []struct {
		name  string
		wire  wireAgent
		agent Agent
		valid bool
	}{
		{"unbound", validWire, unbound, true},
		{"focused", func() wireAgent { value := validWire; value.Focused = true; return value }(), unbound, false},
		{"launch pending", func() wireAgent { value := validWire; value.LaunchPending = true; return value }(), unbound, false},
		{"pane changed", validWire, func() Agent { value := unbound; value.PaneID = "w1:p2"; return value }(), false},
		{"cwd changed", validWire, func() Agent { value := unbound; value.CWD = "/tmp/other"; return value }(), false},
		{"valid bound", validWire, managedAgent(target, "/tmp/project", managedNewID, false), true},
		{"invalid session", validWire, func() Agent {
			value := managedAgent(target, "/tmp/project", managedNewID, false)
			value.Session.Source = "other"
			return value
		}(), false},
		{"wrong prefix", validWire, managedAgent(target, "/tmp/project", otherID, false), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validOrdinaryBindingObservation(prepared, test.wire, test.agent); got != test.valid {
				t.Fatalf("valid=%t want=%t", got, test.valid)
			}
		})
	}
}

func TestResolvedOrdinaryIdentityRequiresEveryAuthoritativeField(t *testing.T) {
	valid := codexThreadIdentity{ID: managedNewID, CWD: "/tmp/project", ForkedFromID: managedSourceID, ModelProvider: "openai", Model: "gpt-5.6-sol", ReasoningEffort: "medium"}
	if !validResolvedOrdinaryIdentity(valid, managedNewID, "/tmp/project", managedSourceID) {
		t.Fatal("valid identity rejected")
	}
	for name, mutate := range map[string]func(*codexThreadIdentity){
		"id":       func(value *codexThreadIdentity) { value.ID = managedSecondID },
		"cwd":      func(value *codexThreadIdentity) { value.CWD = "/tmp/other" },
		"parent":   func(value *codexThreadIdentity) { value.ForkedFromID = managedOriginalID },
		"model":    func(value *codexThreadIdentity) { value.Model = "" },
		"provider": func(value *codexThreadIdentity) { value.ModelProvider = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if validResolvedOrdinaryIdentity(candidate, managedNewID, "/tmp/project", managedSourceID) {
				t.Fatalf("accepted %+v", candidate)
			}
		})
	}
	withoutReasoning := valid
	withoutReasoning.ReasoningEffort = ""
	if !validResolvedOrdinaryIdentity(withoutReasoning, managedNewID, "/tmp/project", managedSourceID) {
		t.Fatal("identity without optional App Server reasoning rejected")
	}
}

func TestResolvedOrdinaryPolicyObservesOrPreservesExactPolicy(t *testing.T) {
	candidate := codexThreadIdentity{Model: "gpt-5.6-sol", ReasoningEffort: "medium"}
	observed, err := resolvedOrdinaryPolicy(ManagedCodexPolicy{}, false, candidate)
	if err != nil || observed.Model != candidate.Model || observed.ReasoningEffort != candidate.ReasoningEffort {
		t.Fatalf("observed=%+v error=%v", observed, err)
	}
	expected := managedPolicy()
	preserved, err := resolvedOrdinaryPolicy(expected, false, candidate)
	if err != nil || preserved != expected {
		t.Fatalf("preserved=%+v error=%v", preserved, err)
	}
	preserved, err = resolvedOrdinaryPolicy(expected, false, codexThreadIdentity{Model: candidate.Model})
	if err != nil || preserved != expected {
		t.Fatalf("preserved without App Server reasoning=%+v error=%v", preserved, err)
	}
	if _, err := resolvedOrdinaryPolicy(ManagedCodexPolicy{}, false, codexThreadIdentity{Model: candidate.Model}); !errors.Is(err, ErrManagedPolicyDrift) {
		t.Fatalf("unobserved missing reasoning error=%v", err)
	}
	for _, candidate := range []codexThreadIdentity{{Model: "other", ReasoningEffort: "medium"}, {Model: "gpt-5.6-sol", ReasoningEffort: "high"}} {
		if _, err := resolvedOrdinaryPolicy(expected, false, candidate); !errors.Is(err, ErrManagedPolicyDrift) {
			t.Fatalf("candidate=%+v error=%v", candidate, err)
		}
	}
}

func TestResolvedOrdinaryPolicyExpandsCompatibleObservedPrefix(t *testing.T) {
	expected := ManagedCodexPolicy{Model: "gpt-5.6-sol", ReasoningEffort: "med"}
	candidate := codexThreadIdentity{Model: "gpt-5.6-sol", ReasoningEffort: "medium"}
	resolved, err := resolvedOrdinaryPolicy(expected, true, candidate)
	if err != nil || resolved != (ManagedCodexPolicy{Model: "gpt-5.6-sol", ReasoningEffort: "medium"}) {
		t.Fatalf("resolved=%+v error=%v", resolved, err)
	}
	missingReasoning := codexThreadIdentity{Model: "gpt-5.6-sol"}
	resolved, err = resolvedOrdinaryPolicy(expected, true, missingReasoning)
	if err != nil || resolved != expected {
		t.Fatalf("missing reasoning resolved=%+v error=%v", resolved, err)
	}
	for _, incompatible := range []codexThreadIdentity{
		{Model: "gpt-6-astra", ReasoningEffort: "medium"},
		{Model: "gpt-5.6-sol", ReasoningEffort: "high"},
	} {
		if _, err := resolvedOrdinaryPolicy(expected, true, incompatible); !errors.Is(err, ErrManagedPolicyDrift) {
			t.Fatalf("candidate=%+v error=%v", incompatible, err)
		}
	}
}

func TestOrdinaryStatusParentMatchesOperationExactly(t *testing.T) {
	prefix := managedSourceID[:len(managedSourceID)-4]
	for _, test := range []struct {
		operation ManagedOperation
		parent    string
		valid     bool
	}{
		{ManagedNew, "", true},
		{ManagedNew, prefix, false},
		{ManagedFork, "", false},
		{ManagedFork, "02b19f5c-37cc-77f1-9409-e9aa070a", false},
		{ManagedFork, prefix, true},
		{ManagedOperation("other"), prefix, false},
	} {
		if got := validOrdinaryStatusParent(test.operation, managedSourceID, test.parent); got != test.valid {
			t.Fatalf("operation=%q parent=%q valid=%t want=%t", test.operation, test.parent, got, test.valid)
		}
	}
}

func TestOrdinaryCodexBindingRejectsInvalidOrSourceIdentityBeforeReporting(t *testing.T) {
	for _, id := range []string{"", "truncated", managedSourceID} {
		launcher := NewManagedCodexLauncher(Host{})
		launcher.topology = &fakeManagedTopology{}
		launcher.service = &fakeManagedService{snapshot: managedServiceSnapshot()}
		launcher.discover = func(context.Context, sessionhost.Origin, string, []string, CodexServiceSnapshot, ManagedCodexPolicy) (Agent, string, error) {
			return Agent{}, id, nil
		}
		binding := &ordinaryCodexBinding{launcher: launcher, operation: ManagedFork, sourceID: managedSourceID, readyTimeout: DefaultPromptReadyTimeout}
		if _, err := binding.Finish(context.Background(), sessionhost.CreatedTarget{}, "/tmp/project", []string{"codex", "fork", managedSourceID}); !errors.Is(err, ErrManagedStale) {
			t.Fatalf("identity %q error=%v", id, err)
		}
	}
}

func TestOrdinaryCodexBindingUsesReadyServiceForPaneDiscovery(t *testing.T) {
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
		if got != target.Origin || cwd != "/tmp/project" || !reflect.DeepEqual(argv, []string{"codex"}) || gotService != managedServiceSnapshot() || policy != managedPolicy() {
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
	if verifier.closed != 0 || service.callCount() != 1 {
		t.Fatalf("closed=%d service=%d error=%v", verifier.closed, service.callCount(), err)
	}
}

func TestOrdinaryCodexBindingRequiresReadyServiceBeforeDiscovery(t *testing.T) {
	serviceErr := errors.New("service unavailable")
	service := &fakeManagedService{err: serviceErr}
	launcher := NewManagedCodexLauncher(Host{Runner: &scriptedRunner{}})
	launcher.service = service
	launcher.topology = &fakeManagedTopology{}
	discovered := false
	launcher.discover = func(context.Context, sessionhost.Origin, string, []string, CodexServiceSnapshot, ManagedCodexPolicy) (Agent, string, error) {
		discovered = true
		return Agent{}, "", nil
	}
	binding := &ordinaryCodexBinding{launcher: launcher, operation: ManagedNew, readyTimeout: DefaultPromptReadyTimeout}
	if _, err := binding.Prepare(context.Background(), sessionhost.CreatedTarget{}, "/tmp/project", []string{"codex"}); !errors.Is(err, serviceErr) {
		t.Fatalf("error=%v", err)
	}
	if discovered || service.callCount() != 1 || len(launcher.Host.Runner.(*scriptedRunner).calls) != 0 {
		t.Fatalf("discovered=%t service=%d", discovered, service.callCount())
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

func TestOrdinaryCodexPrefixResolutionBacksOffBeforeRetry(t *testing.T) {
	target := sessionhost.CreatedTarget{Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	unbound := managedAgent(target.Origin, "/tmp/project", managedNewID, false)
	unbound.Session = nil
	bound := managedAgent(target.Origin, "/tmp/project", managedNewID, false)
	prefix := managedNewID[:len(managedNewID)-4]
	identity := codexThreadIdentity{ID: managedNewID, CWD: "/tmp/project", ModelProvider: "openai", Model: "gpt-5.6-sol", ReasoningEffort: "medium"}
	verifier := &fakeManagedVerifier{listedSeq: [][]codexThreadIdentity{nil, {identity}}, read: []codexThreadIdentity{identity}}
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, unbound), managedAgentResult(t, unbound), managedAgentResult(t, unbound),
		{}, managedAgentResult(t, bound), managedAgentResult(t, bound),
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	launcher.topology = &fakeManagedTopology{}
	launcher.service = &fakeManagedService{snapshot: managedServiceSnapshot()}
	launcher.verifiers = fakeManagedVerifierFactory{verifier}
	binding := &ordinaryCodexBinding{launcher: launcher, operation: ManagedNew, readyTimeout: DefaultPromptReadyTimeout}
	started := time.Now()
	got, _, err := binding.resolvePersistedIdentity(context.Background(), target, "/tmp/project", []string{"codex"}, ordinaryCodexPreparation{Agent: unbound, Prefix: prefix, Policy: managedPolicy()})
	if err != nil || got.Session == nil || got.Session.Value != managedNewID || verifier.listCalls != 2 {
		t.Fatalf("agent=%+v listCalls=%d error=%v", got, verifier.listCalls, err)
	}
	if elapsed := time.Since(started); elapsed < 15*time.Millisecond {
		t.Fatalf("thread-list retry spun without backoff: %v", elapsed)
	}
}

func TestOrdinaryCodexResolvesNarrowStatusPrefixBeforeBinding(t *testing.T) {
	target := sessionhost.CreatedTarget{Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	unbound := managedAgent(target.Origin, "/tmp/project", managedNewID, false)
	unbound.Session = nil
	bound := managedAgent(target.Origin, "/tmp/project", managedNewID, false)
	prefix := managedNewID[:len(managedNewID)-10]
	identity := codexThreadIdentity{ID: managedNewID, CWD: "/tmp/project", ModelProvider: "openai", Model: "gpt-5.6-sol", ReasoningEffort: "medium"}
	verifier := &fakeManagedVerifier{listed: []codexThreadIdentity{identity}, read: []codexThreadIdentity{identity}}
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, unbound), managedAgentResult(t, unbound),
		{}, managedAgentResult(t, bound), managedAgentResult(t, bound),
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	launcher.topology = &fakeManagedTopology{}
	launcher.service = &fakeManagedService{snapshot: managedServiceSnapshot()}
	launcher.verifiers = fakeManagedVerifierFactory{verifier}
	binding := &ordinaryCodexBinding{launcher: launcher, operation: ManagedNew, readyTimeout: DefaultPromptReadyTimeout}
	got, policy, err := binding.resolvePersistedIdentity(context.Background(), target, "/tmp/project", []string{"codex"}, ordinaryCodexPreparation{
		Agent: unbound, Prefix: prefix,
		Policy: ManagedCodexPolicy{Model: "gpt-5.6-sol", ReasoningEffort: "med"}, policyIsPrefix: true,
	})
	wantPolicy := ManagedCodexPolicy{Model: "gpt-5.6-sol", ReasoningEffort: "medium"}
	if err != nil || got.Session == nil || got.Session.Value != managedNewID || policy != wantPolicy || verifier.listCalls != 1 {
		t.Fatalf("agent=%+v policy=%+v listCalls=%d error=%v", got, policy, verifier.listCalls, err)
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
