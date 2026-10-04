package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

// newManagedLaunchForkHarness scripts a Fork whose every step succeeds; rows then
// break exactly one observation.
func newManagedLaunchForkHarness(t *testing.T) (ManagedCodexLauncher, *fakeManagedTopology, *fakeManagedVerifier, *scriptedRunner, ManagedCodexRequest) {
	t.Helper()
	launcher, _, topology, verifier, runner, request := newManagedHarness(t, ManagedFork, managedChildID)
	request.Placement, request.Direction = sessionhost.NewPane, "right"
	source := managedAgent(request.Origin, request.Directory, managedSourceID, false)
	request.Source = source
	verifier.confirm.ForkedFromID, verifier.recheck.ForkedFromID = managedSourceID, managedSourceID
	identity := codexThreadIdentity{ID: managedSourceID, CWD: request.Directory, ModelProvider: "openai"}
	verifier.read = []codexThreadIdentity{identity, identity}
	child := managedAgent(topology.target.Origin, request.Directory, managedChildID, false)
	runner.results = []command.Result{
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("header")},
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("header")},
		{}, managedAgentResult(t, child), managedAgentResult(t, child),
		jsonResult(t, listResponse(wireFromAgent(source))), managedAgentResult(t, child),
	}
	return launcher, topology, verifier, runner, request
}

func TestManagedCodexCleanupWithoutOwnedTargetIsNoOp(t *testing.T) {
	if err := (*ManagedCodex)(nil).Cleanup(context.Background()); err != nil {
		t.Fatalf("nil result cleanup=%v", err)
	}
	if err := (&ManagedCodex{}).Cleanup(context.Background()); err != nil {
		t.Fatalf("unowned result cleanup=%v", err)
	}
}

func TestManagedCodexFailureReportsIncompleteCleanupOnlyForUnprovenPane(t *testing.T) {
	for _, test := range []struct {
		name       string
		configure  func(*testing.T, *ManagedCodexLauncher, *fakeManagedVerifier, *scriptedRunner, *fakeManagedTopology, ManagedCodexRequest)
		incomplete bool
	}{
		{"no pane yet", func(_ *testing.T, _ *ManagedCodexLauncher, verifier *fakeManagedVerifier, _ *scriptedRunner, _ *fakeManagedTopology, _ ManagedCodexRequest) {
			verifier.preflightErr = errors.New("unsupported requirement")
		}, false},
		{"proven identity drifted", func(t *testing.T, _ *ManagedCodexLauncher, verifier *fakeManagedVerifier, runner *scriptedRunner, topology *fakeManagedTopology, request ManagedCodexRequest) {
			verifier.recheck = codexThreadIdentity{ID: "drift", CWD: request.Directory, ModelProvider: "openai"}
			runner.results = managedNewPromptResults(t, topology, request)
		}, false},
		{"unproven identity", func(_ *testing.T, launcher *ManagedCodexLauncher, _ *fakeManagedVerifier, _ *scriptedRunner, _ *fakeManagedTopology, _ ManagedCodexRequest) {
			launcher.discover = func(context.Context, sessionhost.Origin, string, []string, CodexServiceSnapshot, ManagedCodexPolicy) (Agent, string, error) {
				return Agent{}, "", ErrManagedNarrow
			}
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			launcher, _, topology, verifier, runner, request := newManagedHarness(t, ManagedNew, managedNewID)
			test.configure(t, &launcher, verifier, runner, topology, request)
			_, err := launcher.Launch(context.Background(), request)
			if err == nil || errors.Is(err, ErrManagedCleanup) != test.incomplete || verifier.closed != 1 {
				t.Fatalf("error=%v incomplete=%t closed=%d", err, test.incomplete, verifier.closed)
			}
		})
	}
}

