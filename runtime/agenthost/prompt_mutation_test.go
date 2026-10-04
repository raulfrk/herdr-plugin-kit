package agenthost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrapi"
)

const promptOMPSessionPath = "/tmp/omp-session.jsonl"

// promptWaitRecorder replaces a readiness wait with a seam that never sleeps and
// ends the test as soon as a loop keeps waiting, so a loop that stops
// terminating fails the test instead of hanging it.
type promptWaitRecorder struct {
	t     *testing.T
	limit int
	err   error
	calls []time.Duration
}

func newPromptWaitRecorder(t *testing.T, limit int) *promptWaitRecorder {
	t.Helper()
	return &promptWaitRecorder{t: t, limit: limit}
}

func (r *promptWaitRecorder) wait(_ context.Context, duration time.Duration) error {
	r.calls = append(r.calls, duration)
	if len(r.calls) > r.limit {
		r.t.Fatalf("readiness loop kept waiting after %d waits: %v", r.limit, r.calls)
	}
	return r.err
}

// promptDriftRunner is a promptRunner whose Nth agent list or foreground
// process answer can differ from the others.
type promptDriftRunner struct {
	*promptRunner
	listReads    int
	processReads int
	agentAt      func(read int, agent *wireAgent)
	pidAt        func(read int) int
}

func (r *promptDriftRunner) Run(ctx context.Context, executable string, args []string) (command.Result, error) {
	plain := args
	if len(plain) >= 2 && plain[0] == "--session" {
		plain = plain[2:]
	}
	switch {
	case r.agentAt != nil && reflect.DeepEqual(plain, []string{"agent", "list"}):
		r.listReads++
		agent := r.agent
		r.agentAt(r.listReads, &agent)
		return jsonCommandResult(listResponse(agent))
	case r.pidAt != nil && reflect.DeepEqual(plain, []string{"pane", "process-info", "--pane", r.agent.PaneID}):
		r.processReads++
		pid := r.pidAt(r.processReads)
		return jsonCommandResult(map[string]any{
			"id": "cli:pane:process_info",
			"result": map[string]any{"type": "pane_process_info", "process_info": map[string]any{
				"pane_id": r.agent.PaneID, "foreground_process_group_id": pid,
				"foreground_processes": []any{map[string]any{
					"pid": pid, "name": filepath.Base(r.argv[0]), "cwd": r.agent.CWD, "argv": r.argv,
				}},
			}},
		})
	}
	return r.promptRunner.Run(ctx, executable, args)
}

func promptOMPStatusJSON(sessionValue string, sequence int) string {
	return fmt.Sprintf(`{"version":1,"harness":"omp","session_kind":"path","session_value":%q,"status":"idle","state_seq":%d}`, sessionValue, sequence)
}

// promptServeOMPStatus answers each connection on a fresh Unix socket with the
// next payload, repeating the last one, and returns the socket path.
func promptServeOMPStatus(t *testing.T, payloads ...string) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "pm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for served := 0; ; served++ {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_, _ = io.WriteString(connection, payloads[min(served, len(payloads)-1)])
			_ = connection.Close()
		}
	}()
	return socket
}

func promptOMPTarget(t *testing.T, socket string) (PromptTarget, wireAgent) {
	t.Helper()
	target, wire := validPromptTarget(t)
	wire.Agent = "omp"
	wire.AgentSession = &wireSession{Source: "herdr:omp", Agent: "omp", Kind: "path", Value: promptOMPSessionPath}
	target.Agent = mustPublic(t, wire)
	target.Argv = []string{"omp"}
	target.CodexService = CodexServiceSnapshot{}
	target.OMPStatusSocket = socket
	return target, wire
}

func promptOpenCodeTarget(t *testing.T, sessionID, serverURL string) (PromptTarget, wireAgent) {
	t.Helper()
	target, wire := validPromptTarget(t)
	wire.Agent = "opencode"
	wire.AgentSession = &wireSession{Source: "herdr:opencode", Agent: "opencode", Kind: "id", Value: sessionID}
	target.Agent = mustPublic(t, wire)
	target.Argv = []string{"opencode", "--session", sessionID}
	target.CodexService = CodexServiceSnapshot{}
	target.OpenCodeServer = OpenCodeServer{URL: serverURL, Directory: "/tmp/project"}
	return target, wire
}

