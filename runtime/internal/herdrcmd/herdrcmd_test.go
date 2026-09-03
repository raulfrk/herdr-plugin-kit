package herdrcmd

import (
	"context"
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

type runnerFunc func(context.Context, string, []string) (command.Result, error)

func (f runnerFunc) Run(ctx context.Context, executable string, args []string) (command.Result, error) {
	return f(ctx, executable, args)
}

func TestRunJSONStrictBounds(t *testing.T) {
	for _, result := range []command.Result{
		{Stdout: []byte(`{"ok":true,"extra":1}`)},
		{Stdout: []byte(`{"ok":true} {}`)},
		{Stdout: []byte(strings.Repeat(" ", MaxJSONBytes+1))},
		{Stdout: []byte(`{"ok":true}`), StderrTruncated: true},
	} {
		runner := runnerFunc(func(context.Context, string, []string) (command.Result, error) { return result, nil })
		var target struct {
			OK bool `json:"ok"`
		}
		if err := RunJSON(context.Background(), runner, "", nil, &target); err == nil {
			t.Fatalf("accepted result %+v", result)
		}
	}
}
