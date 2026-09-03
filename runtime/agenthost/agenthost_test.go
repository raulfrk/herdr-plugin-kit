package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"pgregory.net/rapid"
)

type scriptedRunner struct {
	mu      sync.Mutex
	results []command.Result
	calls   [][]string
}

func (r *scriptedRunner) Run(_ context.Context, exe string, args []string) (command.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{exe}, args...))
	if len(r.results) == 0 {
		return command.Result{}, errors.New("unexpected command")
	}
	v := r.results[0]
	r.results = r.results[1:]
	return v, nil
}

func TestListExportsKnownFieldsPreservesOrderAndNamedSession(t *testing.T) {
	a, b := validWireAgent("w1:p1"), validWireAgent("w1:p2")
	a.Name = "one"
	a.DisplayAgent = "display"
	a.Title = "title"
	a.CWD = "/cwd"
	a.ForegroundCWD = "/foreground"
	a.AgentSession = &wireSession{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: "session-1"}
	r := &scriptedRunner{results: []command.Result{jsonResult(t, listResponse(a, b))}}
	got, err := (Host{Runner: r, Herdr: "/bin/herdr", Session: "named"}).List(context.Background())
	if err != nil || len(got) != 2 || got[0].Name != "one" || got[0].DisplayName != "display" || got[0].Session.Value != "session-1" || got[1].PaneID != "w1:p2" {
		t.Fatalf("agents=%+v error=%v", got, err)
	}
	want := [][]string{{"/bin/herdr", "--session", "named", "agent", "list"}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls=%q", r.calls)
	}
}
func TestFocusValidatesCompleteIdentity(t *testing.T) {
	w := validWireAgent("w1:p2")
	w.AgentSession = &wireSession{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: "session-1"}
	target := mustPublic(t, w)
	r := &scriptedRunner{results: []command.Result{jsonResult(t, infoResponse(w))}}
	got, err := (Host{Runner: r}).Focus(context.Background(), target)
	if err != nil || got.PaneID != target.PaneID {
		t.Fatalf("agent=%+v error=%v", got, err)
	}
	w.WorkspaceID = "replacement"
	r = &scriptedRunner{results: []command.Result{jsonResult(t, infoResponse(w))}}
	if _, err := (Host{Runner: r}).Focus(context.Background(), target); err == nil {
		t.Fatal("pane replacement accepted")
	}
	w.WorkspaceID = target.WorkspaceID
	w.AgentSession.Value = "session-2"
	r = &scriptedRunner{results: []command.Result{jsonResult(t, infoResponse(w))}}
	if _, err := (Host{Runner: r}).Focus(context.Background(), target); err == nil {
		t.Fatal("session replacement accepted")
	}
}
func TestClassifyTerminalFirstAndHostFallback(t *testing.T) {
	tests := []struct {
		name, kind, host, text string
		status                 Status
		reason                 Reason
	}{
		{"queue beats question", "codex", "blocked", "queued follow-up inputs: would you like this?", Working, Queue},
		{"question", "codex", "working", "Please choose one [y/n] · esc to cancel", Blocked, Question},
		{"narrow background wait", "codex", "idle", "background termin… running", Working, TerminalWait},
		{"stale done ready composer", "codex", "done", "Ask Codex", Idle, Composer},
		{"stale blocked ready composer", "codex", "blocked", "› Ask Codex", Idle, Composer},
		{"empty detection is not a composer", "codex", "idle", "", Unknown, Unrecognized},
		{"idle without composer", "codex", "idle", "ordinary completed output", Unknown, Unrecognized},
		{"done fallback", "codex", "done", "ordinary completed output", Done, HostReport},
		{"mixed agent host report", "claude", "working", "Ask Codex", Working, HostReport},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := mustPublic(t, validWireAgent("w:p"))
			a.Kind, a.Status = tc.kind, tc.host
			s, r := classify(a, tc.text)
			if s != tc.status || r != tc.reason {
				t.Fatalf("got %s/%s", s, r)
			}
		})
	}
}
func TestChangingTextSameClassificationAndStableStatusSeqSucceeds(t *testing.T) {
	a := validWireAgent("w:p")
	results := observationResults(t, a, "running command alpha", a, "running command beta")
	runner := &scriptedRunner{results: results}
	got, err := (Probe{Host: Host{Runner: runner}, Settle: time.Nanosecond}).Assess(context.Background(), mustPublic(t, a))
	if err != nil || !got.Stable || got.Status != Working || got.Reason != TerminalWait {
		t.Fatalf("assessment=%+v error=%v", got, err)
	}
	if len(runner.calls) != 6 || !reflect.DeepEqual(runner.calls[0][1:], []string{"agent", "list"}) || !reflect.DeepEqual(runner.calls[1][1:], []string{"pane", "read", "w:p", "--source", "detection", "--lines", "60", "--format", "text"}) {
		t.Fatalf("observation calls=%q", runner.calls)
	}
}
func TestConstantTextChangingSeqRejects(t *testing.T) {
	a, b := validWireAgent("w:p"), validWireAgent("w:p")
	b.StateChangeSeq++
	results := append(oneObservation(t, a, a, "Ask Codex"), oneObservation(t, b, b, "Ask Codex")...)
	got, err := (Probe{Host: Host{Runner: &scriptedRunner{results: results}}, Settle: time.Nanosecond}).Assess(context.Background(), mustPublic(t, a))
	if !errors.Is(err, ErrStaleReport) || got.Stable {
		t.Fatalf("assessment=%+v error=%v", got, err)
	}
}
func TestRevisionMayChangeBetweenCoherentObservations(t *testing.T) {
	a, b := validWireAgent("w:p"), validWireAgent("w:p")
	b.Revision++
	results := append(oneObservation(t, a, a, "Ask Codex"), oneObservation(t, b, b, "Ask Codex")...)
	got, err := (Probe{Host: Host{Runner: &scriptedRunner{results: results}}, Settle: time.Nanosecond}).Assess(context.Background(), mustPublic(t, a))
	if err != nil || !got.Stable {
		t.Fatalf("assessment=%+v error=%v", got, err)
	}
}
func TestWithinObservationRevisionChangeRejects(t *testing.T) {
	a, b := validWireAgent("w:p"), validWireAgent("w:p")
	b.Revision++
	r := &scriptedRunner{results: oneObservation(t, a, b, "Ask Codex")}
	_, err := (Probe{Host: Host{Runner: r}, Settle: time.Nanosecond}).Assess(context.Background(), mustPublic(t, a))
	if !errors.Is(err, ErrStaleReport) {
		t.Fatalf("error=%v", err)
	}
}
func TestFirstObservationRejectsEachStaleTargetVersionField(t *testing.T) {
	current := validWireAgent("w:p")
	for _, tc := range []struct {
		name   string
		mutate func(*Agent)
	}{
		{"status", func(a *Agent) { a.Status = "working" }},
		{"revision", func(a *Agent) { a.Revision-- }},
		{"state change sequence", func(a *Agent) { a.StateChangeSeq-- }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := mustPublic(t, current)
			tc.mutate(&target)
			runner := &scriptedRunner{results: []command.Result{jsonResult(t, listResponse(current))}}
			got, err := (Probe{Host: Host{Runner: runner}, Settle: time.Nanosecond}).Assess(context.Background(), target)
			if !errors.Is(err, ErrStaleReport) || got != (Assessment{}) || len(runner.calls) != 1 {
				t.Fatalf("assessment=%+v error=%v calls=%q", got, err, runner.calls)
			}
		})
	}
}
func TestPaneReplacementAndStaleTargetReject(t *testing.T) {
	a, b := validWireAgent("w:p"), validWireAgent("w:p")
	b.TabID = "other"
	for _, results := range [][]command.Result{oneObservation(t, a, b, "Ask Codex"), oneObservation(t, b, b, "Ask Codex")} {
		r := &scriptedRunner{results: results}
		_, err := (Probe{Host: Host{Runner: r}, Settle: time.Nanosecond}).Assess(context.Background(), mustPublic(t, a))
		if !errors.Is(err, ErrStaleReport) {
			t.Fatalf("error=%v", err)
		}
	}
}

