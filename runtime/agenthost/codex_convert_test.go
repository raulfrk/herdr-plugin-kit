package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

type conversionVerifier struct {
	constraints    []codexConstraintSnapshot
	constraintErrs []error
	identities     []codexThreadIdentity
	identityErrs   []error
	histories      [][32]byte
	historyErrs    []error
	policies       []EffectiveCodexPolicy
	policyErrs     []error
	closed         int
}

func (f *conversionVerifier) Preflight(context.Context, string, ManagedCodexPolicy) (codexPreflight, error) {
	return codexPreflight{}, errors.New("unexpected preflight")
}
func (f *conversionVerifier) ReadConstraints(context.Context, string) (codexConstraintSnapshot, error) {
	value, err := popConversion(&f.constraints, &f.constraintErrs)
	return value, err
}
func (f *conversionVerifier) Arm() error { return errors.New("unexpected arm") }
func (f *conversionVerifier) ConfirmIdentity(context.Context, ManagedOperation, string, string) (codexThreadIdentity, error) {
	return codexThreadIdentity{}, errors.New("unexpected confirm")
}
func (f *conversionVerifier) ReadThread(context.Context, string) (codexThreadIdentity, error) {
	return popConversion(&f.identities, &f.identityErrs)
}
func (f *conversionVerifier) RecheckIdentity(context.Context, string) (codexThreadIdentity, error) {
	return codexThreadIdentity{}, errors.New("unexpected recheck")
}
func (f *conversionVerifier) ReadEffective(context.Context, string) (EffectiveCodexPolicy, error) {
	return popConversion(&f.policies, &f.policyErrs)
}
func (f *conversionVerifier) ReconcileEffective(_ context.Context, _ string, desired EffectiveCodexPolicy) (EffectiveCodexPolicy, error) {
	return desired, nil
}
func (f *conversionVerifier) ReadStableHistory(context.Context, string) ([32]byte, error) {
	return popConversion(&f.histories, &f.historyErrs)
}
func (f *conversionVerifier) DeleteThread(context.Context, string) error {
	return errors.New("unexpected delete")
}
func (f *conversionVerifier) Close() error { f.closed++; return nil }

func popConversion[T any](values *[]T, errs *[]error) (T, error) {
	var zero T
	if len(*values) == 0 {
		return zero, errors.New("unexpected conversion verifier call")
	}
	value := (*values)[0]
	*values = (*values)[1:]
	var err error
	if len(*errs) != 0 {
		err = (*errs)[0]
		*errs = (*errs)[1:]
	}
	return value, err
}

func TestCodexConverterStopsCapturesAndResumesSameThread(t *testing.T) {
	harness := newConversionHarness(t)
	managed := harness.managedResult()
	harness.converter.launch = func(_ context.Context, request ManagedCodexRequest) (*ManagedCodex, error) {
		harness.launches = append(harness.launches, request)
		return managed, nil
	}
	result, err := harness.converter.Convert(context.Background(), harness.request())
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != CodexConversionManaged || result.FallbackUsed || result.Managed != managed || result.ManagedAttempts != 1 || result.ThreadID != managedSourceID || len(harness.topology.stops) != 1 || harness.topology.stops[0].PID != 77 {
		t.Fatalf("result=%+v stops=%+v", result, harness.topology.stops)
	}
	if len(harness.launches) != 1 {
		t.Fatalf("launches=%+v", harness.launches)
	}
	request := harness.launches[0]
	if request.Operation != ManagedResume || request.Origin != managedOrigin() || request.ThreadID != managedSourceID || request.Directory != harness.source.CWD || request.Focus || request.Placement != "" || request.Policy != managedPolicy() {
		t.Fatalf("managed request=%+v", request)
	}
	if harness.verifier.closed != 1 || len(harness.runner.calls) != 4 || hasPaneInput(harness.runner.calls) {
		t.Fatalf("closed=%d calls=%#v", harness.verifier.closed, harness.runner.calls)
	}
}

