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

const (
	DefaultForkReadyTimeout time.Duration = 30_000_000_000  // 30 seconds
	MaxForkReadyTimeout     time.Duration = 300_000_000_000 // 5 minutes
)

var ErrUnsupportedFork = errors.New("unsupported native fork")

type ForkProfile struct {
	Executable string
	Options    []string
}

type ForkRequest struct {
	Source          Agent
	Profile         ForkProfile
	Placement       sessionhost.Placement
	Direction       string
	TabLabel        string
	Focus           bool
	ReadyTimeout    time.Duration
	OMPStatusSocket string
	OpenCodeServer  OpenCodeServer
	CodexPolicy     ManagedCodexPolicy
	// ObserveCodexPolicy discovers the child policy from guarded Codex status
	// instead of requiring the consumer to predict the source policy.
	ObserveCodexPolicy bool
	Prompt             *Prompt
}

type ForkResult struct {
	Source  Agent
	Child   Agent
	Created sessionhost.CreatedTarget
	Prompt  PromptResult
}

type topologyLauncher interface {
	Launch(context.Context, sessionhost.LaunchRequest) (sessionhost.CreatedTarget, error)
	Cleanup(context.Context, sessionhost.CreatedTarget) error
}

type Forker struct {
	Host     Host
	launcher topologyLauncher
	wait     func(context.Context, time.Duration) error
	deliver  func(context.Context, Prompt, PromptTarget) (PromptResult, error)
	managed  ManagedCodexLauncher
}

func NewForker(host Host) Forker {
	return Forker{Host: host, wait: wait, managed: NewManagedCodexLauncher(host)}
}