func TestNamedSessionIdentityCannotCrossHosts(t *testing.T) {
	a := mustPublic(t, validWireAgent("w:p"))
	a.SessionName = "alpha"
	runner := &scriptedRunner{}
	if _, err := (Host{Runner: runner, Session: "beta"}).Focus(context.Background(), a); err == nil {
		t.Fatal("cross-session focus target accepted")
	}
	if _, err := (Probe{Host: Host{Runner: runner, Session: "beta"}}).Assess(context.Background(), a); err == nil {
		t.Fatal("cross-session assessment target accepted")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("cross-session target executed %d commands", len(runner.calls))
	}
}
func TestCancellationDuringSettle(t *testing.T) {
	a := validWireAgent("w:p")
	r := &scriptedRunner{results: oneObservation(t, a, a, "Ask Codex")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (Probe{Host: Host{Runner: r}, Settle: time.Hour}).Assess(ctx, mustPublic(t, a))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}
func TestDuplicatePaneIDsReject(t *testing.T) {
	a := validWireAgent("w:p")
	r := &scriptedRunner{results: []command.Result{jsonResult(t, listResponse(a, a))}}
	if _, err := (Host{Runner: r}).List(context.Background()); err == nil {
		t.Fatal("duplicate accepted")
	}
}
func TestListRejectsUnsupportedStatusAndOversizedFields(t *testing.T) {
	for _, mutate := range []func(*wireAgent){func(a *wireAgent) { a.AgentStatus = "waiting" }, func(a *wireAgent) { a.Title = strings.Repeat("x", maxIdentityBytes+1) }, func(a *wireAgent) { a.CWD = strings.Repeat("x", maxPathBytes+1) }} {
		a := validWireAgent("w:p")
		mutate(&a)
		if _, err := (Host{Runner: &scriptedRunner{results: []command.Result{jsonResult(t, listResponse(a))}}}).List(context.Background()); err == nil {
			t.Fatal("invalid agent accepted")
		}
	}
}
func TestDefaultSettleAndInvalidHostSession(t *testing.T) {
	if got := (Probe{}).settle(); got != 400*time.Millisecond {
		t.Fatalf("settle=%v", got)
	}
	runner := &scriptedRunner{}
	if _, err := (Host{Runner: runner, Session: "../bad"}).List(context.Background()); err == nil || len(runner.calls) != 0 {
		t.Fatalf("invalid session error=%v calls=%q", err, runner.calls)
	}
}
func TestMalformedOversizedAndTruncatedJSON(t *testing.T) {
	for _, v := range []command.Result{{Stdout: []byte(`{"id":"x","result":`)}, {Stdout: []byte(strings.Repeat(" ", 1<<20+1))}, {StdoutTruncated: true}, {StderrTruncated: true}} {
		if _, err := (Host{Runner: &scriptedRunner{results: []command.Result{v}}}).List(context.Background()); err == nil {
			t.Fatalf("accepted %+v", v)
		}
	}
}
func TestOversizedTruncatedAndInvalidDetection(t *testing.T) {
	a := validWireAgent("w:p")
	for _, v := range []command.Result{{Stdout: []byte(strings.Repeat("x", MaxDetectionBytes+1))}, {Stdout: []byte("x"), StdoutTruncated: true}, {Stdout: []byte{0xff}}} {
		r := &scriptedRunner{results: []command.Result{jsonResult(t, listResponse(a)), v}}
		_, err := (Probe{Host: Host{Runner: r}, Settle: time.Nanosecond}).Assess(context.Background(), mustPublic(t, a))
		if err == nil {
			t.Fatalf("accepted %+v", v)
		}
	}
}
func TestRawDetectionAbsentFromExportedTypes(t *testing.T) {
	a := validWireAgent("w:p")
	secret := "private query 937"
	results := observationResults(t, a, "Ask Codex "+secret, a, "Ask Codex changed "+secret)
	got, err := (Probe{Host: Host{Runner: &scriptedRunner{results: results}}, Settle: time.Nanosecond}).Assess(context.Background(), mustPublic(t, a))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(got)
	if strings.Contains(string(data), secret) {
		t.Fatalf("raw text exported: %s", data)
	}
}
func TestPropertyObservationCoherence(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		revision := rapid.Uint64().Draw(rt, "revision")
		a := validWireAgent("w:p")
		a.Revision = revision
		results := observationResults(t, a, "messages to be submitted at end of turn: one", a, "edit last queued message: two")
		got, err := (Probe{Host: Host{Runner: &scriptedRunner{results: results}}, Settle: time.Nanosecond}).Assess(context.Background(), mustPublic(t, a))
		if err != nil || !got.Stable || got.Status != Working {
			rt.Fatalf("assessment=%+v error=%v", got, err)
		}
	})
}

