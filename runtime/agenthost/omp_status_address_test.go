package agenthost

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

func TestDeriveOMPStatusSocketMatchesSupportedCompanionVector(t *testing.T) {
	herdr := "/home/raul/.config/herdr/sessions/hpk-api5-prompt-live/herdr.sock"
	got, err := DeriveOMPStatusSocket(herdr, "w1:p3")
	want := "/home/raul/.config/herdr/sessions/hpk-api5-prompt-live/olFqKETBRT"
	if err != nil || got != want || len(got) != len(herdr) {
		t.Fatalf("socket=%q length=%d error=%v", got, len(got), err)
	}
}

func TestDerivePromptOMPStatusSocketPropagatesHerdrSocketFailure(t *testing.T) {
	runner := &scriptedRunner{results: []command.Result{{}}, errs: []error{errors.New("session unavailable")}}
	if socket, err := derivePromptOMPStatusSocket(context.Background(), Host{Runner: runner, Session: "named"}, "w1:p1"); err == nil || socket != "" {
		t.Fatalf("socket=%q error=%v", socket, err)
	}
}

func TestDeriveOMPStatusSocketRejectsInvalidIdentity(t *testing.T) {
	for _, input := range [][2]string{
		{"relative/herdr.sock", "w1:p1"},
		{"/tmp/x", "w1:p1"},
		{"/tmp/herdr.sock", ""},
		{"/tmp/herdr.sock", "bad\x00pane"},
		{"/tmp/herdr.sock", strings.Repeat("p", maxIdentityBytes+1)},
	} {
		if got, err := DeriveOMPStatusSocket(input[0], input[1]); err == nil || got != "" {
			t.Fatalf("input=%q socket=%q error=%v", input, got, err)
		}
	}
}

func TestOMPStatusAddressExtensionReturnsFreshCopy(t *testing.T) {
	first := OMPStatusAddressExtension()
	second := OMPStatusAddressExtension()
	if len(first) == 0 || !bytes.Equal(first, second) {
		t.Fatal("embedded address extension is missing or unstable")
	}
	first[0] ^= 0xff
	if bytes.Equal(first, OMPStatusAddressExtension()) {
		t.Fatal("embedded address extension did not return a fresh copy")
	}
}
