package agenthost

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type EffectiveCodexPolicy struct {
	Model, ReasoningEffort, ApprovalPolicy, ApprovalsReviewer  string
	SandboxMode, PermissionProfile, ModelProvider, ServiceTier string
	PermissionProfileExtends                                   string
	MultiAgentMode                                             string
	CWD                                                        string
	SandboxNetworkAccess                                       bool
	SandboxExcludeSlashTmp                                     bool
	SandboxExcludeTmpdirEnvVar                                 bool
	SandboxWritableRoots                                       []string
	RuntimeWorkspaceRoots                                      []string
	InstructionSources                                         []string
}

type codexPreflight struct {
	EffectiveCodexPolicy
	DefaultPermissionProfile string
}

type codexThreadIdentity struct {
	ID, CWD, ForkedFromID, ModelProvider, Model, ReasoningEffort string
}

type codexConstraintSnapshot struct {
	Config       map[string]json.RawMessage
	Requirements map[string]json.RawMessage
}

type managedVerifier interface {
	Preflight(context.Context, string, ManagedCodexPolicy) (codexPreflight, error)
	ReadConstraints(context.Context, string) (codexConstraintSnapshot, error)
	Arm() error
	ConfirmIdentity(context.Context, ManagedOperation, string, string) (codexThreadIdentity, error)
	ListCLIThreads(context.Context, string) ([]codexThreadIdentity, error)
	ListThreads(context.Context, string) ([]codexThreadIdentity, error)
	ReadThread(context.Context, string) (codexThreadIdentity, error)
	ReadLoadedThread(context.Context, string) (codexThreadIdentity, error)
	RecheckIdentity(context.Context, string) (codexThreadIdentity, error)
	ReadEffective(context.Context, string) (EffectiveCodexPolicy, error)
	ApplyObservedReasoning(context.Context, codexThreadIdentity, ManagedCodexPolicy) (safeNoDispatch bool, err error)
	ReconcileEffective(context.Context, string, EffectiveCodexPolicy) (EffectiveCodexPolicy, error)
	ReadStableHistory(context.Context, string) ([32]byte, error)
	DeleteThread(context.Context, string) error
	Close() error
}

type managedVerifierFactory interface {
	Connect(context.Context, string) (managedVerifier, error)
}

type codexVerifierFactory struct{}

func (codexVerifierFactory) Connect(ctx context.Context, socket string) (managedVerifier, error) {
	return newCodexLifecycle(ctx, socket)
}

type codexLifecycle struct {
	socket  string
	conn    *websocket.Conn
	reader  codexReader
	nextID  int
	armed   bool
	started map[string]codexThreadIdentity
}

