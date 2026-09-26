package agenthost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

// CodexConversionOutcome describes the process left running after conversion.
type CodexConversionOutcome string

const (
	CodexConversionManaged  CodexConversionOutcome = "managed"
	CodexConversionFallback CodexConversionOutcome = "ordinary_fallback"
)

var (
	ErrCodexConversionCapture        = errors.New("Codex conversion capture failed")
	ErrCodexConversionFallback       = errors.New("Codex conversion restored the ordinary session")
	ErrCodexConversionComposer       = errors.New("Codex conversion composer is not provably empty before stop")
	ErrCodexConversionExitUnverified = errors.New("Codex conversion exit was attempted but not verified")
)

// CodexConversionRequest selects one ordinary Codex TUI in its existing pane.
type CodexConversionRequest struct {
	Source        Agent
	ReadyTimeout  time.Duration
	Customization *CodexConversionCustomization
}

// CodexConversionCustomization carries a desired transient policy and the
// token returned by PrepareCustomization after the caller shows its disclosure.
type CodexConversionCustomization struct {
	Desired           ManagedCodexPolicy
	ConfirmationToken string
}

// CodexConversionDisclosure separates known replacements from values that
// Codex 0.154.0 cannot expose until the ordinary writer has stopped.
type CodexConversionDisclosure struct {
	ThreadID          string
	CWD               string
	Desired           ManagedCodexPolicy
	KnownReplacements []string
	UnknownBeforeStop []string
	ConfirmationToken string
}

var codexConversionUnknownBeforeStop = []string{
	"approval_policy", "approvals_reviewer", "sandbox_mode", "permission_profile",
	"permission_profile_extends", "service_tier", "runtime_workspace_roots",
	"instruction_sources", "multi_agent_mode", "personality", "collaboration_mode",
	"instruction_contents",
}

// CodexConversionResult reports the exact managed or ordinary process that the
// transaction verified. A fallback result accompanies ErrCodexConversionFallback.
type CodexConversionResult struct {
	Outcome         CodexConversionOutcome
	ThreadID        string
	Agent           Agent
	Process         sessionhost.ForegroundProcess
	Argv            []string
	Policy          EffectiveCodexPolicy
	Service         CodexServiceSnapshot
	Managed         *ManagedCodex
	ManagedAttempts int
	FallbackUsed    bool
}

// CodexConverter converts an ordinary Codex TUI to the shared managed App Server
// without changing panes or replacing the conversation.
type CodexConverter struct {
	launcher     ManagedCodexLauncher
	launch       func(context.Context, ManagedCodexRequest) (*ManagedCodex, error)
	captureShell func(context.Context, sessionhost.Origin, string, *sessionhost.ForegroundProcess) (conversionShellLease, error)
	verifyShell  func(context.Context, sessionhost.Origin, string, conversionShellLease) error
}

type conversionShellLease struct {
	Process  sessionhost.ForegroundProcess
	Revision uint64
	Visible  string
}

// NewCodexConverter creates a converter for the Host's default or named Herdr session.
func NewCodexConverter(host Host) CodexConverter {
	return CodexConverter{launcher: NewManagedCodexLauncher(host)}
}

// PrepareCustomization validates a desired transient policy. When Herdr has no
// Codex session binding, it first performs the guarded /status discovery used
// by ordinary Codex launches. Callers must show the returned disclosure and
// only pass its token to Convert after a dedicated second user confirmation.
func (c CodexConverter) PrepareCustomization(ctx context.Context, source Agent, desired ManagedCodexPolicy, readyTimeout time.Duration) (CodexConversionDisclosure, error) {
	request := CodexConversionRequest{Source: source, ReadyTimeout: readyTimeout}
	if ctx == nil {
		panic("nil context")
	}
	if err := ctx.Err(); err != nil {
		return CodexConversionDisclosure{}, err
	}
	if err := c.validateResolvable(request); err != nil {
		return CodexConversionDisclosure{}, fmt.Errorf("prepare Codex customization: %w", err)
	}
	if err := validateManagedPolicy(desired); err != nil {
		return CodexConversionDisclosure{}, fmt.Errorf("prepare Codex customization: %w", err)
	}
	ready := managedReadyTimeout(readyTimeout)
	service, err := c.launcher.serviceClient().Ensure(ctx)
	if err != nil || !validManagedServiceSnapshot(service) {
		return CodexConversionDisclosure{}, fmt.Errorf("prepare Codex customization service: %w", errors.Join(ErrManagedStale, err))
	}
	request.Source, _, err = c.resolveSource(ctx, request.Source, ready, service)
	if err != nil {
		return CodexConversionDisclosure{}, fmt.Errorf("prepare Codex customization identity: %w", err)
	}
	if err := c.validate(request); err != nil {
		return CodexConversionDisclosure{}, fmt.Errorf("prepare Codex customization identity: %w", err)
	}
	source = request.Source
	verifier, err := c.launcher.verifierFactory().Connect(ctx, service.SocketPath)
	if err != nil {
		return CodexConversionDisclosure{}, fmt.Errorf("prepare Codex customization verifier: %w", err)
	}
	defer verifier.Close()
	readCtx, cancel := context.WithTimeout(ctx, ready)
	defer cancel()
	constraints, err := verifier.ReadConstraints(readCtx, source.CWD)
	if err == nil {
		_, err = constraints.validate(source.CWD, desired)
	}
	identity, identityErr := verifier.ReadThread(readCtx, source.Session.Value)
	if err == nil {
		err = identityErr
	}
	if err == nil {
		err = validateConversionIdentity(identity, source, source.Session.Value)
	}
	if err == nil {
		_, err = verifier.ReadStableHistory(readCtx, source.Session.Value)
	}
	if err != nil {
		return CodexConversionDisclosure{}, fmt.Errorf("prepare Codex customization capture: %w", errors.Join(ErrCodexConversionCapture, err))
	}
	disclosure := CodexConversionDisclosure{
		ThreadID: identity.ID, CWD: identity.CWD, Desired: desired,
		UnknownBeforeStop: append([]string(nil), codexConversionUnknownBeforeStop...),
	}
	if identity.Model != desired.Model {
		disclosure.KnownReplacements = append(disclosure.KnownReplacements, "model")
	}
	if identity.ReasoningEffort != desired.ReasoningEffort {
		disclosure.KnownReplacements = append(disclosure.KnownReplacements, "reasoning_effort")
	}
	disclosure.ConfirmationToken = conversionConfirmationToken(source, identity, desired)
	return disclosure, nil
}

