// Package agenthost inspects and focuses Herdr 0.8.2 agent panes.
package agenthost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrcmd"
	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrid"
)

const (
	DetectionLines    = 60
	MaxDetectionBytes = 64 << 10
	DefaultSettle     = 400 * time.Millisecond
)

var ErrStaleReport = errors.New("agent report changed during probe")

// SessionRef selects a named Herdr session. A nil Host.Session uses default.
type SessionRef struct {
	Name string
}

type Agent struct {
	Kind           string
	Status         string
	PaneID         string
	WorkspaceID    string
	TabID          string
	Focused        bool
	Interactive    bool
	Revision       uint64
	StateChangeSeq uint64
}

type Activity string

const (
	ActivityWorking Activity = "working"
	ActivityBlocked Activity = "blocked"
	ActivityIdle    Activity = "idle"
	ActivityUnknown Activity = "unknown"
)

type Basis string

const (
	BasisQueue        Basis = "codex_queue"
	BasisQuestion     Basis = "codex_question"
	BasisTerminalWait Basis = "codex_terminal_wait"
	BasisComposer     Basis = "codex_ready_composer"
	BasisHostReport   Basis = "host_report"
)

// Assessment intentionally exports classifications only, never terminal text.
type Assessment struct {
	PaneID         string
	Activity       Activity
	Basis          Basis
	HostStatus     string
	Revision       uint64
	StateChangeSeq uint64
	Samples        uint8
}

type Host struct {
	Runner  command.Runner
	Herdr   string
	Session *SessionRef
	Settle  time.Duration
}

func (h Host) List(ctx context.Context) ([]Agent, error) {
	var response agentListResponse
	if err := h.runJSON(ctx, []string{"agent", "list"}, &response); err != nil {
		return nil, err
	}
	if response.Result.Type != "agent_list" {
		return nil, fmt.Errorf("unexpected agent list result %q", response.Result.Type)
	}
	agents := make([]Agent, len(response.Result.Agents))
	for i, wire := range response.Result.Agents {
		agent, err := wire.public()
		if err != nil {
			return nil, fmt.Errorf("agents[%d]: %w", i, err)
		}
		agents[i] = agent
	}
	return agents, nil
}

func (h Host) Focus(ctx context.Context, paneID string) (Agent, error) {
	if err := validatePaneID(paneID); err != nil {
		return Agent{}, err
	}
	var response agentInfoResponse
	if err := h.runJSON(ctx, []string{"agent", "focus", paneID}, &response); err != nil {
		return Agent{}, err
	}
	if response.Result.Type != "agent_info" {
		return Agent{}, fmt.Errorf("unexpected focus result %q", response.Result.Type)
	}
	agent, err := response.Result.Agent.public()
	if err != nil {
		return Agent{}, err
	}
	if agent.PaneID != paneID || !agent.Focused {
		return Agent{}, errors.New("focused agent identity/status changed")
	}
	return agent, nil
}

func (h Host) Probe(ctx context.Context, paneID string) (Assessment, error) {
	if err := validatePaneID(paneID); err != nil {
		return Assessment{}, err
	}
	before, err := h.agent(ctx, paneID)
	if err != nil {
		return Assessment{}, err
	}
	first, err := h.readDetection(ctx, paneID)
	if err != nil {
		return Assessment{}, err
	}
	if err := wait(ctx, h.settle()); err != nil {
		return Assessment{}, err
	}
	second, err := h.readDetection(ctx, paneID)
	if err != nil {
		return Assessment{}, err
	}
	after, err := h.agent(ctx, paneID)
	if err != nil {
		return Assessment{}, err
	}
	if !coherent(before, after) {
		return Assessment{}, ErrStaleReport
	}
	activity, basis := classify(before, first, second)
	return Assessment{PaneID: paneID, Activity: activity, Basis: basis, HostStatus: before.Status, Revision: before.Revision, StateChangeSeq: before.StateChangeSeq, Samples: 2}, nil
}

func (h Host) agent(ctx context.Context, paneID string) (Agent, error) {
	agents, err := h.List(ctx)
	if err != nil {
		return Agent{}, err
	}
	for _, agent := range agents {
		if agent.PaneID == paneID {
			return agent, nil
		}
	}
	return Agent{}, fmt.Errorf("agent pane %q not found", paneID)
}

func (h Host) readDetection(ctx context.Context, paneID string) (string, error) {
	args, err := h.args([]string{"agent", "read", paneID, "--source", "detection", "--lines", "60", "--format", "text"})
	if err != nil {
		return "", err
	}
	result, err := herdrcmd.Run(ctx, h.Runner, h.Herdr, args)
	if err != nil {
		return "", err
	}
	if len(result.Stdout) > MaxDetectionBytes {
		return "", errors.New("agent detection output exceeds 64 KiB")
	}
	if !utf8.Valid(result.Stdout) {
		return "", errors.New("agent detection output is not UTF-8")
	}
	return string(result.Stdout), nil
}

func (h Host) runJSON(ctx context.Context, commandArgs []string, target any) error {
	args, err := h.args(commandArgs)
	if err != nil {
		return err
	}
	return herdrcmd.RunJSON(ctx, h.Runner, h.Herdr, args, target)
}