func newCodexLifecycle(ctx context.Context, socket string) (*codexLifecycle, error) {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || strings.ContainsRune(socket, 0) {
		return nil, errors.New("Codex socket must be clean, absolute, and NUL-free")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	})
	transport.CloseIdleConnections()
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(codexMessageBytes)
	c := &codexLifecycle{socket: socket, conn: conn, nextID: 1, started: make(map[string]codexThreadIdentity)}
	c.reader = codexReader{conn: conn, notify: c.observe}
	var initialized struct {
		UserAgent string `json:"userAgent"`
	}
	if err := c.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "herdr-plugin-kit", "version": "0.3.0"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}, &initialized); err != nil || initialized.UserAgent == "" {
		conn.CloseNow()
		if err == nil {
			err = errCodexProtocol
		}
		return nil, err
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"method":"initialized"}`)); err != nil {
		conn.CloseNow()
		return nil, err
	}
	return c, nil
}

func (c *codexLifecycle) Close() error { return c.conn.CloseNow() }

func (c *codexLifecycle) call(ctx context.Context, method string, params any, result any) error {
	c.nextID++
	id := c.nextID
	request, err := json.Marshal(struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{id, method, params})
	if err != nil {
		return err
	}
	if len(request) > codexMessageBytes {
		return errCodexProtocol
	}
	if err := c.conn.Write(ctx, websocket.MessageText, request); err != nil {
		return err
	}
	raw, err := c.reader.response(ctx, strconv.Itoa(id))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, result); err != nil {
		return errCodexProtocol
	}
	return nil
}

func (c *codexLifecycle) observe(method string, params json.RawMessage) error {
	switch method {
	case "thread/started":
		var notification struct {
			Thread codexThread `json:"thread"`
		}
		if json.Unmarshal(params, &notification) != nil {
			return errCodexProtocol
		}
		identity, err := notification.Thread.identity()
		if err != nil {
			return err
		}
		if !c.armed {
			return nil
		}
		if previous, exists := c.started[identity.ID]; exists && previous != identity {
			return errCodexProtocol
		}
		c.started[identity.ID] = identity
	case "thread/status/changed":
		var notification struct {
			ThreadID string          `json:"threadId"`
			Status   json.RawMessage `json:"status"`
		}
		if json.Unmarshal(params, &notification) != nil || !validCodexThreadID(notification.ThreadID) || len(notification.Status) == 0 {
			return errCodexProtocol
		}
	}
	return nil
}

func (c *codexLifecycle) Preflight(ctx context.Context, cwd string, requested ManagedCodexPolicy) (codexPreflight, error) {
	snapshot, err := c.ReadConstraints(ctx, cwd)
	if err != nil {
		return codexPreflight{}, err
	}
	return snapshot.validate(cwd, requested)
}

func (c *codexLifecycle) ReadConstraints(ctx context.Context, cwd string) (codexConstraintSnapshot, error) {
	var configResponse struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := c.call(ctx, "config/read", struct {
		CWD           string `json:"cwd"`
		IncludeLayers bool   `json:"includeLayers"`
	}{cwd, false}, &configResponse); err != nil {
		return codexConstraintSnapshot{}, err
	}
	var requirementsResponse map[string]json.RawMessage
	if err := c.call(ctx, "configRequirements/read", struct{}{}, &requirementsResponse); err != nil {
		return codexConstraintSnapshot{}, err
	}
	if configResponse.Config == nil || requirementsResponse == nil {
		return codexConstraintSnapshot{}, errCodexProtocol
	}
	return codexConstraintSnapshot{Config: configResponse.Config, Requirements: requirementsResponse}, nil
}

func (s codexConstraintSnapshot) validate(cwd string, requested ManagedCodexPolicy) (codexPreflight, error) {
	preflight, err := parseCodexConfig(s.Config, cwd, requested)
	if err != nil {
		return codexPreflight{}, err
	}
	if err := validateRequirementsResponse(s.Requirements, requested, preflight); err != nil {
		return codexPreflight{}, err
	}
	return preflight, nil
}

func (s codexConstraintSnapshot) validateConversion(cwd string, requested ManagedCodexPolicy, effective EffectiveCodexPolicy) (codexPreflight, error) {
	if effective.ApprovalsReviewer != "user" && effective.ApprovalsReviewer != "auto_review" {
		return codexPreflight{}, ErrManagedUnsupported
	}
	config := s.Config
	if raw := config["approvals_reviewer"]; len(raw) == 0 || string(raw) == "null" {
		// Only Convert can supply an omitted config default, and only from
		// the exact stopped thread's effective App Server response.
		config = make(map[string]json.RawMessage, len(s.Config)+1)
		for key, value := range s.Config {
			config[key] = value
		}
		config["approvals_reviewer"], _ = json.Marshal(effective.ApprovalsReviewer)
	}
	preflight, err := parseCodexConfig(config, cwd, requested)
	if err != nil {
		return codexPreflight{}, err
	}
	if preflight.ApprovalsReviewer != effective.ApprovalsReviewer {
		return codexPreflight{}, ErrManagedPolicyDrift
	}
	if err := validateRequirementsResponse(s.Requirements, requested, preflight); err != nil {
		return codexPreflight{}, err
	}
	return preflight, nil
}

func (s codexConstraintSnapshot) equal(other codexConstraintSnapshot) bool {
	return len(s.driftFields(other)) == 0
}

func (s codexConstraintSnapshot) driftFields(other codexConstraintSnapshot) []string {
	var fields []string
	collect := func(prefix string, left, right map[string]json.RawMessage) {
		seen := make(map[string]struct{}, len(left)+len(right))
		for name := range left {
			seen[name] = struct{}{}
		}
		for name := range right {
			seen[name] = struct{}{}
		}
		for name := range seen {
			if !sameJSONValue(left[name], right[name]) {
				fields = append(fields, prefix+name)
			}
		}
	}
	collect("config.", s.Config, other.Config)
	collect("requirements.", s.Requirements, other.Requirements)
	slices.Sort(fields)
	return fields
}

func sameJSONValue(left, right json.RawMessage) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	leftCanonical, leftErr := json.Marshal(leftValue)
	rightCanonical, rightErr := json.Marshal(rightValue)
	return leftErr == nil && rightErr == nil && string(leftCanonical) == string(rightCanonical)
}

func (c *codexLifecycle) Arm() error {
	if c.armed || len(c.started) != 0 {
		return errors.New("Codex verifier cannot be armed from stale state")
	}
	c.armed = true
	return nil
}

func (c *codexLifecycle) ConfirmIdentity(ctx context.Context, operation ManagedOperation, sourceID, threadID string) (codexThreadIdentity, error) {
	if !c.armed || !validCodexThreadID(threadID) {
		return codexThreadIdentity{}, ErrManagedStale
	}
	if operation == ManagedResume && threadID != sourceID {
		return codexThreadIdentity{}, ErrManagedStale
	}
	for {
		identity, err := c.ReadThread(ctx, threadID)
		if err != nil {
			return codexThreadIdentity{}, err
		}
		switch operation {
		case ManagedNew:
			if started, observed := c.started[threadID]; observed {
				if started != identity || identity.ForkedFromID != "" {
					return codexThreadIdentity{}, ErrManagedStale
				}
				return identity, nil
			}
		case ManagedFork:
			if started, observed := c.started[threadID]; observed {
				if started != identity || identity.ID == sourceID || identity.ForkedFromID != sourceID {
					return codexThreadIdentity{}, ErrManagedStale
				}
				return identity, nil
			}
		case ManagedResume:
			if threadID != sourceID || identity.ID != sourceID {
				return codexThreadIdentity{}, ErrManagedStale
			}
			return identity, nil
		default:
			return codexThreadIdentity{}, ErrManagedUnsupported
		}
		if err := waitManaged(ctx, 25*time.Millisecond); err != nil {
			return codexThreadIdentity{}, err
		}
	}
}

func (c *codexLifecycle) ReadThread(ctx context.Context, threadID string) (codexThreadIdentity, error) {
	return c.readThread(ctx, threadID, false)
}

// ReadLoadedThread refuses disk-only metadata as evidence of a live source.
// This is a precondition, not a subscription or a policy-preservation guarantee.
func (c *codexLifecycle) ReadLoadedThread(ctx context.Context, threadID string) (codexThreadIdentity, error) {
	return c.readThread(ctx, threadID, true)
}

func (c *codexLifecycle) readThread(ctx context.Context, threadID string, requireLoaded bool) (codexThreadIdentity, error) {
	var response struct {
		Thread codexThread `json:"thread"`
	}
	if err := c.call(ctx, "thread/read", struct {
		ThreadID     string `json:"threadId"`
		IncludeTurns bool   `json:"includeTurns"`
	}{threadID, false}, &response); err != nil {
		return codexThreadIdentity{}, err
	}
	identity, err := response.Thread.identity()
	if err != nil || identity.ID != threadID {
		return codexThreadIdentity{}, ErrManagedStale
	}
	if requireLoaded {
		var status struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(response.Thread.Status, &status) != nil || (status.Type != "idle" && status.Type != "active") {
			return codexThreadIdentity{}, ErrManagedSettingsUnknown
		}
	}
	return identity, nil
}

func (c *codexLifecycle) ListCLIThreads(ctx context.Context, cwd string) ([]codexThreadIdentity, error) {
	return c.listThreads(ctx, cwd, true)
}

func (c *codexLifecycle) ListThreads(ctx context.Context, cwd string) ([]codexThreadIdentity, error) {
	return c.listThreads(ctx, cwd, false)
}

func (c *codexLifecycle) listThreads(ctx context.Context, cwd string, cliOnly bool) ([]codexThreadIdentity, error) {
	if !cleanAbsolute(cwd) {
		return nil, ErrManagedStale
	}
	const maxPages = 10
	var (
		cursor *string
		result []codexThreadIdentity
	)
	seen := make(map[string]struct{})
	seenCursors := make(map[string]struct{})
	for page := 0; page < maxPages; page++ {
		params := map[string]any{
			"cwd": cwd, "limit": 100, "sortKey": "created_at", "sortDirection": "desc",
			"useStateDbOnly": true,
		}
		if cliOnly {
			params["sourceKinds"] = []string{"cli"}
		}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		var response struct {
			Data       []codexThread `json:"data"`
			NextCursor *string       `json:"nextCursor"`
		}
		if err := c.call(ctx, "thread/list", params, &response); err != nil {
			return nil, err
		}
		for _, thread := range response.Data {
			identity, err := thread.identity()
			if err != nil || identity.CWD != cwd {
				return nil, ErrManagedStale
			}
			if _, duplicate := seen[identity.ID]; duplicate {
				return nil, ErrManagedAmbiguous
			}
			seen[identity.ID] = struct{}{}
			result = append(result, identity)
		}
		if response.NextCursor == nil {
			return result, nil
		}
		if *response.NextCursor == "" {
			return nil, ErrManagedAmbiguous
		}
		if _, duplicate := seenCursors[*response.NextCursor]; duplicate {
			return nil, ErrManagedAmbiguous
		}
		seenCursors[*response.NextCursor] = struct{}{}
		cursor = response.NextCursor
	}
	return nil, ErrManagedAmbiguous
}

func (c *codexLifecycle) RecheckIdentity(ctx context.Context, threadID string) (codexThreadIdentity, error) {
	return c.ReadThread(ctx, threadID)
}

func (c *codexLifecycle) ReadEffective(ctx context.Context, threadID string) (EffectiveCodexPolicy, error) {
	audit, err := newCodexLifecycle(ctx, c.socket)
	if err != nil {
		return EffectiveCodexPolicy{}, err
	}
	defer audit.Close()
	var response codexEffectiveResponse
	if err := audit.call(ctx, "thread/resume", struct {
		ThreadID     string `json:"threadId"`
		ExcludeTurns bool   `json:"excludeTurns"`
	}{threadID, true}, &response); err != nil {
		return EffectiveCodexPolicy{}, err
	}
	effective, err := response.effective(threadID)
	if err != nil {
		return EffectiveCodexPolicy{}, err
	}
	var unsubscribed struct {
		Status string `json:"status"`
	}
	if err := audit.call(ctx, "thread/unsubscribe", struct {
		ThreadID string `json:"threadId"`
	}{threadID}, &unsubscribed); err != nil || unsubscribed.Status != "unsubscribed" {
		if err == nil {
			err = errCodexProtocol
		}
		return EffectiveCodexPolicy{}, err
	}
	return effective, nil
}

// ApplyObservedReasoning records a fresh /status value on a stopped ordinary
// thread whose App Server record omitted the effort. It changes no permissions.
// The caller must independently read back identity and stable history even if
// the update response is lost.
func (c *codexLifecycle) ApplyObservedReasoning(ctx context.Context, expected codexThreadIdentity, observed ManagedCodexPolicy) (safeNoDispatch bool, resultErr error) {
	if expected.ReasoningEffort != "" || observed.Model != expected.Model || !supportedObservedCodexEffort(observed.ReasoningEffort) {
		return false, ErrManagedPolicyDrift
	}
	audit, err := newCodexLifecycle(ctx, c.socket)
	if err != nil {
		return true, err
	}
	defer audit.Close()
	var response codexEffectiveResponse
	if err := audit.call(ctx, "thread/resume", struct {
		ThreadID     string `json:"threadId"`
		ExcludeTurns bool   `json:"excludeTurns"`
	}{expected.ID, true}, &response); err != nil {
		return true, err
	}
	defer func() {
		var unsubscribe struct {
			Status string `json:"status"`
		}
		if err := audit.call(ctx, "thread/unsubscribe", struct {
			ThreadID string `json:"threadId"`
		}{expected.ID}, &unsubscribe); err != nil || unsubscribe.Status != "unsubscribed" {
			if err == nil {
				err = errCodexProtocol
			}
			resultErr = errors.Join(resultErr, err)
		}
	}()
	identity, err := response.Thread.identity()
	if err != nil || identity.ID != expected.ID || identity.CWD != expected.CWD || identity.ForkedFromID != expected.ForkedFromID || identity.ModelProvider != expected.ModelProvider || identity.Model != "" && identity.Model != expected.Model || identity.ReasoningEffort != "" || response.Model != expected.Model || response.ModelProvider != expected.ModelProvider || response.CWD != expected.CWD || response.ReasoningEffort != nil && *response.ReasoningEffort != "" {
		return false, ErrManagedPolicyDrift
	}
	var updated json.RawMessage
	if err := audit.call(ctx, "thread/settings/update", map[string]string{
		"threadId": expected.ID, "model": observed.Model, "effort": observed.ReasoningEffort,
	}, &updated); err != nil {
		return false, errors.Join(ErrManagedSettingsUnknown, err)
	}
	if string(updated) != "{}" {
		return false, errors.Join(ErrManagedSettingsUnknown, errCodexProtocol)
	}
	return false, nil
}

func (c *codexLifecycle) ReconcileEffective(ctx context.Context, threadID string, desired EffectiveCodexPolicy) (EffectiveCodexPolicy, error) {
	params := map[string]any{
		"threadId":       threadID,
		"model":          desired.Model,
		"effort":         desired.ReasoningEffort,
		"approvalPolicy": desired.ApprovalPolicy,
	}
	if desired.PermissionProfile != "" {
		params["permissions"] = desired.PermissionProfile
	} else {
		typeName, ok := sandboxAppServerType(desired.SandboxMode)
		if !ok {
			return EffectiveCodexPolicy{}, ErrManagedUnsupported
		}
		sandbox := map[string]any{"type": typeName}
		switch desired.SandboxMode {
		case "read-only":
			sandbox["networkAccess"] = desired.SandboxNetworkAccess
		case "workspace-write":
			sandbox["networkAccess"] = desired.SandboxNetworkAccess
			sandbox["writableRoots"] = append([]string{}, desired.SandboxWritableRoots...)
			sandbox["excludeSlashTmp"] = desired.SandboxExcludeSlashTmp
			sandbox["excludeTmpdirEnvVar"] = desired.SandboxExcludeTmpdirEnvVar
		}
		params["sandboxPolicy"] = sandbox
	}
	var response json.RawMessage
	if err := c.call(ctx, "thread/settings/update", params, &response); err != nil {
		return EffectiveCodexPolicy{}, errors.Join(ErrManagedSettingsUnknown, err)
	}
	if string(response) != "{}" {
		return EffectiveCodexPolicy{}, errors.Join(ErrManagedSettingsUnknown, errCodexProtocol)
	}
	effective, err := c.ReadEffective(ctx, threadID)
	if err != nil {
		return EffectiveCodexPolicy{}, errors.Join(ErrManagedSettingsUnknown, err)
	}
	if drift := requiredPolicyDrift(effective, desired); drift != "" {
		return effective, fmt.Errorf("reconciled Codex policy drift (%s): %w", drift, ErrManagedPolicyDrift)
	}
	return effective, nil
}

func (c *codexLifecycle) ReadStableHistory(ctx context.Context, threadID string) ([32]byte, error) {
	var response struct {
		Thread struct {
			ID    string `json:"id"`
			Turns []struct {
				ID    string          `json:"id"`
				Items json.RawMessage `json:"items"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if err := c.call(ctx, "thread/read", struct {
		ThreadID     string `json:"threadId"`
		IncludeTurns bool   `json:"includeTurns"`
	}{threadID, true}, &response); err != nil {
		return [32]byte{}, err
	}
	if response.Thread.ID != threadID || len(response.Thread.Turns) == 0 {
		return [32]byte{}, errCodexProtocol
	}
	for index, turn := range response.Thread.Turns {
		if turn.ID == "" || len(turn.Items) == 0 || !json.Valid(turn.Items) {
			return [32]byte{}, errCodexProtocol
		}
		var items any
		if json.Unmarshal(turn.Items, &items) != nil {
			return [32]byte{}, errCodexProtocol
		}
		canonical, err := json.Marshal(items)
		if err != nil {
			return [32]byte{}, errCodexProtocol
		}
		response.Thread.Turns[index].Items = canonical
	}
	raw, err := json.Marshal(response.Thread.Turns)
	if err != nil {
		return [32]byte{}, errCodexProtocol
	}
	return sha256.Sum256(raw), nil
}

