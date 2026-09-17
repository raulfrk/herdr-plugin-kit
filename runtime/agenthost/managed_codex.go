package agenthost

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrid"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

const (
	DefaultManagedReadyTimeout time.Duration = 30_000_000_000  // 30 seconds
	MaxManagedReadyTimeout     time.Duration = 300_000_000_000 // 5 minutes
	managedCleanupTimeout      time.Duration = 5_000_000_000   // 5 seconds
)

type ManagedOperation string

const (
	ManagedNew    ManagedOperation = "new"
	ManagedFork   ManagedOperation = "fork"
	ManagedResume ManagedOperation = "resume"
)

var (
	ErrManagedUnsupported     = errors.New("managed Codex operation is unsupported")
	ErrManagedPolicyDrift     = errors.New("managed Codex policy drift")
	ErrManagedStale           = errors.New("managed Codex state is stale")
	ErrManagedAmbiguous       = errors.New("managed Codex state is ambiguous")
	ErrManagedCleanup         = errors.New("managed Codex cleanup is incomplete")
	ErrManagedSettingsUnknown = errors.New("managed Codex settings update outcome is unknown")
)

type ManagedCodexPolicy struct {
	Model, ReasoningEffort, ApprovalPolicy, SandboxMode string
}

type ManagedCodexRequest struct {
	Operation  ManagedOperation
	Origin     sessionhost.Origin
	Directory  string
	Placement  sessionhost.Placement
	Direction  string
	TabLabel   string
	Focus      bool
	Source     Agent
	ThreadID   string
	SourceArgv []string
	Policy     ManagedCodexPolicy
	// InheritSourcePolicy makes ManagedFork read and preserve the exact
	// authoritative source policy instead of requiring a consumer to infer it.
	InheritSourcePolicy bool
	// ExtraArgv contains only validated, non-lifecycle Codex global options.
	// Identity, endpoint, directory, selector, and policy options remain Kit-owned.
	ExtraArgv          []string
	ReadyTimeout       time.Duration
	Prompt             *Prompt
	resumeSourcePolicy *EffectiveCodexPolicy
	resumeTargetPolicy *EffectiveCodexPolicy
	resumeShellGuard   func(context.Context) error
}

type ManagedCodex struct {
	Operation      ManagedOperation
	ThreadID       string
	SourceThreadID string
	Agent          Agent
	Target         sessionhost.CreatedTarget
	Process        sessionhost.ForegroundProcess
	Policy         EffectiveCodexPolicy
	Argv           []string
	Service        CodexServiceSnapshot
	Prompt         PromptResult
	cleanup        *managedCleanup
}

func (m *ManagedCodex) Cleanup(ctx context.Context) error {
	if m == nil || m.cleanup == nil {
		return nil
	}
	return m.cleanup.run(ctx)
}

type managedTopology interface {
	Launch(context.Context, sessionhost.LaunchRequest) (sessionhost.CreatedTarget, error)
	RunInShell(context.Context, sessionhost.Origin, string, []string) (bool, error)
	WaitForShell(context.Context, sessionhost.Origin, string) error
	InspectForegroundProcess(context.Context, sessionhost.Origin) (sessionhost.ForegroundProcess, error)
	VerifyForegroundProcess(context.Context, sessionhost.Origin, string, []string) (sessionhost.ForegroundProcess, error)
	StopForegroundProcess(context.Context, sessionhost.Origin, string, []string, int) error
	FocusPane(context.Context, sessionhost.Origin) error
	Cleanup(context.Context, sessionhost.CreatedTarget) error
}

type managedCodexService interface {
	Ensure(context.Context) (CodexServiceSnapshot, error)
}

type ManagedCodexLauncher struct {
	Host             Host
	topology         managedTopology
	service          managedCodexService
	verifiers        managedVerifierFactory
	discover         func(context.Context, sessionhost.Origin, string, []string, CodexServiceSnapshot, ManagedCodexPolicy) (Agent, string, error)
	discoverObserved func(context.Context, sessionhost.Origin, string, []string, CodexServiceSnapshot, ManagedCodexPolicy) (Agent, managedStatusObservation, error)
	deliver          func(context.Context, Prompt, PromptTarget) (PromptResult, error)
}

func NewManagedCodexLauncher(host Host) ManagedCodexLauncher {
	return ManagedCodexLauncher{Host: host}
}