func TestCodexConverterRetriesOneEligibleManagedFailureByteIdentically(t *testing.T) {
	harness := newConversionHarness(t)
	managed := harness.managedResult()
	harness.verifier.identities = append(harness.verifier.identities[:2], harness.identity, harness.identity)
	harness.verifier.histories = append(harness.verifier.histories[:2], harness.history, harness.history)
	attempt := 0
	harness.converter.launch = func(_ context.Context, request ManagedCodexRequest) (*ManagedCodex, error) {
		harness.launches = append(harness.launches, request)
		attempt++
		if attempt == 1 {
			return nil, errors.New("temporary launch failure")
		}
		return managed, nil
	}
	result, err := harness.converter.Convert(context.Background(), harness.request())
	if err != nil || result.ManagedAttempts != 2 || len(harness.launches) != 2 || !sameManagedRequest(harness.launches[0], harness.launches[1]) {
		t.Fatalf("result=%+v error=%v launches=%+v", result, err, harness.launches)
	}
}

func TestCodexConverterFallsBackOnceAfterTwoEligibleManagedFailures(t *testing.T) {
	harness := newConversionHarness(t)
	harness.verifier.identities = []codexThreadIdentity{harness.identity, harness.identity, harness.identity, harness.identity, harness.identity}
	harness.verifier.histories = [][32]byte{harness.history, harness.history, harness.history, harness.history, harness.history}
	harness.runner.results = append(harness.runner.results, managedAgentResult(t, harness.source), managedAgentResult(t, harness.source))
	harness.converter.launch = func(_ context.Context, request ManagedCodexRequest) (*ManagedCodex, error) {
		harness.launches = append(harness.launches, request)
		return nil, errors.New("temporary launch failure")
	}
	result, err := harness.converter.Convert(context.Background(), harness.request())
	if !errors.Is(err, ErrCodexConversionFallback) || result.Outcome != CodexConversionFallback || !result.FallbackUsed || result.ManagedAttempts != 2 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if len(harness.launches) != 2 || !sameManagedRequest(harness.launches[0], harness.launches[1]) || len(harness.topology.runs) != 1 {
		t.Fatalf("launches=%+v runs=%+v", harness.launches, harness.topology.runs)
	}
}

func TestCodexConverterCaptureFailureRestoresOrdinarySameThread(t *testing.T) {
	harness := newConversionHarness(t)
	harness.verifier.policies = []EffectiveCodexPolicy{{}}
	harness.verifier.policyErrs = []error{errors.New("capture unavailable")}
	harness.verifier.identities = []codexThreadIdentity{harness.identity, harness.identity, harness.identity, harness.identity}
	harness.verifier.histories = [][32]byte{harness.history, harness.history, harness.history, harness.history}
	fallbackAgent := harness.source
	harness.runner.results = append(harness.runner.results, managedAgentResult(t, fallbackAgent), managedAgentResult(t, fallbackAgent))
	result, err := harness.converter.Convert(context.Background(), harness.request())
	if !errors.Is(err, ErrCodexConversionFallback) || !errors.Is(err, ErrCodexConversionCapture) || result.Outcome != CodexConversionFallback || !result.FallbackUsed || result.ManagedAttempts != 0 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	wantArgv := []string{"codex", "resume", "--cd", harness.source.CWD, managedSourceID}
	if len(harness.topology.runs) != 1 || !equalStrings(harness.topology.runs[0], wantArgv) || !equalStrings(result.Argv, wantArgv) || result.Agent.Session == nil || result.Agent.Session.Value != managedSourceID {
		t.Fatalf("runs=%v result=%+v", harness.topology.runs, result)
	}
}

func TestCodexConverterConstraintDriftAfterStopRestoresOrdinarySameThread(t *testing.T) {
	harness := newConversionHarness(t)
	drifted := conversionConstraints()
	drifted.Config["approvals_reviewer"] = json.RawMessage(`"auto_review"`)
	harness.verifier.constraints = []codexConstraintSnapshot{conversionConstraints(), drifted}
	harness.verifier.identities = []codexThreadIdentity{harness.identity, harness.identity, harness.identity, harness.identity}
	harness.verifier.histories = [][32]byte{harness.history, harness.history, harness.history, harness.history}
	harness.runner.results = append(harness.runner.results, managedAgentResult(t, harness.source), managedAgentResult(t, harness.source))
	result, err := harness.converter.Convert(context.Background(), harness.request())
	if !errors.Is(err, ErrCodexConversionFallback) || !errors.Is(err, ErrManagedPolicyDrift) || !result.FallbackUsed || len(harness.launches) != 0 || len(harness.topology.runs) != 1 {
		t.Fatalf("result=%+v error=%v launches=%d runs=%d", result, err, len(harness.launches), len(harness.topology.runs))
	}
}

