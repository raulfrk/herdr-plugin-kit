package agenthost

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func managedWireAgentResult(t *testing.T, wire wireAgent) command.Result {
	t.Helper()
	var response agentInfoResponse
	response.ID, response.Result.Type, response.Result.Agent = "cli:agent:get", "agent_info", wire
	return jsonResult(t, response)
}

func managedIdentityTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

func TestParseManagedStatusAcceptsObservedWrappedCodex01540Shape(t *testing.T) {
	service := managedServiceSnapshot()
	text := `startup output
/status

╭─────────────────────────────────────────────────────────────────────────────╮
│  >_ OpenAI Codex (v0.154.0)                                                 │
│                                                                             │
│  Remote:                      unix:///tmp/codex-home/app-server-control/    │
│                               app-server-control.sock                       │
│                               (v0.154.0)                                    │
│  Model:                       gpt-5.6-sol (reasoning medium, summaries deta │
│  Model provider:              openai                                        │
│  Directory:                   /tmp/project                                  │
│  Permissions:                 Full Access                                   │
│  Session:                     01a09f5c-37cc-77f1-9409-e9aa070ad8d0          │
╰─────────────────────────────────────────────────────────────────────────────╯

› Ask Codex to do anything
`
	id, err := parseManagedStatus(text, "/tmp/project", service, managedPolicy())
	if err != nil || id != "01a09f5c-37cc-77f1-9409-e9aa070ad8d0" {
		t.Fatalf("id=%q error=%v", id, err)
	}
}

func TestManagedThreadPrefixRequiresExactStrongPartialUUID(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{
		{"", false},
		{managedNewID, false},
		{"01a09f5c-37cc-77f1-9409-e9", false},
		{"01a09f5c-37cc-77f1-9409-e9aa", true},
		{"01a09f5c-37cc-77f1-9409-e9aa070a", true},
		{"01a09f5c-37cc-77f1-9409-e9aa070ad8d", true},
		{managedNewID + " ", false},
		{"01a09f5c-37cc-77f1-9409-e9aa070a ", false},
		{"01a09f5c-37cc-77f1-9409-e9aa070a\n", false},
		{"01a09f5c-37cc-77f1-9409-e9aa070ad8dZ", false},
	} {
		if got := validManagedThreadPrefix(test.value); got != test.valid {
			t.Fatalf("prefix=%q valid=%t want=%t", test.value, got, test.valid)
		}
	}
	prefix := "01a09f5c-37cc-77f1-9409-e9aa070a"
	if got := managedWireThreadPrefix(wireAgent{TerminalTitleStripped: prefix + "..."}); got != prefix {
		t.Fatalf("wire prefix=%q", got)
	}
	for _, title := range []string{"", managedNewID, "title " + prefix, "01a09f5c-37cc-77f1-9409-e9..."} {
		if got := managedWireThreadPrefix(wireAgent{TerminalTitleStripped: title}); got != "" {
			t.Fatalf("title=%q prefix=%q", title, got)
		}
	}
}

func TestManagedPreInputObservationRequiresEveryStableField(t *testing.T) {
	before := managedAgent(sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}, "/tmp/project", managedNewID, false)
	before.Session = nil
	before.Revision = 7
	before.StateChangeSeq = 9
	if !sameManagedPreInputObservation(before, before) {
		t.Fatal("identical observation rejected")
	}
	for name, mutate := range map[string]func(*Agent){
		"immutable":      func(agent *Agent) { agent.PaneID = "w1:p3" },
		"status":         func(agent *Agent) { agent.Status = "done" },
		"revision":       func(agent *Agent) { agent.Revision++ },
		"state sequence": func(agent *Agent) { agent.StateChangeSeq++ },
	} {
		t.Run(name, func(t *testing.T) {
			after := before
			mutate(&after)
			if sameManagedPreInputObservation(before, after) {
				t.Fatalf("accepted %+v", after)
			}
		})
	}
}

func TestManagedIdentityDiscoveryRejectsEveryProcessAndPaneGuardBeforeInput(t *testing.T) {
	inspectErr := errors.New("inspect failed")
	for name, arrange := range map[string]func(*wireAgent, *fakeManagedTopology){
		"focused":        func(wire *wireAgent, _ *fakeManagedTopology) { wire.Focused = true },
		"launch pending": func(wire *wireAgent, _ *fakeManagedTopology) { wire.LaunchPending = true },
		"working":        func(wire *wireAgent, _ *fakeManagedTopology) { wire.AgentStatus = "working" },
		"inspect error":  func(_ *wireAgent, topology *fakeManagedTopology) { topology.inspectErr = inspectErr },
		"process drift":  func(_ *wireAgent, topology *fakeManagedTopology) { topology.inspected.PID++ },
	} {
		t.Run(name, func(t *testing.T) {
			target := managedOrigin()
			expected := managedAgent(target, "/tmp/project", managedSourceID, false)
			expected.Session = nil
			expected.Revision, expected.StateChangeSeq = 1, 1
			process := sessionhost.ForegroundProcess{PID: 77, Name: "codex", CWD: expected.CWD, Argv: []string{"codex"}}
			wire := wireFromAgent(expected)
			topology := &fakeManagedTopology{inspected: process}
			arrange(&wire, topology)
			runner := &scriptedRunner{results: []command.Result{managedWireAgentResult(t, wire)}}
			launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: target.SessionName})
			launcher.topology = topology
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			_, _, err := launcher.discoverManagedIdentityForSource(ctx, target, expected.CWD, expected, process, managedServiceSnapshot())
			if err == nil {
				t.Fatal("unsafe discovery source accepted")
			}
			if name == "inspect error" && !errors.Is(err, inspectErr) {
				t.Fatalf("error=%v", err)
			}
			if hasPaneInput(runner.calls) {
				t.Fatalf("calls=%+v", runner.calls)
			}
		})
	}
}

func TestManagedIdentityDiscoveryBusyRetryBacksOff(t *testing.T) {
	target := managedOrigin()
	expected := managedAgent(target, "/tmp/project", managedSourceID, false)
	expected.Session = nil
	expected.Revision, expected.StateChangeSeq = 1, 1
	busy := wireFromAgent(expected)
	busy.Focused = true
	changed := wireFromAgent(expected)
	changed.Revision++
	process := sessionhost.ForegroundProcess{PID: 77, Name: "codex", CWD: expected.CWD, Argv: []string{"codex"}}
	runner := &scriptedRunner{results: []command.Result{managedWireAgentResult(t, busy), managedWireAgentResult(t, changed)}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: target.SessionName})
	launcher.topology = &fakeManagedTopology{inspected: process}
	started := time.Now()

	_, _, err := launcher.discoverManagedIdentityForSource(managedIdentityTestContext(t), target, expected.CWD, expected, process, managedServiceSnapshot())
	if !errors.Is(err, ErrManagedStale) {
		t.Fatalf("error=%v", err)
	}
	if elapsed := time.Since(started); elapsed < 15*time.Millisecond {
		t.Fatalf("busy-pane retry spun without backoff: %v", elapsed)
	}
	if hasPaneInput(runner.calls) {
		t.Fatalf("calls=%+v", runner.calls)
	}
}

func TestManagedPrefixReadbackRequirementAndGuards(t *testing.T) {
	prefix := "01a09f5c-37cc-77f1-9409-e9aa070a"
	for _, test := range []struct {
		allow    bool
		identity string
		want     bool
	}{
		{true, prefix, true},
		{false, prefix, false},
		{true, managedNewID, false},
		{false, managedNewID, false},
	} {
		if got := managedPrefixReadbackRequired(test.allow, test.identity); got != test.want {
			t.Fatalf("allow=%t identity=%q got=%t want=%t", test.allow, test.identity, got, test.want)
		}
	}
	origin := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	expected := managedAgent(origin, "/tmp/project", managedNewID, false)
	expected.Session = nil
	wire := wireFromAgent(expected)
	longer := prefix + "d8"
	if !validManagedPrefixReadback(expected, wire, expected, prefix, prefix) ||
		!validManagedPrefixReadback(expected, wire, expected, prefix, longer) ||
		!validManagedPrefixReadback(expected, wire, expected, longer, prefix) {
		t.Fatal("compatible prefix readback rejected")
	}
	for name, mutate := range map[string]func(*wireAgent, *Agent, *string){
		"focused":        func(wire *wireAgent, _ *Agent, _ *string) { wire.Focused = true },
		"launch pending": func(wire *wireAgent, _ *Agent, _ *string) { wire.LaunchPending = true },
		"working":        func(wire *wireAgent, _ *Agent, _ *string) { wire.AgentStatus = "working" },
		"bound session": func(_ *wireAgent, agent *Agent, _ *string) {
			agent.Session = &SessionRef{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: managedNewID}
		},
		"pane drift":   func(_ *wireAgent, agent *Agent, _ *string) { agent.PaneID = "w1:p3" },
		"empty title":  func(_ *wireAgent, _ *Agent, title *string) { *title = "" },
		"wrong prefix": func(_ *wireAgent, _ *Agent, title *string) { *title = "02b19f5c-37cc-77f1-9409-e9aa070a" },
	} {
		t.Run(name, func(t *testing.T) {
			changedWire, changedAgent, title := wire, expected, prefix
			mutate(&changedWire, &changedAgent, &title)
			if validManagedPrefixReadback(expected, changedWire, changedAgent, prefix, title) {
				t.Fatal("unsafe readback accepted")
			}
		})
	}
}