func (l ManagedCodexLauncher) Launch(ctx context.Context, request ManagedCodexRequest) (*ManagedCodex, error) {
	if ctx == nil {
		panic("nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := l.validate(request); err != nil {
		return nil, fmt.Errorf("managed Codex: %w", err)
	}
	service, err := l.serviceClient().Ensure(ctx)
	if err != nil {
		return nil, fmt.Errorf("managed Codex service: %w", err)
	}
	if !validManagedServiceSnapshot(service) {
		return nil, fmt.Errorf("managed Codex service: %w", ErrManagedStale)
	}
	cleanup := &managedCleanup{topology: l.topologyHost()}
	var sourceID string
	var createdThread string
	fail := func(cause error, verifier managedVerifier) (*ManagedCodex, error) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), managedCleanupTimeout)
		defer cancel()
		cleanupErr := cleanup.stopTarget(cleanupCtx)
		if verifier != nil && createdThread != "" {
			cleanupErr = errors.Join(cleanupErr, verifier.DeleteThread(cleanupCtx, createdThread))
		}
		if verifier != nil {
			cleanupErr = errors.Join(cleanupErr, verifier.Close())
		}
		if cleanupErr != nil || request.Operation != ManagedResume && createdThread == "" && cleanup.target.PaneID != "" {
			cleanupErr = errors.Join(ErrManagedCleanup, cleanupErr)
		}
		return nil, errors.Join(cause, cleanupErr)
	}

	verifier, err := l.verifierFactory().Connect(ctx, service.SocketPath)
	if err != nil {
		return fail(fmt.Errorf("managed Codex verifier: %w", err), nil)
	}
	var preflight codexPreflight
	if !request.InheritSourcePolicy {
		preflightCtx, preflightCancel := context.WithTimeout(ctx, managedReadyTimeout(request.ReadyTimeout))
		preflight, err = verifier.Preflight(preflightCtx, request.Directory, request.Policy)
		preflightCancel()
		if err != nil {
			return fail(fmt.Errorf("managed Codex preflight: %w", classifyManagedPolicyError(err)), verifier)
		}
	}
	var sourceBefore codexThreadIdentity
	var sourceEffective EffectiveCodexPolicy
	var sourceProcess sessionhost.ForegroundProcess
	if request.Operation == ManagedFork {
		sourceID = request.Source.Session.Value
		safeCtx, safeCancel := context.WithTimeout(ctx, managedReadyTimeout(request.ReadyTimeout))
		safe, safeErr := l.awaitSafeSessionChange(safeCtx, request.Source, service)
		safeCancel()
		if safeErr != nil {
			return fail(fmt.Errorf("managed Codex source: %w", safeErr), verifier)
		}
		sourceProcess = safe.Process
		readCtx, readCancel := context.WithTimeout(ctx, managedReadyTimeout(request.ReadyTimeout))
		sourceBefore, err = verifier.ReadThread(readCtx, sourceID)
		readCancel()
		if err != nil || sourceBefore.CWD != request.Source.CWD {
			return fail(fmt.Errorf("managed Codex source read: %w", ErrManagedStale), verifier)
		}
		auditCtx, auditCancel := context.WithTimeout(ctx, managedReadyTimeout(request.ReadyTimeout))
		sourceEffective, err = verifier.ReadEffective(auditCtx, sourceID)
		auditCancel()
		if err != nil {
			return fail(fmt.Errorf("managed Codex source policy: %w", errors.Join(ErrManagedPolicyDrift, err)), verifier)
		}
		if request.InheritSourcePolicy {
			request.Policy = ManagedCodexPolicy{
				Model: sourceEffective.Model, ReasoningEffort: sourceEffective.ReasoningEffort,
				ApprovalPolicy: sourceEffective.ApprovalPolicy, SandboxMode: sourceEffective.SandboxMode,
			}
		}
		if sourceEffective.CWD != request.Source.CWD || sourceEffective.ApprovalPolicy != request.Policy.ApprovalPolicy || sourceEffective.SandboxMode != request.Policy.SandboxMode {
			return fail(fmt.Errorf("managed Codex source policy: %w", ErrManagedPolicyDrift), verifier)
		}
	} else if request.Operation == ManagedResume {
		sourceID = request.ThreadID
		readCtx, readCancel := context.WithTimeout(ctx, managedReadyTimeout(request.ReadyTimeout))
		sourceBefore, err = verifier.ReadThread(readCtx, sourceID)
		readCancel()
		if err != nil || sourceBefore.CWD != request.Directory {
			return fail(fmt.Errorf("managed Codex resume source: %w", ErrManagedStale), verifier)
		}
		auditCtx, auditCancel := context.WithTimeout(ctx, managedReadyTimeout(request.ReadyTimeout))
		sourceEffective, err = verifier.ReadEffective(auditCtx, sourceID)
		auditCancel()
		if err != nil {
			return fail(fmt.Errorf("managed Codex resume policy: %w", errors.Join(ErrManagedPolicyDrift, err)), verifier)
		}
		policyDrift := !matchesRequestedPolicy(sourceEffective, request.Policy)
		if request.resumeSourcePolicy != nil {
			policyDrift = requiredPolicyDrift(sourceEffective, *request.resumeSourcePolicy) != ""
		}
		if sourceEffective.CWD != request.Directory || policyDrift {
			return fail(fmt.Errorf("managed Codex resume policy: %w", ErrManagedPolicyDrift), verifier)
		}
	}
	if request.InheritSourcePolicy {
		preflightCtx, preflightCancel := context.WithTimeout(ctx, managedReadyTimeout(request.ReadyTimeout))
		preflight, err = verifier.Preflight(preflightCtx, request.Directory, request.Policy)
		preflightCancel()
		if err != nil {
			return fail(fmt.Errorf("managed Codex preflight: %w", classifyManagedPolicyError(err)), verifier)
		}
	}
	if err := verifier.Arm(); err != nil {
		return fail(fmt.Errorf("managed Codex verifier: %w", ErrManagedStale), verifier)
	}

	var target sessionhost.CreatedTarget
	argv := composeManagedTUIArgv(request, service.SocketPath, sourceID)
	cleanup.argv, cleanup.cwd = append([]string(nil), argv...), request.Directory
	if request.Operation == ManagedResume {
		target = sessionhost.CreatedTarget{Origin: request.Origin}
		cleanup.target, cleanup.resume = target, true
		if request.resumeShellGuard != nil {
			if guardErr := request.resumeShellGuard(ctx); guardErr != nil {
				return fail(fmt.Errorf("managed Codex resume shell guard: %w", guardErr), verifier)
			}
		}
		attempted, runErr := l.topologyHost().RunInShell(ctx, request.Origin, request.Directory, argv)
		// Ownership begins only after RunInShell passes every readiness check
		// and attempts pane input. Pre-submission failure owns nothing.
		cleanup.launchOwned = attempted
		if runErr != nil {
			return fail(fmt.Errorf("managed Codex resume launch: %w", runErr), verifier)
		}
	} else {
		target, err = l.topologyHost().Launch(ctx, sessionhost.LaunchRequest{
			Origin: request.Origin, Argv: argv, Directory: request.Directory, Placement: request.Placement,
			Direction: request.Direction, TabLabel: request.TabLabel, Focus: false,
		})
		if err != nil {
			return fail(fmt.Errorf("managed Codex launch: %w", err), verifier)
		}
		cleanup.target, cleanup.created = target, true
	}
	process, err := l.waitForProcess(ctx, target.Origin, request.Directory, argv, managedReadyTimeout(request.ReadyTimeout))
	if err != nil {
		return fail(fmt.Errorf("managed Codex process: %w", err), verifier)
	}
	cleanup.pid = process.PID
	originalPID := process.PID

	identityCtx, cancel := context.WithTimeout(ctx, managedReadyTimeout(request.ReadyTimeout))
	defer cancel()
	discover := l.discover
	if discover == nil {
		discover = l.discoverManagedIdentity
	}
	discoveryPolicy := request.Policy
	if request.resumeSourcePolicy != nil {
		discoveryPolicy = ManagedCodexPolicy{
			Model: request.resumeSourcePolicy.Model, ReasoningEffort: request.resumeSourcePolicy.ReasoningEffort,
			ApprovalPolicy: request.resumeSourcePolicy.ApprovalPolicy, SandboxMode: request.resumeSourcePolicy.SandboxMode,
		}
	}
	agent, threadID, err := discover(identityCtx, target.Origin, request.Directory, argv, service, discoveryPolicy)
	if err != nil {
		return fail(fmt.Errorf("managed Codex pane identity: %w", err), verifier)
	}
	identity, err := verifier.ConfirmIdentity(identityCtx, request.Operation, sourceID, threadID)
	if err != nil || identity.CWD != request.Directory {
		if err == nil {
			err = ErrManagedStale
		}
		return fail(fmt.Errorf("managed Codex identity: %w", err), verifier)
	}
	if request.Operation != ManagedResume {
		createdThread = identity.ID
	}
	agent, err = l.bindManagedIdentity(identityCtx, target.Origin, request.Directory, agent, threadID)
	if err != nil {
		return fail(fmt.Errorf("managed Codex pane identity: %w", err), verifier)
	}
	if request.Operation == ManagedFork {
		readCtx, readCancel := context.WithTimeout(ctx, managedReadyTimeout(request.ReadyTimeout))
		sourceAfter, readErr := verifier.ReadThread(readCtx, sourceID)
		readCancel()
		if readErr != nil || sourceAfter != sourceBefore {
			return fail(fmt.Errorf("managed Codex source changed: %w", ErrManagedStale), verifier)
		}
		after, findErr := l.Host.find(ctx, request.Source.PaneID)
		if findErr != nil || !sameImmutable(request.Source, after) || after.CWD != request.Source.CWD {
			return fail(fmt.Errorf("managed Codex source pane changed: %w", ErrManagedStale), verifier)
		}
		afterProcess, processErr := l.topologyHost().InspectForegroundProcess(ctx, request.Origin)
		if processErr != nil || !sameForegroundProcess(sourceProcess, afterProcess) {
			return fail(fmt.Errorf("managed Codex source process changed: %w", ErrManagedStale), verifier)
		}
	}

	effective := preflight.EffectiveCodexPolicy
	effective.ModelProvider = identity.ModelProvider
	effective.CWD = identity.CWD
	if request.Operation == ManagedFork {
		expected := sourceEffective
		expected.Model = request.Policy.Model
		expected.ReasoningEffort = request.Policy.ReasoningEffort
		expected.CWD = identity.CWD
		auditCtx, auditCancel := context.WithTimeout(ctx, managedReadyTimeout(request.ReadyTimeout))
		effective, err = verifier.ReadEffective(auditCtx, identity.ID)
		auditCancel()
		if err != nil {
			return fail(fmt.Errorf("managed Codex policy audit: %w", errors.Join(ErrManagedPolicyDrift, err)), verifier)
		}
		if drift := forkPolicyDrift(effective, expected, sourceProcess.Argv); drift != "" {
			return fail(fmt.Errorf("managed Codex policy audit (%s): %w", drift, ErrManagedPolicyDrift), verifier)
		}
	} else if request.Operation == ManagedResume {
		auditCtx, auditCancel := context.WithTimeout(ctx, managedReadyTimeout(request.ReadyTimeout))
		effective, err = verifier.ReadEffective(auditCtx, identity.ID)
		auditCancel()
		if err != nil {
			return fail(fmt.Errorf("managed Codex resume policy audit: %w", errors.Join(ErrManagedPolicyDrift, err)), verifier)
		}
		expected := sourceEffective
		if request.resumeTargetPolicy != nil {
			expected = *request.resumeTargetPolicy
		}
		if drift := forkPolicyDrift(effective, expected, request.SourceArgv); drift != "" {
			if request.resumeTargetPolicy == nil {
				return fail(fmt.Errorf("managed Codex resume changed policy (%s): %w", drift, ErrManagedPolicyDrift), verifier)
			}
			reconcileCtx, reconcileCancel := context.WithTimeout(ctx, managedReadyTimeout(request.ReadyTimeout))
			effective, err = verifier.ReconcileEffective(reconcileCtx, identity.ID, expected)
			reconcileCancel()
			if err != nil {
				return fail(fmt.Errorf("managed Codex resume policy reconciliation: %w", err), verifier)
			}
			identity.Model = effective.Model
			identity.ReasoningEffort = effective.ReasoningEffort
		}
	}
	process, err = l.topologyHost().VerifyForegroundProcess(ctx, target.Origin, request.Directory, argv)
	if err != nil || process.PID != originalPID {
		return fail(fmt.Errorf("managed Codex final process: %w", ErrManagedStale), verifier)
	}
	recheckCtx, recheckCancel := context.WithTimeout(ctx, managedReadyTimeout(request.ReadyTimeout))
	checked, err := verifier.RecheckIdentity(recheckCtx, identity.ID)
	recheckCancel()
	if err != nil || checked != identity {
		return fail(fmt.Errorf("managed Codex final identity: %w", ErrManagedStale), verifier)
	}
	agent, err = l.getStartedAgent(ctx, target.Origin, request.Directory, false, identity.ID)
	if err != nil {
		return fail(fmt.Errorf("managed Codex final target: %w", err), verifier)
	}
	managed := &ManagedCodex{
		Operation: request.Operation, ThreadID: identity.ID, SourceThreadID: sourceID,
		Agent: agent, Target: target, Process: process, Policy: effective,
		Argv: append([]string(nil), argv...), Service: service, cleanup: cleanup,
	}
	if request.Prompt != nil {
		var source *Agent
		if request.Operation == ManagedFork {
			sourceCopy := request.Source
			source = &sourceCopy
		}
		deliver := l.deliver
		if deliver == nil {
			deliver = NewPromptHost(l.Host).Deliver
		}
		promptResult, promptErr := deliver(ctx, *request.Prompt, PromptTarget{
			Created: target, Agent: agent, Directory: request.Directory,
			Argv: argv, Source: source, CodexService: service,
		})
		managed.Prompt = promptResult
		if promptErr != nil {
			if promptResult.State == PromptNotAttempted {
				return fail(fmt.Errorf("managed Codex prompt readiness: %w", promptErr), verifier)
			}
			_ = verifier.Close()
			return managed, promptErr
		}
		agent, managed.Agent = promptResult.Agent, promptResult.Agent
	}
	if request.Focus {
		if err := l.topologyHost().FocusPane(ctx, target.Origin); err != nil {
			if request.Prompt != nil && managed.Prompt.State == PromptAcknowledged {
				_ = verifier.Close()
				return managed, fmt.Errorf("managed Codex focus: %w", err)
			}
			return fail(fmt.Errorf("managed Codex focus: %w", err), verifier)
		}
		focusedProcess, processErr := l.topologyHost().VerifyForegroundProcess(ctx, target.Origin, request.Directory, argv)
		if processErr != nil || focusedProcess.PID != originalPID {
			if request.Prompt != nil && managed.Prompt.State == PromptAcknowledged {
				_ = verifier.Close()
				return managed, fmt.Errorf("managed Codex focused process: %w", ErrManagedStale)
			}
			return fail(fmt.Errorf("managed Codex focused process: %w", ErrManagedStale), verifier)
		}
		agent, err = l.getStartedAgent(ctx, target.Origin, request.Directory, true, identity.ID)
		if err != nil {
			if request.Prompt != nil && managed.Prompt.State == PromptAcknowledged {
				_ = verifier.Close()
				return managed, fmt.Errorf("managed Codex focused target: %w", err)
			}
			return fail(fmt.Errorf("managed Codex focused target: %w", err), verifier)
		}
		recheckCtx, recheckCancel = context.WithTimeout(ctx, managedReadyTimeout(request.ReadyTimeout))
		checked, err = verifier.RecheckIdentity(recheckCtx, identity.ID)
		recheckCancel()
		if err != nil || checked != identity {
			if request.Prompt != nil && managed.Prompt.State == PromptAcknowledged {
				_ = verifier.Close()
				return managed, fmt.Errorf("managed Codex focused identity: %w", ErrManagedStale)
			}
			return fail(fmt.Errorf("managed Codex focused identity: %w", ErrManagedStale), verifier)
		}
	}
	managed.Agent = agent
	_ = verifier.Close()
	return managed, nil
}

