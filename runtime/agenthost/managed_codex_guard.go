package agenthost

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

// ManagedCodexPaneSnapshot is a stable, quiescent pane observation suitable
// for an immediately following session-change transaction. It does not lock
// the pane; callers that need to interrupt it should use StopForSessionChange.
type ManagedCodexPaneSnapshot struct {
	Agent   Agent
	Process sessionhost.ForegroundProcess
	Service CodexServiceSnapshot
}

// VerifySafeSessionChange performs one read-only attempt to prove that the
// exact unfocused Codex source is idle/done with a stable process and empty
// composer. It never starts the shared service, sends input, stops a process,
// or waits for an unsafe pane to become safe. Successful checks use paired reads.
func (l ManagedCodexLauncher) VerifySafeSessionChange(ctx context.Context, source Agent) (ManagedCodexPaneSnapshot, error) {
	if ctx == nil {
		panic("nil context")
	}
	if err := ctx.Err(); err != nil {
		return ManagedCodexPaneSnapshot{}, err
	}
	if err := l.validateSafeSource(source, 0); err != nil {
		return ManagedCodexPaneSnapshot{}, err
	}
	inspector, ok := l.serviceClient().(interface {
		Inspect(context.Context) (CodexServiceSnapshot, error)
	})
	if !ok {
		return ManagedCodexPaneSnapshot{}, ErrCodexServiceUnavailable
	}
	service, err := inspector.Inspect(ctx)
	if err != nil {
		return ManagedCodexPaneSnapshot{}, err
	}
	if !validManagedServiceSnapshot(service) {
		return ManagedCodexPaneSnapshot{}, ErrManagedStale
	}
	snapshot, _, err := l.sampleSafeSessionChange(ctx, source, service, false, false)
	if err == nil && ctx.Err() != nil {
		return ManagedCodexPaneSnapshot{}, ctx.Err()
	}
	return snapshot, err
}

// AwaitSafeSessionChange waits without sending terminal input until the exact
// managed Codex pane is unfocused, quiescent, and has an empty composer on two
// consecutive stable reads.
func (l ManagedCodexLauncher) AwaitSafeSessionChange(ctx context.Context, source Agent, readyTimeout time.Duration) (ManagedCodexPaneSnapshot, error) {
	return l.awaitSafeSessionChangeWithFocus(ctx, source, readyTimeout, false, false)
}

// AwaitSafeOverlaySessionChange applies the managed Codex session-change
// guard while an active Herdr modal owns input over the exact source pane.
// Unlike AwaitSafeSessionChange, it requires the pane to remain focused on
// both stable reads. The caller must independently prove and retain ownership
// of that modal; this method performs no input or focus side effect.
func (l ManagedCodexLauncher) AwaitSafeOverlaySessionChange(ctx context.Context, source Agent, readyTimeout time.Duration) (ManagedCodexPaneSnapshot, error) {
	return l.awaitSafeSessionChangeWithFocus(ctx, source, readyTimeout, true, false)
}

func (l ManagedCodexLauncher) awaitSafeSessionChangeWithFocus(ctx context.Context, source Agent, readyTimeout time.Duration, focused, refuseComposer bool) (ManagedCodexPaneSnapshot, error) {
	if ctx == nil {
		panic("nil context")
	}
	if err := l.validateSafeSource(source, readyTimeout); err != nil {
		return ManagedCodexPaneSnapshot{}, fmt.Errorf("managed Codex session change: %w", err)
	}
	service, err := l.serviceClient().Ensure(ctx)
	if err != nil {
		return ManagedCodexPaneSnapshot{}, fmt.Errorf("managed Codex session change service: %w", err)
	}
	if !validManagedServiceSnapshot(service) {
		return ManagedCodexPaneSnapshot{}, fmt.Errorf("managed Codex session change service: %w", ErrManagedStale)
	}
	waitCtx, cancel := context.WithTimeout(ctx, managedReadyTimeout(readyTimeout))
	defer cancel()
	return l.awaitSafeSessionChangeFocus(waitCtx, source, service, focused, refuseComposer)
}

// StopForSessionChange performs the same non-disruptive guard and then stops
// only the exact observed foreground process, returning after the same pane's
// authoritative shell is ready. No replacement conversation is launched.
func (l ManagedCodexLauncher) StopForSessionChange(ctx context.Context, source Agent, readyTimeout time.Duration) (ManagedCodexPaneSnapshot, error) {
	snapshot, err := l.awaitSafeSessionChangeWithFocus(ctx, source, readyTimeout, false, true)
	if err != nil {
		return ManagedCodexPaneSnapshot{}, err
	}
	if err := l.topologyHost().StopForegroundProcess(ctx, sessionhost.Origin{
		SessionName: source.SessionName, WorkspaceID: source.WorkspaceID, TabID: source.TabID, PaneID: source.PaneID,
	}, source.CWD, snapshot.Process.Argv, snapshot.Process.PID); err != nil {
		return ManagedCodexPaneSnapshot{}, fmt.Errorf("managed Codex session change stop: %w", err)
	}
	return snapshot, nil
}

