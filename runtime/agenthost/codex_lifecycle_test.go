package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func codexListThreadJSON(id string) string {
	return fmt.Sprintf(`{"id":%q,"cwd":"/tmp/project","modelProvider":"openai","model":"gpt-5.6-sol","reasoningEffort":"medium"}`, id)
}

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

func TestCodexLifecycleResponseBudgetIsPerCall(t *testing.T) {
	const calls = 70
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		lifecycleInitialize(t, ctx, conn)
		for i := 0; i < calls; i++ {
			request := readRPCRequest(t, ctx, conn, "test/echo")
			sendRPCResult(t, ctx, conn, request.ID, fmt.Sprintf(`{"value":%d}`, i))
		}
	})
	lifecycle, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Close()
	for i := 0; i < calls; i++ {
		var response struct {
			Value int `json:"value"`
		}
		if err := lifecycle.call(context.Background(), "test/echo", nil, &response); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
		if response.Value != i {
			t.Fatalf("call %d value=%d, want %d", i+1, response.Value, i)
		}
	}
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

func TestValidateRequirementsResponsePreservesOptionalRequirementBoundaries(t *testing.T) {
	preflight := managedPreflight("/tmp/project")
	preflight.ApprovalsReviewer = "user"
	preflight.DefaultPermissionProfile = "team"

	if err := validateRequirementsResponse(map[string]json.RawMessage{
		"requirements": json.RawMessage(`{"allowedApprovalPolicies":["never"]}`),
	}, managedPolicy(), preflight); err != nil {
		t.Fatalf("optional profile and model requirements error=%v", err)
	}

	emptyProfile := preflight
	emptyProfile.DefaultPermissionProfile = ""
	if err := validateRequirementsResponse(map[string]json.RawMessage{
		"requirements": json.RawMessage(`{"allowedPermissionProfiles":{"":true}}`),
	}, managedPolicy(), emptyProfile); err == nil {
		t.Fatal("empty effective permission profile was allowed")
	}
	if err := validateRequirementsResponse(map[string]json.RawMessage{
		"requirements": json.RawMessage(`{"defaultPermissions":""}`),
	}, managedPolicy(), emptyProfile); err == nil {
		t.Fatal("empty required default permission was allowed")
	}
	if err := validateRequirementsResponse(map[string]json.RawMessage{
		"requirements": json.RawMessage(`{"autoReview":[]}`),
	}, managedPolicy(), preflight); err == nil {
		t.Fatal("malformed auto-review requirement was allowed")
	}
	if err := validateRequirementsResponse(map[string]json.RawMessage{
		"requirements": json.RawMessage(`{"autoReview":{"requiredOnModels":["other-model"]}}`),
	}, managedPolicy(), preflight); err != nil {
		t.Fatalf("unrelated auto-review requirement error=%v", err)
	}
}