func forkPolicyDrift(actual, expected EffectiveCodexPolicy, sourceArgv []string) string {
	adjusted := expected
	if expected.PermissionProfile == "" && strings.HasPrefix(actual.PermissionProfile, ":") && explicitSandboxArgv(sourceArgv, expected.SandboxMode) {
		adjusted.PermissionProfile = actual.PermissionProfile
	}
	return requiredPolicyDrift(actual, adjusted)
}

func explicitSandboxArgv(argv []string, sandbox string) bool {
	found := false
	for i := 1; i+1 < len(argv); i++ {
		if argv[i] == "-p" || argv[i] == "--profile" || argv[i] == "--dangerously-bypass-approvals-and-sandbox" || argv[i] == "-c" && (strings.HasPrefix(argv[i+1], "default_permissions=") || strings.HasPrefix(argv[i+1], "permissions.")) {
			return false
		}
		if (argv[i] == "-s" || argv[i] == "--sandbox") && argv[i+1] == sandbox {
			if found {
				return false
			}
			found = true
		}
	}
	return found
}

func sameForegroundProcess(a, b sessionhost.ForegroundProcess) bool {
	if a.PID != b.PID || a.Name != b.Name || a.CWD != b.CWD || len(a.Argv) != len(b.Argv) {
		return false
	}
	for i := range a.Argv {
		if a.Argv[i] != b.Argv[i] {
			return false
		}
	}
	return true
}

