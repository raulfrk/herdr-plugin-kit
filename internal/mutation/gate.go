package mutation

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Report struct {
	GoModule          string         `json:"go_module"`
	Files             []ReportFile   `json:"files"`
	TestEfficacy      float64        `json:"test_efficacy"`
	MutationsCoverage float64        `json:"mutations_coverage"`
	MutantsTotal      int            `json:"mutants_total"`
	MutantsKilled     int            `json:"mutants_killed"`
	MutantsLived      int            `json:"mutants_lived"`
	MutantsNotViable  int            `json:"mutants_not_viable"`
	MutantsNotCovered int            `json:"mutants_not_covered"`
	ElapsedTime       float64        `json:"elapsed_time"`
	MutatorStatistics map[string]int `json:"mutator_statistics"`
}

type ReportFile struct {
	FileName  string           `json:"file_name"`
	Mutations []ReportMutation `json:"mutations"`
}

type ReportMutation struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

type Policy struct {
	CriticalPaths []string  `json:"critical_paths"`
	Critical      Threshold `json:"critical"`
	Noncritical   Threshold `json:"noncritical"`
}

type Threshold struct {
	AdjustedEfficacy float64 `json:"adjusted_efficacy"`
	MutantCoverage   float64 `json:"mutant_coverage"`
}

type Counts struct {
	Killed     int `json:"killed"`
	Lived      int `json:"lived"`
	NotCovered int `json:"not_covered"`
	TimedOut   int `json:"timed_out"`
	NotViable  int `json:"not_viable"`
	Skipped    int `json:"skipped"`
	Equivalent int `json:"equivalent"`
	Unknown    int `json:"unknown"`
}

type Metrics struct {
	Counts             Counts   `json:"counts"`
	RawEfficacy        *float64 `json:"raw_efficacy"`
	AdjustedEfficacy   float64  `json:"adjusted_efficacy"`
	MutantCoverage     *float64 `json:"mutant_coverage"`
	VacuouslySatisfied bool     `json:"vacuously_satisfied"`
}

type Result struct {
	Passed             bool                    `json:"passed"`
	AbsolutePassed     *bool                   `json:"absolute_passed,omitempty"`
	Run                RunMetadata             `json:"run"`
	Policy             Policy                  `json:"policy"`
	Critical           Metrics                 `json:"critical"`
	Noncritical        Metrics                 `json:"noncritical"`
	Survivors          []MutationDisposition   `json:"survivors,omitempty"`
	Equivalents        []Equivalent            `json:"approved_equivalents,omitempty"`
	FullBaseline       *FullBaselineAssessment `json:"full_baseline,omitempty"`
	AbsoluteViolations []string                `json:"absolute_violations,omitempty"`
	Violations         []string                `json:"violations,omitempty"`
}

type RunMetadata struct {
	Candidate          string `json:"candidate"`
	Base               string `json:"base,omitempty"`
	Scope              string `json:"scope"`
	GoVersion          string `json:"go_version"`
	GremlinsVersion    string `json:"gremlins_version"`
	ReportSHA256       string `json:"report_sha256"`
	ConfigSHA256       string `json:"config_sha256"`
	PolicySHA256       string `json:"policy_sha256"`
	BaselineSHA256     string `json:"baseline_sha256"`
	FullBaselineSHA256 string `json:"full_baseline_sha256,omitempty"`
}

type MutationDisposition struct {
	File       string `json:"file"`
	Line       int    `json:"line"`
	Column     int    `json:"column"`
	Mutant     string `json:"mutant"`
	Equivalent bool   `json:"equivalent"`
}

type Equivalent struct {
	File        string `json:"file"`
	Line        int    `json:"line"`
	Column      int    `json:"column"`
	Symbol      string `json:"symbol"`
	Mutant      string `json:"mutant"`
	Original    string `json:"original"`
	Replacement string `json:"replacement"`
	SourceHash  string `json:"source_sha256"`
	Hypothesis  string `json:"hypothesis"`
	Proof       string `json:"proof"`
	Reviewer    string `json:"reviewer"`
}

