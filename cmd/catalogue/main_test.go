package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunStartsLiveCatalogueOrExportsFilteredReviewSet(t *testing.T) {
	liveCalled := false
	if err := runWithLive(nil, &bytes.Buffer{}, func() error { liveCalled = true; return nil }); err != nil || !liveCalled {
		t.Fatalf("live run: called=%t error=%v", liveCalled, err)
	}
	if err := runWithLive([]string{"--theme", "nord"}, &bytes.Buffer{}, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "require --output") {
		t.Fatalf("selector without output error = %v", err)
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

func TestCatalogueDiagnosticsDirectoryUsesAbsoluteXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/review-state")
	directory, err := catalogueDiagnosticsDirectory()
	if err != nil || directory != "/tmp/review-state/herdr-plugin-kit/catalogue" {
		t.Fatalf("directory = %q, error=%v", directory, err)
	}
	t.Setenv("XDG_STATE_HOME", "relative")
	if _, err := catalogueDiagnosticsDirectory(); err == nil {
		t.Fatal("relative XDG_STATE_HOME accepted")
	}
}
