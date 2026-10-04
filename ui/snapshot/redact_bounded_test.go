package snapshot

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// redactCallDeadline is far above the cost of redacting any test input, so
// only a call that never returns can reach it.
const redactCallDeadline = 10 * time.Second

// redactBoundedCall applies transform to input and fails the test, instead of
// hanging the whole test binary, when the call does not return in time. The
// scanning loops advance by index arithmetic, so a mistake there spins forever.
func redactBoundedCall(t *testing.T, transform func(string) string, input string) string {
	t.Helper()
	result := make(chan string, 1)
	go func() { result <- transform(input) }()
	timer := time.NewTimer(redactCallDeadline)
	defer timer.Stop()
	select {
	case output := <-result:
		return output
	case <-timer.C:
		t.Fatalf("redaction of %q did not terminate within %s", input, redactCallDeadline)
		return ""
	}
}

// This test is named and placed so that it runs before the unguarded-pipeline
// comparison: a scanner that stops advancing is reported here, quickly.
func TestRedactTerminatesOnEscapedHeaderValuesAndPrefixedWords(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`Authorization: Basic abc\;def; echo ok`, `Authorization: <redacted>; echo ok`},
		{`Cookie: a\=b; echo ok`, `Cookie: <redacted>; echo ok`},
		{`xBearer secret`, `xBearer secret`},
		{`x--token=secret`, `x--token=secret`},
	} {
		if got := redactBoundedCall(t, Redact, tc.input); got != tc.want {
			t.Errorf("Redact(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// Redact looks only at lines that mention a credential keyword. A policy that
// names a key without any such keyword shows which matchers are gated.
func TestRedactSkipsMatchersForLinesWithoutCredentialKeyword(t *testing.T) {
	patterns, header := sensitiveKeyPatterns, sensitiveHeader
	t.Cleanup(func() { sensitiveKeyPatterns, sensitiveHeader = patterns, header })
	sensitiveKeyPatterns = compilePatterns([]string{`(?i)^zzz$`})
	sensitiveHeader = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_-])((zzz)["']?[\t ]*:[\t ]*)`)

	for _, input := range []string{"zzz=1", "zzz: 1", "run --zzz 1"} {
		if redactBoundedCall(t, unguardedRedact, input) == input {
			t.Fatalf("the ungated pipeline leaves %q unchanged, so this test proves nothing", input)
		}
		if got := redactBoundedCall(t, Redact, input); got != input {
			t.Errorf("Redact(%q) = %q, want the line unchanged", input, got)
		}
		hinted := "token " + input
		if got := redactBoundedCall(t, Redact, hinted); !strings.Contains(got, Replacement) {
			t.Errorf("Redact(%q) = %q, want the keyword line to be matched", hinted, got)
		}
	}
}