func (c *codexLifecycle) DeleteThread(ctx context.Context, threadID string) error {
	if !validCodexThreadID(threadID) {
		return errCodexProtocol
	}
	var response struct{}
	return c.call(ctx, "thread/delete", struct {
		ThreadID string `json:"threadId"`
	}{threadID}, &response)
}

type codexThread struct {
	ID              string          `json:"id"`
	CWD             string          `json:"cwd"`
	ForkedFromID    *string         `json:"forkedFromId"`
	ModelProvider   string          `json:"modelProvider"`
	Model           *string         `json:"model"`
	ReasoningEffort *string         `json:"reasoningEffort"`
	Status          json.RawMessage `json:"status"`
}

func (t codexThread) identity() (codexThreadIdentity, error) {
	if !validCodexThreadID(t.ID) || !filepath.IsAbs(t.CWD) || filepath.Clean(t.CWD) != t.CWD || t.ModelProvider == "" {
		return codexThreadIdentity{}, errCodexProtocol
	}
	identity := codexThreadIdentity{ID: t.ID, CWD: t.CWD, ModelProvider: t.ModelProvider}
	if t.Model != nil {
		identity.Model = *t.Model
	}
	if t.ReasoningEffort != nil {
		identity.ReasoningEffort = *t.ReasoningEffort
	}
	if t.ForkedFromID != nil {
		if !validCodexThreadID(*t.ForkedFromID) {
			return codexThreadIdentity{}, errCodexProtocol
		}
		identity.ForkedFromID = *t.ForkedFromID
	}
	return identity, nil
}

