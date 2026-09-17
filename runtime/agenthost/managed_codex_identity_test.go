package agenthost

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

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
		strings.Replace(base, "1baf1239", "1ba", 1),
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
	for _, text := range []string{
		"header\n› draft text\n status",
		"header\n› first draft line\n  second draft line\n status",
		"header\n\x1b[1m›\x1b[0m Ask Codex to do anything",
		"header\n\x1b[1m›\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m\ncontinued draft",
		"header\n\x1b[1m›\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m\n  user continuation",
		"header\n\x1b[1m›\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m\n  first unstyled line\n  second unstyled line",
		"header\n\x1b[1m›\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m\n  \x1b[1munexpected styled footer\x1b[0m",
		"header without composer",
	} {
		if emptyCodexComposer(text) {
			t.Fatalf("draft accepted: %q", text)
		}
	}
}

func TestExactCodexStatusDraftAcceptsObserved01540MenuAndRejectsAmbiguity(t *testing.T) {
	observed := "header\n\x1b[1m›\x1b[0m /status\n\n  /status      show current session configuration and token usage\n  /\x1b[0m\x1b[1mstatus\x1b[0mline  \x1b[0m\x1b[2mconfigure which items appear in the status line\x1b[0m"
	compact := "header\n\x1b[1m›\x1b[0m /status\n\n  \x1b[1m\x1b[38;5;6m/status      show current session configuration and token\x1b[0m\n  \x1b[1m\x1b[38;5;6m             usage\x1b[0m"
	if !exactCodexSingleLineDraft(observed, "/status") || !exactCodexSingleLineDraft(compact, "/status") {
		t.Fatal("Codex 0.154.0 status menu rejected")
	}
	help := strings.Replace(observed, "›\x1b[0m /status\n", "›\x1b[0m /help\n", 1)
	if exactCodexSingleLineDraft(help, "/help") {
		t.Fatal("status autocomplete accepted for a different command")
	}
	for _, text := range []string{
		strings.ReplaceAll(observed, "\x1b[0m\x1b[1m", ""),
		managedANSI.ReplaceAllString(compact, ""),
		observed + "\n  extra menu row",
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
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
		launcher.topology = &fakeManagedTopology{verifyErr: processErr}
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