type FullBaseline struct {
	SchemaVersion   int                   `json:"schema_version"`
	Candidate       string                `json:"candidate"`
	ReportSHA256    string                `json:"report_sha256"`
	ConfigSHA256    string                `json:"config_sha256"`
	PolicySHA256    string                `json:"policy_sha256"`
	GoVersion       string                `json:"go_version"`
	GremlinsVersion string                `json:"gremlins_version"`
	CriticalDebt    []BaselineDisposition `json:"critical_debt"`
}

type BaselineDisposition struct {
	File       string `json:"file"`
	Line       int    `json:"line"`
	Column     int    `json:"column"`
	Mutant     string `json:"mutant"`
	Status     string `json:"status"`
	Owner      string `json:"owner"`
	SourceHash string `json:"source_sha256"`
}

type FullBaselineAssessment struct {
	Candidate    string               `json:"candidate"`
	ReportSHA256 string               `json:"report_sha256"`
	CriticalDebt int                  `json:"critical_debt"`
	Remaining    int                  `json:"remaining"`
	Resolved     int                  `json:"resolved"`
	Regressions  []MutationRegression `json:"regressions,omitempty"`
}

type MutationRegression struct {
	File           string `json:"file"`
	Line           int    `json:"line"`
	Column         int    `json:"column"`
	Mutant         string `json:"mutant"`
	BaselineStatus string `json:"baseline_status,omitempty"`
	CurrentStatus  string `json:"current_status"`
}

type gremlinsConfig struct {
	Unleash *struct {
		Integration        *bool `yaml:"integration"`
		Workers            *int  `yaml:"workers"`
		TimeoutCoefficient *int  `yaml:"timeout-coefficient"`
		Threshold          *struct {
			Efficacy       *int `yaml:"efficacy"`
			MutantCoverage *int `yaml:"mutant-coverage"`
		} `yaml:"threshold"`
		ExcludeFiles *[]string `yaml:"exclude-files"`
	} `yaml:"unleash"`
	Mutants map[string]struct {
		Enabled *bool `yaml:"enabled"`
	} `yaml:"mutants"`
}

func ValidateGremlinsConfig(r io.Reader) error {
	var config gremlinsConfig
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return fmt.Errorf("decode Gremlins config: %w", err)
	}
	if err := ensureYAMLEOF(decoder); err != nil {
		return err
	}
	if config.Unleash == nil || config.Unleash.Integration == nil ||
		config.Unleash.Workers == nil || config.Unleash.TimeoutCoefficient == nil ||
		config.Unleash.Threshold == nil || config.Unleash.Threshold.Efficacy == nil ||
		config.Unleash.Threshold.MutantCoverage == nil || config.Unleash.ExcludeFiles == nil {
		return errors.New("Gremlins config is missing required unleash settings")
	}
	if *config.Unleash.Integration || *config.Unleash.Workers != 0 ||
		*config.Unleash.TimeoutCoefficient != 3 || *config.Unleash.Threshold.Efficacy != 0 ||
		*config.Unleash.Threshold.MutantCoverage != 0 {
		return errors.New("Gremlins config weakens required unleash settings")
	}
	wantExclusions := []string{"(^|/)vendor/", "_generated[.]go$", "[.]gen[.]go$", "[.]pb[.]go$"}
	if !equalStrings(*config.Unleash.ExcludeFiles, wantExclusions) {
		return errors.New("Gremlins config must exclude only the approved generated and vendor files")
	}
	wantMutants := map[string]bool{
		"arithmetic-base": true, "conditionals-boundary": true,
		"conditionals-negation": true, "increment-decrement": true,
		"invert-assignments": false, "invert-bitwise": false,
		"invert-bwassign": false, "invert-negatives": true,
		"invert-logical": true, "invert-loopctrl": false,
		"remove-self-assignments": false,
	}
	if len(config.Mutants) != len(wantMutants) {
		return errors.New("Gremlins config has an incomplete or unsupported mutator set")
	}
	for name, wantEnabled := range wantMutants {
		mutant, ok := config.Mutants[name]
		if !ok || mutant.Enabled == nil || *mutant.Enabled != wantEnabled {
			return fmt.Errorf("Gremlins config has an invalid %s mutator setting", name)
		}
	}
	return nil
}

