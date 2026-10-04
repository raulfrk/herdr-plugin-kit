package snapshot

import (
	"math/rand"
	"strings"
	"testing"
)

// unguardedRedact preserves the original matching path as an oracle for the
// per-line fast path. This checks that skipped matchers cannot change output.
func unguardedRedact(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		line = redactHeaders(line)
		line = redactBearer(line)
		line = redactFlags(line)
		lines[i] = redactMatches(line, assignment)
	}
	return strings.Join(lines, "\n")
}

func TestRedactMatchesUnguardedPipeline(t *testing.T) {
	components := []string{
		"session", "foo", "keymap", "api_key", "APIKEY", "token", "secret", "pass", "password",
		"authorization", "cookie", "private-key", "Bearer", "bearer", "-", "--", "=", ":", " ", "\t",
		"\n", "'", "\"", "<redacted>", "value", "\u017f", "\u212a", "漢", "◈", "\\", "/", ";", ",",
	}
	for _, input := range []string{
		"authori\u017fation: secret", "coo\u212aie: secret", "--api_key=secret", "Bearer token",
		"token: \"secret\"", "private-key=value", "foo:bar --token value", "\u017fecret=value",
	} {
		if got, want := redactBoundedCall(t, Redact, input), redactBoundedCall(t, unguardedRedact, input); got != want {
			t.Fatalf("input=%q got=%q want=%q", input, got, want)
		}
	}
	rng := rand.New(rand.NewSource(230923))
	for i := 0; i < 20000; i++ {
		var input strings.Builder
		for j := rng.Intn(12); j >= 0; j-- {
			input.WriteString(components[rng.Intn(len(components))])
		}
		value := input.String()
		if got, want := redactBoundedCall(t, Redact, value), redactBoundedCall(t, unguardedRedact, value); got != want {
			t.Fatalf("case %d input=%q got=%q want=%q", i, value, got, want)
		}
	}
}