func matchesRequestedPolicy(effective EffectiveCodexPolicy, requested ManagedCodexPolicy) bool {
	return effective.Model == requested.Model && effective.ReasoningEffort == requested.ReasoningEffort && effective.ApprovalPolicy == requested.ApprovalPolicy && effective.SandboxMode == requested.SandboxMode
}

func (l ManagedCodexLauncher) validate(request ManagedCodexRequest) error {
	if l.Host.Runner == nil || l.Host.Session != request.Origin.SessionName {
		return ErrManagedUnsupported
	}
	if request.Origin.SessionName != "" {
		if err := herdrid.ValidateSessionName(request.Origin.SessionName); err != nil {
			return err
		}
	}
	for name, value := range map[string]string{"workspace_id": request.Origin.WorkspaceID, "tab_id": request.Origin.TabID, "pane_id": request.Origin.PaneID} {
		if err := validateBounded(name, value, maxIdentityBytes, false); err != nil {
			return err
		}
	}
	if len(request.Directory) > maxPathBytes || !filepath.IsAbs(request.Directory) || filepath.Clean(request.Directory) != request.Directory || strings.ContainsRune(request.Directory, 0) {
		return errors.New("managed Codex directory must be clean, absolute, and NUL-free")
	}
	if request.InheritSourcePolicy {
		if request.Operation != ManagedFork || request.Policy != (ManagedCodexPolicy{}) {
			return errors.New("managed Codex source-policy inheritance is invalid")
		}
	} else if err := validateManagedPolicy(request.Policy); err != nil {
		return err
	}
	if err := validateManagedExtraArgv(request.ExtraArgv); err != nil {
		return err
	}
	if (request.resumeSourcePolicy == nil) != (request.resumeTargetPolicy == nil) {
		return errors.New("managed Resume reconciliation policies must be paired")
	}
	if request.resumeShellGuard != nil && request.Operation != ManagedResume {
		return errors.New("managed Resume shell guard is invalid")
	}
	if request.resumeSourcePolicy != nil && (request.Operation != ManagedResume || request.resumeSourcePolicy.CWD != request.Directory || request.resumeTargetPolicy.CWD != request.Directory || !matchesRequestedPolicy(*request.resumeTargetPolicy, request.Policy)) {
		return errors.New("managed Resume reconciliation policy is invalid")
	}
	if request.ReadyTimeout < 0 || request.ReadyTimeout > MaxManagedReadyTimeout || request.ReadyTimeout > 0 && request.ReadyTimeout <= 3*time.Second {
		return errors.New("managed Codex readiness timeout is out of range")
	}
	if request.Prompt != nil {
		if err := validatePreparedPrompt(*request.Prompt); err != nil {
			return err
		}
		if request.Operation == ManagedResume {
			return errors.New("managed Resume does not accept a start prompt")
		}
	}
	switch request.Operation {
	case ManagedNew:
		if request.ThreadID != "" || request.Source.PaneID != "" || request.SourceArgv != nil {
			return errors.New("managed New does not accept an existing thread")
		}
		return validateManagedPlacement(request)
	case ManagedFork:
		if request.ThreadID != "" || request.SourceArgv != nil || validateTarget(request.Source) != nil || request.Source.Session == nil || !strings.EqualFold(request.Source.Kind, "codex") || request.Source.Session.Source != "herdr:codex" || request.Source.Session.Agent != "codex" || request.Source.Session.Kind != "id" || !validCodexThreadID(request.Source.Session.Value) || request.Source.SessionName != request.Origin.SessionName || request.Source.WorkspaceID != request.Origin.WorkspaceID || request.Source.TabID != request.Origin.TabID || request.Source.PaneID != request.Origin.PaneID {
			return errors.New("managed Fork source identity is invalid")
		}
		if len(request.Source.CWD) > maxPathBytes || !filepath.IsAbs(request.Source.CWD) || filepath.Clean(request.Source.CWD) != request.Source.CWD || strings.ContainsRune(request.Source.CWD, 0) {
			return errors.New("managed Fork source CWD is invalid")
		}
		return validateManagedPlacement(request)
	case ManagedResume:
		if !validManagedThreadID(request.ThreadID) || request.Source.PaneID != "" || request.Placement != "" || request.Direction != "" || request.TabLabel != "" || request.Focus || len(request.ExtraArgv) != 0 {
			return errors.New("managed Resume accepts only an existing shell pane and exact thread")
		}
		if err := validateManagedSourceArgv(request.SourceArgv); err != nil {
			return err
		}
	default:
		return ErrManagedUnsupported
	}
	return nil
}

