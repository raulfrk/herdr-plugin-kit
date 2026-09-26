package agenthost

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

type conversionVerifier struct {
	loadedErr       error
	loadedReads     []string
	constraints     []codexConstraintSnapshot
	constraintErrs  []error
	identities      []codexThreadIdentity
	identityErrs    []error
	histories       [][32]byte
	historyErrs     []error
	policies        []EffectiveCodexPolicy
	policyErrs      []error
	applyErr        error
	applyNoDispatch bool
	applied         []ManagedCodexPolicy
	closed          int
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

func (f *conversionVerifier) ReadLoadedThread(ctx context.Context, id string) (codexThreadIdentity, error) {
	f.loadedReads = append(f.loadedReads, id)
	if f.loadedErr != nil {
		return codexThreadIdentity{}, f.loadedErr
	}
	return f.ReadThread(ctx, id)
}
func (f *conversionVerifier) ListCLIThreads(context.Context, string) ([]codexThreadIdentity, error) {
	return nil, errors.New("unexpected list")
}
func (f *conversionVerifier) ListThreads(context.Context, string) ([]codexThreadIdentity, error) {
	return nil, errors.New("unexpected unfiltered list")
}
func (f *conversionVerifier) RecheckIdentity(context.Context, string) (codexThreadIdentity, error) {
	return codexThreadIdentity{}, errors.New("unexpected recheck")
}
func (f *conversionVerifier) ReadEffective(context.Context, string) (EffectiveCodexPolicy, error) {
	return popConversion(&f.policies, &f.policyErrs)
}
func (f *conversionVerifier) ApplyObservedReasoning(_ context.Context, _ codexThreadIdentity, observed ManagedCodexPolicy) (bool, error) {
	f.applied = append(f.applied, observed)
	return f.applyNoDispatch, f.applyErr
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
	if result.Outcome != CodexConversionManaged || result.FallbackUsed || result.Managed != managed || result.ManagedAttempts != 1 || result.ThreadID != managedSourceID || conversionExitCount(harness.runner.calls) != 1 || len(harness.topology.stops) != 0 {
		t.Fatalf("result=%+v stops=%+v", result, harness.topology.stops)
	}
	if len(harness.launches) != 1 {
		t.Fatalf("launches=%+v", harness.launches)
	}
	request := harness.launches[0]
	if request.Operation != ManagedResume || request.Origin != managedOrigin() || request.ThreadID != managedSourceID || request.Directory != harness.source.CWD || request.Focus || request.Placement != "" || request.Policy != managedPolicy() {
		t.Fatalf("managed request=%+v", request)
	}
	if harness.verifier.closed != 1 || len(harness.runner.calls) != 5 || conversionExitCount(harness.runner.calls) != 1 {
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

func TestCodexConverterUnloadedSourceRefusesBeforeExitOrPolicyResume(t *testing.T) {
	harness := newConversionHarness(t)
	harness.verifier.loadedErr = ErrManagedSettingsUnknown
	policies := len(harness.verifier.policies)
	result, err := harness.converter.Convert(context.Background(), harness.request())
	if !errors.Is(err, ErrCodexConversionCapture) || !errors.Is(err, ErrManagedSettingsUnknown) || !reflect.DeepEqual(result, CodexConversionResult{}) {
		t.Fatalf("result=%+v error=%v, want pre-exit settings refusal", result, err)
	}
	if !reflect.DeepEqual(harness.verifier.loadedReads, []string{harness.identity.ID}) || len(harness.verifier.policies) != policies || len(harness.verifier.applied) != 0 || len(harness.launches) != 0 || len(harness.topology.stops) != 0 || len(harness.topology.runs) != 0 || len(harness.runner.calls) != 0 {
		t.Fatalf("unloaded source performed an exit, policy read/update, or launch: %+v", harness)
	}
}

func TestCodexConverterRefusesOrdinaryVisibleDraftBeforeIdentityProbeOrStop(t *testing.T) {
	harness := newConversionHarness(t)
	harness.source.Session = nil
	harness.source.Revision, harness.source.StateChangeSeq = 1, 1
	harness.runner.results = []command.Result{
		managedAgentResult(t, harness.source),
		{Stdout: []byte("answer\n› unsent draft\n status")},
	}

	result, err := harness.converter.Convert(context.Background(), harness.request())
	if !errors.Is(err, ErrCodexConversionComposer) || !errors.Is(err, ErrManagedPaneBusy) || !reflect.DeepEqual(result, CodexConversionResult{}) {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if len(harness.topology.stops) != 0 || len(harness.topology.runs) != 0 || hasPaneInput(harness.runner.calls) {
		t.Fatalf("stops=%+v runs=%+v calls=%+v", harness.topology.stops, harness.topology.runs, harness.runner.calls)
	}
	if len(harness.runner.results) != 0 {
		t.Fatalf("unused probe results=%+v", harness.runner.results)
	}
}

func TestCodexConverterRefusesDraftAppearingBeforeIdentityInput(t *testing.T) {
	harness := newConversionHarness(t)
	harness.source.Session = nil
	harness.source.Revision, harness.source.StateChangeSeq = 1, 1
	bound := harness.source
	bound.Session = &SessionRef{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: managedSourceID}
	results := conversionDiscoveryResults(t, harness.source, bound)
	results[5] = command.Result{Stdout: []byte("answer\n› late unsent draft\n status")}
	harness.runner.results = results[:6]

	result, err := harness.converter.Convert(context.Background(), harness.request())
	if !errors.Is(err, ErrCodexConversionComposer) || !errors.Is(err, ErrManagedPaneBusy) || !reflect.DeepEqual(result, CodexConversionResult{}) {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if len(harness.topology.stops) != 0 || len(harness.topology.runs) != 0 || hasPaneInput(harness.runner.calls) || len(harness.runner.results) != 0 {
		t.Fatalf("stops=%+v runs=%+v calls=%+v unused=%+v", harness.topology.stops, harness.topology.runs, harness.runner.calls, harness.runner.results)
	}
}

func TestCodexConverterRejectsMismatchedThreadBeforeStoppingSource(t *testing.T) {
	harness := newConversionHarness(t)
	harness.verifier.identities[0].ID = "different-thread"
	result, err := harness.converter.Convert(context.Background(), harness.request())
	if !errors.Is(err, ErrCodexConversionCapture) || !errors.Is(err, ErrManagedStale) || !reflect.DeepEqual(result, CodexConversionResult{}) || len(harness.topology.stops) != 0 || len(harness.topology.runs) != 0 || len(harness.runner.calls) != 0 {
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

func TestCodexConverterDiscoversAndBindsMissingOrdinarySessionBeforeStopping(t *testing.T) {
	harness := newConversionHarness(t)
	harness.source.Revision, harness.source.StateChangeSeq = 1, 1
	managed := harness.managedResult()
	bound := harness.source
	harness.source.Session = nil
	harness.verifier.identities = append([]codexThreadIdentity{harness.identity}, harness.verifier.identities...)
	harness.runner.results = append(conversionDiscoveryResults(t, harness.source, bound), harness.runner.results...)
	harness.converter.launch = func(_ context.Context, request ManagedCodexRequest) (*ManagedCodex, error) {
		harness.launches = append(harness.launches, request)
		return managed, nil
	}

	result, err := harness.converter.Convert(context.Background(), harness.request())
	if err != nil || result.Outcome != CodexConversionManaged || result.ThreadID != managedSourceID {
		t.Fatalf("result=%+v error=%v calls=%q", result, err, harness.runner.calls)
	}
	if conversionExitCount(harness.runner.calls) != 1 || len(harness.topology.stops) != 0 || len(harness.launches) != 1 || harness.launches[0].ThreadID != managedSourceID {
		t.Fatalf("stops=%+v launches=%+v", harness.topology.stops, harness.launches)
	}
	assertConversionDiscoveryCalls(t, harness.runner.calls, harness.source.SessionName)
}

func stockDefaultConversionHarness(t *testing.T) (*conversionHarness, Agent) {
	t.Helper()
	h := newConversionHarness(t)
	h.source.Revision, h.source.StateChangeSeq = 1, 1
	bound := h.source
	h.source.Session = nil
	raw := h.identity
	raw.ReasoningEffort = ""
	h.verifier.identities = []codexThreadIdentity{raw, raw, raw, h.identity, h.identity}
	h.verifier.histories = [][32]byte{h.history, h.history, h.history, h.history}
	stock := conversionConstraints()
	delete(stock.Config, "approvals_reviewer")
	h.verifier.constraints = []codexConstraintSnapshot{stock, stock}
	h.runner.results = append(conversionDiscoveryResults(t, h.source, bound), h.runner.results...)
	return h, bound
}

func TestCodexConverterNormalizesObservedStockEffortAfterStop(t *testing.T) {
	h, bound := stockDefaultConversionHarness(t)
	managed := h.managedResult()
	managed.Agent = bound
	h.converter.launch = func(_ context.Context, request ManagedCodexRequest) (*ManagedCodex, error) {
		h.launches = append(h.launches, request)
		return managed, nil
	}
	result, err := h.converter.Convert(context.Background(), h.request())
	if err != nil || result.Outcome != CodexConversionManaged || len(h.verifier.applied) != 1 || h.verifier.applied[0].Model != h.identity.Model || h.verifier.applied[0].ReasoningEffort != "medium" || len(h.launches) != 1 || h.launches[0].resumeSourcePolicy == nil || h.launches[0].resumeSourcePolicy.ReasoningEffort != "medium" {
		t.Fatalf("result=%+v error=%v applied=%+v launches=%+v", result, err, h.verifier.applied, h.launches)
	}
	if conversionExitCount(h.runner.calls) != 1 || len(h.topology.stops) != 0 || len(h.topology.runs) != 0 {
		t.Fatalf("stops=%d ordinary resumes=%d", len(h.topology.stops), len(h.topology.runs))
	}
}

func TestCodexConverterStockUpdateFailureUsesReadbackBeforeFallback(t *testing.T) {
	for _, normalized := range []bool{false, true} {
		t.Run(fmt.Sprintf("normalized=%t", normalized), func(t *testing.T) {
			h, bound := stockDefaultConversionHarness(t)
			h.verifier.applyErr = errors.New("update response lost")
			if !normalized {
				h.verifier.identities[3] = h.verifier.identities[2]
			}
			fallbackIdentity := h.verifier.identities[3]
			h.verifier.identities = append(h.verifier.identities[:4], fallbackIdentity, fallbackIdentity)
			h.verifier.histories = append(h.verifier.histories[:3], h.history, h.history)
			h.runner.results = append(h.runner.results, managedAgentResult(t, bound), managedAgentResult(t, bound))
			result, err := h.converter.Convert(context.Background(), h.request())
			if !errors.Is(err, ErrCodexConversionFallback) || !errors.Is(err, ErrManagedSettingsUnknown) || result.Outcome != CodexConversionFallback || len(h.topology.runs) != 1 || len(h.launches) != 0 {
				t.Fatalf("result=%+v error=%v ordinary resumes=%d launches=%d", result, err, len(h.topology.runs), len(h.launches))
			}
		})
	}
}

func TestCodexConverterStockUpdateAmbiguousReadbackLeavesSourceStopped(t *testing.T) {
	h, _ := stockDefaultConversionHarness(t)
	h.verifier.applyErr = errors.New("update response lost")
	h.verifier.identities[3].Model = "different"
	result, err := h.converter.Convert(context.Background(), h.request())
	if !errors.Is(err, ErrManagedSettingsUnknown) || result.Outcome != "" || conversionExitCount(h.runner.calls) != 1 || len(h.topology.stops) != 0 || len(h.topology.runs) != 0 || len(h.launches) != 0 {
		t.Fatalf("result=%+v error=%v stops=%d resumes=%d launches=%d", result, err, len(h.topology.stops), len(h.topology.runs), len(h.launches))
	}
}

func TestCodexConverterStockUpdateNotDispatchedRestoresExactOrdinaryThreadWithoutAPI(t *testing.T) {
	h, bound := stockDefaultConversionHarness(t)
	h.verifier.applyErr = errors.New("app server unavailable before update")
	h.verifier.applyNoDispatch = true
	h.verifier.identityErrs = []error{nil, nil, nil, errors.New("app server unavailable")}
	h.verifier.historyErrs = []error{nil, nil, errors.New("app server unavailable")}
	h.runner.results = append(h.runner.results, managedAgentResult(t, bound), managedAgentResult(t, bound))
	result, err := h.converter.Convert(context.Background(), h.request())
	if !errors.Is(err, ErrCodexConversionFallback) || !errors.Is(err, ErrManagedSettingsUnknown) || result.Outcome != CodexConversionFallback || len(h.topology.runs) != 1 || len(h.launches) != 0 || h.shellVerifies == 0 {
		t.Fatalf("result=%+v error=%v ordinary resumes=%d launches=%d shell verifies=%d", result, err, len(h.topology.runs), len(h.launches), h.shellVerifies)
	}
	if want := []string{"codex", "resume", "--cd", bound.CWD, managedSourceID}; !reflect.DeepEqual(h.topology.runs[0], want) {
		t.Fatalf("ordinary fallback argv=%q want=%q", h.topology.runs[0], want)
	}
}

func TestCodexConverterNoDispatchDoesNotOverrideKnownReadbackDrift(t *testing.T) {
	h, _ := stockDefaultConversionHarness(t)
	h.verifier.applyErr = errors.New("update was not dispatched")
	h.verifier.applyNoDispatch = true
	h.verifier.identities[3].Model = "different"
	h.verifier.historyErrs = []error{nil, nil, errors.New("history unavailable")}
	result, err := h.converter.Convert(context.Background(), h.request())
	if !errors.Is(err, ErrManagedSettingsUnknown) || result.Outcome != "" || conversionExitCount(h.runner.calls) != 1 || len(h.topology.stops) != 0 || len(h.topology.runs) != 0 {
		t.Fatalf("result=%+v error=%v stops=%d resumes=%d", result, err, len(h.topology.stops), len(h.topology.runs))
	}
}

func TestCodexConverterBoundStockThreadWithoutFreshStatusRefusesBeforeStop(t *testing.T) {
	h := newConversionHarness(t)
	h.source.Revision, h.source.StateChangeSeq = 1, 1
	h.runner.results[0] = managedAgentResult(t, h.source)
	h.verifier.identities[0].ReasoningEffort = ""
	h.runner.results[1].Stdout = []byte("› UNSENT DRAFT")
	result, err := h.converter.Convert(context.Background(), h.request())
	if !errors.Is(err, ErrCodexConversionComposer) || !reflect.DeepEqual(result, CodexConversionResult{}) || len(h.topology.stops) != 0 || len(h.topology.runs) != 0 || hasPaneInput(h.runner.calls) || len(h.verifier.applied) != 0 {
		t.Fatalf("result=%+v error=%v stops=%d resumes=%d", result, err, len(h.topology.stops), len(h.topology.runs))
	}
}

func TestValidateObservedConversionIdentityRejectsMissingOrConflictingStatus(t *testing.T) {
	h := newConversionHarness(t)
	raw := h.identity
	raw.ReasoningEffort = ""
	observed := managedStatusObservation{identity: raw.ID, policy: ManagedCodexPolicy{Model: raw.Model, ReasoningEffort: "medium"}, fromStatus: true}
	if err := validateObservedConversionIdentity(raw, h.source, raw.ID, observed); err != nil {
		t.Fatalf("exact observed default rejected: %v", err)
	}
	for name, change := range map[string]func(*managedStatusObservation){
		"missing status":      func(value *managedStatusObservation) { value.fromStatus = false },
		"prefix effort":       func(value *managedStatusObservation) { value.policyIsPrefix = true },
		"unsupported effort":  func(value *managedStatusObservation) { value.policy.ReasoningEffort = "invalid" },
		"different model":     func(value *managedStatusObservation) { value.policy.Model = "other" },
		"different thread id": func(value *managedStatusObservation) { value.identity = "different" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := observed
			change(&candidate)
			if err := validateObservedConversionIdentity(raw, h.source, raw.ID, candidate); err == nil {
				t.Fatal("unproven effort accepted")
			}
		})
	}
}

func TestCodexConverterPreparesCustomizationAfterBindingMissingOrdinarySession(t *testing.T) {
	harness := newConversionHarness(t)
	harness.source.Revision, harness.source.StateChangeSeq = 1, 1
	bound := harness.source
	harness.source.Session = nil
	harness.runner.results = conversionDiscoveryResults(t, harness.source, bound)
	harness.verifier.identities = []codexThreadIdentity{harness.identity, harness.identity}
	harness.verifier.histories = [][32]byte{harness.history}
	harness.verifier.constraints = []codexConstraintSnapshot{conversionConstraints()}
	desired := ManagedCodexPolicy{Model: "gpt-5.6-sol", ReasoningEffort: "high", ApprovalPolicy: "on-request", SandboxMode: "read-only"}

	disclosure, err := harness.converter.PrepareCustomization(context.Background(), harness.source, desired, 0)
	if err != nil || disclosure.ThreadID != managedSourceID || len(disclosure.ConfirmationToken) != 64 {
		t.Fatalf("disclosure=%+v error=%v calls=%q", disclosure, err, harness.runner.calls)
	}
	if len(harness.topology.stops) != 0 || len(harness.topology.runs) != 0 {
		t.Fatalf("stops=%+v runs=%+v", harness.topology.stops, harness.topology.runs)
	}
	assertConversionDiscoveryCalls(t, harness.runner.calls, harness.source.SessionName)
}

func TestCodexConverterMissingSessionRejectsAppServerDisagreementBeforeBinding(t *testing.T) {
	harness := newConversionHarness(t)
	harness.source.Revision, harness.source.StateChangeSeq = 1, 1
	bound := harness.source
	harness.source.Session = nil
	harness.runner.results = conversionDiscoveryResults(t, harness.source, bound)
	harness.verifier.identities = []codexThreadIdentity{{
		ID: managedSourceID, CWD: "/tmp/other", ModelProvider: "openai",
		Model: "gpt-5.6-sol", ReasoningEffort: "medium",
	}}

	result, err := harness.converter.Convert(context.Background(), harness.request())
	if !errors.Is(err, ErrManagedStale) || !reflect.DeepEqual(result, CodexConversionResult{}) || len(harness.topology.stops) != 0 {
		t.Fatalf("result=%+v error=%v stops=%+v calls=%+v", result, err, harness.topology.stops, harness.runner.calls)
	}
	for _, call := range harness.runner.calls {
		if strings.Contains(strings.Join(call, " "), "report-agent-session") {
			t.Fatalf("authority failure reported a Herdr session: %v", harness.runner.calls)
		}
	}
}

func TestCodexConverterMissingSessionRejectsStaleSourceBeforePaneInput(t *testing.T) {
	harness := newConversionHarness(t)
	harness.source.Revision, harness.source.StateChangeSeq = 1, 1
	harness.source.Session = nil
	changed := harness.source
	changed.Status = "working"
	harness.runner.results = []command.Result{managedAgentResult(t, changed)}

	result, err := harness.converter.Convert(context.Background(), harness.request())
	if !errors.Is(err, ErrManagedStale) || !reflect.DeepEqual(result, CodexConversionResult{}) || len(harness.topology.stops) != 0 || hasPaneInput(harness.runner.calls) {
		t.Fatalf("result=%+v error=%v stops=%+v calls=%+v", result, err, harness.topology.stops, harness.runner.calls)
	}
}

func TestCodexConverterMissingSessionRejectsRemoteProcessBeforePaneInput(t *testing.T) {
	harness := newConversionHarness(t)
	harness.source.Session = nil
	harness.topology.inspected.Argv = []string{"codex", "--remote", "unix:///tmp/shared.sock"}

	result, err := harness.converter.Convert(context.Background(), harness.request())
	if !errors.Is(err, ErrManagedUnsupported) || !reflect.DeepEqual(result, CodexConversionResult{}) || len(harness.runner.calls) != 0 || len(harness.topology.stops) != 0 {
		t.Fatalf("result=%+v error=%v calls=%+v stops=%+v", result, err, harness.runner.calls, harness.topology.stops)
	}
}

func TestCodexConverterMissingSessionRejectsMalformedCustomizationBeforeDiscovery(t *testing.T) {
	harness := newConversionHarness(t)
	harness.source.Session = nil
	request := harness.request()
	request.Customization = &CodexConversionCustomization{Desired: managedPolicy(), ConfirmationToken: "wrong"}

	result, err := harness.converter.Convert(context.Background(), request)
	if !errors.Is(err, ErrManagedUnsupported) || !reflect.DeepEqual(result, CodexConversionResult{}) || harness.service.callCount() != 0 || len(harness.runner.calls) != 0 || len(harness.topology.stops) != 0 {
		t.Fatalf("result=%+v error=%v service=%d calls=%+v stops=%+v", result, err, harness.service.callCount(), harness.runner.calls, harness.topology.stops)
	}
}

func TestCodexConverterRejectsServiceErrorBeforeIdentityWork(t *testing.T) {
	harness := newConversionHarness(t)
	serviceErr := errors.New("service unavailable")
	harness.service.err = serviceErr

	result, err := harness.converter.Convert(context.Background(), harness.request())
	if !errors.Is(err, serviceErr) || !errors.Is(err, ErrManagedStale) || !reflect.DeepEqual(result, CodexConversionResult{}) {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if len(harness.runner.calls) != 0 || len(harness.topology.stops) != 0 {
		t.Fatalf("calls=%+v stops=%+v", harness.runner.calls, harness.topology.stops)
	}
}

func TestCodexConverterPrepareCustomizationRejectsInvalidServiceBeforeIdentityWork(t *testing.T) {
	for _, test := range []struct {
		name       string
		invalidate func(*conversionHarness) error
	}{
		{name: "service error with valid snapshot", invalidate: func(harness *conversionHarness) error {
			serviceErr := errors.New("service unavailable")
			harness.service.err = serviceErr
			return serviceErr
		}},
		{name: "stale snapshot without service error", invalidate: func(harness *conversionHarness) error {
			harness.service.snapshot.SocketPath = ""
			return nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newConversionHarness(t)
			serviceErr := test.invalidate(harness)
			disclosure, err := harness.converter.PrepareCustomization(context.Background(), harness.source, managedPolicy(), 0)
			if !errors.Is(err, ErrManagedStale) || (serviceErr != nil && !errors.Is(err, serviceErr)) || !reflect.DeepEqual(disclosure, CodexConversionDisclosure{}) {
				t.Fatalf("disclosure=%+v error=%v", disclosure, err)
			}
			if harness.service.callCount() != 1 || harness.verifier.closed != 0 || len(harness.runner.calls) != 0 || len(harness.topology.runs) != 0 || len(harness.topology.stops) != 0 {
				t.Fatalf("service calls=%d verifier closes=%d commands=%+v runs=%+v stops=%+v", harness.service.callCount(), harness.verifier.closed, harness.runner.calls, harness.topology.runs, harness.topology.stops)
			}
		})
	}
}

func TestCodexConverterPrepareCustomizationRejectsEachCaptureFailure(t *testing.T) {
	readErr := errors.New("constraints unavailable")
	threadErr := errors.New("thread unavailable")
	historyErr := errors.New("history unavailable")
	for _, test := range []struct {
		name    string
		mutate  func(*conversionHarness)
		wantErr error
	}{
		{name: "constraint read", mutate: func(harness *conversionHarness) {
			harness.verifier.constraintErrs = []error{readErr}
		}, wantErr: readErr},
		{name: "constraint validation", mutate: func(harness *conversionHarness) {
			harness.verifier.constraints[0].Config["approvals_reviewer"] = json.RawMessage("{")
		}},
		{name: "thread read", mutate: func(harness *conversionHarness) {
			harness.verifier.identityErrs = []error{threadErr}
		}, wantErr: threadErr},
		{name: "thread identity", mutate: func(harness *conversionHarness) {
			harness.verifier.identities[0].ID = "wrong-thread"
		}, wantErr: ErrManagedStale},
		{name: "stable history", mutate: func(harness *conversionHarness) {
			harness.verifier.historyErrs = []error{historyErr}
		}, wantErr: historyErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newConversionHarness(t)
			test.mutate(harness)
			disclosure, err := harness.converter.PrepareCustomization(context.Background(), harness.source, managedPolicy(), 0)
			if !errors.Is(err, ErrCodexConversionCapture) || (test.wantErr != nil && !errors.Is(err, test.wantErr)) || !reflect.DeepEqual(disclosure, CodexConversionDisclosure{}) {
				t.Fatalf("disclosure=%+v error=%v, want capture failure with %v", disclosure, err, test.wantErr)
			}
			if harness.verifier.closed != 1 || len(harness.runner.calls) != 0 || len(harness.topology.runs) != 0 || len(harness.topology.stops) != 0 {
				t.Fatalf("verifier closes=%d commands=%+v runs=%+v stops=%+v", harness.verifier.closed, harness.runner.calls, harness.topology.runs, harness.topology.stops)
			}
		})
	}
}

func TestCodexConversionValidationRequiresResolvableAndBoundSourceIndependently(t *testing.T) {
	harness := newConversionHarness(t)
	valid := harness.request()
	for name, request := range map[string]CodexConversionRequest{
		"unresolvable": func() CodexConversionRequest {
			value := valid
			value.Source.SessionName = "other"
			return value
		}(),
		"unbound": func() CodexConversionRequest {
			value := valid
			value.Source.Session = nil
			return value
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if !errors.Is(harness.converter.validate(request), ErrManagedUnsupported) {
				t.Fatalf("accepted request=%+v", request)
			}
		})
	}
}

func TestCodexConverterMissingSessionRejectsEveryForegroundProcessMismatchBeforePaneInput(t *testing.T) {
	inspectErr := errors.New("inspect failed")
	for name, configure := range map[string]func(*fakeManagedTopology){
		"inspect error": func(topology *fakeManagedTopology) { topology.inspectErr = inspectErr },
		"process name":  func(topology *fakeManagedTopology) { topology.inspected.Name = "other" },
		"empty argv":    func(topology *fakeManagedTopology) { topology.inspected.Argv = nil },
		"executable":    func(topology *fakeManagedTopology) { topology.inspected.Argv = []string{"other"} },
		"cwd":           func(topology *fakeManagedTopology) { topology.inspected.CWD = "/tmp/other" },
	} {
		t.Run(name, func(t *testing.T) {
			harness := newConversionHarness(t)
			harness.source.Session = nil
			configure(harness.topology)

			result, err := harness.converter.Convert(context.Background(), harness.request())
			if err == nil || !reflect.DeepEqual(result, CodexConversionResult{}) {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if name == "inspect error" && !errors.Is(err, inspectErr) {
				t.Fatalf("error=%v", err)
			}
			if name != "inspect error" && !errors.Is(err, ErrManagedStale) {
				t.Fatalf("error=%v", err)
			}
			if len(harness.runner.calls) != 0 || len(harness.topology.stops) != 0 {
				t.Fatalf("calls=%+v stops=%+v", harness.runner.calls, harness.topology.stops)
			}
		})
	}
}

func TestRecheckSourceBeforeBindingRejectsEveryGuard(t *testing.T) {
	inspectErr := errors.New("inspect failed")
	for name, arrange := range map[string]func(*conversionHarness, Agent) []command.Result{
		"already bound": func(_ *conversionHarness, expected Agent) []command.Result {
			bound := expected
			bound.Session = &SessionRef{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: managedSourceID}
			return []command.Result{managedAgentResult(t, bound)}
		},
		"immutable": func(_ *conversionHarness, expected Agent) []command.Result {
			wire := wireFromAgent(expected)
			wire.Agent = "CODEX"
			return []command.Result{managedWireAgentResult(t, wire)}
		},
		"focused": func(_ *conversionHarness, expected Agent) []command.Result {
			expected.Focused = true
			return []command.Result{managedAgentResult(t, expected)}
		},
		"launch pending": func(_ *conversionHarness, expected Agent) []command.Result {
			wire := wireFromAgent(expected)
			wire.LaunchPending = true
			return []command.Result{managedWireAgentResult(t, wire)}
		},
		"working": func(_ *conversionHarness, expected Agent) []command.Result {
			expected.Status = "working"
			return []command.Result{managedAgentResult(t, expected)}
		},
		"inspect error": func(harness *conversionHarness, expected Agent) []command.Result {
			harness.topology.inspectErr = inspectErr
			return []command.Result{managedAgentResult(t, expected)}
		},
		"process drift": func(harness *conversionHarness, expected Agent) []command.Result {
			harness.topology.inspected.PID++
			return []command.Result{managedAgentResult(t, expected)}
		},
		"draft": func(_ *conversionHarness, expected Agent) []command.Result {
			return []command.Result{managedAgentResult(t, expected), {Stdout: []byte("answer\n› draft\n status")}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			harness := newConversionHarness(t)
			expected := harness.source
			expected.Session = nil
			expected.Revision, expected.StateChangeSeq = 1, 1
			harness.runner.results = arrange(harness, expected)
			process := sessionhost.ForegroundProcess{PID: 77, Name: "codex", CWD: expected.CWD, Argv: []string{"codex"}}

			_, err := harness.converter.recheckSourceBeforeBinding(context.Background(), managedOrigin(), expected, process)
			if err == nil {
				t.Fatal("unsafe source accepted")
			}
			if name == "inspect error" && !errors.Is(err, inspectErr) {
				t.Fatalf("error=%v", err)
			}
			if name == "draft" && !errors.Is(err, ErrCodexConversionComposer) {
				t.Fatalf("draft error=%v", err)
			}
			if hasPaneInput(harness.runner.calls) {
				t.Fatalf("calls=%+v", harness.runner.calls)
			}
		})
	}
}

func TestConversionConfirmationTokenRejectsUppercaseHex(t *testing.T) {
	if validConversionConfirmationToken(strings.Repeat("A", sha256.Size*2)) {
		t.Fatal("uppercase confirmation token accepted")
	}
}

func TestSameFinalConversionIdentityRejectsEachMismatch(t *testing.T) {
	before := codexThreadIdentity{ID: managedSourceID, CWD: "/tmp/project", ForkedFromID: "parent", ModelProvider: "openai", Model: "gpt-5.6-sol", ReasoningEffort: "medium"}
	target := managedPreflight(before.CWD).EffectiveCodexPolicy
	if !sameFinalConversionIdentity(before, before, target) {
		t.Fatal("matching final identity rejected")
	}
	for _, test := range []struct {
		name   string
		change func(*codexThreadIdentity)
	}{
		{"thread", func(value *codexThreadIdentity) { value.ID = "different-thread" }},
		{"directory", func(value *codexThreadIdentity) { value.CWD = "/tmp/other" }},
		{"fork", func(value *codexThreadIdentity) { value.ForkedFromID = "other-parent" }},
		{"provider", func(value *codexThreadIdentity) { value.ModelProvider = "other" }},
		{"model", func(value *codexThreadIdentity) { value.Model = "other" }},
		{"reasoning", func(value *codexThreadIdentity) { value.ReasoningEffort = "high" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual := before
			test.change(&actual)
			if sameFinalConversionIdentity(actual, before, target) {
				t.Fatal("mismatched final identity accepted")
			}
		})
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

func boundDefaultCustomizationHarness(t *testing.T) *conversionHarness {
	t.Helper()
	h := newConversionHarness(t)
	h.source.Revision, h.source.StateChangeSeq = 1, 1
	raw := h.identity
	raw.ReasoningEffort = ""
	h.verifier.identities = []codexThreadIdentity{raw, raw}
	h.verifier.histories = [][32]byte{h.history}
	results := conversionDiscoveryResults(t, h.source, h.source)
	// Already-bound sources need the guarded status and post-input checks,
	// but must not send another session-binding report.
	h.runner.results = results[:len(results)-3]
	return h
}

func TestCodexConverterPreparesStockCustomizationWithoutChangingSettings(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(fmt.Sprintf("bound=%t", bound), func(t *testing.T) {
			h := boundDefaultCustomizationHarness(t)
			if !bound {
				original := h.source
				h.source.Session = nil
				h.runner.results = conversionDiscoveryResults(t, h.source, original)
			}
			desired := managedPolicy()
			disclosure, err := h.converter.PrepareCustomization(context.Background(), h.source, desired, 4*time.Second)
			if err != nil || disclosure.ThreadID != h.identity.ID || disclosure.Desired != desired || len(disclosure.KnownReplacements) != 0 {
				t.Fatalf("disclosure=%+v error=%v", disclosure, err)
			}
			raw := h.identity
			raw.ReasoningEffort = ""
			if disclosure.ConfirmationToken != conversionConfirmationToken(h.source, raw, desired) || disclosure.ConfirmationToken == conversionConfirmationToken(h.source, h.identity, desired) {
				t.Fatal("confirmation was not bound to the raw unset identity")
			}
			if len(h.verifier.applied) != 0 || conversionExitCount(h.runner.calls) != 0 || len(h.topology.stops) != 0 || len(h.topology.runs) != 0 {
				t.Fatal("preparation changed settings or stopped/resumed the source")
			}
			if bound {
				for _, call := range h.runner.calls {
					if strings.Contains(strings.Join(call, " "), "report-agent-session") {
						t.Fatal("bound source was rebound")
					}
				}
			}
		})
	}
}

func TestCodexConverterConfirmsBoundStockCustomizationWithFreshStatus(t *testing.T) {
	h := boundDefaultCustomizationHarness(t)
	raw := h.identity
	raw.ReasoningEffort = ""
	desired := managedPolicy()
	desired.ReasoningEffort, desired.ApprovalPolicy, desired.SandboxMode = "high", "on-request", "read-only"
	target := h.policy
	target.ReasoningEffort, target.ApprovalPolicy, target.SandboxMode = desired.ReasoningEffort, desired.ApprovalPolicy, desired.SandboxMode
	final := h.identity
	final.ReasoningEffort = desired.ReasoningEffort
	h.verifier.identities = []codexThreadIdentity{raw, raw, raw, h.identity, final}
	h.verifier.histories = [][32]byte{h.history, h.history, h.history, h.history}
	h.verifier.policies = []EffectiveCodexPolicy{h.policy, target}
	h.runner.results = append(h.runner.results,
		managedAgentResult(t, h.source), command.Result{Stdout: managedEmptyComposer("answer")},
		managedAgentResult(t, h.source), command.Result{Stdout: managedEmptyComposer("answer")}, command.Result{})
	managed := h.managedResult()
	managed.Policy = target
	h.converter.launch = func(_ context.Context, request ManagedCodexRequest) (*ManagedCodex, error) {
		h.launches = append(h.launches, request)
		return managed, nil
	}
	request := h.request()
	request.Customization = &CodexConversionCustomization{Desired: desired, ConfirmationToken: conversionConfirmationToken(h.source, raw, desired)}
	result, err := h.converter.Convert(context.Background(), request)
	if err != nil || result.Outcome != CodexConversionManaged || result.FallbackUsed || result.ManagedAttempts != 1 || requiredPolicyDrift(result.Policy, target) != "" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if conversionExitCount(h.runner.calls) != 1 || len(h.verifier.applied) != 1 || h.verifier.applied[0].ReasoningEffort != "medium" || len(h.launches) != 1 || h.launches[0].Policy != desired {
		t.Fatalf("calls=%q applied=%+v launches=%+v", h.runner.calls, h.verifier.applied, h.launches)
	}
}

func TestCodexConverterCanceledCustomPreparationAllowsAutomaticConversion(t *testing.T) {
	h := boundDefaultCustomizationHarness(t)
	bound := h.source
	h.source.Session = nil
	h.runner.results = conversionDiscoveryResults(t, h.source, bound)
	if _, err := h.converter.PrepareCustomization(context.Background(), h.source, managedPolicy(), 4*time.Second); err != nil {
		t.Fatal(err)
	}
	if conversionExitCount(h.runner.calls) != 0 || len(h.verifier.applied) != 0 {
		t.Fatal("preparation changed the source")
	}
	// The user cancels the custom disclosure, then chooses ordinary Convert
	// from the fresh snapshot carrying the discovered thread binding.
	h.source = bound
	raw := h.identity
	raw.ReasoningEffort = ""
	h.verifier.identities = []codexThreadIdentity{raw, raw, raw, h.identity, h.identity}
	h.verifier.histories = [][32]byte{h.history, h.history, h.history, h.history}
	h.verifier.constraints = []codexConstraintSnapshot{conversionConstraints(), conversionConstraints()}
	discovery := conversionDiscoveryResults(t, bound, bound)
	h.runner.results = append(discovery[:len(discovery)-3],
		managedAgentResult(t, bound), command.Result{Stdout: managedEmptyComposer("answer")},
		managedAgentResult(t, bound), command.Result{Stdout: managedEmptyComposer("answer")}, command.Result{})
	managed := h.managedResult()
	h.converter.launch = func(_ context.Context, request ManagedCodexRequest) (*ManagedCodex, error) {
		h.launches = append(h.launches, request)
		return managed, nil
	}
	result, err := h.converter.Convert(context.Background(), h.request())
	if err != nil || result.Outcome != CodexConversionManaged || result.ManagedAttempts != 1 || result.FallbackUsed || requiredPolicyDrift(result.Policy, h.policy) != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if conversionExitCount(h.runner.calls) != 1 || len(h.verifier.applied) != 1 || h.verifier.applied[0].ReasoningEffort != "medium" || len(h.launches) != 1 || h.launches[0].Policy != managedPolicy() {
		t.Fatalf("automatic conversion applied custom replacements: %+v", h.launches)
	}
}

func TestCodexConverterStockCustomizationRefusesUnprovenSources(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*conversionHarness)
		noInput bool
	}{
		{"unloaded", func(h *conversionHarness) { h.verifier.loadedErr = ErrManagedSettingsUnknown }, true},
		{"wrong thread", func(h *conversionHarness) { h.verifier.identities[0].ID = "wrong" }, true},
		{"wrong cwd", func(h *conversionHarness) { h.verifier.identities[0].CWD = "/other" }, true},
		{"missing model", func(h *conversionHarness) { h.verifier.identities[0].Model = "" }, true},
		{"missing provider", func(h *conversionHarness) { h.verifier.identities[0].ModelProvider = "" }, true},
		{"remote process", func(h *conversionHarness) {
			h.topology.inspected.Argv = []string{"codex", "--remote", "unix:///tmp/socket"}
		}, true},
		{"process replaced", func(h *conversionHarness) {
			changed := h.topology.inspected
			changed.PID++
			h.topology.inspectedSeq = []sessionhost.ForegroundProcess{h.topology.inspected, changed}
		}, true},
		{"draft", func(h *conversionHarness) { h.runner.results[1].Stdout = []byte("› UNSENT DRAFT") }, true},
		{"identity drift", func(h *conversionHarness) { h.verifier.identities[1].Model = "other" }, false},
		{"effort became explicit", func(h *conversionHarness) { h.verifier.identities[1].ReasoningEffort = "high" }, false},
		{"contradictory status", func(h *conversionHarness) {
			h.runner.results[13].Stdout = []byte(strings.ReplaceAll(string(h.runner.results[13].Stdout), "gpt-5.6-sol", "other-model"))
		}, false},
		{"post-status draft", func(h *conversionHarness) { h.runner.results[18].Stdout = []byte("› NEW INPUT") }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := boundDefaultCustomizationHarness(t)
			test.mutate(h)
			disclosure, err := h.converter.PrepareCustomization(context.Background(), h.source, managedPolicy(), 4*time.Second)
			if err == nil || !reflect.DeepEqual(disclosure, CodexConversionDisclosure{}) {
				t.Fatalf("unsafe preparation accepted: %+v error=%v", disclosure, err)
			}
			if test.noInput && hasPaneInput(h.runner.calls) {
				t.Fatalf("unsafe input: %q", h.runner.calls)
			}
			if conversionExitCount(h.runner.calls) != 0 || len(h.verifier.applied) != 0 || len(h.topology.runs) != 0 || len(h.topology.stops) != 0 {
				t.Fatal("refusal changed settings or stopped/resumed the source")
			}
		})
	}
}

func TestCodexConverterStockCustomizationRejectsWrongProcessDirectory(t *testing.T) {
	h := boundDefaultCustomizationHarness(t)
	h.topology.inspected.CWD = "/other"
	// The real topology verifier rejects this mismatch if discovery reaches it.
	h.topology.verifyErr = sessionhost.ErrStaleTarget

	disclosure, err := h.converter.PrepareCustomization(context.Background(), h.source, managedPolicy(), 4*time.Second)
	if !errors.Is(err, ErrManagedUnsupported) || !reflect.DeepEqual(disclosure, CodexConversionDisclosure{}) {
		t.Fatalf("wrong process directory: disclosure=%+v error=%v", disclosure, err)
	}
	if hasPaneInput(h.runner.calls) || conversionExitCount(h.runner.calls) != 0 || len(h.verifier.applied) != 0 || len(h.topology.runs) != 0 || len(h.topology.stops) != 0 || len(h.launches) != 0 {
		t.Fatal("unsupported source refusal changed input, settings, or processes")
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
	request.Customization = &CodexConversionCustomization{Desired: desired, ConfirmationToken: strings.Repeat("0", sha256.Size*2)}
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

type cancelAfterRun struct {
	command.Runner
	cancel context.CancelFunc
}

func (r cancelAfterRun) Run(ctx context.Context, executable string, args []string) (command.Result, error) {
	result, err := r.Runner.Run(ctx, executable, args)
	r.cancel()
	return result, err
}

func TestCodexConverterFallbackIdentityCancellationStopsAfterOnePoll(t *testing.T) {
	harness := newConversionHarness(t)
	harness.runner.results = nil
	harness.runner.calls = nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	harness.converter.launcher.Host.Runner = cancelAfterRun{Runner: harness.runner, cancel: cancel}
	_, err := harness.converter.awaitFallbackIdentity(ctx, managedOrigin(), harness.source.CWD, managedSourceID, 99, time.Second)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrManagedStale) {
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
		{},
	}}
	service := &fakeManagedService{snapshot: managedServiceSnapshot()}
	topology := &fakeManagedTopology{inspected: sessionhost.ForegroundProcess{PID: 77, Name: "codex", CWD: source.CWD, Argv: []string{"codex"}}, pid: 77}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: origin.SessionName})
	launcher.service, launcher.topology, launcher.verifiers = service, topology, fakeManagedVerifierFactory{verifier: verifier}
	harness := &conversionHarness{
		converter: CodexConverter{launcher: launcher}, service: service, topology: topology,
		verifier: verifier, runner: runner, source: source, identity: identity, history: history, policy: policy,
	}
	harness.converter.launcher.Host.Runner = runnerFunc(func(ctx context.Context, executable string, args []string) (command.Result, error) {
		result, err := runner.Run(ctx, executable, args)
		if conversionExitCount([][]string{args}) == 1 {
			topology.pid = 99
		}
		return result, err
	})
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
	argv := composeManagedTUIArgv(ManagedCodexRequest{Operation: ManagedResume, Directory: h.source.CWD, ThreadID: managedSourceID}, h.service.snapshot, managedSourceID)
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

func conversionDiscoveryResults(t *testing.T, unbound, bound Agent) []command.Result {
	t.Helper()
	target := sessionhost.Origin{SessionName: unbound.SessionName, WorkspaceID: unbound.WorkspaceID, TabID: unbound.TabID, PaneID: unbound.PaneID}
	status := fmt.Sprintf(`/status
│ >_ OpenAI Codex (v0.154.0) │
│ Model: gpt-5.6-sol (reasoning medium, summaries detailed) │
│ Directory: %s │
│ Session: %s │`, unbound.CWD, bound.Session.Value)
	return []command.Result{
		managedAgentResult(t, unbound),
		{Stdout: managedEmptyComposer("header")},
		paneScrollResult(t, target, 10),
		{Stdout: []byte("old output")},
		managedAgentResult(t, unbound),
		{Stdout: managedEmptyComposer("header")},
		{},
		managedAgentResult(t, unbound),
		{Stdout: managedStatusDraft("header")},
		managedAgentResult(t, unbound),
		{Stdout: managedStatusDraft("header")},
		{},
		paneScrollResult(t, target, 40),
		{Stdout: []byte("old output\n" + status)},
		{Stdout: managedEmptyComposer("status block")},
		managedAgentResult(t, unbound),
		{Stdout: managedEmptyComposer("status block")},
		managedAgentResult(t, unbound),
		{Stdout: managedEmptyComposer("status block")},
		{},
		managedAgentResult(t, bound),
		managedAgentResult(t, bound),
	}
}

func assertConversionDiscoveryCalls(t *testing.T, calls [][]string, sessionName string) {
	t.Helper()
	prefix := []string{"herdr"}
	if sessionName != "" {
		prefix = append(prefix, "--session", sessionName)
	}
	wantStatus := append(append([]string(nil), prefix...), "pane", "send-text", "w1:p1", "/status")
	wantEnter := append(append([]string(nil), prefix...), "pane", "send-keys", "w1:p1", "enter")
	wantBind := append(append([]string(nil), prefix...), "pane", "report-agent-session", "w1:p1", "--source", "herdr:codex", "--agent", "codex", "--agent-session-id", managedSourceID)
	for _, want := range [][]string{wantStatus, wantEnter, wantBind} {
		count := 0
		for _, call := range calls {
			if reflect.DeepEqual(call, want) {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("call=%q count=%d all=%q", want, count, calls)
		}
	}
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
