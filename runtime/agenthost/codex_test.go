package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

const testThread = "01a07d72-2102-7b71-8d28-819c786c1f0c"

func codexWire() wireAgent {
	a := validWireAgent("w:p")
	a.Agent = "codex"
	a.AgentSession = &wireSession{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: testThread}
	return a
}

// A real Unix HTTP/WebSocket server exercises the actual production dialer,
// framing, connection lifetime and cancellation; no production transport seam.
func unixServer(t *testing.T, handler func(context.Context, *websocket.Conn)) string {
	t.Helper()
	socket, _ := unixHTTPServer(t, "", func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow()
		handler(ctx, conn)
	})
	return socket
}

func unixHTTPServer(t *testing.T, socket string, handler func(context.Context, http.ResponseWriter, *http.Request)) (string, func()) {
	t.Helper()
	if socket == "" {
		dir, err := os.MkdirTemp("", "hpk-codex-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		socket = filepath.Join(dir, "app.sock")
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var handlers sync.WaitGroup
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		handler(ctx, w, r)
	})}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			_ = server.Close()
			<-done
			handlers.Wait()
		})
	}
	t.Cleanup(stop)
	return socket, stop
}

func receive(t *testing.T, ctx context.Context, c *websocket.Conn, want string) {
	t.Helper()
	kind, data, err := c.Read(ctx)
	if err != nil {
		t.Errorf("read %s: %v", want, err)
		return
	}
	var got, expected any
	if kind != websocket.MessageText || json.Unmarshal(data, &got) != nil || json.Unmarshal([]byte(want), &expected) != nil || !reflect.DeepEqual(got, expected) {
		t.Errorf("request = %s, want %s", data, want)
	}
}

func send(t *testing.T, ctx context.Context, c *websocket.Conn, data string) {
	t.Helper()
	if err := c.Write(ctx, websocket.MessageText, []byte(data)); err != nil {
		t.Errorf("write: %v", err)
	}
}

const initializeRequest = `{"id":1,"method":"initialize","params":{"clientInfo":{"name":"herdr-plugin-kit","version":"1"},"capabilities":{"experimentalApi":true}}}`
const initializeResponse = `{"id":1,"result":{"userAgent":"codex-cli/0.153.4","codexHome":"/private","platformFamily":"unix","platformOs":"linux","futureMetadata":true}}`

func exchange(t *testing.T, ctx context.Context, c *websocket.Conn) {
	t.Helper()
	receive(t, ctx, c, initializeRequest)
	send(t, ctx, c, initializeResponse)
	receive(t, ctx, c, `{"method":"initialized"}`)
	receive(t, ctx, c, `{"id":2,"method":"thread/read","params":{"threadId":"`+testThread+`","includeTurns":false}}`)
}

func threadResult(status string) string {
	return `{"id":2,"result":{"thread":{"id":"` + testThread + `","status":` + status + `,"preview":"DO_NOT_EXPOSE","newMetadata":{"value":true}}}}`
}

func assessOn(t *testing.T, socket string) (Assessment, error, *scriptedRunner) {
	t.Helper()
	a := codexWire()
	runner := &scriptedRunner{results: []command.Result{jsonResult(t, listResponse(a)), jsonResult(t, listResponse(a))}}
	got, err := NewProbe(Host{Runner: runner, CodexSocket: socket}).Assess(context.Background(), mustPublic(t, a))
	return got, err, runner
}

