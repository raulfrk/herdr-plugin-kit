package agenthost

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrcmd"
)

func TestNativeValidateOpenCodeServerPortAndLoopbackPolicy(t *testing.T) {
	for _, origin := range []string{
		"http://127.0.0.1:1", "http://127.0.0.1:65535", "http://127.255.255.254:8080", "http://[::1]:4096",
	} {
		if err := validateOpenCodeServer(OpenCodeServer{URL: origin, Directory: "/tmp/project"}); err != nil {
			t.Errorf("origin %q rejected: %v", origin, err)
		}
	}
	for _, origin := range []string{
		"http://127.0.0.1:0",
		"http://127.0.0.1:65536",
		"http://10.0.0.1:4096",
		"http://192.168.1.5:4096",
		"http://[2001:db8::1]:4096",
		// Loopback addresses that are not spelled as 127.x.y.z or ::1 are refused literally.
		"http://[::ffff:127.0.0.1]:4096",
		"http://[0:0:0:0:0:0:0:1]:4096",
	} {
		if err := validateOpenCodeServer(OpenCodeServer{URL: origin, Directory: "/tmp/project"}); err == nil {
			t.Errorf("origin %q accepted", origin)
		}
	}
}

func TestNativeValidateTargetManagedDecision(t *testing.T) {
	const kind, source, sessionKind = "omp", "herdr:omp", "path"
	managed := nativeTarget(kind, sessionKind, "/tmp/session.jsonl")
	withSession := func(change func(*SessionRef)) Agent {
		target := nativeTarget(kind, sessionKind, "/tmp/session.jsonl")
		change(target.Session)
		return target
	}
	for _, tc := range []struct {
		name   string
		host   Host
		target Agent
		want   bool
	}{
		{"matching session identity is managed", Host{Session: "named"}, managed, true},
		{"default host session is not managed", Host{}, managed, false},
		{"target without session name is not managed", Host{Session: "named"}, func() Agent { target := managed; target.SessionName = ""; return target }(), false},
		{"other session name is not managed", Host{Session: "named"}, func() Agent { target := managed; target.SessionName = "other"; return target }(), false},
		{"target without session is not managed", Host{Session: "named"}, func() Agent { target := managed; target.Session = nil; return target }(), false},
		{"other session source is not managed", Host{Session: "named"}, withSession(func(s *SessionRef) { s.Source = "herdr:other" }), false},
		{"other session agent is not managed", Host{Session: "named"}, withSession(func(s *SessionRef) { s.Agent = "other" }), false},
		{"other session kind is not managed", Host{Session: "named"}, withSession(func(s *SessionRef) { s.Kind = "id" }), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateNativeTarget(tc.host, tc.target, kind, source, sessionKind)
			if err != nil || got != tc.want {
				t.Fatalf("managed=%t error=%v, want managed=%t without error", got, err, tc.want)
			}
		})
	}
}

func TestNativeSameIdentityComparesEveryField(t *testing.T) {
	base := nativeIdentityFromAgent(nativeTarget("omp", "path", "/tmp/session.jsonl"))
	if !sameNativeIdentity(base, base) {
		t.Fatal("identical identities differ")
	}
	caseOnly := base
	caseOnly.Kind = "OMP"
	if !sameNativeIdentity(base, caseOnly) {
		t.Fatal("kind comparison is not case-insensitive")
	}
	for name, change := range map[string]func(*nativeIdentity){
		"kind":         func(i *nativeIdentity) { i.Kind = "opencode" },
		"workspace id": func(i *nativeIdentity) { i.WorkspaceID = "other" },
		"tab id":       func(i *nativeIdentity) { i.TabID = "other" },
		"pane id":      func(i *nativeIdentity) { i.PaneID = "other" },
		"session name": func(i *nativeIdentity) { i.SessionName = "other" },
		"session":      func(i *nativeIdentity) { i.Session.Value = "/tmp/other.jsonl" },
	} {
		changed := base
		change(&changed)
		if sameNativeIdentity(base, changed) || sameNativeIdentity(changed, base) {
			t.Errorf("identities with a different %s compare equal", name)
		}
	}
}

// nativeAgentListPayload renders a Herdr agent-list response for target. The
// change callback may edit the envelope or its result before encoding.
func nativeAgentListPayload(t *testing.T, target Agent, change func(envelope, result map[string]any)) []byte {
	t.Helper()
	agent := map[string]any{
		"agent": target.Kind, "workspace_id": target.WorkspaceID,
		"tab_id": target.TabID, "pane_id": target.PaneID,
		"agent_session": map[string]any{
			"source": target.Session.Source, "agent": target.Session.Agent,
			"kind": target.Session.Kind, "value": target.Session.Value,
		},
	}
	result := map[string]any{"type": "agent_list", "agents": []any{agent}}
	envelope := map[string]any{"id": "agents", "result": result}
	if change != nil {
		change(envelope, result)
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestNativeFindIdentityAcceptsExactJSONLimitAndRejectsMalformedEnvelopes(t *testing.T) {
	target := nativeTarget("omp", "path", "/tmp/session.jsonl")
	valid := nativeAgentListPayload(t, target, nil)
	pad := func(size int) []byte {
		return append(append([]byte(nil), valid...), bytes.Repeat([]byte(" "), size-len(valid))...)
	}
	for _, tc := range []struct {
		name   string
		stdout []byte
		valid  bool
	}{
		{"plain response", valid, true},
		{"response of exactly the size limit", pad(herdrcmd.MaxJSONBytes), true},
		{"response one byte over the size limit", pad(herdrcmd.MaxJSONBytes + 1), false},
		{"result of another type", nativeAgentListPayload(t, target, func(_, result map[string]any) { result["type"] = "agent_info" }), false},
		{"unknown envelope field", nativeAgentListPayload(t, target, func(envelope, _ map[string]any) { envelope["future"] = true }), false},
		{"unknown result field", nativeAgentListPayload(t, target, func(_, result map[string]any) { result["future"] = true }), false},
		{"second JSON value", append(append([]byte(nil), valid...), []byte(" {}")...), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &scriptedRunner{results: []command.Result{{Stdout: tc.stdout}}}
			got, err := (Host{Runner: runner, Session: "named"}).findNativeIdentity(context.Background(), target.PaneID)
			if tc.valid {
				if err != nil || !sameNativeIdentity(got, nativeIdentityFromAgent(target)) {
					t.Fatalf("identity=%+v error=%v", got, err)
				}
			} else if err == nil {
				t.Fatalf("invalid identity response accepted: identity=%+v", got)
			}
		})
	}
}

// A target that cannot be validated is a failure of the status protocol, not a
// harness that is simply unmanaged.
func TestNativeAssessReportsInvalidTargetAsUnavailable(t *testing.T) {
	for _, kind := range []string{"omp", "opencode"} {
		sessionKind, sessionValue := "path", "/tmp/session.jsonl"
		if kind == "opencode" {
			sessionKind, sessionValue = "id", "session-1"
		}
		target := nativeTarget(kind, sessionKind, sessionValue)
		target.WorkspaceID = ""
		runner := &scriptedRunner{}
		probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, NativeSources{})
		got, err := probe.Assess(context.Background(), target)
		if err == nil || got != nativeUnknown(NativeUnavailable) || len(runner.calls) != 0 {
			t.Fatalf("kind=%s assessment=%+v error=%v calls=%q", kind, got, err, runner.calls)
		}
	}
}
