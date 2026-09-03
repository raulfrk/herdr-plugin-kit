package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

type scriptedRunner struct {
	mu      sync.Mutex
	results []command.Result
	calls   [][]string
}

func (r *scriptedRunner) Run(_ context.Context, executable string, args []string) (command.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{executable}, args...))
	if len(r.results) == 0 {
		return command.Result{}, errors.New("unexpected command")
	}
	result := r.results[0]
	r.results = r.results[1:]
	return result, nil
}

func TestListAndFocusUseNamedSessionAndExactPaneIdentity(t *testing.T) {
	agent := validWireAgent()
	runner := &scriptedRunner{results: []command.Result{jsonResult(t, listResponse(agent)), jsonResult(t, infoResponse(agent))}}
	host := Host{Runner: runner, Herdr: "/bin/herdr", Session: &SessionRef{Name: "named"}}
	agents, err := host.List(context.Background())
	if err != nil || len(agents) != 1 || agents[0].PaneID != agent.PaneID {
		t.Fatalf("agents=%+v error=%v", agents, err)
	}
	focused, err := host.Focus(context.Background(), agent.PaneID)
	if err != nil || focused.PaneID != agent.PaneID {
		t.Fatalf("focused=%+v error=%v", focused, err)
	}
	want := [][]string{
		{"/bin/herdr", "--session", "named", "agent", "list"},
		{"/bin/herdr", "--session", "named", "agent", "focus", "w1:p2"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls=%q want=%q", runner.calls, want)
	}
}

func TestProbeClassifierPrecedenceAndNoRawText(t *testing.T) {
	tests := []struct {
		name, kind, status, terminal string
		wantActivity                 Activity
		wantBasis                    Basis
	}{
		{"queue precedes question", "codex", "idle", "Queued message waiting. Would you like to continue?", ActivityWorking, BasisQueue},
		{"question precedes wait", "codex", "working", "Would you like to continue? esc to interrupt", ActivityBlocked, BasisQuestion},
		{"terminal wait", "codex", "idle", "Running command · esc to interrupt", ActivityWorking, BasisTerminalWait},
		{"ready composer", "codex", "working", "› Ask Codex", ActivityIdle, BasisComposer},
		{"mixed agent host report", "claude", "blocked", "› Ask Codex", ActivityBlocked, BasisHostReport},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := validWireAgent()
			agent.Agent, agent.AgentStatus = tc.kind, tc.status
			runner := probeRunner(t, agent, agent, tc.terminal, tc.terminal)
			got, err := (Host{Runner: runner, Settle: time.Nanosecond}).Probe(context.Background(), agent.PaneID)
			if err != nil || got.Activity != tc.wantActivity || got.Basis != tc.wantBasis || got.Samples != 2 {
				t.Fatalf("assessment=%+v error=%v", got, err)
			}
			encoded, err := json.Marshal(got)
			if err != nil || strings.Contains(string(encoded), tc.terminal) {
				t.Fatalf("assessment exported terminal text: %s (%v)", encoded, err)
			}
		})
	}
}

func TestProbeRejectsStaleReport(t *testing.T) {
	before, after := validWireAgent(), validWireAgent()
	after.StateChangeSeq++
	runner := probeRunner(t, before, after, "›", "›")
	_, err := (Host{Runner: runner, Settle: time.Nanosecond}).Probe(context.Background(), before.PaneID)
	if !errors.Is(err, ErrStaleReport) {
		t.Fatalf("error=%v want ErrStaleReport", err)
	}
}

func TestProbeRejectsOversizeAndTruncatedDetection(t *testing.T) {
	agent := validWireAgent()
	for _, result := range []command.Result{
		{Stdout: []byte(strings.Repeat("x", MaxDetectionBytes+1))},
		{Stdout: []byte("›"), StdoutTruncated: true},
	} {
		runner := &scriptedRunner{results: []command.Result{jsonResult(t, listResponse(agent)), result}}
		if _, err := (Host{Runner: runner, Settle: time.Nanosecond}).Probe(context.Background(), agent.PaneID); err == nil {
			t.Fatal("bounded detection failure accepted")
		}
	}
}

func TestProbeCancellationDuringSettle(t *testing.T) {
	agent := validWireAgent()
	runner := &scriptedRunner{results: []command.Result{jsonResult(t, listResponse(agent)), {Stdout: []byte("›")}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (Host{Runner: runner, Settle: time.Hour}).Probe(ctx, agent.PaneID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}

func TestProbeFallsBackWhenSamplesDisagree(t *testing.T) {
	agent := validWireAgent()
	agent.AgentStatus = "working"
	runner := probeRunner(t, agent, agent, "›", "Running command · esc to interrupt")
	got, err := (Host{Runner: runner, Settle: time.Nanosecond}).Probe(context.Background(), agent.PaneID)
	if err != nil || got.Basis != BasisHostReport || got.Activity != ActivityWorking {
		t.Fatalf("assessment=%+v error=%v", got, err)
	}
}

func TestInvalidSessionAndPaneDoNotExecute(t *testing.T) {
	runner := &scriptedRunner{}
	host := Host{Runner: runner, Session: &SessionRef{Name: "../bad"}}
	if _, err := host.List(context.Background()); err == nil {
		t.Fatal("invalid session accepted")
	}
	if _, err := (Host{Runner: runner}).Focus(context.Background(), "bad\npane"); err == nil {
		t.Fatal("invalid pane accepted")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("executed %d commands", len(runner.calls))
	}
}

func probeRunner(t *testing.T, before, after wireAgent, first, second string) *scriptedRunner {
	t.Helper()
	return &scriptedRunner{results: []command.Result{
		jsonResult(t, listResponse(before)), {Stdout: []byte(first)}, {Stdout: []byte(second)}, jsonResult(t, listResponse(after)),
	}}
}

func validWireAgent() wireAgent {
	return wireAgent{Agent: "codex", AgentStatus: "idle", Focused: true, InteractiveReady: true, PaneID: "w1:p2", Revision: 7, StateChangeSeq: 11, TabID: "w1:t1", WorkspaceID: "w1"}
}

func listResponse(agent wireAgent) agentListResponse {
	var response agentListResponse
	response.ID, response.Result.Type, response.Result.Agents = "cli:agent:list", "agent_list", []wireAgent{agent}
	return response
}

func infoResponse(agent wireAgent) agentInfoResponse {
	var response agentInfoResponse
	response.ID, response.Result.Type, response.Result.Agent = "cli:agent:focus", "agent_info", agent
	return response
}

func jsonResult(t *testing.T, value any) command.Result {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return command.Result{Stdout: data}
}
