package agenthost

import (
	"context"
	"errors"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

const switchGuardOMPEmptyComposer = "header\n\x1b[38;2;156;163;176m╰─ \x1b[0m"

// switchGuardCancelAfterLastResult cancels the caller's context as soon as the final
// scripted Herdr result has been served, so the cancellation lands after the
// last read of a guard.
type switchGuardCancelAfterLastResult struct {
	*scriptedRunner
	cancel context.CancelFunc
}

func (r switchGuardCancelAfterLastResult) Run(ctx context.Context, exe string, args []string) (command.Result, error) {
	result, err := r.scriptedRunner.Run(ctx, exe, args)
	r.mu.Lock()
	last := len(r.results) == 0
	r.mu.Unlock()
	if last {
		r.cancel()
	}
	return result, err
}

// switchGuardAutofocusCase drives VerifyFocusedWorkingAutofocus through a focused,
// working OMP source. Each field can be changed before verify to break one
// guard condition while the rest stays valid.
type switchGuardAutofocusCase struct {
	target                Agent
	before, second, final wireAgent
	processes             []sessionhost.ForegroundProcess
	cancelAfterFinalRead  bool
}

func newSwitchGuardAutofocusCase() *switchGuardAutofocusCase {
	target, before := safeNativeTarget("omp", "path", "/tmp/session.jsonl")
	target.Status, before.AgentStatus = "working", "working"
	target.Focused, before.Focused = true, true
	process := sessionhost.ForegroundProcess{PID: 77, Name: "omp", CWD: target.CWD, Argv: []string{"omp"}}
	second, final := before, before
	second.Revision, final.Revision = before.Revision+100, before.Revision+200
	return &switchGuardAutofocusCase{target: target, before: before, second: second, final: final, processes: []sessionhost.ForegroundProcess{process, process}}
}

func (c *switchGuardAutofocusCase) verify(t *testing.T) (Agent, *scriptedRunner, error) {
	t.Helper()
	results := append([]command.Result{jsonResult(t, listResponse(c.before))}, fourNativeIdentities(t, c.target)...)
	results = append(results,
		command.Result{Stdout: []byte(switchGuardOMPEmptyComposer)}, jsonResult(t, listResponse(c.second)),
		command.Result{Stdout: []byte(switchGuardOMPEmptyComposer)}, jsonResult(t, listResponse(c.final)),
	)
	runner := &scriptedRunner{results: results}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var host command.Runner = runner
	if c.cancelAfterFinalRead {
		host = switchGuardCancelAfterLastResult{scriptedRunner: runner, cancel: cancel}
	}
	payload := `{"version":1,"harness":"omp","session_kind":"path","session_value":"/tmp/session.jsonl","status":"working","state_seq":9}`
	sources := NativeSources{OMPSockets: map[string]string{c.target.PaneID: serveOMPResponses(t, payload, payload)}}
	probe := mustNativeProbe(t, Host{Runner: host, Session: "named"}, sources)
	probe.topology = &fakeManagedTopology{inspectedSeq: c.processes}
	got, err := probe.VerifyFocusedWorkingAutofocus(ctx, c.target)
	return got, runner, err
}

func TestVerifyFocusedWorkingAutofocusRejectsUnsuitableSourceWithoutReadingHerdr(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Agent)
	}{
		{"invalid identity", func(a *Agent) { a.TabID = "" }},
		{"unfocused", func(a *Agent) { a.Focused = false }},
		{"launch pending", func(a *Agent) { a.LaunchPending = true }},
		{"not working", func(a *Agent) { a.Status = "idle" }},
		{"relative directory", func(a *Agent) { a.CWD = "project" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := newSwitchGuardAutofocusCase().target
			test.change(&target)
			runner := &scriptedRunner{}
			probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, NativeSources{})
			_, err := probe.VerifyFocusedWorkingAutofocus(context.Background(), target)
			if !errors.Is(err, ErrUnsafeSessionChange) || errors.Is(err, ErrStaleReport) || len(runner.calls) != 0 {
				t.Fatalf("error=%v calls=%v", err, runner.calls)
			}
		})
	}
}