func TestOptionalCodexRequirementValuesRemainOptional(t *testing.T) {
	if err := requireAllowedString(nil, "never"); err != nil {
		t.Fatalf("absent allowed-values requirement error=%v", err)
	}
	preflight := managedPreflight("/tmp/project")
	if err := validateModelRequirements(json.RawMessage(`{}`), managedPolicy(), preflight); err != nil {
		t.Fatalf("absent new-thread requirement error=%v", err)
	}
	if err := validateModelRequirements(json.RawMessage(`{"newThread":{}}`), managedPolicy(), preflight); err != nil {
		t.Fatalf("absent new-thread field error=%v", err)
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
		RuntimeWorkspaceRoots: []string{"/tmp/project"}, InstructionSources: []string{"/tmp/AGENTS.md"}, MultiAgentMode: "explicitRequestOnly",
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
	response.ModelProvider = "other"
	if _, err := response.effective(managedOriginalID); err == nil {
		t.Fatal("model-provider mismatch accepted")
	}
	response.ModelProvider = "openai"
	response.RuntimeWorkspaceRoots = []string{"relative"}
	if _, err := response.effective(managedOriginalID); err == nil {
		t.Fatal("relative runtime root accepted")
	}
}

func TestCodexEffectiveResponseRejectsInvalidDetailedSandboxPolicy(t *testing.T) {
	reasoning := "medium"
	base := codexEffectiveResponse{
		Thread: codexThread{ID: managedOriginalID, CWD: "/tmp/project", ModelProvider: "openai"},
		Model:  "gpt-5.6-sol", ModelProvider: "openai", CWD: "/tmp/project",
		ApprovalPolicy: json.RawMessage(`"never"`), ApprovalsReviewer: "user", ReasoningEffort: &reasoning,
		MultiAgentMode: "explicitRequestOnly",
	}
	for name, mutate := range map[string]func(*codexEffectiveResponse){
		"relative workspace root": func(response *codexEffectiveResponse) {
			response.Sandbox.Type = "workspaceWrite"
			response.Sandbox.WritableRoots = []string{"relative"}
		},
		"read-only workspace field": func(response *codexEffectiveResponse) {
			response.Sandbox.Type = "readOnly"
			response.Sandbox.ExcludeSlashTmp = true
		},
		"danger network field": func(response *codexEffectiveResponse) {
			response.Sandbox.Type = "dangerFullAccess"
			response.Sandbox.NetworkAccess = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			response := base
			mutate(&response)
			if _, err := response.effective(managedOriginalID); err == nil {
				t.Fatal("invalid detailed sandbox accepted")
			}
		})
	}
}

func TestCodexThreadIdentityIncludesModelAndReasoning(t *testing.T) {
	model, reasoning := "gpt-5.6-sol", "medium"
	identity, err := (codexThread{ID: managedOriginalID, CWD: "/tmp/project", ModelProvider: "openai", Model: &model, ReasoningEffort: &reasoning}).identity()
	if err != nil || identity.Model != model || identity.ReasoningEffort != reasoning {
		t.Fatalf("identity=%+v error=%v", identity, err)
	}
}

