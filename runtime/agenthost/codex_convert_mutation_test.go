package agenthost

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

// codexConvertLaunchManaged makes every managed launch attempt succeed with the given result.
func (h *conversionHarness) codexConvertLaunchManaged(managed *ManagedCodex) {
	h.converter.launch = func(_ context.Context, request ManagedCodexRequest) (*ManagedCodex, error) {
		h.launches = append(h.launches, request)
		return managed, nil
	}
}

// codexConvertAllowOrdinaryFallback queues the extra thread and history reads plus the two
// agent polls that restoring the ordinary session needs.
func (h *conversionHarness) codexConvertAllowOrdinaryFallback(t *testing.T) {
	t.Helper()
	h.verifier.identities = []codexThreadIdentity{h.identity, h.identity, h.identity, h.identity}
	h.verifier.histories = [][32]byte{h.history, h.history, h.history, h.history}
	h.runner.results = append(h.runner.results, managedAgentResult(t, h.source), managedAgentResult(t, h.source))
}

// codexConvertStampedRunner records when each herdr command ran and can cancel its context
// after a fixed number of commands.
type codexConvertStampedRunner struct {
	command.Runner
	times       []time.Time
	cancelAfter int
	cancel      context.CancelFunc
}

func (r *codexConvertStampedRunner) Run(ctx context.Context, executable string, args []string) (command.Result, error) {
	r.times = append(r.times, time.Now())
	result, err := r.Runner.Run(ctx, executable, args)
	if r.cancel != nil && len(r.times) == r.cancelAfter {
		r.cancel()
	}
	return result, err
}

// codexConvertShellCaptureConverter returns a converter that runs the real shell capture
// against the scripted herdr results.
func (h *conversionHarness) codexConvertShellCaptureConverter(results ...command.Result) (CodexConverter, *codexConvertStampedRunner) {
	h.runner.results = results
	h.runner.calls = nil
	runner := &codexConvertStampedRunner{Runner: h.runner}
	converter := CodexConverter{launcher: h.converter.launcher}
	converter.launcher.Host.Runner = runner
	return converter, runner
}

func codexConvertShellObservation(t *testing.T, origin sessionhost.Origin, cwd, visible string) []command.Result {
	t.Helper()
	return []command.Result{
		conversionPaneResult(t, origin, cwd, false, 17), {Stdout: []byte(visible)}, conversionPaneResult(t, origin, cwd, false, 17),
	}
}

func TestCodexConverterPostStopCaptureRestoresOrdinaryThreadOnEachDivergence(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func(*conversionHarness)
		wantErr error
		wantMsg string
	}{
		{"thread identity", func(h *conversionHarness) { h.verifier.identities[1].Model = "changed" }, ErrManagedStale, "thread identity changed"},
		{"stable history", func(h *conversionHarness) { h.verifier.histories[1] = [32]byte{9} }, ErrManagedStale, "stable history changed"},
		{"effective directory", func(h *conversionHarness) { h.verifier.policies[0].CWD = "/tmp/other" }, ErrManagedPolicyDrift, "captured identity policy changed"},
		{"effective provider", func(h *conversionHarness) { h.verifier.policies[0].ModelProvider = "other" }, ErrManagedPolicyDrift, "captured identity policy changed"},
		{"effective model", func(h *conversionHarness) { h.verifier.policies[0].Model = "other" }, ErrManagedPolicyDrift, "captured identity policy changed"},
		{"effective reasoning effort", func(h *conversionHarness) { h.verifier.policies[0].ReasoningEffort = "high" }, ErrManagedPolicyDrift, "captured identity policy changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newConversionHarness(t)
			h.codexConvertAllowOrdinaryFallback(t)
			test.change(h)
			h.codexConvertLaunchManaged(h.managedResult())
			result, err := h.converter.Convert(context.Background(), h.request())
			if !errors.Is(err, ErrCodexConversionFallback) || !errors.Is(err, ErrCodexConversionCapture) || !errors.Is(err, test.wantErr) || !strings.Contains(err.Error(), test.wantMsg) || !result.FallbackUsed || len(h.launches) != 0 || len(h.topology.runs) != 1 {
				t.Fatalf("result=%+v error=%v launches=%d runs=%d, want ordinary restore with %q", result, err, len(h.launches), len(h.topology.runs), test.wantMsg)
			}
		})
	}
}

