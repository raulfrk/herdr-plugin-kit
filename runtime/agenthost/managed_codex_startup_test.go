package agenthost

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

// Observed Codex 0.154.0 startup: the styled empty composer appears before
// slash commands execute, and the header explicitly reports a loading model.
const codexLoadingHeader = "╭─────────────────────────────────────────╮\n" +
	"│ >_ OpenAI Codex (v0.154.0)              │\n" +
	"│ model:       loading   /model to change │\n" +
	"│ directory:   /tmp/project              │\n" +
	"╰─────────────────────────────────────────╯\n"

func TestCodexStartupLoadingUsesNativeModelRow(t *testing.T) {
	for _, test := range []struct {
		name, text string
		loading    bool
	}{
		{"observed", codexLoadingHeader, true},
		{"styled CRLF", "\x1b[1m│\x1b[0m model: \x1b[2mloading\x1b[0m /model to change │\r\n", true},
		{"model row only", "│model: loading│", true},
		{"ready", strings.Replace(codexLoadingHeader, "loading", "gpt-5.6-sol medium", 1), false},
		{"empty", "", false},
		{"blank box row", "│ │", false},
		{"incomplete row", "│ model: │", false},
		{"different field", "│ directory: loading │", false},
		{"different model", "│ model: loading-model │", false},
		{"unboxed text", "model: loading", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := codexStartupLoading(test.text); got != test.loading {
				t.Fatalf("loading=%t want=%t", got, test.loading)
			}
		})
	}
	if !emptyCodexComposer(string(managedEmptyComposer(codexLoadingHeader))) {
		t.Fatal("startup readiness must not redefine the shared empty-composer predicate")
	}
}

func TestManagedIdentityWaitsThroughObservedStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
		initial := managedAgent(target, "/tmp/project", "unused", false)
		initial.Session = nil
		busy := initial
		busy.Status, busy.Revision, busy.StateChangeSeq = "working", 1, 1
		settled := busy
		settled.Status, settled.StateChangeSeq = "done", 2
		ready := managedEmptyComposer("initialized header")
		runner := &scriptedRunner{results: []command.Result{
			managedAgentResult(t, initial), {Stdout: managedEmptyComposer(codexLoadingHeader)},
			managedAgentResult(t, initial), {Stdout: ready},
			paneScrollResult(t, target, 0), {Stdout: []byte("old output")}, managedAgentResult(t, busy),
			managedAgentResult(t, settled), {Stdout: ready},
			paneScrollResult(t, target, 0), {Stdout: []byte("old output")},
			managedAgentResult(t, settled), {Stdout: ready}, {},
			managedAgentResult(t, settled), {Stdout: managedStatusDraft("header")},
			managedAgentResult(t, settled), {Stdout: managedStatusDraft("header")}, {},
			paneScrollResult(t, target, 40), {Stdout: []byte("old output\n› " + observedManagedStatus())}, {Stdout: ready},
		}}
		var busyObserved, settledObserved time.Time
		observedRunner := runnerFunc(func(ctx context.Context, executable string, args []string) (command.Result, error) {
			result, err := runner.Run(ctx, executable, args)
			// These are the fixture's busy response and following settled response.
			if len(runner.calls) == 7 {
				busyObserved = time.Now()
			}
			if len(runner.calls) == 8 {
				settledObserved = time.Now()
			}
			return result, err
		})
		launcher := NewManagedCodexLauncher(Host{Runner: observedRunner, Session: "named"})
		launcher.topology = &fakeManagedTopology{}
		_, id, err := launcher.discoverManagedIdentity(managedIdentityTestContext(t), target, "/tmp/project", []string{"codex"}, managedServiceSnapshot(), managedPolicy())
		if err != nil || id != "01a09f5c-37cc-77f1-9409-e9aa070ad8d0" {
			t.Fatalf("initialized identity not accepted: id=%q error=%v", id, err)
		}
		inputs := 0
		for index, call := range runner.calls {
			if strings.Contains(strings.Join(call, " "), "send-") {
				inputs++
				if index < 13 {
					t.Fatalf("input before initialization settled: %v", call)
				}
			}
		}
		if inputs != 2 {
			t.Fatalf("must send status and Enter once each, got %d calls", inputs)
		}
		if settledObserved.Sub(busyObserved) < 25*time.Millisecond {
			t.Fatalf("startup busy retry bypassed its backoff: %v", settledObserved.Sub(busyObserved))
		}
	})
}

func TestManagedIdentityRejectsBusyRecheckWithoutLoading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
		initial := managedAgent(target, "/tmp/project", "unused", false)
		initial.Session = nil
		busy := initial
		busy.Status = "working"
		runner := &scriptedRunner{results: []command.Result{
			managedAgentResult(t, initial), {Stdout: managedEmptyComposer("initialized header")},
			paneScrollResult(t, target, 0), {Stdout: []byte("old output")}, managedAgentResult(t, busy),
		}}
		launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
		launcher.topology = &fakeManagedTopology{}
		_, _, err := launcher.discoverManagedIdentity(managedIdentityTestContext(t), target, "/tmp/project", []string{"codex"}, managedServiceSnapshot(), managedPolicy())
		if !errors.Is(err, ErrManagedPaneBusy) || len(runner.calls) != 5 || hasPaneInput(runner.calls) {
			t.Fatalf("busy pane retried without observed initialization: error=%v calls=%v", err, runner.calls)
		}
	})
}