func ensureYAMLEOF(decoder *yaml.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode trailing Gremlins config: %w", err)
	}
	return errors.New("Gremlins config contains multiple YAML documents")
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func ReadReport(r io.Reader) (Report, error) {
	var wire reportWire
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return Report{}, fmt.Errorf("decode Gremlins report: %w", err)
	}
	if err := ensureEOF(dec); err != nil {
		return Report{}, err
	}
	report, err := wire.report()
	if err != nil {
		return Report{}, err
	}
	if err := validateReportCounts(report); err != nil {
		return Report{}, err
	}
	return report, nil
}

type reportWire struct {
	GoModule          *string           `json:"go_module"`
	Files             *[]reportFileWire `json:"files"`
	TestEfficacy      *float64          `json:"test_efficacy"`
	MutationsCoverage *float64          `json:"mutations_coverage"`
	MutantsTotal      *int              `json:"mutants_total"`
	MutantsKilled     *int              `json:"mutants_killed"`
	MutantsLived      *int              `json:"mutants_lived"`
	MutantsNotViable  *int              `json:"mutants_not_viable"`
	MutantsNotCovered *int              `json:"mutants_not_covered"`
	ElapsedTime       *float64          `json:"elapsed_time"`
	MutatorStatistics *map[string]int   `json:"mutator_statistics"`
}

type reportFileWire struct {
	FileName  *string               `json:"file_name"`
	Mutations *[]reportMutationWire `json:"mutations"`
}

type reportMutationWire struct {
	Type   *string `json:"type"`
	Status *string `json:"status"`
	Line   *int    `json:"line"`
	Column *int    `json:"column"`
}

func (wire reportWire) report() (Report, error) {
	if wire.GoModule == nil || wire.Files == nil || wire.TestEfficacy == nil ||
		wire.MutationsCoverage == nil || wire.MutantsTotal == nil ||
		wire.MutantsKilled == nil || wire.MutantsLived == nil ||
		wire.MutantsNotViable == nil || wire.MutantsNotCovered == nil ||
		wire.ElapsedTime == nil || wire.MutatorStatistics == nil {
		return Report{}, errors.New("Gremlins report omits a required field")
	}
	report := Report{
		GoModule: *wire.GoModule, TestEfficacy: *wire.TestEfficacy,
		MutationsCoverage: *wire.MutationsCoverage, MutantsTotal: *wire.MutantsTotal,
		MutantsKilled: *wire.MutantsKilled, MutantsLived: *wire.MutantsLived,
		MutantsNotViable: *wire.MutantsNotViable, MutantsNotCovered: *wire.MutantsNotCovered,
		ElapsedTime: *wire.ElapsedTime, MutatorStatistics: *wire.MutatorStatistics,
	}
	for _, fileWire := range *wire.Files {
		if fileWire.FileName == nil || fileWire.Mutations == nil {
			return Report{}, errors.New("Gremlins report file omits a required field")
		}
		file := ReportFile{FileName: *fileWire.FileName}
		for _, mutantWire := range *fileWire.Mutations {
			if mutantWire.Type == nil || mutantWire.Status == nil ||
				mutantWire.Line == nil || mutantWire.Column == nil {
				return Report{}, errors.New("Gremlins report mutation omits a required field")
			}
			file.Mutations = append(file.Mutations, ReportMutation{
				Type: *mutantWire.Type, Status: *mutantWire.Status,
				Line: *mutantWire.Line, Column: *mutantWire.Column,
			})
		}
		report.Files = append(report.Files, file)
	}
	return report, nil
}

