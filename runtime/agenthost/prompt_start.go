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

type PromptStartRequest struct {
	Origin          sessionhost.Origin
	Argv            []string
	Directory       string
	Placement       sessionhost.Placement
	Direction       string
	TabLabel        string
	Focus           bool
	ReadyTimeout    time.Duration
	OMPStatusSocket string
	OpenCodeServer  OpenCodeServer
	// StartOpenCodeServer creates one short-lived loopback OpenCode 1.18.30
	// server for session creation and prompt verification, then stops it while
	// preserving the launched TUI conversation.
	StartOpenCodeServer bool
	CodexPolicy         ManagedCodexPolicy
	// ObserveCodexPolicy allows an ordinary Codex launch to discover its
	// model, reasoning effort, and missing conversation ID from one guarded
	// /status response instead of requiring the consumer to predict them.
	ObserveCodexPolicy bool
}

type PromptStarter struct {
	Host               Host
	launcher           topologyLauncher
	openCodeSessions   openCodePromptSessions
	wait               func(context.Context, time.Duration) error
	deliver            func(context.Context, Prompt, PromptTarget) (PromptResult, error)
	managed            ManagedCodexLauncher
	openCodeServer     openCodePromptServerLauncher
	openCodeTUIReserve func() (int, error)
}

type preparedOpenCodeStart struct {
	server   OpenCodeServer
	session  string
	sessions openCodePromptSessions
	stop     func(context.Context) error
}

func prepareOpenCodeStart(ctx context.Context, host Host, executable, directory string, server OpenCodeServer, startServer bool, launcher openCodePromptServerLauncher, sessions openCodePromptSessions) (preparedOpenCodeStart, error) {
	prepared := preparedOpenCodeStart{server: server, sessions: sessions}
	if prepared.sessions == nil {
		prepared.sessions = nativeOpenCodePromptSessions{}
	}
	if startServer {
		if launcher == nil {
			launcher = nativeOpenCodePromptServerLauncher{runner: host.Runner}
		}
		var err error
		prepared.server, prepared.stop, err = launcher.Start(ctx, executable, directory)
		if err != nil {
			return preparedOpenCodeStart{}, err
		}
	}
	var err error
	prepared.session, err = prepared.sessions.Create(ctx, prepared.server)
	if err == nil {
		return prepared, nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openCodePromptServerStop)
	defer cancel()
	return preparedOpenCodeStart{}, errors.Join(err, prepared.stopServer(cleanupCtx))
}

func (prepared preparedOpenCodeStart) deleteSession(ctx context.Context) error {
	if prepared.session == "" {
		return nil
	}
	return prepared.sessions.Delete(ctx, prepared.server, prepared.session)
}

func (prepared preparedOpenCodeStart) stopServer(ctx context.Context) error {
	if prepared.stop == nil {
		return nil
	}
	return prepared.stop(ctx)
}

func NewPromptStarter(host Host) PromptStarter {
	return PromptStarter{Host: host, wait: wait, managed: NewManagedCodexLauncher(host)}
}