func TestParseManagedStatusRequiresExactStatusMarkerAndSessionCardinality(t *testing.T) {
	base := `/status
│ >_ OpenAI Codex (v0.154.0) │
│ Model: gpt-5.6-sol (reasoning medium) │
│ Directory: /tmp/project │
│ Session: 01a09f5c-37cc-77f1-9409-e9aa070ad8d0 │`
	for _, text := range []string{base, "\n" + base, "old output\n" + base} {
		if _, err := parseManagedStatus(text, "/tmp/project", CodexServiceSnapshot{}, managedPolicy()); err != nil {
			t.Fatalf("valid marker rejected: %v", err)
		}
	}
	for _, text := range []string{
		strings.TrimPrefix(base, "/status\n"),
		"old output" + base,
		base + "\n│ Session: 01a09f5c-37cc-77f1-9409-e9aa070ad8d2 │",
		base + "\n│ Forked from: 01a09f5c-37cc-77f1-9409-e9aa070ad8d1 │\n│ Forked from: 01a09f5c-37cc-77f1-9409-e9aa070ad8d2 │",
	} {
		if _, err := parseManagedStatus(text, "/tmp/project", CodexServiceSnapshot{}, managedPolicy()); err == nil {
			t.Fatalf("invalid status accepted: %q", text)
		}
	}
}

func TestParseOrdinaryStatusUsesPaneUUIDWithoutRemoteOrServerInference(t *testing.T) {
	base := `/status
│ >_ OpenAI Codex (v0.154.0) │
│ Model: gpt-5.6-sol (reasoning medium, summaries auto) │
│ Directory: /tmp/project │
│ Permissions: Read Only (never) │
│ Session: 01a0a793-5a65-78e1-b56f-ab827c31b2f9 │`
	id, err := parseManagedStatus(base, "/tmp/project", CodexServiceSnapshot{}, managedPolicy())
	if err != nil || id != "01a0a793-5a65-78e1-b56f-ab827c31b2f9" {
		t.Fatalf("id=%q error=%v", id, err)
	}
	for _, text := range []string{
		base + "\n│ Remote: unix:///tmp/other.sock (v0.154.0) │",
		strings.Replace(base, "01a0a793-5a65-78e1-b56f-ab827c31b2f9", "01a0a793-5a65-78e1-b56f", 1),
		base + "\n│ Session: " + managedChildID + " │",
		strings.Replace(base, "/tmp/project", "/tmp/other", 1),
	} {
		if _, err := parseManagedStatus(text, "/tmp/project", CodexServiceSnapshot{}, managedPolicy()); err == nil {
			t.Fatalf("accepted ambiguous or drifted ordinary status: %q", text)
		}
	}
}

func TestParseOrdinaryStatusCanObserveOneExactModelAndReasoning(t *testing.T) {
	base := `/status
│ >_ OpenAI Codex (v0.154.0) │
│ Model: gpt-5.6-sol (reasoning high, summaries auto) │
│ Directory: /tmp/project │
│ Session: 01a0a793-5a65-78e1-b56f-ab827c31b2f9 │`
	id, err := parseManagedStatus(base, "/tmp/project", CodexServiceSnapshot{}, ManagedCodexPolicy{})
	if err != nil || id != "01a0a793-5a65-78e1-b56f-ab827c31b2f9" {
		t.Fatalf("id=%q error=%v", id, err)
	}
	for _, text := range []string{
		strings.Replace(base, " (reasoning high, summaries auto)", "", 1),
		base + "\n│ Model: gpt-5.6-terra (reasoning medium) │",
	} {
		if _, err := parseManagedStatus(text, "/tmp/project", CodexServiceSnapshot{}, ManagedCodexPolicy{}); err == nil {
			t.Fatalf("accepted incomplete or ambiguous observed policy: %q", text)
		}
	}
}

func TestConversionStatusAcceptsOnlyLocalOrOwnedCodexEndpoints(t *testing.T) {
	service := managedServiceSnapshot()
	header := `/status
│ >_ OpenAI Codex (v0.154.0) │
`
	tail := `│ Model: gpt-5.6-sol (reasoning medium, summaries auto) │
│ Directory: /tmp/project │
│ Session: 01a0a793-5a65-78e1-b56f-ab827c31b2f9 │`
	base := header + tail
	shared := header + "│ Remote: unix://" + service.SocketPath + " (v0.154.0) │\n" + tail
	for _, text := range []string{base, shared} {
		observed, err := parseManagedStatusObservationMode(text, "/tmp/project", service, ManagedCodexPolicy{}, false, "", true)
		if err != nil || observed.identity != "01a0a793-5a65-78e1-b56f-ab827c31b2f9" {
			t.Fatalf("observation=%+v error=%v text=%q", observed, err, text)
		}
	}
	foreign := header + "│ Remote: unix:///tmp/foreign.sock (v0.154.0) │\n" + tail
	conflicting := header + "│ Remote: unix://" + service.SocketPath + " (v0.154.0) │\n│ Remote: unix:///tmp/foreign.sock (v0.154.0) │\n" + tail
	for _, text := range []string{foreign, conflicting} {
		if _, err := parseManagedStatusObservationMode(text, "/tmp/project", service, ManagedCodexPolicy{}, false, "", true); !errors.Is(err, ErrManagedNarrow) {
			t.Fatalf("foreign or conflicting endpoint error=%v text=%q", err, text)
		}
	}
	if _, err := parseManagedStatusObservationMode(base, "/tmp/project", service, ManagedCodexPolicy{}, false, "", false); !errors.Is(err, ErrManagedNarrow) {
		t.Fatalf("strict local endpoint error=%v", err)
	}
	if observed, err := parseManagedStatusObservationMode(shared, "/tmp/project", service, ManagedCodexPolicy{}, false, "", false); err != nil || observed.identity != "01a0a793-5a65-78e1-b56f-ab827c31b2f9" {
		t.Fatalf("strict shared endpoint observation=%+v error=%v", observed, err)
	}
}

func TestConversionStatusRejectsRemoteWithoutFollowingModelField(t *testing.T) {
	service := managedServiceSnapshot()
	text := `/status
│ >_ OpenAI Codex (v0.154.0) │
│ Directory: /tmp/project │
│ Session: 01a0a793-5a65-78e1-b56f-ab827c31b2f9 │
│ Remote: unix://` + service.SocketPath + ` (v0.154.0) │`
	if _, err := parseManagedStatusObservationMode(text, "/tmp/project", service, ManagedCodexPolicy{}, false, "", true); !errors.Is(err, ErrManagedNarrow) {
		t.Fatalf("malformed Remote ordering error=%v", err)
	}
}

func TestParseObservedOrdinaryStatusAcceptsOnlyOneStrongUUIDPrefix(t *testing.T) {
	base := `/status
│ >_ OpenAI Codex (v0.154.0) │
│ Model: gpt-5.6-sol (reasoning high, summaries auto) │
│ Directory: /tmp/project │
│ Session: 01a0980a-280c-72c2-8a90-1baf1239 │`
	observed, err := parseManagedStatusObservation(base, "/tmp/project", CodexServiceSnapshot{}, ManagedCodexPolicy{}, true, "")
	if err != nil || observed.identity != "01a0980a-280c-72c2-8a90-1baf1239" || observed.policy.Model != "gpt-5.6-sol" || observed.policy.ReasoningEffort != "high" {
		t.Fatalf("observation=%+v error=%v", observed, err)
	}
	for _, text := range []string{
		strings.Replace(base, "1baf1239", "1", 1),
		base + "\n│ Session: 01a0980a-280c-72c2-8a90-1baf5678 │",
	} {
		if _, err := parseManagedStatusObservation(text, "/tmp/project", CodexServiceSnapshot{}, ManagedCodexPolicy{}, true, ""); err == nil {
			t.Fatalf("accepted weak or ambiguous prefix: %q", text)
		}
	}
	if _, err := parseManagedStatusObservation(base, "/tmp/project", CodexServiceSnapshot{}, ManagedCodexPolicy{}, false, ""); !errors.Is(err, ErrManagedNarrow) {
		t.Fatalf("non-prefix parser error=%v", err)
	}
}

func TestParseObservedZeroTurnStatusUsesStrongExactTitlePrefix(t *testing.T) {
	text := `/status
╭──────────────────────────────────────────╮
│ >_ OpenAI Codex (v0.154.0)               │
│ Model: gpt-5.6-sol (reasoning medium)     │
│ Directory: /tmp/project                  │
╰──────────────────────────────────────────╯`
	prefix := "01a0aecc-acb3-7ea1-9224-a643d"
	observed, err := parseManagedStatusObservation(text, "/tmp/project", CodexServiceSnapshot{}, ManagedCodexPolicy{}, true, prefix)
	if err != nil || observed.identity != prefix || observed.policy.Model != "gpt-5.6-sol" || observed.policy.ReasoningEffort != "medium" {
		t.Fatalf("observation=%+v error=%v", observed, err)
	}
	for _, invalid := range []string{"", "01a0aecc-acb3-7ea1-9224-a64"} {
		if _, err := parseManagedStatusObservation(text, "/tmp/project", CodexServiceSnapshot{}, ManagedCodexPolicy{}, true, invalid); err == nil {
			t.Fatalf("accepted fallback prefix %q", invalid)
		}
	}
	if _, err := parseManagedStatusObservation(strings.Replace(text, "╰", " ", 1), "/tmp/project", CodexServiceSnapshot{}, ManagedCodexPolicy{}, true, prefix); err == nil {
		t.Fatal("accepted incomplete status block")
	}
	if _, err := parseManagedStatusObservation(strings.Replace(text, "╯", " ", 1), "/tmp/project", CodexServiceSnapshot{}, ManagedCodexPolicy{}, true, prefix); err == nil {
		t.Fatal("accepted status block without lower border")
	}
	if _, err := parseManagedStatusObservation(text, "/tmp/project", CodexServiceSnapshot{}, ManagedCodexPolicy{}, false, prefix); err == nil {
		t.Fatal("accepted fallback prefix when prefix discovery was disabled")
	}
}

