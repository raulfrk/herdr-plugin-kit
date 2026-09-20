package mutation

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func strictPolicy() Policy {
	return Policy{
		CriticalPaths: []string{"internal/"},
		Critical:      Threshold{AdjustedEfficacy: 100, MutantCoverage: 100},
		Noncritical:   Threshold{AdjustedEfficacy: 85, MutantCoverage: 90},
	}
}

func fullBaseline(debt ...BaselineDisposition) FullBaseline {
	for i := range debt {
		if debt[i].SourceHash == "" {
			debt[i].SourceHash = strings.Repeat("e", 64)
		}
	}
	return FullBaseline{
		SchemaVersion: 1, Candidate: strings.Repeat("a", 40),
		ReportSHA256: strings.Repeat("b", 64), ConfigSHA256: strings.Repeat("c", 64),
		PolicySHA256: strings.Repeat("d", 64), GoVersion: "go1.27.0",
		GremlinsVersion: "v0.6.0", CriticalDebt: debt,
	}
}

func sourceBoundDebt(t *testing.T, root string, entry BaselineDisposition) BaselineDisposition {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(entry.File))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	contents := []byte("package fixture\n")
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	entry.SourceHash = fmt.Sprintf("%x", sha256.Sum256(contents))
	return entry
}

func TestReadFullBaselineValidatesProvenanceAndDebt(t *testing.T) {
	valid := fullBaseline(BaselineDisposition{
		File: "internal/a.go", Line: 2, Column: 3, Mutant: "ARITHMETIC_BASE",
		Status: "lived", Owner: "internal/mutation",
	})
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadFullBaseline(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if got.CriticalDebt[0].Status != "LIVED" {
		t.Fatalf("normalized status = %q", got.CriticalDebt[0].Status)
	}

	invalid := []string{
		`{}`,
		`{"schema_version":1,"candidate":"` + strings.Repeat("a", 40) + `","report_sha256":"` + strings.Repeat("b", 64) + `","config_sha256":"` + strings.Repeat("c", 64) + `","policy_sha256":"` + strings.Repeat("d", 64) + `","go_version":"go1.27.0","gremlins_version":"v0.6.0"}`,
		strings.Replace(string(encoded), `"owner":"internal/mutation"`, `"owner":""`, 1),
		strings.Replace(string(encoded), `"status":"lived"`, `"status":"TIMED_OUT"`, 1),
		strings.TrimSuffix(string(encoded), "}") + `,"unexpected":true}`,
		string(encoded) + `{}`,
	}
	for _, input := range invalid {
		if _, err := ReadFullBaseline(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted invalid full baseline %s", input)
		}
	}

	unsorted := fullBaseline(
		BaselineDisposition{File: "internal/z.go", Line: 1, Column: 1, Mutant: "A", Status: "LIVED", Owner: "z"},
		BaselineDisposition{File: "internal/a.go", Line: 1, Column: 1, Mutant: "A", Status: "LIVED", Owner: "a"},
	)
	encoded, err = json.Marshal(unsorted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFullBaseline(bytes.NewReader(encoded)); err == nil {
		t.Fatal("accepted unsorted full baseline")
	}
}

func TestReadReportRejectsUnknownFields(t *testing.T) {
	_, err := ReadReport(strings.NewReader(`{"go_module":"example","files":[],"surprise":true}`))
	if err == nil {
		t.Fatal("ReadReport accepted an unknown field")
	}
}

func TestReadReportAcceptsGremlinsAggregateFields(t *testing.T) {
	input := `{
		"go_module":"example",
		"files":[{"file_name":"a.go","mutations":[{"type":"ARITHMETIC_BASE","status":"KILLED","line":1,"column":1}]}],
		"test_efficacy":100,
		"mutations_coverage":100,
		"mutants_total":1,
		"mutants_killed":1,
		"mutants_lived":0,
		"mutants_not_viable":0,
		"mutants_not_covered":0,
		"elapsed_time":67.2,
		"mutator_statistics":{"arithmetic_base":7}
	}`
	report, err := ReadReport(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if report.MutantsKilled != 1 || report.MutatorStatistics["arithmetic_base"] != 7 {
		t.Fatalf("report = %+v", report)
	}
}

func TestReadReportRejectsAggregateDetailMismatch(t *testing.T) {
	input := `{
		"go_module":"example",
		"files":[],
		"test_efficacy":100,
		"mutations_coverage":100,
		"mutants_total":1,
		"mutants_killed":1,
		"mutants_lived":0,
		"mutants_not_viable":0,
		"mutants_not_covered":0,
		"elapsed_time":1,
		"mutator_statistics":{}
	}`
	if _, err := ReadReport(strings.NewReader(input)); err == nil ||
		!strings.Contains(err.Error(), "aggregate counts") {
		t.Fatalf("error = %v, want aggregate mismatch", err)
	}
}

func TestJSONReadersRejectMalformedAndTrailingInput(t *testing.T) {
	for name, read := range map[string]func(string) error{
		"report malformed": func(input string) error {
			_, err := ReadReport(strings.NewReader(input))
			return err
		},
		"policy malformed": func(input string) error {
			_, err := ReadPolicy(strings.NewReader(input))
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, input := range []string{"{", "{} {}"} {
				if err := read(input); err == nil {
					t.Fatalf("accepted %q", input)
				}
			}
		})
	}
}

func TestJSONReadersRejectMissingRequiredFields(t *testing.T) {
	if _, err := ReadReport(strings.NewReader(`{}`)); err == nil ||
		!strings.Contains(err.Error(), "required field") {
		t.Fatalf("empty report error = %v", err)
	}
	if _, err := ReadPolicy(strings.NewReader(`{}`)); err == nil ||
		!strings.Contains(err.Error(), "required field") {
		t.Fatalf("empty policy error = %v", err)
	}
	validReport := map[string]any{
		"go_module": "example", "files": []any{}, "test_efficacy": 0,
		"mutations_coverage": 0, "mutants_total": 0, "mutants_killed": 0,
		"mutants_lived": 0, "mutants_not_viable": 0, "mutants_not_covered": 0,
		"elapsed_time": 0, "mutator_statistics": map[string]int{},
	}
	for field := range validReport {
		candidate := make(map[string]any, len(validReport)-1)
		for key, value := range validReport {
			if key != field {
				candidate[key] = value
			}
		}
		encoded, err := json.Marshal(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ReadReport(bytes.NewReader(encoded)); err == nil {
			t.Errorf("report without %s passed", field)
		}
	}

	validPolicy := map[string]any{
		"critical_paths": []string{},
		"critical": map[string]float64{
			"adjusted_efficacy": 100, "mutant_coverage": 100,
		},
		"noncritical": map[string]float64{
			"adjusted_efficacy": 85, "mutant_coverage": 90,
		},
	}
	for _, field := range []string{"critical_paths", "critical", "noncritical"} {
		candidate := make(map[string]any, len(validPolicy)-1)
		for key, value := range validPolicy {
			if key != field {
				candidate[key] = value
			}
		}
		encoded, err := json.Marshal(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPolicy(bytes.NewReader(encoded)); err == nil {
			t.Errorf("policy without %s passed", field)
		}
	}
	for _, tier := range []string{"critical", "noncritical"} {
		for _, field := range []string{"adjusted_efficacy", "mutant_coverage"} {
			candidate := map[string]any{
				"critical_paths": validPolicy["critical_paths"],
				"critical": map[string]float64{
					"adjusted_efficacy": 100, "mutant_coverage": 100,
				},
				"noncritical": map[string]float64{
					"adjusted_efficacy": 85, "mutant_coverage": 90,
				},
			}
			delete(candidate[tier].(map[string]float64), field)
			encoded, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ReadPolicy(bytes.NewReader(encoded)); err == nil {
				t.Errorf("policy %s without %s passed", tier, field)
			}
		}
	}
	missingNestedThreshold := `{
		"critical_paths":[],
		"critical":{"adjusted_efficacy":100},
		"noncritical":{"adjusted_efficacy":85,"mutant_coverage":90}
	}`
	if _, err := ReadPolicy(strings.NewReader(missingNestedThreshold)); err == nil {
		t.Fatal("policy with omitted critical coverage passed")
	}
	for _, field := range []string{"file_name", "mutations"} {
		file := map[string]any{"file_name": "a.go", "mutations": []any{}}
		delete(file, field)
		report := make(map[string]any, len(validReport))
		for key, value := range validReport {
			report[key] = value
		}
		report["files"] = []any{file}
		encoded, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ReadReport(bytes.NewReader(encoded)); err == nil {
			t.Errorf("report file without %s passed", field)
		}
	}
	for _, field := range []string{"type", "status", "line", "column"} {
		mutant := map[string]any{
			"type": "ARITHMETIC_BASE", "status": "SKIPPED", "line": 1, "column": 1,
		}
		delete(mutant, field)
		report := make(map[string]any, len(validReport))
		for key, value := range validReport {
			report[key] = value
		}
		report["files"] = []any{
			map[string]any{"file_name": "a.go", "mutations": []any{mutant}},
		}
		encoded, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ReadReport(bytes.NewReader(encoded)); err == nil {
			t.Errorf("report mutation without %s passed", field)
		}
	}
}

func TestReadPolicyValidatesEveryThreshold(t *testing.T) {
	valid := `{"critical_paths":[],"critical":{"adjusted_efficacy":100,"mutant_coverage":100},"noncritical":{"adjusted_efficacy":85,"mutant_coverage":90}}`
	if _, err := ReadPolicy(strings.NewReader(valid)); err != nil {
		t.Fatalf("valid policy: %v", err)
	}
	invalid := []string{
		`{"critical":{"adjusted_efficacy":-1}}`,
		`{"critical":{"adjusted_efficacy":101}}`,
		`{"critical":{"mutant_coverage":-1}}`,
		`{"critical":{"mutant_coverage":101}}`,
		`{"noncritical":{"adjusted_efficacy":101}}`,
		`{"noncritical":{"mutant_coverage":101}}`,
	}
	for _, input := range invalid {
		if _, err := ReadPolicy(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted invalid policy %s", input)
		}
	}
}

func TestEvaluateCriticalKilledPasses(t *testing.T) {
	report := Report{Files: []ReportFile{{
		FileName: "internal/example.go",
		Mutations: []ReportMutation{{
			Type: "CONDITIONALS_NEGATION", Status: "KILLED", Line: 7, Column: 3,
		}},
	}}}
	result, err := Evaluate(t.TempDir(), report, strictPolicy(), nil, "full")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Passed {
		t.Fatalf("killed critical mutant failed gate: %v", result.Violations)
	}
	if result.Critical.RawEfficacy == nil || *result.Critical.RawEfficacy != 100 {
		t.Fatalf("raw efficacy = %v, want 100", result.Critical.RawEfficacy)
	}
}

func TestEvaluateLivedMutantFailsThreshold(t *testing.T) {
	report := Report{Files: []ReportFile{{
		FileName: "internal/example.go",
		Mutations: []ReportMutation{{
			Type: "ARITHMETIC_BASE", Status: "LIVED", Line: 4, Column: 11,
		}},
	}}}
	result, err := Evaluate(t.TempDir(), report, strictPolicy(), nil, "full")
	if err != nil {
		t.Fatal(err)
	}
	if result.Passed {
		t.Fatal("actionable LIVED critical mutant passed")
	}
	if result.Critical.AdjustedEfficacy != 0 {
		t.Fatalf("adjusted efficacy = %v, want 0", result.Critical.AdjustedEfficacy)
	}
}

func TestEquivalentApprovalAdjustsOnlyActionableMetric(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "internal", "example.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	source := []byte("package internal\n")
	if err := os.WriteFile(path, source, 0o644); err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(source))
	report := Report{Files: []ReportFile{{
		FileName: "internal/example.go",
		Mutations: []ReportMutation{{
			Type: "ARITHMETIC_BASE", Status: "LIVED", Line: 4, Column: 11,
		}},
	}}}
	equivalent := Equivalent{
		File: "internal/example.go", Line: 4, Column: 11, Symbol: "Add",
		Mutant: "ARITHMETIC_BASE", Original: "a + b", Replacement: "a - b",
		SourceHash: hash, Hypothesis: "HYP-MUTATION-01", Proof: "same result",
		Reviewer: "reviewer@example.invalid",
	}
	result, err := Evaluate(root, report, strictPolicy(), []Equivalent{equivalent}, "full")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Passed {
		t.Fatalf("approved equivalent failed: %v", result.Violations)
	}
	if result.Critical.RawEfficacy == nil || *result.Critical.RawEfficacy != 0 {
		t.Fatalf("raw efficacy was rewritten: %v", result.Critical.RawEfficacy)
	}
	if result.Critical.AdjustedEfficacy != 100 || result.Critical.Counts.Equivalent != 1 {
		t.Fatalf("adjusted result = %+v", result.Critical)
	}
}

func TestEquivalentApprovalMayBeDormantWhenMutantIsKilled(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "internal", "example.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	source := []byte("package internal\n")
	if err := os.WriteFile(path, source, 0o644); err != nil {
		t.Fatal(err)
	}
	equivalent := Equivalent{
		File: "internal/example.go", Line: 4, Column: 11, Symbol: "Add",
		Mutant: "ARITHMETIC_BASE", Original: "a + b", Replacement: "a - b",
		SourceHash: fmt.Sprintf("%x", sha256.Sum256(source)), Hypothesis: "HYP-MUTATION-01",
		Proof: "same result", Reviewer: "reviewer@example.invalid",
	}
	report := Report{Files: []ReportFile{{
		FileName: "internal/example.go",
		Mutations: []ReportMutation{{
			Type: "ARITHMETIC_BASE", Status: "KILLED", Line: 4, Column: 11,
		}},
	}}}
	result, err := Evaluate(root, report, strictPolicy(), []Equivalent{equivalent}, "full")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Passed || result.Critical.Counts.Equivalent != 0 {
		t.Fatalf("dormant killed equivalent result = %+v", result)
	}
}

func TestEquivalentApprovalDoesNotMatchSkippedMutant(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "internal", "example.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	source := []byte("package internal\n")
	if err := os.WriteFile(path, source, 0o644); err != nil {
		t.Fatal(err)
	}
	equivalent := Equivalent{
		File: "internal/example.go", Line: 4, Column: 11, Symbol: "Add",
		Mutant: "ARITHMETIC_BASE", Original: "a + b", Replacement: "a - b",
		SourceHash: fmt.Sprintf("%x", sha256.Sum256(source)), Hypothesis: "HYP-MUTATION-01",
		Proof: "same result", Reviewer: "reviewer@example.invalid",
	}
	report := Report{Files: []ReportFile{{
		FileName:  "internal/example.go",
		Mutations: []ReportMutation{{Type: "ARITHMETIC_BASE", Status: "SKIPPED", Line: 4, Column: 11}},
	}}}
	if _, err := Evaluate(root, report, strictPolicy(), []Equivalent{equivalent}, "full"); err == nil ||
		!strings.Contains(err.Error(), "does not match a LIVED mutant") {
		t.Fatalf("skipped equivalent error = %v", err)
	}
	result, err := Evaluate(root, report, strictPolicy(), []Equivalent{equivalent}, "changed")
	if err != nil || !result.Passed || result.Critical.Counts.Skipped != 1 || result.Critical.Counts.Equivalent != 0 {
		t.Fatalf("changed-scope dormant approval = %+v, %v", result, err)
	}
}

func TestEvaluateRejectsSkippedMutantsOnlyInFullScope(t *testing.T) {
	report := Report{Files: []ReportFile{{
		FileName:  "internal/example.go",
		Mutations: []ReportMutation{{Type: "ARITHMETIC_BASE", Status: "SKIPPED", Line: 4, Column: 11}},
	}}}
	full, err := Evaluate(t.TempDir(), report, strictPolicy(), nil, "full")
	if err != nil {
		t.Fatal(err)
	}
	if full.Passed || !strings.Contains(strings.Join(full.Violations, "\n"), "SKIPPED mutants are forbidden") {
		t.Fatalf("full skipped result = %+v", full)
	}
	changed, err := Evaluate(t.TempDir(), report, strictPolicy(), nil, "changed")
	if err != nil || !changed.Passed {
		t.Fatalf("changed skipped result = %+v, %v", changed, err)
	}
}

func TestEvaluateRejectsUnknownScope(t *testing.T) {
	if _, err := Evaluate(t.TempDir(), Report{}, strictPolicy(), nil, "targeted"); err == nil {
		t.Fatal("accepted an unknown mutation scope")
	}
}

func TestEvaluateRejectsDuplicateMutationDispositions(t *testing.T) {
	report := Report{Files: []ReportFile{{
		FileName: "internal/example.go",
		Mutations: []ReportMutation{
			{Type: "ARITHMETIC_BASE", Status: "SKIPPED", Line: 4, Column: 11},
			{Type: "ARITHMETIC_BASE", Status: "KILLED", Line: 4, Column: 11},
		},
	}}}
	if _, err := Evaluate(t.TempDir(), report, strictPolicy(), nil, "changed"); err == nil ||
		!strings.Contains(err.Error(), "duplicate report mutation") {
		t.Fatalf("duplicate mutation error = %v", err)
	}
}

func TestEquivalentSourceHashMismatchFailsClosed(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "internal", "example.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	equivalent := Equivalent{
		File: "internal/example.go", Line: 1, Column: 1, Symbol: "Example",
		Mutant: "ARITHMETIC_BASE", Original: "a + b", Replacement: "a - b",
		SourceHash: strings.Repeat("0", 64), Hypothesis: "HYP-MUTATION-02",
		Proof: "proof", Reviewer: "reviewer@example.invalid",
	}
	_, err := Evaluate(root, Report{}, strictPolicy(), []Equivalent{equivalent}, "full")
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("error = %v, want hash mismatch", err)
	}
}

func TestTimedOutAlwaysFails(t *testing.T) {
	report := Report{Files: []ReportFile{{
		FileName: "pkg/example.go",
		Mutations: []ReportMutation{{
			Type: "ARITHMETIC_BASE", Status: "TIMED OUT", Line: 1, Column: 1,
		}},
	}}}
	result, err := Evaluate(t.TempDir(), report, strictPolicy(), nil, "full")
	if err != nil {
		t.Fatal(err)
	}
	if result.Passed || len(result.Violations) == 0 {
		t.Fatal("TIMED OUT mutant passed")
	}
}

func TestEvaluateCountsEveryGremlinsStatus(t *testing.T) {
	statuses := []string{
		"KILLED", "LIVED", "NOT COVERED", "TIMED OUT", "NOT VIABLE", "SKIPPED", "future-status",
	}
	mutants := make([]ReportMutation, 0, len(statuses))
	for i, status := range statuses {
		mutants = append(mutants, ReportMutation{
			Type: "ARITHMETIC_BASE", Status: status, Line: i + 1, Column: 1,
		})
	}
	result, err := Evaluate(t.TempDir(), Report{Files: []ReportFile{{
		FileName: "pkg/example.go", Mutations: mutants,
	}}}, strictPolicy(), nil, "full")
	if err != nil {
		t.Fatal(err)
	}
	want := Counts{
		Killed: 1, Lived: 1, NotCovered: 1, TimedOut: 1,
		NotViable: 1, Skipped: 1, Unknown: 1,
	}
	if result.Noncritical.Counts != want {
		t.Fatalf("counts = %+v, want %+v", result.Noncritical.Counts, want)
	}
	if result.Passed {
		t.Fatal("timeouts and unknown statuses passed")
	}
}

func TestZeroDenominators(t *testing.T) {
	empty, err := Evaluate(t.TempDir(), Report{}, strictPolicy(), nil, "full")
	if err != nil {
		t.Fatal(err)
	}
	if !empty.Passed || !empty.Critical.VacuouslySatisfied ||
		empty.Critical.RawEfficacy != nil || empty.Critical.MutantCoverage != nil {
		t.Fatalf("empty result = %+v", empty)
	}

	notCovered := Report{Files: []ReportFile{{
		FileName: "pkg/example.go",
		Mutations: []ReportMutation{{
			Type: "ARITHMETIC_BASE", Status: "NOT COVERED", Line: 1, Column: 1,
		}},
	}}}
	result, err := Evaluate(t.TempDir(), notCovered, strictPolicy(), nil, "full")
	if err != nil {
		t.Fatal(err)
	}
	if result.Noncritical.RawEfficacy != nil || result.Noncritical.MutantCoverage == nil ||
		*result.Noncritical.MutantCoverage != 0 || result.Passed {
		t.Fatalf("not-covered result = %+v", result)
	}
}

func TestPropertyMetricsMatchIndependentOracle(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		killed := rapid.IntRange(0, 1000).Draw(t, "killed")
		lived := rapid.IntRange(0, 1000).Draw(t, "lived")
		notCovered := rapid.IntRange(0, 1000).Draw(t, "not-covered")
		equivalent := rapid.IntRange(0, lived).Draw(t, "equivalent")
		got := calculate(Counts{
			Killed: killed, Lived: lived, NotCovered: notCovered, Equivalent: equivalent,
		})

		if killed+lived == 0 {
			if got.RawEfficacy != nil {
				t.Fatalf("raw efficacy = %v, want N/A", *got.RawEfficacy)
			}
		} else {
			want := float64(killed) * 100 / float64(killed+lived)
			if got.RawEfficacy == nil || *got.RawEfficacy != want {
				t.Fatalf("raw efficacy = %v, want %v", got.RawEfficacy, want)
			}
		}

		actionable := lived - equivalent
		wantAdjusted := 100.0
		if killed+actionable > 0 {
			wantAdjusted = float64(killed) * 100 / float64(killed+actionable)
		}
		if got.AdjustedEfficacy != wantAdjusted {
			t.Fatalf("adjusted efficacy = %v, want %v", got.AdjustedEfficacy, wantAdjusted)
		}

		viable := killed + lived + notCovered
		if viable == 0 {
			if got.MutantCoverage != nil || !got.VacuouslySatisfied {
				t.Fatalf("coverage = %v, vacuous = %v", got.MutantCoverage, got.VacuouslySatisfied)
			}
		} else {
			want := float64(killed+lived) * 100 / float64(viable)
			if got.MutantCoverage == nil || *got.MutantCoverage != want {
				t.Fatalf("coverage = %v, want %v", got.MutantCoverage, want)
			}
		}
	})
}