// Start validates transient input before creating one background native target,
// binds its exact immutable identity, and submits the prompt exactly once.
func (s PromptStarter) Start(ctx context.Context, prompt Prompt, request PromptStartRequest) (result PromptResult, returnErr error) {
	if ctx == nil {
		panic("nil context")
	}
	if err := ctx.Err(); err != nil {
		return PromptResult{State: PromptNotAttempted}, err
	}
	if err := validatePreparedPrompt(prompt); err != nil {
		return PromptResult{State: PromptNotAttempted}, err
	}
	argv, kind, err := validatePromptStart(request)
	if err != nil {
		return PromptResult{State: PromptNotAttempted}, err
	}
	if s.wait == nil {
		return PromptResult{State: PromptNotAttempted}, errors.New("prompt starter must be created with NewPromptStarter")
	}
	var (
		openCode preparedOpenCodeStart
		binding  *ordinaryCodexBinding
	)
	if kind == "opencode" {
		openCode, err = prepareOpenCodeStart(ctx, s.Host, request.Argv[0], request.Directory, request.OpenCodeServer, request.StartOpenCodeServer, s.openCodeServer, s.openCodeSessions)
		if err != nil {
			return PromptResult{State: PromptNotAttempted}, fmt.Errorf("prompt OpenCode session: %w", err)
		}
		request.OpenCodeServer = openCode.server
		argv = append(argv, "--session", openCode.session)
		endpointArgs, endpointErr := openCodeTUIEndpointArgs(s.openCodeTUIReserve)
		if endpointErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promptCleanupTimeout)
			defer cancel()
			return PromptResult{State: PromptNotAttempted}, errors.Join(
				fmt.Errorf("prompt OpenCode TUI: %w", endpointErr),
				openCode.deleteSession(cleanupCtx), openCode.stopServer(cleanupCtx),
			)
		}
		argv = append(argv, endpointArgs...)
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openCodePromptServerStop)
			defer cancel()
			returnErr = errors.Join(returnErr, openCode.stopServer(stopCtx))
		}()
	}
	cleanupOpenCode := func(cleanupCtx context.Context) error {
		return openCode.deleteSession(cleanupCtx)
	}
	if kind == "codex" {
		managed := s.managed
		if managed.Host.Runner == nil {
			managed = NewManagedCodexLauncher(s.Host)
		}
		binding, err = beginOrdinaryCodexBinding(ctx, managed, ManagedNew, Agent{}, request.Directory, request.CodexPolicy, request.ObserveCodexPolicy, request.ReadyTimeout)
		if err != nil {
			return PromptResult{State: PromptNotAttempted}, err
		}
	}

	created, err := s.topology().Launch(ctx, sessionhost.LaunchRequest{
		Origin: request.Origin, Argv: argv, Directory: request.Directory,
		Placement: request.Placement, Direction: request.Direction, TabLabel: request.TabLabel,
		Focus: false, OMPStatusSocket: request.OMPStatusSocket,
	})
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promptCleanupTimeout)
		defer cancel()
		return PromptResult{State: PromptNotAttempted}, errors.Join(err, cleanupOpenCode(cleanupCtx))
	}
	base := PromptResult{Target: PromptTarget{Created: created, Directory: request.Directory, Argv: append([]string(nil), argv...), OMPStatusSocket: request.OMPStatusSocket, OpenCodeServer: request.OpenCodeServer}, State: PromptNotAttempted}
	failBeforeSend := func(cause error) (PromptResult, error) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promptCleanupTimeout)
		defer cancel()
		return base, errors.Join(cause, s.topology().Cleanup(cleanupCtx, created), cleanupOpenCode(cleanupCtx))
	}
	if kind == "omp" && base.Target.OMPStatusSocket == "" {
		base.Target.OMPStatusSocket, err = derivePromptOMPStatusSocket(ctx, s.Host, created.PaneID)
		if err != nil {
			return failBeforeSend(fmt.Errorf("prompt OMP status address: %w", err))
		}
	}

	var agent Agent
	var prepared ordinaryCodexPreparation
	if binding != nil {
		prepared, err = binding.Prepare(ctx, created, request.Directory, argv)
		agent = prepared.Agent
	} else {
		agent, err = s.waitForAgent(ctx, created, kind, openCode.session, request.Directory, readyTimeout(request.ReadyTimeout))
	}
	if err != nil {
		return failBeforeSend(fmt.Errorf("prompt target identity: %w", err))
	}
	base.Agent = agent
	base.Target.Agent = agent
	base.Target.codexSessionPrefix = prepared.Prefix
	deliver := s.deliver
	if deliver == nil {
		deliver = NewPromptHost(s.Host).Deliver
	}
	result, err = deliver(ctx, prompt, base.Target)
	if err != nil {
		if result.State == PromptNotAttempted {
			return failBeforeSend(err)
		}
		return result, err
	}
	if prepared.Prefix != "" {
		bound, bindErr := binding.BindAfterPrompt(ctx, created, request.Directory, argv, prepared)
		if bindErr != nil {
			return result, fmt.Errorf("prompt Codex identity: %w", bindErr)
		}
		result.Agent = bound
		result.Target.Agent = bound
		result.Target.codexSessionPrefix = ""
	}
	if request.Focus {
		focused, focusErr := s.Host.Focus(ctx, result.Agent)
		if focusErr != nil {
			return result, focusErr
		}
		result.Agent = focused
	}
	return result, nil
}