func TestCodexStatusMappingsAndOnlyAllowedMethods(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		want         Assessment
	}{
		{"idle", `{"type":"idle"}`, Assessment{Idle, CodexStatus, true}},
		{"active", `{"type":"active","activeFlags":[]}`, Assessment{Working, CodexStatus, true}},
		{"approval", `{"type":"active","activeFlags":["waitingOnApproval"]}`, Assessment{Blocked, Approval, true}},
		{"input", `{"type":"active","activeFlags":["waitingOnUserInput"]}`, Assessment{Blocked, UserInput, true}},
		{"both", `{"type":"active","activeFlags":["waitingOnUserInput","waitingOnApproval"]}`, Assessment{Blocked, ApprovalAndInput, true}},
		{"unloaded", `{"type":"notLoaded"}`, codexUnknown(NotManaged)},
		{"system error", `{"type":"systemError"}`, codexUnknown(AgentSystemError)},
		{"unknown type", `{"type":"future"}`, codexUnknown(UnsupportedStatus)},
		{"unknown flag", `{"type":"active","activeFlags":["waitingOnApproval","future"]}`, codexUnknown(UnsupportedStatus)},
		{"additive metadata", `{"type":"idle","future":[1]}`, Assessment{Idle, CodexStatus, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := unixServer(t, func(ctx context.Context, c *websocket.Conn) {
				exchange(t, ctx, c)
				send(t, ctx, c, threadResult(tc.status))
			})
			got, err, runner := assessOn(t, socket)
			if err != nil || got != tc.want {
				t.Fatalf("assessment=%+v error=%v", got, err)
			}
			if !reflect.DeepEqual(runner.calls, [][]string{{"herdr", "agent", "list"}, {"herdr", "agent", "list"}}) {
				t.Fatalf("unexpected Herdr commands (Codex must never read panes): %q", runner.calls)
			}
		})
	}
}

