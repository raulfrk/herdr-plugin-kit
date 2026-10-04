package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrcmd"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

const (
	managedStatusLines = 220
	managedPaneBytes   = 64 << 10
)

var (
	ErrManagedPaneBusy = errors.New("managed Codex pane is not safely idle")
	ErrManagedNarrow   = errors.New("managed Codex status is incomplete; widen the pane and retry")
	managedUUID        = regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	managedSessionPart = regexp.MustCompile(`(?m)^\s*│?\s*Session:\s*([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{2,12})\s*│?\s*$`)
	managedForkedPart  = regexp.MustCompile(`(?m)^\s*│?\s*Forked from:\s*([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{2,12})\s*│?\s*$`)
	managedANSI        = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	managedStatusCmd   = regexp.MustCompile(`(?m)^(?:›[ \t]+)?/status[ \t]*$`)
	managedStatusModel = regexp.MustCompile(`Model:([A-Za-z0-9][A-Za-z0-9._:/-]*)\(reasoning([A-Za-z0-9][A-Za-z0-9._-]*)(?:,|\))`)
	managedTitlePart   = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4,12})(?:\.\.\.)?$`)
	managedThreadPart  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4,11}$`)
	managedDisplayPart = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{2,12}$`)
)

type managedStatusObservation struct {
	identity         string
	forkedFromPrefix string
	policy           ManagedCodexPolicy
	policyIsPrefix   bool
	fromStatus       bool
}

func (l ManagedCodexLauncher) waitForProcess(ctx context.Context, target sessionhost.Origin, cwd string, argv []string, timeout time.Duration) (sessionhost.ForegroundProcess, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		process, err := l.topologyHost().VerifyForegroundProcess(waitCtx, target, cwd, argv)
		if err == nil {
			return process, nil
		}
		if errors.Is(err, sessionhost.ErrAmbiguousTarget) {
			return sessionhost.ForegroundProcess{}, err
		}
		if err := waitManaged(waitCtx, 25*time.Millisecond); err != nil {
			return sessionhost.ForegroundProcess{}, errors.Join(err, waitCtx.Err())
		}
	}
}

func (l ManagedCodexLauncher) discoverManagedIdentity(ctx context.Context, target sessionhost.Origin, cwd string, argv []string, service CodexServiceSnapshot, policy ManagedCodexPolicy) (Agent, string, error) {
	agent, observed, err := l.discoverManagedIdentityObservationExpected(ctx, target, cwd, argv, service, policy, false, nil, nil, false, false)
	return agent, observed.identity, err
}

func (l ManagedCodexLauncher) discoverManagedIdentityObservation(ctx context.Context, target sessionhost.Origin, cwd string, argv []string, service CodexServiceSnapshot, policy ManagedCodexPolicy, allowPrefix bool) (Agent, managedStatusObservation, error) {
	return l.discoverManagedIdentityObservationExpected(ctx, target, cwd, argv, service, policy, allowPrefix, nil, nil, false, false)
}

func (l ManagedCodexLauncher) discoverOrdinaryManagedIdentityObservation(ctx context.Context, target sessionhost.Origin, cwd string, argv []string, service CodexServiceSnapshot, policy ManagedCodexPolicy, allowPrefix bool) (Agent, managedStatusObservation, error) {
	return l.discoverManagedIdentityObservationExpected(ctx, target, cwd, argv, service, policy, allowPrefix, nil, nil, true, false)
}

func (l ManagedCodexLauncher) discoverManagedIdentityForSource(ctx context.Context, target sessionhost.Origin, cwd string, expected Agent, expectedProcess sessionhost.ForegroundProcess, service CodexServiceSnapshot) (Agent, managedStatusObservation, error) {
	agent, observed, err := l.discoverManagedIdentityObservationExpected(ctx, target, cwd, expectedProcess.Argv, service, ManagedCodexPolicy{}, false, &expected, &expectedProcess, true, false)
	return agent, observed, err
}

func (l ManagedCodexLauncher) discoverManagedIdentityObservationExpected(ctx context.Context, target sessionhost.Origin, cwd string, argv []string, service CodexServiceSnapshot, policy ManagedCodexPolicy, allowPrefix bool, expected *Agent, expectedProcess *sessionhost.ForegroundProcess, allowLocal, requireStatus bool) (Agent, managedStatusObservation, error) {
	observedLoading := false
	var firstProcess *sessionhost.ForegroundProcess
	verifyProcess := func() error {
		if expectedProcess != nil {
			current, err := l.topologyHost().InspectForegroundProcess(ctx, target)
			if err != nil {
				return fmt.Errorf("foreground process changed: %w", err)
			}
			if !sameForegroundProcess(*expectedProcess, current) {
				return fmt.Errorf("foreground process changed: %w", ErrManagedStale)
			}
		}
		process, err := l.topologyHost().VerifyForegroundProcess(ctx, target, cwd, argv)
		if err != nil {
			return fmt.Errorf("foreground process verification: %w", err)
		}
		if firstProcess == nil {
			process.Argv = append([]string(nil), process.Argv...)
			firstProcess = &process
		} else if observedLoading && !sameForegroundProcess(*firstProcess, process) {
			return fmt.Errorf("initializing process changed: %w", ErrManagedStale)
		}
		return nil
	}
	for {
		wire, agent, err := l.readManagedAgent(ctx, target, cwd)
		if err != nil {
			// Herdr 0.8.2 can briefly return agent_not_found after the exact
			// foreground Codex process is already present. Treat only that
			// pane-bound interval as unsettled; process or target drift still
			// fails immediately and no terminal input is sent.
			if processErr := verifyProcess(); processErr != nil {
				return Agent{}, managedStatusObservation{}, processErr
			}
			if waitErr := waitManaged(ctx, 25*time.Millisecond); waitErr != nil {
				return Agent{}, managedStatusObservation{}, errors.Join(ErrManagedStale, err, waitErr)
			}
			continue
		}
		if expected != nil && !sameManagedPreInputObservation(*expected, agent) {
			return Agent{}, managedStatusObservation{}, fmt.Errorf("source observation changed: %w", ErrManagedStale)
		}
		if !managedPaneReadyForSessionChange(wire, false) {
			if err := waitManaged(ctx, 25*time.Millisecond); err != nil {
				return Agent{}, managedStatusObservation{}, errors.Join(ErrManagedPaneBusy, err)
			}
			continue
		}
		if err := verifyProcess(); err != nil {
			return Agent{}, managedStatusObservation{}, err
		}
		if agent.Session != nil && !requireStatus {
			if !validManagedSession(agent.Session) {
				return Agent{}, managedStatusObservation{}, ErrManagedStale
			}
			visible, err := l.readPaneANSI(ctx, target.PaneID, "visible", managedStatusLines)
			if err != nil {
				return Agent{}, managedStatusObservation{}, err
			}
			if !emptyCodexComposer(visible) || codexStartupLoading(visible) {
				if expectedProcess != nil {
					return Agent{}, managedStatusObservation{}, errors.Join(ErrCodexConversionComposer, ErrManagedPaneBusy)
				}
				if err := waitManaged(ctx, 25*time.Millisecond); err != nil {
					return Agent{}, managedStatusObservation{}, errors.Join(ErrManagedPaneBusy, err)
				}
				continue
			}
			checkWire, check, err := l.readManagedAgent(ctx, target, cwd)
			if err != nil || checkWire.Focused || checkWire.LaunchPending || !managedPaneQuiescent(checkWire) || !sameImmutable(agent, check) || check.Session == nil || *agent.Session != *check.Session {
				if err == nil {
					err = ErrManagedPaneBusy
				}
				return Agent{}, managedStatusObservation{}, err
			}
			if err := verifyProcess(); err != nil {
				return Agent{}, managedStatusObservation{}, err
			}
			visible, err = l.readPaneANSI(ctx, target.PaneID, "visible", managedStatusLines)
			if err != nil {
				return Agent{}, managedStatusObservation{}, err
			}
			if !emptyCodexComposer(visible) || codexStartupLoading(visible) {
				if expectedProcess != nil {
					return Agent{}, managedStatusObservation{}, errors.Join(ErrCodexConversionComposer, ErrManagedPaneBusy)
				}
				return Agent{}, managedStatusObservation{}, ErrManagedPaneBusy
			}
			return check, managedStatusObservation{identity: check.Session.Value, policy: policy}, nil
		}
		visible, err := l.readPaneANSI(ctx, target.PaneID, "visible", managedStatusLines)
		if err != nil {
			return Agent{}, managedStatusObservation{}, err
		}
		observedLoading = observedLoading || codexStartupLoading(visible)
		if !emptyCodexComposer(visible) || codexStartupLoading(visible) {
			if expectedProcess != nil {
				return Agent{}, managedStatusObservation{}, errors.Join(ErrCodexConversionComposer, ErrManagedPaneBusy)
			}
			if err := waitManaged(ctx, 25*time.Millisecond); err != nil {
				return Agent{}, managedStatusObservation{}, errors.Join(ErrManagedPaneBusy, err)
			}
			continue
		}
		before, err := l.readFullPaneHistory(ctx, target)
		if err != nil {
			return Agent{}, managedStatusObservation{}, err
		}
		// Re-read every guard immediately before the first byte of input.
		check, checkedAgent, err := l.readManagedAgent(ctx, target, cwd)
		if err != nil || check.Focused || check.LaunchPending || !managedPaneQuiescent(check) {
			// Initialization can change idle to working after the model appears.
			// Reacquire readiness only for that observed startup, before any input,
			// on the same process and immutable target; existing sources still fail.
			if err == nil && observedLoading && expected == nil &&
				!check.Focused && !check.LaunchPending && sameImmutable(agent, checkedAgent) {
				if err := verifyProcess(); err != nil {
					return Agent{}, managedStatusObservation{}, err
				}
				if err := waitManaged(ctx, 25*time.Millisecond); err != nil {
					return Agent{}, managedStatusObservation{}, errors.Join(ErrManagedPaneBusy, err)
				}
				continue
			}
			if err == nil {
				err = ErrManagedPaneBusy
			}
			return Agent{}, managedStatusObservation{}, err
		}
		if !sameManagedPreInputObservation(agent, checkedAgent) {
			return Agent{}, managedStatusObservation{}, fmt.Errorf("pre-input source observation changed: %w", ErrManagedStale)
		}
		if err := verifyProcess(); err != nil {
			return Agent{}, managedStatusObservation{}, err
		}
		visible, err = l.readPaneANSI(ctx, target.PaneID, "visible", managedStatusLines)
		if err != nil {
			return Agent{}, managedStatusObservation{}, err
		}
		if !emptyCodexComposer(visible) || codexStartupLoading(visible) {
			if expectedProcess != nil {
				return Agent{}, managedStatusObservation{}, errors.Join(ErrCodexConversionComposer, ErrManagedPaneBusy)
			}
			return Agent{}, managedStatusObservation{}, ErrManagedPaneBusy
		}
		if err := l.runHerdr(ctx, "pane", "send-text", target.PaneID, "/status"); err != nil {
			return Agent{}, managedStatusObservation{}, err
		}
		if err := l.awaitStatusDraft(ctx, target, cwd, argv, checkedAgent); err != nil {
			return Agent{}, managedStatusObservation{}, err
		}
		if err := l.runHerdr(ctx, "pane", "send-keys", target.PaneID, "enter"); err != nil {
			return Agent{}, managedStatusObservation{}, err
		}
		titlePrefix := ""
		if allowPrefix {
			titlePrefix = managedWireThreadPrefix(check)
		}
		observed, err := l.awaitFreshStatusObservationMode(ctx, target, before, cwd, service, policy, allowPrefix, titlePrefix, allowLocal)
		if err != nil {
			return Agent{}, managedStatusObservation{}, fmt.Errorf("fresh /status observation: %w", err)
		}
		if managedPrefixReadbackRequired(allowPrefix, observed.identity) {
			finalWire, finalAgent, err := l.readManagedAgent(ctx, target, cwd)
			if err != nil {
				return Agent{}, managedStatusObservation{}, err
			}
			finalPrefix := managedWireThreadPrefix(finalWire)
			if !validManagedPrefixReadback(checkedAgent, finalWire, finalAgent, observed.identity, finalPrefix) {
				return Agent{}, managedStatusObservation{}, ErrManagedStale
			}
			if err := verifyProcess(); err != nil {
				return Agent{}, managedStatusObservation{}, err
			}
		}
		// The App Server must validate this observed ID before Launch reports it
		// to Herdr. Return the still-unregistered pane observation here.
		return checkedAgent, observed, nil
	}
}

func managedPrefixReadbackRequired(allowPrefix bool, identity string) bool {
	return allowPrefix && validManagedThreadPrefix(identity)
}

func validManagedPrefixReadback(expected Agent, wire wireAgent, current Agent, observedPrefix, titlePrefix string) bool {
	if !managedPaneQuiescent(wire) {
		return false
	}
	if current.Session != nil {
		return false
	}
	if !sameImmutable(expected, current) {
		return false
	}
	if titlePrefix == "" {
		return false
	}
	return strings.HasPrefix(titlePrefix, observedPrefix) || strings.HasPrefix(observedPrefix, titlePrefix)
}

func sameManagedPreInputObservation(before, after Agent) bool {
	if !sameImmutable(before, after) {
		return false
	}
	if before.Status != after.Status {
		return false
	}
	if before.Revision != after.Revision {
		return false
	}
	return before.StateChangeSeq == after.StateChangeSeq
}

func (l ManagedCodexLauncher) bindManagedIdentity(ctx context.Context, target sessionhost.Origin, cwd string, observed Agent, threadID string) (Agent, error) {
	if observed.Session == nil {
		if err := l.runHerdr(ctx, "pane", "report-agent-session", target.PaneID, "--source", "herdr:codex", "--agent", "codex", "--agent-session-id", threadID); err != nil {
			return Agent{}, err
		}
	} else if !validManagedSession(observed.Session) || observed.Session.Value != threadID {
		return Agent{}, ErrManagedStale
	}
	first, err := l.getStartedAgent(ctx, target, cwd, false, threadID)
	expected := observed
	expected.Session = first.Session
	if err != nil || !sameImmutable(expected, first) {
		if err == nil {
			err = ErrManagedStale
		}
		return Agent{}, err
	}
	second, err := l.getStartedAgent(ctx, target, cwd, false, threadID)
	if err != nil || !sameImmutable(first, second) || first.Session == nil || second.Session == nil || *first.Session != *second.Session {
		return Agent{}, ErrManagedStale
	}
	return second, nil
}

func (l ManagedCodexLauncher) awaitStatusDraft(ctx context.Context, target sessionhost.Origin, cwd string, argv []string, expected Agent) error {
	seenDraft := false
	for {
		wire, current, err := l.readManagedAgent(ctx, target, cwd)
		if err != nil || wire.Focused || wire.LaunchPending || !managedPaneQuiescent(wire) {
			if err == nil {
				err = ErrManagedPaneBusy
			}
			return err
		}
		if !sameImmutable(expected, current) || expected.Status != current.Status {
			return ErrManagedStale
		}
		if _, err := l.topologyHost().VerifyForegroundProcess(ctx, target, cwd, argv); err != nil {
			return err
		}
		visible, err := l.readPaneANSI(ctx, target.PaneID, "visible", managedStatusLines)
		if err != nil {
			return err
		}
		if codexStartupLoading(visible) {
			return ErrManagedPaneBusy
		}
		if exactCodexSingleLineDraft(visible, "/status") {
			if seenDraft {
				return nil
			}
			seenDraft = true
		} else if !emptyCodexComposer(visible) {
			return ErrManagedStale
		} else {
			seenDraft = false
		}
		if err := waitManaged(ctx, 25*time.Millisecond); err != nil {
			return errors.Join(ErrManagedStale, err)
		}
	}
}

func exactCodexSingleLineDraft(text, want string) bool {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		plain := strings.TrimSpace(managedANSI.ReplaceAllString(lines[i], ""))
		if !strings.HasPrefix(plain, "›") {
			continue
		}
		if strings.TrimSpace(strings.TrimPrefix(plain, "›")) != want {
			return false
		}
		var rawTail []string
		var plainTail []string
		for _, trailing := range lines[i+1:] {
			plainTrailing := strings.TrimSpace(managedANSI.ReplaceAllString(trailing, ""))
			if plainTrailing != "" {
				rawTail = append(rawTail, trailing)
				plainTail = append(plainTail, plainTrailing)
			}
		}
		if len(plainTail) == 0 {
			return true
		}
		// The only supported content below this single-line composer is the
		// exact Codex 0.154.0 styled autocomplete block observed for /status.
		// A blank row alone is not a boundary: blank-line-separated user input
		// remains ambiguous and therefore fails closed.
		menu := strings.Join(strings.Fields(strings.Join(plainTail, " ")), " ")
		rawMenu := strings.Join(rawTail, "\n")
		fullMenu := "/status show current session configuration and token usage /statusline configure which items appear in the status line"
		compactMenu := "/status show current session configuration and token usage"
		if want != "/status" || menu != fullMenu && menu != compactMenu {
			return false
		}
		if strings.Contains(rawMenu, "\x1b[1m\x1b[38;5;6m/status") {
			return true
		}
		if menu == compactMenu {
			return false
		}
		return validUnselectedStatusAutocomplete(rawTail, plainTail)
	}
	return false
}

func validUnselectedStatusAutocomplete(rawTail, plainTail []string) bool {
	const styledStatusline = "/\x1b[0m\x1b[1mstatus\x1b[0mline"
	styledStart := -1
	for index, line := range rawTail {
		if strings.Contains(line, styledStatusline) {
			styledStart = index
			break
		}
	}
	if styledStart <= 0 {
		return false
	}
	for _, line := range rawTail[:styledStart] {
		if strings.ContainsRune(line, '\x1b') {
			return false
		}
	}
	first := strings.Join(strings.Fields(strings.Join(plainTail[:styledStart], " ")), " ")
	second := strings.Join(strings.Fields(strings.Join(plainTail[styledStart:], " ")), " ")
	styled := strings.Join(rawTail[styledStart:], "\n")
	return first == "/status show current session configuration and token usage" &&
		second == "/statusline configure which items appear in the status line" &&
		strings.Contains(styled, "\x1b[2mconfigure")
}

func (l ManagedCodexLauncher) readManagedAgent(ctx context.Context, target sessionhost.Origin, cwd string) (wireAgent, Agent, error) {
	var response agentInfoResponse
	if err := l.Host.runJSON(ctx, []string{"agent", "get", target.PaneID}, &response); err != nil {
		return wireAgent{}, Agent{}, err
	}
	if response.ID != "cli:agent:get" || response.Result.Type != "agent_info" {
		return wireAgent{}, Agent{}, ErrManagedStale
	}
	agent, err := response.Result.Agent.public()
	if err != nil {
		return wireAgent{}, Agent{}, err
	}
	agent.SessionName = l.Host.Session
	if !strings.EqualFold(agent.Kind, "codex") || agent.SessionName != target.SessionName || agent.WorkspaceID != target.WorkspaceID || agent.TabID != target.TabID || agent.PaneID != target.PaneID || agent.CWD != cwd {
		return wireAgent{}, Agent{}, ErrManagedStale
	}
	return response.Result.Agent, agent, nil
}

func validManagedSession(session *SessionRef) bool {
	return session != nil && session.Source == "herdr:codex" && session.Agent == "codex" && session.Kind == "id" && validManagedThreadID(session.Value)
}

func validManagedThreadID(id string) bool {
	return id != "" && managedUUID.FindString(id) == id
}

func validManagedThreadPrefix(prefix string) bool {
	return managedThreadPart.MatchString(prefix)
}

func managedWireThreadPrefix(wire wireAgent) string {
	match := managedTitlePart.FindStringSubmatch(wire.TerminalTitleStripped)
	if len(match) != 2 || !validManagedThreadPrefix(match[1]) {
		return ""
	}
	return match[1]
}

func (l ManagedCodexLauncher) readPaneExtent(ctx context.Context, target sessionhost.Origin) (int, error) {
	var response struct {
		ID     string `json:"id"`
		Result struct {
			Type string          `json:"type"`
			Pane json.RawMessage `json:"pane"`
		} `json:"result"`
	}
	if err := l.Host.runJSON(ctx, []string{"pane", "get", target.PaneID}, &response); err != nil {
		return 0, err
	}
	var pane struct {
		PaneID      string `json:"pane_id"`
		WorkspaceID string `json:"workspace_id"`
		TabID       string `json:"tab_id"`
		Focused     bool   `json:"focused"`
		Scroll      struct {
			MaxOffsetFromBottom int `json:"max_offset_from_bottom"`
			ViewportRows        int `json:"viewport_rows"`
		} `json:"scroll"`
	}
	if response.ID != "cli:pane:get" || response.Result.Type != "pane_info" || json.Unmarshal(response.Result.Pane, &pane) != nil || pane.PaneID != target.PaneID || pane.WorkspaceID != target.WorkspaceID || pane.TabID != target.TabID || pane.Focused || pane.Scroll.MaxOffsetFromBottom < 0 || pane.Scroll.ViewportRows <= 0 {
		return 0, ErrManagedStale
	}
	return pane.Scroll.MaxOffsetFromBottom + pane.Scroll.ViewportRows, nil
}

func (l ManagedCodexLauncher) readFullPaneHistory(ctx context.Context, target sessionhost.Origin) (string, error) {
	lines, err := l.readPaneExtent(ctx, target)
	if err != nil {
		return "", err
	}
	return l.readPaneText(ctx, target.PaneID, "recent-unwrapped", lines)
}

func (l ManagedCodexLauncher) readPaneText(ctx context.Context, paneID, source string, lines int) (string, error) {
	return l.readPane(ctx, paneID, source, lines, "text")
}

func (l ManagedCodexLauncher) readPaneANSI(ctx context.Context, paneID, source string, lines int) (string, error) {
	return l.readPane(ctx, paneID, source, lines, "ansi")
}

func (l ManagedCodexLauncher) readPane(ctx context.Context, paneID, source string, lines int, format string) (string, error) {
	args, err := l.Host.args([]string{"pane", "read", paneID, "--source", source, "--lines", strconv.Itoa(lines), "--format", "text"})
	if err != nil {
		return "", err
	}
	args[len(args)-1] = format
	result, err := herdrcmd.Run(ctx, l.Host.Runner, l.Host.Herdr, args)
	if err != nil {
		return "", err
	}
	if len(result.Stdout) > managedPaneBytes || !utf8.Valid(result.Stdout) {
		return "", ErrManagedStale
	}
	return string(result.Stdout), nil
}

func (l ManagedCodexLauncher) runHerdr(ctx context.Context, args ...string) error {
	commandArgs, err := l.Host.args(args)
	if err != nil {
		return err
	}
	result, err := herdrcmd.Run(ctx, l.Host.Runner, l.Host.Herdr, commandArgs)
	if err != nil {
		return err
	}
	if len(result.Stdout) != 0 || len(result.Stderr) != 0 {
		return ErrManagedStale
	}
	return nil
}

func (l ManagedCodexLauncher) awaitFreshStatus(ctx context.Context, target sessionhost.Origin, before string, cwd string, service CodexServiceSnapshot, policy ManagedCodexPolicy) (string, error) {
	observed, err := l.awaitFreshStatusObservation(ctx, target, before, cwd, service, policy, false, "")
	return observed.identity, err
}

func (l ManagedCodexLauncher) awaitFreshStatusObservation(ctx context.Context, target sessionhost.Origin, before string, cwd string, service CodexServiceSnapshot, policy ManagedCodexPolicy, allowPrefix bool, fallbackPrefix string) (managedStatusObservation, error) {
	return l.awaitFreshStatusObservationMode(ctx, target, before, cwd, service, policy, allowPrefix, fallbackPrefix, false)
}

func (l ManagedCodexLauncher) awaitFreshStatusObservationMode(ctx context.Context, target sessionhost.Origin, before string, cwd string, service CodexServiceSnapshot, policy ManagedCodexPolicy, allowPrefix bool, fallbackPrefix string, allowLocal bool) (managedStatusObservation, error) {
	beforeCommands := managedStatusCmd.FindAllStringIndex(strings.ReplaceAll(before, "\r\n", "\n"), -1)
	incomplete := false
	for {
		text, err := l.readFullPaneHistory(ctx, target)
		if err != nil {
			return managedStatusObservation{}, err
		}
		normalized := strings.ReplaceAll(text, "\r\n", "\n")
		commands := managedStatusCmd.FindAllStringIndex(normalized, -1)
		if len(commands) > len(beforeCommands)+1 {
			return managedStatusObservation{}, ErrManagedAmbiguous
		}
		if len(commands) == len(beforeCommands)+1 {
			fresh := "/status" + normalized[commands[len(commands)-1][1]:]
			observed, err := parseManagedStatusObservationMode(fresh, cwd, service, policy, allowPrefix, fallbackPrefix, allowLocal)
			if err != nil {
				// The submitted command is visible before Codex finishes drawing
				// the status block. Wait only while no Session field has appeared;
				// once it appears, malformed or ambiguous content fails closed.
				if !errors.Is(err, ErrManagedNarrow) || strings.Contains(fresh, "Session:") {
					return managedStatusObservation{}, err
				}
				incomplete = true
			} else {
				visible, err := l.readPaneANSI(ctx, target.PaneID, "visible", managedStatusLines)
				if err != nil || !emptyCodexComposer(visible) {
					if err == nil {
						err = ErrManagedStale
					}
					return managedStatusObservation{}, err
				}
				return observed, nil
			}
		}
		if err := waitManaged(ctx, 25*time.Millisecond); err != nil {
			if incomplete {
				return managedStatusObservation{}, errors.Join(ErrManagedNarrow, err)
			}
			return managedStatusObservation{}, errors.Join(ErrManagedStale, err)
		}
	}
}

// Codex 0.154.0 renders its empty composer before it can execute slash commands.
// Keep that startup state separate from the shared draft-emptiness predicate.
func codexStartupLoading(text string) bool {
	for _, line := range strings.Split(managedANSI.ReplaceAllString(text, ""), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "│") {
			continue
		}
		fields := strings.Fields(strings.Trim(line, "│"))
		if len(fields) >= 2 && fields[0] == "model:" && fields[1] == "loading" {
			return true
		}
	}
	return false
}

func emptyCodexComposer(text string) bool {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		plain := strings.TrimSpace(managedANSI.ReplaceAllString(lines[i], ""))
		if plain != "› Ask Codex to do anything" {
			continue
		}
		placeholder := strings.Index(lines[i], "\x1b[2mAsk Codex to do anything")
		if placeholder < 0 {
			return false
		}
		unstyledFooter := false
		for _, trailing := range lines[i+1:] {
			if strings.TrimSpace(managedANSI.ReplaceAllString(trailing, "")) == "" {
				continue
			}
			if strings.Contains(trailing, "\x1b[2m") {
				continue
			}
			if unstyledFooter || !unstyledCodexStatusFooter(trailing) {
				return false
			}
			unstyledFooter = true
		}
		return true
	}
	return false
}

func unstyledCodexStatusFooter(line string) bool {
	if strings.ContainsRune(line, '\x1b') || !strings.HasPrefix(line, "  ") {
		return false
	}
	parts := strings.Split(strings.TrimSpace(line), " · ")
	if len(parts) < 2 {
		return false
	}
	model := strings.Fields(parts[0])
	if len(model) != 2 || !(parts[1] == "~" || strings.HasPrefix(parts[1], "/") || strings.HasPrefix(parts[1], "~/")) {
		return false
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return false
		}
	}
	return true
}

func codexComposer(text string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "›") {
			return strings.TrimSpace(strings.TrimPrefix(line, "›")), true
		}
	}
	return "", false
}

func parseManagedStatus(text, cwd string, service CodexServiceSnapshot, policy ManagedCodexPolicy) (string, error) {
	observed, err := parseManagedStatusObservation(text, cwd, service, policy, false, "")
	return observed.identity, err
}

func parseManagedStatusObservation(text, cwd string, service CodexServiceSnapshot, policy ManagedCodexPolicy, allowPrefix bool, fallbackPrefix string) (managedStatusObservation, error) {
	return parseManagedStatusObservationMode(text, cwd, service, policy, allowPrefix, fallbackPrefix, false)
}

func parseManagedStatusObservationMode(text, cwd string, service CodexServiceSnapshot, policy ManagedCodexPolicy, allowPrefix bool, fallbackPrefix string, allowLocal bool) (managedStatusObservation, error) {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	marker := strings.LastIndex(normalized, "\n/status\n")
	if marker == -1 {
		if !strings.HasPrefix(normalized, "/status\n") {
			return managedStatusObservation{}, ErrManagedNarrow
		}
		marker = 0
	}
	block := normalized[marker:]
	sessionFields := managedStatusFields(block, "Session")
	if len(sessionFields) > 1 {
		return managedStatusObservation{}, ErrManagedAmbiguous
	}
	partialSessions := managedSessionPart.FindAllStringSubmatch(block, 2)
	if len(partialSessions) > 1 {
		return managedStatusObservation{}, ErrManagedAmbiguous
	}
	identity := ""
	if len(partialSessions) == 1 && validManagedThreadID(partialSessions[0][1]) {
		identity = partialSessions[0][1]
	} else if allowPrefix && len(partialSessions) == 1 && validManagedStatusThreadPrefix(partialSessions[0][1]) {
		identity = partialSessions[0][1]
	} else if allowPrefix && len(partialSessions) == 0 && validManagedStatusFallback(block, fallbackPrefix) {
		if len(sessionFields) == 1 && !validManagedDisplayedSessionPrefix(sessionFields[0], fallbackPrefix) {
			return managedStatusObservation{}, ErrManagedStale
		}
		identity = fallbackPrefix
	} else {
		return managedStatusObservation{}, ErrManagedNarrow
	}
	partialForked := managedForkedPart.FindAllStringSubmatch(block, 2)
	if len(partialForked) > 1 {
		return managedStatusObservation{}, ErrManagedAmbiguous
	}
	forkedFromPrefix := ""
	if len(partialForked) == 1 {
		forkedFromPrefix = partialForked[0][1]
	}
	wantUUIDs := 0
	if validManagedThreadID(identity) {
		wantUUIDs++
	}
	if validManagedThreadID(forkedFromPrefix) {
		wantUUIDs++
	}
	if len(managedUUID.FindAllString(block, 3)) != wantUUIDs {
		return managedStatusObservation{}, ErrManagedAmbiguous
	}
	if fallbackPrefix != "" && !(strings.HasPrefix(identity, fallbackPrefix) || strings.HasPrefix(fallbackPrefix, identity)) {
		return managedStatusObservation{}, ErrManagedStale
	}
	compact := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '│' || r == '╭' || r == '╮' || r == '╰' || r == '╯' || r == '─' {
			return -1
		}
		return r
	}, block)
	remoteCount := strings.Count(compact, "Remote:")
	if service == (CodexServiceSnapshot{}) {
		if remoteCount != 0 {
			return managedStatusObservation{}, ErrManagedStale
		}
	} else {
		if !validManagedServiceSnapshot(service) {
			return managedStatusObservation{}, ErrManagedNarrow
		}
		remote := ""
		if remoteCount != 0 {
			_, afterRemote, _ := strings.Cut(compact, "Remote:")
			var foundModel bool
			remote, _, foundModel = strings.Cut(afterRemote, "Model:")
			if !foundModel {
				return managedStatusObservation{}, ErrManagedNarrow
			}
		}
		wantRemote := "unix://" + service.SocketPath + "(v" + SupportedCodexVersion + ")"
		if remoteCount == 0 {
			if !allowLocal {
				return managedStatusObservation{}, ErrManagedNarrow
			}
		} else if remoteCount != 1 || remote != wantRemote {
			return managedStatusObservation{}, ErrManagedNarrow
		}
	}
	displayCWD := cwd
	if home, err := os.UserHomeDir(); err == nil && (cwd == home || strings.HasPrefix(cwd, home+string(filepath.Separator))) {
		displayCWD = "~" + strings.TrimPrefix(cwd, home)
	}
	if !strings.Contains(compact, "OpenAICodex(v"+SupportedCodexVersion+")") ||
		!managedStatusDirectoryMatches(managedStatusField(block, "Directory"), displayCWD) {
		return managedStatusObservation{}, ErrManagedNarrow
	}
	if len(partialSessions) == 1 && !strings.Contains(compact, "Session:"+identity) {
		return managedStatusObservation{}, ErrManagedNarrow
	}
	models := managedStatusModel.FindAllStringSubmatch(compact, -1)
	observedPolicy := ManagedCodexPolicy{}
	policyIsPrefix := false
	if len(models) == 1 {
		observedPolicy = ManagedCodexPolicy{Model: models[0][1], ReasoningEffort: models[0][2]}
	} else if len(models) == 0 {
		var ok bool
		observedPolicy, policyIsPrefix, ok = managedTruncatedPolicy(managedStatusField(block, "Model"), policy)
		if !ok {
			return managedStatusObservation{}, ErrManagedNarrow
		}
	} else {
		return managedStatusObservation{}, ErrManagedNarrow
	}
	if policy != (ManagedCodexPolicy{}) && (observedPolicy.Model != policy.Model || observedPolicy.ReasoningEffort != policy.ReasoningEffort) {
		return managedStatusObservation{}, ErrManagedNarrow
	}
	return managedStatusObservation{identity: identity, forkedFromPrefix: forkedFromPrefix, policy: observedPolicy, policyIsPrefix: policyIsPrefix, fromStatus: true}, nil
}

func validManagedStatusFallback(block, prefix string) bool {
	if !validManagedThreadPrefix(prefix) {
		return false
	}
	if !strings.Contains(block, "╰") {
		return false
	}
	return strings.Contains(block, "╯")
}

func validManagedDisplayedSessionPrefix(displayed, expected string) bool {
	return managedDisplayPart.MatchString(displayed) &&
		(strings.HasPrefix(expected, displayed) || strings.HasPrefix(displayed, expected))
}

func validManagedStatusThreadPrefix(prefix string) bool {
	return managedDisplayPart.MatchString(prefix) && !validManagedThreadID(prefix)
}

func managedStatusDirectoryMatches(displayed, expected string) bool {
	if displayed == expected {
		return true
	}
	suffix := strings.TrimPrefix(displayed, "…")
	return suffix != displayed && strings.HasPrefix(suffix, string(filepath.Separator)) && strings.HasSuffix(expected, suffix)
}

func managedTruncatedPolicy(displayed string, expected ManagedCodexPolicy) (ManagedCodexPolicy, bool, bool) {
	if expected == (ManagedCodexPolicy{}) {
		const suffix = " (reasoning med"
		model := strings.TrimSuffix(displayed, suffix)
		validModel := model != displayed && model != "" && strings.IndexFunc(model, invalidManagedModelRune) == -1
		return ManagedCodexPolicy{Model: model, ReasoningEffort: "med"}, true, validModel
	}
	prefix := expected.Model + " (reasoning "
	if !strings.HasPrefix(displayed, prefix) {
		return ManagedCodexPolicy{}, false, false
	}
	reasoning := strings.TrimPrefix(displayed, prefix)
	return expected, false, len(reasoning) >= 3 && strings.HasPrefix(expected.ReasoningEffort, reasoning)
}

func managedStatusField(block, label string) string {
	values := managedStatusFields(block, label)
	if len(values) != 1 {
		return ""
	}
	return values[0]
}

func managedStatusFields(block, label string) []string {
	prefix := label + ":"
	var values []string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(managedANSI.ReplaceAllString(line, ""))
		line = strings.TrimSpace(strings.TrimPrefix(line, "│"))
		line = strings.TrimSpace(strings.TrimSuffix(line, "│"))
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		values = append(values, strings.TrimSpace(strings.TrimPrefix(line, prefix)))
	}
	return values
}

func invalidManagedModelRune(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:/-", r))
}
