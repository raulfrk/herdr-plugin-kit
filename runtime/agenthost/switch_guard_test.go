package agenthost

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func TestVerifyFocusedWorkingAutofocusCodex(t *testing.T) {
	for _, variant := range []string{"empty streaming", "draft", "blocked native", "focus drift"} {
		t.Run(variant, func(t *testing.T) {
			status := `{"type":"active","activeFlags":[]}`
			if variant == "blocked native" {
				status = `{"type":"active","activeFlags":["waitingOnUserInput"]}`
			}
			socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
				exchange(t, ctx, conn)
				send(t, ctx, conn, threadResult(status))
			})
			target := managedAgent(managedOrigin(), "/tmp/project", testThread, true)
			target.Status = "working"
			target.StateChangeSeq = 1
			before := wireFromAgent(target)
			second, final := before, before
			second.Revision, final.Revision = before.Revision+100, before.Revision+200
			if variant == "focus drift" {
				second.Focused = false
			}
			composer := managedEmptyComposer("answer")
			if variant == "draft" {
				composer = []byte("answer\n› user draft\n status")
			}
			runner := &scriptedRunner{results: []command.Result{
				jsonResult(t, listResponse(before)),
				jsonResult(t, listResponse(before)), jsonResult(t, listResponse(before)),
				{Stdout: composer}, jsonResult(t, listResponse(second)),
				{Stdout: managedEmptyComposer("answer")}, jsonResult(t, listResponse(final)),
			}}
			probe := NewProbe(Host{Runner: runner, Session: "named", CodexSocket: socket})
			probe.topology = &fakeManagedTopology{inspected: sessionhost.ForegroundProcess{PID: 77, Name: "codex", CWD: target.CWD, Argv: []string{"codex"}}}
			got, err := probe.VerifyFocusedWorkingAutofocus(context.Background(), target)
			if variant == "empty streaming" {
				if err != nil || !sameImmutable(got, target) || got.Revision != final.Revision {
					t.Fatalf("agent=%+v error=%v", got, err)
				}
			} else if !errors.Is(err, ErrUnsafeSessionChange) {
				t.Fatalf("%s accepted: agent=%+v error=%v", variant, got, err)
			}
			if hasPaneInput(runner.calls) {
				t.Fatalf("guard sent input: %#v", runner.calls)
			}
		})
	}
}