func TestCodexLifecycleListCLIThreadsRejectsAmbiguousPagination(t *testing.T) {
	tests := []struct {
		name    string
		respond func(*testing.T, context.Context, *websocket.Conn)
	}{
		{
			name: "cross-page duplicate",
			respond: func(t *testing.T, ctx context.Context, conn *websocket.Conn) {
				first := readRPCRequest(t, ctx, conn, "thread/list")
				if string(first.Params["cwd"]) != `"/tmp/project"` || string(first.Params["limit"]) != "100" || string(first.Params["sourceKinds"]) != `["cli"]` || string(first.Params["useStateDbOnly"]) != "true" {
					t.Fatalf("first params=%s", mustJSON(first.Params))
				}
				sendRPCResult(t, ctx, conn, first.ID, `{"data":[`+codexListThreadJSON(managedNewID)+`],"nextCursor":"c1"}`)
				second := readRPCRequest(t, ctx, conn, "thread/list")
				if string(second.Params["cursor"]) != `"c1"` {
					t.Fatalf("second params=%s", mustJSON(second.Params))
				}
				sendRPCResult(t, ctx, conn, second.ID, `{"data":[`+codexListThreadJSON(managedNewID)+`],"nextCursor":null}`)
			},
		},
		{
			name: "repeated cursor",
			respond: func(t *testing.T, ctx context.Context, conn *websocket.Conn) {
				first := readRPCRequest(t, ctx, conn, "thread/list")
				sendRPCResult(t, ctx, conn, first.ID, `{"data":[],"nextCursor":"c1"}`)
				second := readRPCRequest(t, ctx, conn, "thread/list")
				sendRPCResult(t, ctx, conn, second.ID, `{"data":[],"nextCursor":"c2"}`)
				third := readRPCRequest(t, ctx, conn, "thread/list")
				sendRPCResult(t, ctx, conn, third.ID, `{"data":[],"nextCursor":"c1"}`)
			},
		},
		{
			name: "page limit exhausted",
			respond: func(t *testing.T, ctx context.Context, conn *websocket.Conn) {
				for page := 0; page < 10; page++ {
					request := readRPCRequest(t, ctx, conn, "thread/list")
					sendRPCResult(t, ctx, conn, request.ID, fmt.Sprintf(`{"data":[],"nextCursor":"c%d"}`, page))
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
				lifecycleInitialize(t, ctx, conn)
				test.respond(t, ctx, conn)
			})
			verifier, err := newCodexLifecycle(context.Background(), socket)
			if err != nil {
				t.Fatal(err)
			}
			defer verifier.Close()
			if _, err := verifier.ListCLIThreads(context.Background(), "/tmp/project"); !errors.Is(err, ErrManagedAmbiguous) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCodexLifecycleListCLIThreadsRejectsInvalidDirectoryBeforeRPC(t *testing.T) {
	client := &codexLifecycle{}
	for _, cwd := range []string{"", "relative", "/tmp/project/..", "/tmp/project\x00other"} {
		if _, err := client.ListCLIThreads(context.Background(), cwd); !errors.Is(err, ErrManagedStale) {
			t.Fatalf("cwd=%q error=%v", cwd, err)
		}
	}
}

func TestCodexLifecycleListThreadsKeepsStateFilterWithoutRestrictingSource(t *testing.T) {
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		lifecycleInitialize(t, ctx, conn)
		request := readRPCRequest(t, ctx, conn, "thread/list")
		if _, exists := request.Params["sourceKinds"]; exists || string(request.Params["useStateDbOnly"]) != "true" || string(request.Params["cwd"]) != `"/tmp/project"` {
			t.Fatalf("params=%s", mustJSON(request.Params))
		}
		sendRPCResult(t, ctx, conn, request.ID, `{"data":[`+codexListThreadJSON(managedNewID)+`],"nextCursor":null}`)
	})
	verifier, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	threads, err := verifier.ListThreads(context.Background(), "/tmp/project")
	if err != nil || len(threads) != 1 || threads[0].ID != managedNewID {
		t.Fatalf("threads=%+v error=%v", threads, err)
	}
}

func TestCodexLifecycleListCLIThreadsRejectsInvalidIdentityOrWrongDirectory(t *testing.T) {
	for name, thread := range map[string]string{
		"invalid identity": `{"id":"","cwd":"/tmp/project","modelProvider":"openai"}`,
		"wrong directory":  `{"id":"01a09f5c-37cc-77f1-9409-e9aa070ad8d0","cwd":"/tmp/other","modelProvider":"openai","model":"gpt-5.6-sol","reasoningEffort":"medium"}`,
	} {
		t.Run(name, func(t *testing.T) {
			socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
				lifecycleInitialize(t, ctx, conn)
				request := readRPCRequest(t, ctx, conn, "thread/list")
				sendRPCResult(t, ctx, conn, request.ID, `{"data":[`+thread+`],"nextCursor":null}`)
			})
			verifier, err := newCodexLifecycle(context.Background(), socket)
			if err != nil {
				t.Fatal(err)
			}
			defer verifier.Close()
			if _, err := verifier.ListCLIThreads(context.Background(), "/tmp/project"); !errors.Is(err, ErrManagedStale) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCodexLifecycleReadConstraintsPreservesExactValidatedResponses(t *testing.T) {
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
	})
	verifier, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	snapshot, err := verifier.ReadConstraints(context.Background(), "/tmp/project")
	if err != nil || string(snapshot.Config["model"]) != `"gpt-5.6-sol"` || string(snapshot.Requirements["requirements"]) != "null" {
		t.Fatalf("snapshot=%+v error=%v", snapshot, err)
	}
	if _, err := snapshot.validate("/tmp/project", managedPolicy()); err != nil {
		t.Fatal(err)
	}
	identical := snapshot
	if !snapshot.equal(identical) {
		t.Fatal("identical constraint snapshots differ")
	}
	reordered := codexConstraintSnapshot{Config: mapsCloneRaw(snapshot.Config), Requirements: mapsCloneRaw(snapshot.Requirements)}
	snapshot.Config["projects"] = json.RawMessage(`{"/tmp/b":{"trusted":false,"label":"b"},"/tmp/a":{"label":"a","trusted":true}}`)
	reordered.Config["projects"] = json.RawMessage(`{"/tmp/a":{"trusted":true,"label":"a"},"/tmp/b":{"label":"b","trusted":false}}`)
	if !snapshot.equal(reordered) || len(snapshot.driftFields(reordered)) != 0 {
		t.Fatal("JSON object member order changed constraint identity")
	}
	drifted := codexConstraintSnapshot{Config: mapsCloneRaw(snapshot.Config), Requirements: mapsCloneRaw(snapshot.Requirements)}
	drifted.Config["approval_policy"] = json.RawMessage(`"on-request"`)
	if snapshot.equal(drifted) {
		t.Fatal("constraint drift was not detected")
	}
}

func TestCodexLifecycleStableHistoryIgnoresVolatileTurnFieldsAndCanonicalizesItems(t *testing.T) {
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		lifecycleInitialize(t, ctx, conn)
		first := readRPCRequest(t, ctx, conn, "thread/read")
		if string(first.Params["threadId"]) != `"thread-original"` || string(first.Params["includeTurns"]) != "true" {
			t.Fatalf("first params=%s", mustJSON(first.Params))
		}
		sendRPCResult(t, ctx, conn, first.ID, `{"thread":{"id":"thread-original","turns":[{"id":"turn-1","status":"interrupted","items":[{"type":"message","content":{"b":2,"a":1}}]}]}}`)
		second := readRPCRequest(t, ctx, conn, "thread/read")
		sendRPCResult(t, ctx, conn, second.ID, `{"thread":{"id":"thread-original","turns":[{"id":"turn-1","status":"completed","completedAt":999,"items":[{"content":{"a":1,"b":2},"type":"message"}]}]}}`)
	})
	verifier, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	before, err := verifier.ReadStableHistory(context.Background(), "thread-original")
	if err != nil {
		t.Fatal(err)
	}
	after, err := verifier.ReadStableHistory(context.Background(), "thread-original")
	if err != nil || before != after {
		t.Fatalf("before=%x after=%x error=%v", before, after, err)
	}
}

