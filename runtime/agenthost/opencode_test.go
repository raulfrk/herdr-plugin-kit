package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type openCodeFixture struct {
	t              *testing.T
	target         Agent
	directory      string
	questions      bool
	permissions    bool
	status         string
	statusByCall   []string
	session404At   int
	wrongDirectory bool
	sessionParent  string
	sessionBody    string
	statusBody     string
	malformedPath  string
	oversizedPath  string

	mu           sync.Mutex
	calls        []string
	sessionCalls int
	statusCalls  int
}

func TestAssessOpenCodeMapsNativePrecedenceAndStatuses(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		question, permission bool
		status               string
		wantStatus           Status
		wantReason           Reason
	}{
		{"question and permission", true, true, "busy", Blocked, OpenCodeQuestionAndPermission},
		{"question", true, false, "busy", Blocked, OpenCodeQuestion},
		{"permission", false, true, "busy", Blocked, OpenCodePermission},
		{"busy", false, false, "busy", Working, OpenCodeStatus},
		{"retry", false, false, "retry", Working, OpenCodeStatus},
		{"explicit idle", false, false, "idle", Idle, OpenCodeStatus},
		{"implicit idle", false, false, "", Idle, OpenCodeStatus},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := nativeTarget("opencode", "id", "session-1")
			fixture := &openCodeFixture{t: t, target: target, directory: "/tmp/project", questions: tc.question, permissions: tc.permission, status: tc.status}
			server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
			defer server.Close()
			runner := &scriptedRunner{results: fourNativeIdentities(t, target)}
			probe := mustNativeProbe(t, Host{Runner: runner, Herdr: "/opt/herdr", Session: "named"}, NativeSources{OpenCodeServers: map[string]OpenCodeServer{target.PaneID: {URL: server.URL, Directory: fixture.directory}}})
			got, err := probe.Assess(context.Background(), target)
			if err != nil || got != (Assessment{Status: tc.wantStatus, Reason: tc.wantReason, Stable: true}) {
				t.Fatalf("assessment=%+v error=%v", got, err)
			}
			fixture.mu.Lock()
			calls := append([]string(nil), fixture.calls...)
			fixture.mu.Unlock()
			if len(calls) != 10 {
				t.Fatalf("native requests=%q", calls)
			}
			wantHerdr := []string{"/opt/herdr", "--session", "named", "agent", "list"}
			for _, call := range runner.calls {
				if !reflect.DeepEqual(call, wantHerdr) {
					t.Fatalf("OpenCode assessor used nonidentity Herdr command: %q", runner.calls)
				}
			}
		})
	}
}

func TestAssessOpenCodeUnknownAndDrift(t *testing.T) {
	target := nativeTarget("opencode", "id", "session-1")
	for _, tc := range []struct {
		name    string
		fixture *openCodeFixture
		want    Assessment
		stale   bool
	}{
		{"unknown future", &openCodeFixture{status: "paused"}, nativeUnknown(NativeUnsupported), false},
		{"within observation disappearance", &openCodeFixture{status: "idle", session404At: 2}, nativeUnknown(Unsettled), true},
		{"cross observation disappearance", &openCodeFixture{status: "idle", session404At: 3}, nativeUnknown(Unsettled), true},
		{"directory contradiction", &openCodeFixture{status: "idle", wrongDirectory: true}, nativeUnknown(Unsettled), true},
		{"child session", &openCodeFixture{status: "idle", sessionParent: "parent-1"}, nativeUnknown(Unsettled), true},
		{"cross sample semantic drift", &openCodeFixture{statusByCall: []string{"busy", "idle"}}, nativeUnknown(Unsettled), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := tc.fixture
			fixture.t, fixture.target, fixture.directory = t, target, "/tmp/project"
			server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
			defer server.Close()
			runner := &scriptedRunner{results: fourNativeIdentities(t, target)}
			probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, NativeSources{OpenCodeServers: map[string]OpenCodeServer{target.PaneID: {URL: server.URL, Directory: fixture.directory}}})
			got, err := probe.Assess(context.Background(), target)
			if tc.stale {
				if !errors.Is(err, ErrStaleReport) || got != tc.want {
					t.Fatalf("assessment=%+v error=%v", got, err)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("assessment=%+v error=%v", got, err)
			}
		})
	}
}

