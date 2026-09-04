package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMainReportsErrorAndExits(t *testing.T) {
	const childEnv = "HERDR_PLUGIN_KIT_TEST_MAIN_ERROR"
	if os.Getenv(childEnv) == "1" {
		os.Args = []string{os.Args[0]}
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMainReportsErrorAndExits$")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	output, err := cmd.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
		t.Fatalf("main exit = %v, output = %q", err, output)
	}
	if !strings.Contains(string(output), "usage: herdr-plugin-kit new|validate") {
		t.Fatalf("main output = %q", output)
	}
}

func TestRunGeneratesThenValidates(t *testing.T) {
	output := filepath.Join(t.TempDir(), "plugin")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"new", "--id", "example.cli", "--name", "CLI", "--output", output}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "generated ") || stderr.Len() != 0 {
		t.Fatalf("new output = %q, stderr = %q", stdout.String(), stderr.String())
	}
	stdout.Reset()
	if err := run([]string{"validate", output}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "valid ") {
		t.Fatalf("validate output = %q", stdout.String())
	}
}

func TestRunRejectsUnknownAndMalformedCommands(t *testing.T) {
	for name, args := range map[string][]string{
		"empty": {}, "unknown": {"unknown"}, "new positional": {"new", "extra"},
		"validate missing": {"validate"}, "validate extra": {"validate", "one", "two"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
				t.Fatal("malformed command succeeded")
			}
		})
	}
}
