package agenthost

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

// ErrUnsafeSessionChange means the source conversation cannot safely lose
// focus because its lifecycle, composer, or identity is not provably stable.
var ErrUnsafeSessionChange = errors.New("agent source is not safe to leave")

// VerifyNativeSessionChange proves that an unfocused OMP or OpenCode source is
// unchanged, authoritatively idle, and has an empty supported-version
// composer. It performs no input or focus side effect.
func (p Probe) VerifyNativeSessionChange(ctx context.Context, target Agent) (Agent, error) {
	return p.verifyNativeSessionChange(ctx, target, false)
}

// VerifyNativeOverlaySessionChange applies the native OMP/OpenCode guard while
// an active Herdr modal owns input over the exact source pane. It requires the
// pane to remain focused across the stable identity reads. The caller must
// independently prove and retain ownership of that modal.
func (p Probe) VerifyNativeOverlaySessionChange(ctx context.Context, target Agent) (Agent, error) {
	return p.verifyNativeSessionChange(ctx, target, true)
}

// VerifyFocusedWorkingAutofocus is a read-only, best-effort guard for an
// automatic focus handoff. Unlike the session-change guards, it permits the
// exact focused working agent to continue running in the background. Callers
// must separately recheck the foreground and intended target before focusing.
func (p Probe) VerifyFocusedWorkingAutofocus(ctx context.Context, target Agent) (Agent, error) {
	if ctx == nil {
		panic("nil context")
	}
	if err := ctx.Err(); err != nil {
		return Agent{}, errors.Join(ErrUnsafeSessionChange, err)
	}
	if err := validateTarget(target); err != nil || !focusedWorking(target) || !filepath.IsAbs(target.CWD) {
		return Agent{}, errors.Join(ErrUnsafeSessionChange, err)
	}
	before, err := p.Host.find(ctx, target.PaneID)
	if err != nil || !sameWorkingSource(target, before) {
		return Agent{}, errors.Join(ErrUnsafeSessionChange, ErrStaleReport, err)
	}
	origin := sessionhost.Origin{SessionName: target.SessionName, WorkspaceID: target.WorkspaceID, TabID: target.TabID, PaneID: target.PaneID}
	topology := p.topology
	if topology == nil {
		topology = sessionhost.Host{Runner: p.Host.Runner, Herdr: p.Host.Herdr, Session: p.Host.Session}
	}
	process, err := topology.InspectForegroundProcess(ctx, origin)
	if err != nil || process.PID <= 0 || process.CWD != target.CWD || process.Name == "" || len(process.Argv) == 0 {
		return Agent{}, errors.Join(ErrUnsafeSessionChange, ErrStaleReport, err)
	}
	assessment, err := p.Assess(ctx, before)
	if err != nil || !assessment.Stable || assessment.Status != Working {
		return Agent{}, errors.Join(ErrUnsafeSessionChange, err)
	}
	if err := p.verifyAutofocusComposerEmpty(ctx, before); err != nil {
		return Agent{}, errors.Join(ErrUnsafeSessionChange, err)
	}
	second, err := p.Host.find(ctx, target.PaneID)
	if err != nil || !sameWorkingSource(before, second) {
		return Agent{}, errors.Join(ErrUnsafeSessionChange, ErrStaleReport, err)
	}
	if err := p.verifyAutofocusComposerEmpty(ctx, second); err != nil {
		return Agent{}, errors.Join(ErrUnsafeSessionChange, err)
	}
	afterProcess, err := topology.InspectForegroundProcess(ctx, origin)
	if err != nil || !sameForegroundProcess(process, afterProcess) {
		return Agent{}, errors.Join(ErrUnsafeSessionChange, ErrStaleReport, err)
	}
	after, err := p.Host.find(ctx, target.PaneID)
	if err != nil || !sameWorkingSource(second, after) || ctx.Err() != nil {
		return Agent{}, errors.Join(ErrUnsafeSessionChange, ErrStaleReport, err, ctx.Err())
	}
	return after, nil
}

func focusedWorking(agent Agent) bool {
	return agent.Focused && !agent.LaunchPending && agent.Status == "working"
}

func sameWorkingSource(before, after Agent) bool {
	return sameImmutable(before, after) && before.CWD == after.CWD && before.StateChangeSeq == after.StateChangeSeq && focusedWorking(after)
}

func (p Probe) verifyAutofocusComposerEmpty(ctx context.Context, target Agent) error {
	if strings.EqualFold(target.Kind, "codex") {
		if target.Session == nil || !validManagedSession(target.Session) || p.Host.CodexSocket == "" {
			return ErrUnsafeSessionChange
		}
		visible, err := (ManagedCodexLauncher{Host: p.Host}).readPaneANSI(ctx, target.PaneID, "visible", managedStatusLines)
		if err != nil {
			return err
		}
		if !emptyCodexComposer(visible) {
			return ErrUnsafeSessionChange
		}
		return nil
	}
	return p.verifyNativeComposerEmpty(ctx, target)
}

func (p Probe) verifyNativeSessionChange(ctx context.Context, target Agent, focused bool) (Agent, error) {
	if ctx == nil {
		panic("nil context")
	}
	before, err := p.Host.find(ctx, target.PaneID)
	if err != nil || !sameImmutable(target, before) || target.CWD != before.CWD {
		return Agent{}, errors.Join(ErrUnsafeSessionChange, ErrStaleReport, err)
	}
	if before.Focused != focused || before.Status != "idle" && before.Status != "done" {
		return Agent{}, ErrUnsafeSessionChange
	}
	assessment, err := p.Assess(ctx, before)
	if err != nil || !assessment.Stable || assessment.Status != Idle && assessment.Status != Done {
		return Agent{}, errors.Join(ErrUnsafeSessionChange, err)
	}
	if err := p.verifyNativeComposerEmpty(ctx, before); err != nil {
		return Agent{}, errors.Join(ErrUnsafeSessionChange, err)
	}
	after, err := p.Host.find(ctx, target.PaneID)
	if err != nil || !sameImmutable(before, after) || before.CWD != after.CWD || before.Status != after.Status || before.Revision != after.Revision || before.StateChangeSeq != after.StateChangeSeq || after.Focused != focused {
		return Agent{}, errors.Join(ErrUnsafeSessionChange, ErrStaleReport, err)
	}
	return after, nil
}

func (p Probe) verifyNativeComposerEmpty(ctx context.Context, target Agent) error {
	reader := ManagedCodexLauncher{Host: p.Host}
	switch strings.ToLower(target.Kind) {
	case "omp":
		if _, ok := p.native.OMPSockets[target.PaneID]; !ok {
			return ErrStaleReport
		}
		visible, err := reader.readPaneText(ctx, target.PaneID, "visible", managedStatusLines)
		if err != nil {
			return err
		}
		if !emptyOMPComposer(visible) {
			return ErrUnsafeSessionChange
		}
		return nil
	case "opencode":
		if _, ok := p.native.OpenCodeServers[target.PaneID]; !ok {
			return ErrStaleReport
		}
		visible, err := reader.readPaneANSI(ctx, target.PaneID, "visible", managedStatusLines)
		if err != nil {
			return err
		}
		if !emptyOpenCodeComposer(visible) {
			return ErrUnsafeSessionChange
		}
		return nil
	default:
		return ErrStaleReport
	}
}
