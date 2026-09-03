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
	maxIdentityBytes  = 256
	maxPathBytes      = 4096
	classifierLines   = 12
)

var ErrStaleReport = errors.New("agent report changed during probe")

type SessionRef struct{ Source, Agent, Kind, Value string }
type Agent struct {
	Kind, Name, DisplayName, Title, Status, CWD, ForegroundCWD string
	WorkspaceID, TabID, PaneID                                 string
	Focused                                                    bool
	Revision, StateChangeSeq                                   uint64
	Session                                                    *SessionRef
}
type Status string

const (
	Working Status = "working"
	Blocked Status = "blocked"
	Done    Status = "done"
	Idle    Status = "idle"
	Unknown Status = "unknown"
)

type Reason string

const (
	Queue        Reason = "codex_queue"
	Question     Reason = "codex_question"
	TerminalWait Reason = "codex_terminal_wait"
	Composer     Reason = "codex_ready_composer"
	HostReport   Reason = "host_report"
)

type Assessment struct {
	Status Status
	Reason Reason
	Stable bool
}
type Host struct {
	Runner         command.Runner
	Herdr, Session string
}
type Probe struct {
	Host   Host
	Settle time.Duration
}

func (h Host) List(ctx context.Context) ([]Agent, error) {
	var response agentListResponse
	if err := h.runJSON(ctx, []string{"agent", "list"}, &response); err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	if response.Result.Type != "agent_list" {
		return nil, fmt.Errorf("list agents: unexpected result %q", response.Result.Type)
	}
	agents := make([]Agent, len(response.Result.Agents))
	seen := make(map[string]struct{}, len(agents))
	for i, w := range response.Result.Agents {
		a, err := w.public()
		if err != nil {
			return nil, fmt.Errorf("list agents: agents[%d]: %w", i, err)
		}
		if _, ok := seen[a.PaneID]; ok {
			return nil, fmt.Errorf("list agents: duplicate pane_id at agents[%d]", i)
		}
		seen[a.PaneID] = struct{}{}
		agents[i] = a
	}
	return agents, nil
}
func (h Host) Focus(ctx context.Context, target Agent) (Agent, error) {
	if err := validateTarget(target); err != nil {
		return Agent{}, fmt.Errorf("focus agent: %w", err)
	}
	var response agentInfoResponse
	if err := h.runJSON(ctx, []string{"agent", "focus", target.PaneID}, &response); err != nil {
		return Agent{}, fmt.Errorf("focus agent: %w", err)
	}
	if response.Result.Type != "agent_info" {
		return Agent{}, fmt.Errorf("focus agent: unexpected result %q", response.Result.Type)
	}
	a, err := response.Result.Agent.public()
	if err != nil {
		return Agent{}, fmt.Errorf("focus agent: %w", err)
	}
	if !a.Focused || !sameImmutable(target, a) {
		return Agent{}, errors.New("focus agent: returned agent identity changed")
	}
	return a, nil
}
func (p Probe) Assess(ctx context.Context, target Agent) (Assessment, error) {
	if err := validateTarget(target); err != nil {
		return Assessment{}, fmt.Errorf("assess agent: %w", err)
	}
	first, err := p.observe(ctx, target, true)
	if err != nil {
		return Assessment{}, fmt.Errorf("assess agent first sample: %w", err)
	}
	if err := wait(ctx, p.settle()); err != nil {
		return Assessment{}, fmt.Errorf("assess agent settle: %w", err)
	}
	second, err := p.observe(ctx, first.agent, false)
	if err != nil {
		return Assessment{}, fmt.Errorf("assess agent second sample: %w", err)
	}
	a := Assessment{Status: second.status, Reason: second.reason}
	a.Stable = sameImmutable(first.agent, second.agent) && first.agent.Status == second.agent.Status && first.agent.StateChangeSeq == second.agent.StateChangeSeq && first.status == second.status && first.reason == second.reason
	if !a.Stable {
		return a, ErrStaleReport
	}
	return a, nil
}

type observation struct {
	agent  Agent
	status Status
	reason Reason
}