func TestAssessmentRejectsEveryCrossSampleIdentityAndSemanticChange(t *testing.T) {
	base := validWireAgent("w:p")
	tests := []struct {
		name   string
		mutate func(*wireAgent)
		text   string
		want   Assessment
	}{
		{"kind", func(a *wireAgent) { a.Agent = "claude" }, "Ask Codex", Assessment{}}, {"workspace", func(a *wireAgent) { a.WorkspaceID = "w2" }, "Ask Codex", Assessment{}},
		{"tab", func(a *wireAgent) { a.TabID = "w1:t2" }, "Ask Codex", Assessment{}}, {"pane", func(a *wireAgent) { a.PaneID = "w:p2" }, "Ask Codex", Assessment{}},
		{"host status", func(a *wireAgent) { a.AgentStatus = "working" }, "Ask Codex", Assessment{Status: Unknown, Reason: Unsettled}}, {"semantic", func(*wireAgent) {}, "running command", Assessment{Status: Unknown, Reason: Unsettled}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			second := base
			tc.mutate(&second)
			results := append(oneObservation(t, base, base, "Ask Codex"), oneObservation(t, second, second, tc.text)...)
			got, err := (Probe{Host: Host{Runner: &scriptedRunner{results: results}}, Settle: time.Nanosecond}).Assess(context.Background(), mustPublic(t, base))
			if !errors.Is(err, ErrStaleReport) || got != tc.want {
				t.Fatalf("assessment=%+v error=%v", got, err)
			}
		})
	}
}