func TestAssessOpenCodeInitialMissingAndUnavailableProtocol(t *testing.T) {
	target := nativeTarget("opencode", "id", "session-1")
	missing := &openCodeFixture{t: t, target: target, directory: "/tmp/project", session404At: 1}
	server := httptest.NewServer(http.HandlerFunc(missing.serveHTTP))
	runner := &scriptedRunner{results: fourNativeIdentities(t, target)}
	probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, NativeSources{OpenCodeServers: map[string]OpenCodeServer{target.PaneID: {URL: server.URL, Directory: missing.directory}}})
	got, err := probe.Assess(context.Background(), target)
	server.Close()
	if err != nil || got != nativeUnknown(NativeNotManaged) {
		t.Fatalf("initial missing assessment=%+v error=%v", got, err)
	}

	for _, tc := range []struct {
		name, malformed, oversized string
	}{
		{"malformed", "/session/status", ""},
		{"oversized", "", "/question"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &openCodeFixture{t: t, target: target, directory: "/tmp/project", status: "idle", malformedPath: tc.malformed, oversizedPath: tc.oversized}
			server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
			defer server.Close()
			runner := &scriptedRunner{results: fourNativeIdentities(t, target)}
			probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, NativeSources{OpenCodeServers: map[string]OpenCodeServer{target.PaneID: {URL: server.URL, Directory: fixture.directory}}})
			got, err := probe.Assess(context.Background(), target)
			if err == nil || got != nativeUnknown(NativeUnavailable) {
				t.Fatalf("assessment=%+v error=%v", got, err)
			}
		})
	}
	for _, tc := range []struct {
		name, sessionBody, statusBody string
	}{
		{"null session", "null", ""},
		{"empty session", `{}`, ""},
		{"null selected status", "", `{"session-1":null}`},
		{"empty selected status", "", `{"session-1":{}}`},
		{"empty selected status type", "", `{"session-1":{"type":""}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &openCodeFixture{
				t: t, target: target, directory: "/tmp/project", status: "idle",
				sessionBody: tc.sessionBody, statusBody: tc.statusBody,
			}
			server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
			defer server.Close()
			runner := &scriptedRunner{results: fourNativeIdentities(t, target)}
			probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, NativeSources{OpenCodeServers: map[string]OpenCodeServer{target.PaneID: {URL: server.URL, Directory: fixture.directory}}})
			got, err := probe.Assess(context.Background(), target)
			if err == nil || got != nativeUnknown(NativeUnavailable) {
				t.Fatalf("assessment=%+v error=%v", got, err)
			}
		})
	}

	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()
	runner = &scriptedRunner{results: fourNativeIdentities(t, target)}
	probe = mustNativeProbe(t, Host{Runner: runner, Session: "named"}, NativeSources{OpenCodeServers: map[string]OpenCodeServer{target.PaneID: {URL: closedURL, Directory: "/tmp/project"}}})
	got, err = probe.Assess(context.Background(), target)
	if err == nil || got != nativeUnknown(NativeUnavailable) || strings.Contains(err.Error(), closedURL) {
		t.Fatalf("unavailable assessment=%+v error=%v", got, err)
	}
}

func TestNativeAssessmentCancellation(t *testing.T) {
	for _, target := range []Agent{nativeTarget("omp", "path", "/tmp/session.jsonl"), nativeTarget("opencode", "id", "session-1")} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var sources NativeSources
		if target.Kind == "omp" {
			sources.OMPSockets = map[string]string{target.PaneID: "/tmp/missing.sock"}
		} else {
			sources.OpenCodeServers = map[string]OpenCodeServer{target.PaneID: {URL: "http://127.0.0.1:1", Directory: "/tmp"}}
		}
		runner := &scriptedRunner{}
		probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, sources)
		got, err := probe.Assess(ctx, target)
		if !errors.Is(err, context.Canceled) || got != nativeUnknown(NativeUnavailable) || len(runner.calls) != 0 {
			t.Fatalf("kind=%s assessment=%+v error=%v calls=%q", target.Kind, got, err, runner.calls)
		}
	}
}

func TestAssessOpenCodeCancellationAfterFinalIdentityRead(t *testing.T) {
	target := nativeTarget("opencode", "id", "session-1")
	fixture := &openCodeFixture{t: t, target: target, directory: "/tmp/project", status: "idle"}
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	runner := &cancelOnCallRunner{
		inner:  &scriptedRunner{results: fourNativeIdentities(t, target)},
		cancel: cancel,
		at:     4,
	}
	probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, NativeSources{OpenCodeServers: map[string]OpenCodeServer{target.PaneID: {URL: server.URL, Directory: fixture.directory}}})
	got, err := probe.Assess(ctx, target)
	if !errors.Is(err, context.Canceled) || got != nativeUnknown(NativeUnavailable) {
		t.Fatalf("assessment=%+v error=%v", got, err)
	}
}

func (fixture *openCodeFixture) serveHTTP(response http.ResponseWriter, request *http.Request) {
	fixture.mu.Lock()
	fixture.calls = append(fixture.calls, request.URL.RequestURI())
	fixture.mu.Unlock()
	if request.URL.Query().Get("directory") != fixture.directory {
		fixture.t.Errorf("directory query=%q", request.URL.Query().Get("directory"))
	}
	if request.URL.Path == fixture.malformedPath {
		_, _ = response.Write([]byte(`{`))
		return
	}
	if request.URL.Path == fixture.oversizedPath {
		_, _ = response.Write([]byte(strings.Repeat("x", openCodeResponseBytes+1)))
		return
	}
	if request.URL.Path == "/session/"+fixture.target.Session.Value && fixture.sessionBody != "" {
		_, _ = response.Write([]byte(fixture.sessionBody))
		return
	}
	if request.URL.Path == "/session/status" && fixture.statusBody != "" {
		_, _ = response.Write([]byte(fixture.statusBody))
		return
	}
	switch request.URL.Path {
	case "/session/" + fixture.target.Session.Value:
		fixture.mu.Lock()
		fixture.sessionCalls++
		call := fixture.sessionCalls
		fixture.mu.Unlock()
		if fixture.session404At == call {
			response.WriteHeader(http.StatusNotFound)
			return
		}
		directory := fixture.directory
		if fixture.wrongDirectory {
			directory = "/tmp/replacement"
		}
		session := map[string]any{"id": fixture.target.Session.Value, "directory": directory, "ignored": "metadata"}
		if fixture.sessionParent != "" {
			session["parentID"] = fixture.sessionParent
		}
		writeOpenCodeJSON(fixture.t, response, session)
	case "/question":
		items := []any{}
		if fixture.questions {
			items = append(items, map[string]any{"sessionID": fixture.target.Session.Value, "questions": []any{"private ignored"}})
		}
		writeOpenCodeJSON(fixture.t, response, items)
	case "/permission":
		items := []any{}
		if fixture.permissions {
			items = append(items, map[string]any{"sessionID": fixture.target.Session.Value, "permission": "private ignored"})
		}
		writeOpenCodeJSON(fixture.t, response, items)
	case "/session/status":
		fixture.mu.Lock()
		fixture.statusCalls++
		call := fixture.statusCalls
		fixture.mu.Unlock()
		status := fixture.status
		if call <= len(fixture.statusByCall) {
			status = fixture.statusByCall[call-1]
		}
		statuses := map[string]any{}
		if status != "" {
			statuses[fixture.target.Session.Value] = map[string]any{"type": status, "attempt": "ignored"}
		}
		writeOpenCodeJSON(fixture.t, response, statuses)
	default:
		fixture.t.Errorf("unexpected request path %q", request.URL.Path)
		response.WriteHeader(http.StatusNotFound)
	}
}

func writeOpenCodeJSON(t *testing.T, response http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(response).Encode(value); err != nil {
		t.Error(err)
	}
}