func TestCodexLifecycleStableHistoryRejectsMissingStableIdentity(t *testing.T) {
	for _, response := range []string{
		`{"thread":{"id":"thread-other","turns":[{"id":"turn-1","items":[]}]}}`,
		`{"thread":{"id":"thread-original","turns":[]}}`,
		`{"thread":{"id":"thread-original","turns":[{"id":"","items":[]}]}}`,
	} {
		t.Run(response, func(t *testing.T) {
			socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
				lifecycleInitialize(t, ctx, conn)
				read := readRPCRequest(t, ctx, conn, "thread/read")
				sendRPCResult(t, ctx, conn, read.ID, response)
			})
			verifier, err := newCodexLifecycle(context.Background(), socket)
			if err != nil {
				t.Fatal(err)
			}
			defer verifier.Close()
			if _, err := verifier.ReadStableHistory(context.Background(), "thread-original"); !errors.Is(err, errCodexProtocol) {
				t.Fatalf("error=%v", err)
			}
		})
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
			response := `{"id":` + jsonNumber(read.ID) + `,"result":{"thread":{"id":"thread-stale","cwd":"/tmp/project","modelProvider":"openai"}}}`
			if err := conn.Write(ctx, websocket.MessageText, []byte(response)); err != nil {
				return
			}
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
	initialized := make(chan struct{})
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		index := connections.Add(1)
		lifecycleInitialize(t, ctx, conn)
		if index == 1 {
			close(initialized)
			_, _, _ = conn.Read(ctx)
			return
		}
		resume := readRPCRequest(t, ctx, conn, "thread/resume")
		if len(resume.Params) != 2 || string(resume.Params["threadId"]) != `"thread-original"` || string(resume.Params["excludeTurns"]) != "true" {
			t.Fatalf("resume params=%s", mustJSON(resume.Params))
		}
		response := `{"thread":{"id":"thread-original","cwd":"/tmp/project","modelProvider":"openai"},"model":"gpt-5.6-sol","modelProvider":"openai","serviceTier":null,"cwd":"/tmp/project","approvalPolicy":"never","approvalsReviewer":"user","sandbox":{"type":"workspaceWrite","networkAccess":true,"writableRoots":["/tmp/write"],"excludeSlashTmp":true,"excludeTmpdirEnvVar":false},"activePermissionProfile":null,"reasoningEffort":"medium","runtimeWorkspaceRoots":["/tmp/project"],"instructionSources":["/tmp/AGENTS.md"],"multiAgentMode":"explicitRequestOnly"}`
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
	select {
	case <-initialized:
	case <-time.After(time.Second):
		t.Fatal("initial lifecycle did not consume initialized notification")
	}
	effective, err := verifier.ReadEffective(context.Background(), "thread-original")
	if err != nil || effective.ModelProvider != "openai" || effective.MultiAgentMode != "explicitRequestOnly" || effective.SandboxMode != "workspace-write" || !effective.SandboxNetworkAccess || !effective.SandboxExcludeSlashTmp || effective.SandboxExcludeTmpdirEnvVar || !reflect.DeepEqual(effective.SandboxWritableRoots, []string{"/tmp/write"}) || !reflect.DeepEqual(effective.RuntimeWorkspaceRoots, []string{"/tmp/project"}) || !reflect.DeepEqual(effective.InstructionSources, []string{"/tmp/AGENTS.md"}) {
		t.Fatalf("effective=%+v error=%v", effective, err)
	}
}

