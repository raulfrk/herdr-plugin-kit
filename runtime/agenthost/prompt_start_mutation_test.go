package agenthost

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

// These tests run before the other prompt-start tests on purpose: they are the
// ones that fail fast when the identity wait loop stops terminating.

type promptStartListStep struct {
	agents []wireAgent
	err    error
}

// promptStartListRunner answers only `agent list`, one step per call and
// repeating the last step once the script is exhausted.
type promptStartListRunner struct {
	steps []promptStartListStep
	calls int
}

func (r *promptStartListRunner) Run(_ context.Context, _ string, args []string) (command.Result, error) {
	plain := args
	if len(plain) >= 2 && plain[0] == "--session" {
		plain = plain[2:]
	}
	if !reflect.DeepEqual(plain, []string{"agent", "list"}) {
		return command.Result{}, fmt.Errorf("unexpected command %q", args)
	}
	step := r.steps[min(r.calls, len(r.steps)-1)]
	r.calls++
	if step.err != nil {
		return command.Result{}, step.err
	}
	return jsonCommandResult(listResponse(step.agents...))
}

func promptStartCreatedTarget() sessionhost.CreatedTarget {
	return sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
}

func promptStartWire(kind string, session *wireSession) wireAgent {
	return wireAgent{Agent: kind, AgentStatus: "idle", CWD: "/tmp/project", PaneID: "w1:p2", TabID: "w1:t2", WorkspaceID: "w1", AgentSession: session}
}

func promptStartOMPSession() *wireSession {
	return &wireSession{Source: "herdr:omp", Agent: "omp", Kind: "path", Value: "/tmp/new.jsonl"}
}

func promptStartOpenCodeSession() *wireSession {
	return &wireSession{Source: "herdr:opencode", Agent: "opencode", Kind: "id", Value: "ses_created"}
}

func promptStartWithSession(session *wireSession, change func(*wireSession)) *wireSession {
	changed := *session
	change(&changed)
	return &changed
}

func promptStartWaitingStarter(t *testing.T, limit int, steps ...promptStartListStep) (PromptStarter, *promptWaitRecorder, *promptStartListRunner) {
	t.Helper()
	runner := &promptStartListRunner{steps: steps}
	starter := NewPromptStarter(Host{Runner: runner, Session: "named"})
	waits := newPromptWaitRecorder(t, limit)
	starter.wait = waits.wait
	return starter, waits, runner
}

func promptStartExpectedAgent(t *testing.T, wire wireAgent) Agent {
	t.Helper()
	agent := mustPublic(t, wire)
	agent.SessionName = "named"
	return agent
}

func TestPromptStartWaitForAgentReturnsReadyIdentityWithoutWaiting(t *testing.T) {
	for _, test := range []struct {
		name string
		kind string
		wire wireAgent
	}{
		{"OMP", "omp", promptStartWire("omp", promptStartOMPSession())},
		{"OMP kind in another case", "omp", promptStartWire("OMP", promptStartOMPSession())},
		{"OpenCode", "opencode", promptStartWire("opencode", promptStartOpenCodeSession())},
		{"finished OMP", "omp", func() wireAgent {
			wire := promptStartWire("omp", promptStartOMPSession())
			wire.AgentStatus = "done"
			return wire
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			starter, waits, _ := promptStartWaitingStarter(t, 3, promptStartListStep{agents: []wireAgent{test.wire}})
			got, err := starter.waitForAgent(context.Background(), promptStartCreatedTarget(), test.kind, "ses_created", "/tmp/project", time.Minute)
			if err != nil || !reflect.DeepEqual(got, promptStartExpectedAgent(t, test.wire)) || len(waits.calls) != 0 {
				t.Fatalf("agent=%+v waits=%v error=%v", got, waits.calls, err)
			}
		})
	}
}

func TestPromptStartWaitForAgentRejectsEachIdentityDrift(t *testing.T) {
	for _, test := range []struct {
		name  string
		drift func(*sessionhost.CreatedTarget, *wireAgent)
	}{
		{"Herdr session", func(created *sessionhost.CreatedTarget, _ *wireAgent) { created.SessionName = "other" }},
		{"workspace", func(_ *sessionhost.CreatedTarget, agent *wireAgent) { agent.WorkspaceID = "w2" }},
		{"tab", func(_ *sessionhost.CreatedTarget, agent *wireAgent) { agent.TabID = "w1:t9" }},
		{"kind", func(_ *sessionhost.CreatedTarget, agent *wireAgent) { agent.Agent = "claude" }},
		{"directory", func(_ *sessionhost.CreatedTarget, agent *wireAgent) { agent.CWD = "/tmp/other" }},
		{"focus", func(_ *sessionhost.CreatedTarget, agent *wireAgent) { agent.Focused = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			created, wire := promptStartCreatedTarget(), promptStartWire("omp", promptStartOMPSession())
			test.drift(&created, &wire)
			starter, waits, _ := promptStartWaitingStarter(t, 3, promptStartListStep{agents: []wireAgent{wire}})
			got, err := starter.waitForAgent(context.Background(), created, "omp", "", "/tmp/project", time.Minute)
			if !errors.Is(err, ErrPromptStale) || got.PaneID != "" || len(waits.calls) != 0 {
				t.Fatalf("agent=%+v waits=%v error=%v", got, waits.calls, err)
			}
		})
	}
}

