package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/raulfrk/herdr-plugin-kit/internal/mutation"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 || (args[0] != "check" && args[0] != "preflight") {
		return errors.New("usage: mutationgate check --report FILE [options] | mutationgate preflight [options]")
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	reportPath := flags.String("report", "", "Gremlins JSON report")
	policyPath := flags.String("policy", "mutation-policy.json", "mutation policy")
	baselinePath := flags.String("baseline", "docs/mutation-baseline.md", "equivalent-mutant approvals")
	fullBaselinePath := flags.String("full-baseline", "", "release-level full mutation debt baseline")
	outputPath := flags.String("output", "", "machine-readable gate result")
	configPath := flags.String("config", ".gremlins.yaml", "Gremlins configuration")
	candidate := flags.String("candidate", "", "tested candidate commit")
	base := flags.String("base", "", "comparison base")
	scope := flags.String("scope", "", "mutation scope")
	gremlinsVersion := flags.String("gremlins-version", "", "verified Gremlins module version")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if command == "check" && *reportPath == "" {
		return errors.New("--report is required")
	}
	if *scope != "changed" && *scope != "full" {
		return errors.New("--scope must be changed or full")
	}
	if *fullBaselinePath != "" && *scope != "full" {
		return errors.New("--full-baseline requires --scope full")
	}

	policyContents, err := os.ReadFile(*policyPath)
	if err != nil {
		return fmt.Errorf("read policy: %w", err)
	}
	policy, err := mutation.ReadPolicy(bytes.NewReader(policyContents))
	if err != nil {
		return err
	}

	baselineContents, err := os.ReadFile(*baselinePath)
	if err != nil {
		return fmt.Errorf("read baseline: %w", err)
	}
	equivalents, err := mutation.ReadEquivalents(bytes.NewReader(baselineContents))
	if err != nil {
		return err
	}

	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	root = filepath.Clean(root)
	configContents, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("read Gremlins config: %w", err)
	}
	if err := mutation.ValidateGremlinsConfig(bytes.NewReader(configContents)); err != nil {
		return err
	}
	configHash := mutation.SHA256(configContents)
	policyHash := mutation.SHA256(policyContents)
	baselineHash := mutation.SHA256(baselineContents)
	if err := mutation.ValidateEquivalentSources(root, equivalents); err != nil {
		return err
	}
	fullBaselineHash := ""
	var fullBaseline mutation.FullBaseline
	if *fullBaselinePath != "" {
		fullBaselineContents, err := os.ReadFile(*fullBaselinePath)
		if err != nil {
			return fmt.Errorf("read full mutation baseline: %w", err)
		}
		fullBaseline, err = mutation.ReadFullBaseline(bytes.NewReader(fullBaselineContents))
		if err != nil {
			return err
		}
		if fullBaseline.ConfigSHA256 != configHash {
			return errors.New("full mutation baseline Gremlins config hash does not match current config")
		}
		if fullBaseline.PolicySHA256 != policyHash {
			return errors.New("full mutation baseline policy hash does not match current policy")
		}
		if fullBaseline.GoVersion != runtime.Version() {
			return errors.New("full mutation baseline Go version does not match current toolchain")
		}
		if fullBaseline.GremlinsVersion != *gremlinsVersion {
			return errors.New("full mutation baseline Gremlins version does not match current tool")
		}
		if err := mutation.ValidateFullBaseline(root, fullBaseline, policy); err != nil {
			return err
		}
		fullBaselineHash = mutation.SHA256(fullBaselineContents)
	}
	if command == "preflight" {
		encoded, err := json.MarshalIndent(struct {
			Passed             bool   `json:"passed"`
			Scope              string `json:"scope"`
			ConfigSHA256       string `json:"config_sha256"`
			PolicySHA256       string `json:"policy_sha256"`
			BaselineSHA256     string `json:"baseline_sha256"`
			FullBaselineSHA256 string `json:"full_baseline_sha256,omitempty"`
		}{true, *scope, configHash, policyHash, baselineHash, fullBaselineHash}, "", "  ")
		if err != nil {
			return fmt.Errorf("encode preflight result: %w", err)
		}
		_, err = stdout.Write(append(encoded, '\n'))
		return err
	}

	reportContents, err := os.ReadFile(*reportPath)
	if err != nil {
		return fmt.Errorf("read report: %w", err)
	}
	report, err := mutation.ReadReport(bytes.NewReader(reportContents))
	if err != nil {
		return err
	}
	result, err := mutation.Evaluate(root, report, policy, equivalents, *scope)
	if err != nil {
		return err
	}
	if *fullBaselinePath != "" {
		result, err = mutation.ApplyFullBaseline(root, result, report, policy, equivalents, fullBaseline)
		if err != nil {
			return err
		}
	}
	result.Run = mutation.RunMetadata{
		Candidate: *candidate, Base: *base, Scope: *scope,
		GoVersion: runtime.Version(), GremlinsVersion: *gremlinsVersion,
		ConfigSHA256: configHash, PolicySHA256: policyHash,
		BaselineSHA256: baselineHash, FullBaselineSHA256: fullBaselineHash,
		ReportSHA256: mutation.SHA256(reportContents),
	}
	result.Policy = policy
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	encoded = append(encoded, '\n')
	if _, err := stdout.Write(encoded); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	if *outputPath != "" {
		if err := os.WriteFile(*outputPath, encoded, 0o644); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
	}
	if !result.Passed {
		return errors.New("mutation policy failed")
	}
	return nil
}

func fileHash(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return mutation.SHA256(contents), nil
}