func validateManagedSourceArgv(argv []string) error {
	if argv == nil {
		return nil
	}
	if len(argv) == 0 || len(argv) > sessionhost.MaxArgv || filepath.Base(argv[0]) != "codex" {
		return errors.New("managed Resume source argv is invalid")
	}
	for _, arg := range argv {
		if len(arg) > sessionhost.MaxArgBytes || !utf8.ValidString(arg) || strings.ContainsRune(arg, 0) {
			return errors.New("managed Resume source argv is invalid")
		}
	}
	return nil
}

func validateManagedPlacement(request ManagedCodexRequest) error {
	if len(request.TabLabel) > maxIdentityBytes || strings.ContainsAny(request.TabLabel, "\x00\r\n") {
		return errors.New("managed tab label is invalid")
	}
	switch request.Placement {
	case sessionhost.NewTab:
		if request.Direction != "" {
			return errors.New("managed new-tab placement does not accept direction")
		}
	case sessionhost.NewPane:
		if request.Direction != "right" && request.Direction != "down" {
			return errors.New("managed new-pane direction must be right or down")
		}
	default:
		return errors.New("managed placement is unsupported")
	}
	return nil
}

var managedToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

func validateManagedPolicy(policy ManagedCodexPolicy) error {
	for name, value := range map[string]string{"model": policy.Model, "reasoning effort": policy.ReasoningEffort} {
		if len(value) > maxIdentityBytes || !managedToken.MatchString(value) {
			return fmt.Errorf("managed Codex %s is invalid", name)
		}
	}
	switch policy.ApprovalPolicy {
	case "untrusted", "on-request", "never":
	default:
		return errors.New("managed Codex approval policy is unsupported")
	}
	switch policy.SandboxMode {
	case "read-only", "workspace-write", "danger-full-access":
	default:
		return errors.New("managed Codex sandbox mode is unsupported")
	}
	return nil
}

