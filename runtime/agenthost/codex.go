package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

const (
	CodexStatus         Reason = "codex_status"
	Approval            Reason = "codex_approval"
	UserInput           Reason = "codex_user_input"
	ApprovalAndInput    Reason = "codex_approval_and_input"
	NotManaged          Reason = "codex_not_managed"
	AgentSystemError    Reason = "codex_agent_system_error"
	UnsupportedStatus   Reason = "codex_unsupported_status"
	MissingCodexSocket  Reason = "codex_missing_socket"
	MissingCodexSession Reason = "codex_missing_session"
	CodexUnavailable    Reason = "codex_unavailable"

	codexMessageBytes = 1 << 20
	codexTrafficBytes = 4 << 20
	codexMessages     = 64
)

func codexUnknown(reason Reason) Assessment { return Assessment{Status: Unknown, Reason: reason} }

func (p Probe) assessCodex(parent context.Context, target Agent) (Assessment, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	fail := func() (Assessment, error) {
		err := ctx.Err()
		if err == nil {
			err = errors.New("assess Codex: invalid input or unavailable protocol")
		}
		return codexUnknown(CodexUnavailable), err
	}
	// Validate the target independently: an empty Codex session value is missing
	// managed identity, but malformed session fields remain invalid input.
	identity := target
	identity.Session = nil
	if p.wait == nil || validateTarget(identity) != nil || target.SessionName != p.Host.Session || ctx.Err() != nil {
		return fail()
	}
	if s := target.Session; s != nil {
		for name, value := range map[string]string{"session source": s.Source, "session agent": s.Agent, "session kind": s.Kind} {
			if validateBounded(name, value, maxIdentityBytes, false) != nil {
				return fail()
			}
		}
		if validateBounded("session value", s.Value, maxIdentityBytes, true) != nil {
			return fail()
		}
	}
	if p.Host.CodexSocket == "" {
		return codexUnknown(MissingCodexSocket), nil
	}
	if !filepath.IsAbs(p.Host.CodexSocket) || strings.ContainsRune(p.Host.CodexSocket, 0) {
		return fail()
	}
	if target.Session == nil || target.Session.Agent != "codex" || target.Session.Kind != "id" || target.Session.Value == "" {
		return codexUnknown(MissingCodexSession), nil
	}
	before, err := p.Host.find(ctx, target.PaneID)
	if err != nil {
		if errors.Is(err, ErrStaleReport) {
			return codexUnknown(Unsettled), ErrStaleReport
		}
		return fail()
	}
	if !sameImmutable(target, before) {
		return codexUnknown(Unsettled), ErrStaleReport
	}
	assessment, err := readCodex(ctx, p.Host.CodexSocket, target.Session.Value)
	if err != nil {
		return fail()
	}
	after, err := p.Host.find(ctx, target.PaneID)
	if err != nil {
		if errors.Is(err, ErrStaleReport) {
			return codexUnknown(Unsettled), ErrStaleReport
		}
		return fail()
	}
	if !sameImmutable(before, after) {
		return codexUnknown(Unsettled), ErrStaleReport
	}
	if ctx.Err() != nil {
		return fail()
	}
	return assessment, nil
}

// Every call owns its connection. Neither a reconnect nor a later assessment
// can reuse an old thread snapshot. HTTP has no proxy or network fallback.
func readCodex(ctx context.Context, socket, threadID string) (Assessment, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	defer transport.CloseIdleConnections()
	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	})
	if err != nil {
		return Assessment{}, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(codexMessageBytes)
	client := codexReader{conn: conn}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"herdr-plugin-kit","version":"1"},"capabilities":{"experimentalApi":true}}}`)); err != nil {
		return Assessment{}, err
	}
	raw, err := client.response(ctx, "1")
	if err != nil {
		return Assessment{}, err
	}
	var initialized struct {
		UserAgent string `json:"userAgent"`
	}
	if json.Unmarshal(raw, &initialized) != nil || initialized.UserAgent == "" {
		return Assessment{}, errCodexProtocol
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"method":"initialized"}`)); err != nil {
		return Assessment{}, err
	}
	// Encoding a string cannot fail; use JSON escaping for the opaque ID.
	id, _ := json.Marshal(threadID)
	request := []byte(`{"id":2,"method":"thread/read","params":{"threadId":` + string(id) + `,"includeTurns":false}}`)
	if err := conn.Write(ctx, websocket.MessageText, request); err != nil {
		return Assessment{}, err
	}
	raw, err = client.response(ctx, "2")
	if err != nil {
		return Assessment{}, err
	}
	var result struct {
		Thread struct {
			ID     string `json:"id"`
			Status struct {
				Type        string    `json:"type"`
				ActiveFlags []*string `json:"activeFlags"`
			} `json:"status"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Thread.ID != threadID || result.Thread.Status.Type == "" {
		return Assessment{}, errCodexProtocol
	}
	status := result.Thread.Status
	switch status.Type {
	case "idle":
		return Assessment{Status: Idle, Reason: CodexStatus, Stable: true}, nil
	case "notLoaded":
		return codexUnknown(NotManaged), nil
	case "systemError":
		return codexUnknown(AgentSystemError), nil
	case "active":
		if status.ActiveFlags == nil {
			return Assessment{}, errCodexProtocol
		}
		approval, input := false, false
		for _, flag := range status.ActiveFlags {
			if flag == nil {
				return Assessment{}, errCodexProtocol
			}
			switch *flag {
			case "waitingOnApproval":
				approval = true
			case "waitingOnUserInput":
				input = true
			default:
				return codexUnknown(UnsupportedStatus), nil
			}
		}
		if approval && input {
			return Assessment{Status: Blocked, Reason: ApprovalAndInput, Stable: true}, nil
		}
		if approval {
			return Assessment{Status: Blocked, Reason: Approval, Stable: true}, nil
		}
		if input {
			return Assessment{Status: Blocked, Reason: UserInput, Stable: true}, nil
		}
		return Assessment{Status: Working, Reason: CodexStatus, Stable: true}, nil
	default:
		return codexUnknown(UnsupportedStatus), nil
	}
}

var errCodexProtocol = errors.New("invalid Codex protocol")

type codexReader struct {
	conn            *websocket.Conn
	messages, bytes int
}

func (c *codexReader) response(ctx context.Context, id string) (json.RawMessage, error) {
	for {
		kind, data, err := c.conn.Read(ctx)
		if err != nil {
			return nil, err
		}
		c.messages++
		c.bytes += len(data)
		if kind != websocket.MessageText || c.messages > codexMessages || c.bytes > codexTrafficBytes || !utf8.Valid(data) {
			return nil, errCodexProtocol
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method json.RawMessage `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(data, &message) != nil {
			return nil, errCodexProtocol
		}
		if message.Method != nil {
			var method string
			if json.Unmarshal(message.Method, &method) != nil || method == "" || message.ID != nil || message.Result != nil || message.Error != nil {
				return nil, errCodexProtocol
			}
			continue
		}
		if string(message.ID) != id || message.Error != nil || len(message.Result) == 0 || string(message.Result) == "null" {
			return nil, errCodexProtocol
		}
		return message.Result, nil
	}
}
