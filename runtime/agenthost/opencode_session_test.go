package agenthost

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
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