func TestCodexConverterPreStopCaptureFailureSendsNoInput(t *testing.T) {
	harness := newConversionHarness(t)
	harness.verifier.constraintErrs = []error{errors.New("config unavailable")}
	result, err := harness.converter.Convert(context.Background(), harness.request())
	if !errors.Is(err, ErrCodexConversionCapture) || !reflect.DeepEqual(result, CodexConversionResult{}) || len(harness.topology.stops) != 0 || len(harness.topology.runs) != 0 || len(harness.runner.calls) != 0 {
		t.Fatalf("result=%+v error=%v stops=%d runs=%d calls=%d", result, err, len(harness.topology.stops), len(harness.topology.runs), len(harness.runner.calls))
	}
}

func TestCodexConverterFallbackFailureIsReportedWithoutAReplacementAttempt(t *testing.T) {
	harness := newConversionHarness(t)
	harness.verifier.policyErrs = []error{errors.New("capture unavailable")}
	harness.verifier.identities = []codexThreadIdentity{harness.identity, harness.identity, harness.identity}
	harness.verifier.histories = [][32]byte{harness.history, harness.history, harness.history}
	harness.topology.runErr = errors.New("ordinary resume rejected")
	result, err := harness.converter.Convert(context.Background(), harness.request())
	if !errors.Is(err, harness.topology.runErr) || result.FallbackUsed || result.Outcome != "" || len(harness.topology.runs) != 1 {
		t.Fatalf("result=%+v error=%v runs=%d", result, err, len(harness.topology.runs))
	}
}

func TestCodexConverterSupportsDefaultHerdrSession(t *testing.T) {
	harness := newConversionHarness(t)
	harness.source.SessionName = ""
	harness.converter.launcher.Host.Session = ""
	managed := harness.managedResult()
	managed.Agent.SessionName = ""
	managed.Target.Origin.SessionName = ""
	harness.converter.launch = func(_ context.Context, request ManagedCodexRequest) (*ManagedCodex, error) {
		harness.launches = append(harness.launches, request)
		return managed, nil
	}
	result, err := harness.converter.Convert(context.Background(), harness.request())
	if err != nil || result.Outcome != CodexConversionManaged || len(harness.launches) != 1 || harness.launches[0].Origin.SessionName != "" {
		t.Fatalf("result=%+v error=%v launches=%+v", result, err, harness.launches)
	}
}

func TestCodexConverterPreparesCompleteCustomDisclosureWithoutPaneInput(t *testing.T) {
	harness := newConversionHarness(t)
	desired := ManagedCodexPolicy{Model: "gpt-5.6-sol", ReasoningEffort: "high", ApprovalPolicy: "on-request", SandboxMode: "read-only"}
	harness.verifier.identities = []codexThreadIdentity{harness.identity}
	harness.verifier.histories = [][32]byte{harness.history}
	harness.verifier.constraints = []codexConstraintSnapshot{conversionConstraints()}
	disclosure, err := harness.converter.PrepareCustomization(context.Background(), harness.source, desired, 0)
	if err != nil {
		t.Fatal(err)
	}
	if disclosure.ThreadID != managedSourceID || disclosure.CWD != harness.source.CWD || disclosure.Desired != desired || !reflect.DeepEqual(disclosure.KnownReplacements, []string{"reasoning_effort"}) || !reflect.DeepEqual(disclosure.UnknownBeforeStop, codexConversionUnknownBeforeStop) || len(disclosure.ConfirmationToken) != 64 {
		t.Fatalf("disclosure=%+v", disclosure)
	}
	if len(harness.runner.calls) != 0 || len(harness.topology.runs) != 0 || len(harness.topology.stops) != 0 {
		t.Fatalf("calls=%v runs=%v stops=%v", harness.runner.calls, harness.topology.runs, harness.topology.stops)
	}
}

