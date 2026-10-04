package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// codexLifecycleHandshake answers the initialize handshake and reports false when the
// connection ends or the server stops first. A client that finishes before the
// server consumed its last frame is not a test failure.
func codexLifecycleHandshake(ctx context.Context, conn *websocket.Conn) bool {
	_, data, err := conn.Read(ctx)
	var request rpcTestRequest
	if err != nil || json.Unmarshal(data, &request) != nil || request.Method != "initialize" {
		return false
	}
	if conn.Write(ctx, websocket.MessageText, []byte(`{"id":`+jsonNumber(request.ID)+`,"result":{"userAgent":"codex-cli/0.154.0"}}`)) != nil {
		return false
	}
	_, data, err = conn.Read(ctx)
	return err == nil && string(data) == `{"method":"initialized"}`
}

func TestNewCodexLifecycleRejectsUnsafeSocketBeforeDialing(t *testing.T) {
	for name, socket := range map[string]string{
		"relative":  "app.sock",
		"unclean":   "/tmp/hpk/../app.sock",
		"NUL byte":  "/tmp/hpk\x00app.sock",
		"directory": "/tmp/hpk/",
	} {
		t.Run(name, func(t *testing.T) {
			lifecycle, err := newCodexLifecycle(context.Background(), socket)
			if lifecycle != nil || err == nil || err.Error() != "Codex socket must be clean, absolute, and NUL-free" {
				t.Fatalf("lifecycle=%v error=%v, want the socket validation error", lifecycle, err)
			}
		})
	}
}

func TestCodexLifecycleUnidentifiedServerIsAProtocolErrorWithoutInitializedNotice(t *testing.T) {
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		request := readRPCRequest(t, ctx, conn, "initialize")
		sendRPCResult(t, ctx, conn, request.ID, `{"userAgent":""}`)
		if _, data, err := conn.Read(ctx); err == nil {
			t.Errorf("client sent %s after an unidentified initialize response", data)
		}
	})
	lifecycle, err := newCodexLifecycle(context.Background(), socket)
	if lifecycle != nil {
		lifecycle.Close()
	}
	if lifecycle != nil || !errors.Is(err, errCodexProtocol) {
		t.Fatalf("lifecycle=%v error=%v, want errCodexProtocol", lifecycle, err)
	}
}

func TestCodexLifecycleRequestSizeLimitIsInclusive(t *testing.T) {
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		lifecycleInitialize(t, ctx, conn)
		conn.SetReadLimit(2 * codexMessageBytes)
		request := readRPCRequest(t, ctx, conn, "test/limit")
		sendRPCResult(t, ctx, conn, request.ID, `{}`)
	})
	lifecycle, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Close()
	encodedLength := func(id, padding int) int {
		encoded, _ := json.Marshal(struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params any    `json:"params"`
		}{id, "test/limit", map[string]string{"padding": strings.Repeat("a", padding)}})
		return len(encoded)
	}
	fill := codexMessageBytes - encodedLength(lifecycle.nextID+1, 0)
	if encodedLength(lifecycle.nextID+1, fill) != codexMessageBytes {
		t.Fatalf("padding %d does not produce a request of exactly %d bytes", fill, codexMessageBytes)
	}
	var response struct{}
	if err := lifecycle.call(context.Background(), "test/limit", map[string]string{"padding": strings.Repeat("a", fill)}, &response); err != nil {
		t.Fatalf("request of exactly %d bytes rejected: %v", codexMessageBytes, err)
	}
	if err := lifecycle.call(context.Background(), "test/limit", map[string]string{"padding": strings.Repeat("a", fill+1)}, &response); !errors.Is(err, errCodexProtocol) {
		t.Fatalf("request of %d bytes error=%v, want errCodexProtocol", codexMessageBytes+1, err)
	}
}

func TestCodexLifecycleDuplicateStartNoticeMustMatchTheFirst(t *testing.T) {
	c := &codexLifecycle{armed: true, started: make(map[string]codexThreadIdentity)}
	started := `{"thread":{"id":"thread-new","cwd":"/tmp/project","modelProvider":"openai"}}`
	for attempt := 1; attempt <= 2; attempt++ {
		if err := c.observe("thread/started", json.RawMessage(started)); err != nil {
			t.Fatalf("identical start notice %d error=%v", attempt, err)
		}
	}
	changed := `{"thread":{"id":"thread-new","cwd":"/tmp/other","modelProvider":"openai"}}`
	if err := c.observe("thread/started", json.RawMessage(changed)); !errors.Is(err, errCodexProtocol) {
		t.Fatalf("changed start notice error=%v, want errCodexProtocol", err)
	}
}