func TestReadEquivalents(t *testing.T) {
	hash := strings.Repeat("a", 64)
	markdown := bytes.NewBufferString(
		"| file | line | column | symbol | mutant | original | replacement | source_sha256 | hypothesis | proof | reviewer |\n" +
			"| --- | ---: | ---: | --- | --- | --- | --- | --- | --- | --- | --- |\n" +
			"| internal/a.go | 2 | 4 | Add | ARITHMETIC_BASE | a+b | a-b | " + hash +
			" | HYP-MUTATION-03 | equivalent integers | Ada |\n",
	)
	entries, err := ReadEquivalents(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Symbol != "Add" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestReadEquivalentsRejectsMalformedRows(t *testing.T) {
	hash := strings.Repeat("a", 64)
	for name, row := range map[string]string{
		"line":       "| internal/a.go | no | 1 | Add | ARITHMETIC_BASE | a+b | a-b | " + hash + " | HYP-X-01 | proof | Ada |\n",
		"column":     "| internal/a.go | 1 | no | Add | ARITHMETIC_BASE | a+b | a-b | " + hash + " | HYP-X-01 | proof | Ada |\n",
		"required":   "| internal/a.go | 1 | 1 |  | ARITHMETIC_BASE | a+b | a-b | " + hash + " | HYP-X-01 | proof | Ada |\n",
		"hypothesis": "| internal/a.go | 1 | 1 | Add | ARITHMETIC_BASE | a+b | a-b | " + hash + " | ISSUE-1 | proof | Ada |\n",
		"hash":       "| internal/a.go | 1 | 1 | Add | ARITHMETIC_BASE | a+b | a-b | short | HYP-X-01 | proof | Ada |\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadEquivalents(strings.NewReader(row)); err == nil {
				t.Fatalf("accepted malformed row %q", row)
			}
		})
	}
}