func TestVerifyFocusedWorkingAutofocusNative(t *testing.T) {
	for _, test := range []struct {
		name, kind, sessionKind, sessionValue string
		empty, draft                          string
	}{
		{"OMP", "omp", "path", "/tmp/session.jsonl", "header\n\x1b[38;2;156;163;176m╰─ \x1b[0m", "header\n╰─ draft"},
		{"OpenCode", "opencode", "id", "session-1", openCodeEmptyComposerANSI("sidebar"), strings.Replace(openCodeEmptyComposerANSI("sidebar"), "\x1b[38;2;255;255;255;48;2;30;30;30m        \x1b[0m", "\x1b[38;2;255;255;255;48;2;30;30;30m draft  \x1b[0m", 1)},
	} {
		for _, variant := range []string{"empty streaming", "draft", "state drift", "launch pending", "process drift", "cancelled"} {
			t.Run(test.name+"/"+variant, func(t *testing.T) {
				target, before := safeNativeTarget(test.kind, test.sessionKind, test.sessionValue)
				target.Status, before.AgentStatus = "working", "working"
				target.Focused, before.Focused = true, true
				second, final := before, before
				second.Revision, final.Revision = before.Revision+100, before.Revision+200
				if variant == "state drift" {
					second.StateChangeSeq++
				}
				if variant == "launch pending" {
					before.LaunchPending = true
				}
				composer := test.empty
				if variant == "draft" {
					composer = test.draft
				}
				results := append([]command.Result{jsonResult(t, listResponse(before))}, fourNativeIdentities(t, target)...)
				results = append(results, command.Result{Stdout: []byte(composer)}, jsonResult(t, listResponse(second)), command.Result{Stdout: []byte(test.empty)}, jsonResult(t, listResponse(final)))
				runner := &scriptedRunner{results: results}
				sources := NativeSources{}
				if test.kind == "omp" {
					payload := `{"version":1,"harness":"omp","session_kind":"path","session_value":"/tmp/session.jsonl","status":"working","state_seq":9}`
					sources.OMPSockets = map[string]string{target.PaneID: serveOMPResponses(t, payload, payload)}
				} else {
					fixture := &openCodeFixture{t: t, target: target, directory: target.CWD, status: "busy"}
					server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
					t.Cleanup(server.Close)
					sources.OpenCodeServers = map[string]OpenCodeServer{target.PaneID: {URL: server.URL, Directory: target.CWD}}
				}
				probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, sources)
				process := sessionhost.ForegroundProcess{PID: 77, Name: test.kind, CWD: target.CWD, Argv: []string{test.kind}}
				topology := &fakeManagedTopology{inspected: process}
				if variant == "process drift" {
					changed := process
					changed.PID++
					topology.inspectedSeq = []sessionhost.ForegroundProcess{process, changed}
				}
				probe.topology = topology
				ctx := context.Background()
				if variant == "cancelled" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				got, err := probe.VerifyFocusedWorkingAutofocus(ctx, target)
				if variant == "empty streaming" {
					if err != nil || !sameImmutable(got, target) || got.Revision != final.Revision {
						t.Fatalf("agent=%+v error=%v", got, err)
					}
				} else if !errors.Is(err, ErrUnsafeSessionChange) {
					t.Fatalf("%s accepted: agent=%+v error=%v", variant, got, err)
				}
				if hasPaneInput(runner.calls) {
					t.Fatalf("guard sent input: %#v", runner.calls)
				}
			})
		}
	}
}

func TestVerifyNativeSessionChangeChecksSupportedComposers(t *testing.T) {
	for _, test := range []struct {
		name, kind, sessionKind, sessionValue string
		empty, draft                          string
	}{
		{"OMP", "omp", "path", "/tmp/session.jsonl", "header\n\x1b[38;2;156;163;176m╰─ \x1b[0m", "header\n╰─ draft"},
		{"OpenCode", "opencode", "id", "session-1", openCodeEmptyComposerANSI("sidebar"), strings.Replace(openCodeEmptyComposerANSI("sidebar"), "\x1b[38;2;255;255;255;48;2;30;30;30m        \x1b[0m", "\x1b[38;2;255;255;255;48;2;30;30;30m draft  \x1b[0m", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, composer := range []string{test.empty, test.draft} {
				t.Run(map[bool]string{true: "empty", false: "draft"}[composer == test.empty], func(t *testing.T) {
					target, wire := safeNativeTarget(test.kind, test.sessionKind, test.sessionValue)
					results := append([]command.Result{jsonResult(t, listResponse(wire))}, fourNativeIdentities(t, target)...)
					results = append(results, command.Result{Stdout: []byte(composer)}, jsonResult(t, listResponse(wire)))
					runner := &scriptedRunner{results: results}
					sources := NativeSources{}
					if test.kind == "omp" {
						payload := `{"version":1,"harness":"omp","session_kind":"path","session_value":"/tmp/session.jsonl","status":"idle","state_seq":9}`
						sources.OMPSockets = map[string]string{target.PaneID: serveOMPResponses(t, payload, payload)}
					} else {
						fixture := &openCodeFixture{t: t, target: target, directory: target.CWD, status: "idle"}
						server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
						t.Cleanup(server.Close)
						sources.OpenCodeServers = map[string]OpenCodeServer{target.PaneID: {URL: server.URL, Directory: target.CWD}}
					}
					probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, sources)
					probe.wait = func(context.Context, time.Duration) error { return nil }
					got, err := probe.VerifyNativeSessionChange(context.Background(), target)
					if composer == test.empty {
						if err != nil || !sameImmutable(got, target) {
							t.Fatalf("agent=%+v error=%v", got, err)
						}
					} else if !errors.Is(err, ErrUnsafeSessionChange) {
						t.Fatalf("draft accepted: agent=%+v error=%v", got, err)
					}
				})
			}
		})
	}
}