func TestManagedIdentityStartupWaitPreservesDriftRefusal(t *testing.T) {
	for _, scenario := range []string{"focused", "launch pending", "session", "cwd", "process", "ready revision"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				target := sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
				initial := managedAgent(target, "/tmp/project", "unused", false)
				initial.Session = nil
				busy := wireFromAgent(initial)
				busy.AgentStatus = "working"
				busy.StateChangeSeq++
				topology := &fakeManagedTopology{}
				switch scenario {
				case "focused":
					busy.Focused = true
				case "launch pending":
					busy.LaunchPending = true
				case "session":
					busy.AgentSession = wireFromAgent(managedAgent(target, "/tmp/project", managedNewID, false)).AgentSession
				case "cwd":
					busy.CWD, busy.ForegroundCWD = "/different", "/different"
				case "process":
					topology.pids = []int{99, 99, 100}
				case "ready revision":
					busy.AgentStatus = "idle"
					busy.Revision++
				}
				runner := &scriptedRunner{results: []command.Result{
					managedAgentResult(t, initial), {Stdout: managedEmptyComposer(codexLoadingHeader)},
					managedAgentResult(t, initial), {Stdout: managedEmptyComposer("initialized header")},
					paneScrollResult(t, target, 0), {Stdout: []byte("old")}, managedWireAgentResult(t, busy),
				}}
				launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
				launcher.topology = topology
				_, _, err := launcher.discoverManagedIdentity(managedIdentityTestContext(t), target, "/tmp/project", []string{"codex"}, managedServiceSnapshot(), managedPolicy())
				if err == nil || hasPaneInput(runner.calls) || len(runner.calls) != 7 {
					t.Fatalf("drift continued or sent input: error=%v calls=%v", err, runner.calls)
				}
			})
		})
	}
}

func TestManagedIdentityPersistentLoadingRespectsDeadline(t *testing.T) {
	for _, reported := range []bool{false, true} {
		t.Run(map[bool]string{false: "unreported", true: "reported"}[reported], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				target := sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
				agent := managedAgent(target, "/tmp/project", managedNewID, false)
				if !reported {
					agent.Session = nil
				}
				polls := 0
				runner := runnerFunc(func(_ context.Context, _ string, args []string) (command.Result, error) {
					joined := strings.Join(args, " ")
					switch {
					case strings.Contains(joined, "agent get"):
						polls++
						if polls > 42 {
							t.Fatal("startup wait spun instead of respecting its backoff/deadline")
						}
						return managedAgentResult(t, agent), nil
					case strings.Contains(joined, "pane read"):
						return command.Result{Stdout: managedEmptyComposer(codexLoadingHeader)}, nil
					default:
						t.Fatalf("unexpected action during loading: %v", args)
						return command.Result{}, nil
					}
				})
				launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
				launcher.topology = &fakeManagedTopology{}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				started := time.Now()
				_, _, err := launcher.discoverManagedIdentity(ctx, target, "/tmp/project", []string{"codex"}, managedServiceSnapshot(), managedPolicy())
				if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrManagedPaneBusy) || time.Since(started) != time.Second || polls < 2 {
					t.Fatalf("loading bound: error=%v elapsed=%v polls=%d", err, time.Since(started), polls)
				}
			})
		})
	}
}

func TestManagedIdentityLoadingRecheckSendsNoInput(t *testing.T) {
	for _, reported := range []bool{false, true} {
		target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
		agent := managedAgent(target, "/tmp/project", managedNewID, false)
		if !reported {
			agent.Session = nil
		}
		results := []command.Result{managedAgentResult(t, agent), {Stdout: managedEmptyComposer("ready")}}
		if !reported {
			results = append(results, paneScrollResult(t, target, 0), command.Result{Stdout: []byte("old")})
		}
		results = append(results, managedAgentResult(t, agent), command.Result{Stdout: managedEmptyComposer(codexLoadingHeader)})
		runner := &scriptedRunner{results: results}
		launcher := NewManagedCodexLauncher(Host{Runner: runner})
		launcher.topology = &fakeManagedTopology{}
		_, _, err := launcher.discoverManagedIdentity(managedIdentityTestContext(t), target, "/tmp/project", []string{"codex"}, managedServiceSnapshot(), managedPolicy())
		if !errors.Is(err, ErrManagedPaneBusy) || hasPaneInput(runner.calls) {
			t.Fatalf("reported=%t error=%v calls=%v", reported, err, runner.calls)
		}
	}
}

func TestManagedIdentityLoadingAfterStatusDraftNeverEnters(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	agent := managedAgent(target, "/tmp/project", "unused", false)
	agent.Session = nil
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, agent), {Stdout: managedEmptyComposer("ready")},
		paneScrollResult(t, target, 0), {Stdout: []byte("old")},
		managedAgentResult(t, agent), {Stdout: managedEmptyComposer("ready")}, {},
		managedAgentResult(t, agent), {Stdout: managedStatusDraft(codexLoadingHeader)},
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	launcher.topology = &fakeManagedTopology{}
	_, _, err := launcher.discoverManagedIdentity(managedIdentityTestContext(t), target, "/tmp/project", []string{"codex"}, managedServiceSnapshot(), managedPolicy())
	if !errors.Is(err, ErrManagedPaneBusy) || len(runner.calls) != 9 {
		t.Fatalf("error=%v calls=%v", err, runner.calls)
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call, " "), "send-keys") {
			t.Fatal("Enter was sent after initialization drift")
		}
	}
}