func TestEvaluateRejectsMissingDuplicateAndUnmatchedApprovals(t *testing.T) {
	base := Equivalent{
		File: "missing.go", Line: 1, Column: 1, Symbol: "Example",
		Mutant: "ARITHMETIC_BASE", Original: "a+b", Replacement: "a-b",
		SourceHash: strings.Repeat("0", 64), Hypothesis: "HYP-X-01",
		Proof: "proof", Reviewer: "Ada",
	}
	if _, err := Evaluate(t.TempDir(), Report{}, strictPolicy(), []Equivalent{base}, "full"); err == nil {
		t.Fatal("missing equivalent source passed")
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "same.go"), []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	base.File = "same.go"
	base.SourceHash = fmt.Sprintf("%x", sha256.Sum256([]byte("same")))
	if _, err := Evaluate(root, Report{}, strictPolicy(), []Equivalent{base, base}, "full"); err == nil {
		t.Fatal("duplicate equivalent approval passed")
	}
	if _, err := Evaluate(root, Report{}, strictPolicy(), []Equivalent{base}, "full"); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("unmatched approval error = %v", err)
	}
}

func TestApplyFullBaselineAllowsExistingOrImprovedCriticalDebt(t *testing.T) {
	tests := []struct {
		name           string
		baselineStatus string
		currentStatus  string
		remaining      int
		resolved       int
		absolutePassed bool
	}{
		{name: "unchanged lived", baselineStatus: "LIVED", currentStatus: "LIVED", remaining: 1},
		{name: "covered but lived", baselineStatus: "NOT_COVERED", currentStatus: "LIVED", remaining: 1},
		{name: "killed", baselineStatus: "LIVED", currentStatus: "KILLED", resolved: 1, absolutePassed: true},
		{name: "not viable", baselineStatus: "NOT_COVERED", currentStatus: "NOT_VIABLE", resolved: 1, absolutePassed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			entry := sourceBoundDebt(t, root, BaselineDisposition{
				File: "internal/a.go", Line: 1, Column: 1, Mutant: "ARITHMETIC_BASE",
				Status: test.baselineStatus, Owner: "internal",
			})
			report := Report{Files: []ReportFile{{
				FileName: entry.File,
				Mutations: []ReportMutation{{
					Type: entry.Mutant, Status: test.currentStatus, Line: entry.Line, Column: entry.Column,
				}},
			}}}
			absolute, err := Evaluate(root, report, strictPolicy(), nil, "full")
			if err != nil {
				t.Fatal(err)
			}
			result, err := ApplyFullBaseline(root, absolute, report, strictPolicy(), nil, fullBaseline(entry))
			if err != nil {
				t.Fatal(err)
			}
			if !result.Passed || result.AbsolutePassed == nil || *result.AbsolutePassed != test.absolutePassed ||
				result.FullBaseline.Remaining != test.remaining || result.FullBaseline.Resolved != test.resolved {
				t.Fatalf("baseline result = %+v", result)
			}
		})
	}
}

