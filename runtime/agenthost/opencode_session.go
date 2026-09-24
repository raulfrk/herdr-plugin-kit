package agenthost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const openCodeForkRequestTimeout = 10 * time.Second

// forkOpenCodeSession performs one native fork against the caller-verified
// source listener. Once the request is dispatched, an unsuccessful response
// cannot establish whether the server created a child, so it is never retried.
func forkOpenCodeSession(ctx context.Context, server OpenCodeServer, sourceID string) (string, error) {
	if err := validateOpenCodeServer(server); err != nil {
		return "", err
	}
	if err := validateBounded("OpenCode source session ID", sourceID, maxIdentityBytes, false); err != nil || !strings.HasPrefix(sourceID, "ses_") {
		return "", errors.New("invalid OpenCode source session ID")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	requestCtx, cancel := context.WithTimeout(ctx, openCodeForkRequestTimeout)
	defer cancel()
	status, body, err := openCodeSessionRequest(requestCtx, server, http.MethodPost, "/session/"+url.PathEscape(sourceID)+"/fork", []byte("{}"))
	if err != nil {
		return "", errors.Join(ErrForkOutcomeUncertain, err)
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("%w: OpenCode fork server status %d", ErrForkOutcomeUncertain, status)
	}
	var session struct {
		ID        string `json:"id"`
		Directory string `json:"directory"`
	}
	if err := decodeOpenCode(body, &session); err != nil || !strings.HasPrefix(session.ID, "ses_") || session.ID == sourceID || session.Directory != server.Directory {
		return "", ErrForkOutcomeUncertain
	}
	if err := validateBounded("OpenCode child session ID", session.ID, maxIdentityBytes, false); err != nil {
		return "", ErrForkOutcomeUncertain
	}
	return session.ID, nil
}

type openCodePromptSessions interface {
	Create(context.Context, OpenCodeServer) (string, error)
	Delete(context.Context, OpenCodeServer, string) error
}

type nativeOpenCodePromptSessions struct{}

func (nativeOpenCodePromptSessions) Create(ctx context.Context, server OpenCodeServer) (string, error) {
	if err := validateOpenCodeServer(server); err != nil {
		return "", err
	}
	status, body, err := openCodeSessionRequest(ctx, server, http.MethodPost, "/session", []byte("{}"))
	if err != nil || status < 200 || status >= 300 {
		return "", errors.New("create OpenCode prompt session failed")
	}
	var session struct {
		ID        string  `json:"id"`
		Directory string  `json:"directory"`
		ParentID  *string `json:"parentID"`
	}
	if err := decodeOpenCode(body, &session); err != nil || !strings.HasPrefix(session.ID, "ses_") || session.Directory != server.Directory || session.ParentID != nil {
		return "", errors.New("invalid created OpenCode prompt session")
	}
	if err := validateBounded("OpenCode session ID", session.ID, maxIdentityBytes, false); err != nil {
		return "", err
	}
	return session.ID, nil
}

func (nativeOpenCodePromptSessions) Delete(ctx context.Context, server OpenCodeServer, sessionID string) error {
	if err := validateOpenCodeServer(server); err != nil {
		return err
	}
	if err := validateBounded("OpenCode session ID", sessionID, maxIdentityBytes, false); err != nil || !strings.HasPrefix(sessionID, "ses_") {
		return errors.New("invalid OpenCode session ID")
	}
	status, body, err := openCodeSessionRequest(ctx, server, http.MethodDelete, "/session/"+url.PathEscape(sessionID), nil)
	if err != nil || status < 200 || status >= 300 {
		return errors.New("delete OpenCode prompt session failed")
	}
	var deleted bool
	if err := decodeOpenCode(body, &deleted); err != nil || !deleted {
		return errors.New("OpenCode prompt session deletion was not acknowledged")
	}
	return nil
}

func openCodeSessionRequest(ctx context.Context, server OpenCodeServer, method, path string, body []byte) (int, []byte, error) {
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	query := url.Values{"directory": []string{server.Directory}}.Encode()
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(server.URL, "/")+path+"?"+query, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, openCodeResponseBytes+1))
	if err != nil || len(contents) > openCodeResponseBytes {
		return 0, nil, errors.New("OpenCode native response exceeds limit")
	}
	return response.StatusCode, contents, nil
}
