package agenthost

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrapi"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

const (
	MaxPromptCharacters                     = 20_000
	DefaultPromptReadyTimeout time.Duration = 30_000_000_000 // 30 seconds
	promptCleanupTimeout      time.Duration = 5_000_000_000  // 5 seconds
)

var (
	ErrPromptInvalid        = errors.New("prompt is invalid")
	ErrPromptStale          = errors.New("prompt target is stale")
	ErrPromptNotSent        = errors.New("prompt was not sent")
	ErrPromptOutcomeUnknown = errors.New("prompt outcome is unknown")
	errPromptNotReady       = errors.New("prompt target is not ready")
	openCodeANSISpan        = regexp.MustCompile(`\x1b\[([0-9;]*)m([^\x1b]*)`)
)

type Prompt struct {
	text string
}

// PreparePrompt validates transient input before a caller creates any target.
// The original UTF-8 bytes are retained without trimming or normalization.
func PreparePrompt(text string) (Prompt, error) {
	if !utf8.ValidString(text) || strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > MaxPromptCharacters {
		return Prompt{}, ErrPromptInvalid
	}
	return Prompt{text: text}, nil
}

type PromptState string

const (
	PromptNotAttempted          PromptState = "not_attempted"
	CreatedPromptNotSent        PromptState = "created_prompt_not_sent"
	PromptAcknowledged          PromptState = "prompt_acknowledged"
	CreatedPromptOutcomeUnknown PromptState = "created_prompt_outcome_unknown"
)

type PromptTarget struct {
	Created         sessionhost.CreatedTarget
	Agent           Agent
	Directory       string
	Argv            []string
	Source          *Agent
	OMPStatusSocket string
	OpenCodeServer  OpenCodeServer
	CodexService    CodexServiceSnapshot
}

type PromptResult struct {
	Target PromptTarget
	Agent  Agent
	State  PromptState
}

type promptCaller interface {
	CallWithReceipt(context.Context, string, string, string, any, any) (herdrapi.Receipt, error)
}

type PromptHost struct {
	Host    Host
	caller  promptCaller
	managed ManagedCodexLauncher
	wait    func(context.Context, time.Duration) error
}

func NewPromptHost(host Host) PromptHost {
	return PromptHost{Host: host, caller: herdrapi.Client{}, managed: NewManagedCodexLauncher(host), wait: wait}
}

