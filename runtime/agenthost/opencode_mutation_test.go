package agenthost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// openCodeStubResponse is the canned answer an openCodeStubServer gives for one
// request path. A non-empty raw is written to the hijacked connection verbatim,
// which lets a test send informational or truncated HTTP responses.
type openCodeStubResponse struct {
	status int
	body   string
	raw    string
}

const openCodeStubSession = `{"id":"session-1","directory":"/tmp/project"}`

func openCodeStubServer(t *testing.T, routes map[string]openCodeStubResponse) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		stub, ok := routes[request.URL.Path]
		if !ok {
			t.Errorf("unexpected request path %q", request.URL.Path)
			response.WriteHeader(http.StatusNotFound)
			return
		}
		if stub.raw != "" {
			hijacker, ok := response.(http.Hijacker)
			if !ok {
				t.Error("test server does not support connection hijacking")
				return
			}
			connection, buffered, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack response: %v", err)
				return
			}
			defer connection.Close()
			if _, err := fmt.Fprint(buffered, stub.raw); err != nil {
				t.Errorf("write raw response: %v", err)
				return
			}
			if err := buffered.Flush(); err != nil {
				t.Errorf("flush raw response: %v", err)
			}
			return
		}
		if stub.status != 0 {
			response.WriteHeader(stub.status)
		}
		_, _ = io.WriteString(response, stub.body)
	}))
	t.Cleanup(server.Close)
	return server
}

// openCodeStubRoutes answers an idle, unblocked "session-1" and applies the
// per-path overrides on top of that.
func openCodeStubRoutes(overrides map[string]openCodeStubResponse) map[string]openCodeStubResponse {
	routes := map[string]openCodeStubResponse{
		"/session/session-1": {body: openCodeStubSession},
		"/question":          {body: `[]`},
		"/permission":        {body: `[]`},
		"/session/status":    {body: `{}`},
	}
	for path, stub := range overrides {
		routes[path] = stub
	}
	return routes
}

const (
	openCodeStubSwitchingProtocols = "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: hpk-test\r\n\r\n"
	openCodeStubTruncatedBody      = "HTTP/1.1 200 OK\r\nContent-Length: 4096\r\n\r\ntruncated"
)

func TestOpenCodeReadStatusRejectsInvalidNativeResponses(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overrides map[string]openCodeStubResponse
		valid     bool
	}{
		{name: "valid", valid: true},
		{name: "session multiple choices", overrides: map[string]openCodeStubResponse{"/session/session-1": {status: 300, body: openCodeStubSession}}},
		{name: "session server error", overrides: map[string]openCodeStubResponse{"/session/session-1": {status: 500, body: openCodeStubSession}}},
		{name: "session without id", overrides: map[string]openCodeStubResponse{"/session/session-1": {body: `{"directory":"/tmp/project"}`}}},
		{name: "session without directory", overrides: map[string]openCodeStubResponse{"/session/session-1": {body: `{"id":"session-1"}`}}},
		{name: "status informational response", overrides: map[string]openCodeStubResponse{"/session/status": {raw: openCodeStubSwitchingProtocols + `{}`}}},
		{name: "status server error", overrides: map[string]openCodeStubResponse{"/session/status": {status: 500, body: `{}`}}},
		{name: "status multiple choices", overrides: map[string]openCodeStubResponse{"/session/status": {status: 300, body: `{}`}}},
		{name: "status null object", overrides: map[string]openCodeStubResponse{"/session/status": {body: `null`}}},
		{name: "status with trailing value", overrides: map[string]openCodeStubResponse{"/session/status": {body: `{} {}`}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := openCodeStubServer(t, openCodeStubRoutes(tc.overrides))
			got, err := readOpenCodeStatus(context.Background(), OpenCodeServer{URL: server.URL, Directory: "/tmp/project"}, "session-1", false)
			if tc.valid {
				if err != nil || got != (Assessment{Status: Idle, Reason: OpenCodeStatus, Stable: true}) {
					t.Fatalf("assessment=%+v error=%v", got, err)
				}
				return
			}
			if err == nil || got != (Assessment{}) {
				t.Fatalf("assessment=%+v error=%v, want an error", got, err)
			}
			if errors.Is(err, ErrStaleReport) {
				t.Fatalf("protocol failure reported as a stale report: %v", err)
			}
		})
	}
}

func TestOpenCodePendingRejectsInvalidNativeResponses(t *testing.T) {
	const target = `[{"sessionID":"ses_target"}]`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
		fail   bool
	}{
		{name: "matching session", status: http.StatusOK, body: target, want: true},
		{name: "other session", status: http.StatusOK, body: `[{"sessionID":"ses_other"}]`},
		{name: "multiple choices", status: http.StatusMultipleChoices, body: target, fail: true},
		{name: "null list", status: http.StatusOK, body: `null`, fail: true},
		{name: "trailing value", status: http.StatusOK, body: target + ` []`, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: openCodeResponseRoundTripper(func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: tc.status,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(tc.body)),
					Request:    request,
				}, nil
			})}
			matched, err := openCodePending(context.Background(), client, "http://example.invalid/pending", "ses_target")
			if (err != nil) != tc.fail || matched != tc.want {
				t.Fatalf("matched=%t error=%v", matched, err)
			}
		})
	}
}

func TestOpenCodeGETEnforcesResponseSizeLimitAndCompleteBodies(t *testing.T) {
	server := openCodeStubServer(t, map[string]openCodeStubResponse{
		"/exact":     {body: strings.Repeat(" ", openCodeResponseBytes)},
		"/oversized": {body: strings.Repeat(" ", openCodeResponseBytes+1)},
		"/truncated": {raw: openCodeStubTruncatedBody},
	})
	status, body, err := openCodeGET(context.Background(), server.Client(), server.URL+"/exact")
	if err != nil || status != http.StatusOK || len(body) != openCodeResponseBytes {
		t.Fatalf("exact-limit response status=%d length=%d error=%v", status, len(body), err)
	}
	for _, path := range []string{"/oversized", "/truncated"} {
		if status, body, err := openCodeGET(context.Background(), server.Client(), server.URL+path); err == nil {
			t.Fatalf("%s accepted: status=%d length=%d", path, status, len(body))
		}
	}
}