func (p Probe) observe(ctx context.Context, target Agent, requireVersion bool) (observation, error) {
	before, err := p.Host.find(ctx, target.PaneID)
	if err != nil {
		return observation{}, err
	}
	if !sameImmutable(target, before) || requireVersion && (target.Status != before.Status || target.Revision != before.Revision || target.StateChangeSeq != before.StateChangeSeq) {
		return observation{}, ErrStaleReport
	}
	screen, err := p.Host.readDetection(ctx, target.PaneID)
	if err != nil {
		return observation{}, err
	}
	after, err := p.Host.find(ctx, target.PaneID)
	if err != nil {
		return observation{}, err
	}
	if !sameImmutable(before, after) || before.Status != after.Status || before.Revision != after.Revision || before.StateChangeSeq != after.StateChangeSeq {
		return observation{}, ErrStaleReport
	}
	status, reason := classify(after, screen)
	return observation{after, status, reason}, nil
}
func (h Host) find(ctx context.Context, pane string) (Agent, error) {
	agents, err := h.List(ctx)
	if err != nil {
		return Agent{}, err
	}
	for _, a := range agents {
		if a.PaneID == pane {
			return a, nil
		}
	}
	return Agent{}, ErrStaleReport
}
func (h Host) readDetection(ctx context.Context, pane string) (string, error) {
	args, err := h.args([]string{"agent", "read", pane, "--source", "detection", "--lines", "60", "--format", "text"})
	if err != nil {
		return "", err
	}
	r, err := herdrcmd.Run(ctx, h.Runner, h.Herdr, args)
	if err != nil {
		return "", fmt.Errorf("read agent detection: %w", err)
	}
	if len(r.Stdout) > MaxDetectionBytes {
		return "", errors.New("read agent detection: output exceeds 64 KiB")
	}
	if !utf8.Valid(r.Stdout) {
		return "", errors.New("read agent detection: output is not UTF-8")
	}
	return boundedTail(string(r.Stdout)), nil
}
func (h Host) runJSON(ctx context.Context, a []string, target any) error {
	args, err := h.args(a)
	if err != nil {
		return err
	}
	return herdrcmd.RunJSON(ctx, h.Runner, h.Herdr, args, target)
}
func (h Host) args(a []string) ([]string, error) {
	if h.Session == "" {
		return a, nil
	}
	if err := herdrid.ValidateSessionName(h.Session); err != nil {
		return nil, err
	}
	return append([]string{"--session", h.Session}, a...), nil
}
func (p Probe) settle() time.Duration {
	if p.Settle <= 0 {
		return DefaultSettle
	}
	return p.Settle
}
func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func classify(a Agent, screen string) (Status, Reason) {
	if !strings.EqualFold(a.Kind, "codex") {
		return hostStatus(a.Status), HostReport
	}
	tail := normalize(screen)
	switch {
	case containsAny(tail, "queued follow-up inputs", "messages to be submitted after next tool call", "messages to be submitted at end of turn", "edit last queued message"):
		return Working, Queue
	case containsAny(tail, "please confirm", "please choose", "please provide", "please select", "please enter", "please approve", "need your input", "needs your input", "requires your input", "do you want", "would you like", "[y/n]", "press enter to confirm", "enter to confirm", "esc to cancel", "approval required"):
		return Blocked, Question
	case containsAny(tail, "background termin", "waiting for terminal", "terminal active", "running command", "command active", "command is active", "process active", "process running", "continuing in background", "still running", "still working"):
		return Working, TerminalWait
	case isReadyComposer(tail):
		return Idle, Composer
	default:
		return hostStatus(a.Status), HostReport
	}
}
func boundedTail(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > DetectionLines {
		lines = lines[len(lines)-DetectionLines:]
	}
	return strings.Join(lines, "\n")
}
func normalize(s string) string {
	lines := strings.Split(boundedTail(s), "\n")
	if len(lines) > classifierLines {
		lines = lines[len(lines)-classifierLines:]
	}
	return strings.ToLower(strings.Join(strings.Fields(strings.Join(lines, "\n")), " "))
}
func containsAny(s string, ms ...string) bool {
	for _, m := range ms {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
func isReadyComposer(s string) bool {
	return s == "" || containsAny(s, "ask codex", "tab to queue", "tab-to-queue") || strings.HasSuffix(s, "›") || strings.HasSuffix(s, ">")
}
func hostStatus(s string) Status {
	switch s {
	case "working":
		return Working
	case "blocked":
		return Blocked
	case "done":
		return Done
	default:
		return Unknown
	}
}
func sameImmutable(a, b Agent) bool {
	return a.Kind == b.Kind && a.WorkspaceID == b.WorkspaceID && a.TabID == b.TabID && a.PaneID == b.PaneID && sameSession(a.Session, b.Session)
}
func sameSession(a, b *SessionRef) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
func validateTarget(a Agent) error {
	if err := validateBounded("pane_id", a.PaneID, maxIdentityBytes, false); err != nil {
		return err
	}
	if a.Kind == "" || a.WorkspaceID == "" || a.TabID == "" || !validHostStatus(a.Status) {
		return errors.New("agent target identity/status is invalid")
	}
	if a.Session != nil {
		return validateSession(*a.Session)
	}
	return nil
}
func validateBounded(name, value string, max int, optional bool) error {
	if value == "" && !optional || len(value) > max || strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}
func validateSession(s SessionRef) error {
	for n, v := range map[string]string{"session source": s.Source, "session agent": s.Agent, "session kind": s.Kind, "session value": s.Value} {
		if err := validateBounded(n, v, maxIdentityBytes, false); err != nil {
			return err
		}
	}
	return nil
}
func validHostStatus(s string) bool {
	switch s {
	case "working", "blocked", "done", "idle", "unknown":
		return true
	}
	return false
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
type wireSession struct {
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Value  string `json:"value"`
}
type wireAgent struct {
	Agent                 string            `json:"agent"`
	AgentSession          *wireSession      `json:"agent_session,omitempty"`
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

func (a wireAgent) public() (Agent, error) {
	fields := map[string]struct {
		v string
		m int
		o bool
	}{"agent": {a.Agent, maxIdentityBytes, false}, "name": {a.Name, maxIdentityBytes, true}, "display_agent": {a.DisplayAgent, maxIdentityBytes, true}, "title": {a.Title, maxIdentityBytes, true}, "cwd": {a.CWD, maxPathBytes, true}, "foreground_cwd": {a.ForegroundCWD, maxPathBytes, true}, "workspace_id": {a.WorkspaceID, maxIdentityBytes, false}, "tab_id": {a.TabID, maxIdentityBytes, false}, "pane_id": {a.PaneID, maxIdentityBytes, false}, "terminal_id": {a.TerminalID, maxIdentityBytes, true}, "terminal_title": {a.TerminalTitle, maxIdentityBytes, true}, "terminal_title_stripped": {a.TerminalTitleStripped, maxIdentityBytes, true}}
	for n, f := range fields {
		if err := validateBounded(n, f.v, f.m, f.o); err != nil {
			return Agent{}, err
		}
	}
	if !validHostStatus(a.AgentStatus) {
		return Agent{}, fmt.Errorf("unsupported agent_status %q", a.AgentStatus)
	}
	var session *SessionRef
	if a.AgentSession != nil {
		s := SessionRef{a.AgentSession.Source, a.AgentSession.Agent, a.AgentSession.Kind, a.AgentSession.Value}
		if err := validateSession(s); err != nil {
			return Agent{}, err
		}
		session = &s
	}
	return Agent{Kind: a.Agent, Name: a.Name, DisplayName: a.DisplayAgent, Title: a.Title, Status: a.AgentStatus, CWD: a.CWD, ForegroundCWD: a.ForegroundCWD, WorkspaceID: a.WorkspaceID, TabID: a.TabID, PaneID: a.PaneID, Focused: a.Focused, Revision: a.Revision, StateChangeSeq: a.StateChangeSeq, Session: session}, nil
}
