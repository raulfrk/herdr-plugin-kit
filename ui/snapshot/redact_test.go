package snapshot_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/ui/snapshot"
	"pgregory.net/rapid"
)

type testTB interface {
	Helper()
}

func redactWithin(t testTB, input string) string {
	t.Helper()
	result := make(chan string, 1)
	go func() { result <- snapshot.Redact(input) }()
	select {
	case output := <-result:
		return output
	case <-time.After(time.Second):
		panic("redaction did not terminate for bounded diagnostic input")
	}
}

func TestRedactSensitiveAssignmentsFlagsAndBearer(t *testing.T) {
	input := "api_key = \"abc\\\"suffix\"\nTOKEN=def\n{\"password\": \"with space\"},\nrun --private-key \"key\\\"material\" --safe yes\nAuthorization: Basic opaque\nCookie: session=hidden\nusername=raul"
	redacted := redactWithin(t, input)
	want := "api_key = <redacted>\nTOKEN=<redacted>\n{\"password\": <redacted>},\nrun --private-key <redacted> --safe yes\nAuthorization: <redacted>\nCookie: <redacted>\nusername=raul"
	if redacted != want {
		t.Fatal("multi-format credentials were not redacted exactly")
	}
}

func TestPropertyRedactionIsIdempotentAndRemovesToken(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		length := rapid.IntRange(1, 40).Draw(t, "credential_length")
		secret := fmt.Sprintf("value-%s-end", strings.Repeat("x", length))
		input := "access_token=" + secret
		once := redactWithin(t, input)
		if once != "access_token="+snapshot.Replacement {
			t.Fatal("generated token assignment was not fully redacted")
		}
		if twice := redactWithin(t, once); twice != once {
			t.Fatal("redaction was not idempotent")
		}
	})
}

func TestSensitiveKeyPolicyVariants(t *testing.T) {
	keys := []string{"api-key", "service_api_key", "access_token", "refresh-token", "pass", "passwd", "password", "client_secret", "authorization", "cookie", "ssh_private_key"}
	for _, key := range keys {
		if got := redactWithin(t, key+"=credential"); got != key+"="+snapshot.Replacement {
			t.Errorf("key %q was not redacted", key)
		}
	}
	patterns := snapshot.SensitiveKeyPatterns()
	patterns[0] = "changed"
	if snapshot.SensitiveKeyPatterns()[0] == "changed" {
		t.Fatal("sensitive-key policy was mutable")
	}
}

func TestRedactEscapedUnquotedValuesAndValuelessFlag(t *testing.T) {
	input := "TOKEN=alpha\\ beta\\,gamma\nrun --private-key alpha\\ beta --safe yes\nproxy returned Bearer embedded-value\nBearer first,Bearer second\nanti-bearer public\nanti-bearer x/Bearer actual-secret\nrun --token --safe yes\nrun --token= --safe yes\nrun --token --password secret\nrun --verbose --api-key=secret\nrun --token \"abc def\"suffix\nTOKEN=prefix\"abc def\"suffix; username=raul\nTOKEN=secret&&echo ok\nrun --token=first;--password second\n{\"auth\":\"Bearer abc==\",\"safe\":1}\n{\"authorization\":\"Basic abc\",\"safe\":1}\n{password: secret}\nTOKEN=first request Authorization: Basic opaque; username=raul\ncurl -H 'Authorization: Basic abc;def'\nAuthorization: Basic abc\\;def; echo ok\nAuthorization: <redacted> Basic trailing\nCookie: <redacted> session=trailing\ncurl -H 'Cookie: session=hidden' --url safe\ncurl -H 'Authorization: Basic first' -H 'Authorization: Basic second'\nAuthorization: Basic one; Authorization: Basic two\nlog Cookie: a=one; b=two"
	want := "TOKEN=<redacted>\nrun --private-key <redacted> --safe yes\nproxy returned Bearer <redacted>\nBearer <redacted>,Bearer <redacted>\nanti-bearer public\nanti-bearer x/Bearer <redacted>\nrun --token --safe yes\nrun --token= --safe yes\nrun --token --password <redacted>\nrun --verbose --api-key=<redacted>\nrun --token <redacted>\nTOKEN=<redacted>; username=raul\nTOKEN=<redacted>&&echo ok\nrun --token=<redacted>;--password <redacted>\n{\"auth\":\"Bearer <redacted>\",\"safe\":1}\n{\"authorization\":\"<redacted>\",\"safe\":1}\n{password: <redacted>}\nTOKEN=<redacted> request Authorization: <redacted>; username=raul\ncurl -H 'Authorization: <redacted>'\nAuthorization: <redacted>; echo ok\nAuthorization: <redacted>\nCookie: <redacted>\ncurl -H 'Cookie: <redacted>' --url safe\ncurl -H 'Authorization: <redacted>' -H 'Authorization: <redacted>'\nAuthorization: <redacted>; Authorization: <redacted>\nlog Cookie: <redacted>"
	got := strings.Split(redactWithin(t, input), "\n")
	for index, expected := range strings.Split(want, "\n") {
		if got[index] != expected {
			t.Fatalf("credential form %d was handled incorrectly", index)
		}
	}
	twice := strings.Split(redactWithin(t, redactWithin(t, input)), "\n")
	for index, expected := range strings.Split(want, "\n") {
		if twice[index] != expected {
			t.Fatalf("credential form %d was not idempotent", index)
		}
	}
}