func validateReportCounts(report Report) error {
	var killed, lived, notViable, notCovered int
	for _, file := range report.Files {
		for _, mutant := range file.Mutations {
			switch normalizeStatus(mutant.Status) {
			case "KILLED":
				killed++
			case "LIVED":
				lived++
			case "NOT_VIABLE":
				notViable++
			case "NOT_COVERED":
				notCovered++
			}
		}
	}
	if killed != report.MutantsKilled || lived != report.MutantsLived ||
		notViable != report.MutantsNotViable || notCovered != report.MutantsNotCovered ||
		report.MutantsTotal != killed+lived+notViable {
		return errors.New("Gremlins aggregate counts do not match mutation details")
	}
	return nil
}

func ReadPolicy(r io.Reader) (Policy, error) {
	var wire policyWire
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return Policy{}, fmt.Errorf("decode mutation policy: %w", err)
	}
	if err := ensureEOF(dec); err != nil {
		return Policy{}, err
	}
	if wire.CriticalPaths == nil || wire.Critical == nil || wire.Noncritical == nil ||
		wire.Critical.AdjustedEfficacy == nil || wire.Critical.MutantCoverage == nil ||
		wire.Noncritical.AdjustedEfficacy == nil || wire.Noncritical.MutantCoverage == nil {
		return Policy{}, errors.New("mutation policy omits a required field")
	}
	policy := Policy{
		CriticalPaths: *wire.CriticalPaths,
		Critical: Threshold{
			AdjustedEfficacy: *wire.Critical.AdjustedEfficacy,
			MutantCoverage:   *wire.Critical.MutantCoverage,
		},
		Noncritical: Threshold{
			AdjustedEfficacy: *wire.Noncritical.AdjustedEfficacy,
			MutantCoverage:   *wire.Noncritical.MutantCoverage,
		},
	}
	for name, threshold := range map[string]Threshold{
		"critical": policy.Critical, "noncritical": policy.Noncritical,
	} {
		if threshold.AdjustedEfficacy < 0 || threshold.AdjustedEfficacy > 100 ||
			threshold.MutantCoverage < 0 || threshold.MutantCoverage > 100 {
			return Policy{}, fmt.Errorf("%s thresholds must be between 0 and 100", name)
		}
	}
	return policy, nil
}

type policyWire struct {
	CriticalPaths *[]string      `json:"critical_paths"`
	Critical      *thresholdWire `json:"critical"`
	Noncritical   *thresholdWire `json:"noncritical"`
}