func TestVerifyNativeSessionChangeRejectsFocusedBusyAndChangedSource(t *testing.T) {
	for _, change := range []string{"focused", "busy", "changed"} {
		t.Run(change, func(t *testing.T) {
			target, wire := safeNativeTarget("omp", "path", "/tmp/session.jsonl")
			switch change {
			case "focused":
				wire.Focused = true
			case "busy":
				wire.AgentStatus = "working"
			case "changed":
				wire.TabID = "w:other"
			}
			runner := &scriptedRunner{results: []command.Result{jsonResult(t, listResponse(wire))}}
			probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, NativeSources{OMPSockets: map[string]string{target.PaneID: "/tmp/status.sock"}})
			if _, err := probe.VerifyNativeSessionChange(context.Background(), target); !errors.Is(err, ErrUnsafeSessionChange) {
				t.Fatalf("%s source accepted: %v", change, err)
			}
		})
	}
}

func TestVerifyNativeOverlaySessionChangeRequiresFocusedEmptyComposer(t *testing.T) {
	for _, test := range []struct {
		name, kind, sessionKind, sessionValue string
		empty, draft                          string
	}{
		{"OMP", "omp", "path", "/tmp/session.jsonl", "header\n\x1b[38;2;156;163;176m╰─ \x1b[0m", "header\n╰─ draft"},
		{"OpenCode", "opencode", "id", "session-1", openCodeEmptyComposerANSI("sidebar"), strings.Replace(openCodeEmptyComposerANSI("sidebar"), "\x1b[38;2;255;255;255;48;2;30;30;30m        \x1b[0m", "\x1b[38;2;255;255;255;48;2;30;30;30m draft  \x1b[0m", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, composer := range []string{test.empty, test.draft} {
				t.Run(map[bool]string{true: "empty", false: "draft"}[composer == test.empty], func(t *testing.T) {
					target, wire := safeNativeTarget(test.kind, test.sessionKind, test.sessionValue)
					target.Focused, wire.Focused = true, true
					results := append([]command.Result{jsonResult(t, listResponse(wire))}, fourNativeIdentities(t, target)...)
					results = append(results, command.Result{Stdout: []byte(composer)}, jsonResult(t, listResponse(wire)))
					runner := &scriptedRunner{results: results}
					sources := NativeSources{}
					if test.kind == "omp" {
						payload := `{"version":1,"harness":"omp","session_kind":"path","session_value":"/tmp/session.jsonl","status":"idle","state_seq":9}`
						sources.OMPSockets = map[string]string{target.PaneID: serveOMPResponses(t, payload, payload)}
					} else {
						fixture := &openCodeFixture{t: t, target: target, directory: target.CWD, status: "idle"}
						server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
						t.Cleanup(server.Close)
						sources.OpenCodeServers = map[string]OpenCodeServer{target.PaneID: {URL: server.URL, Directory: target.CWD}}
					}
					probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, sources)
					got, err := probe.VerifyNativeOverlaySessionChange(context.Background(), target)
					if composer == test.empty {
						if err != nil || !sameImmutable(got, target) || !got.Focused {
							t.Fatalf("agent=%+v error=%v", got, err)
						}
					} else if !errors.Is(err, ErrUnsafeSessionChange) {
						t.Fatalf("draft accepted: agent=%+v error=%v", got, err)
					}
				})
			}
		})
	}
}

