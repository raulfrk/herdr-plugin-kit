package herdrapi

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientRoundTrip(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		var request struct {
			ID     string `json:"id"`
			Method string `json:"method"`
			Params struct {
				Value string `json:"value"`
			} `json:"params"`
		}
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			done <- err
			return
		}
		_, err = conn.Write([]byte(`{"id":"test-1","result":{"value":"ok"}}` + "\n"))
		done <- err
	}()
	var result struct {
		Value string `json:"value"`
	}
	if err := (Client{}).Call(context.Background(), socket, "test-1", "test.method", struct {
		Value string `json:"value"`
	}{"literal"}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Value != "ok" {
		t.Fatalf("result = %+v", result)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestClientRejectsProtocolDriftAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, response string
	}{
		{"wrong ID", `{"id":"other","result":true}` + "\n"},
		{"error", `{"id":"test","error":{"message":"no"}}` + "\n"},
		{"unknown envelope field", `{"id":"test","result":true,"extra":1}` + "\n"},
		{"missing result", `{"id":"test"}` + "\n"},
		{"multiple values", `{"id":"test","result":true} {}` + "\n"},
		{"oversize", strings.Repeat(" ", MaxMessageBytes+1) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "herdr.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				var request any
				_ = json.NewDecoder(conn).Decode(&request)
				_, _ = conn.Write([]byte(tc.response))
			}()
			var result bool
			if err := (Client{}).Call(context.Background(), socket, "test", "method", struct{}{}, &result); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

func TestClientRejectsUnsafeInputBeforeDial(t *testing.T) {
	for _, socket := range []string{"", "relative", "/tmp/../tmp/sock", "/tmp/bad\x00sock"} {
		if err := (Client{}).Call(context.Background(), socket, "id", "method", struct{}{}, new(bool)); err == nil {
			t.Fatalf("socket %q accepted", socket)
		}
	}
	if err := (Client{}).Call(context.Background(), "/tmp/missing.sock", "bad\n", "method", struct{}{}, new(bool)); err == nil {
		t.Fatal("invalid request ID accepted")
	}
}