func TestSessionIdentityEqualityRequiresPresenceAndAllFields(t *testing.T) {
	base := Agent{SessionName: "named", Kind: "codex", WorkspaceID: "w", TabID: "t", PaneID: "p", Session: &SessionRef{Source: "source", Agent: "codex", Kind: "id", Value: "value"}}
	if !sameImmutable(base, base) {
		t.Fatal("identical identity rejected")
	}
	without := base
	without.Session = nil
	if sameImmutable(base, without) || sameImmutable(without, base) {
		t.Fatal("session presence ignored")
	}
	for _, mutate := range []func(*Agent){func(a *Agent) { a.SessionName = "other" }, func(a *Agent) { a.Session.Source = "other" }, func(a *Agent) { a.Session.Agent = "other" }, func(a *Agent) { a.Session.Kind = "other" }, func(a *Agent) { a.Session.Value = "other" }} {
		changed := base
		session := *base.Session
		changed.Session = &session
		mutate(&changed)
		if sameImmutable(base, changed) {
			t.Fatalf("changed identity matched: %+v", changed)
		}
	}
}

func TestTargetAndSessionValidationBoundaries(t *testing.T) {
	valid := mustPublic(t, validWireAgent("w:p"))
	for _, mutate := range []func(*Agent){func(a *Agent) { a.Kind = "" }, func(a *Agent) { a.WorkspaceID = "" }, func(a *Agent) { a.TabID = "" }, func(a *Agent) { a.Status = "waiting" }, func(a *Agent) { a.PaneID = strings.Repeat("p", maxIdentityBytes+1) }, func(a *Agent) { a.PaneID = "p\nother" }} {
		target := valid
		mutate(&target)
		if validateTarget(target) == nil {
			t.Fatalf("invalid target accepted: %+v", target)
		}
	}
	valid.PaneID = strings.Repeat("p", maxIdentityBytes)
	if err := validateTarget(valid); err != nil {
		t.Fatalf("maximum pane ID rejected: %v", err)
	}
	for _, field := range []string{"source", "agent", "kind", "value"} {
		session := SessionRef{Source: "source", Agent: "agent", Kind: "kind", Value: "value"}
		switch field {
		case "source":
			session.Source = ""
		case "agent":
			session.Agent = ""
		case "kind":
			session.Kind = ""
		case "value":
			session.Value = ""
		}
		if validateSession(session) == nil {
			t.Fatalf("empty %s accepted", field)
		}
	}
	if err := validateBounded("optional", "", 1, true); err != nil {
		t.Fatalf("empty optional rejected: %v", err)
	}
	invalidSessionTarget := mustPublic(t, validWireAgent("w:p"))
	invalidSessionTarget.SessionName = "../other"
	if err := validateTarget(invalidSessionTarget); err == nil {
		t.Fatal("invalid target session name accepted")
	}
}