func TestCodexConverterCustomConfirmationReconcilesExactTargetPolicy(t *testing.T) {
	harness := newConversionHarness(t)
	harness.policy.SandboxMode = "workspace-write"
	harness.policy.SandboxNetworkAccess = true
	harness.policy.SandboxWritableRoots = []string{"/tmp/write"}
	harness.policy.SandboxExcludeSlashTmp = true
	harness.policy.SandboxExcludeTmpdirEnvVar = true
	desired := ManagedCodexPolicy{Model: "gpt-5.6-sol", ReasoningEffort: "high", ApprovalPolicy: "on-request", SandboxMode: "read-only"}
	target := harness.policy
	target.ReasoningEffort, target.ApprovalPolicy, target.SandboxMode = desired.ReasoningEffort, desired.ApprovalPolicy, desired.SandboxMode
	target.SandboxNetworkAccess = false
	target.SandboxWritableRoots = nil
	target.SandboxExcludeSlashTmp = false
	target.SandboxExcludeTmpdirEnvVar = false
	harness.verifier.identities = []codexThreadIdentity{harness.identity, harness.identity, {ID: harness.identity.ID, CWD: harness.identity.CWD, ModelProvider: harness.identity.ModelProvider, Model: target.Model, ReasoningEffort: target.ReasoningEffort}}
	harness.verifier.histories = [][32]byte{harness.history, harness.history, harness.history}
	harness.verifier.policies = []EffectiveCodexPolicy{harness.policy, target}
	managed := harness.managedResult()
	managed.Policy = target
	harness.converter.launch = func(_ context.Context, request ManagedCodexRequest) (*ManagedCodex, error) {
		harness.launches = append(harness.launches, request)
		return managed, nil
	}
	customization := &CodexConversionCustomization{Desired: desired, ConfirmationToken: conversionConfirmationToken(harness.source, harness.identity, desired)}
	request := harness.request()
	request.Customization = customization
	result, err := harness.converter.Convert(context.Background(), request)
	if err != nil || result.Outcome != CodexConversionManaged || requiredPolicyDrift(result.Policy, target) != "" || len(harness.launches) != 1 {
		t.Fatalf("result=%+v error=%v launches=%+v", result, err, harness.launches)
	}
	launchRequest := harness.launches[0]
	if launchRequest.Policy != desired || launchRequest.resumeSourcePolicy == nil || launchRequest.resumeTargetPolicy == nil || requiredPolicyDrift(*launchRequest.resumeSourcePolicy, harness.policy) != "" || requiredPolicyDrift(*launchRequest.resumeTargetPolicy, target) != "" {
		t.Fatalf("request=%+v", launchRequest)
	}
}

func TestCodexConverterRejectsUnconfirmedCustomizationBeforeStoppingSource(t *testing.T) {
	harness := newConversionHarness(t)
	desired := managedPolicy()
	desired.ReasoningEffort = "high"
	request := harness.request()
	request.Customization = &CodexConversionCustomization{Desired: desired, ConfirmationToken: "wrong"}
	result, err := harness.converter.Convert(context.Background(), request)
	if !errors.Is(err, ErrManagedUnsupported) || !reflect.DeepEqual(result, CodexConversionResult{}) || len(harness.topology.stops) != 0 || len(harness.topology.runs) != 0 || len(harness.runner.calls) != 0 {
		t.Fatalf("result=%+v error=%v stops=%d runs=%d calls=%d", result, err, len(harness.topology.stops), len(harness.topology.runs), len(harness.runner.calls))
	}
}

func TestCodexConverterDoesNotRetryUnsafeManagedFailure(t *testing.T) {
	for _, failure := range []error{context.Canceled, context.DeadlineExceeded, ErrManagedStale, ErrManagedAmbiguous, ErrManagedCleanup, ErrManagedPolicyDrift, ErrManagedSettingsUnknown, ErrManagedPaneBusy, ErrManagedNarrow, ErrCodexServiceDrift, errCodexProtocol} {
		t.Run(failure.Error(), func(t *testing.T) {
			harness := newConversionHarness(t)
			harness.converter.launch = func(_ context.Context, request ManagedCodexRequest) (*ManagedCodex, error) {
				harness.launches = append(harness.launches, request)
				return nil, failure
			}
			result, err := harness.converter.Convert(context.Background(), harness.request())
			if !errors.Is(err, failure) || result.ManagedAttempts != 1 || len(harness.launches) != 1 || len(harness.topology.runs) != 0 {
				t.Fatalf("result=%+v error=%v launches=%d runs=%d", result, err, len(harness.launches), len(harness.topology.runs))
			}
		})
	}
}