func (c CodexConverter) Convert(ctx context.Context, request CodexConversionRequest) (CodexConversionResult, error) {
	if ctx == nil {
		panic("nil context")
	}
	if err := ctx.Err(); err != nil {
		return CodexConversionResult{}, err
	}
	if err := c.validateResolvable(request); err != nil {
		return CodexConversionResult{}, fmt.Errorf("convert Codex: %w", err)
	}
	if request.Customization != nil {
		if err := validateManagedPolicy(request.Customization.Desired); err != nil || !validConversionConfirmationToken(request.Customization.ConfirmationToken) {
			return CodexConversionResult{}, fmt.Errorf("convert Codex customization: %w", ErrManagedUnsupported)
		}
	}
	ready := managedReadyTimeout(request.ReadyTimeout)
	service, err := c.launcher.serviceClient().Ensure(ctx)
	if err != nil {
		return CodexConversionResult{}, fmt.Errorf("convert Codex service: %w", errors.Join(ErrManagedStale, err))
	}
	if !validManagedServiceSnapshot(service) {
		return CodexConversionResult{}, fmt.Errorf("convert Codex service: %w", errors.Join(ErrManagedStale, err))
	}
	var observed managedStatusObservation
	request.Source, observed, err = c.resolveSource(ctx, request.Source, ready, service)
	if err != nil {
		return CodexConversionResult{}, fmt.Errorf("convert Codex identity: %w", err)
	}
	if err := c.validate(request); err != nil {
		return CodexConversionResult{}, fmt.Errorf("convert Codex identity: %w", err)
	}
	verifier, err := c.launcher.verifierFactory().Connect(ctx, service.SocketPath)
	if err != nil {
		return CodexConversionResult{}, fmt.Errorf("convert Codex verifier: %w", err)
	}
	defer verifier.Close()

	threadID := request.Source.Session.Value
	preCtx, preCancel := context.WithTimeout(ctx, ready)
	constraints, err := verifier.ReadConstraints(preCtx, request.Source.CWD)
	if err != nil {
		preCancel()
		return CodexConversionResult{}, fmt.Errorf("convert Codex pre-stop constraints: %w", errors.Join(ErrCodexConversionCapture, err))
	}
	identity, err := verifier.ReadLoadedThread(preCtx, threadID)
	if err == nil {
		err = validateObservedConversionIdentity(identity, request.Source, threadID, observed)
	}
	var history [32]byte
	if err == nil {
		history, err = verifier.ReadStableHistory(preCtx, threadID)
	}
	preCancel()
	if err != nil {
		return CodexConversionResult{}, fmt.Errorf("convert Codex pre-stop capture: %w", errors.Join(ErrCodexConversionCapture, err))
	}
	if request.Customization != nil {
		if err := validateManagedPolicy(request.Customization.Desired); err != nil || request.Customization.ConfirmationToken != conversionConfirmationToken(request.Source, identity, request.Customization.Desired) {
			return CodexConversionResult{}, fmt.Errorf("convert Codex customization: %w", ErrManagedUnsupported)
		}
		if _, err := constraints.validate(request.Source.CWD, request.Customization.Desired); err != nil {
			return CodexConversionResult{}, fmt.Errorf("convert Codex customization constraints: %w", classifyManagedPolicyError(err))
		}
	}
	return c.stopCaptureResume(ctx, request, ready, service, verifier, constraints, identity, history, observed)
}

