package agenthost

import (
	"context"
	"errors"
	"strings"
)

// ErrUnsafeSessionChange means the source conversation cannot safely lose
// focus because its lifecycle, composer, or identity is not provably stable.
var ErrUnsafeSessionChange = errors.New("agent source is not safe to leave")

// VerifyNativeSessionChange proves that an unfocused OMP or OpenCode source is
// unchanged, authoritatively idle, and has an empty supported-version
// composer. It performs no input or focus side effect.
func (p Probe) VerifyNativeSessionChange(ctx context.Context, target Agent) (Agent, error) {
	if ctx == nil {
		panic("nil context")
	}
	before, err := p.Host.find(ctx, target.PaneID)
	if err != nil || !sameImmutable(target, before) || target.CWD != before.CWD {
		return Agent{}, errors.Join(ErrUnsafeSessionChange, ErrStaleReport, err)
	}
	if before.Focused || before.Status != "idle" && before.Status != "done" {
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
	if err != nil || !sameImmutable(before, after) || before.CWD != after.CWD || before.Status != after.Status || before.Revision != after.Revision || before.StateChangeSeq != after.StateChangeSeq || after.Focused {
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
