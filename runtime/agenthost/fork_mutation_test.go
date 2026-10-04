package agenthost

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func forkCreatedTarget(pane string) sessionhost.CreatedTarget {
	return sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: pane}}
}

func forkAcknowledgingDeliver(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
	return PromptResult{Target: target, Agent: target.Agent, State: PromptAcknowledged}, nil
}

// forkOpenCodeServer answers the native OpenCode fork API with ses_child.
func forkOpenCodeServer(t *testing.T, source Agent) OpenCodeServer {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(`{"id":"ses_child","directory":"` + source.CWD + `"}`))
	}))
	t.Cleanup(server.Close)
	return OpenCodeServer{URL: server.URL, Directory: source.CWD}
}

func TestForkPromptReportsUnusableOpenCodeServerBeforeReadingSource(t *testing.T) {
	source := forkAgent("opencode", "w1:p1", "ses_source", "/tmp/project")
	prompt, _ := PreparePrompt("fork prompt")
	for _, test := range []struct {
		name    string
		server  OpenCodeServer
		wantErr error
	}{
		{"remote host", OpenCodeServer{URL: "http://example.com:4096", Directory: source.CWD}, nil},
		{"other directory", OpenCodeServer{URL: "http://127.0.0.1:4096", Directory: "/tmp/other"}, ErrPromptInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			topology := &fakeTopology{}
			runner := &scriptedRunner{}
			forker := NewForker(Host{Runner: runner, Session: "named"})
			forker.launcher = topology
			_, err := forker.Fork(context.Background(), ForkRequest{
				Source: source, Profile: ForkProfile{Executable: "opencode"}, Placement: sessionhost.NewTab,
				OpenCodeServer: test.server, Prompt: &prompt,
			})
			if err == nil || !strings.Contains(err.Error(), "fork prompt OpenCode server") || errors.Is(err, ErrUnsupportedFork) ||
				test.wantErr != nil && !errors.Is(err, test.wantErr) || len(runner.calls) != 0 || len(topology.requests) != 0 {
				t.Fatalf("error=%v calls=%v launches=%v", err, runner.calls, topology.requests)
			}
		})
	}
}

func TestForkWithoutPromptRejectsUnusableOpenCodeSourceServerBeforeReadingSource(t *testing.T) {
	source := forkAgent("opencode", "w1:p1", "ses_source", "/tmp/project")
	for _, test := range []struct {
		name   string
		server OpenCodeServer
	}{
		{"remote host", OpenCodeServer{URL: "http://example.com:4096", Directory: source.CWD}},
		{"other directory", OpenCodeServer{URL: "http://127.0.0.1:4096", Directory: "/tmp/other"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			topology := &fakeTopology{}
			runner := &scriptedRunner{}
			forker := NewForker(Host{Runner: runner, Session: "named"})
			forker.launcher = topology
			_, err := forker.Fork(context.Background(), ForkRequest{
				Source: source, Profile: ForkProfile{Executable: "opencode"}, Placement: sessionhost.NewTab, OpenCodeServer: test.server,
			})
			if !errors.Is(err, ErrUnsupportedFork) || len(runner.calls) != 0 || len(topology.requests) != 0 {
				t.Fatalf("error=%v calls=%v launches=%v", err, runner.calls, topology.requests)
			}
		})
	}
}

func TestForkAcceptsMaximumReadyTimeout(t *testing.T) {
	source := forkAgent("codex", "w1:p1", "thread-source", "/tmp/project")
	child := forkAgent("codex", "w1:p2", "thread-child", "/tmp/project")
	topology := &fakeTopology{created: forkCreatedTarget(child.PaneID)}
	runner := &scriptedRunner{results: []command.Result{listForkResult(t, source), listForkResult(t, child), listForkResult(t, source)}}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher = topology
	forker.wait = func(context.Context, time.Duration) error { return nil }
	result, err := forker.Fork(context.Background(), ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "codex"}, Placement: sessionhost.NewPane, Direction: "right", ReadyTimeout: MaxForkReadyTimeout,
	})
	if err != nil || result.Child.Session.Value != "thread-child" || len(topology.requests) != 1 {
		t.Fatalf("result=%+v launches=%v error=%v", result, topology.requests, err)
	}
}

