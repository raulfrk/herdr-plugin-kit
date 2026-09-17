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

var (
	ErrUnsupportedNativeNew    = errors.New("unsupported native New")
	ErrNativeNewRequiresPrompt = errors.New("ordinary Codex New requires a prompt or managed launch")
)

// NativeStartRequest starts one background native OMP or OpenCode
// conversation. Ordinary Codex cannot expose an authoritative zero-turn
// identity and must use PromptStarter or ManagedCodexLauncher instead.
type NativeStartRequest struct {
	Origin          sessionhost.Origin
	Argv            []string
	Directory       string
	Placement       sessionhost.Placement
	Direction       string
	TabLabel        string
	ReadyTimeout    time.Duration
	OMPStatusSocket string
	OpenCodeServer  OpenCodeServer
	// StartOpenCodeServer creates one short-lived loopback OpenCode 1.18.30
	// server to allocate an exact fresh session before launching the TUI.
	StartOpenCodeServer bool
}

type NativeStartResult struct {
	Created sessionhost.CreatedTarget
	Agent   Agent
}

type NativeStarter struct {
	Host             Host
	launcher         topologyLauncher
	wait             func(context.Context, time.Duration) error
	openCodeServer   openCodePromptServerLauncher
	openCodeSessions openCodePromptSessions
}

func NewNativeStarter(host Host) NativeStarter {
	return NativeStarter{Host: host, wait: wait}
}

// Start launches literal argv in the exact Herdr session and returns only
// after two stable reads prove a new immutable identity absent before launch.
func (s NativeStarter) Start(ctx context.Context, request NativeStartRequest) (result NativeStartResult, returnErr error) {
	if ctx == nil {
		panic("nil context")
	}
	if err := ctx.Err(); err != nil {
		return NativeStartResult{}, err
	}
	argv, kind, err := validateNativeStart(request)
	if err != nil {
		return NativeStartResult{}, err
	}
	if s.wait == nil {
		return NativeStartResult{}, errors.New("native starter must be created with NewNativeStarter")
	}
	before, err := s.Host.List(ctx)
	if err != nil {
		return NativeStartResult{}, fmt.Errorf("native New baseline: %w", err)
	}
	prior := make(map[SessionRef]struct{}, len(before))
	for _, candidate := range before {
		if candidate.Session != nil {
			prior[*candidate.Session] = struct{}{}
		}
	}
	var openCode preparedOpenCodeStart
	if kind == "opencode" {
		openCode, err = prepareOpenCodeStart(ctx, s.Host, request.Argv[0], request.Directory, request.OpenCodeServer, request.StartOpenCodeServer, s.openCodeServer, s.openCodeSessions)
		if err != nil {
			return NativeStartResult{}, fmt.Errorf("native New OpenCode session: %w", err)
		}
		argv = append(argv, "--session", openCode.session)
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openCodePromptServerStop)
			defer cancel()
			returnErr = errors.Join(returnErr, openCode.stopServer(stopCtx))
		}()
	}
	created, err := s.topology().Launch(ctx, sessionhost.LaunchRequest{
		Origin: request.Origin, Argv: argv, Directory: request.Directory,
		Placement: request.Placement, Direction: request.Direction, TabLabel: request.TabLabel,
		Focus: false, OMPStatusSocket: request.OMPStatusSocket,
	})
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promptCleanupTimeout)
		defer cancel()
		return NativeStartResult{}, errors.Join(fmt.Errorf("native New launch: %w", err), openCode.deleteSession(cleanupCtx))
	}
	result = NativeStartResult{Created: created}
	fail := func(cause error) (NativeStartResult, error) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promptCleanupTimeout)
		defer cancel()
		cleanupErr := s.topology().Cleanup(cleanupCtx, created)
		sessionErr := openCode.deleteSession(cleanupCtx)
		if cleanupErr != nil {
			return result, errors.Join(cause, cleanupErr, sessionErr)
		}
		return NativeStartResult{}, errors.Join(cause, sessionErr)
	}
	agent, err := s.waitForFresh(ctx, created, kind, openCode.session, request.Directory, prior, readyTimeout(request.ReadyTimeout))
	if err != nil {
		return fail(fmt.Errorf("native New identity: %w", err))
	}
	result.Agent = agent
	return result, nil
}