func TestCodexLifecycleReadConstraintsRejectsMissingConfig(t *testing.T) {
	for name, config := range map[string]string{"null": `{"config":null,"origins":{}}`, "absent": `{"origins":{}}`} {
		t.Run(name, func(t *testing.T) {
			socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
				lifecycleInitialize(t, ctx, conn)
				request := readRPCRequest(t, ctx, conn, "config/read")
				sendRPCResult(t, ctx, conn, request.ID, config)
				requirements := readRPCRequest(t, ctx, conn, "configRequirements/read")
				sendRPCResult(t, ctx, conn, requirements.ID, `{"requirements":null}`)
			})
			lifecycle, err := newCodexLifecycle(context.Background(), socket)
			if err != nil {
				t.Fatal(err)
			}
			defer lifecycle.Close()
			if snapshot, err := lifecycle.ReadConstraints(context.Background(), "/tmp/project"); !errors.Is(err, errCodexProtocol) || snapshot.Config != nil {
				t.Fatalf("snapshot=%+v error=%v, want errCodexProtocol", snapshot, err)
			}
		})
	}
}

func TestCodexLifecycleConversionConstraintsAcceptEitherReviewerAndEnforceRequirements(t *testing.T) {
	policy := managedPolicy()
	effective := managedPreflight("/tmp/project").EffectiveCodexPolicy
	for _, reviewer := range []string{"user", "auto_review"} {
		for name, configured := range map[string]json.RawMessage{"configured": json.RawMessage(fmt.Sprintf("%q", reviewer)), "omitted": json.RawMessage(`null`)} {
			t.Run(reviewer+" "+name, func(t *testing.T) {
				effective := effective
				effective.ApprovalsReviewer = reviewer
				snapshot := codexConstraintSnapshot{
					Config:       map[string]json.RawMessage{"approvals_reviewer": configured},
					Requirements: map[string]json.RawMessage{"requirements": json.RawMessage(fmt.Sprintf(`{"allowedApprovalsReviewers":[%q]}`, reviewer))},
				}
				preflight, err := snapshot.validateConversion("/tmp/project", policy, effective)
				if err != nil || preflight.ApprovalsReviewer != reviewer || preflight.CWD != "/tmp/project" {
					t.Fatalf("preflight=%+v error=%v, want the %s reviewer accepted", preflight, err, reviewer)
				}
				snapshot.Requirements = map[string]json.RawMessage{"requirements": json.RawMessage(`{"allowedApprovalsReviewers":["other"]}`)}
				if preflight, err := snapshot.validateConversion("/tmp/project", policy, effective); err == nil {
					t.Fatalf("preflight=%+v, want the reviewer requirement enforced", preflight)
				}
			})
		}
	}
}

func TestCodexLifecycleArmRefusesStaleState(t *testing.T) {
	armed := &codexLifecycle{}
	if err := armed.Arm(); err != nil {
		t.Fatalf("first arm error=%v", err)
	}
	if err := armed.Arm(); err == nil {
		t.Fatal("an already armed verifier was armed again")
	}
	started := &codexLifecycle{started: map[string]codexThreadIdentity{"thread-new": {ID: "thread-new"}}}
	if err := started.Arm(); err == nil {
		t.Fatal("a verifier that already saw a started thread was armed")
	}
}

