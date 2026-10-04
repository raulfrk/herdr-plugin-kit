package agenthost

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

func TestDeriveOMPStatusSocketAcceptsIdentityAndBasenameLimits(t *testing.T) {
	longest := strings.Repeat("p", maxIdentityBytes)
	if got, err := DeriveOMPStatusSocket("/tmp/herdr.sock", longest); err != nil || got == "" {
		t.Fatalf("pane identity of the maximum length: socket=%q error=%v", got, err)
	}
	// The shortest basename that leaves room for the "o" prefix and one digest character.
	const herdrSocket = "/tmp/abcdefghi"
	got, err := DeriveOMPStatusSocket(herdrSocket, "w1:p1")
	if err != nil || filepath.Dir(got) != "/tmp" || len(filepath.Base(got)) != len(filepath.Base(herdrSocket)) || !strings.HasPrefix(filepath.Base(got), "o") {
		t.Fatalf("socket=%q error=%v", got, err)
	}
}

func TestDerivePromptOMPStatusSocketUsesRunningSessionSocket(t *testing.T) {
	const herdrSocket = "/home/raul/.config/herdr/sessions/hpk-api5-prompt-live/herdr.sock"
	runner := &scriptedRunner{results: []command.Result{{Stdout: []byte(`{"sessions":[{"name":"hpk-api5-prompt-live","default":false,"running":true,"session_dir":"/home/raul/.config/herdr/sessions/hpk-api5-prompt-live","socket_path":"` + herdrSocket + `"}]}`)}}}
	got, err := derivePromptOMPStatusSocket(context.Background(), Host{Runner: runner, Session: "hpk-api5-prompt-live"}, "w1:p3")
	want := "/home/raul/.config/herdr/sessions/hpk-api5-prompt-live/olFqKETBRT"
	if err != nil || got != want {
		t.Fatalf("socket=%q want=%q error=%v", got, want, err)
	}
}