func TestManagedCodexConversionResumeRefusesConflictingReviewerBeforeTopology(t *testing.T) {
	launcher, _, topology, verifier, _, request := newManagedHarness(t, ManagedResume, managedOriginalID)
	request.ThreadID, request.Placement = managedOriginalID, ""
	source := managedPreflight(request.Directory).EffectiveCodexPolicy
	source.ModelProvider = "openai"
	request.resumeSourcePolicy, request.resumeTargetPolicy = &source, &source
	verifier.constraints = conversionConstraints()
	verifier.constraints.Config["approvals_reviewer"] = json.RawMessage(`"auto_review"`)
	if _, err := launcher.Launch(context.Background(), request); !errors.Is(err, ErrManagedPolicyDrift) || verifier.armed != 0 || len(topology.runs) != 0 {
		t.Fatalf("error=%v armed=%d runs=%v", err, verifier.armed, topology.runs)
	}
}

func TestManagedCodexForkRefusesSourceThatChangedBeforeLaunch(t *testing.T) {
	const staleSource = "managed Codex source read"
	other := codexThreadIdentity{ID: managedSourceID, CWD: "/tmp/other", ModelProvider: "openai"}
	for _, test := range []struct {
		name      string
		configure func(*fakeManagedVerifier)
		want      string
	}{
		{"thread directory", func(verifier *fakeManagedVerifier) { verifier.read = []codexThreadIdentity{other} }, staleSource},
		{"effective directory", func(verifier *fakeManagedVerifier) { verifier.audit.CWD = "/tmp/other" }, "managed Codex source policy"},
		{"approval policy", func(verifier *fakeManagedVerifier) { verifier.audit.ApprovalPolicy = "on-request" }, "managed Codex source policy"},
		{"sandbox mode", func(verifier *fakeManagedVerifier) { verifier.audit.SandboxMode = "read-only" }, "managed Codex source policy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			launcher, topology, verifier, _, request := newManagedLaunchForkHarness(t)
			test.configure(verifier)
			_, err := launcher.Launch(context.Background(), request)
			if err == nil || !strings.Contains(err.Error(), test.want) || len(topology.launches) != 0 {
				t.Fatalf("error=%v launches=%+v", err, topology.launches)
			}
		})
	}
}

func TestManagedCodexResumeRefusesSourceThatChangedBeforeLaunch(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*fakeManagedVerifier)
		want      string
	}{
		{"thread directory", func(verifier *fakeManagedVerifier) {
			verifier.read = []codexThreadIdentity{{ID: managedOriginalID, CWD: "/tmp/other", ModelProvider: "openai"}}
		}, "managed Codex resume source"},
		{"effective directory", func(verifier *fakeManagedVerifier) { verifier.audit.CWD = "/tmp/other" }, "managed Codex resume policy"},
		{"policy", func(verifier *fakeManagedVerifier) { verifier.audit.SandboxMode = "read-only" }, "managed Codex resume policy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			launcher, _, topology, verifier, _, request := newManagedHarness(t, ManagedResume, managedOriginalID)
			request.ThreadID, request.Placement = managedOriginalID, ""
			verifier.read = []codexThreadIdentity{{ID: managedOriginalID, CWD: request.Directory, ModelProvider: "openai"}}
			test.configure(verifier)
			_, err := launcher.Launch(context.Background(), request)
			if err == nil || !strings.Contains(err.Error(), test.want) || len(topology.runs) != 0 {
				t.Fatalf("error=%v runs=%v", err, topology.runs)
			}
		})
	}
}