func (l ManagedCodexLauncher) awaitSafeSessionChange(ctx context.Context, source Agent, service CodexServiceSnapshot) (ManagedCodexPaneSnapshot, error) {
	return l.awaitSafeSessionChangeFocus(ctx, source, service, false, false)
}

func (l ManagedCodexLauncher) awaitSafeSessionChangeFocus(ctx context.Context, source Agent, service CodexServiceSnapshot, focused, refuseComposer bool) (ManagedCodexPaneSnapshot, error) {
	for {
		snapshot, retry, err := l.sampleSafeSessionChange(ctx, source, service, focused, refuseComposer)
		if !retry {
			return snapshot, err
		}
		if err := waitManaged(ctx, 25*time.Millisecond); err != nil {
			return ManagedCodexPaneSnapshot{}, errors.Join(ErrManagedPaneBusy, err)
		}
	}
}

func (l ManagedCodexLauncher) sampleSafeSessionChange(ctx context.Context, source Agent, service CodexServiceSnapshot, focused, refuseComposer bool) (ManagedCodexPaneSnapshot, bool, error) {
	target := sessionhost.Origin{SessionName: source.SessionName, WorkspaceID: source.WorkspaceID, TabID: source.TabID, PaneID: source.PaneID}
	wire, current, err := l.readManagedAgent(ctx, target, source.CWD)
	if err != nil || !sameImmutable(source, current) {
		if err == nil {
			err = ErrManagedStale
		}
		return ManagedCodexPaneSnapshot{}, false, err
	}
	if !managedPaneReadyForSessionChange(wire, focused) {
		return ManagedCodexPaneSnapshot{}, true, ErrManagedPaneBusy
	}
	process, err := l.topologyHost().InspectForegroundProcess(ctx, target)
	if err != nil || process.Name != "codex" || len(process.Argv) == 0 || filepath.Base(process.Argv[0]) != "codex" || process.CWD != source.CWD {
		if err == nil {
			err = ErrManagedStale
		}
		return ManagedCodexPaneSnapshot{}, false, err
	}
	visible, err := l.readPaneANSI(ctx, target.PaneID, "visible", managedStatusLines)
	if err != nil {
		return ManagedCodexPaneSnapshot{}, false, err
	}
	if !emptyCodexComposer(visible) {
		if refuseComposer {
			return ManagedCodexPaneSnapshot{}, false, errors.Join(ErrCodexConversionComposer, ErrManagedPaneBusy)
		}
		return ManagedCodexPaneSnapshot{}, true, ErrManagedPaneBusy
	}
	secondWire, second, err := l.readManagedAgent(ctx, target, source.CWD)
	if err != nil {
		return ManagedCodexPaneSnapshot{}, false, err
	}
	if !sameImmutable(current, second) || current.Status != second.Status || !managedPaneReadyForSessionChange(secondWire, focused) {
		return ManagedCodexPaneSnapshot{}, false, ErrManagedStale
	}
	secondProcess, err := l.topologyHost().InspectForegroundProcess(ctx, target)
	if err != nil || !sameForegroundProcess(process, secondProcess) {
		if err == nil {
			err = ErrManagedStale
		}
		return ManagedCodexPaneSnapshot{}, false, err
	}
	visible, err = l.readPaneANSI(ctx, target.PaneID, "visible", managedStatusLines)
	if err != nil {
		return ManagedCodexPaneSnapshot{}, false, err
	}
	if !emptyCodexComposer(visible) {
		if refuseComposer {
			return ManagedCodexPaneSnapshot{}, false, errors.Join(ErrCodexConversionComposer, ErrManagedPaneBusy)
		}
		return ManagedCodexPaneSnapshot{}, false, ErrManagedPaneBusy
	}
	return ManagedCodexPaneSnapshot{Agent: second, Process: secondProcess, Service: service}, false, nil
}

func (l ManagedCodexLauncher) validateSafeSource(source Agent, readyTimeout time.Duration) error {
	if l.Host.Runner == nil || source.SessionName != l.Host.Session || validateTarget(source) != nil || !strings.EqualFold(source.Kind, "codex") || !validManagedSession(source.Session) {
		return ErrManagedUnsupported
	}
	if len(source.CWD) > maxPathBytes || !filepath.IsAbs(source.CWD) || filepath.Clean(source.CWD) != source.CWD || strings.ContainsRune(source.CWD, 0) {
		return ErrManagedUnsupported
	}
	if readyTimeout < 0 || readyTimeout > MaxManagedReadyTimeout || readyTimeout > 0 && readyTimeout <= 3*time.Second {
		return ErrManagedUnsupported
	}
	return nil
}

func managedPaneQuiescent(agent wireAgent) bool {
	return managedPaneReadyForSessionChange(agent, false)
}

func managedPaneReadyForSessionChange(agent wireAgent, focused bool) bool {
	return agent.Focused == focused && !agent.LaunchPending && (agent.AgentStatus == "idle" || agent.AgentStatus == "done")
}

func validManagedServiceSnapshot(service CodexServiceSnapshot) bool {
	return service.State == CodexServiceReady && cleanAbsolute(service.Executable) && cleanAbsolute(service.SocketPath) && service.CLIVersion == SupportedCodexVersion && service.AppServerVersion == SupportedCodexVersion
}