func TestCodexLifecycleAppliesOnlyObservedReasoningOnStoppedThread(t *testing.T) {
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
		response := `{"thread":{"id":"thread-original","cwd":"/tmp/project","modelProvider":"openai"},"model":"gpt-5.6-sol","modelProvider":"openai","cwd":"/tmp/project","reasoningEffort":null}`
		sendRPCResult(t, ctx, conn, resume.ID, response)
		update := readRPCRequest(t, ctx, conn, "thread/settings/update")
		if len(update.Params) != 3 || string(update.Params["threadId"]) != `"thread-original"` || string(update.Params["model"]) != `"gpt-5.6-sol"` || string(update.Params["effort"]) != `"medium"` {
			t.Fatalf("unexpected update or privilege override: %s", mustJSON(update.Params))
		}
		sendRPCResult(t, ctx, conn, update.ID, `{}`)
		unsubscribe := readRPCRequest(t, ctx, conn, "thread/unsubscribe")
		sendRPCResult(t, ctx, conn, unsubscribe.ID, `{"status":"unsubscribed"}`)
	})
	verifier, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	expected := codexThreadIdentity{ID: "thread-original", CWD: "/tmp/project", ModelProvider: "openai", Model: "gpt-5.6-sol"}
	if safeNoDispatch, err := verifier.ApplyObservedReasoning(context.Background(), expected, ManagedCodexPolicy{Model: expected.Model, ReasoningEffort: "medium"}); err != nil || safeNoDispatch {
		t.Fatalf("apply observed reasoning safeNoDispatch=%t error=%v", safeNoDispatch, err)
	}
}