func TestForkRejectsSourceThatDriftedBeforeLaunch(t *testing.T) {
	source := forkAgent("codex", "w1:p1", "thread-source", "/tmp/project")
	for _, test := range []struct {
		name  string
		drift func(*Agent)
	}{
		{"working directory", func(a *Agent) { a.CWD = "/tmp/other" }},
		{"conversation", func(a *Agent) { a.Session.Value = "thread-other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			drifted := forkAgent("codex", "w1:p1", "thread-source", "/tmp/project")
			test.drift(&drifted)
			topology := &fakeTopology{}
			runner := &scriptedRunner{results: []command.Result{listForkResult(t, drifted)}}
			forker := NewForker(Host{Runner: runner, Session: "named"})
			forker.launcher = topology
			_, err := forker.Fork(context.Background(), ForkRequest{Source: source, Profile: ForkProfile{Executable: "codex"}, Placement: sessionhost.NewTab})
			if !errors.Is(err, ErrStaleReport) || len(topology.requests) != 0 {
				t.Fatalf("error=%v launches=%v", err, topology.requests)
			}
		})
	}
}

func TestForkCleansUpChildWhenSourceDriftedAfterChildReady(t *testing.T) {
	source := forkAgent("codex", "w1:p1", "thread-source", "/tmp/project")
	child := forkAgent("codex", "w1:p2", "thread-child", "/tmp/project")
	drifted := source
	drifted.CWD = "/tmp/other"
	created := forkCreatedTarget(child.PaneID)
	topology := &fakeTopology{created: created}
	runner := &scriptedRunner{results: []command.Result{listForkResult(t, source), listForkResult(t, child), listForkResult(t, drifted)}}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher = topology
	forker.wait = func(context.Context, time.Duration) error { return nil }
	_, err := forker.Fork(context.Background(), ForkRequest{Source: source, Profile: ForkProfile{Executable: "codex"}, Placement: sessionhost.NewPane, Direction: "right"})
	if !errors.Is(err, ErrStaleReport) || len(topology.cleanupCalls) != 1 || topology.cleanupCalls[0] != created {
		t.Fatalf("error=%v cleanup=%v", err, topology.cleanupCalls)
	}
}

func TestForkPromptDerivesOMPStatusSocketFromHerdrSession(t *testing.T) {
	source := forkAgent("omp", "w1:p1", "/tmp/source.jsonl", "/tmp/project")
	child := forkAgent("omp", "w1:p2", "/tmp/child.jsonl", "/tmp/project")
	created := forkCreatedTarget(child.PaneID)
	sessions := jsonResult(t, map[string]any{"sessions": []any{map[string]any{
		"name": "named", "default": false, "running": true,
		"session_dir": "/tmp/herdr", "socket_path": "/tmp/herdr/herdr.sock",
	}}})
	runner := &scriptedRunner{results: []command.Result{listForkResult(t, source), sessions, listForkResult(t, child), listForkResult(t, source)}}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher = &fakeTopology{created: created}
	forker.wait = func(context.Context, time.Duration) error { return nil }
	want, err := DeriveOMPStatusSocket("/tmp/herdr/herdr.sock", created.PaneID)
	if err != nil {
		t.Fatal(err)
	}
	delivered := ""
	forker.deliver = func(ctx context.Context, prompt Prompt, target PromptTarget) (PromptResult, error) {
		delivered = target.OMPStatusSocket
		return forkAcknowledgingDeliver(ctx, prompt, target)
	}
	prompt, _ := PreparePrompt("fork prompt")
	_, err = forker.Fork(context.Background(), ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "omp"}, Placement: sessionhost.NewPane, Direction: "right", Prompt: &prompt,
	})
	if err != nil || delivered != want {
		t.Fatalf("delivered socket=%q want=%q error=%v", delivered, want, err)
	}
}

