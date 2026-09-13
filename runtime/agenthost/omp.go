package agenthost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"time"
)

const ompResponseBytes = 64 << 10

type ompObservation struct {
	identity nativeIdentity
	status   Status
	reason   Reason
	session  string
	stateSeq uint64
}

func (p Probe) assessOMP(parent context.Context, target Agent) (Assessment, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	managed, err := validateNativeTarget(p.Host, target, "omp", "herdr:omp", "path")
	if err != nil || p.wait == nil || ctx.Err() != nil {
		return nativeAssessmentFailure(ctx, "OMP", err)
	}
	socket, configured := p.native.OMPSockets[target.PaneID]
	if !managed || !configured || target.Session == nil || !filepath.IsAbs(target.Session.Value) {
		return nativeUnknown(NativeNotManaged), nil
	}
	first, err := p.observeOMP(ctx, target, socket)
	if err != nil {
		return nativeAssessmentFailure(ctx, "OMP", err)
	}
	if err := p.wait(ctx, DefaultSettle()); err != nil {
		return nativeAssessmentFailure(ctx, "OMP", err)
	}
	second, err := p.observeOMP(ctx, target, socket)
	if err != nil {
		return nativeAssessmentFailure(ctx, "OMP", err)
	}
	if err := ctx.Err(); err != nil {
		return nativeAssessmentFailure(ctx, "OMP", err)
	}
	if !sameNativeIdentity(first.identity, second.identity) || first.status != second.status || first.reason != second.reason || first.session != second.session || first.stateSeq != second.stateSeq {
		return nativeFailure(ErrStaleReport)
	}
	if second.status == Unknown {
		return nativeUnknown(second.reason), nil
	}
	return Assessment{Status: second.status, Reason: second.reason, Stable: true}, nil
}

func (p Probe) observeOMP(ctx context.Context, target Agent, socket string) (ompObservation, error) {
	expected := nativeIdentityFromAgent(target)
	before, err := p.Host.findNativeIdentity(ctx, target.PaneID)
	if err != nil {
		return ompObservation{}, err
	}
	if !sameNativeIdentity(expected, before) {
		return ompObservation{}, ErrStaleReport
	}
	payload, err := readOMPStatus(ctx, socket)
	if err != nil {
		return ompObservation{}, errors.New("assess OMP: native status unavailable")
	}
	if payload.SessionValue != target.Session.Value {
		return ompObservation{}, ErrStaleReport
	}
	after, err := p.Host.findNativeIdentity(ctx, target.PaneID)
	if err != nil {
		return ompObservation{}, err
	}
	if err := ctx.Err(); err != nil {
		return ompObservation{}, err
	}
	if !sameNativeIdentity(before, after) {
		return ompObservation{}, ErrStaleReport
	}
	status, reason := Unknown, NativeUnsupported
	switch payload.Status {
	case "working":
		status, reason = Working, OMPStatus
	case "blocked":
		status, reason = Blocked, OMPStatus
	case "idle":
		status, reason = Idle, OMPStatus
	}
	return ompObservation{identity: after, status: status, reason: reason, session: payload.SessionValue, stateSeq: payload.StateSeq}, nil
}

type ompStatusPayload struct {
	Version      int    `json:"version"`
	Harness      string `json:"harness"`
	SessionKind  string `json:"session_kind"`
	SessionValue string `json:"session_value"`
	Status       string `json:"status"`
	StateSeq     uint64 `json:"state_seq"`
}

func readOMPStatus(ctx context.Context, socket string) (ompStatusPayload, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return ompStatusPayload{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return ompStatusPayload{}, err
		}
	}
	stopCancel := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
	})
	defer stopCancel()
	limited := &io.LimitedReader{R: bufio.NewReader(conn), N: ompResponseBytes + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var payload ompStatusPayload
	if err := decoder.Decode(&payload); err != nil {
		return ompStatusPayload{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF || limited.N <= 0 {
		return ompStatusPayload{}, errors.New("invalid OMP status protocol")
	}
	if payload.Version != 1 || payload.Harness != "omp" || payload.SessionKind != "path" ||
		!filepath.IsAbs(payload.SessionValue) || payload.SessionValue == "" ||
		strings.ContainsRune(payload.SessionValue, 0) || payload.Status == "" || payload.StateSeq == 0 {
		return ompStatusPayload{}, errors.New("invalid OMP status protocol")
	}
	return payload, nil
}