// Fork preserves the source conversation and creates one distinct native
// conversation using the supported Codex 0.154.0, OMP 18.1.18, or OpenCode
// 1.18.30 selector. Profile options cannot supply lifecycle, identity, or CWD.
func (f Forker) Fork(ctx context.Context, request ForkRequest) (ForkResult, error) {
	if ctx == nil {
		panic("nil context")
	}
	if f.wait == nil {
		return ForkResult{}, errors.New("forker must be created with NewForker")
	}
	if request.Prompt != nil {
		if err := ctx.Err(); err != nil {
			return ForkResult{}, err
		}
		if err := validatePreparedPrompt(*request.Prompt); err != nil {
			return ForkResult{}, err
		}
		if strings.EqualFold(request.Source.Kind, "opencode") {
			if err := validateOpenCodeServer(request.OpenCodeServer); err != nil {
				return ForkResult{}, fmt.Errorf("fork prompt OpenCode server: %w", err)
			}
			if request.OpenCodeServer.Directory != request.Source.CWD {
				return ForkResult{}, fmt.Errorf("fork prompt OpenCode server: %w", ErrPromptInvalid)
			}
		}
	}
	if request.ReadyTimeout < 0 || request.ReadyTimeout > MaxForkReadyTimeout {
		return ForkResult{}, fmt.Errorf("fork readiness timeout must be between zero and %s", MaxForkReadyTimeout)
	}
	argv, kind, err := composeForkArgv(request)
	if err != nil {
		return ForkResult{}, fmt.Errorf("fork: %w", err)
	}
	if request.ObserveCodexPolicy && (kind != "codex" || request.Prompt == nil || request.CodexPolicy != (ManagedCodexPolicy{})) {
		return ForkResult{}, fmt.Errorf("fork Codex policy: %w", ErrUnsupportedFork)
	}
	before, err := f.Host.find(ctx, request.Source.PaneID)
	if err != nil || !sameForkSource(request.Source, before) {
		return ForkResult{}, fmt.Errorf("fork source: %w", ErrStaleReport)
	}
	var binding *ordinaryCodexBinding
	if request.Prompt != nil && kind == "codex" {
		managed := f.managed
		if managed.Host.Runner == nil {
			managed = NewManagedCodexLauncher(f.Host)
		}
		binding, err = beginOrdinaryCodexBinding(ctx, managed, ManagedFork, request.Source, request.Source.CWD, request.CodexPolicy, request.ObserveCodexPolicy, request.ReadyTimeout)
		if err != nil {
			return ForkResult{}, fmt.Errorf("fork Codex identity: %w", err)
		}
	}
	created, err := f.topology().Launch(ctx, sessionhost.LaunchRequest{
		Origin: sessionhost.Origin{
			SessionName: request.Source.SessionName,
			WorkspaceID: request.Source.WorkspaceID,
			TabID:       request.Source.TabID,
			PaneID:      request.Source.PaneID,
		},
		Argv: argv, Directory: request.Source.CWD, Placement: request.Placement,
		Direction: request.Direction, TabLabel: request.TabLabel, Focus: false,
		OMPStatusSocket: request.OMPStatusSocket,
	})
	if err != nil {
		return ForkResult{}, fmt.Errorf("fork launch: %w", err)
	}
	fail := func(cause error) (ForkResult, error) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promptCleanupTimeout)
		defer cancel()
		return ForkResult{}, errors.Join(cause, f.topology().Cleanup(cleanupCtx, created))
	}
	ompStatusSocket := request.OMPStatusSocket
	if kind == "omp" && request.Prompt != nil && ompStatusSocket == "" {
		ompStatusSocket, err = derivePromptOMPStatusSocket(ctx, f.Host, created.PaneID)
		if err != nil {
			return fail(fmt.Errorf("fork OMP status address: %w", err))
		}
	}
	var (
		child         Agent
		codexPrepared ordinaryCodexPreparation
	)
	if binding != nil {
		codexPrepared, err = binding.Prepare(ctx, created, request.Source.CWD, argv)
		child = codexPrepared.Agent
	} else {
		child, err = f.waitForChild(ctx, created, request.Source, kind, readyTimeout(request.ReadyTimeout))
	}
	if err != nil {
		return fail(fmt.Errorf("fork child: %w", err))
	}
	after, err := f.Host.find(ctx, request.Source.PaneID)
	if err != nil || !sameForkSource(before, after) {
		return fail(fmt.Errorf("fork source changed: %w", ErrStaleReport))
	}
	result := ForkResult{Source: after, Child: child, Created: created}
	if request.Prompt != nil {
		deliver := f.deliver
		if deliver == nil {
			deliver = NewPromptHost(f.Host).Deliver
		}
		promptTarget := PromptTarget{
			Created: created, Agent: child, Directory: request.Source.CWD, Argv: argv,
			Source: &after, OMPStatusSocket: ompStatusSocket, OpenCodeServer: request.OpenCodeServer,
			codexSessionPrefix: codexPrepared.Prefix,
		}
		promptResult, promptErr := deliver(ctx, *request.Prompt, promptTarget)
		result.Prompt = promptResult
		if promptErr != nil {
			if promptResult.State == PromptNotAttempted {
				return fail(promptErr)
			}
			return result, promptErr
		}
		child, result.Child = promptResult.Agent, promptResult.Agent
		if binding != nil && codexPrepared.Prefix != "" {
			child, err = binding.BindAfterPrompt(ctx, created, request.Source.CWD, argv, codexPrepared)
			if err != nil {
				return result, fmt.Errorf("fork Codex identity: %w", err)
			}
			result.Child = child
			result.Prompt.Agent = child
			result.Prompt.Target.Agent = child
			result.Prompt.Target.codexSessionPrefix = ""
		}
	}
	if request.Focus {
		child, err = f.Host.Focus(ctx, child)
		if err != nil {
			if request.Prompt != nil && result.Prompt.State == PromptAcknowledged {
				return result, fmt.Errorf("fork focus: %w", err)
			}
			return fail(fmt.Errorf("fork focus: %w", err))
		}
		result.Child = child
		if ready, _ := classifyForkChild(request.Source, child, created, kind); !ready {
			if request.Prompt != nil && result.Prompt.State == PromptAcknowledged {
				return result, fmt.Errorf("fork focus: %w", ErrStaleReport)
			}
			return fail(fmt.Errorf("fork focus: %w", ErrStaleReport))
		}
		afterFocus, findErr := f.Host.find(ctx, request.Source.PaneID)
		if findErr != nil || !sameForkSource(before, afterFocus) {
			if request.Prompt != nil && result.Prompt.State == PromptAcknowledged {
				return result, fmt.Errorf("fork source changed after focus: %w", ErrStaleReport)
			}
			return fail(fmt.Errorf("fork source changed after focus: %w", ErrStaleReport))
		}
		after = afterFocus
	}
	result.Source, result.Child = after, child
	return result, nil
}

