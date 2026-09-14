package agenthost

import (
	"context"
	"errors"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func TestManagedCodexPromptRunsAfterExactBackgroundIdentity(t *testing.T) {
	launcher, _, topology, verifier, runner, request := newManagedHarness(t, ManagedNew, managedNewID)
	prompt, _ := PreparePrompt("hello 🚀")
	request.Prompt = &prompt
	runner.results = managedNewPromptResults(t, topology, request)
	deliveries := 0
	launcher.deliver = func(_ context.Context, got Prompt, target PromptTarget) (PromptResult, error) {
		deliveries++
		if got.text != prompt.text || target.Agent.Session == nil || target.Agent.Session.Value != managedNewID || target.Source != nil || target.Agent.Focused {
			t.Fatalf("prompt=%q target=%+v", got.text, target)
		}
		return PromptResult{Target: target, Agent: target.Agent, State: PromptAcknowledged}, nil
	}
	managed, err := launcher.Launch(context.Background(), request)
	if err != nil || managed.Prompt.State != PromptAcknowledged || deliveries != 1 || len(topology.cleanups) != 0 || len(verifier.deleted) != 0 {
		t.Fatalf("managed=%+v deliveries=%d cleanup=%v deleted=%v error=%v", managed, deliveries, topology.cleanups, verifier.deleted, err)
	}
}

func TestManagedCodexForkPromptTargetsDistinctChildNeverSource(t *testing.T) {
	launcher, _, topology, verifier, runner, request := newManagedHarness(t, ManagedFork, managedChildID)
	request.Placement, request.Direction = sessionhost.NewPane, "right"
	source := managedAgent(request.Origin, request.Directory, managedSourceID, false)
	request.Source = source
	verifier.confirm.ForkedFromID, verifier.recheck.ForkedFromID = managedSourceID, managedSourceID
	verifier.read = []codexThreadIdentity{{ID: managedSourceID, CWD: request.Directory, ModelProvider: "openai"}, {ID: managedSourceID, CWD: request.Directory, ModelProvider: "openai"}}
	runner.results = []command.Result{
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("header")},
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("header")},
		{},
		managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedChildID, false)),
		managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedChildID, false)),
		jsonResult(t, listResponse(wireFromAgent(source))),
		managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedChildID, false)),
	}
	prompt, _ := PreparePrompt("fork prompt")
	request.Prompt = &prompt
	launcher.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
		if target.Source == nil || target.Source.Session.Value != managedSourceID || target.Agent.Session.Value != managedChildID || target.Source.PaneID == target.Agent.PaneID {
			t.Fatalf("target=%+v", target)
		}
		return PromptResult{Target: target, Agent: target.Agent, State: PromptAcknowledged}, nil
	}
	managed, err := launcher.Launch(context.Background(), request)
	if err != nil || managed.Prompt.State != PromptAcknowledged || managed.ThreadID != managedChildID || managed.SourceThreadID != managedSourceID {
		t.Fatalf("managed=%+v error=%v", managed, err)
	}
}

func TestManagedCodexPromptTransportFailureLeavesCreatedConversation(t *testing.T) {
	for _, state := range []PromptState{CreatedPromptNotSent, CreatedPromptOutcomeUnknown} {
		t.Run(string(state), func(t *testing.T) {
			launcher, _, topology, verifier, runner, request := newManagedHarness(t, ManagedNew, managedNewID)
			prompt, _ := PreparePrompt("hello")
			request.Prompt = &prompt
			runner.results = managedNewPromptResults(t, topology, request)
			launcher.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
				return PromptResult{Target: target, Agent: target.Agent, State: state}, errors.New("transport failed")
			}
			managed, err := launcher.Launch(context.Background(), request)
			if err == nil || managed == nil || managed.Prompt.State != state || len(topology.cleanups) != 0 || len(verifier.deleted) != 0 || verifier.closed != 1 {
				t.Fatalf("managed=%+v cleanup=%v deleted=%v closed=%d error=%v", managed, topology.cleanups, verifier.deleted, verifier.closed, err)
			}
		})
	}
}