func (s NativeStarter) waitForFresh(parent context.Context, created sessionhost.CreatedTarget, kind, expectedSession, directory string, prior map[SessionRef]struct{}, timeout time.Duration) (Agent, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	for {
		first, err := s.Host.find(ctx, created.PaneID)
		if err == nil {
			ready, pending := classifyNativeNew(first, created, kind, expectedSession, directory, prior)
			if !ready && !pending {
				return Agent{}, ErrStaleReport
			}
			if ready {
				if err := s.wait(ctx, 50*time.Millisecond); err != nil {
					return Agent{}, err
				}
				second, secondErr := s.Host.find(ctx, created.PaneID)
				if secondErr != nil {
					return Agent{}, ErrStaleReport
				}
				secondReady, _ := classifyNativeNew(second, created, kind, expectedSession, directory, prior)
				if !secondReady || !sameImmutable(first, second) {
					return Agent{}, ErrStaleReport
				}
				return second, nil
			}
		} else if !errors.Is(err, ErrStaleReport) {
			return Agent{}, err
		}
		if err := s.wait(ctx, 50*time.Millisecond); err != nil {
			return Agent{}, err
		}
	}
}

func (s NativeStarter) topology() topologyLauncher {
	if s.launcher != nil {
		return s.launcher
	}
	return sessionhost.Host{Runner: s.Host.Runner, Herdr: s.Host.Herdr, Session: s.Host.Session}
}

func validateNativeStart(request NativeStartRequest) ([]string, string, error) {
	if len(request.Argv) == 0 {
		return nil, "", ErrUnsupportedNativeNew
	}
	kind := strings.ToLower(filepath.Base(request.Argv[0]))
	if err := validateForkExecutable(request.Argv[0], kind); err != nil {
		return nil, "", err
	}
	if kind == "codex" {
		return nil, "", ErrNativeNewRequiresPrompt
	}
	if kind != "omp" && kind != "opencode" {
		return nil, "", ErrUnsupportedNativeNew
	}
	options, err := validateForkOptions(kind, request.Argv[1:])
	if err != nil {
		return nil, "", err
	}
	if request.Directory == "" || !filepath.IsAbs(request.Directory) || filepath.Clean(request.Directory) != request.Directory || strings.ContainsRune(request.Directory, 0) {
		return nil, "", ErrUnsupportedNativeNew
	}
	if request.ReadyTimeout < 0 || request.ReadyTimeout > MaxForkReadyTimeout {
		return nil, "", ErrUnsupportedNativeNew
	}
	if kind == "omp" {
		if request.StartOpenCodeServer || request.OpenCodeServer != (OpenCodeServer{}) {
			return nil, "", ErrUnsupportedNativeNew
		}
		if request.OMPStatusSocket != "" && (!filepath.IsAbs(request.OMPStatusSocket) || filepath.Clean(request.OMPStatusSocket) != request.OMPStatusSocket || strings.ContainsRune(request.OMPStatusSocket, 0)) {
			return nil, "", ErrUnsupportedNativeNew
		}
	} else {
		if request.OMPStatusSocket != "" || request.StartOpenCodeServer && request.OpenCodeServer != (OpenCodeServer{}) {
			return nil, "", ErrUnsupportedNativeNew
		}
		if !request.StartOpenCodeServer {
			if err := validateOpenCodeServer(request.OpenCodeServer); err != nil {
				return nil, "", err
			}
		}
		if request.OpenCodeServer != (OpenCodeServer{}) && request.OpenCodeServer.Directory != request.Directory {
			return nil, "", ErrUnsupportedNativeNew
		}
	}
	return append([]string{request.Argv[0]}, options...), kind, nil
}

func classifyNativeNew(candidate Agent, created sessionhost.CreatedTarget, kind, expectedSession, directory string, prior map[SessionRef]struct{}) (ready, pending bool) {
	if !strings.EqualFold(candidate.Kind, kind) || candidate.SessionName != created.SessionName || candidate.WorkspaceID != created.WorkspaceID || candidate.TabID != created.TabID || candidate.PaneID != created.PaneID || candidate.CWD != directory || candidate.Focused {
		return false, false
	}
	if candidate.Session == nil || !promptReadyStatus(candidate.Status) {
		return false, true
	}
	session := *candidate.Session
	wantKind := "id"
	if kind == "omp" {
		wantKind = "path"
		if !filepath.IsAbs(session.Value) || filepath.Clean(session.Value) != session.Value {
			return false, false
		}
	}
	if session.Source != "herdr:"+kind || session.Agent != kind || session.Kind != wantKind || session.Value == "" {
		return false, false
	}
	if expectedSession != "" && session.Value != expectedSession {
		return false, false
	}
	if _, existed := prior[session]; existed {
		return false, false
	}
	return true, false
}
