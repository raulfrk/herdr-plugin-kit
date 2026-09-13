package agenthost

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

type cancelOnCallRunner struct {
	inner  *scriptedRunner
	cancel context.CancelFunc
	at     int
	calls  int
}

func (r *cancelOnCallRunner) Run(ctx context.Context, exe string, args []string) (command.Result, error) {
	result, err := r.inner.Run(ctx, exe, args)
	r.calls++
	if r.calls == r.at {
		r.cancel()
	}
	return result, err
}

func TestNewProbeWithNativeValidatesAndClonesSources(t *testing.T) {
	sources := NativeSources{
		OMPSockets: map[string]string{"w:p1": "/tmp/omp.sock"},
		OpenCodeServers: map[string]OpenCodeServer{
			"w:p2": {URL: "http://127.0.0.1:4096", Directory: "/tmp/project"},
		},
	}
	probe, err := NewProbeWithNative(Host{}, sources)
	if err != nil {
		t.Fatal(err)
	}
	sources.OMPSockets["w:p1"] = "/tmp/replaced.sock"
	sources.OpenCodeServers["w:p2"] = OpenCodeServer{URL: "http://127.0.0.1:8192", Directory: "/other"}
	if probe.native.OMPSockets["w:p1"] != "/tmp/omp.sock" || probe.native.OpenCodeServers["w:p2"].Directory != "/tmp/project" {
		t.Fatalf("caller mutation changed probe sources: %+v", probe.native)
	}

	invalid := []NativeSources{
		{OMPSockets: map[string]string{"": "/tmp/socket"}},
		{OMPSockets: map[string]string{"w:p": "relative"}},
		{OMPSockets: map[string]string{"w:p": "/tmp/a\x00b"}},
		{OMPSockets: map[string]string{"w:p": "/tmp/a"}, OpenCodeServers: map[string]OpenCodeServer{"w:p": {URL: "http://127.0.0.1:1", Directory: "/tmp"}}},
		{OpenCodeServers: map[string]OpenCodeServer{"w:p": {URL: "https://127.0.0.1:1", Directory: "/tmp"}}},
		{OpenCodeServers: map[string]OpenCodeServer{"w:p": {URL: "http://localhost:1", Directory: "/tmp"}}},
		{OpenCodeServers: map[string]OpenCodeServer{"w:p": {URL: "http://127.0.0.1", Directory: "/tmp"}}},
		{OpenCodeServers: map[string]OpenCodeServer{"w:p": {URL: "http://127.0.0.1:70000", Directory: "/tmp"}}},
		{OpenCodeServers: map[string]OpenCodeServer{"w:p": {URL: "http://127.0.0.1:1/path", Directory: "/tmp"}}},
		{OpenCodeServers: map[string]OpenCodeServer{"w:p": {URL: "http://user@127.0.0.1:1", Directory: "/tmp"}}},
		{OpenCodeServers: map[string]OpenCodeServer{"w:p": {URL: "http://127.0.0.1:1?q=x", Directory: "/tmp"}}},
		{OpenCodeServers: map[string]OpenCodeServer{"w:p": {URL: "http://127.0.0.1:1?", Directory: "/tmp"}}},
		{OpenCodeServers: map[string]OpenCodeServer{"w:p": {URL: "http://127.0.0.1:1#", Directory: "/tmp"}}},
		{OpenCodeServers: map[string]OpenCodeServer{"w:p": {URL: "http://[::1]:1", Directory: "relative"}}},
	}
	for i, sources := range invalid {
		if _, err := NewProbeWithNative(Host{}, sources); err == nil {
			t.Fatalf("invalid sources[%d] accepted: %+v", i, sources)
		}
	}
	if _, err := NewProbeWithNative(Host{}, NativeSources{OpenCodeServers: map[string]OpenCodeServer{"w:p": {URL: "http://[::1]:1234/", Directory: "/tmp"}}}); err != nil {
		t.Fatalf("IPv6 loopback rejected: %v", err)
	}
}

