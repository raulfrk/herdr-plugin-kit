package snapshot_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/ui/snapshot"
	"pgregory.net/rapid"
)

func TestRedactSensitiveAssignmentsFlagsAndBearer(t *testing.T) {
	input := "api_key = \"abc\\\"suffix\"\nTOKEN=def\n{\"password\": \"with space\"},\nrun --private-key \"key\\\"material\" --safe yes\nAuthorization: Basic opaque\nCookie: session=hidden\nusername=raul"
	redacted := snapshot.Redact(input)
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
		once := snapshot.Redact(input)
		if once != "access_token="+snapshot.Replacement {
			t.Fatal("generated token assignment was not fully redacted")
		}
		if twice := snapshot.Redact(once); twice != once {
			t.Fatal("redaction was not idempotent")
		}
	})
}

func TestSensitiveKeyPolicyVariants(t *testing.T) {
	keys := []string{"api-key", "service_api_key", "access_token", "refresh-token", "pass", "passwd", "password", "client_secret", "authorization", "cookie", "ssh_private_key"}
	for _, key := range keys {
		if got := snapshot.Redact(key + "=credential"); got != key+"="+snapshot.Replacement {
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
	got := strings.Split(snapshot.Redact(input), "\n")
	for index, expected := range strings.Split(want, "\n") {
		if got[index] != expected {
			t.Fatalf("credential form %d was handled incorrectly", index)
		}
	}
	twice := strings.Split(snapshot.Redact(snapshot.Redact(input)), "\n")
	for index, expected := range strings.Split(want, "\n") {
		if twice[index] != expected {
			t.Fatalf("credential form %d was not idempotent", index)
		}
	}
}

func TestRedactMarkerAdjacencyAndHeaderChains(t *testing.T) {
	input := "TOKEN=<redacted>CANARY\nrun --token=<redacted>CANARY\nBearer <redacted>CANARY==\nprefix--token=public\nCookie: session=hidden; echo visible\nAuthorization: Basic one, Cookie: a=two\nAuthorization: Basic abc, username=raul"
	want := "TOKEN=<redacted>\nrun --token=<redacted>\nBearer <redacted>\nprefix--token=public\nCookie: <redacted>; echo visible\nAuthorization: <redacted>, Cookie: <redacted>\nAuthorization: <redacted>, username=raul"
	if got := snapshot.Redact(input); got != want {
		t.Fatal("marker adjacency or chained headers were handled incorrectly")
	}
	if got := snapshot.Redact(want); got != want {
		t.Fatal("marker adjacency or chained headers were not idempotent")
	}
}

func TestRedactLeavesUnsupportedPercentBearerUnchanged(t *testing.T) {
	input := "Bearer abc%def"
	if got := snapshot.Redact(input); got != input {
		t.Fatalf("unsupported bearer form changed to %q", got)
	}
	if got := snapshot.Redact(snapshot.Redact(input)); got != input {
		t.Fatalf("unsupported bearer form was not idempotent: %q", got)
	}
}
