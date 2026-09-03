// Package herdrcmd provides bounded, strict decoding for Herdr CLI commands.
package herdrcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

const MaxJSONBytes = 1 << 20

func Run(ctx context.Context, runner command.Runner, executable string, args []string) (command.Result, error) {
	if runner == nil {
		return command.Result{}, errors.New("Herdr command requires a command runner")
	}
	if executable == "" {
		executable = "herdr"
	}
	result, err := runner.Run(ctx, executable, args)
	if err != nil {
		return result, err
	}
	if result.StdoutTruncated || result.StderrTruncated {
		return result, errors.New("Herdr command output was truncated")
	}
	return result, nil
}

func RunJSON(ctx context.Context, runner command.Runner, executable string, args []string, target any) error {
	result, err := Run(ctx, runner, executable, args)
	if err != nil {
		return err
	}
	if len(result.Stdout) > MaxJSONBytes {
		return errors.New("Herdr JSON exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(result.Stdout))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode Herdr JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("Herdr output must contain exactly one JSON value")
	}
	return nil
}
