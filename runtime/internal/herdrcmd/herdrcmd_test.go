package herdrcmd

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

type runnerFunc func(context.Context, string, []string) (command.Result, error)

func (f runnerFunc) Run(ctx context.Context, executable string, args []string) (command.Result, error) {
	return f(ctx, executable, args)
}

func TestRunRequiresRunnerAndUsesExactExecutableAndArguments(t *testing.T) {
	if _, err := Run(context.Background(), nil, "herdr", nil); err == nil {
		t.Fatal("nil runner accepted")
	}
	for _, tc := range []struct{ input, want string }{{"", "herdr"}, {"/bin/custom-herdr", "/bin/custom-herdr"}} {
		var gotExe string
		var gotArgs []string
		runner := runnerFunc(func(_ context.Context, exe string, args []string) (command.Result, error) {
			gotExe, gotArgs = exe, append([]string(nil), args...)
			return command.Result{Stdout: []byte("ok"), ExitCode: 7}, nil
		})
		wantArgs := []string{"agent", "list"}
		result, err := Run(context.Background(), runner, tc.input, wantArgs)
		if err != nil || gotExe != tc.want || !reflect.DeepEqual(gotArgs, wantArgs) || string(result.Stdout) != "ok" || result.ExitCode != 7 {
			t.Fatalf("input=%q executable=%q args=%q result=%+v error=%v", tc.input, gotExe, gotArgs, result, err)
		}
	}
}

func TestRunPreservesRunnerErrorAndRejectsEitherTruncatedStream(t *testing.T) {
	wantErr := errors.New("runner failed")
	wantResult := command.Result{Stderr: []byte("bounded diagnostic"), ExitCode: 9}
	got, err := Run(context.Background(), runnerFunc(func(context.Context, string, []string) (command.Result, error) { return wantResult, wantErr }), "", nil)
	if !errors.Is(err, wantErr) || !reflect.DeepEqual(got, wantResult) {
		t.Fatalf("result=%+v error=%v", got, err)
	}
	for _, result := range []command.Result{{StdoutTruncated: true}, {StderrTruncated: true}} {
		if _, err := Run(context.Background(), runnerFunc(func(context.Context, string, []string) (command.Result, error) { return result, nil }), "", nil); err == nil {
			t.Fatalf("accepted truncation %+v", result)
		}
	}
}

func TestRunJSONAcceptsExactSizeLimit(t *testing.T) {
	prefix := `{"ok":true}`
	output := prefix + strings.Repeat(" ", MaxJSONBytes-len(prefix))
	runner := runnerFunc(func(context.Context, string, []string) (command.Result, error) {
		return command.Result{Stdout: []byte(output)}, nil
	})
	var target struct {
		OK bool `json:"ok"`
	}
	if err := RunJSON(context.Background(), runner, "", nil, &target); err != nil || !target.OK {
		t.Fatalf("target=%+v error=%v", target, err)
	}
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