func TestRedactMarkerAdjacencyAndHeaderChains(t *testing.T) {
	input := "TOKEN=<redacted>CANARY\nrun --token=<redacted>CANARY\nBearer <redacted>CANARY==\nprefix--token=public\nCookie: session=hidden; echo visible\nAuthorization: Basic one, Cookie: a=two\nAuthorization: Basic abc, username=raul"
	want := "TOKEN=<redacted>\nrun --token=<redacted>\nBearer <redacted>\nprefix--token=public\nCookie: <redacted>; echo visible\nAuthorization: <redacted>, Cookie: <redacted>\nAuthorization: <redacted>, username=raul"
	if got := redactWithin(t, input); got != want {
		t.Fatal("marker adjacency or chained headers were handled incorrectly")
	}
	if got := redactWithin(t, want); got != want {
		t.Fatal("marker adjacency or chained headers were not idempotent")
	}
}

func TestRedactLeavesUnsupportedPercentBearerUnchanged(t *testing.T) {
	input := "Bearer abc%def"
	if got := redactWithin(t, input); got != input {
		t.Fatalf("unsupported bearer form changed to %q", got)
	}
	if got := redactWithin(t, redactWithin(t, input)); got != input {
		t.Fatalf("unsupported bearer form was not idempotent: %q", got)
	}
}

func TestRedactPreservesSyntaxAtCredentialBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: ""},
		{name: "blank lines", input: "\n\n", want: "\n\n"},
		{name: "empty header value", input: "Authorization:", want: "Authorization:<redacted>"},
		{name: "single quoted header", input: "Authorization: 'Basic abc' tail", want: "Authorization: '<redacted>' tail"},
		{name: "escaped quoted header", input: `Authorization: "Basic a\"b" tail`, want: `Authorization: "<redacted>" tail`},
		{name: "structured comma", input: `{"authorization":"Basic abc","safe":1}`, want: `{"authorization":"<redacted>","safe":1}`},
		{name: "structured brace", input: `{authorization: Basic abc}`, want: `{authorization: <redacted>}`},
		{name: "structured bracket", input: `[authorization: Basic abc]`, want: `[authorization: <redacted>]`},
		{name: "assignment closing brace", input: `{token: secret}`, want: `{token: <redacted>}`},
		{name: "assignment closing bracket", input: `[token: secret]`, want: `[token: <redacted>]`},
		{name: "assignment closing parenthesis", input: `(token=secret)`, want: `(token=<redacted>)`},
		{name: "empty equals flag", input: `run --token=`, want: `run --token=`},
		{name: "flag at start", input: `--token=secret`, want: `--token=<redacted>`},
		{name: "valueless flag at end", input: `run --token`, want: `run --token`},
		{name: "whitespace-only flag value", input: `run --token   `, want: `run --token   `},
		{name: "tab separated flag", input: "run --token\tsecret --safe yes", want: "run --token\t<redacted> --safe yes"},
		{name: "escaped flag value", input: `run --token secret\ value; next`, want: `run --token <redacted>; next`},
		{name: "qualified key", input: `service.token=secret`, want: `service.token=<redacted>`},
		{name: "shortest qualified key", input: `a.token=secret`, want: `a.token=<redacted>`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := redactWithin(t, test.input); got != test.want {
				t.Fatalf("Redact(%q) = %q, want %q", test.input, got, test.want)
			}
			if got := redactWithin(t, test.want); got != test.want {
				t.Fatalf("redacted form is not stable: %q", got)
			}
		})
	}
}

func TestRedactTerminatesOnMalformedQuoting(t *testing.T) {
	for _, input := range []string{
		`Authorization: Basic abc\`,
		`run --token "secret`,
		`run --token secret\`,
		`run --token "secret\`,
	} {
		redactWithin(t, input)
	}
}

func TestRedactRequiresBearerAndFlagTokenBoundaries(t *testing.T) {
	wordBytes := []byte{'-', '_', '0', '9', 'A', 'Z', 'a', 'z'}
	for _, prefix := range wordBytes {
		for _, credential := range []string{"Bearer secret", "--token=secret"} {
			input := string(prefix) + credential
			if got := redactWithin(t, input); got != input {
				t.Errorf("word byte %q incorrectly started %q: %q", prefix, credential, got)
			}
		}
	}

	for _, prefix := range []byte{'.', '/', ':', ' '} {
		input := string(prefix) + "Bearer secret"
		want := string(prefix) + "Bearer " + snapshot.Replacement
		if got := redactWithin(t, input); got != want {
			t.Errorf("separator %q did not start a bearer credential: %q", prefix, got)
		}
	}
}