func TestCodexLifecycleConfirmIdentityRefusesBeforeAnyRequest(t *testing.T) {
	var requests atomic.Int32
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		if !codexLifecycleHandshake(ctx, conn) {
			return
		}
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			requests.Add(1)
			var request rpcTestRequest
			if json.Unmarshal(data, &request) == nil {
				send(t, ctx, conn, `{"id":`+jsonNumber(request.ID)+`,"error":{"code":-32600,"message":"unexpected"}}`)
			}
		}
	})
	for name, arm := range map[string]bool{"unarmed": false, "armed": true} {
		t.Run(name, func(t *testing.T) {
			lifecycle, err := newCodexLifecycle(context.Background(), socket)
			if err != nil {
				t.Fatal(err)
			}
			defer lifecycle.Close()
			threadID := "thread-new"
			if arm {
				if err := lifecycle.Arm(); err != nil {
					t.Fatal(err)
				}
				threadID = ""
			}
			if _, err := lifecycle.ConfirmIdentity(context.Background(), ManagedNew, "", threadID); !errors.Is(err, ErrManagedStale) {
				t.Fatalf("error=%v, want ErrManagedStale", err)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("%d requests were sent for an unarmed verifier or an invalid thread ID", requests.Load())
	}
}

func TestCodexLifecycleConfirmIdentityPollsAtTheDocumentedInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var arrivals []time.Time
	socket := unixServer(t, func(serverCtx context.Context, conn *websocket.Conn) {
		lifecycleInitialize(t, serverCtx, conn)
		for {
			_, data, err := conn.Read(serverCtx)
			if err != nil {
				return
			}
			var request rpcTestRequest
			if json.Unmarshal(data, &request) != nil || request.Method != "thread/read" {
				t.Errorf("request=%s, want thread/read", data)
				return
			}
			mu.Lock()
			arrivals = append(arrivals, time.Now())
			polls := len(arrivals)
			mu.Unlock()
			sendRPCResult(t, serverCtx, conn, request.ID, `{"thread":{"id":"thread-new","cwd":"/tmp/project","modelProvider":"openai"}}`)
			if polls == 3 {
				cancel()
			}
		}
	})
	lifecycle, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Close()
	if err := lifecycle.Arm(); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.ConfirmIdentity(ctx, ManagedNew, "", "thread-new"); err == nil {
		t.Fatal("a thread without a start notice was confirmed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(arrivals) != 3 {
		t.Fatalf("polls=%d, want 3", len(arrivals))
	}
	for poll := 1; poll < len(arrivals); poll++ {
		if gap := arrivals[poll].Sub(arrivals[poll-1]); gap < 25*time.Millisecond {
			t.Fatalf("poll %d arrived %v after the previous one, want the 25ms poll interval", poll+1, gap)
		}
	}
}

func TestCodexLifecycleReadThreadRejectsAnotherThread(t *testing.T) {
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		lifecycleInitialize(t, ctx, conn)
		request := readRPCRequest(t, ctx, conn, "thread/read")
		sendRPCResult(t, ctx, conn, request.ID, `{"thread":{"id":"thread-other","cwd":"/tmp/project","modelProvider":"openai"}}`)
	})
	lifecycle, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Close()
	if identity, err := lifecycle.ReadThread(context.Background(), "thread-requested"); !errors.Is(err, ErrManagedStale) || identity != (codexThreadIdentity{}) {
		t.Fatalf("identity=%+v error=%v, want ErrManagedStale", identity, err)
	}
}

func TestCodexLifecycleApplyObservedReasoningRefusesBeforeConnecting(t *testing.T) {
	expected := codexThreadIdentity{ID: "thread-original", CWD: "/tmp/project", ModelProvider: "openai", Model: "gpt-5.6-sol"}
	observed := ManagedCodexPolicy{Model: expected.Model, ReasoningEffort: "medium"}
	for name, change := range map[string]func(*codexThreadIdentity, *ManagedCodexPolicy){
		"stored effort":      func(identity *codexThreadIdentity, _ *ManagedCodexPolicy) { identity.ReasoningEffort = "medium" },
		"observed model":     func(_ *codexThreadIdentity, policy *ManagedCodexPolicy) { policy.Model = "other" },
		"unsupported effort": func(_ *codexThreadIdentity, policy *ManagedCodexPolicy) { policy.ReasoningEffort = "invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			identity, policy := expected, observed
			change(&identity, &policy)
			safeNoDispatch, err := (&codexLifecycle{}).ApplyObservedReasoning(context.Background(), identity, policy)
			if safeNoDispatch || !errors.Is(err, ErrManagedPolicyDrift) {
				t.Fatalf("safeNoDispatch=%t error=%v, want ErrManagedPolicyDrift before any connection", safeNoDispatch, err)
			}
		})
	}
}