func TestParseObservedNarrowStatusUsesOnlyCompatibleKnownValues(t *testing.T) {
	text := `/status
╭───────────────────────────────────────────────────╮
│  >_ OpenAI Codex (v0.154.0)                       │
│  Model:                gpt-5.6-sol (reasoning med │
│  Directory:            …/herdr-attention-sessions │
│  Session:              01a0c4e0-c206-7061-a45c-d0 │
╰───────────────────────────────────────────────────╯`
	cwd := "/home/raul/projects/herdr-attention-sessions"
	prefix := "01a0c4e0-c206-7061-a45c-d0abc"
	displayedPrefix := "01a0c4e0-c206-7061-a45c-d0"
	policy := ManagedCodexPolicy{Model: "gpt-5.6-sol", ReasoningEffort: "medium"}
	observed, err := parseManagedStatusObservation(text, cwd, CodexServiceSnapshot{}, policy, true, prefix)
	if err != nil || observed.identity != displayedPrefix || observed.policy != policy {
		t.Fatalf("observation=%+v error=%v", observed, err)
	}
	for _, changed := range []string{
		strings.Replace(text, "a45c-d0", "a45c-ff", 1),
		strings.Replace(text, "…/herdr-attention-sessions", "…/other-sessions", 1),
		strings.Replace(text, "reasoning med", "reasoning max", 1),
		text + "\n│ Session: 01a0c4e0-c206-7061-a45c-d0 │",
	} {
		if _, err := parseManagedStatusObservation(changed, cwd, CodexServiceSnapshot{}, policy, true, prefix); err == nil {
			t.Fatalf("accepted incompatible narrow status: %q", changed)
		}
	}
	withoutPolicy, err := parseManagedStatusObservation(text, cwd, CodexServiceSnapshot{}, ManagedCodexPolicy{}, true, prefix)
	wantObserved := ManagedCodexPolicy{Model: "gpt-5.6-sol", ReasoningEffort: "med"}
	if err != nil || withoutPolicy.identity != displayedPrefix || withoutPolicy.policy != wantObserved || !withoutPolicy.policyIsPrefix {
		t.Fatalf("policy-free observation=%+v error=%v", withoutPolicy, err)
	}
}

func TestParseManagedStatusRejectsTruncationDriftAndAmbiguity(t *testing.T) {
	base := `/status
│ >_ OpenAI Codex (v0.154.0) │
│ Remote: unix:///tmp/codex-home/app-server-control/app-server-control.sock (v0.154.0) │
│ Model: gpt-5.6-sol (reasoning medium, summaries detailed) │
│ Directory: /tmp/project │
│ Session: 01a09f5c-37cc-77f1-9409-e9aa070ad8d0 │`
	tests := []string{
		strings.Replace(base, "01a09f5c-37cc-77f1-9409-e9aa070ad8d0", "01a09f5c-37cc-77f1-9409-e9aa0...", 1),
		strings.Replace(base, "v0.154.0", "v0.155.0", 1),
		strings.Replace(base, "app-server-control.sock", "other.sock", 1),
		strings.Replace(base, "Directory: /tmp/project", "Directory: /tmp/project-other", 1),
		strings.Replace(base, "reasoning medium", "reasoning mediumx", 1),
		base + "\nSession: 01a09f5c-37cc-77f1-9409-e9aa070ad8d1",
	}
	for i, text := range tests {
		_, err := parseManagedStatus(text, "/tmp/project", managedServiceSnapshot(), managedPolicy())
		want := ErrManagedNarrow
		if i == len(tests)-1 {
			want = ErrManagedAmbiguous
		}
		if !errors.Is(err, want) {
			t.Fatalf("case %d error=%v", i, err)
		}
	}
}

func TestParseManagedStatusAcceptsLabeledForkParentButRejectsUnlabeledUUID(t *testing.T) {
	base := `/status
│ >_ OpenAI Codex (v0.154.0) │
│ Remote: unix:///tmp/codex-home/app-server-control/app-server-control.sock (v0.154.0) │
│ Model: gpt-5.6-sol (reasoning medium, summaries detailed) │
│ Directory: /tmp/project │
│ Session: 01a09f5c-37cc-77f1-9409-e9aa070ad8d0 │`
	withParent := base + "\n│ Forked from: 01a09f5c-37cc-77f1-9409-e9aa070ad8d1 │"
	id, err := parseManagedStatus(withParent, "/tmp/project", managedServiceSnapshot(), managedPolicy())
	if err != nil || id != "01a09f5c-37cc-77f1-9409-e9aa070ad8d0" {
		t.Fatalf("id=%q error=%v", id, err)
	}
	if _, err := parseManagedStatus(withParent+"\n01a09f5c-37cc-77f1-9409-e9aa070ad8d2", "/tmp/project", managedServiceSnapshot(), managedPolicy()); !errors.Is(err, ErrManagedAmbiguous) {
		t.Fatalf("unlabeled UUID error=%v", err)
	}
}

func TestEmptyCodexComposerRejectsSingleAndMultilineDrafts(t *testing.T) {
	// Observed zero-turn Codex 0.154.0 ANSI: no context item until the first turn.
	if !emptyCodexComposer("\x1b[0m\x1b[1m›\x1b[0m \x1b[0m\x1b[2mAsk Codex to do anything\x1b[0m\r\n \r\n  gpt-5.6-sol medium · ~/projects/herdr-plugin-kit") {
		t.Fatal("observed zero-turn status footer rejected")
	}
	if !emptyCodexComposer(string(managedEmptyComposer("header"))) {
		t.Fatal("empty composer rejected")
	}
	if !emptyCodexComposer("header\n\x1b[1m›\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m\n \n  gpt-5.6-sol medium · ~/projects/herdr-plugin-kit · Context 100% left") {
		t.Fatal("Codex 0.154.0 unstyled status footer rejected")
	}
	if !emptyCodexComposer("header\n\x1b[1m›\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m\n \n  gpt-5.6-sol medium · ~ · weekly 23% left · Context 100% left") {
		t.Fatal("Codex 0.154.0 home-directory status footer rejected")
	}
	for _, text := range []string{
		"header\n› draft text\n status",
		"header\n› first draft line\n  second draft line\n status",
		"header\n\x1b[1m›\x1b[0m Ask Codex to do anything",
		"header\n\x1b[1m›\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m\ncontinued draft",
		"header\n\x1b[1m›\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m\n  user continuation",
		"header\n\x1b[1m›\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m\n  first unstyled line\n  second unstyled line",
		"header\n\x1b[1m›\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m\n  \x1b[1munexpected styled footer\x1b[0m",
		"header\n\x1b[1m›\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m\n  gpt-5.6-sol medium · ~raul · Context 100% left",
		"header\n\x1b[1m›\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m\n  gpt-5.6-sol medium · relative · Context 100% left",
		"header without composer",
	} {
		if emptyCodexComposer(text) {
			t.Fatalf("draft accepted: %q", text)
		}
	}
}

func TestCodexComposerParsersInspectTheFirstAndOnlyLine(t *testing.T) {
	if !exactCodexSingleLineDraft("› /status", "/status") {
		t.Fatal("single-line status draft was rejected")
	}
	if composer, ok := codexComposer("› /status"); !ok || composer != "/status" {
		t.Fatalf("single-line composer=%q ok=%t", composer, ok)
	}
}

func TestExactCodexStatusDraftAcceptsObserved01540MenuAndRejectsAmbiguity(t *testing.T) {
	observed := "header\n\x1b[1m›\x1b[0m /status\n\n  /status      show current session configuration and token usage\n  /\x1b[0m\x1b[1mstatus\x1b[0mline  \x1b[0m\x1b[2mconfigure which items appear in the status line\x1b[0m"
	compact := "header\n\x1b[1m›\x1b[0m /status\n\n  \x1b[1m\x1b[38;5;6m/status      show current session configuration and token\x1b[0m\n  \x1b[1m\x1b[38;5;6m             usage\x1b[0m"
	narrow := "header\n\x1b[1m›\x1b[0m /status\n\n  /status      show current session configuration and\n               token usage\n  /\x1b[0m\x1b[1mstatus\x1b[0mline  \x1b[0m\x1b[2mconfigure which items appear in the\x1b[0m\n               \x1b[0m\x1b[2mstatus line\x1b[0m"
	if !exactCodexSingleLineDraft(observed, "/status") || !exactCodexSingleLineDraft(compact, "/status") || !exactCodexSingleLineDraft(narrow, "/status") {
		t.Fatal("Codex 0.154.0 status menu rejected")
	}
	help := strings.Replace(observed, "›\x1b[0m /status\n", "›\x1b[0m /help\n", 1)
	if exactCodexSingleLineDraft(help, "/help") {
		t.Fatal("status autocomplete accepted for a different command")
	}
	for _, text := range []string{
		strings.ReplaceAll(observed, "\x1b[0m\x1b[1m", ""),
		strings.ReplaceAll(narrow, "\x1b[0m\x1b[1m", ""),
		managedANSI.ReplaceAllString(compact, ""),
		observed + "\n  extra menu row",
		narrow + "\n  extra menu row",
		strings.Replace(observed, "token usage", "token totals", 1),
	} {
		if exactCodexSingleLineDraft(text, "/status") {
			t.Fatalf("ambiguous status menu accepted: %q", text)
		}
	}
}

