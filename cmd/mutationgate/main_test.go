package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunWritesMachineReadableFailureBeforeReturningError(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "report.json")
	policy := filepath.Join(dir, "policy.json")
	baseline := filepath.Join(dir, "baseline.md")
	config := filepath.Join(dir, "gremlins.yaml")
	output := filepath.Join(dir, "gate.json")
	write := func(path, contents string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(report, `{"go_module":"example","files":[{"file_name":"critical.go","mutations":[{"type":"ARITHMETIC_BASE","status":"LIVED","line":1,"column":1}]}],"test_efficacy":0,"mutations_coverage":100,"mutants_total":1,"mutants_killed":0,"mutants_lived":1,"mutants_not_viable":0,"mutants_not_covered":0,"elapsed_time":1,"mutator_statistics":{"arithmetic_base":1}}`)
	write(policy, `{"critical_paths":[],"critical":{"adjusted_efficacy":100,"mutant_coverage":100},"noncritical":{"adjusted_efficacy":85,"mutant_coverage":90}}`)
	write(baseline, "# no approvals\n")
	write(config, "unleash: {}\n")

	var stdout bytes.Buffer
	err := run([]string{
		"check", "--report", report, "--policy", policy,
		"--baseline", baseline, "--config", config, "--output", output, "--scope", "full",
	}, &stdout)
	if err == nil {
		t.Fatal("failing policy returned nil")
	}
	written, readErr := os.ReadFile(output)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(stdout.String(), `"passed": false`) ||
		!bytes.Equal(stdout.Bytes(), written) {
		t.Fatalf("stdout=%q output=%q", stdout.String(), written)
	}
}
