package herdrid

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidateSessionName(t *testing.T) {
	for _, name := range []string{"default", "A.b-c_1", strings.Repeat("a", 64)} {
		if err := ValidateSessionName(name); err != nil {
			t.Fatalf("valid %q: %v", name, err)
		}
	}
	for _, name := range []string{"", ".", "..", "../bad", strings.Repeat("a", 65), "bad\x00name"} {
		if err := ValidateSessionName(name); err == nil {
			t.Fatalf("invalid %q accepted", name)
		}
	}
}

func TestArgsMakesDefaultAndNamedRoutingExplicit(t *testing.T) {
	for _, test := range []struct {
		session string
		want    []string
	}{
		{"", []string{"--session", "default", "agent", "list"}},
		{"work", []string{"--session", "work", "agent", "list"}},
	} {
		got, err := Args(test.session, []string{"agent", "list"})
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("session=%q args=%q err=%v", test.session, got, err)
		}
	}
	for _, invalid := range []string{"default", "../bad"} {
		if _, err := Args(invalid, nil); err == nil {
			t.Fatalf("session=%q accepted", invalid)
		}
	}
}