func TestCodexRejectsMalformedResponsesWithoutLeaking(t *testing.T) {
	const secret = "PRIVATE_CREDENTIAL_SENTINEL"
	valid := threadResult(`{"type":"idle"}`)
	for _, tc := range []struct {
		name, data string
		init       bool
	}{
		{"invalid JSON", "{" + secret, false},
		{"invalid UTF8", strings.Replace(threadResult(`{"type":"idle"}`), "DO_NOT_EXPOSE", "\xff", 1), false},
		{"extra JSON", threadResult(`{"type":"idle"}`) + "{}", false},
		{"missing result", `{"id":2}`, false},
		{"null result", `{"id":2,"result":null}`, false},
		{"server error", `{"id":2,"error":{"code":-32600,"message":"` + secret + `"}}`, false},
		{"result and error", strings.Replace(valid, `{"id":2,`, `{"id":2,"error":null,`, 1), false},
		{"wrong ID", strings.Replace(valid, `"id":2`, `"id":3`, 1), false},
		{"string ID", strings.Replace(valid, `"id":2`, `"id":"2"`, 1), false},
		{"missing ID", strings.Replace(valid, `"id":2,`, "", 1), false},
		{"null ID", strings.Replace(valid, `"id":2`, `"id":null`, 1), false},
		{"missing thread", `{"id":2,"result":{}}`, false},
		{"null thread", `{"id":2,"result":{"thread":null}}`, false},
		{"different thread", strings.Replace(threadResult(`{"type":"idle"}`), testThread, "other", 1), false},
		{"missing thread ID", `{"id":2,"result":{"thread":{"status":{"type":"idle"}}}}`, false},
		{"missing status", `{"id":2,"result":{"thread":{"id":"` + testThread + `"}}}`, false},
		{"null status", threadResult("null"), false},
		{"missing type", threadResult("{}"), false},
		{"wrong type type", threadResult(`{"type":3}`), false},
		{"missing flags", threadResult(`{"type":"active"}`), false},
		{"null flags", threadResult(`{"type":"active","activeFlags":null}`), false},
		{"nonarray flags", threadResult(`{"type":"active","activeFlags":"waitingOnApproval"}`), false},
		{"nonstring flag", threadResult(`{"type":"active","activeFlags":[42]}`), false},
		{"null flag", threadResult(`{"type":"active","activeFlags":[null]}`), false},
		{"approval request", `{"id":99,"method":"item/commandExecution/requestApproval","params":{"secret":"` + secret + `"}}`, false},
		{"empty method", `{"method":""}`, false},
		{"null method", strings.Replace(valid, `{"id":2,`, `{"id":2,"method":null,`, 1), false},
		{"nonstring method", `{"method":42}`, false},
		{"notification result", `{"method":"event","result":{}}`, false},
		{"notification error", `{"method":"event","error":{}}`, false},
		{"initialization error", `{"id":1,"error":{"message":"` + secret + `"}}`, true},
		{"initialization wrong ID", strings.Replace(initializeResponse, `"id":1`, `"id":2`, 1), true},
		{"initialization missing field", `{"id":1,"result":{}}`, true},
		{"initialization wrong field", `{"id":1,"result":{"userAgent":32}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := unixServer(t, func(ctx context.Context, c *websocket.Conn) {
				if tc.init {
					receive(t, ctx, c, initializeRequest)
				} else {
					exchange(t, ctx, c)
				}
				send(t, ctx, c, tc.data)
				if !tc.init {
					// Even if a valid snapshot follows, a malformed message or
					// server request must not be silently treated as a notification.
					// Rejection may already have closed the connection.
					_ = c.Write(ctx, websocket.MessageText, []byte(valid))
				}
				// A rejected response must close, never produce approval replies.
				_, data, err := c.Read(ctx)
				if err == nil {
					t.Errorf("unexpected outbound data: %s", data)
				}
			})
			got, err, _ := assessOn(t, socket)
			if err == nil || got != codexUnknown(CodexUnavailable) {
				t.Fatalf("assessment=%+v error=%v", got, err)
			}
			data, _ := json.Marshal(got)
			if strings.Contains(err.Error(), secret) || strings.Contains(string(data), secret) {
				t.Fatal("server data leaked")
			}
		})
	}
}

func TestCodexMissingConfigurationAndInvalidInputs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(*Probe, *Agent)
		reason    Reason
		wantError bool
	}{
		{"no socket", func(p *Probe, a *Agent) { p.Host.CodexSocket = "" }, MissingCodexSocket, false},
		{"no session", func(p *Probe, a *Agent) { a.Session = nil }, MissingCodexSession, false},
		{"not Codex session", func(p *Probe, a *Agent) { a.Session.Agent = "claude" }, MissingCodexSession, false},
		{"not ID session", func(p *Probe, a *Agent) { a.Session.Kind = "path" }, MissingCodexSession, false},
		{"relative socket", func(p *Probe, a *Agent) { p.Host.CodexSocket = "relative.sock" }, CodexUnavailable, true},
		{"network URL", func(p *Probe, a *Agent) { p.Host.CodexSocket = "ws://localhost:1234" }, CodexUnavailable, true},
		{"NUL socket", func(p *Probe, a *Agent) { p.Host.CodexSocket = "/socket\x00" }, CodexUnavailable, true},
		{"invalid pane", func(p *Probe, a *Agent) { a.PaneID = "" }, CodexUnavailable, true},
		{"empty session value", func(p *Probe, a *Agent) { a.Session.Value = "" }, MissingCodexSession, false},
		{"malformed session value", func(p *Probe, a *Agent) { a.Session.Value = "thread\n" }, CodexUnavailable, true},
		{"oversized session value", func(p *Probe, a *Agent) { a.Session.Value = strings.Repeat("x", maxIdentityBytes+1) }, CodexUnavailable, true},
		{"cross Herdr session", func(p *Probe, a *Agent) { a.SessionName = "other" }, CodexUnavailable, true},
		{"not NewProbe", func(p *Probe, a *Agent) { p.wait = nil }, CodexUnavailable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &scriptedRunner{}
			var requests atomic.Int32
			socket, stop := unixHTTPServer(t, "", func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
			})
			p := NewProbe(Host{Runner: runner, CodexSocket: socket})
			a := mustPublic(t, codexWire())
			tc.change(&p, &a)
			got, err := p.Assess(context.Background(), a)
			stop()
			if requests.Load() != 0 {
				t.Fatal("unexpected socket request")
			}
			if got != codexUnknown(tc.reason) || (err != nil) != tc.wantError || len(runner.calls) != 0 {
				t.Fatalf("assessment=%+v error=%v calls=%q", got, err, runner.calls)
			}
		})
	}
}

func TestCodexEmptySessionPreservesValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Probe, *Agent)
	}{
		{"pane", func(p *Probe, a *Agent) { a.PaneID = "" }},
		{"workspace", func(p *Probe, a *Agent) { a.WorkspaceID = "" }},
		{"tab", func(p *Probe, a *Agent) { a.TabID = "" }},
		{"status", func(p *Probe, a *Agent) { a.Status = "invalid" }},
		{"Herdr session", func(p *Probe, a *Agent) { a.SessionName = "bad\n" }},
		{"cross Herdr session", func(p *Probe, a *Agent) { a.SessionName = "other" }},
		{"probe", func(p *Probe, a *Agent) { p.wait = nil }},
		{"empty source", func(p *Probe, a *Agent) { a.Session.Source = "" }},
		{"malformed source", func(p *Probe, a *Agent) { a.Session.Source = "source\n" }},
		{"empty agent", func(p *Probe, a *Agent) { a.Session.Agent = "" }},
		{"malformed agent", func(p *Probe, a *Agent) { a.Session.Agent = "codex\x00" }},
		{"empty kind", func(p *Probe, a *Agent) { a.Session.Kind = "" }},
		{"malformed kind", func(p *Probe, a *Agent) { a.Session.Kind = "id\r" }},
		{"cancelled", func(p *Probe, a *Agent) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &scriptedRunner{}
			p := NewProbe(Host{Runner: runner, CodexSocket: "/unused.sock"})
			a := mustPublic(t, codexWire())
			a.Session.Value = ""
			tc.change(&p, &a)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.name == "cancelled" {
				cancel()
			}
			got, err := p.Assess(ctx, a)
			if got != codexUnknown(CodexUnavailable) || err == nil || len(runner.calls) != 0 {
				t.Fatalf("assessment=%+v error=%v calls=%q", got, err, runner.calls)
			}
			if tc.name == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v, want cancellation", err)
			}
		})
	}
}

func TestCodexImmutableIdentityAndVolatileMetadata(t *testing.T) {
	for _, phase := range []string{"before", "after"} {
		for _, field := range []string{"kind", "workspace", "tab", "pane", "source", "agent", "session kind", "thread", "session missing", "disappeared"} {
			t.Run(phase+"/"+field, func(t *testing.T) {
				a, b := codexWire(), codexWire()
				switch field {
				case "kind":
					b.Agent = "claude"
				case "workspace":
					b.WorkspaceID = "other"
				case "tab":
					b.TabID = "other"
				case "pane":
					b.PaneID = "other"
				case "source":
					b.AgentSession.Source = "other"
				case "agent":
					b.AgentSession.Agent = "other"
				case "session kind":
					b.AgentSession.Kind = "path"
				case "thread":
					b.AgentSession.Value = "other"
				case "session missing":
					b.AgentSession = nil
				}
				changed := jsonResult(t, listResponse(b))
				if field == "disappeared" {
					changed = jsonResult(t, listResponse())
				}
				results := []command.Result{jsonResult(t, listResponse(a)), changed}
				if phase == "before" {
					results = []command.Result{changed}
				}
				var connections atomic.Int32
				socket := unixServer(t, func(ctx context.Context, c *websocket.Conn) {
					connections.Add(1)
					exchange(t, ctx, c)
					send(t, ctx, c, threadResult(`{"type":"idle"}`))
				})
				runner := &scriptedRunner{results: results}
				got, err := NewProbe(Host{Runner: runner, CodexSocket: socket}).Assess(context.Background(), mustPublic(t, a))
				if got != codexUnknown(Unsettled) || !errors.Is(err, ErrStaleReport) {
					t.Fatalf("assessment=%+v error=%v", got, err)
				}
				if phase == "before" && connections.Load() != 0 {
					t.Fatal("connected before validating identity")
				}
			})
		}
	}
	a, b, c := codexWire(), codexWire(), codexWire()
	b.AgentStatus, b.Revision, b.StateChangeSeq = "working", 99, 101
	c.AgentStatus, c.Revision, c.StateChangeSeq = "blocked", 200, 300
	c.CWD, c.Title, c.Name = "/different", "different", "renamed"
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		exchange(t, ctx, conn)
		send(t, ctx, conn, threadResult(`{"type":"idle"}`))
	})
	runner := &scriptedRunner{results: []command.Result{jsonResult(t, listResponse(b)), jsonResult(t, listResponse(c))}}
	got, err := NewProbe(Host{Runner: runner, CodexSocket: socket}).Assess(context.Background(), mustPublic(t, a))
	if err != nil || got != (Assessment{Idle, CodexStatus, true}) {
		t.Fatalf("volatile fields treated as drift: %+v %v", got, err)
	}
}

type runnerFunc func(context.Context, string, []string) (command.Result, error)

func (f runnerFunc) Run(ctx context.Context, exe string, args []string) (command.Result, error) {
	return f(ctx, exe, args)
}

func TestCodexBoundsIncludeHerdrAndCallerCancellation(t *testing.T) {
	for _, phase := range []string{"before", "after", "initialize", "thread"} {
		t.Run(phase, func(t *testing.T) {
			socket := unixServer(t, func(ctx context.Context, c *websocket.Conn) {
				if phase == "initialize" {
					<-ctx.Done()
					return
				}
				exchange(t, ctx, c)
				if phase == "thread" {
					<-ctx.Done()
					return
				}
				send(t, ctx, c, threadResult(`{"type":"idle"}`))
			})
			var calls int
			a := codexWire()
			runner := runnerFunc(func(ctx context.Context, _ string, _ []string) (command.Result, error) {
				calls++
				if (phase == "before" && calls == 1) || (phase == "after" && calls == 2) {
					<-ctx.Done()
					return command.Result{}, ctx.Err()
				}
				return jsonResult(t, listResponse(a)), nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			start := time.Now()
			got, err := NewProbe(Host{Runner: runner, CodexSocket: socket}).Assess(ctx, mustPublic(t, a))
			if !errors.Is(err, context.DeadlineExceeded) || got != codexUnknown(CodexUnavailable) || time.Since(start) > 500*time.Millisecond {
				t.Fatalf("assessment=%+v error=%v elapsed=%v", got, err, time.Since(start))
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &scriptedRunner{}
	got, err := NewProbe(Host{Runner: runner, CodexSocket: "/unused"}).Assess(ctx, mustPublic(t, codexWire()))
	if !errors.Is(err, context.Canceled) || got.Stable || len(runner.calls) != 0 {
		t.Fatalf("got=%+v error=%v", got, err)
	}
}

func TestCodexTotalTwoSecondBudget(t *testing.T) {
	socket := unixServer(t, func(ctx context.Context, c *websocket.Conn) { <-ctx.Done() })
	a := codexWire()
	var deadline time.Time
	runner := runnerFunc(func(ctx context.Context, _ string, _ []string) (command.Result, error) {
		deadline, _ = ctx.Deadline()
		select {
		case <-time.After(600 * time.Millisecond):
		case <-ctx.Done():
		}
		return jsonResult(t, listResponse(a)), nil
	})
	start := time.Now()
	got, err := NewProbe(Host{Runner: runner, CodexSocket: socket}).Assess(context.Background(), mustPublic(t, a))
	elapsed := time.Since(start)
	if deadline.Sub(start) < 1900*time.Millisecond || deadline.Sub(start) > 2100*time.Millisecond || elapsed < 1900*time.Millisecond || elapsed > 2400*time.Millisecond || !errors.Is(err, context.DeadlineExceeded) || got.Stable {
		t.Fatalf("deadline offset=%v elapsed=%v assessment=%+v error=%v", deadline.Sub(start), elapsed, got, err)
	}
}

func TestCodexTransportFailureAndSanitizedHerdrError(t *testing.T) {
	got, err, _ := assessOn(t, filepath.Join(t.TempDir(), "absent.sock"))
	if err == nil || got != codexUnknown(CodexUnavailable) || strings.Contains(err.Error(), "absent.sock") {
		t.Fatalf("got=%+v error=%v", got, err)
	}
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			socket := unixServer(t, func(ctx context.Context, c *websocket.Conn) {
				exchange(t, ctx, c)
				send(t, ctx, c, threadResult(`{"type":"idle"}`))
			})
			var calls int
			a := codexWire()
			r := runnerFunc(func(context.Context, string, []string) (command.Result, error) {
				calls++
				if !after || calls == 2 {
					return command.Result{}, errors.New("PRIVATE_CREDENTIAL_SENTINEL")
				}
				return jsonResult(t, listResponse(a)), nil
			})
			got, err := NewProbe(Host{Runner: r, CodexSocket: socket}).Assess(context.Background(), mustPublic(t, a))
			if err == nil || got != codexUnknown(CodexUnavailable) || strings.Contains(err.Error(), "PRIVATE_CREDENTIAL_SENTINEL") {
				t.Fatalf("got=%+v error=%v", got, err)
			}
		})
	}
	socket := unixServer(t, func(ctx context.Context, c *websocket.Conn) { receive(t, ctx, c, initializeRequest) })
	got, err, _ = assessOn(t, socket)
	if err == nil || got.Stable {
		t.Fatalf("disconnect accepted: %+v %v", got, err)
	}
}

func TestCodexFreshConcurrentConnectionsAndNoStaleState(t *testing.T) {
	var connections atomic.Int32
	socket := unixServer(t, func(ctx context.Context, c *websocket.Conn) {
		n := connections.Add(1)
		exchange(t, ctx, c)
		status := `{"type":"idle"}`
		if n > 8 {
			status = `{"type":"notLoaded"}`
		}
		send(t, ctx, c, threadResult(status))
	})
	a := codexWire()
	runner := runnerFunc(func(ctx context.Context, exe string, args []string) (command.Result, error) {
		if !reflect.DeepEqual(args, []string{"agent", "list"}) {
			t.Errorf("unexpected Herdr call: %q", args)
		}
		return jsonResult(t, listResponse(a)), nil
	})
	probe := NewProbe(Host{Runner: runner, CodexSocket: socket})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := probe.Assess(context.Background(), mustPublic(t, a))
			if err != nil || got != (Assessment{Idle, CodexStatus, true}) {
				t.Errorf("assessment=%+v error=%v", got, err)
			}
		}()
	}
	wg.Wait()
	if connections.Load() != 8 {
		t.Fatalf("connections=%d", connections.Load())
	}
	got, err := probe.Assess(context.Background(), mustPublic(t, a))
	if err != nil || got != codexUnknown(NotManaged) || connections.Load() != 9 {
		t.Fatalf("stale state: %+v %v connections=%d", got, err, connections.Load())
	}
}

func paddedMessage(s string, size int) string {
	// JSON whitespace counts toward the on-wire application message size.
	return s + strings.Repeat(" ", size-len(s))
}

func TestCodexExactMessageAndAggregateTrafficBounds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		messages  []string
		wantError bool
	}{
		{"one MiB", []string{paddedMessage(threadResult(`{"type":"idle"}`), 1<<20)}, false},
		{"over one MiB", []string{paddedMessage(threadResult(`{"type":"idle"}`), (1<<20)+1)}, true},
		{"four MiB aggregate", []string{
			paddedMessage(`{"method":"event"}`, 1<<20),
			paddedMessage(`{"method":"event"}`, 1<<20),
			paddedMessage(`{"method":"event"}`, 1<<20),
			paddedMessage(threadResult(`{"type":"idle"}`), (1<<20)-len(initializeResponse)),
		}, false},
		{"over four MiB aggregate", []string{
			paddedMessage(`{"method":"event"}`, 1<<20),
			paddedMessage(`{"method":"event"}`, 1<<20),
			paddedMessage(`{"method":"event"}`, 1<<20),
			paddedMessage(threadResult(`{"type":"idle"}`), (1<<20)-len(initializeResponse)+1),
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := unixServer(t, func(ctx context.Context, c *websocket.Conn) {
				exchange(t, ctx, c)
				for _, message := range tc.messages {
					if err := c.Write(ctx, websocket.MessageText, []byte(message)); err != nil {
						if !tc.wantError {
							t.Errorf("write: %v", err)
						}
						return
					}
				}
			})
			got, err, _ := assessOn(t, socket)
			want := Assessment{Idle, CodexStatus, true}
			if tc.wantError {
				want = codexUnknown(CodexUnavailable)
			}
			if (err != nil) != tc.wantError || got != want {
				t.Fatalf("assessment=%+v error=%v", got, err)
			}
		})
	}
	for _, notifications := range []int{62, 63} {
		t.Run(fmt.Sprintf("notification count %d", notifications), func(t *testing.T) {
			socket := unixServer(t, func(ctx context.Context, c *websocket.Conn) {
				exchange(t, ctx, c)
				for i := 0; i < notifications; i++ {
					send(t, ctx, c, `{"method":"event","params":{"ignored":"private"}}`)
				}
				send(t, ctx, c, threadResult(`{"type":"idle"}`))
			})
			got, err, _ := assessOn(t, socket)
			if notifications == 62 {
				if err != nil || got != (Assessment{Idle, CodexStatus, true}) {
					t.Fatalf("assessment=%+v error=%v", got, err)
				}
			} else if err == nil || got != codexUnknown(CodexUnavailable) {
				t.Fatalf("limit not enforced: %+v %v", got, err)
			}
		})
	}
}

func TestCodexRejectsBinaryAndClosesAfterSuccess(t *testing.T) {
	socket := unixServer(t, func(ctx context.Context, c *websocket.Conn) {
		exchange(t, ctx, c)
		if err := c.Write(ctx, websocket.MessageBinary, []byte(threadResult(`{"type":"idle"}`))); err != nil {
			t.Errorf("write: %v", err)
		}
	})
	got, err, _ := assessOn(t, socket)
	if err == nil || got != codexUnknown(CodexUnavailable) {
		t.Fatalf("binary accepted: %+v %v", got, err)
	}
	closed := make(chan struct{})
	socket = unixServer(t, func(ctx context.Context, c *websocket.Conn) {
		exchange(t, ctx, c)
		send(t, ctx, c, threadResult(`{"type":"idle"}`))
		_, _, err := c.Read(ctx)
		if err == nil {
			t.Error("unexpected message after assessment")
		}
		close(closed)
	})
	got, err, _ = assessOn(t, socket)
	if err != nil || got != (Assessment{Idle, CodexStatus, true}) {
		t.Fatalf("assessment=%+v error=%v", got, err)
	}
	select {
	case <-closed:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("assessment left connection open")
	}
}

func TestCodexCancellationAfterFinalHerdrRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := unixServer(t, func(ctx context.Context, c *websocket.Conn) {
		exchange(t, ctx, c)
		send(t, ctx, c, threadResult(`{"type":"idle"}`))
	})
	var calls int
	a := codexWire()
	r := runnerFunc(func(context.Context, string, []string) (command.Result, error) {
		calls++
		if calls == 2 {
			cancel()
		}
		return jsonResult(t, listResponse(a)), nil
	})
	got, err := NewProbe(Host{Runner: r, CodexSocket: socket}).Assess(ctx, mustPublic(t, a))
	if !errors.Is(err, context.Canceled) || got != codexUnknown(CodexUnavailable) {
		t.Fatalf("assessment=%+v error=%v", got, err)
	}
}

func TestCodexRestartAndFreshQuery(t *testing.T) {
	handler := func(status string) func(context.Context, http.ResponseWriter, *http.Request) {
		return func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept: %v", err)
				return
			}
			defer conn.CloseNow()
			exchange(t, ctx, conn)
			send(t, ctx, conn, threadResult(status))
		}
	}
	socket, stop := unixHTTPServer(t, "", handler(`{"type":"idle"}`))
	a := codexWire()
	runner := runnerFunc(func(context.Context, string, []string) (command.Result, error) {
		return jsonResult(t, listResponse(a)), nil
	})
	probe := NewProbe(Host{Runner: runner, CodexSocket: socket})
	got, err := probe.Assess(context.Background(), mustPublic(t, a))
	if err != nil || got != (Assessment{Idle, CodexStatus, true}) {
		t.Fatalf("first=%+v error=%v", got, err)
	}
	stop()
	got, err = probe.Assess(context.Background(), mustPublic(t, a))
	if err == nil || got != codexUnknown(CodexUnavailable) {
		t.Fatalf("stopped=%+v error=%v", got, err)
	}
	_, _ = unixHTTPServer(t, socket, handler(`{"type":"notLoaded"}`))
	got, err = probe.Assess(context.Background(), mustPublic(t, a))
	if err != nil || got != codexUnknown(NotManaged) {
		t.Fatalf("restarted=%+v error=%v", got, err)
	}
}

func TestCodexHandshakeRejectsRedirectsAndHonorsDeadline(t *testing.T) {
	var requests atomic.Int32
	socket, _ := unixHTTPServer(t, "", func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Location", "http://do-not-connect.invalid/")
		w.WriteHeader(http.StatusFound)
	})
	got, err, _ := assessOn(t, socket)
	if err == nil || got != codexUnknown(CodexUnavailable) || requests.Load() != 1 {
		t.Fatalf("redirect result=%+v error=%v requests=%d", got, err, requests.Load())
	}
	socket, _ = unixHTTPServer(t, "", func(ctx context.Context, w http.ResponseWriter, r *http.Request) { <-ctx.Done() })
	a := codexWire()
	runner := &scriptedRunner{results: []command.Result{jsonResult(t, listResponse(a))}}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err = NewProbe(Host{Runner: runner, CodexSocket: socket}).Assess(ctx, mustPublic(t, a))
	if !errors.Is(err, context.DeadlineExceeded) || got != codexUnknown(CodexUnavailable) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("assessment=%+v error=%v elapsed=%v", got, err, time.Since(start))
	}
}

func TestCodexNamedSessionAndCaseInsensitiveAgentKind(t *testing.T) {
	socket := unixServer(t, func(ctx context.Context, c *websocket.Conn) {
		exchange(t, ctx, c)
		send(t, ctx, c, threadResult(`{"type":"idle"}`))
	})
	a := codexWire()
	a.Agent = "CoDeX"
	runner := &scriptedRunner{results: []command.Result{jsonResult(t, listResponse(a)), jsonResult(t, listResponse(a))}}
	target := mustPublic(t, a)
	target.SessionName = "named"
	got, err := NewProbe(Host{Runner: runner, Herdr: "/opt/herdr", Session: "named", CodexSocket: socket}).Assess(context.Background(), target)
	if err != nil || got != (Assessment{Idle, CodexStatus, true}) || !reflect.DeepEqual(runner.calls, [][]string{{"/opt/herdr", "--session", "named", "agent", "list"}, {"/opt/herdr", "--session", "named", "agent", "list"}}) {
		t.Fatalf("assessment=%+v error=%v calls=%q", got, err, runner.calls)
	}
}
