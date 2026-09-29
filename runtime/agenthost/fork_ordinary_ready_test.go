package agenthost

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func TestOrdinaryCodexForkResolvesIdentityBeforeFirstTurn(t *testing.T) {
	for _, tc := range []struct {
		name, prefix string
		wrongParent  bool
		serviceError bool
		ambiguous    bool
		discoveryErr error
		preserve     bool
		wantErr      error
	}{
		{name: "full identity"},
		{name: "persisted prefix", prefix: "01a09f5c-37cc-77f1-9409-e9aa070a"},
		{name: "wrong parent", wrongParent: true, preserve: true, wantErr: ErrManagedStale},
		{name: "missing service", serviceError: true, wantErr: ErrCodexServiceUnavailable},
		{name: "ambiguous prefix", prefix: "01a09f5c-37cc-77f1-9409-e9aa070a", ambiguous: true, wantErr: ErrManagedAmbiguous},
		{name: "child adopted during discovery", discoveryErr: ErrManagedPaneBusy, preserve: true, wantErr: ErrManagedPaneBusy},
		{name: "child changed during discovery", discoveryErr: ErrManagedStale, preserve: true, wantErr: ErrManagedStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
			created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
			unbound := managedAgent(created.Origin, source.CWD, managedChildID, false)
			unbound.Session = nil
			bound := managedAgent(created.Origin, source.CWD, managedChildID, false)
			process := sessionhost.ForegroundProcess{PID: 101, Name: "codex", CWD: source.CWD, Argv: []string{"codex"}}
			topology := &fakeManagedTopology{target: created, inspected: process}
			service := &fakeManagedService{snapshot: managedServiceSnapshot()}
			if tc.serviceError {
				service.err = ErrCodexServiceUnavailable
			}
			identity := codexThreadIdentity{ID: managedChildID, CWD: source.CWD, ForkedFromID: managedSourceID, ModelProvider: "openai", Model: "gpt-5.6-sol", ReasoningEffort: "medium"}
			if tc.wrongParent {
				identity.ForkedFromID = managedOriginalID
			}
			verifier := &fakeManagedVerifier{read: []codexThreadIdentity{identity}, listed: []codexThreadIdentity{identity}}
			if tc.ambiguous {
				other := identity
				other.ID = managedSecondID
				verifier.listed = append(verifier.listed, other)
			}
			results := []command.Result{
				listForkResult(t, source), jsonResult(t, listResponse(wireFromAgent(unbound))),
				managedAgentResult(t, source), {Stdout: managedEmptyComposer("header")},
				managedAgentResult(t, source), {Stdout: managedEmptyComposer("header")},
			}
			if tc.prefix != "" {
				results = append(results, listForkResult(t, source))
			}
			results = append(results,
				managedAgentResult(t, unbound), managedAgentResult(t, unbound), command.Result{},
				managedAgentResult(t, bound), managedAgentResult(t, bound),
				listForkResult(t, source), listForkResult(t, source),
			)
			runner := &scriptedRunner{results: results}
			launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
			launcher.service, launcher.topology, launcher.verifiers = service, topology, fakeManagedVerifierFactory{verifier}
			launcher.discoverObserved = func(context.Context, sessionhost.Origin, string, []string, CodexServiceSnapshot, ManagedCodexPolicy) (Agent, managedStatusObservation, error) {
				if tc.discoveryErr != nil {
					return Agent{}, managedStatusObservation{}, tc.discoveryErr
				}
				id := managedChildID
				if tc.prefix != "" {
					id = tc.prefix
				}
				return unbound, managedStatusObservation{identity: id, forkedFromPrefix: managedSourceID, policy: managedPolicy(), fromStatus: true}, nil
			}
			forker := NewForker(Host{Runner: runner, Session: "named"})
			forker.launcher, forker.managed = topology, launcher
			forker.wait = func(context.Context, time.Duration) error {
				return errors.New("unreported child should resolve before waiting")
			}
			forker.deliver = func(context.Context, Prompt, PromptTarget) (PromptResult, error) {
				t.Fatal("unprompted Fork delivered a prompt")
				return PromptResult{}, nil
			}
			result, err := forker.Fork(context.Background(), ForkRequest{Source: source, Profile: ForkProfile{Executable: "codex"}, Placement: sessionhost.NewTab})
			if tc.wantErr != nil {
				cleanupCount := 1
				if tc.preserve {
					cleanupCount = 0
					if result.Created != created || !errors.Is(err, ErrForkOutcomeUncertain) {
						t.Errorf("lost uncertain created target: result=%+v error=%v", result, err)
					}
				}
				if !errors.Is(err, tc.wantErr) || len(topology.cleanups) != cleanupCount || hasAgentSessionReport(runner.calls) {
					t.Fatalf("error=%v cleanup=%v calls=%q", err, topology.cleanups, runner.calls)
				}
				return
			}
			if err != nil || result.Child.Session == nil || result.Child.Session.Value != managedChildID || result.Source.Session.Value != managedSourceID || result.Prompt.State != "" {
				t.Fatalf("result=%+v error=%v calls=%q", result, err, runner.calls)
			}
			if len(topology.cleanups) != 0 || !hasAgentSessionReport(runner.calls) || verifier.confirmID != "" || len(verifier.deleted) != 0 {
				t.Fatalf("cleanup=%v verifier=%+v calls=%q", topology.cleanups, verifier, runner.calls)
			}
		})
	}
}

