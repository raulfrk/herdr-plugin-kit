package agenthost

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

func TestAssessOMPMapsProtocolStatesWithoutPaneReads(t *testing.T) {
	for _, tc := range []struct {
		state  string
		status Status
		reason Reason
		stable bool
	}{
		{"working", Working, OMPStatus, true},
		{"blocked", Blocked, OMPStatus, true},
		{"idle", Idle, OMPStatus, true},
		{"paused", Unknown, NativeUnsupported, false},
	} {
		t.Run(tc.state, func(t *testing.T) {
			target := nativeTarget("omp", "path", "/tmp/session.jsonl")
			payload := `{"version":1,"harness":"omp","session_kind":"path","session_value":"/tmp/session.jsonl","status":"` + tc.state + `","state_seq":9}`
			socket := serveOMPResponses(t, payload, payload)
			runner := &scriptedRunner{results: fourNativeIdentities(t, target)}
			probe, err := NewProbeWithNative(Host{Runner: runner, Herdr: "/opt/herdr", Session: "named"}, NativeSources{OMPSockets: map[string]string{target.PaneID: socket}})
			if err != nil {
				t.Fatal(err)
			}
			probe.wait = func(context.Context, time.Duration) error { return nil }
			got, err := probe.Assess(context.Background(), target)
			if err != nil || got != (Assessment{Status: tc.status, Reason: tc.reason, Stable: tc.stable}) {
				t.Fatalf("assessment=%+v error=%v", got, err)
			}
			wantCall := []string{"/opt/herdr", "--session", "named", "agent", "list"}
			for _, call := range runner.calls {
				if !reflect.DeepEqual(call, wantCall) {
					t.Fatalf("OMP assessor used nonidentity Herdr command: %q", runner.calls)
				}
			}
		})
	}
}

func TestAssessOMPRejectsSemanticAndIdentityDrift(t *testing.T) {
	target := nativeTarget("omp", "path", "/tmp/session.jsonl")
	first := `{"version":1,"harness":"omp","session_kind":"path","session_value":"/tmp/session.jsonl","status":"idle","state_seq":3}`
	second := `{"version":1,"harness":"omp","session_kind":"path","session_value":"/tmp/session.jsonl","status":"idle","state_seq":4}`
	socket := serveOMPResponses(t, first, second)
	runner := &scriptedRunner{results: fourNativeIdentities(t, target)}
	probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, NativeSources{OMPSockets: map[string]string{target.PaneID: socket}})
	got, err := probe.Assess(context.Background(), target)
	if !errors.Is(err, ErrStaleReport) || got != nativeUnknown(Unsettled) {
		t.Fatalf("sequence drift assessment=%+v error=%v", got, err)
	}

	replacement := target
	replacement.TabID = "w:replacement"
	socket = serveOMPResponses(t, first)
	runner = &scriptedRunner{results: []command.Result{
		nativeIdentityResult(t, target, nil), nativeIdentityResult(t, replacement, nil),
	}}
	probe = mustNativeProbe(t, Host{Runner: runner, Session: "named"}, NativeSources{OMPSockets: map[string]string{target.PaneID: socket}})
	got, err = probe.Assess(context.Background(), target)
	if !errors.Is(err, ErrStaleReport) || got != nativeUnknown(Unsettled) {
		t.Fatalf("identity drift assessment=%+v error=%v", got, err)
	}
}

func TestAssessOMPDistinguishesUnavailableMalformedAndSessionDrift(t *testing.T) {
	target := nativeTarget("omp", "path", "/tmp/session.jsonl")
	cases := []struct {
		name      string
		socket    func(*testing.T) string
		wantStale bool
	}{
		{"unavailable", func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing.sock") }, false},
		{"malformed", func(t *testing.T) string { return serveOMPResponses(t, `{`) }, false},
		{"oversized", func(t *testing.T) string { return serveOMPResponses(t, strings.Repeat(" ", ompResponseBytes+1)) }, false},
		{"wrong session", func(t *testing.T) string {
			return serveOMPResponses(t, `{"version":1,"harness":"omp","session_kind":"path","session_value":"/tmp/other.jsonl","status":"idle","state_seq":1}`)
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &scriptedRunner{results: fourNativeIdentities(t, target)}
			probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, NativeSources{OMPSockets: map[string]string{target.PaneID: tc.socket(t)}})
			got, err := probe.Assess(context.Background(), target)
			if tc.wantStale {
				if !errors.Is(err, ErrStaleReport) || got != nativeUnknown(Unsettled) {
					t.Fatalf("assessment=%+v error=%v", got, err)
				}
			} else if err == nil || got != nativeUnknown(NativeUnavailable) {
				t.Fatalf("assessment=%+v error=%v", got, err)
			}
		})
	}
}

func TestAssessOMPCancellationInterruptsSocketRead(t *testing.T) {
	target := nativeTarget("omp", "path", "/tmp/session.jsonl")
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "status.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- connection
		}
	}()

	runner := &scriptedRunner{results: fourNativeIdentities(t, target)}
	probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, NativeSources{OMPSockets: map[string]string{target.PaneID: listener.Addr().String()}})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, assessErr := probe.Assess(ctx, target)
		result <- assessErr
	}()
	connection := <-accepted
	defer connection.Close()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v, want context cancellation", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("OMP socket read ignored context cancellation")
	}
}

func TestAssessOMPCancellationAfterFinalIdentityRead(t *testing.T) {
	target := nativeTarget("omp", "path", "/tmp/session.jsonl")
	payload := `{"version":1,"harness":"omp","session_kind":"path","session_value":"/tmp/session.jsonl","status":"idle","state_seq":9}`
	socket := serveOMPResponses(t, payload, payload)
	ctx, cancel := context.WithCancel(context.Background())
	runner := &cancelOnCallRunner{
		inner:  &scriptedRunner{results: fourNativeIdentities(t, target)},
		cancel: cancel,
		at:     4,
	}
	probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, NativeSources{OMPSockets: map[string]string{target.PaneID: socket}})
	got, err := probe.Assess(ctx, target)
	if !errors.Is(err, context.Canceled) || got != nativeUnknown(NativeUnavailable) {
		t.Fatalf("assessment=%+v error=%v", got, err)
	}
}

func serveOMPResponses(t *testing.T, responses ...string) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "status.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for _, response := range responses {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = connection.Write([]byte(response))
			_ = connection.Close()
		}
	}()
	return socket
}

func fourNativeIdentities(t *testing.T, target Agent) []command.Result {
	t.Helper()
	return []command.Result{
		nativeIdentityResult(t, target, map[string]any{"agent_status": "future"}),
		nativeIdentityResult(t, target, map[string]any{"agent_status": []any{"malformed"}}),
		nativeIdentityResult(t, target, map[string]any{"revision": "ignored"}),
		nativeIdentityResult(t, target, map[string]any{"interactive_ready": map[string]any{"ignored": true}}),
	}
}

func mustNativeProbe(t *testing.T, host Host, sources NativeSources) Probe {
	t.Helper()
	probe, err := NewProbeWithNative(host, sources)
	if err != nil {
		t.Fatal(err)
	}
	probe.wait = func(context.Context, time.Duration) error { return nil }
	return probe
}
