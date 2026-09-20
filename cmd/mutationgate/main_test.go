package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const validGremlinsConfig = `unleash:
  integration: false
  workers: 0
  timeout-coefficient: 3
  threshold:
    efficacy: 0
    mutant-coverage: 0
  exclude-files:
    - '(^|/)vendor/'
    - '_generated[.]go$'
    - '[.]gen[.]go$'
    - '[.]pb[.]go$'
mutants:
  arithmetic-base: {enabled: true}
  conditionals-boundary: {enabled: true}
  conditionals-negation: {enabled: true}
  increment-decrement: {enabled: true}
  invert-assignments: {enabled: false}
  invert-bitwise: {enabled: false}
  invert-bwassign: {enabled: false}
  invert-negatives: {enabled: true}
  invert-logical: {enabled: true}
  invert-loopctrl: {enabled: false}
  remove-self-assignments: {enabled: false}
`

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
	write(config, validGremlinsConfig)

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

func TestRunUsesFullBaselineAsRegressionGate(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	report := filepath.Join(dir, "report.json")
	policy := filepath.Join(dir, "policy.json")
	equivalents := filepath.Join(dir, "equivalents.md")
	config := filepath.Join(dir, "gremlins.yaml")
	fullBaseline := filepath.Join(dir, "full-baseline.json")
	write := func(path, contents string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(report, `{"go_module":"example","files":[{"file_name":"critical.go","mutations":[{"type":"ARITHMETIC_BASE","status":"LIVED","line":1,"column":1}]}],"test_efficacy":0,"mutations_coverage":100,"mutants_total":1,"mutants_killed":0,"mutants_lived":1,"mutants_not_viable":0,"mutants_not_covered":0,"elapsed_time":1,"mutator_statistics":{"arithmetic_base":1}}`)
	write(policy, `{"critical_paths":["critical.go"],"critical":{"adjusted_efficacy":100,"mutant_coverage":100},"noncritical":{"adjusted_efficacy":85,"mutant_coverage":90}}`)
	write(equivalents, "# no approvals\n")
	write(config, validGremlinsConfig)
	write(filepath.Join(dir, "critical.go"), "package fixture\n")
	configHash, err := fileHash(config)
	if err != nil {
		t.Fatal(err)
	}
	policyHash, err := fileHash(policy)
	if err != nil {
		t.Fatal(err)
	}
	reportHash, err := fileHash(report)
	if err != nil {
		t.Fatal(err)
	}
	baselineContents, err := json.Marshal(map[string]any{
		"schema_version": 1, "candidate": strings.Repeat("a", 40),
		"report_sha256": reportHash, "config_sha256": configHash,
		"policy_sha256": policyHash, "go_version": runtime.Version(),
		"gremlins_version": "v0.6.0",
		"critical_debt": []map[string]any{{
			"file": "critical.go", "line": 1, "column": 1,
			"mutant": "ARITHMETIC_BASE", "status": "LIVED", "owner": "root",
			"source_sha256": mustFileHash(t, filepath.Join(dir, "critical.go")),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	write(fullBaseline, string(baselineContents))

	var stdout bytes.Buffer
	err = run([]string{
		"check", "--report", report, "--policy", policy,
		"--baseline", equivalents, "--full-baseline", fullBaseline,
		"--config", config, "--scope", "full", "--gremlins-version", "v0.6.0",
	}, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"passed": true`) ||
		!strings.Contains(stdout.String(), `"absolute_passed": false`) ||
		!strings.Contains(stdout.String(), `"remaining": 1`) ||
		!strings.Contains(stdout.String(), `"full_baseline_sha256"`) ||
		!strings.Contains(stdout.String(), `"report_sha256": "`+reportHash+`"`) {
		t.Fatalf("result = %s", stdout.String())
	}

	write(config, validGremlinsConfig+"# hash drift\n")
	err = run([]string{
		"check", "--report", report, "--policy", policy,
		"--baseline", equivalents, "--full-baseline", fullBaseline,
		"--config", config, "--scope", "full", "--gremlins-version", "v0.6.0",
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "config hash does not match") {
		t.Fatalf("configuration drift error = %v", err)
	}
}

func TestRunPreflightValidatesBindingsWithoutAReport(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	write := func(name, contents string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	policy := write("policy.json", `{"critical_paths":["critical.go"],"critical":{"adjusted_efficacy":100,"mutant_coverage":100},"noncritical":{"adjusted_efficacy":85,"mutant_coverage":90}}`)
	equivalents := write("equivalents.md", "# no approvals\n")
	config := write("gremlins.yaml", validGremlinsConfig)
	write("critical.go", "package fixture\n")
	baselineBytes, err := json.Marshal(map[string]any{
		"schema_version": 1, "candidate": strings.Repeat("a", 40),
		"report_sha256": strings.Repeat("b", 64), "config_sha256": mustFileHash(t, config),
		"policy_sha256": mustFileHash(t, policy), "go_version": runtime.Version(),
		"gremlins_version": "v0.6.0", "critical_debt": []map[string]any{{
			"file": "critical.go", "line": 1, "column": 1, "mutant": "ARITHMETIC_BASE",
			"status": "LIVED", "owner": "root", "source_sha256": mustFileHash(t, "critical.go"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fullBaseline := write("full-baseline.json", string(baselineBytes))

	var stdout bytes.Buffer
	if err := run([]string{
		"preflight", "--policy", policy, "--baseline", equivalents,
		"--full-baseline", fullBaseline, "--config", config,
		"--scope", "full", "--gremlins-version", "v0.6.0",
	}, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"passed": true`) {
		t.Fatalf("preflight output = %s", stdout.String())
	}
	write("critical.go", "package changed\n")
	if err := run([]string{
		"preflight", "--policy", policy, "--baseline", equivalents,
		"--full-baseline", fullBaseline, "--config", config,
		"--scope", "full", "--gremlins-version", "v0.6.0",
	}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "source hash mismatch") {
		t.Fatalf("source drift preflight error = %v", err)
	}
	write("critical.go", "package fixture\n")
	write("gremlins.yaml", "not: [valid\n")
	if err := run([]string{
		"preflight", "--policy", policy, "--baseline", equivalents,
		"--config", config, "--scope", "changed", "--gremlins-version", "v0.6.0",
	}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "decode Gremlins config") {
		t.Fatalf("malformed config preflight error = %v", err)
	}
	write("gremlins.yaml", "unleash: {}\n")
	if err := run([]string{
		"preflight", "--policy", policy, "--baseline", equivalents,
		"--config", config, "--scope", "changed", "--gremlins-version", "v0.6.0",
	}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "missing required unleash settings") {
		t.Fatalf("weakened config preflight error = %v", err)
	}
}

func mustFileHash(t *testing.T, path string) string {
	t.Helper()
	hash, err := fileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestRunRejectsInvalidFullBaselineUse(t *testing.T) {
	if err := run([]string{
		"check", "--report", "unused", "--scope", "changed", "--full-baseline", "unused",
	}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "requires --scope full") {
		t.Fatalf("changed-scope full baseline error = %v", err)
	}
}