func TestCodexLifecycleRejectsContradictoryResumeBeforeSettingsUpdate(t *testing.T) {
	var connections atomic.Int32
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		index := connections.Add(1)
		lifecycleInitialize(t, ctx, conn)
		if index == 1 {
			_, _, _ = conn.Read(ctx)
			return
		}
		resume := readRPCRequest(t, ctx, conn, "thread/resume")
		response := `{"thread":{"id":"thread-original","cwd":"/tmp/project","modelProvider":"openai"},"model":"different","modelProvider":"openai","cwd":"/tmp/project","reasoningEffort":null}`
		sendRPCResult(t, ctx, conn, resume.ID, response)
		unsubscribe := readRPCRequest(t, ctx, conn, "thread/unsubscribe")
		sendRPCResult(t, ctx, conn, unsubscribe.ID, `{"status":"unsubscribed"}`)
	})
	verifier, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	expected := codexThreadIdentity{ID: "thread-original", CWD: "/tmp/project", ModelProvider: "openai", Model: "gpt-5.6-sol"}
	if safeNoDispatch, err := verifier.ApplyObservedReasoning(context.Background(), expected, ManagedCodexPolicy{Model: expected.Model, ReasoningEffort: "medium"}); !errors.Is(err, ErrManagedPolicyDrift) || safeNoDispatch {
		t.Fatalf("contradictory resume safeNoDispatch=%t error=%v", safeNoDispatch, err)
	}
}

func TestConversionConstraintsUseOnlyMatchingObservedDefaultReviewer(t *testing.T) {
	snapshot := codexConstraintSnapshot{
		Config:       map[string]json.RawMessage{"approvals_reviewer": json.RawMessage(`null`)},
		Requirements: map[string]json.RawMessage{"requirements": json.RawMessage(`{"allowedApprovalsReviewers":["user"]}`)},
	}
	policy := managedPolicy()
	effective := managedPreflight("/tmp/project").EffectiveCodexPolicy
	if _, err := snapshot.validateConversion("/tmp/project", policy, effective); err != nil {
		t.Fatalf("observed default rejected: %v", err)
	}
	for name, config := range map[string]json.RawMessage{"conflicting": json.RawMessage(`"auto_review"`), "malformed": json.RawMessage(`{"bad":true}`)} {
		snapshot.Config["approvals_reviewer"] = config
		if _, err := snapshot.validateConversion("/tmp/project", policy, effective); err == nil {
			t.Fatalf("%s explicit reviewer accepted", name)
		}
	}
	delete(snapshot.Config, "approvals_reviewer")
	effective.ApprovalsReviewer = "unknown"
	if _, err := snapshot.validateConversion("/tmp/project", policy, effective); err == nil {
		t.Fatal("unknown effective reviewer accepted")
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
		response := `{"thread":{"id":"thread-original","cwd":"/tmp/project","modelProvider":"openai"},"model":"gpt-5.6-sol","modelProvider":"openai","serviceTier":null,"cwd":"/tmp/project","approvalPolicy":"never","approvalsReviewer":"user","sandbox":{"type":"dangerFullAccess"},"activePermissionProfile":null,"reasoningEffort":"medium","runtimeWorkspaceRoots":["/tmp/project"],"instructionSources":[],"multiAgentMode":"explicitRequestOnly"}`
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

func TestCodexLifecycleReconcilesActiveManagedThreadWithExactSettingsAndReadback(t *testing.T) {
	var connections atomic.Int32
	desired := managedPreflight("/tmp/project").EffectiveCodexPolicy
	desired.ModelProvider = "openai"
	desired.RuntimeWorkspaceRoots = []string{"/tmp/project"}
	desired.InstructionSources = []string{"/tmp/AGENTS.md"}
	desired.MultiAgentMode = "explicitRequestOnly"
	desired.SandboxMode = "workspace-write"
	desired.SandboxNetworkAccess = true
	desired.SandboxWritableRoots = []string{"/tmp/write"}
	desired.SandboxExcludeSlashTmp = true
	desired.SandboxExcludeTmpdirEnvVar = true
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		index := connections.Add(1)
		lifecycleInitialize(t, ctx, conn)
		if index == 1 {
			update := readRPCRequest(t, ctx, conn, "thread/settings/update")
			var sandbox map[string]any
			if json.Unmarshal(update.Params["sandboxPolicy"], &sandbox) != nil || len(update.Params) != 5 || string(update.Params["threadId"]) != `"thread-original"` || string(update.Params["model"]) != `"gpt-5.6-sol"` || string(update.Params["effort"]) != `"medium"` || string(update.Params["approvalPolicy"]) != `"never"` || !reflect.DeepEqual(sandbox, map[string]any{"type": "workspaceWrite", "networkAccess": true, "writableRoots": []any{"/tmp/write"}, "excludeSlashTmp": true, "excludeTmpdirEnvVar": true}) {
				t.Fatalf("update params=%s", mustJSON(update.Params))
			}
			sendRPCResult(t, ctx, conn, update.ID, `{}`)
			return
		}
		resume := readRPCRequest(t, ctx, conn, "thread/resume")
		response := `{"thread":{"id":"thread-original","cwd":"/tmp/project","modelProvider":"openai"},"model":"gpt-5.6-sol","modelProvider":"openai","serviceTier":null,"cwd":"/tmp/project","approvalPolicy":"never","approvalsReviewer":"user","sandbox":{"type":"workspaceWrite","networkAccess":true,"writableRoots":["/tmp/write"],"excludeSlashTmp":true,"excludeTmpdirEnvVar":true},"activePermissionProfile":null,"reasoningEffort":"medium","runtimeWorkspaceRoots":["/tmp/project"],"instructionSources":["/tmp/AGENTS.md"],"multiAgentMode":"explicitRequestOnly"}`
		sendRPCResult(t, ctx, conn, resume.ID, response)
		unsubscribe := readRPCRequest(t, ctx, conn, "thread/unsubscribe")
		sendRPCResult(t, ctx, conn, unsubscribe.ID, `{"status":"unsubscribed"}`)
	})
	verifier, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	effective, err := verifier.ReconcileEffective(context.Background(), "thread-original", desired)
	if err != nil || requiredPolicyDrift(effective, desired) != "" {
		t.Fatalf("effective=%+v error=%v", effective, err)
	}
}