type thresholdWire struct {
	AdjustedEfficacy *float64 `json:"adjusted_efficacy"`
	MutantCoverage   *float64 `json:"mutant_coverage"`
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON input contains more than one value")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

func ReadFullBaseline(r io.Reader) (FullBaseline, error) {
	type fullBaselineWire struct {
		SchemaVersion   int                    `json:"schema_version"`
		Candidate       string                 `json:"candidate"`
		ReportSHA256    string                 `json:"report_sha256"`
		ConfigSHA256    string                 `json:"config_sha256"`
		PolicySHA256    string                 `json:"policy_sha256"`
		GoVersion       string                 `json:"go_version"`
		GremlinsVersion string                 `json:"gremlins_version"`
		CriticalDebt    *[]BaselineDisposition `json:"critical_debt"`
	}
	var wire fullBaselineWire
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return FullBaseline{}, fmt.Errorf("decode full mutation baseline: %w", err)
	}
	if err := ensureEOF(dec); err != nil {
		return FullBaseline{}, err
	}
	if wire.CriticalDebt == nil {
		return FullBaseline{}, errors.New("full mutation baseline requires critical_debt")
	}
	baseline := FullBaseline{
		SchemaVersion: wire.SchemaVersion, Candidate: wire.Candidate,
		ReportSHA256: wire.ReportSHA256, ConfigSHA256: wire.ConfigSHA256,
		PolicySHA256: wire.PolicySHA256, GoVersion: wire.GoVersion,
		GremlinsVersion: wire.GremlinsVersion, CriticalDebt: *wire.CriticalDebt,
	}
	if baseline.SchemaVersion != 1 {
		return FullBaseline{}, errors.New("full mutation baseline schema_version must be 1")
	}
	if len(baseline.Candidate) != 40 || !validHex(baseline.Candidate, 20) ||
		!validHex(baseline.ReportSHA256, sha256.Size) ||
		!validHex(baseline.ConfigSHA256, sha256.Size) ||
		!validHex(baseline.PolicySHA256, sha256.Size) ||
		baseline.GoVersion == "" || baseline.GremlinsVersion == "" {
		return FullBaseline{}, errors.New("full mutation baseline has invalid provenance")
	}
	previous := ""
	for i := range baseline.CriticalDebt {
		entry := &baseline.CriticalDebt[i]
		if entry.File == "" || entry.Line < 1 || entry.Column < 1 || entry.Mutant == "" || entry.Owner == "" ||
			!validHex(entry.SourceHash, sha256.Size) {
			return FullBaseline{}, errors.New("full mutation baseline has an invalid debt entry")
		}
		entry.Status = normalizeStatus(entry.Status)
		entry.SourceHash = strings.ToLower(entry.SourceHash)
		if entry.Status != "LIVED" && entry.Status != "NOT_COVERED" {
			return FullBaseline{}, errors.New("full mutation baseline debt must be LIVED or NOT_COVERED")
		}
		key := mutationKey(entry.File, entry.Line, entry.Column, entry.Mutant)
		if previous != "" && key <= previous {
			return FullBaseline{}, errors.New("full mutation baseline debt must be sorted and unique")
		}
		previous = key
	}
	return baseline, nil
}

func validHex(value string, size int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == size
}

func ReadEquivalents(r io.Reader) ([]Equivalent, error) {
	var entries []Equivalent
	scanner := bufio.NewScanner(r)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := splitRow(line)
		if len(cells) != 11 || cells[0] == "file" || strings.HasPrefix(cells[0], "---") {
			continue
		}
		lineValue, err := strconv.Atoi(cells[1])
		if err != nil {
			return nil, fmt.Errorf("baseline line %d: invalid source line: %w", lineNumber, err)
		}
		columnValue, err := strconv.Atoi(cells[2])
		if err != nil {
			return nil, fmt.Errorf("baseline line %d: invalid source column: %w", lineNumber, err)
		}
		entry := Equivalent{
			File: cells[0], Line: lineValue, Column: columnValue,
			Symbol: cells[3], Mutant: cells[4], Original: cells[5],
			Replacement: cells[6], SourceHash: strings.ToLower(cells[7]),
			Hypothesis: cells[8], Proof: cells[9], Reviewer: cells[10],
		}
		if err := validateEquivalent(entry); err != nil {
			return nil, fmt.Errorf("baseline line %d: %w", lineNumber, err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read mutation baseline: %w", err)
	}
	return entries, nil
}

func splitRow(line string) []string {
	parts := strings.Split(strings.Trim(line, "|"), "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func validateEquivalent(entry Equivalent) error {
	if entry.File == "" || entry.Line < 1 || entry.Column < 1 || entry.Symbol == "" ||
		entry.Mutant == "" || entry.Original == "" || entry.Replacement == "" ||
		entry.Proof == "" || entry.Reviewer == "" {
		return errors.New("equivalent approval has an empty required field")
	}
	if !strings.HasPrefix(entry.Hypothesis, "HYP-") {
		return errors.New("hypothesis must use a HYP- identifier")
	}
	decoded, err := hex.DecodeString(entry.SourceHash)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("source_sha256 must be exactly 64 hexadecimal characters")
	}
	return nil
}

func equivalentIndex(root string, equivalents []Equivalent) (map[string]Equivalent, error) {
	equivalentKeys := make(map[string]Equivalent, len(equivalents))
	for _, entry := range equivalents {
		path := filepath.Join(root, filepath.FromSlash(entry.File))
		contents, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read equivalent source %q: %w", entry.File, err)
		}
		actual := fmt.Sprintf("%x", sha256.Sum256(contents))
		if actual != entry.SourceHash {
			return nil, fmt.Errorf("equivalent source hash mismatch for %s", entry.File)
		}
		key := mutationKey(entry.File, entry.Line, entry.Column, entry.Mutant)
		if _, exists := equivalentKeys[key]; exists {
			return nil, fmt.Errorf("duplicate equivalent approval for %s", key)
		}
		equivalentKeys[key] = entry
	}
	return equivalentKeys, nil
}