func TestDiscoverManagedIdentityUsesExactNamedPaneStatusAndStableReadback(t *testing.T) {
	target := sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	withoutID := managedAgent(target, "/tmp/project", "unused", false)
	withoutID.Session = nil
	withID := managedAgent(target, "/tmp/project", "01a09f5c-37cc-77f1-9409-e9aa070ad8d0", false)
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, withoutID),
		{Stdout: managedEmptyComposer("header")},
		paneScrollResult(t, target, 10),
		{Stdout: []byte("old output")},
		managedAgentResult(t, withoutID),
		{Stdout: managedEmptyComposer("header")},
		{},
		managedAgentResult(t, withoutID),
		{Stdout: managedStatusDraft("header")},
		managedAgentResult(t, withoutID),
		{Stdout: managedStatusDraft("header")},
		{},
		paneScrollResult(t, target, 40),
		{Stdout: []byte("old output\n› " + observedManagedStatus())},
		{Stdout: managedEmptyComposer("status block")},
	}}
	topology := &fakeManagedTopology{}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	launcher.topology = topology
	argv := []string{"codex", "--remote", "unix:///tmp/codex-home/app-server-control/app-server-control.sock"}
	agent, id, err := launcher.discoverManagedIdentity(managedIdentityTestContext(t), target, "/tmp/project", argv, managedServiceSnapshot(), managedPolicy())
	if err != nil || id != withID.Session.Value || agent.Session != nil {
		t.Fatalf("agent=%+v id=%q error=%v", agent, id, err)
	}
	wantSend := []string{"herdr", "--session", "named", "pane", "send-text", "w1:p2", "/status"}
	wantEnter := []string{"herdr", "--session", "named", "pane", "send-keys", "w1:p2", "enter"}
	if len(runner.calls) != 15 || !reflect.DeepEqual(runner.calls[6], wantSend) || !reflect.DeepEqual(runner.calls[11], wantEnter) {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestDiscoverManagedIdentityPrefixRechecksFinalPaneAndProcess(t *testing.T) {
	target := sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	unbound := managedAgent(target, "/tmp/project", "unused", false)
	unbound.Session = nil
	prefix := "01a09f5c-37cc-77f1-9409-e9aa070a"
	withTitle := func(agent Agent) command.Result {
		wire := wireFromAgent(agent)
		wire.TerminalTitleStripped = prefix + "..."
		var response agentInfoResponse
		response.ID, response.Result.Type, response.Result.Agent = "cli:agent:get", "agent_info", wire
		return jsonResult(t, response)
	}
	status := strings.Replace(observedManagedStatus(), managedNewID, prefix, 1)
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, unbound),
		{Stdout: managedEmptyComposer("header")},
		paneScrollResult(t, target, 10),
		{Stdout: []byte("old output")},
		withTitle(unbound),
		{Stdout: managedEmptyComposer("header")},
		{},
		managedAgentResult(t, unbound),
		{Stdout: managedStatusDraft("header")},
		managedAgentResult(t, unbound),
		{Stdout: managedStatusDraft("header")},
		{},
		paneScrollResult(t, target, 40),
		{Stdout: []byte("old output\n› " + status)},
		{Stdout: managedEmptyComposer("status block")},
		withTitle(unbound),
	}}
	topology := &fakeManagedTopology{}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	launcher.topology = topology
	argv := []string{"codex", "--remote", "unix:///tmp/codex-home/app-server-control/app-server-control.sock"}
	agent, observed, err := launcher.discoverManagedIdentityObservation(managedIdentityTestContext(t), target, "/tmp/project", argv, managedServiceSnapshot(), managedPolicy(), true)
	if err != nil || observed.identity != prefix || agent.Session != nil {
		t.Fatalf("agent=%+v observation=%+v error=%v", agent, observed, err)
	}
	if len(runner.calls) != 16 || len(topology.verifications) != 5 {
		t.Fatalf("calls=%d verifications=%d", len(runner.calls), len(topology.verifications))
	}
}

func TestDiscoverOrdinaryManagedIdentityAllowsLocalStatusWithReadyService(t *testing.T) {
	target := sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	unbound := managedAgent(target, "/tmp/project", "unused", false)
	unbound.Session = nil
	prefix := "01a09f5c-37cc-77f1-9409-e9aa070a"
	withTitle := func(agent Agent) command.Result {
		wire := wireFromAgent(agent)
		wire.TerminalTitleStripped = prefix + "..."
		var response agentInfoResponse
		response.ID, response.Result.Type, response.Result.Agent = "cli:agent:get", "agent_info", wire
		return jsonResult(t, response)
	}
	status := strings.Replace(observedManagedStatus(), managedNewID, prefix, 1)
	status = strings.Replace(status, "│ Remote: unix:///tmp/codex-home/app-server-control/app-server-control.sock (v0.154.0) │\n", "", 1)
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, unbound),
		{Stdout: managedEmptyComposer("header")},
		paneScrollResult(t, target, 10),
		{Stdout: []byte("old output")},
		withTitle(unbound),
		{Stdout: managedEmptyComposer("header")},
		{},
		managedAgentResult(t, unbound),
		{Stdout: managedStatusDraft("header")},
		managedAgentResult(t, unbound),
		{Stdout: managedStatusDraft("header")},
		{},
		paneScrollResult(t, target, 40),
		{Stdout: []byte("old output\n› " + status)},
		{Stdout: managedEmptyComposer("status block")},
		withTitle(unbound),
	}}
	topology := &fakeManagedTopology{}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	launcher.topology = topology
	agent, observed, err := launcher.discoverOrdinaryManagedIdentityObservation(managedIdentityTestContext(t), target, "/tmp/project", []string{"codex", "--yolo"}, managedServiceSnapshot(), managedPolicy(), true)
	if err != nil || observed.identity != prefix || agent.Session != nil {
		t.Fatalf("agent=%+v observation=%+v error=%v", agent, observed, err)
	}
	if len(runner.calls) != 16 || len(topology.verifications) != 5 {
		t.Fatalf("calls=%d verifications=%d", len(runner.calls), len(topology.verifications))
	}
}

func TestDiscoverManagedIdentityNeverSendsInputWhileDraftPresent(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	agent := managedAgent(target, "/tmp/project", "unused", false)
	agent.Session = nil
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, agent),
		{Stdout: []byte("header\n› user draft\n status")},
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	launcher.topology = &fakeManagedTopology{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := launcher.discoverManagedIdentity(ctx, target, "/tmp/project", []string{"codex"}, managedServiceSnapshot(), managedPolicy())
	if !errors.Is(err, ErrManagedPaneBusy) || len(runner.calls) != 2 {
		t.Fatalf("error=%v calls=%#v", err, runner.calls)
	}
}

func TestDiscoverManagedIdentityNeverEntersMultilineStatusDraft(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	agent := managedAgent(target, "/tmp/project", "unused", false)
	agent.Session = nil
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, agent), {Stdout: managedEmptyComposer("header")},
		paneScrollResult(t, target, 0), {Stdout: []byte("old output")},
		managedAgentResult(t, agent), {Stdout: managedEmptyComposer("header")},
		{},
		managedAgentResult(t, agent), {Stdout: []byte("header\n\x1b[1m›\x1b[0m /status\n\n  user continuation\n")},
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	launcher.topology = &fakeManagedTopology{}
	if _, _, err := launcher.discoverManagedIdentity(managedIdentityTestContext(t), target, "/tmp/project", []string{"codex"}, managedServiceSnapshot(), managedPolicy()); !errors.Is(err, ErrManagedStale) {
		t.Fatalf("error=%v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call, " "), "send-keys") {
			t.Fatalf("Enter was sent: %v", runner.calls)
		}
	}
}

func TestDiscoverManagedIdentityDoesNotLetReportedSessionBypassDraftGuard(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	agent := managedAgent(target, "/tmp/project", "01a09f5c-37cc-77f1-9409-e9aa070ad8d0", false)
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, agent),
		{Stdout: []byte("header\n› existing user draft\n status")},
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	launcher.topology = &fakeManagedTopology{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := launcher.discoverManagedIdentity(ctx, target, "/tmp/project", []string{"codex"}, managedServiceSnapshot(), managedPolicy())
	if !errors.Is(err, ErrManagedPaneBusy) || len(runner.calls) != 2 {
		t.Fatalf("error=%v calls=%#v", err, runner.calls)
	}
}