func (f Forker) waitForChild(parent context.Context, created sessionhost.CreatedTarget, source Agent, kind string, timeout time.Duration) (Agent, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	for {
		child, err := f.Host.find(ctx, created.PaneID)
		if err == nil {
			ready, pending := classifyForkChild(source, child, created, kind)
			if ready {
				return child, nil
			}
			if !pending {
				return Agent{}, ErrStaleReport
			}
		}
		if err != nil && !errors.Is(err, ErrStaleReport) {
			return Agent{}, err
		}
		if err := f.wait(ctx, 50*time.Millisecond); err != nil {
			return Agent{}, err
		}
	}
}

func (f Forker) topology() topologyLauncher {
	if f.launcher != nil {
		return f.launcher
	}
	return sessionhost.Host{Runner: f.Host.Runner, Herdr: f.Host.Herdr, Session: f.Host.Session}
}

func composeForkArgv(request ForkRequest) ([]string, string, error) {
	if err := validateTarget(request.Source); err != nil || request.Source.Session == nil {
		return nil, "", ErrUnsupportedFork
	}
	if request.Source.CWD == "" || !filepath.IsAbs(request.Source.CWD) || filepath.Clean(request.Source.CWD) != request.Source.CWD || strings.ContainsRune(request.Source.CWD, 0) {
		return nil, "", errors.New("source CWD must be clean, absolute, and NUL-free")
	}
	kind := strings.ToLower(request.Source.Kind)
	if err := validateForkExecutable(request.Profile.Executable, kind); err != nil {
		return nil, "", err
	}
	options, err := validateForkOptions(kind, request.Profile.Options)
	if err != nil {
		return nil, "", err
	}
	session := request.Source.Session
	argv := []string{request.Profile.Executable}
	switch kind {
	case "codex":
		if session.Source != "herdr:codex" || session.Agent != "codex" || session.Kind != "id" {
			return nil, "", ErrUnsupportedFork
		}
		argv = append(argv, options...)
		argv = append(argv, "-C", request.Source.CWD, "fork", session.Value)
	case "omp":
		if session.Source != "herdr:omp" || session.Agent != "omp" || session.Kind != "path" || !filepath.IsAbs(session.Value) || filepath.Clean(session.Value) != session.Value {
			return nil, "", ErrUnsupportedFork
		}
		if request.OMPStatusSocket != "" && (!filepath.IsAbs(request.OMPStatusSocket) || filepath.Clean(request.OMPStatusSocket) != request.OMPStatusSocket || strings.ContainsRune(request.OMPStatusSocket, 0)) {
			return nil, "", ErrUnsupportedFork
		}
		argv = append(argv, options...)
		argv = append(argv, "--cwd", request.Source.CWD, "--fork", session.Value)
	case "opencode":
		if session.Source != "herdr:opencode" || session.Agent != "opencode" || session.Kind != "id" {
			return nil, "", ErrUnsupportedFork
		}
		argv = append(argv, request.Source.CWD)
		argv = append(argv, options...)
		argv = append(argv, "--session", session.Value, "--fork")
	default:
		return nil, "", ErrUnsupportedFork
	}
	return argv, kind, nil
}

func validateForkExecutable(executable, kind string) error {
	if executable == "" || len(executable) > maxPathBytes || strings.ContainsAny(executable, "\x00\r\n") || strings.HasPrefix(executable, "-") || filepath.Base(executable) != kind {
		return errors.New("fork executable must be the matching canonical harness name or clean absolute path")
	}
	if strings.ContainsRune(executable, filepath.Separator) && (!filepath.IsAbs(executable) || filepath.Clean(executable) != executable) {
		return errors.New("fork executable path must be clean and absolute")
	}
	return nil
}

type optionSpec struct {
	value     bool
	config    bool
	canonical string
}