func TestCodexConverterRejectsInvalidSourceBeforeServiceOrInput(t *testing.T) {
	harness := newConversionHarness(t)
	invalid := harness.source
	invalid.SessionName = "other"
	request := harness.request()
	request.Source = invalid
	result, err := harness.converter.Convert(context.Background(), request)
	if !errors.Is(err, ErrManagedUnsupported) || !reflect.DeepEqual(result, CodexConversionResult{}) || harness.service.callCount() != 0 || len(harness.runner.calls) != 0 {
		t.Fatalf("result=%+v error=%v service=%d calls=%v", result, err, harness.service.callCount(), harness.runner.calls)
	}
}

func TestCodexConverterShellDriftPreventsNextSubmissionAtEveryRecoveryStage(t *testing.T) {
	draftChanged := fmt.Errorf("pane draft changed: %w", ErrManagedStale)
	for _, drift := range []struct {
		name string
		err  error
	}{
		{name: "focused", err: ErrManagedPaneBusy},
		{name: "draft", err: draftChanged},
	} {
		for _, stage := range []string{"initial", "retry", "fallback"} {
			t.Run(stage+"_"+drift.name, func(t *testing.T) {
				harness := newConversionHarness(t)
				submissions := 0
				switch stage {
				case "initial":
					harness.shellVerifyErrs = []error{drift.err}
				case "retry":
					harness.shellVerifyErrs = []error{nil, drift.err}
					harness.verifier.identities = append(harness.verifier.identities[:2], harness.identity)
					harness.verifier.histories = append(harness.verifier.histories[:2], harness.history)
				case "fallback":
					harness.shellVerifyErrs = []error{nil, nil, drift.err}
					harness.verifier.identities = append(harness.verifier.identities[:2], harness.identity, harness.identity)
					harness.verifier.histories = append(harness.verifier.histories[:2], harness.history, harness.history)
				}
				harness.converter.launch = func(ctx context.Context, request ManagedCodexRequest) (*ManagedCodex, error) {
					harness.launches = append(harness.launches, request)
					if err := request.resumeShellGuard(ctx); err != nil {
						return nil, err
					}
					submissions++
					return nil, errors.New("temporary launch failure")
				}

				result, err := harness.converter.Convert(context.Background(), harness.request())
				if !errors.Is(err, drift.err) || result.FallbackUsed || len(harness.topology.runs) != 0 {
					t.Fatalf("result=%+v error=%v runs=%v", result, err, harness.topology.runs)
				}
				wantSubmissions := map[string]int{"initial": 0, "retry": 1, "fallback": 2}[stage]
				wantCaptures := map[string]int{"initial": 1, "retry": 2, "fallback": 3}[stage]
				if submissions != wantSubmissions || harness.shellCaptures != wantCaptures {
					t.Fatalf("submissions=%d want=%d captures=%d want=%d", submissions, wantSubmissions, harness.shellCaptures, wantCaptures)
				}
			})
		}
	}
}

func TestCodexConversionShellCaptureRejectsFocusedPane(t *testing.T) {
	harness := newConversionHarness(t)
	origin := managedOrigin()
	harness.runner.results = []command.Result{conversionPaneResult(t, origin, harness.source.CWD, true, 17)}
	converter := CodexConverter{launcher: harness.converter.launcher}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := converter.captureConversionShell(ctx, origin, harness.source.CWD, nil); !errors.Is(err, ErrManagedPaneBusy) {
		t.Fatalf("error=%v", err)
	}
	if hasPaneInput(harness.runner.calls) {
		t.Fatalf("calls=%v", harness.runner.calls)
	}
}