func TestForkPollsPendingChildEveryFiftyMilliseconds(t *testing.T) {
	source := forkAgent("codex", "w1:p1", "thread-source", "/tmp/project")
	pending := forkAgent("codex", "w1:p2", "thread-source", "/tmp/project")
	child := forkAgent("codex", "w1:p2", "thread-child", "/tmp/project")
	runner := &scriptedRunner{results: []command.Result{listForkResult(t, source), listForkResult(t, pending), listForkResult(t, child), listForkResult(t, source)}}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher = &fakeTopology{created: forkCreatedTarget(child.PaneID)}
	var waits []time.Duration
	forker.wait = func(_ context.Context, interval time.Duration) error {
		waits = append(waits, interval)
		return nil
	}
	if _, err := forker.Fork(context.Background(), ForkRequest{Source: source, Profile: ForkProfile{Executable: "codex"}, Placement: sessionhost.NewPane, Direction: "right"}); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0] != 50*time.Millisecond {
		t.Fatalf("waits=%v, want one 50ms poll", waits)
	}
}

func TestForkOpenCodeChildWithOtherNativeSessionIsRejectedBeforeSourceRecheck(t *testing.T) {
	source := forkAgent("opencode", "w1:p1", "ses_source", "/tmp/project")
	other := forkAgent("opencode", "w1:p2", "ses_other", "/tmp/project")
	created := forkCreatedTarget(other.PaneID)
	topology := &fakeTopology{created: created}
	// The final source read would let a missing child check go unnoticed.
	runner := &scriptedRunner{results: []command.Result{listForkResult(t, source), listForkResult(t, source), listForkResult(t, other), listForkResult(t, source)}}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher = topology
	forker.wait = func(context.Context, time.Duration) error { return nil }
	forker.openCodeTUIReserve = fixedOpenCodeTUIReserve(49154)
	_, err := forker.Fork(context.Background(), ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "opencode"}, Placement: sessionhost.NewPane, Direction: "right",
		OpenCodeServer: forkOpenCodeServer(t, source),
	})
	if !errors.Is(err, ErrForkOutcomeUncertain) || !errors.Is(err, ErrStaleReport) || !strings.Contains(err.Error(), "fork child:") || len(topology.cleanupCalls) != 1 {
		t.Fatalf("error=%v cleanup=%v", err, topology.cleanupCalls)
	}
}

func TestForkOpenCodePromptedFocusedChildKeepsNativeSession(t *testing.T) {
	source := forkAgent("opencode", "w1:p1", "ses_source", "/tmp/project")
	child := forkAgent("opencode", "w1:p2", "ses_child", "/tmp/project")
	focused := child
	focused.Focused = true
	topology := &fakeTopology{created: forkCreatedTarget(child.PaneID)}
	runner := &scriptedRunner{results: []command.Result{
		listForkResult(t, source), listForkResult(t, source), listForkResult(t, child), listForkResult(t, source),
		jsonResult(t, infoResponse(forkWire(focused))), listForkResult(t, source),
	}}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher = topology
	forker.wait = func(context.Context, time.Duration) error { return nil }
	forker.openCodeTUIReserve = fixedOpenCodeTUIReserve(49154)
	forker.deliver = forkAcknowledgingDeliver
	prompt, _ := PreparePrompt("hello")
	result, err := forker.Fork(context.Background(), ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "opencode"}, Placement: sessionhost.NewPane, Direction: "right",
		OpenCodeServer: forkOpenCodeServer(t, source), Prompt: &prompt, Focus: true,
	})
	if err != nil || result.Child.Session == nil || result.Child.Session.Value != "ses_child" || !result.Child.Focused || result.Prompt.State != PromptAcknowledged || len(topology.cleanupCalls) != 0 {
		t.Fatalf("result=%+v cleanup=%v error=%v", result, topology.cleanupCalls, err)
	}
}

