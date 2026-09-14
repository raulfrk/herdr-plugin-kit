package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type rpcTestRequest struct {
	ID     int                        `json:"id"`
	Method string                     `json:"method"`
	Params map[string]json.RawMessage `json:"params"`
}

func readRPCRequest(t *testing.T, ctx context.Context, conn *websocket.Conn, method string) rpcTestRequest {
	t.Helper()
	kind, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read %s: %v", method, err)
	}
	var request rpcTestRequest
	if kind != websocket.MessageText || json.Unmarshal(data, &request) != nil || request.Method != method || request.ID <= 0 {
		t.Fatalf("request=%s, want method %s", data, method)
	}
	return request
}

func sendRPCResult(t *testing.T, ctx context.Context, conn *websocket.Conn, id int, result string) {
	t.Helper()
	send(t, ctx, conn, `{"id":`+jsonNumber(id)+`,"result":`+result+`}`)
}

func jsonNumber(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func lifecycleInitialize(t *testing.T, ctx context.Context, conn *websocket.Conn) {
	t.Helper()
	request := readRPCRequest(t, ctx, conn, "initialize")
	sendRPCResult(t, ctx, conn, request.ID, `{"userAgent":"codex-cli/0.154.0"}`)
	kind, data, err := conn.Read(ctx)
	if err != nil || kind != websocket.MessageText || string(data) != `{"method":"initialized"}` {
		t.Fatalf("initialized=%s error=%v", data, err)
	}
}

func lifecycleConfig() string {
	return `{"config":{"model":"gpt-5.6-sol","model_reasoning_effort":"medium","approval_policy":"never","sandbox_mode":"danger-full-access","approvals_reviewer":"user","default_permissions":null,"service_tier":null},"origins":{}}`
}

func TestParseCodexConfigAllowsUnsetOverriddenThreadDefaults(t *testing.T) {
	config := map[string]json.RawMessage{
		"approvals_reviewer":  json.RawMessage(`"user"`),
		"default_permissions": json.RawMessage(`":danger-full-access"`),
	}
	preflight, err := parseCodexConfig(config, "/tmp/project", managedPolicy())
	if err != nil || !matchesRequestedPolicy(preflight.EffectiveCodexPolicy, managedPolicy()) || preflight.ApprovalsReviewer != "user" || preflight.PermissionProfile != "" || preflight.DefaultPermissionProfile != ":danger-full-access" {
		t.Fatalf("preflight=%+v error=%v", preflight, err)
	}
}

func TestValidateRequirementsResponseRejectsEachRelevantPolicyDrift(t *testing.T) {
	preflight := managedPreflight("/tmp/project")
	preflight.ApprovalsReviewer = "user"
	preflight.DefaultPermissionProfile = "team"
	preflight.ServiceTier = "default"
	tests := []struct {
		name, requirements string
		wantErr            bool
	}{
		{"none", `null`, false},
		{"matching", `{"allowedApprovalPolicies":["never"],"allowedSandboxModes":["danger-full-access"],"allowedApprovalsReviewers":["user"],"allowedPermissionProfiles":{"team":true},"defaultPermissions":"team","models":{"newThread":{"model":"gpt-5.6-sol","modelReasoningEffort":"medium","serviceTier":"default"}}}`, false},
		{"approval", `{"allowedApprovalPolicies":["on-request"]}`, true},
		{"sandbox", `{"allowedSandboxModes":["read-only"]}`, true},
		{"reviewer", `{"allowedApprovalsReviewers":["auto_review"]}`, true},
		{"permission", `{"allowedPermissionProfiles":{"other":true}}`, true},
		{"default permission", `{"defaultPermissions":"other"}`, true},
		{"model", `{"models":{"newThread":{"model":"other"}}}`, true},
		{"auto review", `{"autoReview":{"requiredOnModels":["gpt-5.6-sol"]}}`, true},
		{"unknown", `{"futureConstraint":true}`, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := map[string]json.RawMessage{"requirements": json.RawMessage(test.requirements)}
			err := validateRequirementsResponse(response, managedPolicy(), preflight)
			if (err != nil) != test.wantErr {
				t.Fatalf("error=%v wantErr=%t", err, test.wantErr)
			}
		})
	}
}

