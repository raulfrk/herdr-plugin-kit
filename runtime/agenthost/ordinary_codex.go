package agenthost

import (
	"context"
	"errors"
	"strings"
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

func (b *ordinaryCodexBinding) BindAfterPrompt(ctx context.Context, target sessionhost.CreatedTarget, directory string, argv []string, prepared ordinaryCodexPreparation) (Agent, error) {
	if b.operation != ManagedNew && b.operation != ManagedFork {
		return Agent{}, ErrManagedUnsupported
	}
	agent, _, err := b.resolvePersistedIdentity(ctx, target, directory, argv, prepared)
	if err == nil {
		err = b.recheckForkSource(ctx)
	}
	return agent, err
}

func (b *ordinaryCodexBinding) resolvePersistedIdentity(ctx context.Context, target sessionhost.CreatedTarget, directory string, argv []string, prepared ordinaryCodexPreparation) (Agent, ManagedCodexPolicy, error) {
	if !validManagedThreadPrefix(prepared.Prefix) && !validManagedStatusThreadPrefix(prepared.Prefix) && !validManagedThreadID(prepared.Prefix) {
		return Agent{}, ManagedCodexPolicy{}, ErrManagedUnsupported
	}
	readyCtx, cancel := context.WithTimeout(ctx, b.readyTimeout)
	defer cancel()
	service, err := b.launcher.serviceClient().Ensure(readyCtx)
	if err != nil {
		return Agent{}, ManagedCodexPolicy{}, err
	}
	verifier, err := b.launcher.verifierFactory().Connect(readyCtx, service.SocketPath)
	if err != nil {
		return Agent{}, ManagedCodexPolicy{}, err
	}
	closeVerifier := func(resultErr error) error { return errors.Join(resultErr, verifier.Close()) }
	var threadID string
	for {
		if _, err := b.launcher.topologyHost().VerifyForegroundProcess(readyCtx, target.Origin, directory, argv); err != nil {
			return Agent{}, ManagedCodexPolicy{}, closeVerifier(err)
		}
		wire, current, err := b.launcher.readManagedAgent(readyCtx, target.Origin, directory)
		if err != nil {
			return Agent{}, ManagedCodexPolicy{}, closeVerifier(err)
		}
		if !validOrdinaryBindingObservation(prepared, wire, current) {
			return Agent{}, ManagedCodexPolicy{}, closeVerifier(ErrManagedStale)
		}
		threadID = prepared.Prefix
		if !validManagedThreadID(threadID) {
			listed, err := verifier.ListThreads(readyCtx, directory)
			if err != nil {
				return Agent{}, ManagedCodexPolicy{}, closeVerifier(err)
			}
			threadID = ""
			matches := 0
			for _, candidate := range listed {
				if !strings.HasPrefix(candidate.ID, prepared.Prefix) {
					continue
				}
				matches++
				if matches > 1 {
					return Agent{}, ManagedCodexPolicy{}, closeVerifier(ErrManagedAmbiguous)
				}
				threadID = candidate.ID
			}
		}
		if threadID != "" {
			candidate, err := verifier.ReadThread(readyCtx, threadID)
			if err != nil {
				return Agent{}, ManagedCodexPolicy{}, closeVerifier(err)
			}
			if !validResolvedOrdinaryIdentity(candidate, threadID, directory, b.sourceID) {
				return Agent{}, ManagedCodexPolicy{}, closeVerifier(ErrManagedStale)
			}
			prepared.Policy, err = resolvedOrdinaryPolicy(prepared.Policy, prepared.policyIsPrefix, candidate)
			if err != nil {
				return Agent{}, ManagedCodexPolicy{}, closeVerifier(err)
			}
			break
		}
		if err := waitManaged(readyCtx, 25*time.Millisecond); err != nil {
			return Agent{}, ManagedCodexPolicy{}, closeVerifier(errors.Join(ErrManagedStale, err))
		}
	}
	if err := verifier.Close(); err != nil {
		return Agent{}, ManagedCodexPolicy{}, err
	}
	if _, err := b.launcher.topologyHost().VerifyForegroundProcess(readyCtx, target.Origin, directory, argv); err != nil {
		return Agent{}, ManagedCodexPolicy{}, err
	}
	wire, current, err := b.launcher.readManagedAgent(readyCtx, target.Origin, directory)
	if err != nil {
		return Agent{}, ManagedCodexPolicy{}, err
	}
	if !validOrdinaryBindingObservation(prepared, wire, current) {
		return Agent{}, ManagedCodexPolicy{}, ErrManagedStale
	}
	agent, err := b.launcher.bindManagedIdentity(readyCtx, target.Origin, directory, current, threadID)
	return agent, prepared.Policy, err
}

func sameOrdinaryPane(expected, current Agent) bool {
	expected.Session = current.Session
	return sameImmutable(expected, current) && expected.CWD == current.CWD
}

func validOrdinaryBindingObservation(prepared ordinaryCodexPreparation, wire wireAgent, current Agent) bool {
	if wire.Focused || wire.LaunchPending || !sameOrdinaryPane(prepared.Agent, current) {
		return false
	}
	return current.Session == nil || validManagedSession(current.Session) && strings.HasPrefix(current.Session.Value, prepared.Prefix)
}

func validResolvedOrdinaryIdentity(candidate codexThreadIdentity, threadID, directory, sourceID string) bool {
	return candidate.ID == threadID && candidate.CWD == directory && candidate.ForkedFromID == sourceID && candidate.ModelProvider == "openai" && candidate.Model != ""
}

func resolvedOrdinaryPolicy(expected ManagedCodexPolicy, expectedIsPrefix bool, candidate codexThreadIdentity) (ManagedCodexPolicy, error) {
	observed := ManagedCodexPolicy{Model: candidate.Model, ReasoningEffort: candidate.ReasoningEffort}
	if expected == (ManagedCodexPolicy{}) {
		if observed.ReasoningEffort == "" {
			return ManagedCodexPolicy{}, ErrManagedPolicyDrift
		}
		return observed, nil
	}
	if observed.Model != expected.Model {
		return ManagedCodexPolicy{}, ErrManagedPolicyDrift
	}
	if observed.ReasoningEffort == "" {
		return expected, nil
	}
	if expectedIsPrefix {
		if !strings.HasPrefix(observed.ReasoningEffort, expected.ReasoningEffort) {
			return ManagedCodexPolicy{}, ErrManagedPolicyDrift
		}
		return observed, nil
	}
	if observed.ReasoningEffort != expected.ReasoningEffort {
		return ManagedCodexPolicy{}, ErrManagedPolicyDrift
	}
	return expected, nil
}

type ordinaryCodexPreparation struct {
	Agent          Agent
	Prefix         string
	Policy         ManagedCodexPolicy
	policyIsPrefix bool
}

func beginOrdinaryCodexBinding(ctx context.Context, launcher ManagedCodexLauncher, operation ManagedOperation, source Agent, directory string, policy ManagedCodexPolicy, observePolicy bool, timeout time.Duration) (*ordinaryCodexBinding, error) {
	if operation != ManagedNew && operation != ManagedFork || observePolicy && policy != (ManagedCodexPolicy{}) || !observePolicy && validateManagedPolicy(policy) != nil {
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
	prepared, err := b.Prepare(ctx, target, directory, argv)
	if err != nil {
		return Agent{}, err
	}
	if prepared.Prefix != "" {
		if b.operation != ManagedFork {
			return Agent{}, ErrManagedNarrow
		}
		agent, _, err := b.resolvePersistedIdentity(ctx, target, directory, argv, prepared)
		if err == nil {
			err = b.recheckForkSource(ctx)
		}
		return agent, err
	}
	return prepared.Agent, nil
}

func (b *ordinaryCodexBinding) Prepare(ctx context.Context, target sessionhost.CreatedTarget, directory string, argv []string) (ordinaryCodexPreparation, error) {
	readyCtx, cancel := context.WithTimeout(ctx, b.readyTimeout)
	defer cancel()
	if codexRemoteArgv(argv) {
		return ordinaryCodexPreparation{}, ErrManagedUnsupported
	}
	service, err := b.launcher.serviceClient().Ensure(readyCtx)
	if err != nil {
		return ordinaryCodexPreparation{}, err
	}
	// Split launch returns after submitting the literal command, before its
	// shell has necessarily exec'd Codex. Observe the exact process before
	// applying identity discovery's stricter no-drift guard.
	if _, err := b.launcher.waitForProcess(readyCtx, target.Origin, directory, argv, b.readyTimeout); err != nil {
		return ordinaryCodexPreparation{}, err
	}
	var (
		agent    Agent
		observed managedStatusObservation
	)
	if b.launcher.discoverObserved != nil {
		agent, observed, err = b.launcher.discoverObserved(readyCtx, target.Origin, directory, argv, service, b.policy)
	} else if b.launcher.discover != nil {
		var identity string
		agent, identity, err = b.launcher.discover(readyCtx, target.Origin, directory, argv, service, b.policy)
		observed = managedStatusObservation{identity: identity, policy: b.policy}
	} else {
		agent, observed, err = b.launcher.discoverOrdinaryManagedIdentityObservation(readyCtx, target.Origin, directory, argv, service, b.policy, true)
	}
	if err != nil {
		return ordinaryCodexPreparation{}, err
	}
	if observed.fromStatus && !validOrdinaryStatusParent(b.operation, b.sourceID, observed.forkedFromPrefix) {
		return ordinaryCodexPreparation{}, ErrManagedStale
	}
	if (validManagedThreadPrefix(observed.identity) && observed.policy.Model != "" && observed.policy.ReasoningEffort != "") || validManagedStatusThreadPrefix(observed.identity) {
		prepared := ordinaryCodexPreparation{Agent: agent, Prefix: observed.identity, Policy: observed.policy, policyIsPrefix: observed.policyIsPrefix}
		if err := b.recheckForkSource(readyCtx); err != nil {
			return ordinaryCodexPreparation{}, err
		}
		return prepared, nil
	}
	if !validManagedThreadID(observed.identity) || observed.identity == b.sourceID {
		return ordinaryCodexPreparation{}, ErrManagedStale
	}
	if b.operation == ManagedFork {
		prepared := ordinaryCodexPreparation{Agent: agent, Prefix: observed.identity, Policy: observed.policy}
		agent, observed.policy, err = b.resolvePersistedIdentity(readyCtx, target, directory, argv, prepared)
		if err != nil {
			return ordinaryCodexPreparation{}, err
		}
		if err := b.recheckForkSource(readyCtx); err != nil {
			return ordinaryCodexPreparation{}, err
		}
		return ordinaryCodexPreparation{Agent: agent, Policy: observed.policy}, nil
	}
	agent, err = b.launcher.bindManagedIdentity(readyCtx, target.Origin, directory, agent, observed.identity)
	if err != nil {
		return ordinaryCodexPreparation{}, err
	}
	if err := b.recheckForkSource(readyCtx); err != nil {
		return ordinaryCodexPreparation{}, err
	}
	return ordinaryCodexPreparation{Agent: agent, Policy: observed.policy}, nil
}

func validOrdinaryStatusParent(operation ManagedOperation, sourceID, parentPrefix string) bool {
	if operation == ManagedNew {
		return parentPrefix == ""
	}
	return operation == ManagedFork && parentPrefix != "" && strings.HasPrefix(sourceID, parentPrefix)
}

func (b *ordinaryCodexBinding) recheckForkSource(ctx context.Context) error {
	if b.operation != ManagedFork {
		return nil
	}
	current, findErr := b.launcher.Host.find(ctx, b.source.PaneID)
	process, processErr := b.launcher.topologyHost().InspectForegroundProcess(ctx, sessionhost.Origin{
		SessionName: b.source.SessionName, WorkspaceID: b.source.WorkspaceID,
		TabID: b.source.TabID, PaneID: b.source.PaneID,
	})
	if findErr != nil || !sameImmutable(b.source, current) || current.CWD != b.source.CWD || processErr != nil || !sameForegroundProcess(b.sourceProcess, process) {
		return ErrManagedStale
	}
	return nil
}