func TestCodexLifecycleReconcileWorkspaceWriteSerializesEmptyRootsAsArray(t *testing.T) {
	var connections atomic.Int32
	desired := managedPreflight("/tmp/project").EffectiveCodexPolicy
	desired.ModelProvider = "openai"
	desired.MultiAgentMode = "explicitRequestOnly"
	desired.SandboxMode = "workspace-write"
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		index := connections.Add(1)
		lifecycleInitialize(t, ctx, conn)
		if index == 1 {
			update := readRPCRequest(t, ctx, conn, "thread/settings/update")
			if string(update.Params["sandboxPolicy"]) != `{"excludeSlashTmp":false,"excludeTmpdirEnvVar":false,"networkAccess":false,"type":"workspaceWrite","writableRoots":[]}` {
				t.Fatalf("sandbox policy=%s", update.Params["sandboxPolicy"])
			}
			sendRPCResult(t, ctx, conn, update.ID, `{}`)
			return
		}
		resume := readRPCRequest(t, ctx, conn, "thread/resume")
		response := `{"thread":{"id":"` + managedOriginalID + `","cwd":"/tmp/project","modelProvider":"openai"},"model":"gpt-5.6-sol","modelProvider":"openai","serviceTier":null,"cwd":"/tmp/project","approvalPolicy":"never","approvalsReviewer":"user","sandbox":{"type":"workspaceWrite","networkAccess":false,"writableRoots":[],"excludeSlashTmp":false,"excludeTmpdirEnvVar":false},"activePermissionProfile":null,"reasoningEffort":"medium","runtimeWorkspaceRoots":[],"instructionSources":[],"multiAgentMode":"explicitRequestOnly"}`
		sendRPCResult(t, ctx, conn, resume.ID, response)
		unsubscribe := readRPCRequest(t, ctx, conn, "thread/unsubscribe")
		sendRPCResult(t, ctx, conn, unsubscribe.ID, `{"status":"unsubscribed"}`)
	})
	verifier, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	if _, err := verifier.ReconcileEffective(context.Background(), managedOriginalID, desired); err != nil {
		t.Fatalf("error=%v", err)
	}
}

