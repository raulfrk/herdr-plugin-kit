package agenthost

import (
	"context"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

// ordinaryCodexBinding binds a literal native launch to its exact pane. An
// embedded Codex does not emit events to the shared service; identity comes
// only from that pane's authoritative hook or a fresh, guarded /status.
type ordinaryCodexBinding struct {
	launcher      ManagedCodexLauncher
	operation     ManagedOperation
	policy        ManagedCodexPolicy
	source        Agent
	sourceID      string
	sourceProcess sessionhost.ForegroundProcess
	readyTimeout  time.Duration
}

func beginOrdinaryCodexBinding(ctx context.Context, launcher ManagedCodexLauncher, operation ManagedOperation, source Agent, directory string, policy ManagedCodexPolicy, timeout time.Duration) (*ordinaryCodexBinding, error) {
	if operation != ManagedNew && operation != ManagedFork || validateManagedPolicy(policy) != nil {
		return nil, ErrManagedUnsupported
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	binding := &ordinaryCodexBinding{
		launcher: launcher, operation: operation, policy: policy, source: source,
		readyTimeout: managedReadyTimeout(timeout),
	}
	if operation == ManagedFork {
		if !validManagedSession(source.Session) || source.CWD != directory {
			return nil, ErrManagedStale
		}
		binding.sourceID = source.Session.Value
		safeCtx, cancel := context.WithTimeout(ctx, binding.readyTimeout)
		defer cancel()
		safe, err := launcher.awaitSafeSessionChange(safeCtx, source, CodexServiceSnapshot{})
		if err != nil {
			return nil, err
		}
		binding.sourceProcess = safe.Process
	}
	return binding, nil
}

func (b *ordinaryCodexBinding) Finish(ctx context.Context, target sessionhost.CreatedTarget, directory string, argv []string) (Agent, error) {
	readyCtx, cancel := context.WithTimeout(ctx, b.readyTimeout)
	defer cancel()
	if codexRemoteArgv(argv) {
		return Agent{}, ErrManagedUnsupported
	}
	// Split launch returns after submitting the literal command, before its
	// shell has necessarily exec'd Codex. Observe the exact process before
	// applying identity discovery's stricter no-drift guard.
	if _, err := b.launcher.waitForProcess(readyCtx, target.Origin, directory, argv, b.readyTimeout); err != nil {
		return Agent{}, err
	}
	discover := b.launcher.discover
	if discover == nil {
		discover = b.launcher.discoverManagedIdentity
	}
	agent, threadID, err := discover(readyCtx, target.Origin, directory, argv, CodexServiceSnapshot{}, b.policy)
	if err != nil {
		return Agent{}, err
	}
	if !validManagedThreadID(threadID) || threadID == b.sourceID {
		return Agent{}, ErrManagedStale
	}
	agent, err = b.launcher.bindManagedIdentity(readyCtx, target.Origin, directory, agent, threadID)
	if err != nil {
		return Agent{}, err
	}
	if b.operation == ManagedFork {
		current, findErr := b.launcher.Host.find(readyCtx, b.source.PaneID)
		process, processErr := b.launcher.topologyHost().InspectForegroundProcess(readyCtx, sessionhost.Origin{
			SessionName: b.source.SessionName, WorkspaceID: b.source.WorkspaceID,
			TabID: b.source.TabID, PaneID: b.source.PaneID,
		})
		if findErr != nil || !sameImmutable(b.source, current) || current.CWD != b.source.CWD || processErr != nil || !sameForegroundProcess(b.sourceProcess, process) {
			return Agent{}, ErrManagedStale
		}
	}
	return agent, nil
}