type codexEffectiveResponse struct {
	Thread            codexThread     `json:"thread"`
	Model             string          `json:"model"`
	ModelProvider     string          `json:"modelProvider"`
	ServiceTier       *string         `json:"serviceTier"`
	CWD               string          `json:"cwd"`
	ApprovalPolicy    json.RawMessage `json:"approvalPolicy"`
	ApprovalsReviewer string          `json:"approvalsReviewer"`
	Sandbox           struct {
		Type                string   `json:"type"`
		NetworkAccess       bool     `json:"networkAccess"`
		WritableRoots       []string `json:"writableRoots"`
		ExcludeSlashTmp     bool     `json:"excludeSlashTmp"`
		ExcludeTmpdirEnvVar bool     `json:"excludeTmpdirEnvVar"`
	} `json:"sandbox"`
	ActivePermissionProfile *struct {
		ID      string  `json:"id"`
		Extends *string `json:"extends"`
	} `json:"activePermissionProfile"`
	ReasoningEffort       *string  `json:"reasoningEffort"`
	RuntimeWorkspaceRoots []string `json:"runtimeWorkspaceRoots"`
	InstructionSources    []string `json:"instructionSources"`
	MultiAgentMode        string   `json:"multiAgentMode"`
}

func (r codexEffectiveResponse) effective(threadID string) (EffectiveCodexPolicy, error) {
	identity, err := r.Thread.identity()
	if err != nil || identity.ID != threadID || r.CWD != identity.CWD || r.Model == "" || r.ModelProvider == "" || r.ModelProvider != identity.ModelProvider || r.ReasoningEffort == nil || *r.ReasoningEffort == "" || r.ApprovalsReviewer == "" {
		return EffectiveCodexPolicy{}, errCodexProtocol
	}
	var approval string
	if json.Unmarshal(r.ApprovalPolicy, &approval) != nil || approval == "" {
		return EffectiveCodexPolicy{}, errCodexProtocol
	}
	sandbox, ok := sandboxCLIValue(r.Sandbox.Type)
	if !ok {
		return EffectiveCodexPolicy{}, errCodexProtocol
	}
	for _, root := range r.Sandbox.WritableRoots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return EffectiveCodexPolicy{}, errCodexProtocol
		}
	}
	if sandbox != "workspace-write" && (len(r.Sandbox.WritableRoots) != 0 || r.Sandbox.ExcludeSlashTmp || r.Sandbox.ExcludeTmpdirEnvVar) {
		return EffectiveCodexPolicy{}, errCodexProtocol
	}
	if sandbox == "danger-full-access" && r.Sandbox.NetworkAccess {
		return EffectiveCodexPolicy{}, errCodexProtocol
	}
	if r.MultiAgentMode == "" {
		return EffectiveCodexPolicy{}, errCodexProtocol
	}
	effective := EffectiveCodexPolicy{
		Model: r.Model, ReasoningEffort: *r.ReasoningEffort, ApprovalPolicy: approval,
		ApprovalsReviewer: r.ApprovalsReviewer, SandboxMode: sandbox, ModelProvider: r.ModelProvider,
		MultiAgentMode: r.MultiAgentMode, CWD: r.CWD, SandboxNetworkAccess: r.Sandbox.NetworkAccess,
		SandboxExcludeSlashTmp: r.Sandbox.ExcludeSlashTmp, SandboxExcludeTmpdirEnvVar: r.Sandbox.ExcludeTmpdirEnvVar,
		SandboxWritableRoots: append([]string(nil), r.Sandbox.WritableRoots...),
	}
	if r.ServiceTier != nil {
		effective.ServiceTier = *r.ServiceTier
	}
	if r.ActivePermissionProfile != nil {
		if validateBounded("active permission profile", r.ActivePermissionProfile.ID, maxIdentityBytes, false) != nil {
			return EffectiveCodexPolicy{}, errCodexProtocol
		}
		effective.PermissionProfile = r.ActivePermissionProfile.ID
		if r.ActivePermissionProfile.Extends != nil {
			if validateBounded("active permission profile parent", *r.ActivePermissionProfile.Extends, maxIdentityBytes, false) != nil {
				return EffectiveCodexPolicy{}, errCodexProtocol
			}
			effective.PermissionProfileExtends = *r.ActivePermissionProfile.Extends
		}
	}
	for _, root := range r.RuntimeWorkspaceRoots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return EffectiveCodexPolicy{}, errCodexProtocol
		}
	}
	effective.RuntimeWorkspaceRoots = append([]string(nil), r.RuntimeWorkspaceRoots...)
	for _, source := range r.InstructionSources {
		if validateBounded("instruction source", source, maxPathBytes, false) != nil || strings.ContainsRune(source, 0) {
			return EffectiveCodexPolicy{}, errCodexProtocol
		}
	}
	effective.InstructionSources = append([]string(nil), r.InstructionSources...)
	return effective, nil
}