func (s PromptStarter) waitForAgent(parent context.Context, created sessionhost.CreatedTarget, kind, expectedSession, directory string, timeout time.Duration) (Agent, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	for {
		agent, err := s.Host.find(ctx, created.PaneID)
		if err == nil {
			if agent.SessionName != created.SessionName || agent.WorkspaceID != created.WorkspaceID || agent.TabID != created.TabID || agent.PaneID != created.PaneID || !strings.EqualFold(agent.Kind, kind) || agent.CWD != directory || agent.Focused {
				return Agent{}, ErrPromptStale
			}
			if agent.Session != nil {
				if kind == "omp" && agent.Session.Source == "herdr:omp" && agent.Session.Agent == "omp" && agent.Session.Kind == "path" && filepath.IsAbs(agent.Session.Value) ||
					kind == "opencode" && agent.Session.Source == "herdr:opencode" && agent.Session.Agent == "opencode" && agent.Session.Kind == "id" && agent.Session.Value == expectedSession {
					if promptReadyStatus(agent.Status) {
						return agent, nil
					}
				} else {
					return Agent{}, ErrPromptStale
				}
			}
		}
		if err != nil && !errors.Is(err, ErrStaleReport) {
			return Agent{}, err
		}
		if err := s.wait(ctx, 50*time.Millisecond); err != nil {
			return Agent{}, err
		}
	}
}

func (s PromptStarter) topology() topologyLauncher {
	if s.launcher != nil {
		return s.launcher
	}
	return sessionhost.Host{Runner: s.Host.Runner, Herdr: s.Host.Herdr, Session: s.Host.Session}
}

func validatePromptStart(request PromptStartRequest) ([]string, string, error) {
	if len(request.Argv) == 0 {
		return nil, "", ErrPromptInvalid
	}
	kind := strings.ToLower(filepath.Base(request.Argv[0]))
	if err := validateForkExecutable(request.Argv[0], kind); err != nil {
		return nil, "", err
	}
	if kind != "codex" && kind != "omp" && kind != "opencode" {
		return nil, "", ErrUnsupportedFork
	}
	options, err := validatePromptStartOptions(kind, request.Argv[1:], request.ObserveCodexPolicy)
	if err != nil {
		return nil, "", err
	}
	if request.StartOpenCodeServer && kind != "opencode" {
		return nil, "", ErrPromptInvalid
	}
	if request.Directory == "" || !filepath.IsAbs(request.Directory) || filepath.Clean(request.Directory) != request.Directory || strings.ContainsRune(request.Directory, 0) {
		return nil, "", ErrPromptInvalid
	}
	if request.ReadyTimeout < 0 || request.ReadyTimeout > MaxForkReadyTimeout {
		return nil, "", ErrPromptInvalid
	}
	switch kind {
	case "codex":
		if request.ObserveCodexPolicy {
			if request.CodexPolicy != (ManagedCodexPolicy{}) {
				return nil, "", ErrPromptInvalid
			}
		} else if err := validateManagedPolicy(request.CodexPolicy); err != nil {
			return nil, "", err
		}
	case "omp":
		if request.ObserveCodexPolicy {
			return nil, "", ErrPromptInvalid
		}
		if request.OMPStatusSocket != "" && (!filepath.IsAbs(request.OMPStatusSocket) || filepath.Clean(request.OMPStatusSocket) != request.OMPStatusSocket || strings.ContainsRune(request.OMPStatusSocket, 0)) {
			return nil, "", ErrPromptInvalid
		}
	case "opencode":
		if request.ObserveCodexPolicy {
			return nil, "", ErrPromptInvalid
		}
		if request.StartOpenCodeServer && request.OpenCodeServer != (OpenCodeServer{}) {
			return nil, "", ErrPromptInvalid
		}
		if !request.StartOpenCodeServer {
			if err := validateOpenCodeServer(request.OpenCodeServer); err != nil {
				return nil, "", err
			}
		}
		if request.OpenCodeServer != (OpenCodeServer{}) && request.OpenCodeServer.Directory != request.Directory {
			return nil, "", ErrPromptInvalid
		}
	}
	return append([]string{request.Argv[0]}, options...), kind, nil
}

func validatePromptStartOptions(kind string, options []string, observeCodexPolicy bool) ([]string, error) {
	if kind != "codex" {
		return validateForkOptions(kind, options)
	}
	filtered := make([]string, 0, len(options))
	yolo := false
	for _, option := range options {
		if option != "--yolo" {
			filtered = append(filtered, option)
			continue
		}
		if yolo || !observeCodexPolicy {
			return nil, fmt.Errorf("prompt-start option %q is unsupported", option)
		}
		yolo = true
	}
	if _, err := validateForkOptions(kind, filtered); err != nil {
		return nil, err
	}
	return append([]string(nil), options...), nil
}