func TestPromptStartWaitForAgentRejectsSessionsThatDoNotMatchTheKind(t *testing.T) {
	omp, openCode := promptStartOMPSession(), promptStartOpenCodeSession()
	for _, test := range []struct {
		name    string
		kind    string
		session *wireSession
	}{
		{"OMP from another source", "omp", promptStartWithSession(omp, func(s *wireSession) { s.Source = "herdr:other" })},
		{"OMP for another agent", "omp", promptStartWithSession(omp, func(s *wireSession) { s.Agent = "other" })},
		{"OMP with an ID instead of a path", "omp", promptStartWithSession(omp, func(s *wireSession) { s.Kind = "id" })},
		{"OMP with a relative path", "omp", promptStartWithSession(omp, func(s *wireSession) { s.Value = "relative/new.jsonl" })},
		{"OMP with an OpenCode session", "omp", openCode},
		{"OpenCode from another source", "opencode", promptStartWithSession(openCode, func(s *wireSession) { s.Source = "herdr:other" })},
		{"OpenCode for another agent", "opencode", promptStartWithSession(openCode, func(s *wireSession) { s.Agent = "other" })},
		{"OpenCode with a path instead of an ID", "opencode", promptStartWithSession(openCode, func(s *wireSession) { s.Kind = "path" })},
		{"OpenCode with another conversation", "opencode", promptStartWithSession(openCode, func(s *wireSession) { s.Value = "ses_other" })},
		{"OpenCode with an OMP session", "opencode", omp},
	} {
		t.Run(test.name, func(t *testing.T) {
			wire := promptStartWire(test.kind, test.session)
			starter, waits, _ := promptStartWaitingStarter(t, 3, promptStartListStep{agents: []wireAgent{wire}})
			got, err := starter.waitForAgent(context.Background(), promptStartCreatedTarget(), test.kind, "ses_created", "/tmp/project", time.Minute)
			if !errors.Is(err, ErrPromptStale) || got.PaneID != "" || len(waits.calls) != 0 {
				t.Fatalf("agent=%+v waits=%v error=%v", got, waits.calls, err)
			}
		})
	}
}

