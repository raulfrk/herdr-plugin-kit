package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunStartsLiveOrExportsFilteredCatalogue(t *testing.T) {
	live := false
	if err := runWithLive(nil, &bytes.Buffer{}, func() error { live = true; return nil }); err != nil || !live {
		t.Fatalf("live = %t, err = %v", live, err)
	}
	if err := runWithLive([]string{"--theme", "nord"}, &bytes.Buffer{}, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "require --output") {
		t.Fatalf("selector error = %v", err)
	}
	output := filepath.Join(t.TempDir(), "review")
	var stdout bytes.Buffer
	if err := run([]string{"--output", output, "--theme", "vesper", "--viewport", "phone-keyboard", "--scenario", "error"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "1 images, 1 contact sheets") {
		t.Fatal(stdout.String())
	}
	if _, err := os.Stat(filepath.Join(output, "bento-command", "vesper", "phone-keyboard", "error.png")); err != nil {
		t.Fatal(err)
	}
}