func TestOrdinaryCodexNewStillRefusesUnmaterializedPrefix(t *testing.T) {
	target := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	unbound := managedAgent(target.Origin, "/tmp/project", managedNewID, false)
	unbound.Session = nil
	runner := &scriptedRunner{}
	verifier := &fakeManagedVerifier{read: []codexThreadIdentity{{ID: managedNewID}}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	launcher.topology = &fakeManagedTopology{}
	launcher.service = &fakeManagedService{snapshot: managedServiceSnapshot()}
	launcher.verifiers = fakeManagedVerifierFactory{verifier}
	launcher.discoverObserved = func(context.Context, sessionhost.Origin, string, []string, CodexServiceSnapshot, ManagedCodexPolicy) (Agent, managedStatusObservation, error) {
		return unbound, managedStatusObservation{identity: "01a09f5c-37cc-77f1-9409-e9aa070a", policy: managedPolicy()}, nil
	}
	binding, err := beginOrdinaryCodexBinding(context.Background(), launcher, ManagedNew, Agent{}, "/tmp/project", managedPolicy(), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = binding.Finish(context.Background(), target, "/tmp/project", []string{"codex"})
	if !errors.Is(err, ErrManagedNarrow) || len(runner.calls) != 0 || len(verifier.read) != 1 || verifier.closed != 0 {
		t.Fatalf("error=%v calls=%q verifier=%+v", err, runner.calls, verifier)
	}
}

func TestForkUnobservedChildDoesNotProducePreservedReceipt(t *testing.T) {
	for _, lookupErr := range []error{errors.New("child lookup failed"), ErrForkOutcomeUncertain} {
		t.Run(lookupErr.Error(), func(t *testing.T) {
			source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
			created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
			topology := &fakeTopology{created: created}
			runner := &scriptedRunner{
				results: []command.Result{listForkResult(t, source), {}},
				errs:    []error{nil, lookupErr},
			}
			forker := NewForker(Host{Runner: runner, Session: "named"})
			forker.launcher = topology
			result, err := forker.Fork(context.Background(), ForkRequest{Source: source, Profile: ForkProfile{Executable: "codex"}, Placement: sessionhost.NewTab})
			if !errors.Is(err, lookupErr) || result.Created != (sessionhost.CreatedTarget{}) || result.Child.PaneID != "" {
				t.Fatalf("unobserved child produced a preserved receipt: result=%+v error=%v", result, err)
			}
			if len(topology.cleanupCalls) != 1 || topology.cleanupCalls[0] != created {
				t.Fatalf("cleanup did not retain exact created-target ownership: %+v", topology.cleanupCalls)
			}
		})
	}
}