func ValidateEquivalentSources(root string, equivalents []Equivalent) error {
	_, err := equivalentIndex(root, equivalents)
	return err
}

func ValidateFullBaseline(root string, baseline FullBaseline, policy Policy) error {
	seen := make(map[string]bool, len(baseline.CriticalDebt))
	fileHashes := make(map[string]string)
	for _, entry := range baseline.CriticalDebt {
		key := mutationKey(entry.File, entry.Line, entry.Column, entry.Mutant)
		if seen[key] {
			return fmt.Errorf("duplicate full mutation baseline entry: %s", key)
		}
		seen[key] = true
		if !isCritical(entry.File, policy.CriticalPaths) {
			return fmt.Errorf("full mutation baseline contains noncritical debt: %s", key)
		}
		actual, exists := fileHashes[entry.File]
		if !exists {
			contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.File)))
			if err != nil {
				return fmt.Errorf("read full mutation baseline source %q: %w", entry.File, err)
			}
			actual = SHA256(contents)
			fileHashes[entry.File] = actual
		}
		if actual != entry.SourceHash {
			return fmt.Errorf("full mutation baseline source hash mismatch for %s", entry.File)
		}
	}
	return nil
}

func Evaluate(root string, report Report, policy Policy, equivalents []Equivalent, scope string) (Result, error) {
	if scope != "changed" && scope != "full" {
		return Result{}, errors.New("mutation scope must be changed or full")
	}
	equivalentKeys, err := equivalentIndex(root, equivalents)
	if err != nil {
		return Result{}, err
	}

	var critical, noncritical Counts
	matched := make(map[string]bool, len(equivalentKeys))
	dormant := make(map[string]bool, len(equivalentKeys))
	seenMutants := make(map[string]bool)
	for _, file := range report.Files {
		for _, mutant := range file.Mutations {
			key := mutationKey(file.FileName, mutant.Line, mutant.Column, mutant.Type)
			if seenMutants[key] {
				return Result{}, fmt.Errorf("duplicate report mutation: %s", key)
			}
			seenMutants[key] = true
			counts := &noncritical
			if isCritical(file.FileName, policy.CriticalPaths) {
				counts = &critical
			}
			switch normalizeStatus(mutant.Status) {
			case "KILLED":
				counts.Killed++
				if _, ok := equivalentKeys[key]; ok {
					dormant[key] = true
				}
			case "LIVED":
				counts.Lived++
				if _, ok := equivalentKeys[key]; ok {
					counts.Equivalent++
					matched[key] = true
				}
			case "NOT_COVERED":
				counts.NotCovered++
			case "TIMED_OUT":
				counts.TimedOut++
			case "NOT_VIABLE":
				counts.NotViable++
			case "SKIPPED":
				counts.Skipped++
				if scope == "changed" {
					if _, ok := equivalentKeys[key]; ok {
						dormant[key] = true
					}
				}
			default:
				counts.Unknown++
			}
		}
	}
	for key := range equivalentKeys {
		if !matched[key] && !dormant[key] {
			return Result{}, fmt.Errorf("equivalent approval does not match a LIVED mutant: %s", key)
		}
	}

	result := Result{
		Critical:    calculate(critical),
		Noncritical: calculate(noncritical),
		Equivalents: equivalents,
	}
	for _, file := range report.Files {
		for _, mutant := range file.Mutations {
			if normalizeStatus(mutant.Status) != "LIVED" {
				continue
			}
			key := mutationKey(file.FileName, mutant.Line, mutant.Column, mutant.Type)
			result.Survivors = append(result.Survivors, MutationDisposition{
				File: file.FileName, Line: mutant.Line, Column: mutant.Column,
				Mutant: mutant.Type, Equivalent: matched[key],
			})
		}
	}
	result.Violations = append(
		checkTier("critical", result.Critical, policy.Critical, true, scope == "full"),
		checkTier("noncritical", result.Noncritical, policy.Noncritical, false, scope == "full")...,
	)
	result.Passed = len(result.Violations) == 0
	return result, nil
}

