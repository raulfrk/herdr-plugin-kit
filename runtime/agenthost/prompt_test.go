package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrapi"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
	"pgregory.net/rapid"
)

type promptRunner struct {
	mu             sync.Mutex
	agent          wireAgent
	agentReads     int
	mutateAt       int
	argv           []string
	socket         string
	calls          [][]string
	visibleReads   int
	readyVisibleAt int
}

func (r *promptRunner) Run(_ context.Context, executable string, args []string) (command.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{executable}, args...))
	plain := args
	if len(plain) >= 2 && plain[0] == "--session" {
		plain = plain[2:]
	}
	switch {
	case reflect.DeepEqual(plain, []string{"agent", "list"}):
		r.agentReads++
		agent := r.agent
		if r.mutateAt > 0 && r.agentReads >= r.mutateAt {
			agent.StateChangeSeq++
		}
		return jsonCommandResult(listResponse(agent))
	case reflect.DeepEqual(plain, []string{"pane", "get", r.agent.PaneID}):
		return jsonCommandResult(map[string]any{
			"id": "cli:pane:get",
			"result": map[string]any{"type": "pane_info", "pane": map[string]any{
				"pane_id": r.agent.PaneID, "workspace_id": r.agent.WorkspaceID,
				"tab_id": r.agent.TabID, "cwd": r.agent.CWD, "focused": false,
			}},
		})
	case reflect.DeepEqual(plain, []string{"pane", "process-info", "--pane", r.agent.PaneID}):
		argv := r.argv
		if len(argv) == 0 {
			argv = []string{r.agent.Agent}
			if r.agent.Agent == "codex" {
				argv = []string{"codex", "--remote", "unix://" + managedServiceSnapshot().SocketPath}
			}
		}
		return jsonCommandResult(map[string]any{
			"id": "cli:pane:process_info",
			"result": map[string]any{"type": "pane_process_info", "process_info": map[string]any{
				"pane_id": r.agent.PaneID, "foreground_process_group_id": 202,
				"foreground_processes": []any{map[string]any{
					"pid": 202, "name": filepath.Base(argv[0]), "cwd": r.agent.CWD, "argv": argv,
				}},
			}},
		})
	case reflect.DeepEqual(plain, []string{"pane", "read", r.agent.PaneID, "--source", "detection", "--lines", "60", "--format", "text"}):
		return command.Result{Stdout: []byte("ready")}, nil
	case reflect.DeepEqual(plain, []string{"pane", "read", r.agent.PaneID, "--source", "visible", "--lines", "220", "--format", "ansi"}),
		reflect.DeepEqual(plain, []string{"pane", "read", r.agent.PaneID, "--source", "visible", "--lines", "220", "--format", "text"}):
		r.visibleReads++
		if r.readyVisibleAt > r.visibleReads {
			return command.Result{Stdout: []byte("starting")}, nil
		}
		switch r.agent.Agent {
		case "omp":
			return command.Result{Stdout: []byte("ready\n\x1b[38;2;156;163;176m╰─ \x1b[0m")}, nil
		case "opencode":
			return command.Result{Stdout: []byte(openCodeEmptyComposerANSI("sidebar"))}, nil
		default:
			return command.Result{Stdout: managedEmptyComposer("ready")}, nil
		}
	case reflect.DeepEqual(plain, []string{"session", "list", "--json"}):
		socket := r.socket
		if socket == "" {
			socket = "/tmp/herdr.sock"
		}
		return jsonCommandResult(map[string]any{"sessions": []any{map[string]any{
			"name": "default", "default": true, "running": true,
			"session_dir": "/tmp/herdr", "socket_path": socket,
		}}})
	default:
		return command.Result{}, errors.New("unexpected command")
	}
}

func jsonCommandResult(value any) (command.Result, error) {
	data, err := json.Marshal(value)
	return command.Result{Stdout: data}, err
}

type recordedPromptCall struct {
	receipt herdrapi.Receipt
	err     error
	agent   wireAgent
	kind    string
	calls   int
	socket  string
	method  string
	target  string
	text    string
}