func TestDiscoverManagedIdentityAcceptsReportedSessionOnlyAfterStableEmptyComposer(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	agent := managedAgent(target, "/tmp/project", "01a09f5c-37cc-77f1-9409-e9aa070ad8d0", false)
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, agent),
		{Stdout: managedEmptyComposer("header")},
		managedAgentResult(t, agent),
		{Stdout: managedEmptyComposer("header")},
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	topology := &fakeManagedTopology{}
	launcher.topology = topology
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	got, id, err := launcher.discoverManagedIdentity(ctx, target, "/tmp/project", []string{"codex"}, managedServiceSnapshot(), managedPolicy())
	if err != nil || id != agent.Session.Value || got.Session == nil || got.Session.Value != id {
		t.Fatalf("agent=%+v id=%q error=%v", got, id, err)
	}
	if len(runner.calls) != 4 || len(topology.verifications) != 2 {
		t.Fatalf("calls=%#v verifications=%#v", runner.calls, topology.verifications)
	}
}

func TestDiscoverManagedIdentityRejectsSecondNonQuiescentAgentRead(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	agent := managedAgent(target, "/tmp/project", managedNewID, false)
	busy := agent
	busy.Status = "working"
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, agent), {Stdout: managedEmptyComposer("header")},
		managedAgentResult(t, busy), {Stdout: managedEmptyComposer("header")},
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	launcher.topology = &fakeManagedTopology{}
	if _, _, err := launcher.discoverManagedIdentity(managedIdentityTestContext(t), target, "/tmp/project", []string{"codex"}, managedServiceSnapshot(), managedPolicy()); !errors.Is(err, ErrManagedPaneBusy) {
		t.Fatalf("non-quiescent second read error=%v", err)
	}
}

func TestDiscoverManagedIdentityPreservesSecondAgentReadFailure(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	agent := managedAgent(target, "/tmp/project", "01a09f5c-37cc-77f1-9409-e9aa070ad8d0", false)
	want := errors.New("second agent read failed")
	runner := &scriptedRunner{
		results: []command.Result{managedAgentResult(t, agent), {Stdout: managedEmptyComposer("header")}, {}},
		errs:    []error{nil, nil, want},
	}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	launcher.topology = &fakeManagedTopology{}
	_, _, err := launcher.discoverManagedIdentity(managedIdentityTestContext(t), target, "/tmp/project", []string{"codex"}, managedServiceSnapshot(), managedPolicy())
	if !errors.Is(err, want) || errors.Is(err, ErrManagedPaneBusy) {
		t.Fatalf("error=%v", err)
	}
}

func TestWaitForManagedProcessTimesOutAndRejectsAmbiguity(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	for _, processErr := range []error{sessionhost.ErrStaleTarget, sessionhost.ErrAmbiguousTarget} {
		launcher := NewManagedCodexLauncher(Host{Runner: &scriptedRunner{}})
		// A finite run of failures, then success: a wait that keeps polling after its timeout ends in a
		// process instead of spinning for ever, so the check below fails instead of hanging.
		launcher.topology = &fakeManagedTopology{verifyErrs: slices.Repeat([]error{processErr}, 50)}
		_, err := launcher.waitForProcess(context.Background(), target, "/tmp/project", []string{"codex"}, time.Millisecond)
		if err == nil {
			t.Fatalf("process error %v accepted", processErr)
		}
	}
}

func TestDiscoverManagedIdentityRejectsEachPreInputDrift(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	base := managedAgent(target, "/tmp/project", "unused", false)
	base.Session = nil
	busy := base
	busy.Focused = true
	driftedPane := base
	driftedPane.PaneID = "w1:p3"
	driftedStatus := base
	driftedStatus.Status = "done"
	driftedRevision := base
	driftedRevision.Revision++
	driftedSequence := base
	driftedSequence.StateChangeSeq++
	recheck := func(agent Agent) []command.Result {
		return []command.Result{
			managedAgentResult(t, base), {Stdout: managedEmptyComposer("header")},
			paneScrollResult(t, target, 0), {Stdout: []byte("old")}, managedAgentResult(t, agent),
		}
	}
	tests := []struct {
		name    string
		results []command.Result
	}{
		{"busy initial", []command.Result{managedAgentResult(t, busy)}},
		{"busy recheck", recheck(busy)},
		{"pane recheck", recheck(driftedPane)},
		{"status recheck", recheck(driftedStatus)},
		{"revision recheck", recheck(driftedRevision)},
		{"state sequence recheck", recheck(driftedSequence)},
		{"draft recheck", []command.Result{managedAgentResult(t, base), {Stdout: managedEmptyComposer("header")}, paneScrollResult(t, target, 0), {Stdout: []byte("old")}, managedAgentResult(t, base), {Stdout: []byte("\x1b[1m›\x1b[0m user draft")}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &scriptedRunner{results: test.results}
			launcher := NewManagedCodexLauncher(Host{Runner: runner})
			launcher.topology = &fakeManagedTopology{}
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			defer cancel()
			if _, _, err := launcher.discoverManagedIdentity(ctx, target, "/tmp/project", []string{"codex"}, managedServiceSnapshot(), managedPolicy()); err == nil || hasPaneInput(runner.calls) {
				t.Fatalf("error=%v calls=%v", err, runner.calls)
			}
		})
	}
}

func TestBindManagedIdentityRejectsReportedOrReadbackDrift(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	observed := managedAgent(target, "/tmp/project", managedNewID, false)
	observed.Session.Value = managedSecondID
	launcher := NewManagedCodexLauncher(Host{Runner: &scriptedRunner{}})
	if _, err := launcher.bindManagedIdentity(context.Background(), target, "/tmp/project", observed, managedNewID); !errors.Is(err, ErrManagedStale) {
		t.Fatalf("reported drift error=%v", err)
	}
	observed.Session = nil
	drifted := managedAgent(target, "/tmp/project", managedNewID, false)
	drifted.TabID = "w1:other"
	runner := &scriptedRunner{results: []command.Result{{}, managedAgentResult(t, drifted)}}
	launcher = NewManagedCodexLauncher(Host{Runner: runner})
	if _, err := launcher.bindManagedIdentity(context.Background(), target, "/tmp/project", observed, managedNewID); err == nil {
		t.Fatal("readback drift accepted")
	}
}

func TestAwaitStatusDraftRejectsBusyPaneWithoutInput(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	expected := managedAgent(target, "/tmp/project", "unused", true)
	expected.Session = nil
	runner := &scriptedRunner{results: []command.Result{managedAgentResult(t, expected)}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	launcher.topology = &fakeManagedTopology{}
	if err := launcher.awaitStatusDraft(context.Background(), target, "/tmp/project", []string{"codex"}, expected); !errors.Is(err, ErrManagedPaneBusy) || hasPaneInput(runner.calls) {
		t.Fatalf("error=%v calls=%v", err, runner.calls)
	}
}

func TestAwaitStatusDraftRejectsStatusDriftBeforeProcessRecheck(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	expected := managedAgent(target, "/tmp/project", managedNewID, false)
	drifted := expected
	drifted.Status = "done"
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, drifted), {Stdout: managedStatusDraft("header")},
		managedAgentResult(t, drifted), {Stdout: managedStatusDraft("header")},
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	topology := &fakeManagedTopology{}
	launcher.topology = topology
	if err := launcher.awaitStatusDraft(managedIdentityTestContext(t), target, "/tmp/project", []string{"codex"}, expected); !errors.Is(err, ErrManagedStale) {
		t.Fatalf("status drift error=%v", err)
	}
	if len(topology.verifications) != 0 {
		t.Fatalf("status drift reached process verification: %q", topology.verifications)
	}
}

func TestDiscoverManagedIdentityAcceptsStableDoneReportedSession(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	agent := managedAgent(target, "/tmp/project", "01a09f5c-37cc-77f1-9409-e9aa070ad8d0", false)
	agent.Status = "done"
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, agent), {Stdout: managedEmptyComposer("header")},
		managedAgentResult(t, agent), {Stdout: managedEmptyComposer("header")},
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	launcher.topology = &fakeManagedTopology{}
	got, id, err := launcher.discoverManagedIdentity(managedIdentityTestContext(t), target, "/tmp/project", []string{"codex"}, managedServiceSnapshot(), managedPolicy())
	if err != nil || got.Status != "done" || id != agent.Session.Value {
		t.Fatalf("agent=%+v id=%q error=%v", got, id, err)
	}
}

func TestDiscoverManagedIdentityRejectsDraftAppearingOnSecondComposerRead(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	agent := managedAgent(target, "/tmp/project", "01a09f5c-37cc-77f1-9409-e9aa070ad8d0", false)
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, agent), {Stdout: managedEmptyComposer("header")},
		managedAgentResult(t, agent), {Stdout: []byte("header\n\x1b[1m›\x1b[0m user draft")},
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	launcher.topology = &fakeManagedTopology{}
	if _, _, err := launcher.discoverManagedIdentity(managedIdentityTestContext(t), target, "/tmp/project", []string{"codex"}, managedServiceSnapshot(), managedPolicy()); !errors.Is(err, ErrManagedPaneBusy) {
		t.Fatalf("error=%v", err)
	}
}