func TestCodexEffectiveResponsePreservesPermissionProfileProvenance(t *testing.T) {
	extends := "base"
	reasoning := "medium"
	serviceTier := "default"
	response := codexEffectiveResponse{
		Thread: codexThread{ID: managedOriginalID, CWD: "/tmp/project", ModelProvider: "openai"},
		Model:  "gpt-5.6-sol", ModelProvider: "openai", ServiceTier: &serviceTier, CWD: "/tmp/project",
		ApprovalPolicy: json.RawMessage(`"never"`), ApprovalsReviewer: "user", ReasoningEffort: &reasoning,
		RuntimeWorkspaceRoots: []string{"/tmp/project"},
	}
	response.Sandbox.Type = "dangerFullAccess"
	response.ActivePermissionProfile = &struct {
		ID      string  `json:"id"`
		Extends *string `json:"extends"`
	}{ID: "team", Extends: &extends}
	effective, err := response.effective(managedOriginalID)
	if err != nil || effective.PermissionProfile != "team" || effective.PermissionProfileExtends != "base" || effective.SandboxMode != "danger-full-access" {
		t.Fatalf("effective=%+v error=%v", effective, err)
	}
	response.RuntimeWorkspaceRoots = []string{"relative"}
	if _, err := response.effective(managedOriginalID); err == nil {
		t.Fatal("relative runtime root accepted")
	}
}

func TestCodexLifecyclePreflightAndFreshStartedIdentity(t *testing.T) {
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		lifecycleInitialize(t, ctx, conn)
		config := readRPCRequest(t, ctx, conn, "config/read")
		if len(config.Params) != 2 || string(config.Params["cwd"]) != `"/tmp/project"` || string(config.Params["includeLayers"]) != "false" {
			t.Fatalf("config params=%s", mustJSON(config.Params))
		}
		sendRPCResult(t, ctx, conn, config.ID, lifecycleConfig())
		requirements := readRPCRequest(t, ctx, conn, "configRequirements/read")
		if len(requirements.Params) != 0 {
			t.Fatalf("requirements params=%s", mustJSON(requirements.Params))
		}
		sendRPCResult(t, ctx, conn, requirements.ID, `{"requirements":null}`)
		read := readRPCRequest(t, ctx, conn, "thread/read")
		send(t, ctx, conn, `{"method":"thread/started","params":{"thread":{"id":"thread-new","cwd":"/tmp/project","modelProvider":"openai"}}}`)
		sendRPCResult(t, ctx, conn, read.ID, `{"thread":{"id":"thread-new","cwd":"/tmp/project","modelProvider":"openai"}}`)
	})
	verifier, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	preflight, err := verifier.Preflight(context.Background(), "/tmp/project", managedPolicy())
	if err != nil || preflight.ApprovalsReviewer != "user" {
		t.Fatalf("preflight=%+v error=%v", preflight, err)
	}
	if err := verifier.Arm(); err != nil {
		t.Fatal(err)
	}
	identity, err := verifier.ConfirmIdentity(context.Background(), ManagedNew, "", "thread-new")
	if err != nil || identity.ID != "thread-new" || identity.CWD != "/tmp/project" || identity.ModelProvider != "openai" {
		t.Fatalf("identity=%+v error=%v", identity, err)
	}
}

func TestCodexLifecycleRejectsEmptyInitializeIdentity(t *testing.T) {
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		request := readRPCRequest(t, ctx, conn, "initialize")
		sendRPCResult(t, ctx, conn, request.ID, `{"userAgent":""}`)
	})
	if lifecycle, err := newCodexLifecycle(context.Background(), socket); err == nil || lifecycle != nil {
		t.Fatalf("lifecycle=%v error=%v", lifecycle, err)
	}
}

