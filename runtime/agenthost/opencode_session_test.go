package agenthost

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenCodePromptSessionCreateAndDeleteExactContract(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Query().Get("directory") != "/tmp/project" || request.Header.Get("Authorization") != "" {
			http.Error(response, "unexpected request", http.StatusBadRequest)
			return
		}
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/session" && request.Header.Get("Content-Type") == "application/json":
			_, _ = response.Write([]byte(`{"id":"ses_created","directory":"/tmp/project","parentID":null,"title":"ignored"}`))
		case request.Method == http.MethodDelete && request.URL.Path == "/session/ses_created":
			_, _ = response.Write([]byte("true"))
		default:
			http.Error(response, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	configured := OpenCodeServer{URL: server.URL, Directory: "/tmp/project"}
	sessions := nativeOpenCodePromptSessions{}
	id, err := sessions.Create(context.Background(), configured)
	if err != nil || id != "ses_created" {
		t.Fatalf("id=%q error=%v", id, err)
	}
	if err := sessions.Delete(context.Background(), configured, id); err != nil || requests != 2 {
		t.Fatalf("requests=%d error=%v", requests, err)
	}
}

func TestOpenCodeNativeForkExactContractAndUncertainResponses(t *testing.T) {
	const sourceID = "ses_source"
	for _, test := range []struct {
		name       string
		status     int
		body       string
		wantChild  string
		wantErrors bool
	}{
		{name: "valid", status: http.StatusOK, body: `{"id":"ses_child","directory":"/tmp/project","parentID":null}`, wantChild: "ses_child"},
		{name: "same identity", status: http.StatusOK, body: `{"id":"ses_source","directory":"/tmp/project"}`, wantErrors: true},
		{name: "wrong directory", status: http.StatusOK, body: `{"id":"ses_child","directory":"/tmp/other"}`, wantErrors: true},
		{name: "malformed", status: http.StatusOK, body: `{"id":"ses_child"} {}`, wantErrors: true},
		{name: "empty success", status: http.StatusNoContent, wantErrors: true},
		{name: "server error after possible creation", status: http.StatusInternalServerError, wantErrors: true},
		{name: "rejected response not proof of no effect", status: http.StatusBadRequest, wantErrors: true},
		{name: "redirect not followed", status: http.StatusFound, wantErrors: true},
		{name: "oversized", status: http.StatusOK, body: strings.Repeat("x", openCodeResponseBytes+1), wantErrors: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				requests++
				if request.Method != http.MethodPost || request.URL.Path != "/session/"+sourceID+"/fork" || request.URL.Query().Get("directory") != "/tmp/project" || request.Header.Get("Content-Type") != "application/json" {
					t.Errorf("unexpected native fork request: %s %s", request.Method, request.URL)
				}
				if test.status == http.StatusFound {
					response.Header().Set("Location", "http://example.com/elsewhere")
				}
				response.WriteHeader(test.status)
				_, _ = response.Write([]byte(test.body))
			}))
			defer server.Close()
			child, err := forkOpenCodeSession(context.Background(), OpenCodeServer{URL: server.URL, Directory: "/tmp/project"}, sourceID)
			if requests != 1 || child != test.wantChild || (test.wantErrors && !errors.Is(err, ErrForkOutcomeUncertain)) || (!test.wantErrors && err != nil) {
				t.Fatalf("requests=%d child=%q error=%v", requests, child, err)
			}
		})
	}
}

func TestOpenCodeNativeForkPreflightAndCanceledRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
	}))
	defer server.Close()
	defer close(release)
	configured := OpenCodeServer{URL: server.URL, Directory: "/tmp/project"}
	if _, err := forkOpenCodeSession(context.Background(), configured, "bad"); err == nil {
		t.Fatal("invalid source ID reached native API")
	}
	if _, err := forkOpenCodeSession(context.Background(), OpenCodeServer{URL: "http://example.com:1234", Directory: "/tmp/project"}, "ses_source"); err == nil {
		t.Fatal("nonloopback source server reached native API")
	}
	preCanceled, cancelPre := context.WithCancel(context.Background())
	cancelPre()
	if _, err := forkOpenCodeSession(preCanceled, configured, "ses_source"); !errors.Is(err, context.Canceled) || errors.Is(err, ErrForkOutcomeUncertain) {
		t.Fatalf("pre-dispatch cancellation=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := forkOpenCodeSession(ctx, configured, "ses_source")
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("native fork request was not dispatched")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrForkOutcomeUncertain) || !errors.Is(err, context.Canceled) {
			t.Fatalf("dispatched cancellation=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled native fork request did not return")
	}
}

func TestOpenCodePromptSessionRejectsProtocolDrift(t *testing.T) {
	for name, responseBody := range map[string]string{
		"wrong id":        `{"id":"bad","directory":"/tmp/project","parentID":null}`,
		"wrong directory": `{"id":"ses_created","directory":"/tmp/other","parentID":null}`,
		"parent":          `{"id":"ses_created","directory":"/tmp/project","parentID":"ses_parent"}`,
		"multiple values": `{"id":"ses_created","directory":"/tmp/project","parentID":null} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(response, responseBody)
			}))
			defer server.Close()
			if _, err := (nativeOpenCodePromptSessions{}).Create(context.Background(), OpenCodeServer{URL: server.URL, Directory: "/tmp/project"}); err == nil {
				t.Fatal("protocol drift accepted")
			}
		})
	}
}

func TestOpenCodePromptSessionRejectsInformationalCreateResponse(t *testing.T) {
	const body = `{"id":"ses_created","directory":"/tmp/project","parentID":null}`
	server := openCodeSwitchingProtocolServer(t, body)
	configured := OpenCodeServer{URL: server.URL, Directory: "/tmp/project"}
	status, got, err := openCodeSessionRequest(context.Background(), configured, http.MethodPost, "/session", []byte("{}"))
	if err != nil || status != http.StatusSwitchingProtocols || string(got) != body {
		t.Fatalf("switching response status=%d body=%q error=%v", status, got, err)
	}
	if _, err := (nativeOpenCodePromptSessions{}).Create(context.Background(), configured); err == nil {
		t.Fatal("informational create response was accepted")
	}
}

func openCodeSwitchingProtocolServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
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
		if _, err := fmt.Fprintf(buffered, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: hpk-test\r\n\r\n%s", body); err != nil {
			t.Errorf("write switching response: %v", err)
			return
		}
		if err := buffered.Flush(); err != nil {
			t.Errorf("flush switching response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}