func TestAwaitFreshStatusRejectsOldBlockWithUnrelatedNewOutput(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	before := observedManagedStatus()
	scroll := paneScrollResult(t, target, 10)
	history := command.Result{Stdout: []byte(before + "\nunrelated output")}
	reads := 0
	// Polling may observe the same snapshot again before the deadline is handled.
	// Keep observations repeatable; the wait loop owns cancellation.
	runner := runnerFunc(func(_ context.Context, executable string, args []string) (command.Result, error) {
		if executable == "herdr" {
			switch {
			case reflect.DeepEqual(args, []string{"--session", "default", "pane", "get", target.PaneID}):
				return scroll, nil
			case reflect.DeepEqual(args, []string{"--session", "default", "pane", "read", target.PaneID, "--source", "recent-unwrapped", "--lines", "50", "--format", "text"}):
				reads++
				if reads > 100 {
					// A wait that ignores the ended context would poll forever; fail instead of hanging.
					t.Errorf("polling continued after the context ended: %d reads", reads)
					return command.Result{}, errors.New("polling did not stop")
				}
				return history, nil
			}
		}
		t.Fatalf("unexpected command: %s %q", executable, args)
		return command.Result{}, errors.New("unexpected command")
	})
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if id, err := launcher.awaitFreshStatus(ctx, target, before, "/tmp/project", managedServiceSnapshot(), managedPolicy()); !errors.Is(err, ErrManagedStale) || id != "" || reads == 0 {
		t.Fatalf("identity=%q reads=%d error=%v", id, reads, err)
	}
}