func (h Host) args(commandArgs []string) ([]string, error) {
	if h.Session == nil {
		return commandArgs, nil
	}
	if err := herdrid.ValidateSessionName(h.Session.Name); err != nil {
		return nil, err
	}
	args := []string{"--session", h.Session.Name}
	return append(args, commandArgs...), nil
}

func (h Host) settle() time.Duration {
	if h.Settle <= 0 {
		return DefaultSettle
	}
	return h.Settle
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func coherent(a, b Agent) bool {
	return a.Kind == b.Kind && a.PaneID == b.PaneID && a.WorkspaceID == b.WorkspaceID && a.TabID == b.TabID &&
		a.Status == b.Status && a.Revision == b.Revision && a.StateChangeSeq == b.StateChangeSeq
}

func classify(agent Agent, samples ...string) (Activity, Basis) {
	if !strings.EqualFold(agent.Kind, "codex") {
		return hostActivity(agent.Status), BasisHostReport
	}
	for _, candidate := range []struct {
		basis    Basis
		activity Activity
		match    func(string) bool
	}{
		{BasisQueue, ActivityWorking, isQueue},
		{BasisQuestion, ActivityBlocked, isQuestion},
		{BasisTerminalWait, ActivityWorking, isTerminalWait},
		{BasisComposer, ActivityIdle, isReadyComposer},
	} {
		matched := len(samples) > 0
		for _, sample := range samples {
			matched = matched && candidate.match(sample)
		}
		if matched {
			return candidate.activity, candidate.basis
		}
	}
	return hostActivity(agent.Status), BasisHostReport
}

func isQueue(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "queued message") || strings.Contains(text, "message queued") ||
		strings.Contains(text, "queued prompt") || strings.Contains(text, "will be queued")
}

func isQuestion(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "would you like") || strings.Contains(text, "choose an option") ||
		strings.Contains(text, "press enter to confirm") || strings.Contains(text, "do you want to proceed")
}

func isTerminalWait(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "waiting for terminal") || strings.Contains(text, "waiting for command") ||
		strings.Contains(text, "running command") || strings.Contains(text, "esc to interrupt") ||
		strings.Contains(text, "ctrl+c to interrupt")
}

func isReadyComposer(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "›") || line == ">" || strings.HasPrefix(line, "> ") || strings.Contains(strings.ToLower(line), "ask codex") {
			return true
		}
	}
	return false
}

func hostActivity(status string) Activity {
	switch strings.ToLower(status) {
	case "working", "running":
		return ActivityWorking
	case "blocked", "waiting":
		return ActivityBlocked
	case "idle", "ready":
		return ActivityIdle
	default:
		return ActivityUnknown
	}
}

func validatePaneID(value string) error {
	if value == "" || len(value) > 128 || strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("pane_id must be 1-128 bytes without control separators")
	}
	return nil
}

type agentListResponse struct {
	ID     string `json:"id"`
	Result struct {
		Type   string      `json:"type"`
		Agents []wireAgent `json:"agents"`
	} `json:"result"`
}

type agentInfoResponse struct {
	ID     string `json:"id"`
	Result struct {
		Type  string    `json:"type"`
		Agent wireAgent `json:"agent"`
	} `json:"result"`
}

type wireAgent struct {
	Agent                 string            `json:"agent"`
	AgentSession          *agentSession     `json:"agent_session,omitempty"`
	AgentStatus           string            `json:"agent_status"`
	CWD                   string            `json:"cwd,omitempty"`
	DisplayAgent          string            `json:"display_agent,omitempty"`
	Focused               bool              `json:"focused"`
	ForegroundCWD         string            `json:"foreground_cwd,omitempty"`
	InteractiveReady      bool              `json:"interactive_ready,omitempty"`
	Name                  string            `json:"name,omitempty"`
	PaneID                string            `json:"pane_id"`
	Revision              uint64            `json:"revision"`
	StateChangeSeq        uint64            `json:"state_change_seq"`
	TabID                 string            `json:"tab_id"`
	TerminalID            string            `json:"terminal_id,omitempty"`
	TerminalTitle         string            `json:"terminal_title,omitempty"`
	TerminalTitleStripped string            `json:"terminal_title_stripped,omitempty"`
	Title                 string            `json:"title,omitempty"`
	Tokens                map[string]string `json:"tokens,omitempty"`
	WorkspaceID           string            `json:"workspace_id"`
}

type agentSession struct {
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Value  string `json:"value"`
}

func (a wireAgent) public() (Agent, error) {
	if a.Agent == "" || a.AgentStatus == "" || a.PaneID == "" || a.WorkspaceID == "" || a.TabID == "" {
		return Agent{}, errors.New("agent identity/status is incomplete")
	}
	if err := validatePaneID(a.PaneID); err != nil {
		return Agent{}, err
	}
	return Agent{Kind: a.Agent, Status: a.AgentStatus, PaneID: a.PaneID, WorkspaceID: a.WorkspaceID, TabID: a.TabID, Focused: a.Focused, Interactive: a.InteractiveReady, Revision: a.Revision, StateChangeSeq: a.StateChangeSeq}, nil
}