func TestCodexLifecycleApplyObservedReasoningReportsFailedUnsubscribe(t *testing.T) {
	for _, test := range []struct {
		name         string
		unsubscribe  string
		wantProtocol bool
	}{
		{"unexpected status", `{"status":"still-subscribed"}`, true},
		{"lost connection", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var connections atomic.Int32
			socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
				index := connections.Add(1)
				if index == 1 {
					if codexLifecycleHandshake(ctx, conn) {
						_, _, _ = conn.Read(ctx)
					}
					return
				}
				lifecycleInitialize(t, ctx, conn)
				resume := readRPCRequest(t, ctx, conn, "thread/resume")
				sendRPCResult(t, ctx, conn, resume.ID, `{"thread":{"id":"thread-original","cwd":"/tmp/project","modelProvider":"openai"},"model":"gpt-5.6-sol","modelProvider":"openai","cwd":"/tmp/project","reasoningEffort":null}`)
				update := readRPCRequest(t, ctx, conn, "thread/settings/update")
				sendRPCResult(t, ctx, conn, update.ID, `{}`)
				unsubscribe := readRPCRequest(t, ctx, conn, "thread/unsubscribe")
				if test.unsubscribe != "" {
					sendRPCResult(t, ctx, conn, unsubscribe.ID, test.unsubscribe)
				}
			})
			lifecycle, err := newCodexLifecycle(context.Background(), socket)
			if err != nil {
				t.Fatal(err)
			}
			defer lifecycle.Close()
			expected := codexThreadIdentity{ID: "thread-original", CWD: "/tmp/project", ModelProvider: "openai", Model: "gpt-5.6-sol"}
			safeNoDispatch, err := lifecycle.ApplyObservedReasoning(context.Background(), expected, ManagedCodexPolicy{Model: expected.Model, ReasoningEffort: "medium"})
			if safeNoDispatch || err == nil || errors.Is(err, errCodexProtocol) != test.wantProtocol {
				t.Fatalf("safeNoDispatch=%t error=%v, want a failed unsubscribe (protocol error %t)", safeNoDispatch, err, test.wantProtocol)
			}
		})
	}
}

func TestCodexLifecycleApplyObservedReasoningChecksEveryResumeField(t *testing.T) {
	resume := func(threadID, threadCWD, threadProvider, threadExtra, model, provider, cwd, effort string) string {
		return fmt.Sprintf(`{"thread":{"id":%q,"cwd":%q,"modelProvider":%q%s},"model":%q,"modelProvider":%q,"cwd":%q,"reasoningEffort":%s}`,
			threadID, threadCWD, threadProvider, threadExtra, model, provider, cwd, effort)
	}
	const id, cwd, provider, model = "thread-original", "/tmp/project", "openai", "gpt-5.6-sol"
	for _, test := range []struct {
		name     string
		response string
		accepted bool
	}{
		{"equivalent descriptions", resume(id, cwd, provider, `,"model":"`+model+`"`, model, provider, cwd, `""`), true},
		{"invalid thread identity", resume(id, "relative", provider, "", model, provider, cwd, "null"), false},
		{"thread id", resume("thread-other", cwd, provider, "", model, provider, cwd, "null"), false},
		{"thread directory", resume(id, "/tmp/other", provider, "", model, provider, cwd, "null"), false},
		{"thread fork parent", resume(id, cwd, provider, `,"forkedFromId":"thread-parent"`, model, provider, cwd, "null"), false},
		{"thread provider", resume(id, cwd, "other", "", model, provider, cwd, "null"), false},
		{"thread model", resume(id, cwd, provider, `,"model":"different"`, model, provider, cwd, "null"), false},
		{"thread effort", resume(id, cwd, provider, `,"reasoningEffort":"medium"`, model, provider, cwd, "null"), false},
		{"response model", resume(id, cwd, provider, "", "different", provider, cwd, "null"), false},
		{"response provider", resume(id, cwd, provider, "", model, "other", cwd, "null"), false},
		{"response directory", resume(id, cwd, provider, "", model, provider, "/tmp/other", "null"), false},
		{"response effort", resume(id, cwd, provider, "", model, provider, cwd, `"medium"`), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var connections atomic.Int32
			socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
				index := connections.Add(1)
				if index == 1 {
					if codexLifecycleHandshake(ctx, conn) {
						_, _, _ = conn.Read(ctx)
					}
					return
				}
				lifecycleInitialize(t, ctx, conn)
				request := readRPCRequest(t, ctx, conn, "thread/resume")
				sendRPCResult(t, ctx, conn, request.ID, test.response)
				if test.accepted {
					update := readRPCRequest(t, ctx, conn, "thread/settings/update")
					sendRPCResult(t, ctx, conn, update.ID, `{}`)
				}
				unsubscribe := readRPCRequest(t, ctx, conn, "thread/unsubscribe")
				sendRPCResult(t, ctx, conn, unsubscribe.ID, `{"status":"unsubscribed"}`)
			})
			lifecycle, err := newCodexLifecycle(context.Background(), socket)
			if err != nil {
				t.Fatal(err)
			}
			defer lifecycle.Close()
			expected := codexThreadIdentity{ID: id, CWD: cwd, ModelProvider: provider, Model: model}
			safeNoDispatch, err := lifecycle.ApplyObservedReasoning(context.Background(), expected, ManagedCodexPolicy{Model: model, ReasoningEffort: "medium"})
			if safeNoDispatch || test.accepted && err != nil || !test.accepted && !errors.Is(err, ErrManagedPolicyDrift) {
				t.Fatalf("safeNoDispatch=%t error=%v, want accepted=%t", safeNoDispatch, err, test.accepted)
			}
		})
	}
}