func TestAwaitFreshStatusRejectsValidNewBlockWhenComposerIsNotEmpty(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	runner := &scriptedRunner{results: []command.Result{
		paneScrollResult(t, target, 10), {Stdout: []byte("old\n› " + observedManagedStatus())}, {Stdout: []byte("\x1b[1m›\x1b[0m user draft")},
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	if _, err := launcher.awaitFreshStatus(context.Background(), target, "old", "/tmp/project", managedServiceSnapshot(), managedPolicy()); !errors.Is(err, ErrManagedStale) {
		t.Fatalf("error=%v", err)
	}
}

func TestAwaitFreshStatusWaitsForStatusBlockAfterCommandEcho(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	runner := &scriptedRunner{results: []command.Result{
		paneScrollResult(t, target, 10), {Stdout: []byte("old\n› /status\n")},
		paneScrollResult(t, target, 20), {Stdout: []byte("old\n› " + observedManagedStatus())},
		{Stdout: managedEmptyComposer("status block")},
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	id, err := launcher.awaitFreshStatus(context.Background(), target, "old", "/tmp/project", managedServiceSnapshot(), managedPolicy())
	if err != nil || id != managedNewID || len(runner.calls) != 5 {
		t.Fatalf("id=%q calls=%#v error=%v", id, runner.calls, err)
	}
}

func TestAwaitFreshStatusCountsEveryPriorCommandMarker(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	prior := observedManagedStatus() + "\n" + strings.Replace(observedManagedStatus(), managedNewID, managedSecondID, 1)
	history := prior + "\n› " + observedManagedStatus()
	runner := &scriptedRunner{results: []command.Result{
		paneScrollResult(t, target, 30), {Stdout: []byte(history)}, {Stdout: managedEmptyComposer("status block")},
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	id, err := launcher.awaitFreshStatus(ctx, target, prior, "/tmp/project", managedServiceSnapshot(), managedPolicy())
	if err != nil || id != managedNewID || len(runner.calls) != 3 {
		t.Fatalf("id=%q calls=%#v error=%v", id, runner.calls, err)
	}
}

func TestComposerParsersCoverEmptyAndHomeDisplayBranches(t *testing.T) {
	if value, ok := codexComposer("header\n› /status\n"); !ok || value != "/status" {
		t.Fatalf("composer=%q ok=%t", value, ok)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(observedManagedStatus(), "/tmp/project", "~")
	if _, err := parseManagedStatus(text, home, managedServiceSnapshot(), managedPolicy()); err != nil {
		t.Fatalf("home status error=%v", err)
	}
}

func TestReadPaneExtentRejectsMismatchedPaneID(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	mismatched := target
	mismatched.PaneID = "w1:other"
	runner := &scriptedRunner{results: []command.Result{paneScrollResult(t, mismatched, 10)}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	if _, err := launcher.readPaneExtent(context.Background(), target); !errors.Is(err, ErrManagedStale) {
		t.Fatalf("mismatched pane error=%v", err)
	}
}

func TestReadPaneAcceptsExactManagedByteLimit(t *testing.T) {
	text := strings.Repeat("x", managedPaneBytes)
	runner := &scriptedRunner{results: []command.Result{{Stdout: []byte(text)}}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	got, err := launcher.readPaneANSI(context.Background(), "w1:p2", "visible", 1)
	if err != nil || got != text {
		t.Fatalf("exact byte limit text length=%d error=%v", len(got), err)
	}
}

func TestDiscoverManagedIdentityRetriesUnregisteredAgentOnlyWhileExactProcessLives(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	agent := managedAgent(target, "/tmp/project", "01a09f5c-37cc-77f1-9409-e9aa070ad8d0", false)
	runner := &scriptedRunner{
		results: []command.Result{{}, managedAgentResult(t, agent), {Stdout: managedEmptyComposer("header")}, managedAgentResult(t, agent), {Stdout: managedEmptyComposer("header")}},
		errs:    []error{errors.New("agent_not_found"), nil, nil, nil, nil},
	}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	topology := &fakeManagedTopology{}
	launcher.topology = topology
	started := time.Now()
	got, id, err := launcher.discoverManagedIdentity(managedIdentityTestContext(t), target, "/tmp/project", []string{"codex"}, managedServiceSnapshot(), managedPolicy())
	if err != nil || id != agent.Session.Value || got.Session == nil || got.Session.Value != id {
		t.Fatalf("agent=%+v id=%q error=%v", got, id, err)
	}
	if elapsed := time.Since(started); elapsed < 15*time.Millisecond {
		t.Fatalf("unregistered-agent retry spun without backoff: %v", elapsed)
	}
	if len(runner.calls) != 5 || len(topology.verifications) != 3 {
		t.Fatalf("calls=%#v verifications=%#v", runner.calls, topology.verifications)
	}
}

func TestDiscoverManagedIdentityNarrowStatusSendsNoFurtherInput(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	agent := managedAgent(target, "/tmp/project", "unused", false)
	agent.Session = nil
	runner := &scriptedRunner{results: []command.Result{
		managedAgentResult(t, agent),
		{Stdout: managedEmptyComposer("header")},
		paneScrollResult(t, target, 10),
		{Stdout: []byte("old output")},
		managedAgentResult(t, agent),
		{Stdout: managedEmptyComposer("header")},
		{},
		managedAgentResult(t, agent),
		{Stdout: managedStatusDraft("header")},
		managedAgentResult(t, agent),
		{Stdout: managedStatusDraft("header")},
		{},
		paneScrollResult(t, target, 11),
		{Stdout: []byte("old output\n› /status\nSession: 01a09f5c-37cc-77f1-9409-e9aa0...")},
	}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	launcher.topology = &fakeManagedTopology{}
	_, _, err := launcher.discoverManagedIdentity(managedIdentityTestContext(t), target, "/tmp/project", []string{"codex"}, managedServiceSnapshot(), managedPolicy())
	if !errors.Is(err, ErrManagedNarrow) || len(runner.calls) != 14 {
		t.Fatalf("error=%v calls=%#v", err, runner.calls)
	}
}

func TestManagedCodexIdentityWaitForProcessBacksOffBetweenRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
		topology := &fakeManagedTopology{verifyErrs: []error{sessionhost.ErrStaleTarget, sessionhost.ErrStaleTarget}}
		launcher := NewManagedCodexLauncher(Host{Runner: &scriptedRunner{}})
		launcher.topology = topology
		started := time.Now()
		process, err := launcher.waitForProcess(context.Background(), target, "/tmp/project", []string{"codex"}, time.Minute)
		if err != nil || process.PID == 0 || len(topology.verifications) != 3 {
			t.Fatalf("process=%+v verifications=%d error=%v", process, len(topology.verifications), err)
		}
		if elapsed := time.Since(started); elapsed < 2*25*time.Millisecond {
			t.Fatalf("two process retries waited only %v", elapsed)
		}
	})
}

func TestManagedCodexIdentityAwaitStatusDraftBacksOffBetweenReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
		expected := managedAgent(target, "/tmp/project", managedNewID, false)
		runner := &scriptedRunner{results: []command.Result{
			managedAgentResult(t, expected), {Stdout: managedStatusDraft("header")},
			managedAgentResult(t, expected), {Stdout: managedStatusDraft("header")},
		}}
		launcher := NewManagedCodexLauncher(Host{Runner: runner})
		launcher.topology = &fakeManagedTopology{}
		started := time.Now()
		if err := launcher.awaitStatusDraft(context.Background(), target, "/tmp/project", []string{"codex"}, expected); err != nil {
			t.Fatalf("stable draft rejected: %v", err)
		}
		if elapsed := time.Since(started); elapsed < 25*time.Millisecond || len(runner.calls) != 4 {
			t.Fatalf("draft confirmed after %v with %d reads", elapsed, len(runner.calls))
		}
	})
}

func TestManagedCodexIdentityAwaitFreshStatusBacksOffUntilStatusBlockAppears(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
		runner := &scriptedRunner{results: []command.Result{
			paneScrollResult(t, target, 10), {Stdout: []byte("old\n› /status\n")},
			paneScrollResult(t, target, 20), {Stdout: []byte("old\n› " + observedManagedStatus())},
			{Stdout: managedEmptyComposer("status block")},
		}}
		launcher := NewManagedCodexLauncher(Host{Runner: runner})
		started := time.Now()
		id, err := launcher.awaitFreshStatus(context.Background(), target, "old", "/tmp/project", managedServiceSnapshot(), managedPolicy())
		if err != nil || id != managedNewID || len(runner.calls) != 5 {
			t.Fatalf("id=%q calls=%d error=%v", id, len(runner.calls), err)
		}
		if elapsed := time.Since(started); elapsed < 25*time.Millisecond {
			t.Fatalf("status poll retried after only %v", elapsed)
		}
	})
}

func TestManagedCodexIdentityFinalComposerErrorNamesConversionOnlyForExpectedSource(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	agent := managedAgent(target, "/tmp/project", managedNewID, false)
	agent.Revision, agent.StateChangeSeq = 1, 1
	process := sessionhost.ForegroundProcess{PID: 77, Name: "codex", CWD: agent.CWD, Argv: []string{"codex"}}
	arrange := func() ManagedCodexLauncher {
		runner := &scriptedRunner{results: []command.Result{
			managedAgentResult(t, agent), {Stdout: managedEmptyComposer("header")},
			managedAgentResult(t, agent), {Stdout: []byte("header\n\x1b[1m›\x1b[0m user draft")},
		}}
		launcher := NewManagedCodexLauncher(Host{Runner: runner})
		launcher.topology = &fakeManagedTopology{inspected: process}
		return launcher
	}

	_, _, err := arrange().discoverManagedIdentity(managedIdentityTestContext(t), target, "/tmp/project", process.Argv, managedServiceSnapshot(), managedPolicy())
	if !errors.Is(err, ErrManagedPaneBusy) || errors.Is(err, ErrCodexConversionComposer) {
		t.Fatalf("launch discovery error=%v", err)
	}
	_, _, err = arrange().discoverManagedIdentityForSource(managedIdentityTestContext(t), target, "/tmp/project", agent, process, managedServiceSnapshot())
	if !errors.Is(err, ErrManagedPaneBusy) || !errors.Is(err, ErrCodexConversionComposer) {
		t.Fatalf("source discovery error=%v", err)
	}
}

func TestManagedCodexIdentityAwaitStatusDraftRejectsNonIdleStatusWithoutInput(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	expected := managedAgent(target, "/tmp/project", managedNewID, false)
	for _, status := range []string{"working", "blocked", "unknown"} {
		t.Run(status, func(t *testing.T) {
			current := expected
			current.Status = status
			runner := &scriptedRunner{results: []command.Result{managedAgentResult(t, current)}}
			launcher := NewManagedCodexLauncher(Host{Runner: runner})
			launcher.topology = &fakeManagedTopology{}
			if err := launcher.awaitStatusDraft(context.Background(), target, "/tmp/project", []string{"codex"}, expected); !errors.Is(err, ErrManagedPaneBusy) || hasPaneInput(runner.calls) {
				t.Fatalf("error=%v calls=%v", err, runner.calls)
			}
		})
	}
}

func TestManagedCodexIdentityBindRejectsObservedPaneThatDiffersFromTarget(t *testing.T) {
	target := sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	bound := managedAgent(target, "/tmp/project", managedNewID, false)
	observed := bound
	observed.TabID = "w1:t9"
	observed.Session = nil
	runner := &scriptedRunner{results: []command.Result{{}, managedAgentResult(t, bound), managedAgentResult(t, bound)}}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	agent, err := launcher.bindManagedIdentity(context.Background(), target, "/tmp/project", observed, managedNewID)
	if !errors.Is(err, ErrManagedStale) || agent.PaneID != "" || len(runner.calls) != 2 {
		t.Fatalf("agent=%+v error=%v calls=%d", agent, err, len(runner.calls))
	}
}

func TestManagedCodexIdentityBindRejectsSecondReadThatDrifts(t *testing.T) {
	target := sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	bound := managedAgent(target, "/tmp/project", managedNewID, false)
	recased := wireFromAgent(bound)
	recased.Agent = "CODEX"
	for _, test := range []struct {
		name   string
		second command.Result
		err    error
		stable bool
	}{
		{"unchanged", managedAgentResult(t, bound), nil, true},
		{"kind spelling changed", managedWireAgentResult(t, recased), nil, false},
		{"read failed", command.Result{}, errors.New("agent get failed"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &scriptedRunner{results: []command.Result{managedAgentResult(t, bound), test.second}, errs: []error{nil, test.err}}
			launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
			agent, err := launcher.bindManagedIdentity(context.Background(), target, "/tmp/project", bound, managedNewID)
			if test.stable {
				if err != nil || agent.Session == nil || agent.Session.Value != managedNewID {
					t.Fatalf("agent=%+v error=%v", agent, err)
				}
				return
			}
			if !errors.Is(err, ErrManagedStale) || agent.PaneID != "" {
				t.Fatalf("agent=%+v error=%v", agent, err)
			}
		})
	}
}

func TestManagedCodexIdentityUnselectedStatusAutocompleteNeedsBothExactRows(t *testing.T) {
	const (
		statusRow     = "/status      show current session configuration and token usage"
		statuslineRow = "/statusline  configure which items appear in the status line"
		styledLine    = "  /\x1b[0m\x1b[1mstatus\x1b[0mline  \x1b[0m\x1b[2mconfigure which items appear in the status line\x1b[0m"
	)
	rawStatus := "  " + statusRow
	for _, test := range []struct {
		name  string
		raw   []string
		plain []string
		want  bool
	}{
		{"observed menu", []string{rawStatus, styledLine}, []string{statusRow, statuslineRow}, true},
		{"second row changed", []string{rawStatus, styledLine}, []string{statusRow, "/statusline configure which items appear in the status bar"}, false},
		{"first row changed", []string{rawStatus, styledLine}, []string{"/status show current session configuration and token totals", statuslineRow}, false},
		{"both rows changed", []string{rawStatus, styledLine}, []string{"/status show something", "/statusline configure something"}, false},
		{"second row not dimmed", []string{rawStatus, strings.ReplaceAll(styledLine, "\x1b[2m", "")}, []string{statusRow, statuslineRow}, false},
		{"styled row first", []string{styledLine, rawStatus}, []string{statuslineRow, statusRow}, false},
		{"no styled row", []string{rawStatus, "  " + statuslineRow}, []string{statusRow, statuslineRow}, false},
		{"styled text before the styled row", []string{"  \x1b[1m" + statusRow + "\x1b[0m", styledLine}, []string{statusRow, statuslineRow}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validUnselectedStatusAutocomplete(test.raw, test.plain); got != test.want {
				t.Fatalf("valid=%t want=%t", got, test.want)
			}
		})
	}
}

func TestManagedCodexIdentityReadManagedAgentRejectsEnvelopeAndTargetDrift(t *testing.T) {
	target := sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	valid := wireFromAgent(managedAgent(target, "/tmp/project", managedNewID, false))
	envelope := func(id, kind string, wire wireAgent) command.Result {
		var response agentInfoResponse
		response.ID, response.Result.Type, response.Result.Agent = id, kind, wire
		return jsonResult(t, response)
	}
	for _, test := range []struct {
		name   string
		mutate func(*sessionhost.Origin, *wireAgent, *string)
		result func(wireAgent) command.Result
		want   error
	}{
		{"exact target", nil, nil, nil},
		{"kind spelling", func(_ *sessionhost.Origin, wire *wireAgent, _ *string) { wire.Agent = "Codex" }, nil, nil},
		{"wrong envelope id", nil, func(wire wireAgent) command.Result { return envelope("cli:agent:list", "agent_info", wire) }, ErrManagedStale},
		{"wrong result type", nil, func(wire wireAgent) command.Result { return envelope("cli:agent:get", "agent_list", wire) }, ErrManagedStale},
		{"other agent kind", func(_ *sessionhost.Origin, wire *wireAgent, _ *string) { wire.Agent = "claude" }, nil, ErrManagedStale},
		{"other herdr session", func(origin *sessionhost.Origin, _ *wireAgent, _ *string) { origin.SessionName = "other" }, nil, ErrManagedStale},
		{"other workspace", func(_ *sessionhost.Origin, wire *wireAgent, _ *string) { wire.WorkspaceID = "w2" }, nil, ErrManagedStale},
		{"other tab", func(_ *sessionhost.Origin, wire *wireAgent, _ *string) { wire.TabID = "w1:t9" }, nil, ErrManagedStale},
		{"other pane", func(_ *sessionhost.Origin, wire *wireAgent, _ *string) { wire.PaneID = "w1:p9" }, nil, ErrManagedStale},
		{"other directory", func(_ *sessionhost.Origin, _ *wireAgent, cwd *string) { *cwd = "/tmp/other" }, nil, ErrManagedStale},
	} {
		t.Run(test.name, func(t *testing.T) {
			wire, origin, cwd := valid, target, "/tmp/project"
			if test.mutate != nil {
				test.mutate(&origin, &wire, &cwd)
			}
			result := envelope("cli:agent:get", "agent_info", wire)
			if test.result != nil {
				result = test.result(wire)
			}
			launcher := NewManagedCodexLauncher(Host{Runner: &scriptedRunner{results: []command.Result{result}}, Session: "named"})
			gotWire, agent, err := launcher.readManagedAgent(context.Background(), origin, cwd)
			if !errors.Is(err, test.want) || (test.want != nil && (!reflect.DeepEqual(gotWire, wireAgent{}) || agent.PaneID != "")) {
				t.Fatalf("wire=%+v agent=%+v error=%v want=%v", gotWire, agent, err, test.want)
			}
		})
	}
}

func TestManagedCodexIdentityReadPaneExtentRejectsEnvelopeAndEmptyViewport(t *testing.T) {
	target := sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}
	pane := func(id, kind string, offset, rows int) command.Result {
		return jsonResult(t, map[string]any{"id": id, "result": map[string]any{"type": kind, "pane": map[string]any{
			"pane_id": target.PaneID, "workspace_id": target.WorkspaceID, "tab_id": target.TabID, "focused": false,
			"scroll": map[string]any{"max_offset_from_bottom": offset, "viewport_rows": rows},
		}}})
	}
	for _, test := range []struct {
		name   string
		result command.Result
		want   int
		fail   bool
	}{
		{"scrolled pane", pane("cli:pane:get", "pane_info", 10, 40), 50, false},
		{"one row viewport", pane("cli:pane:get", "pane_info", 0, 1), 1, false},
		{"wrong envelope id", pane("cli:agent:get", "pane_info", 10, 40), 0, true},
		{"wrong result type", pane("cli:pane:get", "agent_info", 10, 40), 0, true},
		{"empty viewport", pane("cli:pane:get", "pane_info", 10, 0), 0, true},
		{"negative scroll offset", pane("cli:pane:get", "pane_info", -1, 40), 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			launcher := NewManagedCodexLauncher(Host{Runner: &scriptedRunner{results: []command.Result{test.result}}})
			got, err := launcher.readPaneExtent(context.Background(), target)
			if test.fail {
				if !errors.Is(err, ErrManagedStale) {
					t.Fatalf("extent=%d error=%v", got, err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("extent=%d error=%v want=%d", got, err, test.want)
			}
		})
	}
}

func TestManagedCodexIdentityReadPaneRejectsOversizedOrInvalidText(t *testing.T) {
	for _, test := range []struct {
		name   string
		output []byte
	}{
		{"one byte over the limit", []byte(strings.Repeat("x", managedPaneBytes+1))},
		{"invalid UTF-8 within the limit", []byte{'o', 'k', 0xff, 0xfe}},
	} {
		t.Run(test.name, func(t *testing.T) {
			launcher := NewManagedCodexLauncher(Host{Runner: &scriptedRunner{results: []command.Result{{Stdout: test.output}}}})
			if got, err := launcher.readPaneANSI(context.Background(), "w1:p2", "visible", 1); !errors.Is(err, ErrManagedStale) || got != "" {
				t.Fatalf("length=%d error=%v", len(got), err)
			}
		})
	}
}

func TestManagedCodexIdentityStatusFooterMustBeIndentedAndUnstyled(t *testing.T) {
	const composer = "header\n\x1b[1m›\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m\n"
	for _, test := range []struct {
		name string
		line string
		want bool
	}{
		{"indented plain footer", "  gpt-5.6-sol medium · ~/projects/herdr-plugin-kit", true},
		{"styled footer", "  \x1b[1mgpt-5.6-sol medium\x1b[0m · ~/projects/herdr-plugin-kit", false},
		{"unindented footer", "gpt-5.6-sol medium · ~/projects/herdr-plugin-kit", false},
		{"styled and unindented footer", "\x1b[1mgpt-5.6-sol medium\x1b[0m · ~/projects/herdr-plugin-kit", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := unstyledCodexStatusFooter(test.line); got != test.want {
				t.Fatalf("footer=%t want=%t", got, test.want)
			}
			if got := emptyCodexComposer(composer + test.line); got != test.want {
				t.Fatalf("composer=%t want=%t", got, test.want)
			}
		})
	}
}