func ApplyFullBaseline(root string, result Result, report Report, policy Policy, equivalents []Equivalent, baseline FullBaseline) (Result, error) {
	if err := ValidateFullBaseline(root, baseline, policy); err != nil {
		return Result{}, err
	}
	baselineDebt := make(map[string]BaselineDisposition, len(baseline.CriticalDebt))
	for _, entry := range baseline.CriticalDebt {
		key := mutationKey(entry.File, entry.Line, entry.Column, entry.Mutant)
		baselineDebt[key] = entry
	}

	equivalentKeys := make(map[string]bool, len(equivalents))
	for _, entry := range equivalents {
		equivalentKeys[mutationKey(entry.File, entry.Line, entry.Column, entry.Mutant)] = true
	}
	currentCritical := make(map[string]BaselineDisposition)
	for _, file := range report.Files {
		if !isCritical(file.FileName, policy.CriticalPaths) {
			continue
		}
		for _, mutant := range file.Mutations {
			key := mutationKey(file.FileName, mutant.Line, mutant.Column, mutant.Type)
			status := normalizeStatus(mutant.Status)
			currentCritical[key] = BaselineDisposition{
				File: file.FileName, Line: mutant.Line, Column: mutant.Column,
				Mutant: mutant.Type, Status: status,
			}
		}
	}

	assessment := FullBaselineAssessment{
		Candidate: baseline.Candidate, ReportSHA256: baseline.ReportSHA256,
		CriticalDebt: len(baseline.CriticalDebt),
	}
	for key, entry := range baselineDebt {
		current, exists := currentCritical[key]
		if !exists {
			return Result{}, fmt.Errorf("full mutation baseline entry is missing from report: %s", key)
		}
		delete(currentCritical, key)
		if current.Status == "LIVED" && equivalentKeys[key] {
			assessment.Resolved++
			continue
		}
		if !isCriticalDebtStatus(current.Status) {
			assessment.Resolved++
			continue
		}
		if criticalDebtRank(current.Status) <= criticalDebtRank(entry.Status) {
			assessment.Remaining++
			continue
		}
		assessment.Regressions = append(assessment.Regressions, MutationRegression{
			File: current.File, Line: current.Line, Column: current.Column, Mutant: current.Mutant,
			BaselineStatus: entry.Status, CurrentStatus: current.Status,
		})
	}
	for key, current := range currentCritical {
		if current.Status == "LIVED" && equivalentKeys[key] {
			continue
		}
		if !isCriticalDebtStatus(current.Status) {
			continue
		}
		assessment.Regressions = append(assessment.Regressions, MutationRegression{
			File: current.File, Line: current.Line, Column: current.Column, Mutant: current.Mutant,
			CurrentStatus: current.Status,
		})
	}
	sort.Slice(assessment.Regressions, func(i, j int) bool {
		left, right := assessment.Regressions[i], assessment.Regressions[j]
		return mutationKey(left.File, left.Line, left.Column, left.Mutant) <
			mutationKey(right.File, right.Line, right.Column, right.Mutant)
	})

	absolutePassed := result.Passed
	result.AbsolutePassed = &absolutePassed
	result.AbsoluteViolations = append([]string(nil), result.Violations...)
	result.FullBaseline = &assessment
	result.Violations = checkTier("noncritical", result.Noncritical, policy.Noncritical, false, true)
	if len(assessment.Regressions) > 0 {
		result.Violations = append(result.Violations, fmt.Sprintf(
			"critical: %d mutation disposition regressions from full baseline", len(assessment.Regressions),
		))
	}
	result.Passed = len(result.Violations) == 0
	return result, nil
}

