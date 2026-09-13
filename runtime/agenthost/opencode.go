package agenthost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const openCodeResponseBytes = 1 << 20

var errOpenCodeInitialMissing = errors.New("OpenCode session is not managed")

type openCodeObservation struct {
	identity nativeIdentity
	status   Status
	reason   Reason
}

func (p Probe) assessOpenCode(parent context.Context, target Agent) (Assessment, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	managed, err := validateNativeTarget(p.Host, target, "opencode", "herdr:opencode", "id")
	if err != nil || p.wait == nil || ctx.Err() != nil {
		return nativeAssessmentFailure(ctx, "OpenCode", err)
	}
	server, configured := p.native.OpenCodeServers[target.PaneID]
	if !managed || !configured || target.Session == nil || target.Session.Value == "" {
		return nativeUnknown(NativeNotManaged), nil
	}
	first, err := p.observeOpenCode(ctx, target, server, false)
	if errors.Is(err, errOpenCodeInitialMissing) {
		return nativeUnknown(NativeNotManaged), nil
	}
	if err != nil {
		return nativeAssessmentFailure(ctx, "OpenCode", err)
	}
	if err := p.wait(ctx, DefaultSettle()); err != nil {
		return nativeAssessmentFailure(ctx, "OpenCode", err)
	}
	second, err := p.observeOpenCode(ctx, target, server, true)
	if err != nil {
		return nativeAssessmentFailure(ctx, "OpenCode", err)
	}
	if err := ctx.Err(); err != nil {
		return nativeAssessmentFailure(ctx, "OpenCode", err)
	}
	if !sameNativeIdentity(first.identity, second.identity) || first.status != second.status || first.reason != second.reason {
		return nativeFailure(ErrStaleReport)
	}
	if second.status == Unknown {
		return nativeUnknown(second.reason), nil
	}
	return Assessment{Status: second.status, Reason: second.reason, Stable: true}, nil
}

func (p Probe) observeOpenCode(ctx context.Context, target Agent, server OpenCodeServer, previouslyObserved bool) (openCodeObservation, error) {
	expected := nativeIdentityFromAgent(target)
	before, err := p.Host.findNativeIdentity(ctx, target.PaneID)
	if err != nil {
		return openCodeObservation{}, err
	}
	if !sameNativeIdentity(expected, before) {
		return openCodeObservation{}, ErrStaleReport
	}
	assessment, err := readOpenCodeStatus(ctx, server, target.Session.Value, previouslyObserved)
	if err != nil {
		return openCodeObservation{}, err
	}
	after, err := p.Host.findNativeIdentity(ctx, target.PaneID)
	if err != nil {
		return openCodeObservation{}, err
	}
	if err := ctx.Err(); err != nil {
		return openCodeObservation{}, err
	}
	if !sameNativeIdentity(before, after) {
		return openCodeObservation{}, ErrStaleReport
	}
	return openCodeObservation{identity: after, status: assessment.Status, reason: assessment.Reason}, nil
}

func readOpenCodeStatus(ctx context.Context, server OpenCodeServer, sessionID string, previouslyObserved bool) (Assessment, error) {
	transport := &http.Transport{
		Proxy:       nil,
		DialContext: (&net.Dialer{}).DialContext,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	base := strings.TrimSuffix(server.URL, "/")
	query := url.Values{"directory": []string{server.Directory}}.Encode()
	sessionEndpoint := base + "/session/" + url.PathEscape(sessionID) + "?" + query

	checkSession := func(initial bool) error {
		statusCode, body, err := openCodeGET(ctx, client, sessionEndpoint)
		if err != nil {
			return err
		}
		if statusCode == http.StatusNotFound {
			if initial && !previouslyObserved {
				return errOpenCodeInitialMissing
			}
			return ErrStaleReport
		}
		if statusCode < 200 || statusCode >= 300 {
			return errors.New("OpenCode native request failed")
		}
		var session struct {
			ID        string `json:"id"`
			Directory string `json:"directory"`
			ParentID  string `json:"parentID"`
		}
		if err := decodeOpenCode(body, &session); err != nil {
			return err
		}
		if session.ID == "" || session.Directory == "" {
			return errors.New("invalid OpenCode native protocol")
		}
		if session.ID != sessionID || session.Directory != server.Directory || session.ParentID != "" {
			return ErrStaleReport
		}
		return nil
	}
	if err := checkSession(true); err != nil {
		return Assessment{}, err
	}

	question, err := openCodePending(ctx, client, base+"/question?"+query, sessionID)
	if err != nil {
		return Assessment{}, err
	}
	permission, err := openCodePending(ctx, client, base+"/permission?"+query, sessionID)
	if err != nil {
		return Assessment{}, err
	}
	statusCode, body, err := openCodeGET(ctx, client, base+"/session/status?"+query)
	if err != nil || statusCode < 200 || statusCode >= 300 {
		return Assessment{}, errors.New("OpenCode native request failed")
	}
	var statuses map[string]json.RawMessage
	if err := decodeOpenCode(body, &statuses); err != nil || statuses == nil {
		return Assessment{}, errors.New("invalid OpenCode native protocol")
	}
	var selected struct {
		Type string `json:"type"`
	}
	rawStatus, present := statuses[sessionID]
	if present {
		if err := decodeOpenCode(rawStatus, &selected); err != nil || selected.Type == "" {
			return Assessment{}, errors.New("invalid OpenCode native protocol")
		}
	}
	if err := checkSession(false); err != nil {
		return Assessment{}, err
	}

	if question && permission {
		return Assessment{Status: Blocked, Reason: OpenCodeQuestionAndPermission, Stable: true}, nil
	}
	if question {
		return Assessment{Status: Blocked, Reason: OpenCodeQuestion, Stable: true}, nil
	}
	if permission {
		return Assessment{Status: Blocked, Reason: OpenCodePermission, Stable: true}, nil
	}
	if !present || selected.Type == "idle" {
		return Assessment{Status: Idle, Reason: OpenCodeStatus, Stable: true}, nil
	}
	switch selected.Type {
	case "busy", "retry":
		return Assessment{Status: Working, Reason: OpenCodeStatus, Stable: true}, nil
	default:
		return nativeUnknown(NativeUnsupported), nil
	}
}

func openCodePending(ctx context.Context, client *http.Client, endpoint, sessionID string) (bool, error) {
	statusCode, body, err := openCodeGET(ctx, client, endpoint)
	if err != nil || statusCode < 200 || statusCode >= 300 {
		return false, errors.New("OpenCode native request failed")
	}
	var pending []struct {
		SessionID string `json:"sessionID"`
	}
	if err := decodeOpenCode(body, &pending); err != nil || pending == nil {
		return false, errors.New("invalid OpenCode native protocol")
	}
	matched := false
	for _, item := range pending {
		if item.SessionID == "" {
			return false, errors.New("invalid OpenCode native protocol")
		}
		matched = matched || item.SessionID == sessionID
	}
	return matched, nil
}

func openCodeGET(ctx context.Context, client *http.Client, endpoint string) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	limited := &io.LimitedReader{R: response.Body, N: openCodeResponseBytes + 1}
	body, err := io.ReadAll(limited)
	if err != nil || limited.N <= 0 {
		return 0, nil, errors.New("OpenCode native response exceeds limit")
	}
	return response.StatusCode, body, nil
}

func decodeOpenCode(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid OpenCode native protocol")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("invalid OpenCode native protocol")
	}
	return nil
}