func TestManagedCodexIdentityFailureKeepsItsCause(t *testing.T) {
	cause := errors.New("confirmation failed")
	t.Run("confirmation error", func(t *testing.T) {
		launcher, _, _, verifier, _, request := newManagedHarness(t, ManagedNew, managedNewID)
		verifier.confirmErr = cause
		if _, err := launcher.Launch(context.Background(), request); !errors.Is(err, cause) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("directory mismatch", func(t *testing.T) {
		launcher, _, _, verifier, _, request := newManagedHarness(t, ManagedNew, managedNewID)
		verifier.confirm.CWD = "/tmp/other"
		if _, err := launcher.Launch(context.Background(), request); !errors.Is(err, ErrManagedStale) || !strings.Contains(err.Error(), "managed Codex identity: managed Codex state is stale") {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestManagedCodexForkRefusesSourceThatChangedDuringLaunch(t *testing.T) {
	changed := sessionhost.ForegroundProcess{PID: 78, Name: "codex", CWD: "/tmp/project", Argv: []string{"codex"}}
	same := sessionhost.ForegroundProcess{PID: 77, Name: "codex", CWD: "/tmp/project", Argv: []string{"codex"}}
	for _, test := range []struct {
		name      string
		configure func(*fakeManagedTopology, *fakeManagedVerifier, *scriptedRunner, *testing.T, ManagedCodexRequest)
		want      string
	}{
		{"thread", func(_ *fakeManagedTopology, verifier *fakeManagedVerifier, _ *scriptedRunner, _ *testing.T, request ManagedCodexRequest) {
			verifier.read[1] = codexThreadIdentity{ID: managedSourceID, CWD: request.Directory, ModelProvider: "other"}
		}, "managed Codex source changed"},
		{"pane identity", func(_ *fakeManagedTopology, _ *fakeManagedVerifier, runner *scriptedRunner, t *testing.T, request ManagedCodexRequest) {
			moved := managedAgent(request.Origin, request.Directory, managedChildID, false)
			runner.results[7] = jsonResult(t, listResponse(wireFromAgent(moved)))
		}, "managed Codex source pane changed"},
		{"pane directory", func(_ *fakeManagedTopology, _ *fakeManagedVerifier, runner *scriptedRunner, t *testing.T, request ManagedCodexRequest) {
			moved := managedAgent(request.Origin, "/tmp/other", managedSourceID, false)
			runner.results[7] = jsonResult(t, listResponse(wireFromAgent(moved)))
		}, "managed Codex source pane changed"},
		{"process", func(topology *fakeManagedTopology, _ *fakeManagedVerifier, _ *scriptedRunner, _ *testing.T, _ ManagedCodexRequest) {
			topology.inspectedSeq = []sessionhost.ForegroundProcess{same, same, changed}
		}, "managed Codex source process changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			launcher, topology, verifier, runner, request := newManagedLaunchForkHarness(t)
			test.configure(topology, verifier, runner, t, request)
			managed, err := launcher.Launch(context.Background(), request)
			if err == nil || managed != nil || !errors.Is(err, ErrManagedStale) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("managed=%+v error=%v", managed, err)
			}
		})
	}
}

func TestManagedCodexRefusesProcessThatChangedAfterIdentity(t *testing.T) {
	for _, test := range []struct {
		name  string
		focus bool
		pids  []int
		want  string
	}{
		{"final", false, []int{99, 100}, "managed Codex final process"},
		{"focused", true, []int{99, 99, 100}, "managed Codex focused process"},
	} {
		t.Run(test.name, func(t *testing.T) {
			launcher, _, topology, _, runner, request := newManagedHarness(t, ManagedNew, managedNewID)
			request.Focus = test.focus
			runner.results = managedNewPromptResults(t, topology, request)
			topology.pids = test.pids
			managed, err := launcher.Launch(context.Background(), request)
			if err == nil || managed != nil || !errors.Is(err, ErrManagedStale) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("managed=%+v error=%v", managed, err)
			}
		})
	}
}

// Only an acknowledged prompt keeps the pane when a later focus step fails.
// The injected delivery reports success without an acknowledgement.
func TestManagedCodexUnacknowledgedPromptStillCleansUpWhenFocusFails(t *testing.T) {
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
				return PromptResult{Target: target, Agent: target.Agent, State: CreatedPromptNotSent}, nil
			}
			managed, err := launcher.Launch(context.Background(), request)
			if err == nil || managed != nil || len(topology.cleanups) != 1 || len(verifier.deleted) != 1 {
				t.Fatalf("managed=%+v cleanup=%v deleted=%v error=%v", managed, topology.cleanups, verifier.deleted, err)
			}
		})
	}
}