func TestForkOpenCodePromptedChildWithOtherNativeSessionStaysUncertain(t *testing.T) {
	source := forkAgent("opencode", "w1:p1", "ses_source", "/tmp/project")
	child := forkAgent("opencode", "w1:p2", "ses_child", "/tmp/project")
	topology := &fakeTopology{created: forkCreatedTarget(child.PaneID)}
	runner := &scriptedRunner{results: []command.Result{listForkResult(t, source), listForkResult(t, source), listForkResult(t, child), listForkResult(t, source)}}
	forker := NewForker(Host{Runner: runner, Session: "named"})
	forker.launcher = topology
	forker.wait = func(context.Context, time.Duration) error { return nil }
	forker.openCodeTUIReserve = fixedOpenCodeTUIReserve(49154)
	forker.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
		replaced := target.Agent
		replaced.Session = &SessionRef{Source: "herdr:opencode", Agent: "opencode", Kind: "id", Value: "ses_other"}
		return PromptResult{Target: target, Agent: replaced, State: PromptAcknowledged}, nil
	}
	prompt, _ := PreparePrompt("hello")
	result, err := forker.Fork(context.Background(), ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "opencode"}, Placement: sessionhost.NewPane, Direction: "right",
		OpenCodeServer: forkOpenCodeServer(t, source), Prompt: &prompt,
	})
	if !errors.Is(err, ErrForkOutcomeUncertain) || !errors.Is(err, ErrStaleReport) || !strings.Contains(err.Error(), "fork prompted child") ||
		result.Prompt.State != PromptAcknowledged || len(topology.cleanupCalls) != 0 {
		t.Fatalf("result=%+v cleanup=%v error=%v", result, topology.cleanupCalls, err)
	}
}

// A created child is kept only when its prompt was acknowledged; every other
// failure after the child exists must clean it up.
func TestForkFocusStageFailuresKeepOnlyAcknowledgedPromptChildren(t *testing.T) {
	for _, stage := range []string{"focus rejected", "focused child not ready", "source drift after focus"} {
		for _, mode := range []string{"no prompt", "prompt acknowledged", "prompt not acknowledged"} {
			t.Run(stage+"/"+mode, func(t *testing.T) {
				source := forkAgent("omp", "w1:p1", "/tmp/source.jsonl", "/tmp/project")
				child := forkAgent("omp", "w1:p2", "/tmp/child.jsonl", "/tmp/project")
				focused := child
				focused.Focused = true
				finalSource := source
				results := []command.Result{listForkResult(t, source), listForkResult(t, child), listForkResult(t, source), jsonResult(t, infoResponse(forkWire(focused)))}
				var errs []error
				switch stage {
				case "focus rejected":
					results[3] = command.Result{}
					errs = []error{nil, nil, nil, errors.New("focus failed")}
				case "focused child not ready":
					focused.CWD = "/tmp/other"
					results[3] = jsonResult(t, infoResponse(forkWire(focused)))
				case "source drift after focus":
					finalSource.CWD = "/tmp/other"
				}
				results = append(results, listForkResult(t, finalSource))
				created := forkCreatedTarget(child.PaneID)
				topology := &fakeTopology{created: created}
				forker := NewForker(Host{Runner: &scriptedRunner{results: results, errs: errs}, Session: "named"})
				forker.launcher = topology
				forker.wait = func(context.Context, time.Duration) error { return nil }
				request := ForkRequest{Source: source, Profile: ForkProfile{Executable: "omp"}, Placement: sessionhost.NewPane, Direction: "right", Focus: true, OMPStatusSocket: "/tmp/omp.sock"}
				if mode != "no prompt" {
					prompt, _ := PreparePrompt("hello")
					request.Prompt = &prompt
					forker.deliver = func(_ context.Context, _ Prompt, target PromptTarget) (PromptResult, error) {
						state := PromptAcknowledged
						if mode == "prompt not acknowledged" {
							state = CreatedPromptNotSent
						}
						return PromptResult{Target: target, Agent: target.Agent, State: state}, nil
					}
				}
				result, err := forker.Fork(context.Background(), request)
				if err == nil || stage != "focus rejected" && !errors.Is(err, ErrStaleReport) {
					t.Fatalf("error=%v", err)
				}
				if mode == "prompt acknowledged" {
					if result.Created != created || len(topology.cleanupCalls) != 0 {
						t.Fatalf("acknowledged child was not kept: result=%+v cleanup=%v", result, topology.cleanupCalls)
					}
					return
				}
				if result.Created != (sessionhost.CreatedTarget{}) || len(topology.cleanupCalls) != 1 || topology.cleanupCalls[0] != created {
					t.Fatalf("child was not cleaned up: result=%+v cleanup=%v", result, topology.cleanupCalls)
				}
			})
		}
	}
}