func TestManagedCodexIdentityDisplayedSessionPrefixMustBeCompatibleAndWellFormed(t *testing.T) {
	const expected = "01a0c4e0-c206-7061-a45c-d0abc"
	for _, test := range []struct {
		displayed string
		want      bool
	}{
		{"01a0c4e0-c206-7061-a45c-d0", true},
		{expected, true},
		{expected + "d", true},
		{"01a0c4e0-c206-7061-a45c-ff", false},
		{"01a0c4e0-c206-7061-a45c-", false},
		{"01a0c4e0-c206-7061-a45c-d", false},
		{"", false},
	} {
		if got := validManagedDisplayedSessionPrefix(test.displayed, expected); got != test.want {
			t.Fatalf("displayed=%q valid=%t want=%t", test.displayed, got, test.want)
		}
	}
}

func TestManagedCodexIdentityStatusSessionFieldMustBeWellFormedWhenTitlePrefixIsUsed(t *testing.T) {
	const prefix = "01a0c4e0-c206-7061-a45c-d0abc"
	status := func(session string) string {
		return "/status\n╭──────────────────────────────────────────╮\n│ >_ OpenAI Codex (v0.154.0)               │\n" +
			"│ Model: gpt-5.6-sol (reasoning medium)     │\n│ Directory: /tmp/project                  │\n" +
			"│ Session: " + session + " │\n╰──────────────────────────────────────────╯"
	}
	for _, test := range []struct {
		name    string
		session string
		want    error
	}{
		// Styled text defeats the strict Session row match, so only the label-based field carries the value.
		{"styled prefix of the title prefix", "\x1b[2m01a0c4e0-c206-7061-a45c-d0\x1b[0m", nil},
		{"styled title prefix itself", "\x1b[2m" + prefix + "\x1b[0m", nil},
		{"styled longer value", "\x1b[2m" + prefix + "d\x1b[0m", nil},
		{"styled different value", "\x1b[2m01a0c4e0-c206-7061-a45c-ff\x1b[0m", ErrManagedStale},
		{"cut right after the dash", "01a0c4e0-c206-7061-a45c-", ErrManagedStale},
	} {
		t.Run(test.name, func(t *testing.T) {
			observed, err := parseManagedStatusObservation(status(test.session), "/tmp/project", CodexServiceSnapshot{}, ManagedCodexPolicy{}, true, prefix)
			if !errors.Is(err, test.want) || (test.want == nil && observed.identity != prefix) {
				t.Fatalf("observation=%+v error=%v want=%v", observed, err, test.want)
			}
		})
	}
}

func TestManagedCodexIdentityStatusDirectoryTruncationNeedsEllipsisAndRootedSuffix(t *testing.T) {
	const expected = "/home/raul/projects/herdr-attention-sessions"
	for _, test := range []struct {
		displayed string
		want      bool
	}{
		{expected, true},
		{"…/herdr-attention-sessions", true},
		{"…/projects/herdr-attention-sessions", true},
		{"/herdr-attention-sessions", false},
		{"…herdr-attention-sessions", false},
		{"…/other-sessions", false},
		{"", false},
	} {
		if got := managedStatusDirectoryMatches(test.displayed, expected); got != test.want {
			t.Fatalf("displayed=%q match=%t want=%t", test.displayed, got, test.want)
		}
	}
}

func TestManagedCodexIdentityTruncatedModelUsesOnlyPortableModelCharacters(t *testing.T) {
	const suffix = " (reasoning med"
	valid := "az-AZ_09.x:y/Mm"
	got, isPrefix, ok := managedTruncatedPolicy(valid+suffix, ManagedCodexPolicy{})
	if !ok || !isPrefix || got != (ManagedCodexPolicy{Model: valid, ReasoningEffort: "med"}) {
		t.Fatalf("policy=%+v prefix=%t ok=%t", got, isPrefix, ok)
	}
	for _, model := range []string{
		"#", "a#", "#a", "gpt 5", "gpt-5.6-sol\n", "gpt-5.6-é", "gpt`5", "gpt{5", "gpt@5", "gpt[5", "gpt\x00", "",
	} {
		if _, _, ok := managedTruncatedPolicy(model+suffix, ManagedCodexPolicy{}); ok {
			t.Fatalf("model %q accepted", model)
		}
	}
	if _, _, ok := managedTruncatedPolicy(valid, ManagedCodexPolicy{}); ok {
		t.Fatal("model without the truncated reasoning marker accepted")
	}
}

func paneScrollResult(t *testing.T, target sessionhost.Origin, maximum int) command.Result {
	t.Helper()
	return jsonResult(t, map[string]any{"id": "cli:pane:get", "result": map[string]any{"type": "pane_info", "pane": map[string]any{"pane_id": target.PaneID, "workspace_id": target.WorkspaceID, "tab_id": target.TabID, "focused": false, "scroll": map[string]any{"max_offset_from_bottom": maximum, "viewport_rows": 40}}}})
}

func observedManagedStatus() string {
	return `/status
│ >_ OpenAI Codex (v0.154.0) │
│ Remote: unix:///tmp/codex-home/app-server-control/app-server-control.sock (v0.154.0) │
│ Model: gpt-5.6-sol (reasoning medium, summaries detailed) │
│ Directory: /tmp/project │
│ Session: 01a09f5c-37cc-77f1-9409-e9aa070ad8d0 │`
}