func TestCodexLifecycleEffectiveResponseRejectsEachInconsistentOrUnsafeField(t *testing.T) {
	reasoning := "medium"
	base := codexEffectiveResponse{
		Thread: codexThread{ID: managedOriginalID, CWD: "/tmp/project", ModelProvider: "openai"},
		Model:  "gpt-5.6-sol", ModelProvider: "openai", CWD: "/tmp/project",
		ApprovalPolicy: json.RawMessage(`"never"`), ApprovalsReviewer: "user", ReasoningEffort: &reasoning,
		MultiAgentMode: "explicitRequestOnly", InstructionSources: []string{"/tmp/AGENTS.md"},
	}
	base.Sandbox.Type = "dangerFullAccess"
	if _, err := base.effective(managedOriginalID); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	for name, change := range map[string]func(*codexEffectiveResponse){
		"invalid thread identity":      func(response *codexEffectiveResponse) { response.Thread.CWD = "relative" },
		"another thread":               func(response *codexEffectiveResponse) { response.Thread.ID = "thread-other" },
		"another directory":            func(response *codexEffectiveResponse) { response.CWD = "/tmp/other" },
		"missing model":                func(response *codexEffectiveResponse) { response.Model = "" },
		"missing provider":             func(response *codexEffectiveResponse) { response.ModelProvider = "" },
		"empty approval policy":        func(response *codexEffectiveResponse) { response.ApprovalPolicy = json.RawMessage(`""`) },
		"undecodable approval policy":  func(response *codexEffectiveResponse) { response.ApprovalPolicy = json.RawMessage(`{"granular":{}}`) },
		"instruction source line feed": func(response *codexEffectiveResponse) { response.InstructionSources = []string{"/tmp/AGENTS\n.md"} },
		"empty instruction source":     func(response *codexEffectiveResponse) { response.InstructionSources = []string{""} },
	} {
		t.Run(name, func(t *testing.T) {
			response := base
			change(&response)
			if _, err := response.effective(managedOriginalID); !errors.Is(err, errCodexProtocol) {
				t.Fatalf("error=%v, want errCodexProtocol", err)
			}
		})
	}
}

func TestCodexLifecycleParseConfigRejectsEmptyOrNonStringValues(t *testing.T) {
	for name, raw := range map[string]string{"empty string": `""`, "number": `5`} {
		t.Run(name, func(t *testing.T) {
			config := map[string]json.RawMessage{"approvals_reviewer": json.RawMessage(`"user"`), "model": json.RawMessage(raw)}
			if _, err := parseCodexConfig(config, "/tmp/project", managedPolicy()); err == nil {
				t.Fatalf("model %s accepted", raw)
			}
		})
	}
}