func (c *recordedPromptCall) CallWithReceipt(_ context.Context, socket, _ string, method string, params, result any) (herdrapi.Receipt, error) {
	c.calls++
	c.socket, c.method = socket, method
	encoded, err := json.Marshal(params)
	if err != nil {
		return c.receipt, err
	}
	var request struct {
		Target string `json:"target"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(encoded, &request); err != nil {
		return c.receipt, err
	}
	c.target, c.text = request.Target, request.Text
	if c.err != nil {
		return c.receipt, c.err
	}
	kind := c.kind
	if kind == "" {
		kind = "agent_prompted"
	}
	encoded, err = json.Marshal(map[string]any{"type": kind, "agent": c.agent})
	if err == nil {
		err = json.Unmarshal(encoded, result)
	}
	return c.receipt, err
}

func TestPreparePromptPreservesExactUTF8AndEnforcesScalarLimit(t *testing.T) {
	want := "  first line\nrocket 🚀  "
	prompt, err := PreparePrompt(want)
	if err != nil || prompt.text != want {
		t.Fatalf("prompt=%q error=%v", prompt.text, err)
	}
	if _, err := PreparePrompt(strings.Repeat("🚀", MaxPromptCharacters)); err != nil {
		t.Fatalf("20,000 Unicode scalars rejected: %v", err)
	}
	for name, value := range map[string]string{
		"empty": "", "whitespace": " \n\t", "too many scalars": strings.Repeat("🚀", MaxPromptCharacters+1),
		"invalid UTF-8": string([]byte{0xff}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PreparePrompt(value); !errors.Is(err, ErrPromptInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestPreparePromptUnicodeScalarBoundaryProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		count := rapid.IntRange(0, MaxPromptCharacters+5).Draw(rt, "count")
		scalar := rapid.SampledFrom([]string{"a", "🚀", "雪"}).Draw(rt, "scalar")
		value := strings.Repeat(scalar, count)
		prompt, err := PreparePrompt(value)
		wantValid := count > 0 && count <= MaxPromptCharacters
		if (err == nil) != wantValid {
			rt.Fatalf("count=%d scalar=%q prompt=%q error=%v", count, scalar, prompt.text, err)
		}
		if err == nil && prompt.text != value {
			rt.Fatal("valid prompt bytes changed")
		}
	})
}

func TestDeliverPromptAcknowledgedExactlyOnce(t *testing.T) {
	target, wire := validPromptTarget(t)
	runner := &promptRunner{agent: wire}
	ack := wire
	ack.AgentStatus = "working"
	caller := &recordedPromptCall{receipt: herdrapi.Receipt{State: herdrapi.RequestAcknowledged}, agent: ack}
	host := newTestCodexPromptHost(Host{Runner: runner}, caller)
	prompt, err := PreparePrompt("  line one\nline two 🚀  ")
	if err != nil {
		t.Fatal(err)
	}
	got, err := host.Deliver(context.Background(), prompt, target)
	if err != nil || got.State != PromptAcknowledged || got.Agent.Status != "working" {
		t.Fatalf("result=%+v error=%v", got, err)
	}
	if caller.calls != 1 || caller.socket != "/tmp/herdr.sock" || caller.method != "agent.prompt" || caller.target != wire.PaneID || caller.text != prompt.text {
		t.Fatalf("call=%+v", caller)
	}
	if runner.agentReads != 3 {
		t.Fatalf("agent observations=%d calls=%v", runner.agentReads, runner.calls)
	}
}

func TestDeliverPromptAcceptsGuardedOrdinaryCodexPrefixExactlyOnce(t *testing.T) {
	target, wire := validPromptTarget(t)
	wire.AgentSession = nil
	target.Agent = mustPublic(t, wire)
	target.Argv = []string{"codex", "--yolo"}
	target.CodexService = CodexServiceSnapshot{}
	target.codexSessionPrefix = "01a09f5c-37cc-77f1-9409-e9aa070a"
	caller := &recordedPromptCall{receipt: herdrapi.Receipt{State: herdrapi.RequestAcknowledged}, agent: wire}
	host := NewPromptHost(Host{Runner: &promptRunner{agent: wire, argv: target.Argv}})
	host.caller = caller
	prompt, _ := PreparePrompt("hello")
	result, err := host.Deliver(context.Background(), prompt, target)
	if err != nil || result.State != PromptAcknowledged || result.Agent.Session != nil || caller.calls != 1 {
		t.Fatalf("result=%+v calls=%d error=%v", result, caller.calls, err)
	}
	for _, prefix := range []string{"", "01a09f5c-37cc-77f1-9409-e9a", managedNewID} {
		bad := target
		bad.codexSessionPrefix = prefix
		if validPromptCodexTarget(bad) {
			t.Fatalf("accepted invalid unbound prefix %q", prefix)
		}
	}
}

func TestPromptTargetTopLevelGuardsRejectEachDrift(t *testing.T) {
	target, _ := validPromptTarget(t)
	validHost := NewPromptHost(Host{Runner: &promptRunner{}})
	for name, change := range map[string]func(*PromptHost, *PromptTarget){
		"runner":        func(host *PromptHost, _ *PromptTarget) { host.Host.Runner = nil },
		"invalid agent": func(_ *PromptHost, target *PromptTarget) { target.Agent.PaneID = "" },
		"session route": func(_ *PromptHost, target *PromptTarget) { target.Agent.SessionName = "named" },
		"focused":       func(_ *PromptHost, target *PromptTarget) { target.Agent.Focused = true },
		"status":        func(_ *PromptHost, target *PromptTarget) { target.Agent.Status = "working" },
	} {
		t.Run(name, func(t *testing.T) {
			host, changed := validHost, target
			change(&host, &changed)
			if err := host.validateTarget(changed); !errors.Is(err, ErrPromptStale) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestSamePromptAcknowledgementRequiresExactPaneAndCompatibleSession(t *testing.T) {
	target, _ := validPromptTarget(t)
	before := target.Agent
	prefix := managedNewID[:len(managedNewID)-4]
	if !samePromptAcknowledgement(before, before, "") {
		t.Fatal("exact identified acknowledgement rejected")
	}
	unbound := before
	unbound.Session = nil
	if !samePromptAcknowledgement(unbound, unbound, prefix) || !samePromptAcknowledgement(unbound, before, prefix) {
		t.Fatal("valid prefix acknowledgement rejected")
	}
	for name, after := range map[string]Agent{
		"immutable drift": func() Agent { value := unbound; value.PaneID = "w1:p3"; return value }(),
		"invalid session": func() Agent {
			value := before
			session := *before.Session
			session.Source = "other"
			value.Session = &session
			return value
		}(),
		"wrong prefix": func() Agent {
			value := before
			session := *before.Session
			session.Value = "02b19f5c-37cc-77f1-9409-e9aa070ad8d0"
			value.Session = &session
			return value
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if samePromptAcknowledgement(unbound, after, prefix) {
				t.Fatalf("accepted %+v", after)
			}
		})
	}
	drifted := before
	drifted.PaneID = "w1:p3"
	if samePromptAcknowledgement(before, drifted, "") {
		t.Fatal("identified acknowledgement drift accepted")
	}
}

func TestDeliverPromptUsesOneDirectSocketRequestWithExactBytes(t *testing.T) {
	target, wire := validPromptTarget(t)
	socket := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			received <- acceptErr
			return
		}
		defer connection.Close()
		var request struct {
			ID     string `json:"id"`
			Method string `json:"method"`
			Params struct {
				Target string `json:"target"`
				Text   string `json:"text"`
			} `json:"params"`
		}
		if decodeErr := json.NewDecoder(connection).Decode(&request); decodeErr != nil {
			received <- decodeErr
			return
		}
		if request.ID != "plugin-kit:prompt" || request.Method != "agent.prompt" || request.Params.Target != wire.PaneID || request.Params.Text != "  exact\n雪 🚀  " {
			received <- fmt.Errorf("request=%+v", request)
			return
		}
		response, encodeErr := json.Marshal(map[string]any{"id": request.ID, "result": map[string]any{"type": "agent_prompted", "agent": wire}})
		if encodeErr == nil {
			_, encodeErr = connection.Write(append(response, '\n'))
		}
		received <- encodeErr
	}()
	prompt, _ := PreparePrompt("  exact\n雪 🚀  ")
	result, err := newTestCodexPromptHost(Host{Runner: &promptRunner{agent: wire, socket: socket}}, nil).Deliver(context.Background(), prompt, target)
	if err != nil || result.State != PromptAcknowledged {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if err := <-received; err != nil {
		t.Fatal(err)
	}
}

func TestDeliverPromptRealTransportDistinguishesPrewriteAndNoAcknowledgement(t *testing.T) {
	target, wire := validPromptTarget(t)
	prompt, _ := PreparePrompt("hello")
	missing := filepath.Join(t.TempDir(), "missing.sock")
	result, err := newTestCodexPromptHost(Host{Runner: &promptRunner{agent: wire, socket: missing}}, nil).Deliver(context.Background(), prompt, target)
	if !errors.Is(err, ErrPromptNotSent) || result.State != CreatedPromptNotSent {
		t.Fatalf("prewrite result=%+v error=%v", result, err)
	}

	socket := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			received <- acceptErr
			return
		}
		var request map[string]any
		decodeErr := json.NewDecoder(connection).Decode(&request)
		_ = connection.Close()
		received <- decodeErr
	}()
	result, err = newTestCodexPromptHost(Host{Runner: &promptRunner{agent: wire, socket: socket}}, nil).Deliver(context.Background(), prompt, target)
	if !errors.Is(err, ErrPromptOutcomeUnknown) || result.State != CreatedPromptOutcomeUnknown {
		t.Fatalf("no-ack result=%+v error=%v", result, err)
	}
	if err := <-received; err != nil {
		t.Fatal(err)
	}
}

func TestDeliverPromptClassifiesTransportWithoutRetryOrCleanup(t *testing.T) {
	target, wire := validPromptTarget(t)
	prompt, err := PreparePrompt("hello")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		receipt herdrapi.Receipt
		want    PromptState
		wantErr error
	}{
		{"pre-write", herdrapi.Receipt{State: herdrapi.RequestNotWritten}, CreatedPromptNotSent, ErrPromptNotSent},
		{"write started", herdrapi.Receipt{State: herdrapi.RequestWriteStarted}, CreatedPromptOutcomeUnknown, ErrPromptOutcomeUnknown},
		{"request written", herdrapi.Receipt{State: herdrapi.RequestWritten}, CreatedPromptOutcomeUnknown, ErrPromptOutcomeUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			caller := &recordedPromptCall{receipt: test.receipt, err: errors.New("transport failed"), agent: wire}
			got, err := newTestCodexPromptHost(Host{Runner: &promptRunner{agent: wire}}, caller).Deliver(context.Background(), prompt, target)
			if !errors.Is(err, test.wantErr) || got.State != test.want || caller.calls != 1 {
				t.Fatalf("result=%+v calls=%d error=%v", got, caller.calls, err)
			}
		})
	}
}

func TestDeliverPromptRejectsStaleOrInvalidTargetsBeforeSend(t *testing.T) {
	target, wire := validPromptTarget(t)
	prompt, err := PreparePrompt("hello")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*PromptTarget, *promptRunner)
	}{
		{"focused", func(target *PromptTarget, _ *promptRunner) { target.Agent.Focused = true }},
		{"missing identity", func(target *PromptTarget, _ *promptRunner) { target.Agent.Session = nil }},
		{"wrong cwd", func(target *PromptTarget, _ *promptRunner) { target.Directory = "/tmp/other" }},
		{"identity changed", func(_ *PromptTarget, runner *promptRunner) { runner.mutateAt = 2 }},
		{"source is target", func(target *PromptTarget, _ *promptRunner) { source := target.Agent; target.Source = &source }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := clonePromptTarget(target)
			runner := &promptRunner{agent: wire}
			test.mutate(&candidate, runner)
			caller := &recordedPromptCall{receipt: herdrapi.Receipt{State: herdrapi.RequestAcknowledged}, agent: wire}
			got, err := newTestCodexPromptHost(Host{Runner: runner}, caller).Deliver(context.Background(), prompt, candidate)
			if !errors.Is(err, ErrPromptStale) || got.State != PromptNotAttempted || caller.calls != 0 {
				t.Fatalf("result=%+v calls=%d error=%v", got, caller.calls, err)
			}
		})
	}
}

func TestDeliverPromptRejectsInvalidAcknowledgementAsUnknown(t *testing.T) {
	prompt, err := PreparePrompt("hello")
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*recordedPromptCall){
		func(call *recordedPromptCall) { call.kind = "wrong" },
		func(call *recordedPromptCall) { call.agent.AgentSession.Value = "replacement" },
		func(call *recordedPromptCall) { call.agent.Focused = true },
	} {
		target, wire := validPromptTarget(t)
		acknowledged := wire
		acknowledgedSession := *wire.AgentSession
		acknowledged.AgentSession = &acknowledgedSession
		caller := &recordedPromptCall{receipt: herdrapi.Receipt{State: herdrapi.RequestAcknowledged}, agent: acknowledged}
		mutate(caller)
		got, err := newTestCodexPromptHost(Host{Runner: &promptRunner{agent: wire}}, caller).Deliver(context.Background(), prompt, target)
		if !errors.Is(err, ErrPromptOutcomeUnknown) || got.State != CreatedPromptOutcomeUnknown || caller.calls != 1 {
			t.Fatalf("result=%+v calls=%d error=%v", got, caller.calls, err)
		}
	}
}

func TestDeliverCodexPromptRequiresSharedServiceAndThreadBeforeSend(t *testing.T) {
	target, wire := validPromptTarget(t)
	caller := &recordedPromptCall{receipt: herdrapi.Receipt{State: herdrapi.RequestAcknowledged}, agent: wire}
	host := newTestCodexPromptHost(Host{Runner: &promptRunner{agent: wire}}, caller)
	host.managed.service = &fakeManagedService{err: ErrCodexServiceUnavailable}
	prompt, _ := PreparePrompt("hello")
	result, err := host.Deliver(context.Background(), prompt, target)
	if !errors.Is(err, ErrPromptStale) || result.State != PromptNotAttempted || caller.calls != 0 {
		t.Fatalf("result=%+v calls=%d error=%v", result, caller.calls, err)
	}
}

func TestDeliverOrdinaryCodexPromptDoesNotStartOrReadService(t *testing.T) {
	target, wire := validPromptTarget(t)
	target.Argv = []string{"codex"}
	target.CodexService = CodexServiceSnapshot{}
	caller := &recordedPromptCall{receipt: herdrapi.Receipt{State: herdrapi.RequestAcknowledged}, agent: wire}
	host := newTestCodexPromptHost(Host{Runner: &promptRunner{agent: wire, argv: target.Argv}}, caller)
	service := &fakeManagedService{err: ErrCodexServiceUnavailable}
	host.managed.service = service
	prompt, _ := PreparePrompt("hello")
	result, err := host.Deliver(context.Background(), prompt, target)
	if err != nil || result.State != PromptAcknowledged || caller.calls != 1 || service.callCount() != 0 {
		t.Fatalf("result=%+v calls=%d service=%d error=%v", result, caller.calls, service.callCount(), err)
	}
}

func TestPromptCodexTransportCannotBeDowngradedByOmittingSnapshot(t *testing.T) {
	target, _ := validPromptTarget(t)
	for _, argv := range [][]string{
		{"codex", "--remote", "unix:///tmp/other.sock"},
		{"codex", "--remote"},
		{"codex", "--remote="},
		{"codex", "--remote", "unix://" + target.CodexService.SocketPath, "--remote", "unix://" + target.CodexService.SocketPath},
	} {
		bad := target
		bad.Argv = argv
		if validPromptCodexTarget(bad) {
			t.Fatalf("accepted mismatching or ambiguous endpoint: %v", argv)
		}
	}
	target.CodexService = CodexServiceSnapshot{}
	if validPromptCodexTarget(target) {
		t.Fatal("remote argv accepted without service snapshot")
	}
	target.Argv = []string{"codex", "--remote=unix:///tmp/socket"}
	if validPromptCodexTarget(target) {
		t.Fatal("inline remote argv accepted without service snapshot")
	}
	target.Argv = []string{"codex"}
	target.CodexService = managedServiceSnapshot()
	if validPromptCodexTarget(target) {
		t.Fatal("ordinary argv incorrectly described as managed")
	}
}

func TestPromptCodexTransportModel(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Model: ordinary has no endpoint/receipt; managed has exactly one
		// endpoint equal to the receipt. All other combinations reject.
		mode := rapid.IntRange(0, 4).Draw(t, "mode")
		receipt := rapid.Bool().Draw(t, "receipt")
		validSession := rapid.Bool().Draw(t, "valid-session")
		service := managedServiceSnapshot()
		argv := []string{"codex"}
		switch mode {
		case 1:
			argv = append(argv, "--remote", "unix://"+service.SocketPath)
		case 2:
			argv = append(argv, "--remote=unix://"+service.SocketPath)
		case 3:
			argv = append(argv, "--remote", "unix:///different/socket")
		case 4:
			argv = append(argv, "--remote", "unix://"+service.SocketPath, "--remote=unix://"+service.SocketPath)
		}
		target := PromptTarget{Argv: argv, Agent: Agent{Session: &SessionRef{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: managedNewID}}}
		if receipt {
			target.CodexService = service
		}
		if !validSession {
			target.Agent.Session.Value = "truncated"
		}
		want := validSession && (mode == 0 && !receipt || (mode == 1 || mode == 2) && receipt)
		if got := validPromptCodexTarget(target); got != want {
			t.Fatalf("mode=%d receipt=%t validSession=%t got=%t want=%t", mode, receipt, validSession, got, want)
		}
	})
}

func TestDeliverOpenCodePromptRejectsServerDirectoryMismatchBeforeNativeRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	target, wire := validPromptTarget(t)
	wire.Agent = "opencode"
	wire.AgentSession = &wireSession{Source: "herdr:opencode", Agent: "opencode", Kind: "id", Value: "ses_prompt_target"}
	target.Agent = mustPublic(t, wire)
	target.Argv = []string{"opencode", "--session", "ses_prompt_target"}
	target.OpenCodeServer = OpenCodeServer{URL: server.URL, Directory: "/tmp/other"}
	caller := &recordedPromptCall{receipt: herdrapi.Receipt{State: herdrapi.RequestAcknowledged}, agent: wire}
	prompt, _ := PreparePrompt("hello")
	result, err := (PromptHost{Host: Host{Runner: &promptRunner{agent: wire, argv: target.Argv}}, caller: caller}).Deliver(context.Background(), prompt, target)
	if !errors.Is(err, ErrPromptStale) || result.State != PromptNotAttempted || requests != 0 || caller.calls != 0 {
		t.Fatalf("result=%+v requests=%d calls=%d error=%v", result, requests, caller.calls, err)
	}
}

func TestDeliverPromptRequiresStableOMPNativeIdleIdentity(t *testing.T) {
	target, wire := validPromptTarget(t)
	wire.Agent = "omp"
	wire.AgentSession = &wireSession{Source: "herdr:omp", Agent: "omp", Kind: "path", Value: "/tmp/omp-session.jsonl"}
	target.Agent = mustPublic(t, wire)
	target.Argv = []string{"omp"}
	target.OMPStatusSocket = filepath.Join(t.TempDir(), "omp.sock")

	listener, err := net.Listen("unix", target.OMPStatusSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 3; i++ {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				done <- acceptErr
				return
			}
			_, writeErr := fmt.Fprintf(connection, `{"version":1,"harness":"omp","session_kind":"path","session_value":"/tmp/omp-session.jsonl","status":"idle","state_seq":7}`)
			_ = connection.Close()
			if writeErr != nil {
				done <- writeErr
				return
			}
		}
		done <- nil
	}()
	caller := &recordedPromptCall{receipt: herdrapi.Receipt{State: herdrapi.RequestAcknowledged}, agent: wire}
	prompt, _ := PreparePrompt("hello")
	got, err := (PromptHost{Host: Host{Runner: &promptRunner{agent: wire, argv: target.Argv}}, caller: caller}).Deliver(context.Background(), prompt, target)
	if err != nil || got.State != PromptAcknowledged || caller.calls != 1 {
		t.Fatalf("result=%+v calls=%d error=%v", got, caller.calls, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDeliverPromptRequiresOpenCodeServerIdleIdentity(t *testing.T) {
	const sessionID = "ses_prompt_target"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
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
			_, _ = response.Write([]byte("{}"))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	target, wire := validPromptTarget(t)
	wire.Agent = "opencode"
	wire.AgentSession = &wireSession{Source: "herdr:opencode", Agent: "opencode", Kind: "id", Value: sessionID}
	target.Agent = mustPublic(t, wire)
	target.Argv = []string{"opencode", "--session", sessionID}
	target.OpenCodeServer = OpenCodeServer{URL: server.URL, Directory: "/tmp/project"}
	caller := &recordedPromptCall{receipt: herdrapi.Receipt{State: herdrapi.RequestAcknowledged}, agent: wire}
	prompt, _ := PreparePrompt("hello")
	got, err := (PromptHost{Host: Host{Runner: &promptRunner{agent: wire, argv: target.Argv}}, caller: caller}).Deliver(context.Background(), prompt, target)
	if err != nil || got.State != PromptAcknowledged || caller.calls != 1 {
		t.Fatalf("result=%+v calls=%d error=%v", got, caller.calls, err)
	}
}

func TestDeliverPromptWaitsForFirstNativeReadyObservationBeforeOneSend(t *testing.T) {
	const sessionID = "ses_prompt_target"
	server := newPromptOpenCodeServer(t, sessionID)

	target, wire := validPromptTarget(t)
	wire.Agent = "opencode"
	wire.AgentSession = &wireSession{Source: "herdr:opencode", Agent: "opencode", Kind: "id", Value: sessionID}
	target.Agent = mustPublic(t, wire)
	target.Argv = []string{"opencode", "--session", sessionID}
	target.OpenCodeServer = OpenCodeServer{URL: server.URL, Directory: "/tmp/project"}
	runner := &promptRunner{agent: wire, argv: target.Argv, readyVisibleAt: 2}
	caller := &recordedPromptCall{receipt: herdrapi.Receipt{State: herdrapi.RequestAcknowledged}, agent: wire}
	host := PromptHost{Host: Host{Runner: runner}, caller: caller, wait: func(context.Context, time.Duration) error { return nil }}
	prompt, _ := PreparePrompt("hello")

	got, err := host.Deliver(context.Background(), prompt, target)
	if err != nil || got.State != PromptAcknowledged || caller.calls != 1 || runner.visibleReads != 4 {
		t.Fatalf("result=%+v calls=%d visible_reads=%d error=%v", got, caller.calls, runner.visibleReads, err)
	}
}

func TestDeliverPromptPendingReadinessTimeoutSendsNothing(t *testing.T) {
	const sessionID = "ses_prompt_target"
	server := newPromptOpenCodeServer(t, sessionID)

	target, wire := validPromptTarget(t)
	wire.Agent = "opencode"
	wire.AgentSession = &wireSession{Source: "herdr:opencode", Agent: "opencode", Kind: "id", Value: sessionID}
	target.Agent = mustPublic(t, wire)
	target.Argv = []string{"opencode", "--session", sessionID}
	target.OpenCodeServer = OpenCodeServer{URL: server.URL, Directory: "/tmp/project"}
	runner := &promptRunner{agent: wire, argv: target.Argv, readyVisibleAt: 100}
	caller := &recordedPromptCall{receipt: herdrapi.Receipt{State: herdrapi.RequestAcknowledged}, agent: wire}
	host := PromptHost{Host: Host{Runner: runner}, caller: caller, wait: func(context.Context, time.Duration) error { return context.DeadlineExceeded }}
	prompt, _ := PreparePrompt("hello")

	got, err := host.Deliver(context.Background(), prompt, target)
	if !errors.Is(err, ErrPromptStale) || !errors.Is(err, context.DeadlineExceeded) || got.State != PromptNotAttempted || caller.calls != 0 || runner.visibleReads != 1 {
		t.Fatalf("result=%+v calls=%d visible_reads=%d error=%v", got, caller.calls, runner.visibleReads, err)
	}
}

func newPromptOpenCodeServer(t *testing.T, sessionID string) *httptest.Server {
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
			_, _ = response.Write([]byte("{}"))
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestNativeEmptyComposerParsersUseSupportedVersionShapes(t *testing.T) {
	if !emptyOMPComposer("header\n\x1b[38;2;156;163;176m╰─ \x1b[0m") || emptyOMPComposer("header\n╰─ draft") || emptyOMPComposer("header") {
		t.Fatal("OMP composer shape misclassified")
	}
	blank := "\x1b[38;2;255;255;255;48;2;30;30;30m        \x1b[0m"
	empty := openCodeEmptyComposerANSI("sidebar")
	freeModel := strings.Replace(empty, "Build · GPT-5.6 Sol OpenAI · medium", "Build · Big Pickle OpenCode Zen", 1)
	draft := strings.Replace(empty, blank, "\x1b[38;2;255;255;255;48;2;30;30;30m draft  \x1b[0m", 1)
	wrongStatus := strings.Replace(empty, "Build · GPT-5.6 Sol OpenAI · medium", "user draft", 1)
	alternateTheme := strings.ReplaceAll(empty, "48;2;30;30;30", "48;2;1;2;3")
	indexedTheme := strings.ReplaceAll(empty, "48;2;30;30;30", "48;5;236")
	if !emptyOpenCodeComposer(empty) || !emptyOpenCodeComposer(freeModel) || !emptyOpenCodeComposer(alternateTheme) || !emptyOpenCodeComposer(indexedTheme) || emptyOpenCodeComposer(draft) || emptyOpenCodeComposer(wrongStatus) || emptyOpenCodeComposer("header") {
		t.Fatal("OpenCode composer shape misclassified")
	}
}

func TestOpenCodeEmptyComposerAcceptsBoundedWrappedStatus(t *testing.T) {
	empty := openCodeEmptyComposerANSI("sidebar")
	status := "  Build · GPT-5.6 Sol OpenAI · medium        "
	wrapped := strings.Replace(empty, status, "  Build · GPT-5.6 Sol OpenAI · mediu\n"+openCodeComposerRow("m       ", "sidebar"), 1)
	if !emptyOpenCodeComposer(wrapped) {
		t.Fatal("supported wrapped OpenCode status was rejected")
	}
	changedBackground := strings.Replace(wrapped, "48;2;30;30;30mm", "48;2;1;2;3mm", 1)
	if emptyOpenCodeComposer(changedBackground) {
		t.Fatal("mixed status backgrounds were accepted")
	}
	excessive := strings.Replace(empty, status, "  Build · GPT-5.6 Sol OpenAI · mediu\n"+
		openCodeComposerRow("m", "sidebar")+"\n"+openCodeComposerRow("continued", "sidebar")+"\n"+openCodeComposerRow("again", "sidebar"), 1)
	if emptyOpenCodeComposer(excessive) {
		t.Fatal("excessive status continuations were accepted")
	}
	draft := strings.Replace(wrapped, openCodeComposerRow("", "sidebar"), openCodeComposerRow("draft", "sidebar"), 1)
	if emptyOpenCodeComposer(draft) {
		t.Fatal("typed draft above a wrapped status was accepted")
	}
}

func TestOpenCodeStatusStartAcceptsExactContinuationAndIndexBoundaries(t *testing.T) {
	lines := []string{
		openCodeComposerRow("  Build · Model", "sidebar"),
		openCodeComposerRow("status middle", "sidebar"),
		openCodeComposerRow("status tail", "sidebar"),
		"╹▀▀▀",
	}
	status, background, ok := openCodeStatusStart(lines, len(lines)-1)
	if !ok || status != 0 || background != "48;2;30;30;30" {
		t.Fatalf("status=%d background=%q ok=%t", status, background, ok)
	}
}

func TestOpenCodeStatusStartRejectsMissingANSIAndBlankContinuation(t *testing.T) {
	for _, lines := range [][]string{
		{"┃  Build · Model", "╹▀▀▀"},
		{openCodeComposerRow("  Build · Model", "sidebar"), openCodeComposerRow("", ""), "╹▀▀▀"},
	} {
		if status, background, ok := openCodeStatusStart(lines, len(lines)-1); ok || status != 0 || background != "" {
			t.Fatalf("lines=%q status=%d background=%q ok=%t", lines, status, background, ok)
		}
	}
}

func openCodeEmptyComposerANSI(sidebarText string) string {
	left := "\x1b[38;2;255;255;255;48;2;10;10;10m  \x1b[0m"
	border := "\x1b[38;2;92;156;245;48;2;10;10;10m┃\x1b[0m"
	blank := "\x1b[38;2;255;255;255;48;2;30;30;30m        \x1b[0m"
	sidebar := "\x1b[38;2;128;128;128;48;2;20;20;20m" + sidebarText + "\x1b[0m"
	input := left + border + blank + left + sidebar
	status := left + border + "\x1b[38;2;255;255;255;48;2;30;30;30m  Build · GPT-5.6 Sol OpenAI · medium        \x1b[0m" + left + sidebar
	return strings.Join([]string{"header", input, input, input, status, left + "╹▀▀▀", "footer"}, "\n")
}

func openCodeComposerRow(content, sidebarText string) string {
	left := "\x1b[38;2;255;255;255;48;2;10;10;10m  \x1b[0m"
	border := "\x1b[38;2;92;156;245;48;2;10;10;10m┃\x1b[0m"
	composer := "\x1b[38;2;255;255;255;48;2;30;30;30m" + content + strings.Repeat(" ", max(0, 8-len(content))) + "\x1b[0m"
	sidebar := "\x1b[38;2;128;128;128;48;2;20;20;20m" + sidebarText + "\x1b[0m"
	return left + border + composer + left + sidebar
}

func validPromptTarget(t *testing.T) (PromptTarget, wireAgent) {
	t.Helper()
	wire := validWireAgent("w1:p2")
	wire.Agent = "codex"
	wire.AgentStatus = "idle"
	wire.CWD = "/tmp/project"
	wire.Focused = false
	wire.AgentSession = &wireSession{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: "01a09f5c-37cc-77f1-9409-e9aa070ad8d0"}
	agent := mustPublic(t, wire)
	return PromptTarget{
		Created: sessionhost.CreatedTarget{Origin: sessionhost.Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}},
		Agent:   agent, Directory: "/tmp/project", Argv: []string{"codex", "--remote", "unix://" + managedServiceSnapshot().SocketPath}, CodexService: managedServiceSnapshot(),
	}, wire
}

type promptFailingVerifier struct {
	*fakeManagedVerifier
	identity codexThreadIdentity
	readErr  error
	closeErr error
}

func (v *promptFailingVerifier) ReadThread(context.Context, string) (codexThreadIdentity, error) {
	return v.identity, v.readErr
}

func (v *promptFailingVerifier) Close() error {
	v.closed++
	return v.closeErr
}

func TestPromptRemoteReadinessRejectsEachUncertainIdentityBeforePaneRead(t *testing.T) {
	for _, name := range []string{"service error", "service changed", "read error", "close error", "thread changed", "directory changed"} {
		t.Run(name, func(t *testing.T) {
			target, wire := validPromptTarget(t)
			runner := &promptRunner{agent: wire}
			service := &fakeManagedService{snapshot: target.CodexService}
			verifier := &promptFailingVerifier{
				fakeManagedVerifier: &fakeManagedVerifier{},
				identity:            codexThreadIdentity{ID: target.Agent.Session.Value, CWD: target.Directory},
			}
			failure := errors.New("unavailable")
			wantClosed := 1
			switch name {
			case "service error":
				service.err, wantClosed = failure, 0
			case "service changed":
				service.snapshot.SocketPath += "-changed"
				wantClosed = 0
			case "read error":
				verifier.readErr = failure
			case "close error":
				verifier.closeErr = failure
			case "thread changed":
				verifier.identity.ID = managedSecondID
			case "directory changed":
				verifier.identity.CWD = "/tmp/other-project"
			}
			host := newTestCodexPromptHost(Host{Runner: runner}, nil)
			host.managed.service = service
			host.managed.verifiers = fakeManagedVerifierFactory{verifier}
			ready, err := host.nativeReady(context.Background(), target)
			if ready != "" || !errors.Is(err, ErrPromptStale) {
				t.Fatalf("ready=%q error=%v", ready, err)
			}
			if verifier.closed != wantClosed || service.callCount() != 1 || len(runner.calls) != 0 {
				t.Fatalf("closed=%d service calls=%d pane calls=%q", verifier.closed, service.callCount(), runner.calls)
			}
		})
	}
}

func newTestCodexPromptHost(host Host, caller promptCaller) PromptHost {
	identity := codexThreadIdentity{ID: managedNewID, CWD: "/tmp/project", ModelProvider: "openai"}
	verifier := &fakeManagedVerifier{read: []codexThreadIdentity{identity, identity, identity}}
	managed := NewManagedCodexLauncher(host)
	managed.service = &fakeManagedService{snapshot: managedServiceSnapshot()}
	managed.verifiers = fakeManagedVerifierFactory{verifier}
	result := NewPromptHost(host)
	result.managed = managed
	if caller != nil {
		result.caller = caller
	}
	return result
}