func TestVerifyNativeOverlaySessionChangeRejectsUnfocusedSource(t *testing.T) {
	target, wire := safeNativeTarget("omp", "path", "/tmp/session.jsonl")
	runner := &scriptedRunner{results: []command.Result{jsonResult(t, listResponse(wire))}}
	probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, NativeSources{OMPSockets: map[string]string{target.PaneID: "/tmp/status.sock"}})
	if _, err := probe.VerifyNativeOverlaySessionChange(context.Background(), target); !errors.Is(err, ErrUnsafeSessionChange) {
		t.Fatalf("unfocused overlay source accepted: %v", err)
	}
}

func TestVerifyNativeSessionChangeRejectsEachGuardConditionIndependently(t *testing.T) {
	newProbe := func(t *testing.T, target Agent, before, after wireAgent) Probe {
		t.Helper()
		results := append([]command.Result{jsonResult(t, listResponse(before))}, fourNativeIdentities(t, target)...)
		results = append(results,
			command.Result{Stdout: []byte("header\n\x1b[38;2;156;163;176m╰─ \x1b[0m")},
			jsonResult(t, listResponse(after)),
		)
		payload := `{"version":1,"harness":"omp","session_kind":"path","session_value":"/tmp/session.jsonl","status":"idle","state_seq":9}`
		return mustNativeProbe(t,
			Host{Runner: &scriptedRunner{results: results}, Session: "named"},
			NativeSources{OMPSockets: map[string]string{target.PaneID: serveOMPResponses(t, payload, payload)}},
		)
	}

	t.Run("initial focus", func(t *testing.T) {
		target, before := safeNativeTarget("omp", "path", "/tmp/session.jsonl")
		before.Focused = true
		after := before
		after.Focused = false
		if _, err := newProbe(t, target, before, after).VerifyNativeSessionChange(context.Background(), target); !errors.Is(err, ErrUnsafeSessionChange) {
			t.Fatalf("focused source accepted: %v", err)
		}
	})

	t.Run("initial busy status", func(t *testing.T) {
		target, before := safeNativeTarget("omp", "path", "/tmp/session.jsonl")
		before.AgentStatus = "working"
		after := before
		if _, err := newProbe(t, target, before, after).VerifyNativeSessionChange(context.Background(), target); !errors.Is(err, ErrUnsafeSessionChange) {
			t.Fatalf("busy source accepted: %v", err)
		}
	})

	for _, field := range []string{"identity", "cwd", "status", "revision", "state sequence", "focus"} {
		t.Run("stable read "+field, func(t *testing.T) {
			target, before := safeNativeTarget("omp", "path", "/tmp/session.jsonl")
			after := before
			switch field {
			case "identity":
				after.TabID = "w:other"
			case "cwd":
				after.CWD = "/tmp/other"
			case "status":
				after.AgentStatus = "done"
			case "revision":
				after.Revision++
			case "state sequence":
				after.StateChangeSeq++
			case "focus":
				after.Focused = true
			}
			if _, err := newProbe(t, target, before, after).VerifyNativeSessionChange(context.Background(), target); !errors.Is(err, ErrUnsafeSessionChange) {
				t.Fatalf("%s drift accepted: %v", field, err)
			}
		})
	}
}

func safeNativeTarget(kind, sessionKind, sessionValue string) (Agent, wireAgent) {
	target := nativeTarget(kind, sessionKind, sessionValue)
	target.Status = "idle"
	target.CWD = "/tmp/project"
	target.Revision = 7
	target.StateChangeSeq = 9
	wire := validWireAgent(target.PaneID)
	wire.Agent = kind
	wire.AgentStatus = target.Status
	wire.CWD = target.CWD
	wire.ForegroundCWD = target.CWD
	wire.Focused = false
	wire.WorkspaceID = target.WorkspaceID
	wire.TabID = target.TabID
	wire.Revision = target.Revision
	wire.StateChangeSeq = target.StateChangeSeq
	wire.AgentSession = &wireSession{Source: target.Session.Source, Agent: target.Session.Agent, Kind: target.Session.Kind, Value: target.Session.Value}
	return target, wire
}