func parseCodexConfig(config map[string]json.RawMessage, cwd string, requested ManagedCodexPolicy) (codexPreflight, error) {
	readString := func(name string, required bool) (string, error) {
		raw, ok := config[name]
		if !ok || string(raw) == "null" {
			if required {
				return "", fmt.Errorf("config %s is missing", name)
			}
			return "", nil
		}
		var value string
		if json.Unmarshal(raw, &value) != nil || value == "" {
			return "", fmt.Errorf("config %s is unsupported", name)
		}
		return value, nil
	}
	for _, name := range []string{"model", "model_reasoning_effort", "approval_policy", "sandbox_mode"} {
		if _, err := readString(name, false); err != nil {
			return codexPreflight{}, err
		}
	}
	reviewer, err := readString("approvals_reviewer", true)
	if err != nil {
		return codexPreflight{}, err
	}
	permission, err := readString("default_permissions", false)
	if err != nil {
		return codexPreflight{}, err
	}
	serviceTier, err := readString("service_tier", false)
	if err != nil {
		return codexPreflight{}, err
	}
	// New applies the requested per-thread values in its TUI argv, and resolved
	// config/read legitimately omits unset defaults such as sandbox_mode.
	// Fork and Resume cannot override permissions and are audited against the
	// exact identified conversation after launch.
	effective := EffectiveCodexPolicy{Model: requested.Model, ReasoningEffort: requested.ReasoningEffort, ApprovalPolicy: requested.ApprovalPolicy, ApprovalsReviewer: reviewer, SandboxMode: requested.SandboxMode, ServiceTier: serviceTier, CWD: cwd}
	return codexPreflight{EffectiveCodexPolicy: effective, DefaultPermissionProfile: permission}, nil
}