func validateManagedExtraArgv(argv []string) error {
	if len(argv) > sessionhost.MaxArgv-16 {
		return errors.New("managed Codex extra argv exceeds the argument limit")
	}
	seen := make(map[string]struct{}, len(argv))
	for index := 0; index < len(argv); index++ {
		name := argv[index]
		switch name {
		case "--search", "--no-alt-screen":
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("managed Codex extra option %q is duplicated", name)
			}
			seen[name] = struct{}{}
		case "--enable", "--disable":
			if index+1 >= len(argv) {
				return fmt.Errorf("managed Codex extra option %q requires a value", name)
			}
			value := argv[index+1]
			if len(value) > sessionhost.MaxArgBytes || !managedToken.MatchString(value) {
				return fmt.Errorf("managed Codex extra option %q has an invalid value", name)
			}
			key := "feature\x00" + value
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("managed Codex extra option %q is duplicated", name)
			}
			seen[key] = struct{}{}
			index++
		default:
			return fmt.Errorf("managed Codex extra option %q is unsupported", name)
		}
	}
	return nil
}

func composeManagedTUIArgv(request ManagedCodexRequest, socket, sourceID string) []string {
	remote := "unix://" + socket
	if request.Operation == ManagedResume {
		return []string{"codex", "resume", "--remote", remote, "--cd", request.Directory, request.ThreadID}
	}
	policy := request.Policy
	args := []string{"codex"}
	if request.Operation == ManagedFork {
		args = append(args, "fork")
	}
	args = append(args, request.ExtraArgv...)
	args = append(args,
		"-m", policy.Model,
		"-c", "model_reasoning_effort="+strconv.Quote(policy.ReasoningEffort),
	)
	if request.Operation == ManagedNew {
		args = append(args, "-a", policy.ApprovalPolicy, "-s", policy.SandboxMode)
	}
	args = append(args, "--strict-config", "--remote", remote, "--cd", request.Directory)
	if request.Operation == ManagedFork {
		args = append(args, sourceID)
	}
	return args
}