func TestCodexConverterFinalVerificationRejectsEachDivergence(t *testing.T) {
	unavailable := errors.New("read unavailable")
	for _, test := range []struct {
		name    string
		change  func(*conversionHarness)
		wantErr error
	}{
		{"history read", func(h *conversionHarness) { h.verifier.historyErrs = []error{nil, nil, unavailable} }, unavailable},
		{"thread read", func(h *conversionHarness) { h.verifier.identityErrs = []error{nil, nil, unavailable} }, unavailable},
		{"effective read", func(h *conversionHarness) { h.verifier.policyErrs = []error{nil, unavailable} }, unavailable},
		{"history", func(h *conversionHarness) { h.verifier.histories[2] = [32]byte{9} }, nil},
		{"thread identity", func(h *conversionHarness) { h.verifier.identities[2].Model = "changed" }, nil},
		{"effective policy", func(h *conversionHarness) { h.verifier.policies[1].Model = "changed" }, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newConversionHarness(t)
			test.change(h)
			managed := h.managedResult()
			h.codexConvertLaunchManaged(managed)
			result, err := h.converter.Convert(context.Background(), h.request())
			if !errors.Is(err, ErrManagedStale) || !strings.Contains(err.Error(), "final verification") || test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("error=%v, want a final verification failure", err)
			}
			if result.Outcome != CodexConversionManaged || result.Managed != managed || len(h.launches) != 1 || len(h.topology.runs) != 0 {
				t.Fatalf("result=%+v launches=%d runs=%d", result, len(h.launches), len(h.topology.runs))
			}
		})
	}
}

func TestCodexConverterRejectsManagedResultForAnotherThreadBeforeFinalVerification(t *testing.T) {
	h := newConversionHarness(t)
	managed := h.managedResult()
	managed.ThreadID = "other-thread"
	h.codexConvertLaunchManaged(managed)
	result, err := h.converter.Convert(context.Background(), h.request())
	if !errors.Is(err, ErrManagedStale) || result.Managed != managed || len(h.verifier.histories) != 1 {
		t.Fatalf("result=%+v error=%v unread histories=%d, want rejection before the final reads", result, err, len(h.verifier.histories))
	}
}

func TestCodexConvertManagedResultRejectsEachMismatch(t *testing.T) {
	h := newConversionHarness(t)
	request := ManagedCodexRequest{Operation: ManagedResume, Origin: managedOrigin(), Directory: h.source.CWD, ThreadID: managedSourceID}
	service := h.service.snapshot
	if err := validateManagedConversionResult(h.managedResult(), request, service, h.policy); err != nil {
		t.Fatalf("matching managed result rejected: %v", err)
	}
	if err := validateManagedConversionResult(nil, request, service, h.policy); !errors.Is(err, ErrManagedStale) {
		t.Fatalf("missing managed result error=%v", err)
	}
	for name, change := range map[string]func(*ManagedCodex){
		"operation":     func(managed *ManagedCodex) { managed.Operation = ManagedFork },
		"thread":        func(managed *ManagedCodex) { managed.ThreadID = "other-thread" },
		"source thread": func(managed *ManagedCodex) { managed.SourceThreadID = "other-thread" },
		"origin":        func(managed *ManagedCodex) { managed.Target.Origin.PaneID = "w1:p9" },
		"service":       func(managed *ManagedCodex) { managed.Service.SocketPath = "/tmp/other.sock" },
		"missing session": func(managed *ManagedCodex) {
			managed.Agent.Session = nil
		},
		"session thread": func(managed *ManagedCodex) {
			session := *managed.Agent.Session
			session.Value = "other-thread"
			managed.Agent.Session = &session
		},
		"directory": func(managed *ManagedCodex) { managed.Process.CWD = "/tmp/other" },
		"policy":    func(managed *ManagedCodex) { managed.Policy.Model = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			managed := h.managedResult()
			change(managed)
			if err := validateManagedConversionResult(managed, request, service, h.policy); !errors.Is(err, ErrManagedStale) {
				t.Fatalf("mismatched managed result error=%v", err)
			}
		})
	}
}