func TestOMPStatusExtensionReturnsFreshCopy(t *testing.T) {
	first := OMPStatusExtension()
	second := OMPStatusExtension()
	if len(first) == 0 || !reflect.DeepEqual(first, second) {
		t.Fatal("embedded OMP extension is empty or inconsistent")
	}
	first[0] ^= 0xff
	if reflect.DeepEqual(first, OMPStatusExtension()) {
		t.Fatal("caller mutation changed embedded OMP extension")
	}
}

func TestNewProbeLeavesNativeHarnessesUnmanaged(t *testing.T) {
	for _, target := range []Agent{nativeTarget("omp", "path", "/tmp/session.jsonl"), nativeTarget("opencode", "id", "session-1")} {
		runner := &scriptedRunner{}
		got, err := NewProbe(Host{Runner: runner, Session: "named"}).Assess(context.Background(), target)
		if err != nil || got != (Assessment{Status: Unknown, Reason: NativeNotManaged}) || len(runner.calls) != 0 {
			t.Fatalf("kind=%s assessment=%+v error=%v calls=%q", target.Kind, got, err, runner.calls)
		}
	}
}

func TestFindNativeIdentityProjectsOnlyIdentity(t *testing.T) {
	target := nativeTarget("omp", "path", "/tmp/session.jsonl")
	agent := map[string]any{
		"agent": target.Kind, "agent_session": map[string]any{
			"source": target.Session.Source, "agent": target.Session.Agent,
			"kind": target.Session.Kind, "value": target.Session.Value,
		},
		"workspace_id": target.WorkspaceID, "tab_id": target.TabID, "pane_id": target.PaneID,
		"agent_status": map[string]any{"future": true}, "revision": "malformed",
		"state_change_seq": []any{"ignored"}, "interactive_ready": "ignored",
	}
	payload, err := json.Marshal(map[string]any{"id": "agents", "result": map[string]any{"type": "agent_list", "agents": []any{agent}}})
	if err != nil {
		t.Fatal(err)
	}
	runner := &scriptedRunner{results: []command.Result{{Stdout: payload}}}
	got, err := (Host{Runner: runner, Herdr: "/opt/herdr", Session: "named"}).findNativeIdentity(context.Background(), target.PaneID)
	if err != nil || !sameNativeIdentity(got, nativeIdentityFromAgent(target)) {
		t.Fatalf("identity=%+v error=%v", got, err)
	}
	if want := [][]string{{"/opt/herdr", "--session", "named", "agent", "list"}}; !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls=%q want=%q", runner.calls, want)
	}
}

func TestFindNativeIdentityBoundsAndStrictlyDecodesEnvelope(t *testing.T) {
	for _, output := range [][]byte{
		[]byte(`{"id":"x","result":{"type":"agent_list","agents":[]},"future":true}`),
		[]byte(`{"id":"x","result":{"type":"agent_list","agents":[]}} trailing`),
		[]byte(strings.Repeat(" ", 1<<20+1)),
	} {
		runner := &scriptedRunner{results: []command.Result{{Stdout: output}}}
		if _, err := (Host{Runner: runner, Session: "named"}).findNativeIdentity(context.Background(), "w:p"); err == nil {
			t.Fatalf("invalid identity envelope accepted")
		}
	}
}

func nativeTarget(kind, sessionKind, sessionValue string) Agent {
	return Agent{
		SessionName: "named", Kind: kind, Status: "unknown",
		WorkspaceID: "w", TabID: "w:t", PaneID: "w:p",
		Session: &SessionRef{Source: "herdr:" + kind, Agent: kind, Kind: sessionKind, Value: sessionValue},
	}
}

func nativeIdentityResult(t *testing.T, target Agent, extras map[string]any) command.Result {
	t.Helper()
	agent := map[string]any{
		"agent": target.Kind, "workspace_id": target.WorkspaceID,
		"tab_id": target.TabID, "pane_id": target.PaneID,
		"agent_session": map[string]any{
			"source": target.Session.Source, "agent": target.Session.Agent,
			"kind": target.Session.Kind, "value": target.Session.Value,
		},
	}
	for key, value := range extras {
		agent[key] = value
	}
	payload, err := json.Marshal(map[string]any{"id": "agents", "result": map[string]any{"type": "agent_list", "agents": []any{agent}}})
	if err != nil {
		t.Fatal(err)
	}
	return command.Result{Stdout: payload}
}