func TestCodexLifecycleStatusNotificationRejectsEachMalformedField(t *testing.T) {
	c := &codexLifecycle{}
	for _, params := range []string{
		`{`, `{"threadId":"","status":{"type":"idle"}}`, `{"threadId":"thread-ok"}`,
	} {
		if err := c.observe("thread/status/changed", json.RawMessage(params)); !errors.Is(err, errCodexProtocol) {
			t.Fatalf("params=%s error=%v", params, err)
		}
	}
	if err := c.observe("thread/status/changed", json.RawMessage(`{"threadId":"thread-ok","status":{"type":"idle"}}`)); err != nil {
		t.Fatalf("valid status error=%v", err)
	}
}

func TestCodexLifecycleForkRejectsEachIdentityDrift(t *testing.T) {
	tests := []struct {
		name, source, started, read string
	}{
		{"started metadata", "thread-source", `{"id":"thread-child","cwd":"/tmp/other","forkedFromId":"thread-source","modelProvider":"openai"}`, `{"id":"thread-child","cwd":"/tmp/project","forkedFromId":"thread-source","modelProvider":"openai"}`},
		{"same identity", "thread-child", `{"id":"thread-child","cwd":"/tmp/project","forkedFromId":"thread-child","modelProvider":"openai"}`, `{"id":"thread-child","cwd":"/tmp/project","forkedFromId":"thread-child","modelProvider":"openai"}`},
		{"wrong parent", "thread-source", `{"id":"thread-child","cwd":"/tmp/project","forkedFromId":"thread-other","modelProvider":"openai"}`, `{"id":"thread-child","cwd":"/tmp/project","forkedFromId":"thread-other","modelProvider":"openai"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
				lifecycleInitialize(t, ctx, conn)
				read := readRPCRequest(t, ctx, conn, "thread/read")
				send(t, ctx, conn, `{"method":"thread/started","params":{"thread":`+test.started+`}}`)
				sendRPCResult(t, ctx, conn, read.ID, `{"thread":`+test.read+`}`)
			})
			verifier, err := newCodexLifecycle(context.Background(), socket)
			if err != nil {
				t.Fatal(err)
			}
			defer verifier.Close()
			if err := verifier.Arm(); err != nil {
				t.Fatal(err)
			}
			if _, err := verifier.ConfirmIdentity(context.Background(), ManagedFork, test.source, "thread-child"); !errors.Is(err, ErrManagedStale) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCodexLifecycleLateLoadedStateCannotPass(t *testing.T) {
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		lifecycleInitialize(t, ctx, conn)
		config := readRPCRequest(t, ctx, conn, "config/read")
		sendRPCResult(t, ctx, conn, config.ID, lifecycleConfig())
		requirements := readRPCRequest(t, ctx, conn, "configRequirements/read")
		sendRPCResult(t, ctx, conn, requirements.ID, `{"requirements":null}`)
		for {
			kind, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var read rpcTestRequest
			if kind != websocket.MessageText || json.Unmarshal(data, &read) != nil || read.Method != "thread/read" {
				t.Errorf("read request=%s", data)
				return
			}
			sendRPCResult(t, ctx, conn, read.ID, `{"thread":{"id":"thread-stale","cwd":"/tmp/project","modelProvider":"openai"}}`)
		}
	})
	verifier, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	if _, err := verifier.Preflight(context.Background(), "/tmp/project", managedPolicy()); err != nil {
		t.Fatal(err)
	}
	if err := verifier.Arm(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, err := verifier.ConfirmIdentity(ctx, ManagedNew, "", "thread-stale"); err == nil {
		t.Fatal("late loaded state passed without thread/started")
	}
}

func TestCodexLifecycleIgnoresUnrelatedStartAndRequiresMatchingTargetNotification(t *testing.T) {
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		lifecycleInitialize(t, ctx, conn)
		config := readRPCRequest(t, ctx, conn, "config/read")
		sendRPCResult(t, ctx, conn, config.ID, lifecycleConfig())
		requirements := readRPCRequest(t, ctx, conn, "configRequirements/read")
		sendRPCResult(t, ctx, conn, requirements.ID, `{"requirements":null}`)
		read := readRPCRequest(t, ctx, conn, "thread/read")
		send(t, ctx, conn, `{"method":"thread/started","params":{"thread":{"id":"thread-unrelated","cwd":"/tmp/other","modelProvider":"openai"}}}`)
		sendRPCResult(t, ctx, conn, read.ID, `{"thread":{"id":"thread-target","cwd":"/tmp/project","modelProvider":"openai"}}`)
		read = readRPCRequest(t, ctx, conn, "thread/read")
		send(t, ctx, conn, `{"method":"thread/started","params":{"thread":{"id":"thread-target","cwd":"/tmp/project","modelProvider":"openai"}}}`)
		sendRPCResult(t, ctx, conn, read.ID, `{"thread":{"id":"thread-target","cwd":"/tmp/project","modelProvider":"openai"}}`)
	})
	verifier, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	if _, err := verifier.Preflight(context.Background(), "/tmp/project", managedPolicy()); err != nil {
		t.Fatal(err)
	}
	if err := verifier.Arm(); err != nil {
		t.Fatal(err)
	}
	identity, err := verifier.ConfirmIdentity(context.Background(), ManagedNew, "", "thread-target")
	if err != nil || identity.ID != "thread-target" {
		t.Fatalf("identity=%+v error=%v", identity, err)
	}
}

func TestCodexLifecycleRejectsStartedMetadataDrift(t *testing.T) {
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		lifecycleInitialize(t, ctx, conn)
		config := readRPCRequest(t, ctx, conn, "config/read")
		sendRPCResult(t, ctx, conn, config.ID, lifecycleConfig())
		requirements := readRPCRequest(t, ctx, conn, "configRequirements/read")
		sendRPCResult(t, ctx, conn, requirements.ID, `{"requirements":null}`)
		read := readRPCRequest(t, ctx, conn, "thread/read")
		send(t, ctx, conn, `{"method":"thread/started","params":{"thread":{"id":"thread-target","cwd":"/tmp/wrong","modelProvider":"openai"}}}`)
		sendRPCResult(t, ctx, conn, read.ID, `{"thread":{"id":"thread-target","cwd":"/tmp/project","modelProvider":"openai"}}`)
	})
	verifier, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	if _, err := verifier.Preflight(context.Background(), "/tmp/project", managedPolicy()); err != nil {
		t.Fatal(err)
	}
	if err := verifier.Arm(); err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.ConfirmIdentity(context.Background(), ManagedNew, "", "thread-target"); !errors.Is(err, ErrManagedStale) {
		t.Fatalf("metadata drift error=%v", err)
	}
}

func TestCodexLifecycleResumeRequiresExactPaneDiscoveredID(t *testing.T) {
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		lifecycleInitialize(t, ctx, conn)
		config := readRPCRequest(t, ctx, conn, "config/read")
		sendRPCResult(t, ctx, conn, config.ID, lifecycleConfig())
		requirements := readRPCRequest(t, ctx, conn, "configRequirements/read")
		sendRPCResult(t, ctx, conn, requirements.ID, `{"requirements":null}`)
		for {
			kind, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var read rpcTestRequest
			if kind != websocket.MessageText || json.Unmarshal(data, &read) != nil || read.Method != "thread/read" {
				t.Errorf("read request=%s", data)
				return
			}
			sendRPCResult(t, ctx, conn, read.ID, `{"thread":{"id":"thread-original","cwd":"/tmp/project","modelProvider":"openai"}}`)
		}
	})
	verifier, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	if _, err := verifier.Preflight(context.Background(), "/tmp/project", managedPolicy()); err != nil {
		t.Fatal(err)
	}
	if err := verifier.Arm(); err != nil {
		t.Fatal(err)
	}
	identity, err := verifier.ConfirmIdentity(context.Background(), ManagedResume, "thread-original", "thread-original")
	if err != nil || identity.ID != "thread-original" {
		t.Fatalf("identity=%+v error=%v", identity, err)
	}
	if _, err := verifier.ConfirmIdentity(context.Background(), ManagedResume, "thread-original", "thread-other"); !errors.Is(err, ErrManagedStale) {
		t.Fatalf("wrong pane-discovered ID error = %v", err)
	}
}

func TestCodexLifecycleEffectiveReadSendsNoOverridesAndUnsubscribes(t *testing.T) {
	var connections atomic.Int32
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		index := connections.Add(1)
		lifecycleInitialize(t, ctx, conn)
		if index == 1 {
			_, _, _ = conn.Read(ctx)
			return
		}
		resume := readRPCRequest(t, ctx, conn, "thread/resume")
		if len(resume.Params) != 2 || string(resume.Params["threadId"]) != `"thread-original"` || string(resume.Params["excludeTurns"]) != "true" {
			t.Fatalf("resume params=%s", mustJSON(resume.Params))
		}
		response := `{"thread":{"id":"thread-original","cwd":"/tmp/project","modelProvider":"openai"},"model":"gpt-5.6-sol","modelProvider":"openai","serviceTier":null,"cwd":"/tmp/project","approvalPolicy":"never","approvalsReviewer":"user","sandbox":{"type":"dangerFullAccess"},"activePermissionProfile":null,"reasoningEffort":"medium","runtimeWorkspaceRoots":["/tmp/project"]}`
		sendRPCResult(t, ctx, conn, resume.ID, response)
		unsubscribe := readRPCRequest(t, ctx, conn, "thread/unsubscribe")
		if len(unsubscribe.Params) != 1 || string(unsubscribe.Params["threadId"]) != `"thread-original"` {
			t.Fatalf("unsubscribe params=%s", mustJSON(unsubscribe.Params))
		}
		sendRPCResult(t, ctx, conn, unsubscribe.ID, `{"status":"unsubscribed"}`)
	})
	verifier, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	effective, err := verifier.ReadEffective(context.Background(), "thread-original")
	if err != nil || effective.ModelProvider != "openai" || !reflect.DeepEqual(effective.RuntimeWorkspaceRoots, []string{"/tmp/project"}) {
		t.Fatalf("effective=%+v error=%v", effective, err)
	}
}

func TestCodexLifecycleEffectiveReadRejectsWrongUnsubscribeStatus(t *testing.T) {
	var connections atomic.Int32
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		index := connections.Add(1)
		lifecycleInitialize(t, ctx, conn)
		if index == 1 {
			_, _, _ = conn.Read(ctx)
			return
		}
		resume := readRPCRequest(t, ctx, conn, "thread/resume")
		response := `{"thread":{"id":"thread-original","cwd":"/tmp/project","modelProvider":"openai"},"model":"gpt-5.6-sol","modelProvider":"openai","serviceTier":null,"cwd":"/tmp/project","approvalPolicy":"never","approvalsReviewer":"user","sandbox":{"type":"dangerFullAccess"},"activePermissionProfile":null,"reasoningEffort":"medium","runtimeWorkspaceRoots":["/tmp/project"]}`
		sendRPCResult(t, ctx, conn, resume.ID, response)
		unsubscribe := readRPCRequest(t, ctx, conn, "thread/unsubscribe")
		sendRPCResult(t, ctx, conn, unsubscribe.ID, `{"status":"still-subscribed"}`)
	})
	verifier, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	if _, err := verifier.ReadEffective(context.Background(), "thread-original"); !errors.Is(err, errCodexProtocol) {
		t.Fatalf("error=%v", err)
	}
}

func TestRequiredPolicyComparisonCoversEqualAndRootDrift(t *testing.T) {
	policy := managedPreflight("/tmp/project").EffectiveCodexPolicy
	policy.RuntimeWorkspaceRoots = []string{"/tmp/project"}
	if !sameRequiredPolicy(policy, policy) {
		t.Fatal("identical policy rejected")
	}
	drifted := policy
	drifted.RuntimeWorkspaceRoots = []string{"/tmp/other"}
	if drift := requiredPolicyDrift(drifted, policy); drift != "runtime_workspace_roots" {
		t.Fatalf("drift=%q", drift)
	}
}

func mustJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