func (l ManagedCodexLauncher) getStartedAgent(ctx context.Context, target sessionhost.Origin, cwd string, focused bool, threadID string) (Agent, error) {
	var response agentInfoResponse
	if err := l.Host.runJSON(ctx, []string{"agent", "get", target.PaneID}, &response); err != nil {
		return Agent{}, err
	}
	if response.ID != "cli:agent:get" || response.Result.Type != "agent_info" || response.Result.Agent.LaunchPending {
		return Agent{}, ErrManagedStale
	}
	agent, err := response.Result.Agent.public()
	if err != nil {
		return Agent{}, err
	}
	agent.SessionName = l.Host.Session
	if !sameManagedTarget(agent, target, cwd, focused, threadID) {
		return Agent{}, ErrManagedStale
	}
	return agent, nil
}

func sameManagedTarget(agent Agent, target sessionhost.Origin, cwd string, focused bool, threadID string) bool {
	return strings.EqualFold(agent.Kind, "codex") && agent.SessionName == target.SessionName && agent.WorkspaceID == target.WorkspaceID && agent.TabID == target.TabID && agent.PaneID == target.PaneID && agent.CWD == cwd && agent.Focused == focused && agent.Session != nil && agent.Session.Source == "herdr:codex" && agent.Session.Agent == "codex" && agent.Session.Kind == "id" && agent.Session.Value == threadID
}