func TestApplyFullBaselineRejectsNewAndWorsenedCriticalDebt(t *testing.T) {
	tests := []struct {
		name           string
		baselineStatus string
		currentStatus  string
	}{
		{name: "worsened uncovered", baselineStatus: "LIVED", currentStatus: "NOT_COVERED"},
		{name: "timed out", baselineStatus: "LIVED", currentStatus: "TIMED_OUT"},
		{name: "unknown", baselineStatus: "LIVED", currentStatus: "future_status"},
		{name: "skipped", baselineStatus: "LIVED", currentStatus: "SKIPPED"},
		{name: "new lived", currentStatus: "LIVED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			var debt []BaselineDisposition
			if test.baselineStatus != "" {
				debt = append(debt, sourceBoundDebt(t, root, BaselineDisposition{
					File: "internal/a.go", Line: 1, Column: 1, Mutant: "ARITHMETIC_BASE",
					Status: test.baselineStatus, Owner: "internal",
				}))
			} else if err := os.MkdirAll(filepath.Join(root, "internal"), 0o755); err != nil {
				t.Fatal(err)
			}
			report := Report{Files: []ReportFile{{
				FileName: "internal/a.go",
				Mutations: []ReportMutation{{
					Type: "ARITHMETIC_BASE", Status: test.currentStatus, Line: 1, Column: 1,
				}},
			}}}
			absolute, err := Evaluate(root, report, strictPolicy(), nil, "full")
			if err != nil {
				t.Fatal(err)
			}
			result, err := ApplyFullBaseline(root, absolute, report, strictPolicy(), nil, fullBaseline(debt...))
			if err != nil {
				t.Fatal(err)
			}
			if result.Passed || len(result.FullBaseline.Regressions) != 1 {
				t.Fatalf("regression result = %+v", result)
			}
		})
	}
}