func TestComposeForkArgvRejectsUncleanSourceCWD(t *testing.T) {
	for _, cwd := range []string{"", "project", "/tmp/project/", "/tmp//project", "/tmp/a/../project", "/tmp/pro\x00ject"} {
		_, _, err := composeForkArgv(ForkRequest{Source: forkAgent("codex", "w1:p1", "thread", cwd), Profile: ForkProfile{Executable: "codex"}})
		if err == nil || !strings.Contains(err.Error(), "source CWD must be clean, absolute, and NUL-free") {
			t.Errorf("cwd %q: error=%v", cwd, err)
		}
	}
}

func TestComposeForkArgvRejectsUnsupportedSourceIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		kind   string
		value  string
		change func(*ForkRequest)
	}{
		{"missing conversation", "codex", "thread", func(r *ForkRequest) { r.Source.Session = nil }},
		{"missing pane", "codex", "thread", func(r *ForkRequest) { r.Source.PaneID = "" }},
		{"Codex session source", "codex", "thread", func(r *ForkRequest) { r.Source.Session.Source = "herdr:other" }},
		{"Codex session agent", "codex", "thread", func(r *ForkRequest) { r.Source.Session.Agent = "other" }},
		{"Codex session kind", "codex", "thread", func(r *ForkRequest) { r.Source.Session.Kind = "path" }},
		{"OMP session source", "omp", "/tmp/source.jsonl", func(r *ForkRequest) { r.Source.Session.Source = "herdr:other" }},
		{"OMP session agent", "omp", "/tmp/source.jsonl", func(r *ForkRequest) { r.Source.Session.Agent = "other" }},
		{"OMP session kind", "omp", "/tmp/source.jsonl", func(r *ForkRequest) { r.Source.Session.Kind = "id" }},
		{"OMP unclean session path", "omp", "/tmp/source.jsonl", func(r *ForkRequest) { r.Source.Session.Value = "/tmp/a/../source.jsonl" }},
		{"OMP relative status socket", "omp", "/tmp/source.jsonl", func(r *ForkRequest) { r.OMPStatusSocket = "status.sock" }},
		{"OMP unclean status socket", "omp", "/tmp/source.jsonl", func(r *ForkRequest) { r.OMPStatusSocket = "/tmp//status.sock" }},
		{"OMP status socket with NUL", "omp", "/tmp/source.jsonl", func(r *ForkRequest) { r.OMPStatusSocket = "/tmp/status\x00.sock" }},
		{"OpenCode session source", "opencode", "ses_source", func(r *ForkRequest) { r.Source.Session.Source = "herdr:other" }},
		{"OpenCode session agent", "opencode", "ses_source", func(r *ForkRequest) { r.Source.Session.Agent = "other" }},
		{"OpenCode session kind", "opencode", "ses_source", func(r *ForkRequest) { r.Source.Session.Kind = "path" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := ForkRequest{Source: forkAgent(test.kind, "w1:p1", test.value, "/tmp/project"), Profile: ForkProfile{Executable: test.kind}}
			if _, _, err := composeForkArgv(request); err != nil {
				t.Fatalf("unchanged %s request rejected: %v", test.kind, err)
			}
			test.change(&request)
			if _, _, err := composeForkArgv(request); !errors.Is(err, ErrUnsupportedFork) {
				t.Fatalf("error=%v, want %v", err, ErrUnsupportedFork)
			}
		})
	}
}