func TestDetectionAndTailExactBoundaries(t *testing.T) {
	a := validWireAgent("w:p")
	exact := strings.Repeat("x", MaxDetectionBytes)
	results := observationResults(t, a, exact, a, exact)
	got, err := (Probe{Host: Host{Runner: &scriptedRunner{results: results}}, Settle: time.Nanosecond}).Assess(context.Background(), mustPublic(t, a))
	if err != nil || !got.Stable || got.Status != Unknown || got.Reason != Unrecognized {
		t.Fatalf("assessment=%+v error=%v", got, err)
	}
	sixty := make([]string, DetectionLines)
	for i := range sixty {
		sixty[i] = fmt.Sprintf("line-%d", i)
	}
	tail := strings.Split(boundedTail(strings.Join(sixty, "\n")), "\n")
	if len(tail) != DetectionLines || tail[0] != "line-0" {
		t.Fatalf("60-line tail=%q", tail)
	}
	tail = strings.Split(boundedTail(strings.Join(append([]string{"discard"}, sixty...), "\n")), "\n")
	if len(tail) != DetectionLines || tail[0] != "line-0" {
		t.Fatalf("61-line tail=%q", tail)
	}
	twelve := append([]string{"queued follow-up inputs"}, make([]string, classifierLines-1)...)
	if !strings.Contains(normalize(strings.Join(twelve, "\n")), "queued follow-up inputs") {
		t.Fatal("12-line marker lost")
	}
	thirteen := append([]string{"queued follow-up inputs", "ordinary"}, make([]string, classifierLines-1)...)
	if strings.Contains(normalize(strings.Join(thirteen, "\n")), "queued follow-up inputs") {
		t.Fatal("old marker retained")
	}
}

func observationResults(t *testing.T, a wireAgent, textA string, b wireAgent, textB string) []command.Result {
	return append(oneObservation(t, a, a, textA), oneObservation(t, b, b, textB)...)
}
func oneObservation(t *testing.T, before, after wireAgent, text string) []command.Result {
	return []command.Result{jsonResult(t, listResponse(before)), {Stdout: []byte(text)}, jsonResult(t, listResponse(after))}
}
func validWireAgent(pane string) wireAgent {
	return wireAgent{Agent: "codex", AgentStatus: "idle", Focused: true, InteractiveReady: true, PaneID: pane, Revision: 7, StateChangeSeq: 11, TabID: "w1:t1", WorkspaceID: "w1"}
}
func mustPublic(t *testing.T, w wireAgent) Agent {
	t.Helper()
	a, err := w.public()
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func listResponse(agents ...wireAgent) agentListResponse {
	var r agentListResponse
	r.ID, r.Result.Type, r.Result.Agents = "cli:agent:list", "agent_list", agents
	return r
}
func infoResponse(agent wireAgent) agentInfoResponse {
	var r agentInfoResponse
	r.ID, r.Result.Type, r.Result.Agent = "cli:agent:focus", "agent_info", agent
	return r
}
func jsonResult(t *testing.T, v any) command.Result {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return command.Result{Stdout: b}
}