func (l ManagedCodexLauncher) topologyHost() managedTopology {
	if l.topology != nil {
		return l.topology
	}
	return sessionhost.Host{Runner: l.Host.Runner, Herdr: l.Host.Herdr, Session: l.Host.Session}
}

func (l ManagedCodexLauncher) verifierFactory() managedVerifierFactory {
	if l.verifiers != nil {
		return l.verifiers
	}
	return codexVerifierFactory{}
}

func (l ManagedCodexLauncher) serviceClient() managedCodexService {
	if l.service != nil {
		return l.service
	}
	return NewCodexService(l.Host.Runner)
}

func managedReadyTimeout(timeout time.Duration) time.Duration {
	if timeout == 0 {
		return DefaultManagedReadyTimeout
	}
	return timeout
}

func classifyManagedPolicyError(err error) error {
	if errors.Is(err, ErrManagedPolicyDrift) {
		return err
	}
	return errors.Join(ErrManagedUnsupported, err)
}

type managedCleanup struct {
	once        sync.Once
	topology    managedTopology
	target      sessionhost.CreatedTarget
	created     bool
	resume      bool
	launchOwned bool
	argv        []string
	cwd         string
	pid         int
	err         error
}

func (c *managedCleanup) stopTarget(ctx context.Context) error {
	if c.created {
		err := c.topology.Cleanup(ctx, c.target)
		if err == nil {
			c.created = false
		}
		return err
	}
	if !c.resume || !c.launchOwned {
		return nil
	}
	pid := c.pid
	if pid <= 0 {
		process, err := c.topology.VerifyForegroundProcess(ctx, c.target.Origin, c.cwd, c.argv)
		if err != nil {
			return c.reconcilePending(ctx)
		}
		pid = process.PID
	}
	err := c.topology.StopForegroundProcess(ctx, c.target.Origin, c.cwd, c.argv, pid)
	if err != nil {
		// Once a launch PID has been captured, never substitute a later
		// same-argv process: it may belong to a different transaction.
		return err
	}
	err = c.topology.WaitForShell(ctx, c.target.Origin, c.cwd)
	if err == nil {
		c.launchOwned = false
	}
	return err
}

func (c *managedCleanup) reconcilePending(ctx context.Context) error {
	for {
		process, processErr := c.topology.VerifyForegroundProcess(ctx, c.target.Origin, c.cwd, c.argv)
		if processErr == nil {
			if err := c.topology.StopForegroundProcess(ctx, c.target.Origin, c.cwd, c.argv, process.PID); err != nil {
				return err
			}
		}
		if err := c.topology.WaitForShell(ctx, c.target.Origin, c.cwd); err == nil {
			if err := waitManaged(ctx, 25*time.Millisecond); err != nil {
				return errors.Join(ErrManagedStale, err)
			}
			if _, processErr := c.topology.VerifyForegroundProcess(ctx, c.target.Origin, c.cwd, c.argv); processErr == nil {
				continue
			}
			if err := c.topology.WaitForShell(ctx, c.target.Origin, c.cwd); err != nil {
				continue
			}
			c.launchOwned = false
			return nil
		}
		if err := waitManaged(ctx, 25*time.Millisecond); err != nil {
			return errors.Join(ErrManagedStale, err)
		}
	}
}

func (c *managedCleanup) run(ctx context.Context) error {
	if ctx == nil {
		panic("nil context")
	}
	c.once.Do(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), managedCleanupTimeout)
		defer cancel()
		if err := c.stopTarget(cleanupCtx); err != nil {
			c.err = errors.Join(c.err, ErrManagedCleanup, err)
		}
	})
	return c.err
}