func TestManagedCodexPromptReadinessFailureUsesExistingOwnedCleanup(t *testing.T) {
	launcher, _, topology, verifier, runner, request := newManagedHarness(t, ManagedNew, managedNewID)
	prompt, _ := PreparePrompt("hello")
	request.Prompt = &prompt
	runner.results = managedNewPromptResults(t, topology, request)
	launcher.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
		return PromptResult{Target: target, Agent: target.Agent, State: PromptNotAttempted}, ErrPromptStale
	}
	managed, err := launcher.Launch(context.Background(), request)
	if !errors.Is(err, ErrPromptStale) || managed != nil || len(topology.cleanups) != 1 || len(verifier.deleted) != 1 || verifier.deleted[0] != managedNewID {
		t.Fatalf("managed=%+v cleanup=%v deleted=%v error=%v", managed, topology.cleanups, verifier.deleted, err)
	}
}

func TestManagedCodexPostPromptFocusFailuresPreserveAcknowledgedConversation(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*fakeManagedTopology, *fakeManagedVerifier, *scriptedRunner)
	}{
		{"focus", func(topology *fakeManagedTopology, _ *fakeManagedVerifier, _ *scriptedRunner) {
			topology.focusErr = errors.New("focus failed")
		}},
		{"process", func(topology *fakeManagedTopology, _ *fakeManagedVerifier, _ *scriptedRunner) {
			topology.verifyErrs = []error{nil, nil, errors.New("process changed")}
		}},
		{"target", func(_ *fakeManagedTopology, _ *fakeManagedVerifier, runner *scriptedRunner) {
			runner.errs = []error{nil, nil, nil, nil, errors.New("target changed")}
		}},
		{"identity", func(_ *fakeManagedTopology, verifier *fakeManagedVerifier, _ *scriptedRunner) {
			drifted := verifier.recheck
			drifted.CWD = "/tmp/other"
			verifier.rechecks = []codexThreadIdentity{verifier.recheck, drifted}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			launcher, _, topology, verifier, runner, request := newManagedHarness(t, ManagedNew, managedNewID)
			request.Focus = true
			prompt, _ := PreparePrompt("hello")
			request.Prompt = &prompt
			runner.results = append(managedNewPromptResults(t, topology, request), managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, true)))
			test.configure(topology, verifier, runner)
			launcher.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
				return PromptResult{Target: target, Agent: target.Agent, State: PromptAcknowledged}, nil
			}
			managed, err := launcher.Launch(context.Background(), request)
			if err == nil || managed == nil || managed.Prompt.State != PromptAcknowledged || len(topology.cleanups) != 0 || len(verifier.deleted) != 0 || verifier.closed != 1 {
				t.Fatalf("managed=%+v cleanup=%v deleted=%v closed=%d error=%v", managed, topology.cleanups, verifier.deleted, verifier.closed, err)
			}
		})
	}
}

func TestManagedCodexInvalidOrCancelledPromptStopsBeforeSharedService(t *testing.T) {
	for _, test := range []struct {
		name   string
		ctx    context.Context
		prompt Prompt
	}{
		{"invalid", context.Background(), Prompt{}},
		{"cancelled", cancelledContext(), func() Prompt { prompt, _ := PreparePrompt("hello"); return prompt }()},
	} {
		t.Run(test.name, func(t *testing.T) {
			launcher, service, _, _, _, request := newManagedHarness(t, ManagedNew, managedNewID)
			request.Prompt = &test.prompt
			if managed, err := launcher.Launch(test.ctx, request); err == nil || managed != nil || service.callCount() != 0 {
				t.Fatalf("managed=%+v service=%d error=%v", managed, service.callCount(), err)
			}
		})
	}
}

func managedNewPromptResults(t *testing.T, topology *fakeManagedTopology, request ManagedCodexRequest) []command.Result {
	return []command.Result{
		{},
		managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false)),
		managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false)),
		managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false)),
	}
}
