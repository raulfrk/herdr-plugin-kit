package herdrid

import (
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