func TestVerifyFocusedWorkingAutofocusChecksForegroundProcessAndFinalRead(t *testing.T) {
	good := sessionhost.ForegroundProcess{PID: 77, Name: "omp", CWD: "/tmp/project", Argv: []string{"omp"}}
	t.Run("unchanged source", func(t *testing.T) {
		c := newSwitchGuardAutofocusCase()
		got, _, err := c.verify(t)
		if err != nil || !sameImmutable(got, c.target) || got.Revision != c.final.Revision {
			t.Fatalf("agent=%+v error=%v", got, err)
		}
	})
	for _, test := range []struct {
		name   string
		change func(*sessionhost.ForegroundProcess)
	}{
		{"zero PID", func(p *sessionhost.ForegroundProcess) { p.PID = 0 }},
		{"negative PID", func(p *sessionhost.ForegroundProcess) { p.PID = -1 }},
		{"other directory", func(p *sessionhost.ForegroundProcess) { p.CWD = "/tmp/other" }},
		{"empty name", func(p *sessionhost.ForegroundProcess) { p.Name = "" }},
		{"empty argv", func(p *sessionhost.ForegroundProcess) { p.Argv = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := newSwitchGuardAutofocusCase()
			bad := good
			test.change(&bad)
			c.processes = []sessionhost.ForegroundProcess{bad}
			_, runner, err := c.verify(t)
			if !errors.Is(err, ErrUnsafeSessionChange) || !errors.Is(err, ErrStaleReport) || len(runner.calls) != 1 {
				t.Fatalf("error=%v calls=%d", err, len(runner.calls))
			}
		})
	}
	for _, read := range []string{"first", "second", "last"} {
		t.Run("identity changed at the "+read+" read", func(t *testing.T) {
			c := newSwitchGuardAutofocusCase()
			switch read {
			case "first":
				c.before.TabID = "w:other"
			case "second":
				c.second.TabID = "w:other"
			default:
				c.final.TabID = "w:other"
			}
			_, runner, err := c.verify(t)
			if !errors.Is(err, ErrUnsafeSessionChange) || !errors.Is(err, ErrStaleReport) || read == "first" && len(runner.calls) != 1 {
				t.Fatalf("error=%v calls=%d", err, len(runner.calls))
			}
		})
	}
	t.Run("source changed after the last composer check", func(t *testing.T) {
		c := newSwitchGuardAutofocusCase()
		c.final.StateChangeSeq++
		if _, _, err := c.verify(t); !errors.Is(err, ErrUnsafeSessionChange) || !errors.Is(err, ErrStaleReport) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("cancelled after the last read", func(t *testing.T) {
		c := newSwitchGuardAutofocusCase()
		c.cancelAfterFinalRead = true
		if _, _, err := c.verify(t); !errors.Is(err, ErrUnsafeSessionChange) || !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestVerifyAutofocusComposerEmptyRejectsCodexSourceWithoutVerifiableIdentity(t *testing.T) {
	valid := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, true)
	invalid := valid
	invalid.Session = &SessionRef{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: "not-a-thread"}
	missing := valid
	missing.Session = nil
	for _, test := range []struct {
		name   string
		target Agent
		socket string
	}{
		{"missing conversation", missing, "/tmp/codex.sock"},
		{"invalid conversation", invalid, "/tmp/codex.sock"},
		{"missing App Server socket", valid, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &scriptedRunner{}
			probe := NewProbe(Host{Runner: runner, Session: "named", CodexSocket: test.socket})
			if err := probe.verifyAutofocusComposerEmpty(context.Background(), test.target); !errors.Is(err, ErrUnsafeSessionChange) || len(runner.calls) != 0 {
				t.Fatalf("error=%v calls=%v", err, runner.calls)
			}
		})
	}
}

func TestVerifyNativeSessionChangeStopsAtFirstReadDrift(t *testing.T) {
	for _, field := range []string{"identity", "directory"} {
		t.Run(field, func(t *testing.T) {
			target, wire := safeNativeTarget("omp", "path", "/tmp/session.jsonl")
			if field == "identity" {
				wire.TabID = "w:other"
			} else {
				wire.CWD = "/tmp/other"
			}
			runner := &scriptedRunner{results: []command.Result{jsonResult(t, listResponse(wire))}}
			probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, NativeSources{OMPSockets: map[string]string{target.PaneID: "/tmp/status.sock"}})
			_, err := probe.VerifyNativeSessionChange(context.Background(), target)
			if !errors.Is(err, ErrUnsafeSessionChange) || !errors.Is(err, ErrStaleReport) || len(runner.calls) != 1 {
				t.Fatalf("error=%v calls=%d", err, len(runner.calls))
			}
		})
	}
}

func TestVerifyNativeSessionChangeRejectsStableAssessmentOtherThanIdleOrDone(t *testing.T) {
	// Herdr still reports idle, but the native OMP status protocol says working.
	target, wire := safeNativeTarget("omp", "path", "/tmp/session.jsonl")
	results := append([]command.Result{jsonResult(t, listResponse(wire))}, fourNativeIdentities(t, target)...)
	results = append(results, command.Result{Stdout: []byte(switchGuardOMPEmptyComposer)}, jsonResult(t, listResponse(wire)))
	payload := `{"version":1,"harness":"omp","session_kind":"path","session_value":"/tmp/session.jsonl","status":"working","state_seq":9}`
	probe := mustNativeProbe(t,
		Host{Runner: &scriptedRunner{results: results}, Session: "named"},
		NativeSources{OMPSockets: map[string]string{target.PaneID: serveOMPResponses(t, payload, payload)}},
	)
	if got, err := probe.VerifyNativeSessionChange(context.Background(), target); !errors.Is(err, ErrUnsafeSessionChange) {
		t.Fatalf("working native status accepted: agent=%+v error=%v", got, err)
	}
}

func TestVerifyNativeSessionChangeLetsDoneAssessmentReachTheComposerCheck(t *testing.T) {
	// A done source passes the status guard; this harness has no composer
	// verifier, so the guard then fails closed as stale instead of as unsafe.
	target, wire := safeNativeTarget("claude", "id", "session-1")
	target.Status, wire.AgentStatus = "done", "done"
	results := append([]command.Result{jsonResult(t, listResponse(wire))}, oneObservation(t, wire, wire, "text")...)
	results = append(results, oneObservation(t, wire, wire, "text")...)
	probe := mustNativeProbe(t, Host{Runner: &scriptedRunner{results: results}, Session: "named"}, NativeSources{})
	if _, err := probe.VerifyNativeSessionChange(context.Background(), target); !errors.Is(err, ErrUnsafeSessionChange) || !errors.Is(err, ErrStaleReport) {
		t.Fatalf("error=%v", err)
	}
}