func TestCodexConversionShellCaptureRejectsChangedVisibleDraft(t *testing.T) {
	harness := newConversionHarness(t)
	origin := managedOrigin()
	harness.runner.results = []command.Result{
		conversionPaneResult(t, origin, harness.source.CWD, false, 17), {Stdout: []byte("promptDRAFT")}, conversionPaneResult(t, origin, harness.source.CWD, false, 17),
		conversionPaneResult(t, origin, harness.source.CWD, false, 17), {Stdout: []byte("promptDRAFT")}, conversionPaneResult(t, origin, harness.source.CWD, false, 17),
	}
	converter := CodexConverter{launcher: harness.converter.launcher}
	expected := conversionShellLease{Process: harness.topology.inspected, Revision: 17, Visible: "prompt"}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := converter.verifyConversionShell(ctx, origin, harness.source.CWD, expected); !errors.Is(err, ErrManagedPaneBusy) {
		t.Fatalf("error=%v", err)
	}
	if hasPaneInput(harness.runner.calls) {
		t.Fatalf("calls=%v", harness.runner.calls)
	}
}

func TestCodexConversionShellObservationRejectsRevisionDriftWithoutAPIError(t *testing.T) {
	harness := newConversionHarness(t)
	origin := managedOrigin()
	harness.runner.results = []command.Result{
		conversionPaneResult(t, origin, harness.source.CWD, false, 17),
		{Stdout: []byte("prompt")},
		conversionPaneResult(t, origin, harness.source.CWD, false, 18),
	}
	converter := CodexConverter{launcher: harness.converter.launcher}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := converter.observeConversionShell(ctx, origin, harness.source.CWD); !errors.Is(err, ErrManagedStale) {
		t.Fatalf("error=%v", err)
	}
}

func TestCodexConverterFallbackIdentityTimeoutStopsAfterOnePoll(t *testing.T) {
	harness := newConversionHarness(t)
	harness.runner.results = nil
	harness.runner.calls = nil
	_, err := harness.converter.awaitFallbackIdentity(context.Background(), managedOrigin(), harness.source.CWD, managedSourceID, 99, 5*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrManagedStale) {
		t.Fatalf("error=%v", err)
	}
	if len(harness.runner.calls) != 1 {
		t.Fatalf("calls=%v", harness.runner.calls)
	}
}

type conversionHarness struct {
	converter        CodexConverter
	service          *fakeManagedService
	topology         *fakeManagedTopology
	verifier         *conversionVerifier
	runner           *scriptedRunner
	source           Agent
	identity         codexThreadIdentity
	history          [32]byte
	policy           EffectiveCodexPolicy
	launches         []ManagedCodexRequest
	shellLease       conversionShellLease
	shellCaptures    int
	shellVerifies    int
	shellCaptureErrs []error
	shellVerifyErrs  []error
}