// promptOpenCodeStatusServer is an OpenCode server with one idle-by-default
// session whose /session/status body the test chooses.
func promptOpenCodeStatusServer(t *testing.T, sessionID, statusBody string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("directory") != "/tmp/project" {
			http.Error(response, "wrong directory", http.StatusBadRequest)
			return
		}
		switch request.URL.Path {
		case "/session/" + sessionID:
			_, _ = fmt.Fprintf(response, `{"id":%q,"directory":"/tmp/project","parentID":null}`, sessionID)
		case "/question", "/permission":
			_, _ = response.Write([]byte("[]"))
		case "/session/status":
			_, _ = response.Write([]byte(statusBody))
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestPromptValidateTargetRejectsEachSingleDrift(t *testing.T) {
	host := NewPromptHost(Host{Runner: &promptRunner{}})
	base, _ := promptOMPTarget(t, "/tmp/omp.sock")
	if err := host.validateTarget(base); err != nil {
		t.Fatalf("baseline target rejected: %v", err)
	}
	withSource := func(change func(*Agent)) func(*PromptTarget) {
		return func(target *PromptTarget) {
			source := clonePromptAgent(target.Agent)
			source.PaneID = "w1:p1"
			source.Session = &SessionRef{Source: "herdr:omp", Agent: "omp", Kind: "path", Value: "/tmp/source-session.jsonl"}
			change(&source)
			target.Source = &source
		}
	}
	for _, test := range []struct {
		name      string
		change    func(*PromptTarget)
		wantStale bool
	}{
		{"created origin", func(target *PromptTarget) { target.Created.Origin.TabID = "w1:t9" }, true},
		{"directory", func(target *PromptTarget) { target.Directory = "/tmp/other" }, true},
		{"argv", func(target *PromptTarget) { target.Argv = nil }, true},
		{"distinct source", withSource(func(*Agent) {}), false},
		{"invalid source", withSource(func(source *Agent) { source.Kind = "" }), true},
		{"source Herdr session", withSource(func(source *Agent) { source.SessionName = "other" }), true},
		{"source workspace", withSource(func(source *Agent) { source.WorkspaceID = "w2" }), true},
		{"source is target pane", withSource(func(source *Agent) { source.PaneID = base.Agent.PaneID }), true},
		{"source is target conversation", withSource(func(source *Agent) { session := *base.Agent.Session; source.Session = &session }), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := clonePromptTarget(base)
			test.change(&candidate)
			err := host.validateTarget(candidate)
			if test.wantStale != errors.Is(err, ErrPromptStale) || !test.wantStale && err != nil {
				t.Fatalf("wantStale=%t error=%v", test.wantStale, err)
			}
		})
	}
}

func TestPromptValidateTargetChecksOpenCodeServerAndDirectoryIndependently(t *testing.T) {
	host := NewPromptHost(Host{Runner: &promptRunner{}})
	for _, test := range []struct {
		name      string
		server    OpenCodeServer
		wantStale bool
	}{
		{"matching loopback server", OpenCodeServer{URL: "http://127.0.0.1:4096", Directory: "/tmp/project"}, false},
		{"invalid server", OpenCodeServer{URL: "ftp://127.0.0.1:4096", Directory: "/tmp/project"}, true},
		{"other directory", OpenCodeServer{URL: "http://127.0.0.1:4096", Directory: "/tmp/other"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, _ := promptOpenCodeTarget(t, "ses_prompt_target", test.server.URL)
			target.OpenCodeServer = test.server
			err := host.validateTarget(target)
			if test.wantStale != errors.Is(err, ErrPromptStale) || !test.wantStale && err != nil {
				t.Fatalf("wantStale=%t error=%v", test.wantStale, err)
			}
		})
	}
}

func TestPromptObserveRejectsImmutableDriftBeforeProcessInspection(t *testing.T) {
	target, wire := validPromptTarget(t)
	current := wire
	session := *wire.AgentSession
	session.Value = managedSecondID
	current.AgentSession = &session
	runner := &promptRunner{agent: current}
	host := newTestCodexPromptHost(Host{Runner: runner}, nil)
	if _, _, _, err := host.observe(context.Background(), target); !errors.Is(err, ErrPromptStale) {
		t.Fatalf("immutable drift error=%v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("immutable drift reached process inspection: %q", runner.calls)
	}
}

func TestDeliverPromptStaleWhenLaterObservationDrifts(t *testing.T) {
	prompt, err := PreparePrompt("hello")
	if err != nil {
		t.Fatal(err)
	}
	pid := func(from int, drifted int) func(int) int {
		return func(read int) int {
			if read >= from {
				return drifted
			}
			return 202
		}
	}
	for _, test := range []struct {
		name  string
		agent func(read int, agent *wireAgent)
		pid   func(read int) int
	}{
		{"second status", func(read int, agent *wireAgent) {
			if read >= 2 {
				agent.AgentStatus = "done"
			}
		}, nil},
		{"final status", func(read int, agent *wireAgent) {
			if read == 3 {
				agent.AgentStatus = "done"
			}
		}, nil},
		{"second sequence", func(read int, agent *wireAgent) {
			if read >= 2 {
				agent.StateChangeSeq++
			}
		}, nil},
		{"final sequence", func(read int, agent *wireAgent) {
			if read == 3 {
				agent.StateChangeSeq++
			}
		}, nil},
		{"second process", nil, pid(2, 303)},
		{"final process", nil, pid(3, 303)},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, wire := validPromptTarget(t)
			runner := &promptDriftRunner{promptRunner: &promptRunner{agent: wire, argv: target.Argv}, agentAt: test.agent, pidAt: test.pid}
			caller := &recordedPromptCall{receipt: herdrapi.Receipt{State: herdrapi.RequestAcknowledged}, agent: wire}
			host := newTestCodexPromptHost(Host{Runner: runner}, caller)
			host.wait = newPromptWaitRecorder(t, 5).wait
			got, err := host.Deliver(context.Background(), prompt, target)
			if !errors.Is(err, ErrPromptStale) || got.State != PromptNotAttempted || caller.calls != 0 {
				t.Fatalf("result=%+v calls=%d error=%v", got, caller.calls, err)
			}
		})
	}
}