func TestCodexLifecycleTreatsSettingsUpdateFailureAsOutcomeUnknown(t *testing.T) {
	socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
		lifecycleInitialize(t, ctx, conn)
		update := readRPCRequest(t, ctx, conn, "thread/settings/update")
		send(t, ctx, conn, `{"id":`+jsonNumber(update.ID)+`,"error":{"code":-32600,"message":"rejected"}}`)
	})
	verifier, err := newCodexLifecycle(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	if _, err := verifier.ReconcileEffective(context.Background(), "thread-original", managedPreflight("/tmp/project").EffectiveCodexPolicy); !errors.Is(err, ErrManagedSettingsUnknown) {
		t.Fatalf("error=%v", err)
	}
}

func TestRequiredPolicyComparisonCoversEqualAndRootDrift(t *testing.T) {
	policy := managedPreflight("/tmp/project").EffectiveCodexPolicy
	policy.RuntimeWorkspaceRoots = []string{"/tmp/project"}
	policy.InstructionSources = []string{"/tmp/AGENTS.md"}
	policy.MultiAgentMode = "explicitRequestOnly"
	if !sameRequiredPolicy(policy, policy) {
		t.Fatal("identical policy rejected")
	}
	drifted := policy
	drifted.RuntimeWorkspaceRoots = []string{"/tmp/other"}
	if drift := requiredPolicyDrift(drifted, policy); drift != "runtime_workspace_roots" {
		t.Fatalf("drift=%q", drift)
	}
	drifted = policy
	drifted.InstructionSources = []string{"/tmp/other.md"}
	drifted.MultiAgentMode = "proactive"
	if drift := requiredPolicyDrift(drifted, policy); drift != "instruction_sources,multi_agent_mode" {
		t.Fatalf("drift=%q", drift)
	}
	for name, mutate := range map[string]func(*EffectiveCodexPolicy){
		"sandbox_exclude_slash_tmp":  func(value *EffectiveCodexPolicy) { value.SandboxExcludeSlashTmp = true },
		"sandbox_exclude_tmpdir_env": func(value *EffectiveCodexPolicy) { value.SandboxExcludeTmpdirEnvVar = true },
		"sandbox_network_access":     func(value *EffectiveCodexPolicy) { value.SandboxNetworkAccess = true },
		"sandbox_writable_roots":     func(value *EffectiveCodexPolicy) { value.SandboxWritableRoots = []string{"/tmp/write"} },
	} {
		t.Run(name, func(t *testing.T) {
			drifted := policy
			mutate(&drifted)
			if drift := requiredPolicyDrift(drifted, policy); drift != name {
				t.Fatalf("drift=%q", drift)
			}
		})
	}
}

func TestValidCodexThreadIDAcceptsExactMaximumLength(t *testing.T) {
	if !validCodexThreadID(strings.Repeat("a", maxIdentityBytes)) {
		t.Fatal("exact maximum thread ID was rejected")
	}
}

func mapsCloneRaw(source map[string]json.RawMessage) map[string]json.RawMessage {
	clone := make(map[string]json.RawMessage, len(source))
	for key, value := range source {
		clone[key] = append(json.RawMessage(nil), value...)
	}
	return clone
}

func mustJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
