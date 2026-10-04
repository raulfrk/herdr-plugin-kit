package agenthost

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOpenCodeSessionForkRejectsUncertainSuccessResponses(t *testing.T) {
	const child = `{"id":"ses_child","directory":"/tmp/project"}`
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"server error with a valid child", http.StatusInternalServerError, child},
		{"multiple choices with a valid child", http.StatusMultipleChoices, child},
		{"valid child followed by another value", http.StatusOK, child + ` {}`},
		{"child without the session prefix", http.StatusOK, `{"id":"child","directory":"/tmp/project"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(test.status)
				_, _ = response.Write([]byte(test.body))
			}))
			defer server.Close()
			got, err := forkOpenCodeSession(context.Background(), OpenCodeServer{URL: server.URL, Directory: "/tmp/project"}, "ses_source")
			if got != "" || !errors.Is(err, ErrForkOutcomeUncertain) {
				t.Fatalf("child=%q error=%v", got, err)
			}
		})
	}
}

func TestOpenCodePromptSessionCreateRejectsMultipleChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusMultipleChoices)
		_, _ = response.Write([]byte(`{"id":"ses_created","directory":"/tmp/project","parentID":null}`))
	}))
	defer server.Close()
	if id, err := (nativeOpenCodePromptSessions{}).Create(context.Background(), OpenCodeServer{URL: server.URL, Directory: "/tmp/project"}); err == nil {
		t.Fatalf("multiple-choices create response accepted: %q", id)
	}
}

func TestOpenCodePromptSessionDeleteRejectsInvalidSessionIDWithoutRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = response.Write([]byte("true"))
	}))
	defer server.Close()
	configured := OpenCodeServer{URL: server.URL, Directory: "/tmp/project"}
	for name, id := range map[string]string{
		"missing session prefix": "bad",
		"oversized identity":     "ses_" + strings.Repeat("a", maxIdentityBytes),
		"identity with newline":  "ses_a\nb",
	} {
		if err := (nativeOpenCodePromptSessions{}).Delete(context.Background(), configured, id); err == nil {
			t.Errorf("%s: invalid session ID accepted", name)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid session IDs reached the native API %d times", requests.Load())
	}
	if err := (nativeOpenCodePromptSessions{}).Delete(context.Background(), configured, "ses_valid"); err != nil || requests.Load() != 1 {
		t.Fatalf("valid session ID requests=%d error=%v", requests.Load(), err)
	}
}

func TestOpenCodePromptSessionDeleteRequiresSuccessStatusAndAcknowledgement(t *testing.T) {
	deleteWith := func(t *testing.T, server *httptest.Server) error {
		t.Helper()
		return (nativeOpenCodePromptSessions{}).Delete(context.Background(), OpenCodeServer{URL: server.URL, Directory: "/tmp/project"}, "ses_created")
	}
	t.Run("informational status", func(t *testing.T) {
		if err := deleteWith(t, openCodeSwitchingProtocolServer(t, "true")); err == nil {
			t.Fatal("informational delete response accepted")
		}
	})
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"server error", http.StatusInternalServerError, "true"},
		{"multiple choices", http.StatusMultipleChoices, "true"},
		{"not acknowledged", http.StatusOK, "false"},
		{"acknowledgement followed by another value", http.StatusOK, "true {}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(test.status)
				_, _ = response.Write([]byte(test.body))
			}))
			defer server.Close()
			if err := deleteWith(t, server); err == nil {
				t.Fatal("invalid delete response accepted")
			}
		})
	}
}

func TestOpenCodeSessionRequestEnforcesResponseSizeLimitAndCompleteBodies(t *testing.T) {
	stub := openCodeStubServer(t, map[string]openCodeStubResponse{
		"/exact":     {body: strings.Repeat(" ", openCodeResponseBytes)},
		"/oversized": {body: strings.Repeat(" ", openCodeResponseBytes+1)},
		"/truncated": {raw: openCodeStubTruncatedBody},
	})
	configured := OpenCodeServer{URL: stub.URL, Directory: "/tmp/project"}
	status, body, err := openCodeSessionRequest(context.Background(), configured, http.MethodGet, "/exact", nil)
	if err != nil || status != http.StatusOK || len(body) != openCodeResponseBytes {
		t.Fatalf("exact-limit response status=%d length=%d error=%v", status, len(body), err)
	}
	for _, path := range []string{"/oversized", "/truncated"} {
		if status, body, err := openCodeSessionRequest(context.Background(), configured, http.MethodGet, path, nil); err == nil {
			t.Fatalf("%s accepted: status=%d length=%d", path, status, len(body))
		}
	}
}