func TestDeliverPromptStaleWhenOMPStatusSequenceDrifts(t *testing.T) {
	prompt, err := PreparePrompt("hello")
	if err != nil {
		t.Fatal(err)
	}
	first, later := promptOMPStatusJSON(promptOMPSessionPath, 7), promptOMPStatusJSON(promptOMPSessionPath, 8)
	for _, test := range []struct {
		name     string
		payloads []string
	}{
		{"second", []string{first, later}},
		{"final", []string{first, first, later}},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, wire := promptOMPTarget(t, promptServeOMPStatus(t, test.payloads...))
			caller := &recordedPromptCall{receipt: herdrapi.Receipt{State: herdrapi.RequestAcknowledged}, agent: wire}
			host := PromptHost{Host: Host{Runner: &promptRunner{agent: wire, argv: target.Argv}}, caller: caller, wait: newPromptWaitRecorder(t, 5).wait}
			got, err := host.Deliver(context.Background(), prompt, target)
			if !errors.Is(err, ErrPromptStale) || got.State != PromptNotAttempted || caller.calls != 0 {
				t.Fatalf("result=%+v calls=%d error=%v", got, caller.calls, err)
			}
		})
	}
}

func TestDeliverPromptPollsEveryFiftyMillisecondsThenSettles(t *testing.T) {
	const sessionID = "ses_prompt_target"
	server := newPromptOpenCodeServer(t, sessionID)
	target, wire := promptOpenCodeTarget(t, sessionID, server.URL)
	runner := &promptRunner{agent: wire, argv: target.Argv, readyVisibleAt: 2}
	caller := &recordedPromptCall{receipt: herdrapi.Receipt{State: herdrapi.RequestAcknowledged}, agent: wire}
	waits := newPromptWaitRecorder(t, 5)
	host := PromptHost{Host: Host{Runner: runner}, caller: caller, wait: waits.wait}
	prompt, _ := PreparePrompt("hello")
	got, err := host.Deliver(context.Background(), prompt, target)
	if err != nil || got.State != PromptAcknowledged || caller.calls != 1 {
		t.Fatalf("result=%+v calls=%d error=%v", got, caller.calls, err)
	}
	if want := []time.Duration{50 * time.Millisecond, DefaultSettle()}; !reflect.DeepEqual(waits.calls, want) {
		t.Fatalf("waits=%v want=%v", waits.calls, want)
	}
}

func TestPromptNativeReadyOMPRequiresMatchingSessionIdentity(t *testing.T) {
	for _, test := range []struct {
		name      string
		payload   string
		noSession bool
		want      string
		wantStale bool
	}{
		{"matching session", promptOMPStatusJSON(promptOMPSessionPath, 7), false, "omp:" + promptOMPSessionPath + ":7", false},
		{"other session", promptOMPStatusJSON("/tmp/other-session.jsonl", 7), false, "", true},
		{"target without session", promptOMPStatusJSON(promptOMPSessionPath, 7), true, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, wire := promptOMPTarget(t, promptServeOMPStatus(t, test.payload))
			if test.noSession {
				target.Agent.Session = nil
			}
			runner := &promptRunner{agent: wire, argv: target.Argv}
			ready, err := (PromptHost{Host: Host{Runner: runner}}).nativeReady(context.Background(), target)
			if ready != test.want || test.wantStale != errors.Is(err, ErrPromptStale) || !test.wantStale && err != nil {
				t.Fatalf("ready=%q error=%v", ready, err)
			}
			if test.wantStale && len(runner.calls) != 0 {
				t.Fatalf("stale target read the pane: %q", runner.calls)
			}
		})
	}
}