func TestCodexConverterStockReasoningReadbackDecidesRestoreOrStop(t *testing.T) {
	unavailable := errors.New("app server unavailable")
	for _, test := range []struct {
		name        string
		arrange     func(*conversionHarness)
		wantRestore bool
	}{
		{"update accepted but thread readback unavailable", func(h *conversionHarness) {
			h.verifier.identityErrs = []error{nil, nil, nil, unavailable}
		}, false},
		{"update dispatched and both readbacks unavailable", func(h *conversionHarness) {
			h.verifier.applyErr = unavailable
			h.verifier.identityErrs = []error{nil, nil, nil, unavailable}
			h.verifier.historyErrs = []error{nil, nil, unavailable}
		}, false},
		{"not dispatched with thread readback unavailable", func(h *conversionHarness) {
			h.verifier.applyErr, h.verifier.applyNoDispatch = unavailable, true
			h.verifier.identityErrs = []error{nil, nil, nil, unavailable}
		}, true},
		{"not dispatched with unchanged thread and history readback unavailable", func(h *conversionHarness) {
			h.verifier.applyErr, h.verifier.applyNoDispatch = unavailable, true
			h.verifier.identities[3] = h.verifier.identities[2]
			h.verifier.histories[2] = [32]byte{9}
			h.verifier.historyErrs = []error{nil, nil, unavailable}
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, bound := stockDefaultConversionHarness(t)
			test.arrange(h)
			h.runner.results = append(h.runner.results, managedAgentResult(t, bound), managedAgentResult(t, bound))
			managed := h.managedResult()
			managed.Agent = bound
			h.codexConvertLaunchManaged(managed)
			result, err := h.converter.Convert(context.Background(), h.request())
			if !errors.Is(err, ErrManagedSettingsUnknown) || len(h.launches) != 0 || conversionExitCount(h.runner.calls) != 1 {
				t.Fatalf("result=%+v error=%v launches=%d", result, err, len(h.launches))
			}
			restored := errors.Is(err, ErrCodexConversionFallback)
			if restored != test.wantRestore || result.FallbackUsed != test.wantRestore || len(h.topology.runs) != codexConvertBoolInt(test.wantRestore) {
				t.Fatalf("restored=%t want=%t result=%+v ordinary resumes=%d", restored, test.wantRestore, result, len(h.topology.runs))
			}
		})
	}
}

func codexConvertBoolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestCodexConversionShellCaptureWaitsForTwoIdenticalObservations(t *testing.T) {
	h := newConversionHarness(t)
	origin := managedOrigin()
	var results []command.Result
	for _, visible := range []string{"first", "second", "second"} {
		results = append(results, codexConvertShellObservation(t, origin, h.source.CWD, visible)...)
	}
	converter, runner := h.codexConvertShellCaptureConverter(results...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := converter.captureConversionShell(ctx, origin, h.source.CWD, nil)
	want := conversionShellLease{Process: h.topology.inspected, Revision: 17, Visible: "second"}
	if err != nil || !sameConversionShellLease(lease, want) {
		t.Fatalf("lease=%+v error=%v, want %+v", lease, err, want)
	}
	if len(runner.times) != 9 {
		t.Fatalf("herdr commands=%d, want 3 observations of 3 commands (a changed draft must restart the comparison)", len(runner.times))
	}
	for observation := 1; observation < 3; observation++ {
		if gap := runner.times[3*observation].Sub(runner.times[3*observation-1]); gap < 25*time.Millisecond {
			t.Fatalf("observation %d began %v after the previous one, want the 25ms poll interval", observation+1, gap)
		}
	}
}

func TestCodexConversionShellCaptureRejectsDifferentExpectedProcess(t *testing.T) {
	h := newConversionHarness(t)
	origin := managedOrigin()
	converter, _ := h.codexConvertShellCaptureConverter(codexConvertShellObservation(t, origin, h.source.CWD, "prompt")...)
	expected := h.topology.inspected
	expected.PID++
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := converter.captureConversionShell(ctx, origin, h.source.CWD, &expected); !errors.Is(err, ErrManagedStale) {
		t.Fatalf("error=%v, want a stale shell process", err)
	}
}

func TestCodexConversionShellObservationRejectsEachInvalidPaneDescription(t *testing.T) {
	origin := managedOrigin()
	for _, test := range []struct {
		name   string
		change func(response, result, pane map[string]any)
	}{
		{"response id", func(response, _, _ map[string]any) { response["id"] = "cli:pane:list" }},
		{"result type", func(_, result, _ map[string]any) { result["type"] = "pane_list" }},
		{"undecodable pane", func(_, _, pane map[string]any) { pane["focused"] = "yes" }},
		{"pane id", func(_, _, pane map[string]any) { pane["pane_id"] = "w1:p9" }},
		{"workspace", func(_, _, pane map[string]any) { pane["workspace_id"] = "w9" }},
		{"tab", func(_, _, pane map[string]any) { pane["tab_id"] = "w1:t9" }},
		{"directory", func(_, _, pane map[string]any) { pane["cwd"] = "/tmp/other" }},
		{"zero revision", func(_, _, pane map[string]any) { pane["revision"] = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newConversionHarness(t)
			pane := map[string]any{
				"pane_id": origin.PaneID, "workspace_id": origin.WorkspaceID, "tab_id": origin.TabID,
				"cwd": h.source.CWD, "focused": false, "revision": 17,
			}
			result := map[string]any{"type": "pane_info", "pane": pane}
			response := map[string]any{"id": "cli:pane:get", "result": result}
			test.change(response, result, pane)
			converter, runner := h.codexConvertShellCaptureConverter(jsonResult(t, response))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := converter.observeConversionShell(ctx, origin, h.source.CWD); !errors.Is(err, ErrManagedStale) || len(runner.times) != 1 {
				t.Fatalf("error=%v commands=%d, want a stale pane before reading its text", err, len(runner.times))
			}
		})
	}
}

func TestCodexConverterFallbackReadbackMismatchLeavesRestoredThreadUnverified(t *testing.T) {
	unavailable := errors.New("readback unavailable")
	for _, test := range []struct {
		name   string
		change func(*conversionHarness)
	}{
		{"thread read", func(h *conversionHarness) { h.verifier.identityErrs = []error{nil, nil, nil, unavailable} }},
		{"history read", func(h *conversionHarness) { h.verifier.historyErrs = []error{nil, nil, nil, unavailable} }},
		{"thread", func(h *conversionHarness) { h.verifier.identities[3].Model = "changed" }},
		{"history", func(h *conversionHarness) { h.verifier.histories[3] = [32]byte{9} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newConversionHarness(t)
			h.verifier.policies = []EffectiveCodexPolicy{{}}
			h.verifier.policyErrs = []error{errors.New("capture unavailable")}
			h.codexConvertAllowOrdinaryFallback(t)
			test.change(h)
			result, err := h.converter.Convert(context.Background(), h.request())
			if !errors.Is(err, ErrManagedStale) || errors.Is(err, ErrCodexConversionFallback) || result.FallbackUsed || result.Outcome != "" || len(h.topology.runs) != 1 {
				t.Fatalf("result=%+v error=%v ordinary resumes=%d, want an unverified restore", result, err, len(h.topology.runs))
			}
		})
	}
}

func TestCodexConverterRetryRequiresUnchangedServiceThreadAndHistory(t *testing.T) {
	launchFailure := errors.New("temporary launch failure")
	for _, test := range []struct {
		name         string
		serviceMoves bool
		change       func(*conversionHarness)
	}{
		{"service", true, func(*conversionHarness) {}},
		{"thread", false, func(h *conversionHarness) { h.verifier.identities[2].Model = "changed" }},
		{"history", false, func(h *conversionHarness) { h.verifier.histories[2] = [32]byte{9} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newConversionHarness(t)
			test.change(h)
			h.converter.launch = func(_ context.Context, request ManagedCodexRequest) (*ManagedCodex, error) {
				h.launches = append(h.launches, request)
				if test.serviceMoves {
					h.service.snapshot.SocketPath = "/tmp/moved.sock"
				}
				return nil, launchFailure
			}
			result, err := h.converter.Convert(context.Background(), h.request())
			if !errors.Is(err, launchFailure) || !errors.Is(err, ErrManagedStale) || len(h.launches) != 1 || result.ManagedAttempts != 1 || len(h.topology.runs) != 0 {
				t.Fatalf("result=%+v error=%v launches=%d runs=%d, want no second submission", result, err, len(h.launches), len(h.topology.runs))
			}
		})
	}
}

func TestCodexConverterFallbackIdentityRejectsForegroundProcessOfAnotherPID(t *testing.T) {
	h := newConversionHarness(t)
	h.topology.pid = 100
	h.runner.results = []command.Result{managedAgentResult(t, h.source), managedAgentResult(t, h.source), managedAgentResult(t, h.source)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := &codexConvertStampedRunner{Runner: h.runner, cancelAfter: 3, cancel: cancel}
	h.converter.launcher.Host.Runner = runner
	agent, err := h.converter.awaitFallbackIdentity(ctx, managedOrigin(), h.source.CWD, managedSourceID, 99, 10*time.Second)
	if !errors.Is(err, ErrManagedStale) || !errors.Is(err, context.Canceled) || agent.Session != nil || len(runner.times) != 3 {
		t.Fatalf("agent=%+v error=%v polls=%d, want every poll rejected until canceled", agent, err, len(runner.times))
	}
	for poll := 1; poll < len(runner.times); poll++ {
		if gap := runner.times[poll].Sub(runner.times[poll-1]); gap < 25*time.Millisecond {
			t.Fatalf("poll %d began %v after the previous one, want the 25ms poll interval", poll+1, gap)
		}
	}
}

func TestCodexConverterFallbackIdentityRequiresTwoIdenticalSamples(t *testing.T) {
	h := newConversionHarness(t)
	h.topology.pid = 99
	upper := wireFromAgent(h.source)
	upper.Agent = "CODEX"
	h.runner.results = []command.Result{managedAgentResult(t, h.source), managedWireAgentResult(t, upper), managedWireAgentResult(t, upper)}
	agent, err := h.converter.awaitFallbackIdentity(context.Background(), managedOrigin(), h.source.CWD, managedSourceID, 99, 10*time.Second)
	if err != nil || agent.Kind != "CODEX" || len(h.runner.calls) != 3 {
		t.Fatalf("agent=%+v error=%v polls=%d, want the second sample to differ and the third to confirm it", agent, err, len(h.runner.calls))
	}
}

func TestCodexConversionValidationRejectsEachUnsafeDirectoryAndTimeout(t *testing.T) {
	h := newConversionHarness(t)
	for _, test := range []struct {
		name   string
		change func(*CodexConversionRequest)
		valid  bool
	}{
		{"relative directory", func(request *CodexConversionRequest) { request.Source.CWD = "project" }, false},
		{"unclean directory", func(request *CodexConversionRequest) { request.Source.CWD = "/tmp/project/../project" }, false},
		{"directory with NUL", func(request *CodexConversionRequest) { request.Source.CWD = "/tmp/pro\x00ject" }, false},
		{"default timeout", func(request *CodexConversionRequest) { request.ReadyTimeout = 0 }, true},
		{"negative timeout", func(request *CodexConversionRequest) { request.ReadyTimeout = -time.Second }, false},
		{"one second", func(request *CodexConversionRequest) { request.ReadyTimeout = time.Second }, false},
		{"three seconds", func(request *CodexConversionRequest) { request.ReadyTimeout = 3 * time.Second }, false},
		{"just over three seconds", func(request *CodexConversionRequest) { request.ReadyTimeout = 3*time.Second + 1 }, true},
		{"maximum", func(request *CodexConversionRequest) { request.ReadyTimeout = MaxManagedReadyTimeout }, true},
		{"over maximum", func(request *CodexConversionRequest) { request.ReadyTimeout = MaxManagedReadyTimeout + 1 }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := h.request()
			test.change(&request)
			err := h.converter.validateResolvable(request)
			if test.valid && err != nil || !test.valid && !errors.Is(err, ErrManagedUnsupported) {
				t.Fatalf("request=%+v error=%v, want valid=%t", request, err, test.valid)
			}
		})
	}
}

func TestCodexConvertIdentityRequiresEveryStoredField(t *testing.T) {
	h := newConversionHarness(t)
	if err := validateConversionIdentity(h.identity, h.source, h.identity.ID); err != nil {
		t.Fatalf("complete identity rejected: %v", err)
	}
	for name, change := range map[string]func(*codexThreadIdentity){
		"thread":           func(identity *codexThreadIdentity) { identity.ID = "other-thread" },
		"directory":        func(identity *codexThreadIdentity) { identity.CWD = "/tmp/other" },
		"provider":         func(identity *codexThreadIdentity) { identity.ModelProvider = "" },
		"model":            func(identity *codexThreadIdentity) { identity.Model = "" },
		"reasoning effort": func(identity *codexThreadIdentity) { identity.ReasoningEffort = "" },
	} {
		t.Run(name, func(t *testing.T) {
			identity := h.identity
			change(&identity)
			if err := validateConversionIdentity(identity, h.source, h.identity.ID); !errors.Is(err, ErrManagedStale) {
				t.Fatalf("incomplete identity error=%v", err)
			}
		})
	}
}