func validateForkOptions(kind string, options []string) ([]string, error) {
	var specs map[string]optionSpec
	switch kind {
	case "codex":
		specs = map[string]optionSpec{
			"-m": {value: true, canonical: "model"}, "--model": {value: true, canonical: "model"},
			"-s": {value: true, canonical: "sandbox"}, "--sandbox": {value: true, canonical: "sandbox"},
			"-a": {value: true, canonical: "approval"}, "--ask-for-approval": {value: true, canonical: "approval"},
			"--enable": {value: true}, "--disable": {value: true},
			"-c": {value: true, config: true, canonical: "config"}, "--config": {value: true, config: true, canonical: "config"},
			"--strict-config": {}, "--search": {}, "--no-alt-screen": {}, "--approve-for-me": {},
		}
	case "omp":
		specs = map[string]optionSpec{
			"--model": {value: true}, "--smol": {value: true}, "--slow": {value: true}, "--plan": {value: true},
			"--provider": {value: true}, "--system-prompt": {value: true}, "--append-system-prompt": {value: true},
			"--models": {value: true}, "--thinking": {value: true}, "--service-tier": {value: true}, "--tools": {value: true},
			"--approval-mode": {value: true}, "--prewalk": {}, "--no-prewalk": {}, "--no-tools": {}, "--no-lsp": {},
			"--no-pty": {}, "--hide-thinking": {}, "--advisor": {}, "--external-thinking": {}, "--no-title": {},
			"--auto-approve": {}, "--no-extensions": {}, "--no-skills": {}, "--no-rules": {},
		}
	case "opencode":
		specs = map[string]optionSpec{
			"-m": {value: true, canonical: "model"}, "--model": {value: true, canonical: "model"}, "--agent": {value: true}, "--log-level": {value: true},
			"--replay-limit": {value: true}, "--print-logs": {}, "--pure": {}, "--auto": {}, "--mini": {}, "--no-replay": {},
		}
	default:
		return nil, ErrUnsupportedFork
	}
	validated := append([]string(nil), options...)
	seen := make(map[string]struct{}, len(options))
	for i := 0; i < len(options); i++ {
		raw := options[i]
		name, inline, hasInline := strings.Cut(raw, "=")
		spec, allowed := specs[name]
		if !allowed || raw == "--" || strings.ContainsRune(raw, 0) {
			return nil, fmt.Errorf("fork option %q is unsupported", raw)
		}
		if !spec.value {
			key := optionKey(name, spec)
			if _, duplicate := seen[key]; duplicate {
				return nil, fmt.Errorf("fork option %q is duplicated", name)
			}
			seen[key] = struct{}{}
			if hasInline {
				return nil, fmt.Errorf("fork option %q does not accept a value", name)
			}
			continue
		}
		value := inline
		if !hasInline {
			i++
			if i >= len(options) {
				return nil, fmt.Errorf("fork option %q requires a value", name)
			}
			value = options[i]
			if value == "" || strings.HasPrefix(value, "-") || strings.ContainsRune(value, 0) {
				return nil, fmt.Errorf("fork option %q has an invalid value", name)
			}
		} else if value == "" {
			return nil, fmt.Errorf("fork option %q has an invalid value", name)
		}
		if spec.config {
			key, _, ok := strings.Cut(value, "=")
			if !ok || key != "model" && key != "model_reasoning_effort" && key != "service_tier" {
				return nil, fmt.Errorf("Codex config override %q is unsupported", value)
			}
			duplicateKey := "config:" + key
			if _, duplicate := seen[duplicateKey]; duplicate {
				return nil, fmt.Errorf("Codex config override %q is duplicated", key)
			}
			seen[duplicateKey] = struct{}{}
			continue
		}
		key := optionKey(name, spec)
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("fork option %q is duplicated", name)
		}
		seen[key] = struct{}{}
	}
	return validated, nil
}

func optionKey(name string, spec optionSpec) string {
	if spec.canonical != "" {
		return spec.canonical
	}
	return name
}

func classifyForkChild(source, child Agent, created sessionhost.CreatedTarget, kind string) (ready, pending bool) {
	if !strings.EqualFold(child.Kind, kind) || child.SessionName != source.SessionName ||
		child.WorkspaceID != created.WorkspaceID || child.TabID != created.TabID || child.PaneID != created.PaneID || child.CWD != source.CWD || source.Session == nil {
		return false, false
	}
	if child.Session == nil {
		return false, true
	}
	if child.Session.Source != source.Session.Source || child.Session.Agent != source.Session.Agent || child.Session.Kind != source.Session.Kind {
		return false, false
	}
	if child.Session.Value == source.Session.Value {
		return false, true
	}
	if kind == "omp" && (!filepath.IsAbs(child.Session.Value) || filepath.Clean(child.Session.Value) != child.Session.Value) {
		return false, false
	}
	return true, false
}

func sameForkSource(a, b Agent) bool { return sameImmutable(a, b) && a.CWD == b.CWD }

func readyTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return DefaultForkReadyTimeout
	}
	return timeout
}