func TestPromptNativeReadyOpenCodeChecksServerDirectorySessionAndIdleIndependently(t *testing.T) {
	const sessionID = "ses_prompt_target"
	idle := promptOpenCodeStatusServer(t, sessionID, `{}`)
	busy := promptOpenCodeStatusServer(t, sessionID, `{"`+sessionID+`":{"type":"busy"}}`)
	for _, test := range []struct {
		name      string
		server    OpenCodeServer
		noSession bool
		want      string
		wantErr   error
	}{
		{"idle session", OpenCodeServer{URL: idle.URL, Directory: "/tmp/project"}, false, "opencode:idle", nil},
		{"working session", OpenCodeServer{URL: busy.URL, Directory: "/tmp/project"}, false, "", errPromptNotReady},
		{"invalid server", OpenCodeServer{URL: "ftp://127.0.0.1:4096", Directory: "/tmp/project"}, false, "", ErrPromptStale},
		{"other directory", OpenCodeServer{URL: idle.URL, Directory: "/tmp/other"}, false, "", ErrPromptStale},
		{"target without session", OpenCodeServer{URL: idle.URL, Directory: "/tmp/project"}, true, "", ErrPromptStale},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, wire := promptOpenCodeTarget(t, sessionID, test.server.URL)
			target.OpenCodeServer = test.server
			if test.noSession {
				target.Agent.Session = nil
			}
			runner := &promptRunner{agent: wire, argv: target.Argv}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ready, err := (PromptHost{Host: Host{Runner: runner}}).nativeReady(ctx, target)
			if ready != test.want || !errors.Is(err, test.wantErr) {
				t.Fatalf("ready=%q error=%v", ready, err)
			}
			if test.wantErr != nil && len(runner.calls) != 0 {
				t.Fatalf("target that is not ready read the pane: %q", runner.calls)
			}
		})
	}
}

func TestPromptEmptyOMPComposerFindsBorderOnFirstAndLastNonBlankLine(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
		want bool
	}{
		{"border is the only line", "╰─", true},
		{"blank lines after the border", "header\n╰─\n\n  \n", true},
		{"only blank lines", "\n  \n", false},
		{"draft on the last line", "╰─\n╰─ draft", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := emptyOMPComposer(test.text); got != test.want {
				t.Fatalf("emptyOMPComposer(%q)=%t want %t", test.text, got, test.want)
			}
		})
	}
}

func TestPromptANSIBackgroundNeedsEveryParameterOfItsColorForm(t *testing.T) {
	for _, test := range []struct {
		parameters string
		want       string
		ok         bool
	}{
		{"49", "default", true},
		{"48;5;236", "48;5;236", true},
		{"1;48;5;236", "48;5;236", true},
		{"48;2;1;2;3", "48;2;1;2;3", true},
		{"0;48;2;1;2;3;1", "48;2;1;2;3", true},
		{"48", "", false},
		{"1;48", "", false},
		{"48;5", "", false},
		{"48;2;1;2", "", false},
		{"38;5;236", "", false},
	} {
		t.Run(test.parameters, func(t *testing.T) {
			got, ok := ansiBackground(test.parameters)
			if got != test.want || ok != test.ok {
				t.Fatalf("ansiBackground(%q)=%q,%t want %q,%t", test.parameters, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestPromptValidatePreparedPromptAcceptsExactlyTheScalarLimit(t *testing.T) {
	if err := validatePreparedPrompt(Prompt{text: strings.Repeat("🚀", MaxPromptCharacters)}); err != nil {
		t.Fatalf("20,000 Unicode scalars rejected: %v", err)
	}
	for name, text := range map[string]string{
		"one scalar too many": strings.Repeat("🚀", MaxPromptCharacters+1),
		"blank":               " \n\t",
		"invalid UTF-8":       string([]byte{0xff}),
	} {
		if err := validatePreparedPrompt(Prompt{text: text}); !errors.Is(err, ErrPromptInvalid) {
			t.Fatalf("%s: error=%v", name, err)
		}
	}
}