func (c CodexConverter) stopCaptureResume(ctx context.Context, request CodexConversionRequest, ready time.Duration, service CodexServiceSnapshot, verifier managedVerifier, constraints codexConstraintSnapshot, before codexThreadIdentity, history [32]byte, observed managedStatusObservation) (CodexConversionResult, error) {
	target := sessionhost.Origin{
		SessionName: request.Source.SessionName, WorkspaceID: request.Source.WorkspaceID,
		TabID: request.Source.TabID, PaneID: request.Source.PaneID,
	}
	stopped, err := c.launcher.awaitSafeSessionChangeWithFocus(ctx, request.Source, ready, false, true)
	if err != nil {
		return CodexConversionResult{}, fmt.Errorf("convert Codex source: %w", err)
	}
	if stopped.Service != service || stopped.Agent.Session == nil || stopped.Agent.Session.Value != before.ID || stopped.Process.CWD != request.Source.CWD {
		return CodexConversionResult{}, fmt.Errorf("convert Codex source changed: %w", ErrManagedStale)
	}
	process, err := c.launcher.topologyHost().VerifyForegroundProcess(ctx, target, before.CWD, stopped.Process.Argv)
	if err != nil || !sameForegroundProcess(process, stopped.Process) {
		return CodexConversionResult{}, fmt.Errorf("convert Codex source process: %w", errors.Join(ErrManagedStale, err))
	}
	if err := ctx.Err(); err != nil {
		return CodexConversionResult{}, err
	}
	// Native exit restores the terminal modes that a signal can leave behind.
	// Herdr cannot bind this key to a PID atomically; the guarded input race is
	// specific to conversion. Never repeat the key or fall back to a signal.
	base := CodexConversionResult{ThreadID: before.ID, Service: service}
	exitErr := c.launcher.runHerdr(ctx, "pane", "send-keys", target.PaneID, "ctrl+d")
	exitErr = errors.Join(exitErr, ctx.Err())
	if exitErr == nil {
		exitErr = c.launcher.topologyHost().WaitForShell(ctx, target, before.CWD)
	}
	if exitErr != nil {
		return base, fmt.Errorf("convert Codex exit: %w", errors.Join(ErrCodexConversionExitUnverified, exitErr))
	}
	shellCtx, shellCancel := context.WithTimeout(ctx, ready)
	shellLease, err := c.captureConversionShell(shellCtx, target, before.CWD, nil)
	shellCancel()
	if err != nil {
		return base, fmt.Errorf("convert Codex stopped shell: %w", err)
	}

	captureCtx, captureCancel := context.WithTimeout(ctx, ready)
	after, captureErr := verifier.ReadThread(captureCtx, before.ID)
	if captureErr != nil {
		captureErr = fmt.Errorf("thread identity: %w", captureErr)
	}
	if captureErr == nil && after != before {
		captureErr = fmt.Errorf("thread identity changed: %w", ErrManagedStale)
	}
	if captureErr == nil {
		var afterHistory [32]byte
		afterHistory, captureErr = verifier.ReadStableHistory(captureCtx, before.ID)
		if captureErr != nil {
			captureErr = fmt.Errorf("stable history: %w", captureErr)
		}
		if captureErr == nil && afterHistory != history {
			captureErr = fmt.Errorf("stable history changed: %w", ErrManagedStale)
		}
	}
	if captureErr == nil && before.ReasoningEffort == "" {
		// Codex 0.154.0 can show the effective effort in a guarded /status
		// while storing null in thread/read. Only the stopped writer may be
		// normalized, and an uncertain update is never assumed to have failed.
		safeNoDispatch, updateErr := verifier.ApplyObservedReasoning(captureCtx, before, observed.policy)
		readback, readErr := verifier.ReadThread(captureCtx, before.ID)
		readbackHistory, historyErr := verifier.ReadStableHistory(captureCtx, before.ID)
		normalized := before
		normalized.ReasoningEffort = observed.policy.ReasoningEffort
		if readErr != nil || historyErr != nil || readbackHistory != history || readback != before && readback != normalized {
			captureCancel()
			if safeNoDispatch && updateErr != nil && (readErr != nil || historyErr != nil) && (readErr != nil || readback == before) && (historyErr != nil || readbackHistory == history) {
				return c.fallbackWithoutReadback(ctx, ready, target, before, shellLease.Process, &shellLease, base, fmt.Errorf("observed reasoning update was not dispatched; API readback unavailable: %w", errors.Join(ErrManagedSettingsUnknown, updateErr, readErr, historyErr)))
			}
			return base, fmt.Errorf("convert Codex reasoning readback uncertain; ordinary source remains stopped: %w", errors.Join(ErrManagedSettingsUnknown, updateErr, readErr, historyErr))
		}
		if updateErr != nil || readback != normalized {
			captureCancel()
			return c.fallback(ctx, ready, service, verifier, readback, history, target, shellLease.Process, &shellLease, base, fmt.Errorf("observed reasoning update failed: %w", errors.Join(ErrManagedSettingsUnknown, updateErr)))
		}
		before = normalized
	}
	var policy EffectiveCodexPolicy
	if captureErr == nil {
		policy, captureErr = verifier.ReadEffective(captureCtx, before.ID)
		if captureErr != nil {
			captureErr = fmt.Errorf("effective policy: %w", captureErr)
		}
	}
	var afterConstraints codexConstraintSnapshot
	if captureErr == nil {
		afterConstraints, captureErr = verifier.ReadConstraints(captureCtx, request.Source.CWD)
		if captureErr != nil {
			captureErr = fmt.Errorf("constraints: %w", captureErr)
		}
	}
	if captureErr == nil && !constraints.equal(afterConstraints) {
		captureErr = fmt.Errorf("constraints changed (%s): %w", strings.Join(constraints.driftFields(afterConstraints), ","), ErrManagedPolicyDrift)
	}
	targetPolicy := policy
	managedPolicy := ManagedCodexPolicy{Model: policy.Model, ReasoningEffort: policy.ReasoningEffort, ApprovalPolicy: policy.ApprovalPolicy, SandboxMode: policy.SandboxMode}
	if request.Customization != nil {
		managedPolicy = request.Customization.Desired
		targetPolicy.Model = managedPolicy.Model
		targetPolicy.ReasoningEffort = managedPolicy.ReasoningEffort
		targetPolicy.ApprovalPolicy = managedPolicy.ApprovalPolicy
		if targetPolicy.SandboxMode != managedPolicy.SandboxMode {
			targetPolicy.PermissionProfile = ""
			targetPolicy.PermissionProfileExtends = ""
			targetPolicy.SandboxNetworkAccess = false
			targetPolicy.SandboxExcludeSlashTmp = false
			targetPolicy.SandboxExcludeTmpdirEnvVar = false
			targetPolicy.SandboxWritableRoots = nil
		}
		targetPolicy.SandboxMode = managedPolicy.SandboxMode
	}
	if captureErr == nil {
		if policy.CWD != before.CWD || policy.ModelProvider != before.ModelProvider || policy.Model != before.Model || policy.ReasoningEffort != before.ReasoningEffort {
			captureErr = fmt.Errorf("captured identity policy changed: %w", ErrManagedPolicyDrift)
		} else if _, captureErr = constraints.validateConversion(before.CWD, managedPolicy, policy); captureErr != nil {
			captureErr = fmt.Errorf("captured policy rejected: %w", classifyManagedPolicyError(captureErr))
		}
	}
	captureCancel()
	if captureErr != nil {
		return c.fallback(ctx, ready, service, verifier, before, history, target, shellLease.Process, &shellLease, base, fmt.Errorf("post-stop capture: %w", errors.Join(ErrCodexConversionCapture, captureErr)))
	}
	base.Policy = targetPolicy

	launch := c.launch
	if launch == nil {
		launch = c.launcher.Launch
	}
	launchRequest := ManagedCodexRequest{
		Operation: ManagedResume,
		Origin:    target,
		Directory: request.Source.CWD, ThreadID: before.ID,
		SourceArgv: append([]string(nil), stopped.Process.Argv...), Policy: managedPolicy,
		ReadyTimeout: ready,
	}
	launchRequest.resumeSourcePolicy = &policy
	launchRequest.resumeTargetPolicy = &targetPolicy
	var managed *ManagedCodex
	var launchErr error
	for attempt := 1; attempt <= 2; attempt++ {
		base.ManagedAttempts = attempt
		attemptLease := shellLease
		launchRequest.resumeShellGuard = func(guardCtx context.Context) error {
			verifyCtx, verifyCancel := context.WithTimeout(guardCtx, ready)
			defer verifyCancel()
			return c.verifyConversionShell(verifyCtx, target, before.CWD, attemptLease)
		}
		managed, launchErr = launch(ctx, launchRequest)
		if launchErr == nil {
			break
		}
		if attempt == 2 || !eligibleConversionRetry(launchErr) {
			break
		}
		if err := c.recheckRecovery(ctx, ready, service, verifier, before, history); err != nil {
			return base, errors.Join(launchErr, err)
		}
		shellCtx, shellCancel := context.WithTimeout(ctx, ready)
		shellLease, err = c.captureConversionShell(shellCtx, target, before.CWD, &shellLease.Process)
		shellCancel()
		if err != nil {
			return base, errors.Join(launchErr, err)
		}
	}
	if launchErr != nil {
		if !eligibleConversionRetry(launchErr) {
			return base, launchErr
		}
		return c.fallback(ctx, ready, service, verifier, before, history, target, shellLease.Process, nil, base, launchErr)
	}
	base.Managed = managed
	base.Outcome = CodexConversionManaged
	base.Agent, base.Process = managed.Agent, managed.Process
	base.Argv = append([]string(nil), managed.Argv...)
	if err := validateManagedConversionResult(managed, launchRequest, service, targetPolicy); err != nil {
		return base, err
	}
	verifyCtx, verifyCancel := context.WithTimeout(ctx, ready)
	afterHistory, verifyErr := verifier.ReadStableHistory(verifyCtx, before.ID)
	afterIdentity, identityErr := verifier.ReadThread(verifyCtx, before.ID)
	afterPolicy, policyErr := verifier.ReadEffective(verifyCtx, before.ID)
	verifyCancel()
	if verifyErr != nil || identityErr != nil || policyErr != nil || afterHistory != history || !sameFinalConversionIdentity(afterIdentity, before, targetPolicy) || requiredPolicyDrift(afterPolicy, targetPolicy) != "" {
		return base, fmt.Errorf("convert Codex final verification: %w", errors.Join(ErrManagedStale, verifyErr, identityErr, policyErr))
	}
	return base, nil
}