func validateRequirementsResponse(response map[string]json.RawMessage, requested ManagedCodexPolicy, preflight codexPreflight) error {
	if len(response) != 1 {
		return errors.New("unsupported configRequirements/read response")
	}
	raw, ok := response["requirements"]
	if !ok {
		return errors.New("configRequirements/read response is missing requirements")
	}
	if string(raw) == "null" {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return errCodexProtocol
	}
	for name := range fields {
		if _, ok := knownRequirementFields[name]; !ok {
			return fmt.Errorf("unsupported Codex requirement %q", name)
		}
	}
	if err := requireAllowedString(fields["allowedApprovalPolicies"], requested.ApprovalPolicy); err != nil {
		return fmt.Errorf("approval policy requirement: %w", err)
	}
	if err := requireAllowedString(fields["allowedSandboxModes"], requested.SandboxMode); err != nil {
		return fmt.Errorf("sandbox requirement: %w", err)
	}
	if err := requireAllowedString(fields["allowedApprovalsReviewers"], preflight.ApprovalsReviewer); err != nil {
		return fmt.Errorf("reviewer requirement: %w", err)
	}
	if permissionRaw := fields["allowedPermissionProfiles"]; len(permissionRaw) != 0 && string(permissionRaw) != "null" {
		var allowed map[string]bool
		if json.Unmarshal(permissionRaw, &allowed) != nil || preflight.DefaultPermissionProfile == "" || !allowed[preflight.DefaultPermissionProfile] {
			return errors.New("permission profile requirement rejects effective profile")
		}
	}
	if defaultRaw := fields["defaultPermissions"]; len(defaultRaw) != 0 && string(defaultRaw) != "null" {
		var required string
		if json.Unmarshal(defaultRaw, &required) != nil || required == "" || required != preflight.DefaultPermissionProfile {
			return errors.New("default permission requirement rejects effective profile")
		}
	}
	if modelsRaw := fields["models"]; len(modelsRaw) != 0 && string(modelsRaw) != "null" {
		if err := validateModelRequirements(modelsRaw, requested, preflight); err != nil {
			return err
		}
	}
	if autoReviewRaw := fields["autoReview"]; len(autoReviewRaw) != 0 && string(autoReviewRaw) != "null" {
		var auto struct {
			RequiredOnModels []string `json:"requiredOnModels"`
		}
		if json.Unmarshal(autoReviewRaw, &auto) != nil {
			return errCodexProtocol
		}
		for _, model := range auto.RequiredOnModels {
			if model == requested.Model && preflight.ApprovalsReviewer != "auto_review" {
				return errors.New("reviewer requirement mandates auto_review")
			}
		}
	}
	return nil
}