func TestPromptStartWaitForAgentKeepsWaitingUntilIdentityIsReady(t *testing.T) {
	ready := promptStartWire("omp", promptStartOMPSession())
	working := ready
	working.AgentStatus = "working"
	for _, test := range []struct {
		name  string
		steps []promptStartListStep
	}{
		{"identity not reported yet", []promptStartListStep{{agents: []wireAgent{promptStartWire("omp", nil)}}, {agents: []wireAgent{ready}}}},
		{"status not ready yet", []promptStartListStep{{agents: []wireAgent{working}}, {agents: []wireAgent{ready}}}},
		{"pane not listed yet", []promptStartListStep{{}, {agents: []wireAgent{ready}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			starter, waits, runner := promptStartWaitingStarter(t, 3, test.steps...)
			got, err := starter.waitForAgent(context.Background(), promptStartCreatedTarget(), "omp", "", "/tmp/project", time.Minute)
			if err != nil || !reflect.DeepEqual(got, promptStartExpectedAgent(t, ready)) || runner.calls != 2 {
				t.Fatalf("agent=%+v lists=%d error=%v", got, runner.calls, err)
			}
			if want := []time.Duration{50 * time.Millisecond}; !reflect.DeepEqual(waits.calls, want) {
				t.Fatalf("waits=%v want=%v", waits.calls, want)
			}
		})
	}
}

func TestPromptStartWaitForAgentStopsOnDiagnosticFailureAndOnWaitFailure(t *testing.T) {
	t.Run("agent list failure is returned without waiting", func(t *testing.T) {
		failure := errors.New("agent list failed")
		starter, waits, runner := promptStartWaitingStarter(t, 3, promptStartListStep{err: failure})
		got, err := starter.waitForAgent(context.Background(), promptStartCreatedTarget(), "omp", "", "/tmp/project", time.Minute)
		if !errors.Is(err, failure) || got.PaneID != "" || runner.calls != 1 || len(waits.calls) != 0 {
			t.Fatalf("agent=%+v lists=%d waits=%v error=%v", got, runner.calls, waits.calls, err)
		}
	})
	t.Run("wait failure ends the loop", func(t *testing.T) {
		starter, waits, runner := promptStartWaitingStarter(t, 2, promptStartListStep{agents: []wireAgent{promptStartWire("omp", nil)}})
		waits.err = context.DeadlineExceeded
		got, err := starter.waitForAgent(context.Background(), promptStartCreatedTarget(), "omp", "", "/tmp/project", time.Minute)
		if !errors.Is(err, context.DeadlineExceeded) || got.PaneID != "" || runner.calls != 1 || len(waits.calls) != 1 {
			t.Fatalf("agent=%+v lists=%d waits=%v error=%v", got, runner.calls, waits.calls, err)
		}
	})
}

func TestPromptStartDerivesOMPStatusSocketWhenOmitted(t *testing.T) {
	created := sessionhost.CreatedTarget{Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}}
	topology := &fakeTopology{created: created}
	wire := promptStartWire("omp", promptStartOMPSession())
	var delivered PromptTarget
	deliveries := 0
	starter := NewPromptStarter(Host{Runner: &promptRunner{agent: wire}})
	starter.launcher = topology
	starter.wait = newPromptWaitRecorder(t, 3).wait
	starter.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
		deliveries++
		delivered = target
		return PromptResult{Target: target, Agent: target.Agent, State: PromptAcknowledged}, nil
	}
	prompt, _ := PreparePrompt("hello")
	result, err := starter.Start(context.Background(), prompt, PromptStartRequest{
		Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"},
		Argv:   []string{"omp"}, Directory: "/tmp/project", Placement: sessionhost.NewTab,
	})
	want, deriveErr := DeriveOMPStatusSocket("/tmp/herdr.sock", created.PaneID)
	if deriveErr != nil {
		t.Fatal(deriveErr)
	}
	if err != nil || result.State != PromptAcknowledged || deliveries != 1 || delivered.OMPStatusSocket != want || len(topology.cleanupCalls) != 0 {
		t.Fatalf("result=%+v deliveries=%d socket=%q want=%q cleanup=%v error=%v", result, deliveries, delivered.OMPStatusSocket, want, topology.cleanupCalls, err)
	}
}

func TestPromptStartValidatesDirectorySocketAndTimeoutBoundaries(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func(*PromptStartRequest)
		invalid bool
	}{
		{"valid request", func(*PromptStartRequest) {}, false},
		{"default timeout", func(request *PromptStartRequest) { request.ReadyTimeout = 0 }, false},
		{"maximum timeout", func(request *PromptStartRequest) { request.ReadyTimeout = MaxForkReadyTimeout }, false},
		{"negative timeout", func(request *PromptStartRequest) { request.ReadyTimeout = -time.Nanosecond }, true},
		{"empty directory", func(request *PromptStartRequest) { request.Directory = "" }, true},
		{"relative directory", func(request *PromptStartRequest) { request.Directory = "tmp/project" }, true},
		{"unclean directory", func(request *PromptStartRequest) { request.Directory = "/tmp//project" }, true},
		{"directory with NUL", func(request *PromptStartRequest) { request.Directory = "/tmp/pro\x00ject" }, true},
		{"derived socket", func(request *PromptStartRequest) { request.OMPStatusSocket = "" }, false},
		{"relative socket", func(request *PromptStartRequest) { request.OMPStatusSocket = "omp.sock" }, true},
		{"unclean socket", func(request *PromptStartRequest) { request.OMPStatusSocket = "/tmp//omp.sock" }, true},
		{"socket with NUL", func(request *PromptStartRequest) { request.OMPStatusSocket = "/tmp/omp\x00.sock" }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := PromptStartRequest{Argv: []string{"omp"}, Directory: "/tmp/project", OMPStatusSocket: "/tmp/omp.sock"}
			test.change(&request)
			argv, kind, err := validatePromptStart(request)
			if test.invalid {
				if !errors.Is(err, ErrPromptInvalid) || argv != nil || kind != "" {
					t.Fatalf("argv=%q kind=%q error=%v", argv, kind, err)
				}
				return
			}
			if err != nil || kind != "omp" || !reflect.DeepEqual(argv, []string{"omp"}) {
				t.Fatalf("argv=%q kind=%q error=%v", argv, kind, err)
			}
		})
	}
}