// Deliver rechecks an already-created, authoritative background target and
// makes one prompt request. It never retries or cleans up the target.
func (h PromptHost) Deliver(ctx context.Context, prompt Prompt, target PromptTarget) (PromptResult, error) {
	if ctx == nil {
		panic("nil context")
	}
	result := PromptResult{Target: clonePromptTarget(target), Agent: clonePromptAgent(target.Agent), State: PromptNotAttempted}
	if err := validatePreparedPrompt(prompt); err != nil {
		return result, err
	}
	if err := h.validateTarget(target); err != nil {
		return result, err
	}

	if err := h.Host.readDetection(ctx, target.Agent.PaneID); err != nil {
		return result, fmt.Errorf("prompt target detection: %w", err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, DefaultPromptReadyTimeout)
	defer cancel()
	waitReady := h.wait
	if waitReady == nil {
		waitReady = wait
	}
	var (
		first   Agent
		process sessionhost.ForegroundProcess
		native  string
		err     error
	)
	for {
		first, process, native, err = h.observe(readyCtx, target)
		if !errors.Is(err, errPromptNotReady) {
			break
		}
		if err := waitReady(readyCtx, 50*time.Millisecond); err != nil {
			return result, errors.Join(ErrPromptStale, err)
		}
	}
	if err != nil {
		return result, err
	}
	if err := waitReady(readyCtx, DefaultSettle()); err != nil {
		return result, errors.Join(ErrPromptStale, err)
	}
	second, secondProcess, secondNative, err := h.observe(readyCtx, target)
	if err != nil || !sameImmutable(first, second) || first.CWD != second.CWD || first.Status != second.Status || first.StateChangeSeq != second.StateChangeSeq || !sameForegroundProcess(process, secondProcess) || native != secondNative {
		return result, ErrPromptStale
	}

	socket, err := (sessionhost.Host{Runner: h.Host.Runner, Herdr: h.Host.Herdr, Session: h.Host.Session}).Socket(readyCtx)
	if err != nil {
		return result, fmt.Errorf("prompt Herdr session: %w", err)
	}
	final, finalProcess, finalNative, err := h.observe(readyCtx, target)
	if err != nil || !sameImmutable(second, final) || second.CWD != final.CWD || second.Status != final.Status || second.StateChangeSeq != final.StateChangeSeq || !sameForegroundProcess(secondProcess, finalProcess) || secondNative != finalNative {
		return result, ErrPromptStale
	}

	params := struct {
		Target string `json:"target"`
		Text   string `json:"text"`
	}{Target: final.PaneID, Text: prompt.text}
	var response struct {
		Type  string    `json:"type"`
		Agent wireAgent `json:"agent"`
	}
	caller := h.caller
	if caller == nil {
		caller = herdrapi.Client{}
	}
	receipt, callErr := caller.CallWithReceipt(ctx, socket, "plugin-kit:prompt", "agent.prompt", params, &response)
	if callErr != nil {
		if receipt.State == herdrapi.RequestNotWritten {
			result.State = CreatedPromptNotSent
			return result, ErrPromptNotSent
		}
		result.State = CreatedPromptOutcomeUnknown
		return result, ErrPromptOutcomeUnknown
	}
	if receipt.State != herdrapi.RequestAcknowledged || response.Type != "agent_prompted" {
		result.State = CreatedPromptOutcomeUnknown
		return result, ErrPromptOutcomeUnknown
	}
	acknowledged, err := response.Agent.public()
	if err != nil {
		result.State = CreatedPromptOutcomeUnknown
		return result, ErrPromptOutcomeUnknown
	}
	acknowledged.SessionName = h.Host.Session
	if !sameImmutable(final, acknowledged) || acknowledged.CWD != final.CWD || acknowledged.Focused {
		result.State = CreatedPromptOutcomeUnknown
		return result, ErrPromptOutcomeUnknown
	}
	result.Agent = clonePromptAgent(acknowledged)
	result.State = PromptAcknowledged
	return result, nil
}

func (h PromptHost) validateTarget(target PromptTarget) error {
	if h.Host.Runner == nil || validateTarget(target.Agent) != nil || target.Agent.Session == nil || target.Agent.SessionName != h.Host.Session || target.Agent.Focused || !promptReadyStatus(target.Agent.Status) {
		return ErrPromptStale
	}
	if target.Created.Origin != (sessionhost.Origin{
		SessionName: target.Agent.SessionName,
		WorkspaceID: target.Agent.WorkspaceID,
		TabID:       target.Agent.TabID,
		PaneID:      target.Agent.PaneID,
	}) || target.Directory != target.Agent.CWD || len(target.Argv) == 0 {
		return ErrPromptStale
	}
	if target.Source != nil {
		if validateTarget(*target.Source) != nil || target.Source.SessionName != target.Agent.SessionName || target.Source.WorkspaceID != target.Agent.WorkspaceID || target.Source.PaneID == target.Agent.PaneID || sameSession(target.Source.Session, target.Agent.Session) {
			return ErrPromptStale
		}
	}
	switch strings.ToLower(target.Agent.Kind) {
	case "codex":
		if !validManagedServiceSnapshot(target.CodexService) || !validManagedSession(target.Agent.Session) {
			return ErrPromptStale
		}
	case "opencode":
		if validateOpenCodeServer(target.OpenCodeServer) != nil || target.OpenCodeServer.Directory != target.Directory {
			return ErrPromptStale
		}
	}
	return nil
}

func (h PromptHost) observe(ctx context.Context, target PromptTarget) (Agent, sessionhost.ForegroundProcess, string, error) {
	current, err := h.Host.find(ctx, target.Agent.PaneID)
	if err != nil || !sameImmutable(target.Agent, current) || current.CWD != target.Directory || current.Focused || !promptReadyStatus(current.Status) {
		return Agent{}, sessionhost.ForegroundProcess{}, "", ErrPromptStale
	}
	process, err := (sessionhost.Host{Runner: h.Host.Runner, Herdr: h.Host.Herdr, Session: h.Host.Session}).VerifyForegroundProcess(ctx, target.Created.Origin, target.Directory, target.Argv)
	if err != nil {
		return Agent{}, sessionhost.ForegroundProcess{}, "", fmt.Errorf("prompt target process: %w", err)
	}
	native, err := h.nativeReady(ctx, target)
	if err != nil {
		return Agent{}, sessionhost.ForegroundProcess{}, "", err
	}
	return current, process, native, nil
}

func (h PromptHost) nativeReady(ctx context.Context, target PromptTarget) (string, error) {
	switch strings.ToLower(target.Agent.Kind) {
	case "codex":
		if !validManagedServiceSnapshot(target.CodexService) || !validManagedSession(target.Agent.Session) {
			return "", ErrPromptStale
		}
		managed := h.managed
		if managed.Host.Runner == nil {
			managed = NewManagedCodexLauncher(h.Host)
		}
		service, err := managed.serviceClient().Ensure(ctx)
		if err != nil || service != target.CodexService {
			return "", ErrPromptStale
		}
		verifier, err := managed.verifierFactory().Connect(ctx, service.SocketPath)
		if err != nil {
			return "", ErrPromptStale
		}
		identity, readErr := verifier.ReadThread(ctx, target.Agent.Session.Value)
		closeErr := verifier.Close()
		if readErr != nil || closeErr != nil || identity.ID != target.Agent.Session.Value || identity.CWD != target.Directory {
			return "", ErrPromptStale
		}
		visible, err := (ManagedCodexLauncher{Host: h.Host}).readPaneANSI(ctx, target.Agent.PaneID, "visible", managedStatusLines)
		if err != nil {
			return "", ErrPromptStale
		}
		if !emptyCodexComposer(visible) {
			return "", errPromptNotReady
		}
		return "codex:empty", nil
	case "omp":
		payload, err := readOMPStatus(ctx, target.OMPStatusSocket)
		if err != nil {
			return "", errPromptNotReady
		}
		if target.Agent.Session == nil || payload.SessionValue != target.Agent.Session.Value {
			return "", ErrPromptStale
		}
		if payload.Status != "idle" {
			return "", errPromptNotReady
		}
		visible, err := (ManagedCodexLauncher{Host: h.Host}).readPaneText(ctx, target.Agent.PaneID, "visible", managedStatusLines)
		if err != nil {
			return "", errPromptNotReady
		}
		if !emptyOMPComposer(visible) {
			return "", errPromptNotReady
		}
		return fmt.Sprintf("omp:%s:%d", payload.SessionValue, payload.StateSeq), nil
	case "opencode":
		if err := validateOpenCodeServer(target.OpenCodeServer); err != nil || target.OpenCodeServer.Directory != target.Directory || target.Agent.Session == nil {
			return "", ErrPromptStale
		}
		assessment, err := readOpenCodeStatus(ctx, target.OpenCodeServer, target.Agent.Session.Value, true)
		if errors.Is(err, ErrStaleReport) {
			return "", ErrPromptStale
		}
		if err != nil || assessment.Status != Idle || !assessment.Stable {
			return "", errPromptNotReady
		}
		visible, err := (ManagedCodexLauncher{Host: h.Host}).readPaneANSI(ctx, target.Agent.PaneID, "visible", managedStatusLines)
		if err != nil || !emptyOpenCodeComposer(visible) {
			return "", errPromptNotReady
		}
		return "opencode:idle", nil
	default:
		return "", ErrPromptStale
	}
}

func emptyOMPComposer(text string) bool {
	// OMP 18.1.18 renders its empty input as the final bare lower-left border.
	lines := strings.Split(strings.ReplaceAll(managedANSI.ReplaceAllString(text, ""), "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		plain := strings.TrimSpace(lines[i])
		if plain == "" {
			continue
		}
		return plain == "╰─"
	}
	return false
}

func emptyOpenCodeComposer(text string) bool {
	// OpenCode 1.18.30 renders blank input rows immediately above one model
	// status row and the heavy lower border. The optional right sidebar shares
	// those rows, so compare ANSI backgrounds and inspect only the composer.
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for bottom := len(lines) - 1; bottom >= 0; bottom-- {
		if !strings.HasPrefix(strings.TrimSpace(managedANSI.ReplaceAllString(lines[bottom], "")), "╹▀") {
			continue
		}
		status := bottom - 1
		statusLine := ""
		if status >= 0 {
			statusLine = strings.TrimSpace(managedANSI.ReplaceAllString(lines[status], ""))
		}
		if !strings.HasPrefix(statusLine, "┃  ") || strings.Count(statusLine, "·") < 2 {
			return false
		}
		background, ok := openCodeComposerBackground(lines[status])
		if !ok {
			return false
		}
		blankRows := 0
		for row := status - 1; row >= 0; row-- {
			plain := strings.TrimSpace(managedANSI.ReplaceAllString(lines[row], ""))
			if !strings.HasPrefix(plain, "┃") {
				break
			}
			content, found := openCodeBackgroundText(lines[row], background)
			if !found || strings.TrimSpace(content) != "" {
				return false
			}
			blankRows++
		}
		return blankRows >= 2
	}
	return false
}

func openCodeComposerBackground(line string) (string, bool) {
	afterBorder := false
	for _, match := range openCodeANSISpan.FindAllStringSubmatch(line, -1) {
		if !afterBorder {
			afterBorder = strings.Contains(match[2], "┃")
			continue
		}
		if strings.TrimSpace(match[2]) == "" {
			continue
		}
		background, ok := ansiBackground(match[1])
		return background, ok
	}
	return "", false
}

func openCodeBackgroundText(line, expected string) (string, bool) {
	var content strings.Builder
	found := false
	for _, match := range openCodeANSISpan.FindAllStringSubmatch(line, -1) {
		background, ok := ansiBackground(match[1])
		if ok && background == expected {
			found = true
			content.WriteString(match[2])
		}
	}
	return content.String(), found
}

func ansiBackground(parameters string) (string, bool) {
	parts := strings.Split(parameters, ";")
	for i := 0; i < len(parts); i++ {
		if parts[i] == "49" {
			return "default", true
		}
		if parts[i] != "48" || i+1 >= len(parts) {
			continue
		}
		switch parts[i+1] {
		case "5":
			if i+2 < len(parts) {
				return strings.Join(parts[i:i+3], ";"), true
			}
		case "2":
			if i+4 < len(parts) {
				return strings.Join(parts[i:i+5], ";"), true
			}
		}
	}
	return "", false
}

func validatePreparedPrompt(prompt Prompt) error {
	if !utf8.ValidString(prompt.text) || strings.TrimSpace(prompt.text) == "" || utf8.RuneCountInString(prompt.text) > MaxPromptCharacters {
		return ErrPromptInvalid
	}
	return nil
}

func promptReadyStatus(status string) bool {
	return status == "idle" || status == "done"
}

func clonePromptTarget(target PromptTarget) PromptTarget {
	target.Argv = append([]string(nil), target.Argv...)
	target.Agent = clonePromptAgent(target.Agent)
	if target.Source != nil {
		source := clonePromptAgent(*target.Source)
		target.Source = &source
	}
	return target
}

func clonePromptAgent(agent Agent) Agent {
	if agent.Session != nil {
		session := *agent.Session
		agent.Session = &session
	}
	return agent
}