func TestValidateForkExecutableRequiresCanonicalHarnessName(t *testing.T) {
	longDirectory := strings.Repeat("a", maxPathBytes-len("//codex"))
	for _, test := range []struct {
		executable, kind string
		valid            bool
	}{
		{"codex", "codex", true},
		{"/opt/bin/codex", "codex", true},
		{"/" + longDirectory + "/codex", "codex", true},
		{"/" + longDirectory + "a/codex", "codex", false},
		{"", ".", false},
		{"/tmp/\n/codex", "codex", false},
		{"-codex", "-codex", false},
		{"/opt/bin/other", "codex", false},
		{"bin/codex", "codex", false},
		{"/opt/../codex", "codex", false},
	} {
		err := validateForkExecutable(test.executable, test.kind)
		if (err == nil) != test.valid {
			t.Errorf("executable length %d %q for %q: error=%v, valid=%v", len(test.executable), test.executable[:min(len(test.executable), 24)], test.kind, err, test.valid)
		}
	}
}

func TestValidateForkOptionsRejectsMissingAndMalformedValues(t *testing.T) {
	for _, test := range []struct {
		kind    string
		options []string
		want    string
	}{
		{"codex", []string{"--model"}, "requires a value"},
		{"omp", []string{"--thinking", "max", "--tools"}, "requires a value"},
		{"codex", []string{"--model", ""}, "invalid value"},
		{"codex", []string{"--model", "-x"}, "invalid value"},
		{"codex", []string{"--model", "a\x00b"}, "invalid value"},
	} {
		if _, err := validateForkOptions(test.kind, test.options); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s %q: error=%v, want %q", test.kind, test.options, err, test.want)
		}
	}
}

func TestClassifyForkChildRequiresEveryIdentityToMatch(t *testing.T) {
	created := forkCreatedTarget("w1:p2")
	for _, test := range []struct {
		name                   string
		change                 func(source, child *Agent)
		wantReady, wantPending bool
	}{
		{"ready child", func(*Agent, *Agent) {}, true, false},
		{"kind compared case-insensitively", func(_, child *Agent) { child.Kind = "OMP" }, true, false},
		{"unreported session", func(_, child *Agent) { child.Session = nil }, false, true},
		{"source session unchanged", func(_, child *Agent) { child.Session.Value = "/tmp/source.jsonl" }, false, true},
		{"other harness", func(_, child *Agent) { child.Kind = "codex" }, false, false},
		{"other Herdr session", func(_, child *Agent) { child.SessionName = "other" }, false, false},
		{"other workspace", func(_, child *Agent) { child.WorkspaceID = "w2" }, false, false},
		{"other tab", func(_, child *Agent) { child.TabID = "w1:t9" }, false, false},
		{"other pane", func(_, child *Agent) { child.PaneID = "w1:p9" }, false, false},
		{"other directory", func(_, child *Agent) { child.CWD = "/tmp/other" }, false, false},
		{"source without conversation", func(source, child *Agent) { source.Session, child.Session = nil, nil }, false, false},
		{"session source differs", func(_, child *Agent) { child.Session.Source = "herdr:other" }, false, false},
		{"session agent differs", func(_, child *Agent) { child.Session.Agent = "other" }, false, false},
		{"session kind differs", func(_, child *Agent) { child.Session.Kind = "id" }, false, false},
		{"relative OMP session path", func(_, child *Agent) { child.Session.Value = "child.jsonl" }, false, false},
		{"unclean OMP session path", func(_, child *Agent) { child.Session.Value = "/tmp/a/../child.jsonl" }, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			source, child := forkAgent("omp", "w1:p1", "/tmp/source.jsonl", "/tmp/project"), forkAgent("omp", "w1:p2", "/tmp/child.jsonl", "/tmp/project")
			test.change(&source, &child)
			ready, pending := classifyForkChild(source, child, created, "omp")
			if ready != test.wantReady || pending != test.wantPending {
				t.Fatalf("ready=%v pending=%v, want ready=%v pending=%v", ready, pending, test.wantReady, test.wantPending)
			}
		})
	}
}