func conversionConfirmationToken(source Agent, identity codexThreadIdentity, desired ManagedCodexPolicy) string {
	values := []string{source.SessionName, source.WorkspaceID, source.TabID, source.PaneID, identity.ID, identity.CWD, identity.ModelProvider, identity.Model, identity.ReasoningEffort, desired.Model, desired.ReasoningEffort, desired.ApprovalPolicy, desired.SandboxMode}
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(digest[:])
}

func validConversionConfirmationToken(token string) bool {
	if len(token) != sha256.Size*2 || token != strings.ToLower(token) {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

func sameFinalConversionIdentity(actual, before codexThreadIdentity, target EffectiveCodexPolicy) bool {
	return actual.ID == before.ID && actual.CWD == before.CWD && actual.ForkedFromID == before.ForkedFromID && actual.ModelProvider == before.ModelProvider && actual.Model == target.Model && actual.ReasoningEffort == target.ReasoningEffort
}

func (c CodexConverter) captureConversionShell(ctx context.Context, target sessionhost.Origin, cwd string, expected *sessionhost.ForegroundProcess) (conversionShellLease, error) {
	if c.captureShell != nil {
		return c.captureShell(ctx, target, cwd, expected)
	}
	var prior conversionShellLease
	for ctx.Err() == nil {
		current, err := c.observeConversionShell(ctx, target, cwd)
		if err != nil {
			return conversionShellLease{}, err
		}
		if expected != nil && !sameForegroundProcess(current.Process, *expected) {
			return conversionShellLease{}, ErrManagedStale
		}
		if prior.Process.PID != 0 && sameConversionShellLease(prior, current) {
			return current, nil
		}
		prior = current
		_ = waitManaged(ctx, 25*time.Millisecond)
	}
	return conversionShellLease{}, errors.Join(ErrManagedPaneBusy, ctx.Err())
}

func (c CodexConverter) verifyConversionShell(ctx context.Context, target sessionhost.Origin, cwd string, expected conversionShellLease) error {
	if c.verifyShell != nil {
		return c.verifyShell(ctx, target, cwd, expected)
	}
	current, err := c.captureConversionShell(ctx, target, cwd, &expected.Process)
	if err != nil {
		return err
	}
	if !sameConversionShellLease(current, expected) {
		return ErrManagedPaneBusy
	}
	return nil
}

func (c CodexConverter) observeConversionShell(ctx context.Context, target sessionhost.Origin, cwd string) (conversionShellLease, error) {
	if err := c.launcher.topologyHost().WaitForShell(ctx, target, cwd); err != nil {
		return conversionShellLease{}, err
	}
	process, err := c.launcher.topologyHost().InspectForegroundProcess(ctx, target)
	if err != nil {
		return conversionShellLease{}, err
	}
	readPane := func() (uint64, error) {
		var response struct {
			ID     string `json:"id"`
			Result struct {
				Type string          `json:"type"`
				Pane json.RawMessage `json:"pane"`
			} `json:"result"`
		}
		if err := c.launcher.Host.runJSON(ctx, []string{"pane", "get", target.PaneID}, &response); err != nil {
			return 0, err
		}
		var pane struct {
			PaneID      string `json:"pane_id"`
			WorkspaceID string `json:"workspace_id"`
			TabID       string `json:"tab_id"`
			CWD         string `json:"cwd"`
			Focused     bool   `json:"focused"`
			Revision    uint64 `json:"revision"`
		}
		if response.ID != "cli:pane:get" || response.Result.Type != "pane_info" || json.Unmarshal(response.Result.Pane, &pane) != nil || pane.PaneID != target.PaneID || pane.WorkspaceID != target.WorkspaceID || pane.TabID != target.TabID || pane.CWD != cwd || pane.Revision == 0 {
			return 0, ErrManagedStale
		}
		if pane.Focused {
			return 0, ErrManagedPaneBusy
		}
		return pane.Revision, nil
	}
	revision, err := readPane()
	if err != nil {
		return conversionShellLease{}, err
	}
	visible, err := c.launcher.readPaneANSI(ctx, target.PaneID, "visible", managedStatusLines)
	if err != nil {
		return conversionShellLease{}, err
	}
	afterRevision, err := readPane()
	if err != nil {
		return conversionShellLease{}, err
	}
	afterProcess, err := c.launcher.topologyHost().InspectForegroundProcess(ctx, target)
	if err != nil || revision != afterRevision || !sameForegroundProcess(process, afterProcess) {
		if err == nil {
			err = ErrManagedStale
		}
		return conversionShellLease{}, err
	}
	return conversionShellLease{Process: afterProcess, Revision: afterRevision, Visible: visible}, nil
}

func sameConversionShellLease(left, right conversionShellLease) bool {
	return left.Revision == right.Revision && left.Visible == right.Visible && sameForegroundProcess(left.Process, right.Process)
}

func (c CodexConverter) fallback(ctx context.Context, ready time.Duration, service CodexServiceSnapshot, verifier managedVerifier, identity codexThreadIdentity, history [32]byte, target sessionhost.Origin, shellProcess sessionhost.ForegroundProcess, lease *conversionShellLease, result CodexConversionResult, cause error) (CodexConversionResult, error) {
	return c.fallbackResume(ctx, ready, service, verifier, identity, history, target, shellProcess, lease, result, cause, true)
}

// When App Server was lost before any settings update was dispatched, the
// already verified raw identity/history cannot be read again. Restore only
// that exact ordinary thread, reporting the missing API readback in the cause.
func (c CodexConverter) fallbackWithoutReadback(ctx context.Context, ready time.Duration, target sessionhost.Origin, identity codexThreadIdentity, shellProcess sessionhost.ForegroundProcess, lease *conversionShellLease, result CodexConversionResult, cause error) (CodexConversionResult, error) {
	return c.fallbackResume(ctx, ready, CodexServiceSnapshot{}, nil, identity, [32]byte{}, target, shellProcess, lease, result, cause, false)
}

func (c CodexConverter) fallbackResume(ctx context.Context, ready time.Duration, service CodexServiceSnapshot, verifier managedVerifier, identity codexThreadIdentity, history [32]byte, target sessionhost.Origin, shellProcess sessionhost.ForegroundProcess, lease *conversionShellLease, result CodexConversionResult, cause error, checkReadback bool) (CodexConversionResult, error) {
	if checkReadback {
		if err := c.recheckRecovery(ctx, ready, service, verifier, identity, history); err != nil {
			return result, errors.Join(cause, err)
		}
	}
	var current conversionShellLease
	if lease == nil {
		shellCtx, shellCancel := context.WithTimeout(ctx, ready)
		var err error
		current, err = c.captureConversionShell(shellCtx, target, identity.CWD, &shellProcess)
		shellCancel()
		if err != nil {
			return result, errors.Join(cause, err)
		}
	} else {
		current = *lease
		if !sameForegroundProcess(current.Process, shellProcess) {
			return result, errors.Join(cause, ErrManagedStale)
		}
	}
	verifyCtx, verifyCancel := context.WithTimeout(ctx, ready)
	verifyErr := c.verifyConversionShell(verifyCtx, target, identity.CWD, current)
	verifyCancel()
	if verifyErr != nil {
		return result, errors.Join(cause, verifyErr)
	}
	argv := []string{"codex", "resume", "--cd", identity.CWD, identity.ID}
	attempted, err := c.launcher.topologyHost().RunInShell(ctx, target, identity.CWD, argv)
	if err != nil || !attempted {
		return result, errors.Join(cause, err)
	}
	process, err := c.launcher.waitForProcess(ctx, target, identity.CWD, argv, ready)
	if err != nil {
		return result, errors.Join(cause, err)
	}
	agent, err := c.awaitFallbackIdentity(ctx, target, identity.CWD, identity.ID, process.PID, ready)
	if err != nil {
		return result, errors.Join(cause, err)
	}
	if checkReadback {
		checkCtx, cancel := context.WithTimeout(ctx, ready)
		checked, checkErr := verifier.ReadThread(checkCtx, identity.ID)
		checkedHistory, historyErr := verifier.ReadStableHistory(checkCtx, identity.ID)
		cancel()
		if checkErr != nil || historyErr != nil || checked != identity || checkedHistory != history {
			return result, errors.Join(cause, ErrManagedStale, checkErr, historyErr)
		}
	}
	result.Outcome, result.FallbackUsed = CodexConversionFallback, true
	result.Agent, result.Process, result.Argv = agent, process, argv
	return result, errors.Join(ErrCodexConversionFallback, cause)
}

func (c CodexConverter) recheckRecovery(ctx context.Context, ready time.Duration, service CodexServiceSnapshot, verifier managedVerifier, identity codexThreadIdentity, history [32]byte) error {
	current, err := c.launcher.serviceClient().Ensure(ctx)
	if err != nil || current != service {
		return errors.Join(ErrManagedStale, err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, ready)
	defer cancel()
	checked, err := verifier.ReadThread(checkCtx, identity.ID)
	if err != nil || checked != identity {
		return errors.Join(ErrManagedStale, err)
	}
	checkedHistory, err := verifier.ReadStableHistory(checkCtx, identity.ID)
	if err != nil || checkedHistory != history {
		return errors.Join(ErrManagedStale, err)
	}
	return nil
}

func (c CodexConverter) awaitFallbackIdentity(ctx context.Context, target sessionhost.Origin, cwd, threadID string, pid int, ready time.Duration) (Agent, error) {
	waitCtx, cancel := context.WithTimeout(ctx, ready)
	defer cancel()
	var prior Agent
	for waitCtx.Err() == nil {
		agent, err := c.launcher.getStartedAgent(waitCtx, target, cwd, false, threadID)
		if err == nil {
			if process, processErr := c.launcher.topologyHost().VerifyForegroundProcess(waitCtx, target, cwd, []string{"codex", "resume", "--cd", cwd, threadID}); processErr == nil && process.PID == pid {
				if prior.Session != nil && sameImmutable(prior, agent) && *prior.Session == *agent.Session {
					return agent, nil
				}
				prior = agent
				continue
			}
		}
		prior = Agent{}
		_ = waitManaged(waitCtx, 25*time.Millisecond)
	}
	return Agent{}, errors.Join(ErrManagedStale, waitCtx.Err())
}

func (c CodexConverter) validate(request CodexConversionRequest) error {
	if err := c.validateResolvable(request); err != nil {
		return ErrManagedUnsupported
	}
	if !validManagedSession(request.Source.Session) {
		return ErrManagedUnsupported
	}
	return nil
}

func (c CodexConverter) validateResolvable(request CodexConversionRequest) error {
	if c.launcher.Host.Runner == nil || request.Source.SessionName != c.launcher.Host.Session || validateTarget(request.Source) != nil || !strings.EqualFold(request.Source.Kind, "codex") || request.Source.Session != nil && !validManagedSession(request.Source.Session) {
		return ErrManagedUnsupported
	}
	if !filepath.IsAbs(request.Source.CWD) || filepath.Clean(request.Source.CWD) != request.Source.CWD || strings.ContainsRune(request.Source.CWD, 0) {
		return ErrManagedUnsupported
	}
	if request.ReadyTimeout < 0 || request.ReadyTimeout > MaxManagedReadyTimeout || request.ReadyTimeout > 0 && request.ReadyTimeout <= 3*time.Second {
		return ErrManagedUnsupported
	}
	return nil
}

func (c CodexConverter) resolveSource(ctx context.Context, source Agent, ready time.Duration, service CodexServiceSnapshot) (Agent, managedStatusObservation, error) {
	if source.Session != nil {
		return source, managedStatusObservation{}, nil
	}
	target := sessionhost.Origin{
		SessionName: source.SessionName, WorkspaceID: source.WorkspaceID,
		TabID: source.TabID, PaneID: source.PaneID,
	}
	process, err := c.launcher.topologyHost().InspectForegroundProcess(ctx, target)
	if err != nil {
		return Agent{}, managedStatusObservation{}, fmt.Errorf("inspect ordinary Codex process: %w", err)
	}
	if process.Name != "codex" {
		return Agent{}, managedStatusObservation{}, fmt.Errorf("inspect ordinary Codex process name: %w", ErrManagedStale)
	}
	if len(process.Argv) == 0 {
		return Agent{}, managedStatusObservation{}, fmt.Errorf("inspect ordinary Codex process argv: %w", ErrManagedStale)
	}
	if filepath.Base(process.Argv[0]) != "codex" {
		return Agent{}, managedStatusObservation{}, fmt.Errorf("inspect ordinary Codex executable: %w", ErrManagedStale)
	}
	if process.CWD != source.CWD {
		return Agent{}, managedStatusObservation{}, fmt.Errorf("inspect ordinary Codex cwd: %w", ErrManagedStale)
	}
	if codexRemoteArgv(process.Argv) || validateManagedSourceArgv(process.Argv) != nil {
		return Agent{}, managedStatusObservation{}, fmt.Errorf("inspect ordinary Codex argv: %w", ErrManagedUnsupported)
	}
	resolveCtx, cancel := context.WithTimeout(ctx, ready)
	defer cancel()
	agent, observation, err := c.launcher.discoverManagedIdentityForSource(resolveCtx, target, source.CWD, source, process, service)
	if err != nil {
		return Agent{}, managedStatusObservation{}, fmt.Errorf("discover ordinary Codex identity: %w", err)
	}
	threadID := observation.identity
	if !validManagedThreadID(threadID) {
		return Agent{}, managedStatusObservation{}, ErrManagedNarrow
	}
	verifier, err := c.launcher.verifierFactory().Connect(resolveCtx, service.SocketPath)
	if err != nil {
		return Agent{}, managedStatusObservation{}, fmt.Errorf("validate ordinary Codex identity: %w", err)
	}
	identity, identityErr := verifier.ReadThread(resolveCtx, threadID)
	closeErr := verifier.Close()
	if identityErr == nil {
		identityErr = validateObservedConversionIdentity(identity, agent, threadID, observation)
	}
	if err := errors.Join(identityErr, closeErr); err != nil {
		return Agent{}, managedStatusObservation{}, fmt.Errorf("validate ordinary Codex identity: %w", err)
	}
	agent, err = c.recheckSourceBeforeBinding(resolveCtx, target, agent, process)
	if err != nil {
		return Agent{}, managedStatusObservation{}, fmt.Errorf("recheck ordinary Codex identity: %w", err)
	}
	bound, err := c.launcher.bindManagedIdentity(resolveCtx, target, source.CWD, agent, threadID)
	if err != nil {
		return Agent{}, managedStatusObservation{}, fmt.Errorf("bind ordinary Codex identity: %w", err)
	}
	return bound, observation, nil
}

func (c CodexConverter) recheckSourceBeforeBinding(ctx context.Context, target sessionhost.Origin, expected Agent, expectedProcess sessionhost.ForegroundProcess) (Agent, error) {
	var prior Agent
	for sample := 0; sample < 2; sample++ {
		wire, current, err := c.launcher.readManagedAgent(ctx, target, expected.CWD)
		if err != nil {
			return Agent{}, err
		}
		if current.Session != nil {
			return Agent{}, ErrManagedStale
		}
		if !sameImmutable(expected, current) {
			return Agent{}, ErrManagedStale
		}
		if !managedPaneReadyForSessionChange(wire, false) {
			return Agent{}, ErrManagedPaneBusy
		}
		process, err := c.launcher.topologyHost().InspectForegroundProcess(ctx, target)
		if err != nil {
			return Agent{}, err
		}
		if !sameForegroundProcess(expectedProcess, process) {
			return Agent{}, ErrManagedStale
		}
		visible, err := c.launcher.readPaneANSI(ctx, target.PaneID, "visible", managedStatusLines)
		if err != nil {
			return Agent{}, err
		}
		if !emptyCodexComposer(visible) {
			return Agent{}, errors.Join(ErrCodexConversionComposer, ErrManagedPaneBusy)
		}
		if sample != 0 && !sameManagedPreInputObservation(prior, current) {
			return Agent{}, ErrManagedStale
		}
		prior = current
	}
	return prior, nil
}

func validateConversionIdentity(identity codexThreadIdentity, source Agent, threadID string) error {
	if identity.ID != threadID || identity.CWD != source.CWD || identity.ModelProvider == "" || identity.Model == "" || identity.ReasoningEffort == "" {
		return ErrManagedStale
	}
	return nil
}

func validateObservedConversionIdentity(identity codexThreadIdentity, source Agent, threadID string, observed managedStatusObservation) error {
	if identity.ID != threadID || identity.CWD != source.CWD || identity.ModelProvider == "" || identity.Model == "" {
		return ErrManagedStale
	}
	if observed.fromStatus {
		if observed.identity != threadID || observed.policyIsPrefix || observed.policy.Model != identity.Model || !supportedObservedCodexEffort(observed.policy.ReasoningEffort) || identity.ReasoningEffort != "" && identity.ReasoningEffort != observed.policy.ReasoningEffort {
			return ErrManagedPolicyDrift
		}
	} else if identity.ReasoningEffort == "" {
		// A pre-bound source has no fresh, pane-guarded /status observation.
		return ErrManagedNarrow
	}
	return nil
}

func supportedObservedCodexEffort(effort string) bool {
	switch effort {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

func eligibleConversionRetry(err error) bool {
	for _, blocked := range []error{context.Canceled, context.DeadlineExceeded, ErrManagedUnsupported, ErrManagedPolicyDrift, ErrManagedStale, ErrManagedAmbiguous, ErrManagedCleanup, ErrManagedSettingsUnknown, ErrManagedPaneBusy, ErrManagedNarrow, ErrCodexServiceUnavailable, ErrCodexServiceIncompatible, ErrCodexServiceReceipt, ErrCodexServiceDrift, errCodexProtocol} {
		if errors.Is(err, blocked) {
			return false
		}
	}
	return err != nil
}

func validateManagedConversionResult(managed *ManagedCodex, request ManagedCodexRequest, service CodexServiceSnapshot, captured EffectiveCodexPolicy) error {
	if managed == nil || managed.Operation != ManagedResume || managed.ThreadID != request.ThreadID || managed.SourceThreadID != request.ThreadID || managed.Target.Origin != request.Origin || managed.Service != service || managed.Agent.Session == nil || managed.Agent.Session.Value != request.ThreadID || managed.Process.CWD != request.Directory || requiredPolicyDrift(managed.Policy, captured) != "" {
		return ErrManagedStale
	}
	return nil
}
