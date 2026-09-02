package main

import (
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
	if len(args) == 0 || args[0] != "check" {
		return errors.New("usage: mutationgate check --report FILE --policy FILE --baseline FILE [--output FILE]")
	}
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	reportPath := flags.String("report", "", "Gremlins JSON report")
	policyPath := flags.String("policy", "mutation-policy.json", "mutation policy")
	baselinePath := flags.String("baseline", "docs/mutation-baseline.md", "equivalent-mutant approvals")
	outputPath := flags.String("output", "", "machine-readable gate result")
	configPath := flags.String("config", ".gremlins.yaml", "Gremlins configuration")
	candidate := flags.String("candidate", "", "tested candidate commit")
	base := flags.String("base", "", "comparison base")
	scope := flags.String("scope", "", "mutation scope")
	gremlinsVersion := flags.String("gremlins-version", "", "verified Gremlins module version")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *reportPath == "" {
		return errors.New("--report is required")
	}

	reportFile, err := os.Open(*reportPath)
	if err != nil {
		return fmt.Errorf("open report: %w", err)
	}
	defer reportFile.Close()
	report, err := mutation.ReadReport(reportFile)
	if err != nil {
		return err
	}

	policyFile, err := os.Open(*policyPath)
	if err != nil {
		return fmt.Errorf("open policy: %w", err)
	}
	defer policyFile.Close()
	policy, err := mutation.ReadPolicy(policyFile)
	if err != nil {
		return err
	}

	baselineFile, err := os.Open(*baselinePath)
	if err != nil {
		return fmt.Errorf("open baseline: %w", err)
	}
	defer baselineFile.Close()
	equivalents, err := mutation.ReadEquivalents(baselineFile)
	if err != nil {
		return err
	}

	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	result, err := mutation.Evaluate(filepath.Clean(root), report, policy, equivalents)
	if err != nil {
		return err
	}
	configHash, err := fileHash(*configPath)
	if err != nil {
		return fmt.Errorf("hash Gremlins config: %w", err)
	}
	policyHash, err := fileHash(*policyPath)
	if err != nil {
		return fmt.Errorf("hash policy: %w", err)
	}
	baselineHash, err := fileHash(*baselinePath)
	if err != nil {
		return fmt.Errorf("hash baseline: %w", err)
	}
	result.Run = mutation.RunMetadata{
		Candidate: *candidate, Base: *base, Scope: *scope,
		GoVersion: runtime.Version(), GremlinsVersion: *gremlinsVersion,
		ConfigSHA256: configHash, PolicySHA256: policyHash,
		BaselineSHA256: baselineHash,
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
