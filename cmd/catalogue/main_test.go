package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRequiresCallerSelectedOutputAndExportsFilteredReviewSet(t *testing.T) {
	if err := run(nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--output") {
		t.Fatalf("missing output error = %v", err)
	}
	out := filepath.Join(t.TempDir(), "review")
	var stdout bytes.Buffer
	err := run([]string{"--output", out, "--design", "calm-cards", "--theme", "vesper", "--viewport", "phone-keyboard", "--scenario", "error"}, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "1 images, 1 contact sheets") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(out, "calm-cards", "vesper", "phone-keyboard", "error.png")); err != nil {
		t.Fatal(err)
	}
}