func isCriticalDebtStatus(status string) bool {
	switch status {
	case "KILLED", "NOT_VIABLE":
		return false
	default:
		return true
	}
}

func criticalDebtRank(status string) int {
	switch normalizeStatus(status) {
	case "LIVED":
		return 1
	case "NOT_COVERED":
		return 2
	default:
		return 3
	}
}

func normalizeStatus(status string) string {
	return strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(status)), " ", "_")
}

func mutationKey(file string, line, column int, mutant string) string {
	clean := filepath.ToSlash(filepath.Clean(file))
	clean = strings.TrimPrefix(clean, "./")
	return fmt.Sprintf("%s:%d:%d:%s", clean, line, column, strings.ToUpper(mutant))
}

func isCritical(file string, prefixes []string) bool {
	clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(file)), "./")
	for _, prefix := range prefixes {
		if strings.HasPrefix(clean, strings.TrimPrefix(filepath.ToSlash(prefix), "./")) {
			return true
		}
	}
	return false
}

func calculate(counts Counts) Metrics {
	metrics := Metrics{Counts: counts}
	executed := counts.Killed + counts.Lived
	if executed > 0 {
		value := percentage(counts.Killed, executed)
		metrics.RawEfficacy = &value
	}
	actionable := counts.Lived - counts.Equivalent
	metrics.AdjustedEfficacy = 100
	if counts.Killed+actionable > 0 {
		metrics.AdjustedEfficacy = percentage(counts.Killed, counts.Killed+actionable)
	}
	viable := executed + counts.NotCovered
	if viable > 0 {
		value := percentage(executed, viable)
		metrics.MutantCoverage = &value
	} else {
		metrics.VacuouslySatisfied = true
	}
	return metrics
}

func percentage(numerator, denominator int) float64 {
	return float64(numerator) * 100 / float64(denominator)
}

func SHA256(contents []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(contents))
}

func checkTier(name string, metrics Metrics, threshold Threshold, critical, forbidSkipped bool) []string {
	var violations []string
	if forbidSkipped && metrics.Counts.Skipped > 0 {
		violations = append(violations, fmt.Sprintf("%s: SKIPPED mutants are forbidden", name))
	}
	if metrics.Counts.TimedOut > 0 {
		violations = append(violations, fmt.Sprintf("%s: TIMED OUT mutants are forbidden", name))
	}
	if metrics.Counts.Unknown > 0 {
		violations = append(violations, fmt.Sprintf("%s: unknown mutant statuses are forbidden", name))
	}
	if critical && metrics.Counts.NotCovered > 0 {
		violations = append(violations, "critical: NOT COVERED mutants are forbidden")
	}
	if metrics.AdjustedEfficacy < threshold.AdjustedEfficacy {
		violations = append(violations, fmt.Sprintf(
			"%s: adjusted efficacy %.2f is below %.2f", name,
			metrics.AdjustedEfficacy, threshold.AdjustedEfficacy,
		))
	}
	if metrics.MutantCoverage != nil && *metrics.MutantCoverage < threshold.MutantCoverage {
		violations = append(violations, fmt.Sprintf(
			"%s: mutant coverage %.2f is below %.2f", name,
			*metrics.MutantCoverage, threshold.MutantCoverage,
		))
	}
	return violations
}
