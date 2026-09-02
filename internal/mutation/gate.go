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
	"strconv"
	"strings"
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
	Passed      bool                  `json:"passed"`
	Run         RunMetadata           `json:"run"`
	Policy      Policy                `json:"policy"`
	Critical    Metrics               `json:"critical"`
	Noncritical Metrics               `json:"noncritical"`
	Survivors   []MutationDisposition `json:"survivors,omitempty"`
	Equivalents []Equivalent          `json:"approved_equivalents,omitempty"`
	Violations  []string              `json:"violations,omitempty"`
}

type RunMetadata struct {
	Candidate       string `json:"candidate"`
	Base            string `json:"base,omitempty"`
	Scope           string `json:"scope"`
	GoVersion       string `json:"go_version"`
	GremlinsVersion string `json:"gremlins_version"`
	ConfigSHA256    string `json:"config_sha256"`
	PolicySHA256    string `json:"policy_sha256"`
	BaselineSHA256  string `json:"baseline_sha256"`
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

func Evaluate(root string, report Report, policy Policy, equivalents []Equivalent) (Result, error) {
	equivalentKeys := make(map[string]Equivalent, len(equivalents))
	for _, entry := range equivalents {
		path := filepath.Join(root, filepath.FromSlash(entry.File))
		contents, err := os.ReadFile(path)
		if err != nil {
			return Result{}, fmt.Errorf("read equivalent source %q: %w", entry.File, err)
		}
		actual := fmt.Sprintf("%x", sha256.Sum256(contents))
		if actual != entry.SourceHash {
			return Result{}, fmt.Errorf("equivalent source hash mismatch for %s", entry.File)
		}
		key := mutationKey(entry.File, entry.Line, entry.Column, entry.Mutant)
		if _, exists := equivalentKeys[key]; exists {
			return Result{}, fmt.Errorf("duplicate equivalent approval for %s", key)
		}
		equivalentKeys[key] = entry
	}

	var critical, noncritical Counts
	matched := make(map[string]bool, len(equivalentKeys))
	for _, file := range report.Files {
		for _, mutant := range file.Mutations {
			counts := &noncritical
			if isCritical(file.FileName, policy.CriticalPaths) {
				counts = &critical
			}
			switch normalizeStatus(mutant.Status) {
			case "KILLED":
				counts.Killed++
			case "LIVED":
				counts.Lived++
				key := mutationKey(file.FileName, mutant.Line, mutant.Column, mutant.Type)
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
			default:
				counts.Unknown++
			}
		}
	}
	for key := range equivalentKeys {
		if !matched[key] {
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
		checkTier("critical", result.Critical, policy.Critical, true),
		checkTier("noncritical", result.Noncritical, policy.Noncritical, false)...,
	)
	result.Passed = len(result.Violations) == 0
	return result, nil
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

func checkTier(name string, metrics Metrics, threshold Threshold, critical bool) []string {
	var violations []string
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
