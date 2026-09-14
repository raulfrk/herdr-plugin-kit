package agenthost

import (
	"context"
	"errors"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

// ordinaryCodexBinding applies the api.10 identity discovery and binding
// protocol to a literal/custom Codex launch. It deliberately owns no process.
type ordinaryCodexBinding struct {
	launcher        ManagedCodexLauncher
	verifier        managedVerifier
	service         CodexServiceSnapshot
	operation       ManagedOperation
	policy          ManagedCodexPolicy
	source          Agent
	sourceID        string
	sourceBefore    codexThreadIdentity
	sourceProcess   sessionhost.ForegroundProcess
	readyTimeout    time.Duration
	createdThreadID string
}

func beginOrdinaryCodexBinding(ctx context.Context, launcher ManagedCodexLauncher, operation ManagedOperation, source Agent, directory string, policy ManagedCodexPolicy, timeout time.Duration) (*ordinaryCodexBinding, error) {
	if operation != ManagedNew && operation != ManagedFork || validateManagedPolicy(policy) != nil {
		return nil, ErrManagedUnsupported
	}
	service, err := launcher.serviceClient().Ensure(ctx)
	if err != nil || !validManagedServiceSnapshot(service) {
		return nil, errors.Join(ErrManagedStale, err)
	}
	verifier, err := launcher.verifierFactory().Connect(ctx, service.SocketPath)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*ordinaryCodexBinding, error) {
		return nil, errors.Join(cause, verifier.Close())
	}
	ready := managedReadyTimeout(timeout)
	preflightCtx, cancel := context.WithTimeout(ctx, ready)
	_, err = verifier.Preflight(preflightCtx, directory, policy)
	cancel()
	if err != nil {
		return fail(classifyManagedPolicyError(err))
	}
	binding := &ordinaryCodexBinding{
		launcher: launcher, verifier: verifier, service: service, operation: operation,
		policy: policy, source: source, readyTimeout: ready,
	}
	if operation == ManagedFork {
		if source.Session == nil || !validManagedSession(source.Session) || source.CWD != directory {
			return fail(ErrManagedStale)
		}
		binding.sourceID = source.Session.Value
		safeCtx, safeCancel := context.WithTimeout(ctx, ready)
		safe, safeErr := launcher.awaitSafeSessionChange(safeCtx, source, service)
		safeCancel()
		if safeErr != nil {
			return fail(safeErr)
		}
		binding.sourceProcess = safe.Process
		readCtx, readCancel := context.WithTimeout(ctx, ready)
		binding.sourceBefore, err = verifier.ReadThread(readCtx, binding.sourceID)
		readCancel()
		if err != nil || binding.sourceBefore.CWD != source.CWD {
			return fail(ErrManagedStale)
		}
		auditCtx, auditCancel := context.WithTimeout(ctx, ready)
		effective, auditErr := verifier.ReadEffective(auditCtx, binding.sourceID)
		auditCancel()
		if auditErr != nil || effective.CWD != source.CWD || effective.ApprovalPolicy != policy.ApprovalPolicy || effective.SandboxMode != policy.SandboxMode {
			return fail(errors.Join(ErrManagedPolicyDrift, auditErr))
		}
	}
	if err := verifier.Arm(); err != nil {
		return fail(err)
	}
	return binding, nil
}

func (b *ordinaryCodexBinding) Finish(ctx context.Context, target sessionhost.CreatedTarget, directory string, argv []string) (Agent, error) {
	readyCtx, cancel := context.WithTimeout(ctx, b.readyTimeout)
	defer cancel()
	discover := b.launcher.discover
	if discover == nil {
		discover = b.launcher.discoverManagedIdentity
	}
	agent, threadID, err := discover(readyCtx, target.Origin, directory, argv, b.service, b.policy)
	if err != nil {
		return Agent{}, err
	}
	identity, err := b.verifier.ConfirmIdentity(readyCtx, b.operation, b.sourceID, threadID)
	if err != nil || identity.CWD != directory {
		return Agent{}, errors.Join(ErrManagedStale, err)
	}
	b.createdThreadID = threadID
	agent, err = b.launcher.bindManagedIdentity(readyCtx, target.Origin, directory, agent, threadID)
	if err != nil {
		return Agent{}, err
	}
	// A zero-turn New thread is authoritative through the armed start event,
	// thread/read, and the exact preflight policy. Codex 0.154.0 cannot resume
	// that thread for an effective-policy audit until its first turn exists.
	// Forks already have persisted history and retain the stronger readback.
	if b.operation == ManagedFork {
		effective, auditErr := b.verifier.ReadEffective(readyCtx, threadID)
		if auditErr != nil || effective.CWD != directory || !matchesRequestedPolicy(effective, b.policy) {
			return Agent{}, errors.Join(ErrManagedPolicyDrift, auditErr)
		}
	}
	checked, err := b.verifier.RecheckIdentity(readyCtx, threadID)
	if err != nil || checked != identity {
		return Agent{}, errors.Join(ErrManagedStale, err)
	}
	if b.operation == ManagedFork {
		sourceAfter, readErr := b.verifier.ReadThread(readyCtx, b.sourceID)
		current, findErr := b.launcher.Host.find(readyCtx, b.source.PaneID)
		process, processErr := b.launcher.topologyHost().InspectForegroundProcess(readyCtx, sessionhost.Origin{
			SessionName: b.source.SessionName, WorkspaceID: b.source.WorkspaceID,
			TabID: b.source.TabID, PaneID: b.source.PaneID,
		})
		if readErr != nil || sourceAfter != b.sourceBefore || findErr != nil || !sameImmutable(b.source, current) || current.CWD != b.source.CWD || processErr != nil || !sameForegroundProcess(b.sourceProcess, process) {
			return Agent{}, ErrManagedStale
		}
	}
	return agent, nil
}

func (b *ordinaryCodexBinding) CleanupThread(ctx context.Context) error {
	if b == nil || b.verifier == nil || b.createdThreadID == "" {
		return nil
	}
	return b.verifier.DeleteThread(ctx, b.createdThreadID)
}

func (b *ordinaryCodexBinding) Close() error {
	if b == nil || b.verifier == nil {
		return nil
	}
	return b.verifier.Close()
}