func requireAllowedString(raw json.RawMessage, value string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return errCodexProtocol
	}
	for _, candidate := range values {
		var allowed string
		if json.Unmarshal(candidate, &allowed) == nil && allowed == value {
			return nil
		}
	}
	return errors.New("effective value is not allowed")
}

func validateModelRequirements(raw json.RawMessage, requested ManagedCodexPolicy, preflight codexPreflight) error {
	var models map[string]json.RawMessage
	if json.Unmarshal(raw, &models) != nil {
		return errCodexProtocol
	}
	for name := range models {
		if name != "newThread" {
			return fmt.Errorf("unsupported model requirement %q", name)
		}
	}
	newThread := models["newThread"]
	if len(newThread) == 0 || string(newThread) == "null" {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(newThread, &fields) != nil {
		return errCodexProtocol
	}
	for name := range fields {
		if name != "model" && name != "modelReasoningEffort" && name != "serviceTier" {
			return fmt.Errorf("unsupported new-thread model requirement %q", name)
		}
	}
	for name, expected := range map[string]string{"model": requested.Model, "modelReasoningEffort": requested.ReasoningEffort, "serviceTier": preflight.ServiceTier} {
		value := fields[name]
		if len(value) == 0 || string(value) == "null" {
			continue
		}
		var actual string
		if json.Unmarshal(value, &actual) != nil || actual != expected {
			return fmt.Errorf("new-thread %s requirement rejects effective value", name)
		}
	}
	return nil
}

var knownRequirementFields = map[string]struct{}{
	"additionalDeveloperInstructions": {}, "allowAppshots": {}, "allowBrowserAndComputerUse": {}, "allowLoginShell": {},
	"allowManagedHooksOnly": {}, "allowRemoteControl": {}, "allowedApprovalPolicies": {}, "allowedApprovalsReviewers": {},
	"allowedPermissionProfiles": {}, "allowedSandboxModes": {}, "allowedWebSearchModes": {}, "allowedWindowsSandboxImplementations": {},
	"application": {}, "autoReview": {}, "browserUse": {}, "chatgptBaseUrl": {}, "checkForUpdateOnStartup": {},
	"cliAuthCredentialsStore": {}, "computerUse": {}, "defaultPermissions": {}, "enforceResidency": {}, "featureRequirements": {},
	"feedback": {}, "hooks": {}, "inAppBrowser": {}, "logDir": {}, "modelCatalogJson": {}, "models": {}, "network": {},
	"sqliteHome": {}, "windowsSandboxPrivateDesktop": {},
}

func validCodexThreadID(id string) bool {
	return id != "" && len(id) <= maxIdentityBytes && !strings.ContainsAny(id, "\x00\r\n")
}

func sandboxCLIValue(protocol string) (string, bool) {
	switch protocol {
	case "readOnly":
		return "read-only", true
	case "workspaceWrite":
		return "workspace-write", true
	case "dangerFullAccess":
		return "danger-full-access", true
	default:
		return "", false
	}
}

func sandboxAppServerType(value string) (string, bool) {
	switch value {
	case "read-only":
		return "readOnly", true
	case "workspace-write":
		return "workspaceWrite", true
	case "danger-full-access":
		return "dangerFullAccess", true
	default:
		return "", false
	}
}

func sameRequiredPolicy(actual, expected EffectiveCodexPolicy) bool {
	return requiredPolicyDrift(actual, expected) == ""
}

func requiredPolicyDrift(actual, expected EffectiveCodexPolicy) string {
	var fields []string
	for name, different := range map[string]bool{
		"model": actual.Model != expected.Model, "reasoning": actual.ReasoningEffort != expected.ReasoningEffort,
		"approval": actual.ApprovalPolicy != expected.ApprovalPolicy, "reviewer": actual.ApprovalsReviewer != expected.ApprovalsReviewer,
		"sandbox": actual.SandboxMode != expected.SandboxMode, "permission_profile": actual.PermissionProfile != expected.PermissionProfile,
		"permission_profile_extends": actual.PermissionProfileExtends != expected.PermissionProfileExtends,
		"sandbox_network_access":     actual.SandboxNetworkAccess != expected.SandboxNetworkAccess,
		"sandbox_exclude_slash_tmp":  actual.SandboxExcludeSlashTmp != expected.SandboxExcludeSlashTmp,
		"sandbox_exclude_tmpdir_env": actual.SandboxExcludeTmpdirEnvVar != expected.SandboxExcludeTmpdirEnvVar,
		"model_provider":             actual.ModelProvider != expected.ModelProvider, "service_tier": actual.ServiceTier != expected.ServiceTier,
		"multi_agent_mode": actual.MultiAgentMode != expected.MultiAgentMode,
		"cwd":              actual.CWD != expected.CWD,
	} {
		if different {
			fields = append(fields, name)
		}
	}
	if !slices.Equal(actual.InstructionSources, expected.InstructionSources) {
		fields = append(fields, "instruction_sources")
	}
	if !slices.Equal(actual.SandboxWritableRoots, expected.SandboxWritableRoots) {
		fields = append(fields, "sandbox_writable_roots")
	}
	if len(actual.RuntimeWorkspaceRoots) != len(expected.RuntimeWorkspaceRoots) {
		fields = append(fields, "runtime_workspace_roots")
	} else {
		for i := range actual.RuntimeWorkspaceRoots {
			if actual.RuntimeWorkspaceRoots[i] != expected.RuntimeWorkspaceRoots[i] {
				fields = append(fields, "runtime_workspace_roots")
				break
			}
		}
	}
	slices.Sort(fields)
	return strings.Join(fields, ",")
}

func waitManaged(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