func TestApplyFullBaselineRejectsSourceDriftAndMissingReportEntry(t *testing.T) {
	root := t.TempDir()
	entry := sourceBoundDebt(t, root, BaselineDisposition{
		File: "internal/a.go", Line: 1, Column: 1, Mutant: "ARITHMETIC_BASE",
		Status: "LIVED", Owner: "internal",
	})
	baseline := fullBaseline(entry)
	if err := os.WriteFile(filepath.Join(root, "internal/a.go"), []byte("package changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyFullBaseline(root, Result{}, Report{}, strictPolicy(), nil, baseline); err == nil ||
		!strings.Contains(err.Error(), "source hash mismatch") {
		t.Fatalf("source drift error = %v", err)
	}

	entry = sourceBoundDebt(t, root, entry)
	if _, err := ApplyFullBaseline(root, Result{}, Report{}, strictPolicy(), nil, fullBaseline(entry)); err == nil ||
		!strings.Contains(err.Error(), "missing from report") {
		t.Fatalf("missing report entry error = %v", err)
	}
}

func TestApplyFullBaselineStillEnforcesNoncriticalThresholds(t *testing.T) {
	report := Report{Files: []ReportFile{{
		FileName:  "pkg/a.go",
		Mutations: []ReportMutation{{Type: "ARITHMETIC_BASE", Status: "LIVED", Line: 1, Column: 1}},
	}}}
	absolute, err := Evaluate(t.TempDir(), report, strictPolicy(), nil, "full")
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyFullBaseline(t.TempDir(), absolute, report, strictPolicy(), nil, fullBaseline())
	if err != nil {
		t.Fatal(err)
	}
	if result.Passed || len(result.Violations) == 0 || len(result.FullBaseline.Regressions) != 0 {
		t.Fatalf("noncritical failure result = %+v", result)
	}
}

func TestApplyFullBaselineRejectsNoncriticalBaselineEntry(t *testing.T) {
	entry := BaselineDisposition{
		File: "pkg/a.go", Line: 1, Column: 1, Mutant: "ARITHMETIC_BASE",
		Status: "LIVED", Owner: "pkg",
	}
	if _, err := ApplyFullBaseline(t.TempDir(), Result{}, Report{}, strictPolicy(), nil, fullBaseline(entry)); err == nil {
		t.Fatal("accepted noncritical entry in full baseline")
	}
}