func newConversionHarness(t *testing.T) *conversionHarness {
	t.Helper()
	origin := managedOrigin()
	source := managedAgent(origin, "/tmp/project", managedSourceID, false)
	source.Status = "done"
	identity := codexThreadIdentity{ID: managedSourceID, CWD: source.CWD, ModelProvider: "openai", Model: "gpt-5.6-sol", ReasoningEffort: "medium"}
	history := [32]byte{1, 2, 3}
	policy := managedPreflight(source.CWD).EffectiveCodexPolicy
	policy.ModelProvider = "openai"
	policy.MultiAgentMode = "explicitRequestOnly"
	policy.RuntimeWorkspaceRoots = []string{source.CWD}
	policy.InstructionSources = []string{"/tmp/AGENTS.md"}
	constraints := conversionConstraints()
	verifier := &conversionVerifier{
		constraints: []codexConstraintSnapshot{constraints, constraints},
		identities:  []codexThreadIdentity{identity, identity, identity},
		histories:   [][32]byte{history, history, history},
		policies:    []EffectiveCodexPolicy{policy, policy},
	}
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")},
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")},
	}}
	service := &fakeManagedService{snapshot: managedServiceSnapshot()}
	topology := &fakeManagedTopology{inspected: sessionhost.ForegroundProcess{PID: 77, Name: "codex", CWD: source.CWD, Argv: []string{"codex"}}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: origin.SessionName})
	launcher.service, launcher.topology, launcher.verifiers = service, topology, fakeManagedVerifierFactory{verifier: verifier}
	harness := &conversionHarness{
		converter: CodexConverter{launcher: launcher}, service: service, topology: topology,
		verifier: verifier, runner: runner, source: source, identity: identity, history: history, policy: policy,
	}
	harness.shellLease = conversionShellLease{
		Process:  sessionhost.ForegroundProcess{PID: 88, Name: "zsh", CWD: source.CWD, Argv: []string{"zsh"}},
		Revision: 17,
		Visible:  "prompt",
	}
	harness.converter.captureShell = func(_ context.Context, _ sessionhost.Origin, _ string, expected *sessionhost.ForegroundProcess) (conversionShellLease, error) {
		harness.shellCaptures++
		if len(harness.shellCaptureErrs) != 0 {
			err := harness.shellCaptureErrs[0]
			harness.shellCaptureErrs = harness.shellCaptureErrs[1:]
			if err != nil {
				return conversionShellLease{}, err
			}
		}
		if expected != nil && !sameForegroundProcess(*expected, harness.shellLease.Process) {
			return conversionShellLease{}, ErrManagedStale
		}
		return harness.shellLease, nil
	}
	harness.converter.verifyShell = func(_ context.Context, _ sessionhost.Origin, _ string, expected conversionShellLease) error {
		harness.shellVerifies++
		if len(harness.shellVerifyErrs) != 0 {
			err := harness.shellVerifyErrs[0]
			harness.shellVerifyErrs = harness.shellVerifyErrs[1:]
			if err != nil {
				return err
			}
		}
		if !sameConversionShellLease(expected, harness.shellLease) {
			return ErrManagedStale
		}
		return nil
	}
	return harness
}

func (h *conversionHarness) managedResult() *ManagedCodex {
	argv := composeManagedTUIArgv(ManagedCodexRequest{Operation: ManagedResume, Directory: h.source.CWD, ThreadID: managedSourceID}, h.service.snapshot.SocketPath, managedSourceID)
	return &ManagedCodex{
		Operation: ManagedResume, ThreadID: managedSourceID, SourceThreadID: managedSourceID,
		Agent: h.source, Target: sessionhost.CreatedTarget{Origin: managedOrigin()},
		Process: sessionhost.ForegroundProcess{PID: 99, Name: "codex", CWD: h.source.CWD, Argv: argv},
		Policy:  h.policy, Argv: argv, Service: h.service.snapshot,
	}
}

func (h *conversionHarness) request() CodexConversionRequest {
	return CodexConversionRequest{Source: h.source, ReadyTimeout: 4 * time.Second}
}

func conversionConstraints() codexConstraintSnapshot {
	return codexConstraintSnapshot{
		Config: map[string]json.RawMessage{
			"approvals_reviewer": json.RawMessage(`"user"`),
		},
		Requirements: map[string]json.RawMessage{"requirements": json.RawMessage(`null`)},
	}
}

func conversionPaneResult(t *testing.T, origin sessionhost.Origin, cwd string, focused bool, revision uint64) command.Result {
	t.Helper()
	return jsonResult(t, map[string]any{
		"id": "cli:pane:get",
		"result": map[string]any{
			"type": "pane_info",
			"pane": map[string]any{
				"pane_id": origin.PaneID, "workspace_id": origin.WorkspaceID, "tab_id": origin.TabID,
				"cwd": cwd, "focused": focused, "revision": revision,
			},
		},
	})
}

func sameManagedRequest(a, b ManagedCodexRequest) bool {
	return a.Operation == b.Operation && a.Origin == b.Origin && a.Directory == b.Directory && a.ThreadID == b.ThreadID && a.Policy == b.Policy && a.ReadyTimeout == b.ReadyTimeout && equalStrings(a.SourceArgv, b.SourceArgv) && equalPolicyPointer(a.resumeSourcePolicy, b.resumeSourcePolicy) && equalPolicyPointer(a.resumeTargetPolicy, b.resumeTargetPolicy)
}

func equalPolicyPointer(a, b *EffectiveCodexPolicy) bool {
	return a == nil && b == nil || a != nil && b != nil && requiredPolicyDrift(*a, *b) == ""
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
