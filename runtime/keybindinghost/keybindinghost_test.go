package keybindinghost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/documentstore"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrcmd"
	"pgregory.net/rapid"
)

const reloadOK = `{"id":"cli:server:reload-config","result":{"type":"config_reload","status":"applied","diagnostics":[]}}` + "\n"

func TestTypedErrorFormattingAndSentinels(t *testing.T) {
	cause := errors.New("cause")
	err := &Error{Kind: FailureCheck, Cause: cause, Diagnostics: "diagnostic"}
	if got, want := err.Error(), "check: cause: diagnostic"; got != want {
		t.Fatalf("Error()=%q want=%q", got, want)
	}
	if !errors.Is(err, ErrCheck) || errors.Is(err, ErrReload) || errors.Is(err, errors.New("unknown")) || !errors.Is(err, cause) {
		t.Fatalf("sentinel matching is wrong: %v", err)
	}
	var nilError *Error
	if got := nilError.Error(); got != "<nil>" {
		t.Fatalf("nil Error()=%q", got)
	}
}

func TestLiteralManagedSerializationGolden(t *testing.T) {
	got, changed, err := replaceOwned(nil, "plugin.owner", []Binding{{Key: "prefix+x", Action: "plugin.owner.open", Description: "Open it"}})
	want := []byte("# herdr-plugin-kit keybindings v1 begin separator=none\n" +
		"# herdr-plugin-kit keybindings v1 owner=plugin.owner\n" +
		"[[keys.command]]\n" +
		"key = 'prefix+x'\n" +
		"type = 'plugin_action'\n" +
		"command = 'plugin.owner.open'\n" +
		"description = 'Open it'\n" +
		"# herdr-plugin-kit keybindings v1 end\n")
	if err != nil || !changed || !bytes.Equal(got, want) {
		t.Fatalf("golden changed=%t error=%v\ngot  %q\nwant %q", changed, err, got, want)
	}
}

type recordedCall struct {
	executable string
	args       []string
}

type fakeRunner struct {
	mu    sync.Mutex
	calls []recordedCall
	run   func(context.Context, string, []string, int) (command.Result, error)
}

type fakeDocumentStore struct {
	document documentstore.Document
	writes   int
	write    func(context.Context, string, []byte, documentstore.Revision, int) (documentstore.Revision, error)
}

func (s *fakeDocumentStore) Close() error { return nil }

func (s *fakeDocumentStore) ReadBounded(context.Context, string, int) (documentstore.Document, error) {
	return s.document, nil
}

func (s *fakeDocumentStore) WriteBounded(ctx context.Context, name string, data []byte, expected documentstore.Revision, _ int) (documentstore.Revision, error) {
	s.writes++
	return s.write(ctx, name, data, expected, s.writes)
}

func (r *fakeRunner) Run(ctx context.Context, executable string, args []string) (command.Result, error) {
	r.mu.Lock()
	index := len(r.calls)
	r.calls = append(r.calls, recordedCall{executable: executable, args: append([]string(nil), args...)})
	r.mu.Unlock()
	if r.run != nil {
		return r.run(ctx, executable, args, index)
	}
	if slicesEnd(args, "config", "check") {
		return command.Result{Stdout: []byte("config: ok\n"), ExitCode: 0}, nil
	}
	return command.Result{Stdout: []byte(reloadOK), ExitCode: 0}, nil
}

func (r *fakeRunner) snapshotCalls() []recordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedCall(nil), r.calls...)
}

func TestOwnedBindingsAddChangeRemovePreserveOtherBytesAndOwners(t *testing.T) {
	initial := []byte("onboarding = false\n\n[update]\nversion_check = false\n# unmanaged tail")
	path := writeConfig(t, initial, 0o640)
	runner := &fakeRunner{}
	host := Host{Runner: runner, Herdr: "/opt/herdr", ConfigPath: path, Session: "named"}

	alpha := []Binding{{Key: "prefix+a", Action: "plugin.alpha.open", Description: "Open alpha"}}
	applyBindings(t, host, "plugin.alpha", alpha)
	afterAlpha := readFile(t, path)
	if !bytes.HasPrefix(afterAlpha, initial) {
		t.Fatalf("unmanaged prefix changed:\n%s", afterAlpha)
	}
	alphaRaw := rawOwnerGroup(t, afterAlpha, "plugin.alpha")

	beta := []Binding{{Key: "prefix+b", Action: "plugin.beta.open", Description: "Open beta"}}
	applyBindings(t, host, "plugin.beta", beta)
	afterBeta := readFile(t, path)
	if got := rawOwnerGroup(t, afterBeta, "plugin.alpha"); !bytes.Equal(got, alphaRaw) {
		t.Fatalf("other owner bytes changed:\n%s\nwant:\n%s", got, alphaRaw)
	}
	betaRaw := rawOwnerGroup(t, afterBeta, "plugin.beta")

	alpha = []Binding{
		{Key: "prefix+x", Action: "plugin.alpha.open", Description: "Changed"},
		{Key: "prefix+y", Action: "plugin.alpha.quick", Description: "Quick"},
	}
	applyBindings(t, host, "plugin.alpha", alpha)
	afterChange := readFile(t, path)
	if got := rawOwnerGroup(t, afterChange, "plugin.beta"); !bytes.Equal(got, betaRaw) {
		t.Fatalf("other owner bytes changed during replacement")
	}
	assertSnapshot(t, host, "plugin.alpha", alpha)

	applyBindings(t, host, "plugin.alpha", nil)
	assertSnapshot(t, host, "plugin.alpha", nil)
	assertSnapshot(t, host, "plugin.beta", beta)
	applyBindings(t, host, "plugin.beta", nil)
	if got := readFile(t, path); !bytes.Equal(got, initial) {
		t.Fatalf("last removal did not restore exact original bytes:\ngot  %q\nwant %q", got, initial)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode=%v", info.Mode().Perm())
	}
}

func TestCommandsUseFixedEnvironmentAndExactDefaultNamedRouting(t *testing.T) {
	for _, session := range []string{"", "named"} {
		t.Run(map[bool]string{true: "default", false: "named"}[session == ""], func(t *testing.T) {
			path := writeConfig(t, []byte("onboarding = false\n"), 0o600)
			runner := &fakeRunner{}
			host := Host{Runner: runner, Herdr: "/tmp/herdr-literal", ConfigPath: path, Session: session}
			applyBindings(t, host, "plugin.owner", []Binding{{Key: "prefix+o", Action: "plugin.owner.open"}})
			calls := runner.snapshotCalls()
			if len(calls) != 2 {
				t.Fatalf("calls=%+v", calls)
			}
			for _, call := range calls {
				prefix := []string{"-u", "HERDR_SOCKET_PATH", "-u", "HERDR_SESSION", "-u", "HERDR_CLIENT_SOCKET_PATH", "--"}
				if call.executable != environmentBinary || len(call.args) < 11 || !reflect.DeepEqual(call.args[:7], prefix) || call.args[7] != "HERDR_CONFIG_PATH="+configArg(call.args) || call.args[8] != "/tmp/herdr-literal" {
					t.Fatalf("call=%+v", call)
				}
			}
			candidate := configArg(calls[0].args)
			if candidate == path || filepath.Dir(candidate) != filepath.Dir(path) || !strings.HasPrefix(filepath.Base(candidate), ".herdr-plugin-kit-check-") {
				t.Fatalf("candidate path=%q config=%q", candidate, path)
			}
			if configArg(calls[1].args) != path {
				t.Fatalf("reload config path=%q want=%q", configArg(calls[1].args), path)
			}
			if !slicesEnd(calls[0].args, "config", "check") {
				t.Fatalf("check args=%q", calls[0].args)
			}
			selected := session
			if selected == "" {
				selected = "default"
			}
			wantReload := []string{"--session", selected, "server", "reload-config"}
			if !reflect.DeepEqual(calls[1].args[9:], wantReload) {
				t.Fatalf("reload args=%q want suffix=%q", calls[1].args, wantReload)
			}
		})
	}
}

func TestActivateReloadsExistingBindingsWithoutWritingConfig(t *testing.T) {
	for _, session := range []string{"", "named"} {
		t.Run(map[bool]string{true: "default", false: "named"}[session == ""], func(t *testing.T) {
			path := writeConfig(t, []byte("onboarding = false\n"), 0o600)
			bindings := []Binding{{Key: "prefix+o", Action: "plugin.owner.open"}}
			applyBindings(t, Host{Runner: &fakeRunner{}, ConfigPath: path, Session: session}, "plugin.owner", bindings)
			before := readFile(t, path)
			snapshot, err := (Host{Runner: &fakeRunner{}, ConfigPath: path, Session: session}).Read(context.Background(), "plugin.owner")
			if err != nil {
				t.Fatal(err)
			}
			runner := &fakeRunner{}
			host := Host{Runner: runner, Herdr: "/tmp/herdr-literal", ConfigPath: path, Session: session}
			result, err := host.Activate(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: bindings})
			if err != nil || result.Changed || !result.Activated || result.Revision != snapshot.Revision {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if got := readFile(t, path); !bytes.Equal(got, before) {
				t.Fatalf("activation changed config:\ngot  %q\nwant %q", got, before)
			}
			calls := runner.snapshotCalls()
			if len(calls) != 2 || !slicesEnd(calls[0].args, "config", "check") {
				t.Fatalf("calls=%+v", calls)
			}
			selected := session
			if selected == "" {
				selected = "default"
			}
			if want := []string{"--session", selected, "server", "reload-config"}; !reflect.DeepEqual(calls[1].args[9:], want) {
				t.Fatalf("reload args=%q want=%q", calls[1].args, want)
			}
		})
	}
}

func TestActivateRejectsDriftMismatchAndCollisionWithoutCommandsOrWrites(t *testing.T) {
	path := writeConfig(t, []byte("onboarding = false\n"), 0o600)
	bindings := []Binding{{Key: "prefix+o", Action: "plugin.owner.open"}}
	applyBindings(t, Host{Runner: &fakeRunner{}, ConfigPath: path}, "plugin.owner", bindings)
	snapshot, err := (Host{Runner: &fakeRunner{}, ConfigPath: path}).Read(context.Background(), "plugin.owner")
	if err != nil {
		t.Fatal(err)
	}
	before := readFile(t, path)
	for name, request := range map[string]Request{
		"stale revision":   {Owner: "plugin.owner", Expected: documentstore.Revision{}, Bindings: bindings},
		"binding mismatch": {Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{}
			result, err := (Host{Runner: runner, ConfigPath: path}).Activate(context.Background(), request)
			if !errors.Is(err, ErrDrift) || result.Activated || len(runner.snapshotCalls()) != 0 || !bytes.Equal(readFile(t, path), before) {
				t.Fatalf("result=%+v error=%v calls=%+v", result, err, runner.snapshotCalls())
			}
		})
	}
	section, err := parseManaged(before)
	if err != nil {
		t.Fatal(err)
	}
	unmanaged := []byte("[[keys.command]]\nkey='prefix+o'\ntype='plugin_action'\ncommand='external.open'\n")
	conflicted := append(append([]byte(nil), before[:section.start]...), unmanaged...)
	conflicted = append(conflicted, before[section.start:]...)
	if err := os.WriteFile(path, conflicted, 0o600); err != nil {
		t.Fatal(err)
	}
	conflictSnapshot, err := (Host{Runner: &fakeRunner{}, ConfigPath: path}).Read(context.Background(), "plugin.owner")
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	result, err := (Host{Runner: runner, ConfigPath: path}).Activate(context.Background(), Request{Owner: "plugin.owner", Expected: conflictSnapshot.Revision, Bindings: bindings})
	if !errors.Is(err, ErrCollision) || result.Activated || len(runner.snapshotCalls()) != 0 || !bytes.Equal(readFile(t, path), conflicted) {
		t.Fatalf("result=%+v error=%v calls=%+v", result, err, runner.snapshotCalls())
	}
}

func TestActivateReportsCheckAndReloadFailuresWithoutWritingConfig(t *testing.T) {
	path := writeConfig(t, []byte("onboarding = false\n"), 0o600)
	bindings := []Binding{{Key: "prefix+o", Action: "plugin.owner.open"}}
	applyBindings(t, Host{Runner: &fakeRunner{}, ConfigPath: path}, "plugin.owner", bindings)
	snapshot, _ := (Host{Runner: &fakeRunner{}, ConfigPath: path}).Read(context.Background(), "plugin.owner")
	before := readFile(t, path)
	for _, test := range []struct {
		name string
		want error
		run  func(int) (command.Result, error)
	}{
		{name: "check", want: ErrCheck, run: func(int) (command.Result, error) {
			return command.Result{Stdout: []byte("config: invalid\n"), ExitCode: 1}, &command.RunError{Kind: command.ErrorExit, Cause: errors.New("exit status 1")}
		}},
		{name: "reload", want: ErrReload, run: func(index int) (command.Result, error) {
			if index == 0 {
				return command.Result{Stdout: []byte("config: ok\n")}, nil
			}
			return command.Result{Stderr: []byte("down"), ExitCode: 1}, &command.RunError{Kind: command.ErrorExit, Cause: errors.New("exit status 1")}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{run: func(_ context.Context, _ string, _ []string, index int) (command.Result, error) {
				return test.run(index)
			}}
			result, err := (Host{Runner: runner, ConfigPath: path}).Activate(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: bindings})
			if !errors.Is(err, test.want) || result.Activated || !bytes.Equal(readFile(t, path), before) {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
}

func TestActivateRejectsExternalDriftAfterCheckBeforeReload(t *testing.T) {
	path := writeConfig(t, []byte("onboarding = false\n"), 0o600)
	bindings := []Binding{{Key: "prefix+o", Action: "plugin.owner.open"}}
	applyBindings(t, Host{Runner: &fakeRunner{}, ConfigPath: path}, "plugin.owner", bindings)
	snapshot, err := (Host{Runner: &fakeRunner{}, ConfigPath: path}).Read(context.Background(), "plugin.owner")
	if err != nil {
		t.Fatal(err)
	}
	external := append(readFile(t, path), []byte("\n# external edit\n")...)
	runner := &fakeRunner{run: func(_ context.Context, _ string, args []string, _ int) (command.Result, error) {
		if !slicesEnd(args, "config", "check") {
			t.Fatal("reload ran after post-check drift")
		}
		if err := os.WriteFile(path, external, 0o600); err != nil {
			t.Fatal(err)
		}
		return command.Result{Stdout: []byte("config: ok\n")}, nil
	}}
	result, err := (Host{Runner: runner, ConfigPath: path}).Activate(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: bindings})
	if !errors.Is(err, ErrDrift) || result.Activated || result.Revision == snapshot.Revision || len(runner.snapshotCalls()) != 1 {
		t.Fatalf("result=%+v error=%v calls=%+v", result, err, runner.snapshotCalls())
	}
	if got := readFile(t, path); !bytes.Equal(got, external) {
		t.Fatalf("activation changed external bytes: %q", got)
	}
}

func TestActivateClassifiesCanceledTransactionLock(t *testing.T) {
	path := writeConfig(t, []byte("onboarding = false\n"), 0o600)
	binding := []Binding{{Key: "prefix+o", Action: "plugin.owner.open"}}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			runner := &fakeRunner{}
			host := Host{
				Runner:     runner,
				ConfigPath: path,
				acquireLock: func(context.Context, string) (io.Closer, error) {
					return nil, cause
				},
			}
			result, err := host.Activate(context.Background(), Request{Owner: "plugin.owner", Bindings: binding})
			var operation *Error
			if !errors.As(err, &operation) || operation.Kind != FailureCanceled || !errors.Is(err, cause) || result.Activated {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if calls := runner.snapshotCalls(); len(calls) != 0 {
				t.Fatalf("canceled activation ran commands: %+v", calls)
			}
		})
	}
}

func TestNoopSkipsCheckWriteAndReload(t *testing.T) {
	path := writeConfig(t, []byte("onboarding = false\n"), 0o600)
	runner := &fakeRunner{}
	host := Host{Runner: runner, ConfigPath: path}
	snapshot, err := host.Read(context.Background(), "plugin.owner")
	if err != nil {
		t.Fatal(err)
	}
	result, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision})
	if err != nil || result.Changed || len(runner.snapshotCalls()) != 0 {
		t.Fatalf("result=%+v error=%v calls=%+v", result, err, runner.snapshotCalls())
	}
}

func TestNoopStillRejectsNewUnmanagedCollision(t *testing.T) {
	path := writeConfig(t, []byte("onboarding = false\n"), 0o600)
	host := Host{Runner: &fakeRunner{}, ConfigPath: path}
	bindings := []Binding{{Key: "prefix+o", Action: "plugin.owner.open"}}
	applyBindings(t, host, "plugin.owner", bindings)
	before := readFile(t, path)
	section, err := parseManaged(before)
	if err != nil {
		t.Fatal(err)
	}
	unmanaged := []byte("[[keys.command]]\nkey='prefix+o'\ntype='plugin_action'\ncommand='external.open'\n")
	conflicted := append(append([]byte(nil), before[:section.start]...), unmanaged...)
	conflicted = append(conflicted, before[section.start:]...)
	if err := os.WriteFile(path, conflicted, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := host.Read(context.Background(), "plugin.owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: bindings}); !errors.Is(err, ErrCollision) {
		t.Fatalf("collision error=%v", err)
	}
}

func TestUnmanagedAndOtherOwnerCollisionsFailBeforeCommand(t *testing.T) {
	unmanaged := []byte("[[keys.command]]\nkey = 'prefix+x'\ntype = 'plugin_action'\ncommand = 'external.open'\n")
	path := writeConfig(t, unmanaged, 0o600)
	runner := &fakeRunner{}
	host := Host{Runner: runner, ConfigPath: path}
	snapshot, err := host.Read(context.Background(), "plugin.owner")
	if err != nil {
		t.Fatal(err)
	}
	_, err = host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}})
	if !errors.Is(err, ErrCollision) || len(runner.snapshotCalls()) != 0 || !bytes.Equal(readFile(t, path), unmanaged) {
		t.Fatalf("error=%v calls=%+v bytes=%q", err, runner.snapshotCalls(), readFile(t, path))
	}

	applyBindings(t, host, "plugin.other", []Binding{{Key: "prefix+y", Action: "plugin.other.open"}})
	snapshot, err = host.Read(context.Background(), "plugin.owner")
	if err != nil {
		t.Fatal(err)
	}
	before := readFile(t, path)
	_, err = host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+y", Action: "plugin.owner.open"}}})
	if !errors.Is(err, ErrCollision) || !bytes.Equal(readFile(t, path), before) {
		t.Fatalf("error=%v config changed=%t", err, !bytes.Equal(readFile(t, path), before))
	}
}

func TestStaleRevisionAndInvalidRequestsFailClosed(t *testing.T) {
	path := writeConfig(t, []byte("onboarding = false\n"), 0o600)
	runner := &fakeRunner{}
	host := Host{Runner: runner, ConfigPath: path}
	for _, request := range []Request{
		{Owner: "plugin.owner", Expected: documentstore.Revision{}, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}},
		{Owner: "Bad Owner"},
		{Owner: "plugin.owner", Bindings: []Binding{{Key: "prefix+x", Action: "other.open"}}},
		{Owner: "plugin.owner", Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.one"}, {Key: "prefix+x", Action: "plugin.owner.two"}}},
	} {
		_, err := host.Apply(context.Background(), request)
		if err == nil {
			t.Fatalf("request accepted: %+v", request)
		}
	}
	if len(runner.snapshotCalls()) != 0 {
		t.Fatalf("commands run=%+v", runner.snapshotCalls())
	}
}

func TestCheckFailureLeavesOriginalBytesAndCleansCandidate(t *testing.T) {
	initial := []byte("onboarding = false\n")
	path := writeConfig(t, initial, 0o600)
	runner := &fakeRunner{run: func(_ context.Context, _ string, _ []string, _ int) (command.Result, error) {
		return command.Result{Stdout: []byte("config: issues found\ncollision\n"), ExitCode: 1}, &command.RunError{Kind: command.ErrorExit, Cause: errors.New("exit status 1")}
	}}
	host := Host{Runner: runner, ConfigPath: path}
	snapshot, _ := host.Read(context.Background(), "plugin.owner")
	_, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}})
	var operation *Error
	if !errors.Is(err, ErrCheck) || !errors.As(err, &operation) || !strings.Contains(operation.Diagnostics, "collision") {
		t.Fatalf("error=%T %v", err, err)
	}
	if !bytes.Equal(readFile(t, path), initial) {
		t.Fatal("config changed after check failure")
	}
	assertNoCandidates(t, filepath.Dir(path))
}

func TestConfigCheckRequiresExactCleanResponse(t *testing.T) {
	for name, response := range map[string]command.Result{
		"unexpected stdout": {Stdout: []byte("config: okay\n")},
		"nonempty stderr":   {Stdout: []byte("config: ok\n"), Stderr: []byte("warning")},
	} {
		t.Run(name, func(t *testing.T) {
			initial := []byte("onboarding=false\n")
			path := writeConfig(t, initial, 0o600)
			runner := &fakeRunner{run: func(_ context.Context, _ string, _ []string, _ int) (command.Result, error) {
				return response, nil
			}}
			host := Host{Runner: runner, ConfigPath: path}
			snapshot, _ := host.Read(context.Background(), "plugin.owner")
			_, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}})
			if !errors.Is(err, ErrCheck) || len(runner.snapshotCalls()) != 1 || !bytes.Equal(readFile(t, path), initial) {
				t.Fatalf("error=%v calls=%+v", err, runner.snapshotCalls())
			}
		})
	}
}

func TestOversizedConcurrentReplacementDuringCheckFailsBeforePublication(t *testing.T) {
	path := writeConfig(t, []byte("onboarding=false\n"), 0o600)
	oversized := bytes.Repeat([]byte{'#'}, MaxConfigBytes+1)
	runner := &fakeRunner{run: func(_ context.Context, _ string, args []string, _ int) (command.Result, error) {
		if !slicesEnd(args, "config", "check") {
			t.Fatal("reload ran after oversized concurrent replacement")
		}
		if err := os.WriteFile(path, oversized, 0o600); err != nil {
			t.Fatal(err)
		}
		return command.Result{Stdout: []byte("config: ok\n")}, nil
	}}
	host := Host{Runner: runner, ConfigPath: path}
	snapshot, err := host.Read(context.Background(), "plugin.owner")
	if err != nil {
		t.Fatal(err)
	}
	result, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}})
	if !errors.Is(err, ErrWrite) || result.Changed || len(runner.snapshotCalls()) != 1 || !bytes.Equal(readFile(t, path), oversized) {
		t.Fatalf("result=%+v calls=%+v error=%v", result, runner.snapshotCalls(), err)
	}
}

func TestReloadFailureRollsBackExactBytesAndRestoresEffectiveConfig(t *testing.T) {
	initial := []byte("onboarding = false\n# exact prior bytes")
	path := writeConfig(t, initial, 0o644)
	reloads := 0
	runner := &fakeRunner{run: func(_ context.Context, _ string, args []string, _ int) (command.Result, error) {
		if slicesEnd(args, "config", "check") {
			return command.Result{Stdout: []byte("config: ok\n")}, nil
		}
		reloads++
		if reloads == 1 {
			return command.Result{Stdout: []byte(`{"error":{"code":"server_not_running"}}`), ExitCode: 1}, &command.RunError{Kind: command.ErrorExit, Cause: errors.New("exit status 1")}
		}
		return command.Result{Stdout: []byte(reloadOK)}, nil
	}}
	host := Host{Runner: runner, ConfigPath: path}
	snapshot, _ := host.Read(context.Background(), "plugin.owner")
	result, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}})
	if !errors.Is(err, ErrReload) || !result.Changed || !result.RolledBack || !result.EffectiveRestored || result.Revision != snapshot.Revision {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if reloads != 2 || !bytes.Equal(readFile(t, path), initial) {
		t.Fatalf("reloads=%d bytes=%q", reloads, readFile(t, path))
	}
}

func TestInvalidReloadReceiptsRollBack(t *testing.T) {
	for name, receipt := range map[string]string{
		"wrong type":    `{"id":"cli:server:reload-config","result":{"type":"wrong","status":"applied","diagnostics":[]}}`,
		"diagnostic":    `{"id":"cli:server:reload-config","result":{"type":"config_reload","status":"applied","diagnostics":[{}]}}`,
		"unknown field": `{"id":"cli:server:reload-config","result":{"type":"config_reload","status":"applied","diagnostics":[]},"future":true}`,
		"trailing JSON": reloadOK + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			initial := []byte("onboarding=false\n")
			path := writeConfig(t, initial, 0o600)
			reloads := 0
			runner := &fakeRunner{run: func(_ context.Context, _ string, args []string, _ int) (command.Result, error) {
				if slicesEnd(args, "config", "check") {
					return command.Result{Stdout: []byte("config: ok\n")}, nil
				}
				reloads++
				if reloads == 1 {
					return command.Result{Stdout: []byte(receipt)}, nil
				}
				return command.Result{Stdout: []byte(reloadOK)}, nil
			}}
			host := Host{Runner: runner, ConfigPath: path}
			snapshot, _ := host.Read(context.Background(), "plugin.owner")
			result, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}})
			if !errors.Is(err, ErrReload) || !result.RolledBack || !result.EffectiveRestored || !bytes.Equal(readFile(t, path), initial) {
				t.Fatalf("result=%+v error=%v bytes=%q", result, err, readFile(t, path))
			}
		})
	}
}

func TestReloadOutputAndStderrBounds(t *testing.T) {
	exact := reloadOK + strings.Repeat(" ", herdrcmd.MaxJSONBytes-len(reloadOK))
	for name, first := range map[string]command.Result{
		"exact output limit": {Stdout: []byte(exact)},
		"over output limit":  {Stdout: []byte(exact + " ")},
		"nonempty stderr":    {Stdout: []byte(reloadOK), Stderr: []byte("warning")},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, []byte("onboarding=false\n"), 0o600)
			reloads := 0
			runner := &fakeRunner{run: func(_ context.Context, _ string, args []string, _ int) (command.Result, error) {
				if slicesEnd(args, "config", "check") {
					return command.Result{Stdout: []byte("config: ok\n")}, nil
				}
				reloads++
				if reloads == 1 {
					return first, nil
				}
				return command.Result{Stdout: []byte(reloadOK)}, nil
			}}
			host := Host{Runner: runner, ConfigPath: path}
			snapshot, _ := host.Read(context.Background(), "plugin.owner")
			result, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}})
			if name == "exact output limit" {
				if err != nil || !result.Changed || reloads != 1 {
					t.Fatalf("result=%+v reloads=%d error=%v", result, reloads, err)
				}
				return
			}
			if !errors.Is(err, ErrReload) || !result.RolledBack || !result.EffectiveRestored || reloads != 2 {
				t.Fatalf("result=%+v reloads=%d error=%v", result, reloads, err)
			}
		})
	}
}

func TestRollbackReloadFailureIsExplicit(t *testing.T) {
	initial := []byte("onboarding=false\n")
	path := writeConfig(t, initial, 0o600)
	runner := &fakeRunner{run: func(_ context.Context, _ string, args []string, _ int) (command.Result, error) {
		if slicesEnd(args, "config", "check") {
			return command.Result{Stdout: []byte("config: ok\n")}, nil
		}
		return command.Result{ExitCode: 1}, &command.RunError{Kind: command.ErrorExit, Cause: errors.New("reload failed")}
	}}
	host := Host{Runner: runner, ConfigPath: path}
	snapshot, _ := host.Read(context.Background(), "plugin.owner")
	result, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}})
	if !errors.Is(err, ErrRollback) || !result.RolledBack || result.EffectiveRestored || !bytes.Equal(readFile(t, path), initial) {
		t.Fatalf("result=%+v error=%v bytes=%q", result, err, readFile(t, path))
	}
}

func TestRollbackFailureIsExplicitAndNeverOverwritesDrift(t *testing.T) {
	initial := []byte("onboarding = false\n")
	external := []byte("onboarding = false\n# external writer\n")
	path := writeConfig(t, initial, 0o600)
	runner := &fakeRunner{run: func(_ context.Context, _ string, args []string, _ int) (command.Result, error) {
		if slicesEnd(args, "config", "check") {
			return command.Result{Stdout: []byte("config: ok\n")}, nil
		}
		if err := os.WriteFile(path, external, 0o600); err != nil {
			panic(err)
		}
		return command.Result{ExitCode: 1}, &command.RunError{Kind: command.ErrorExit, Cause: errors.New("reload failed")}
	}}
	host := Host{Runner: runner, ConfigPath: path}
	snapshot, _ := host.Read(context.Background(), "plugin.owner")
	result, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}})
	if !errors.Is(err, ErrRollback) || result.RolledBack || !bytes.Equal(readFile(t, path), external) {
		t.Fatalf("result=%+v error=%v bytes=%q", result, err, readFile(t, path))
	}
}

func TestOversizedConcurrentReplacementBeforeRollbackIsPreserved(t *testing.T) {
	path := writeConfig(t, []byte("onboarding=false\n"), 0o600)
	oversized := bytes.Repeat([]byte{'#'}, MaxConfigBytes+1)
	reloads := 0
	runner := &fakeRunner{run: func(_ context.Context, _ string, args []string, _ int) (command.Result, error) {
		if slicesEnd(args, "config", "check") {
			return command.Result{Stdout: []byte("config: ok\n")}, nil
		}
		reloads++
		if err := os.WriteFile(path, oversized, 0o600); err != nil {
			t.Fatal(err)
		}
		return command.Result{ExitCode: 1}, &command.RunError{Kind: command.ErrorExit, Cause: errors.New("reload failed")}
	}}
	host := Host{Runner: runner, ConfigPath: path}
	snapshot, err := host.Read(context.Background(), "plugin.owner")
	if err != nil {
		t.Fatal(err)
	}
	result, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}})
	if !errors.Is(err, ErrRollback) || !result.Changed || result.RolledBack || result.EffectiveRestored || reloads != 1 || !bytes.Equal(readFile(t, path), oversized) {
		t.Fatalf("result=%+v reloads=%d error=%v", result, reloads, err)
	}
}

func TestCommittedRollbackWriteErrorStillReloadsRestoredConfig(t *testing.T) {
	path := writeConfig(t, []byte("onboarding=false\n"), 0o600)
	previousRevision := documentstore.Revision{1}
	writtenRevision := documentstore.Revision{2}
	store := &fakeDocumentStore{
		document: documentstore.Document{Bytes: []byte("onboarding=false\n"), Revision: previousRevision},
		write: func(ctx context.Context, _ string, _ []byte, expected documentstore.Revision, call int) (documentstore.Revision, error) {
			switch call {
			case 1:
				if expected != previousRevision {
					t.Fatalf("publish expected revision = %s", expected)
				}
				return writtenRevision, nil
			case 2:
				if expected != writtenRevision {
					t.Fatalf("rollback expected revision = %s", expected)
				}
				deadline, ok := ctx.Deadline()
				remaining := time.Until(deadline)
				if !ok || remaining < 4*time.Second || remaining > 5*time.Second {
					t.Fatalf("rollback deadline remaining=%v present=%t", remaining, ok)
				}
				return previousRevision, errors.New("directory fsync failed after rollback rename")
			default:
				t.Fatalf("unexpected write %d", call)
				return documentstore.Revision{}, nil
			}
		},
	}
	reloads := 0
	runner := &fakeRunner{run: func(_ context.Context, _ string, args []string, _ int) (command.Result, error) {
		if slicesEnd(args, "config", "check") {
			return command.Result{Stdout: []byte("config: ok\n")}, nil
		}
		reloads++
		if reloads == 1 {
			return command.Result{ExitCode: 1}, &command.RunError{Kind: command.ErrorExit, Cause: errors.New("reload failed")}
		}
		return command.Result{Stdout: []byte(reloadOK)}, nil
	}}
	host := Host{
		Runner:     runner,
		ConfigPath: path,
		openStore: func(string) (documentStore, string, error) {
			return store, filepath.Base(path), nil
		},
	}
	result, err := host.Apply(context.Background(), Request{
		Owner:    "plugin.owner",
		Expected: previousRevision,
		Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}},
	})
	if !errors.Is(err, ErrRollback) || !strings.Contains(err.Error(), "directory fsync failed") || !result.RolledBack || !result.EffectiveRestored || result.Revision != previousRevision || reloads != 2 {
		t.Fatalf("result=%+v reloads=%d error=%v", result, reloads, err)
	}
}

func TestCancellationAfterWriteUsesIndependentRollbackContext(t *testing.T) {
	initial := []byte("onboarding = false\n")
	path := writeConfig(t, initial, 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	reloads := 0
	runner := &fakeRunner{run: func(callCtx context.Context, _ string, args []string, _ int) (command.Result, error) {
		if slicesEnd(args, "config", "check") {
			return command.Result{Stdout: []byte("config: ok\n")}, nil
		}
		reloads++
		if reloads == 1 {
			cancel()
			return command.Result{Canceled: true, ExitCode: -1}, &command.RunError{Kind: command.ErrorCanceled, Cause: context.Canceled}
		}
		if callCtx.Err() != nil {
			t.Fatalf("rollback inherited cancellation: %v", callCtx.Err())
		}
		return command.Result{Stdout: []byte(reloadOK)}, nil
	}}
	host := Host{Runner: runner, ConfigPath: path}
	snapshot, _ := host.Read(context.Background(), "plugin.owner")
	result, err := host.Apply(ctx, Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}})
	if !errors.Is(err, context.Canceled) || result.Revision != snapshot.Revision || !result.RolledBack || !result.EffectiveRestored || !bytes.Equal(readFile(t, path), initial) {
		t.Fatalf("result=%+v error=%v bytes=%q", result, err, readFile(t, path))
	}
}

func TestDeadlineFailureAfterWriteRestoresThenReturnsCanceledKind(t *testing.T) {
	initial := []byte("onboarding=false\n")
	path := writeConfig(t, initial, 0o600)
	reloads := 0
	runner := &fakeRunner{run: func(callCtx context.Context, _ string, args []string, _ int) (command.Result, error) {
		if slicesEnd(args, "config", "check") {
			return command.Result{Stdout: []byte("config: ok\n")}, nil
		}
		reloads++
		if reloads == 1 {
			return command.Result{ExitCode: -1}, context.DeadlineExceeded
		}
		if callCtx.Err() != nil {
			t.Fatalf("rollback reload inherited deadline: %v", callCtx.Err())
		}
		return command.Result{Stdout: []byte(reloadOK)}, nil
	}}
	host := Host{Runner: runner, ConfigPath: path}
	snapshot, _ := host.Read(context.Background(), "plugin.owner")
	result, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}})
	var operation *Error
	if !errors.As(err, &operation) || operation.Kind != FailureCanceled || !errors.Is(err, context.DeadlineExceeded) || !result.RolledBack || !result.EffectiveRestored || !bytes.Equal(readFile(t, path), initial) {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestCancellationKindsAtLockPublicationAndCommandBoundaries(t *testing.T) {
	causes := []error{context.Canceled, context.DeadlineExceeded}
	for _, cause := range causes {
		t.Run(cause.Error()+"/lock", func(t *testing.T) {
			path := writeConfig(t, []byte("onboarding=false\n"), 0o600)
			host := Host{
				Runner:     &fakeRunner{},
				ConfigPath: path,
				acquireLock: func(context.Context, string) (io.Closer, error) {
					return nil, cause
				},
			}
			_, err := host.Apply(context.Background(), Request{Owner: "plugin.owner"})
			var operation *Error
			if !errors.As(err, &operation) || operation.Kind != FailureCanceled || !errors.Is(err, cause) {
				t.Fatalf("lock error=%T %v", err, err)
			}
		})
		t.Run(cause.Error()+"/publication", func(t *testing.T) {
			path := writeConfig(t, []byte("onboarding=false\n"), 0o600)
			revision := documentstore.Revision{1}
			store := &fakeDocumentStore{
				document: documentstore.Document{Bytes: []byte("onboarding=false\n"), Revision: revision},
				write: func(context.Context, string, []byte, documentstore.Revision, int) (documentstore.Revision, error) {
					return documentstore.Revision{}, cause
				},
			}
			host := Host{
				Runner:     &fakeRunner{},
				ConfigPath: path,
				openStore: func(string) (documentStore, string, error) {
					return store, filepath.Base(path), nil
				},
			}
			_, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: revision, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}})
			var operation *Error
			if !errors.As(err, &operation) || operation.Kind != FailureCanceled || !errors.Is(err, cause) {
				t.Fatalf("publication error=%T %v", err, err)
			}
		})
		result := command.Result{Stdout: []byte("out"), Stderr: []byte("err")}
		var operation *Error
		err := classifyCommandFailure(FailureReload, cause, result)
		if !errors.As(err, &operation) || operation.Kind != FailureCanceled || !errors.Is(err, cause) || operation.Diagnostics != "outerr" {
			t.Fatalf("command error=%T %v", err, err)
		}
	}
}

func TestConcurrentWritersWithOneRevisionHaveOneWinner(t *testing.T) {
	path := writeConfig(t, []byte("onboarding = false\n"), 0o600)
	ready := make(chan struct{}, 1)
	release := make(chan struct{})
	runner := &fakeRunner{run: func(_ context.Context, _ string, args []string, _ int) (command.Result, error) {
		if slicesEnd(args, "config", "check") {
			select {
			case ready <- struct{}{}:
				<-release
			default:
			}
			return command.Result{Stdout: []byte("config: ok\n")}, nil
		}
		return command.Result{Stdout: []byte(reloadOK)}, nil
	}}
	host := Host{Runner: runner, ConfigPath: path}
	snapshot, _ := host.Read(context.Background(), "plugin.owner")
	type outcome struct {
		result Result
		err    error
	}
	outcomes := make(chan outcome, 2)
	for _, key := range []string{"prefix+a", "prefix+b"} {
		go func(key string) {
			result, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: key, Action: "plugin.owner.open"}}})
			outcomes <- outcome{result: result, err: err}
		}(key)
	}
	<-ready
	time.Sleep(20 * time.Millisecond)
	if calls := runner.snapshotCalls(); len(calls) != 1 {
		t.Fatalf("second writer entered transaction before first reload: calls=%+v", calls)
	}
	close(release)
	winners, drifts := 0, 0
	for range 2 {
		result := <-outcomes
		switch {
		case result.err == nil:
			winners++
		case errors.Is(result.err, ErrDrift):
			drifts++
		default:
			t.Fatalf("unexpected outcome: %+v %v", result.result, result.err)
		}
	}
	if winners != 1 || drifts != 1 {
		t.Fatalf("winners=%d drifts=%d", winners, drifts)
	}
	assertNoCandidates(t, filepath.Dir(path))
}

func TestSuccessiveRevisionsCannotPublishBeforeEarlierReload(t *testing.T) {
	path := writeConfig(t, []byte("onboarding = false\n"), 0o600)
	firstReload := make(chan struct{})
	releaseFirstReload := make(chan struct{})
	secondCheck := make(chan struct{}, 1)
	var mu sync.Mutex
	var reloaded [][]byte
	checks := 0
	runner := &fakeRunner{run: func(_ context.Context, _ string, args []string, _ int) (command.Result, error) {
		if slicesEnd(args, "config", "check") {
			mu.Lock()
			checks++
			checkIndex := checks
			mu.Unlock()
			if checkIndex == 2 {
				secondCheck <- struct{}{}
			}
			return command.Result{Stdout: []byte("config: ok\n")}, nil
		}
		bytesAtReload := readFile(t, path)
		mu.Lock()
		reloaded = append(reloaded, bytesAtReload)
		reloadIndex := len(reloaded)
		mu.Unlock()
		if reloadIndex == 1 {
			close(firstReload)
			<-releaseFirstReload
		}
		return command.Result{Stdout: []byte(reloadOK)}, nil
	}}
	hostA := Host{Runner: runner, ConfigPath: path}
	initial, err := hostA.Read(context.Background(), "plugin.owner")
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := hostA.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: initial.Revision, Bindings: []Binding{{Key: "prefix+a", Action: "plugin.owner.open"}}})
		firstDone <- err
	}()
	<-firstReload
	firstPublished, err := hostA.Read(context.Background(), "plugin.owner")
	if err != nil {
		t.Fatal(err)
	}
	secondAttempted := make(chan struct{})
	hostB := Host{
		Runner:     runner,
		ConfigPath: path,
		acquireLock: func(ctx context.Context, path string) (io.Closer, error) {
			close(secondAttempted)
			return acquireTransactionLock(ctx, path)
		},
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := hostB.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: firstPublished.Revision, Bindings: []Binding{{Key: "prefix+b", Action: "plugin.owner.open"}}})
		secondDone <- err
	}()
	<-secondAttempted
	select {
	case <-secondCheck:
		t.Fatal("second revision reached config check before first reload completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseFirstReload)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotReloads := append([][]byte(nil), reloaded...)
	mu.Unlock()
	if len(gotReloads) != 2 || !bytes.Contains(gotReloads[0], []byte("prefix+a")) || bytes.Contains(gotReloads[0], []byte("prefix+b")) || !bytes.Contains(gotReloads[1], []byte("prefix+b")) {
		t.Fatalf("reload snapshots=%q", gotReloads)
	}
}

func TestCancellationWhileWaitingForTransactionLock(t *testing.T) {
	path := writeConfig(t, []byte("onboarding = false\n"), 0o600)
	firstCheck := make(chan struct{})
	release := make(chan struct{})
	runner := &fakeRunner{run: func(_ context.Context, _ string, args []string, _ int) (command.Result, error) {
		if slicesEnd(args, "config", "check") {
			close(firstCheck)
			<-release
			return command.Result{Stdout: []byte("config: ok\n")}, nil
		}
		return command.Result{Stdout: []byte(reloadOK)}, nil
	}}
	host := Host{Runner: runner, ConfigPath: path}
	snapshot, _ := host.Read(context.Background(), "plugin.owner")
	firstDone := make(chan error, 1)
	go func() {
		_, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+a", Action: "plugin.owner.open"}}})
		firstDone <- err
	}()
	<-firstCheck
	secondAttempted := make(chan struct{})
	hostB := Host{
		Runner:     runner,
		ConfigPath: path,
		acquireLock: func(ctx context.Context, path string) (io.Closer, error) {
			close(secondAttempted)
			return acquireTransactionLock(ctx, path)
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		_, err := hostB.Apply(ctx, Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+b", Action: "plugin.owner.open"}}})
		secondDone <- err
	}()
	<-secondAttempted
	cancel()
	err := <-secondDone
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting writer error=%v", err)
	}
	if calls := runner.snapshotCalls(); len(calls) != 1 {
		t.Fatalf("canceled writer ran commands: %+v", calls)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestMalformedManagedBytesFailOwnershipClosed(t *testing.T) {
	for _, suffix := range []string{
		beginNone + "\n" + endMarker + "\n",
		beginNone + "\n" + ownerPrefix + "plugin.owner\n[[keys.command]]\nkey='prefix+x'\ntype='plugin_action'\ncommand='plugin.owner.open'\n",
		beginNone + "\n" + ownerPrefix + "plugin.owner\n[[keys.command]]\nkey='prefix+x'\ntype='plugin_action'\ncommand='other.open'\n" + endMarker + "\n",
	} {
		path := writeConfig(t, []byte("onboarding=false\n"+suffix), 0o600)
		host := Host{Runner: &fakeRunner{}, ConfigPath: path}
		if _, err := host.Read(context.Background(), "plugin.owner"); !errors.Is(err, ErrOwnership) {
			t.Fatalf("suffix=%q error=%v", suffix, err)
		}
	}
}

func TestManagedMarkerStateMachineBoundaries(t *testing.T) {
	command := "[[keys.command]]\nkey = 'prefix+x'\ntype = 'plugin_action'\ncommand = 'plugin.owner.open'\n"
	group := ownerPrefix + "plugin.owner\n" + command
	validNone := beginNone + "\n" + group + endMarker + "\n"
	validLF := "x=1\n" + beginLF + "\n" + group + endMarker + "\n"
	for name, data := range map[string]string{
		"separator none": validNone,
		"separator lf":   validLF,
	} {
		t.Run(name, func(t *testing.T) {
			section, err := parseManaged([]byte(data))
			if err != nil || !section.present || len(section.groups) != 1 {
				t.Fatalf("section=%+v error=%v", section, err)
			}
		})
	}

	invalid := []struct {
		name string
		data string
		want string
	}{
		{"unsupported marker", markerPrefix + "future\n", "unsupported managed marker"},
		{"end only", endMarker + "\n", "unmatched managed section end"},
		{"begin only", beginNone + "\n", "incomplete managed section"},
		{"nested begin", beginNone + "\n" + beginLF + "\n" + endMarker + "\n", "duplicate or nested managed section"},
		{"duplicate end", beginNone + "\n" + group + endMarker + "\n" + endMarker + "\n", "unmatched managed section end"},
		{"owner outside", ownerPrefix + "plugin.owner\n", "owner marker outside managed section"},
		{"empty owners", beginNone + "\n" + endMarker + "\n", "managed section has no owners"},
		{"lf at start", beginLF + "\n" + group + endMarker + "\n", "managed separator is missing"},
		{"end without LF", beginNone + "\n" + group + endMarker, "managed section markers must end with LF"},
		{"invalid owner", beginNone + "\n" + ownerPrefix + "Bad.Owner\n" + command + endMarker + "\n", "invalid or duplicate owner"},
		{"duplicate owner", beginNone + "\n" + group + group + endMarker + "\n", "invalid or duplicate owner"},
		{"noncanonical valid TOML", beginNone + "\n" + ownerPrefix + "plugin.owner\n[[keys.command]]\nkey='prefix+x'\ntype='plugin_action'\ncommand='plugin.owner.open'\n" + endMarker + "\n", "bytes are not canonical"},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseManaged([]byte(test.data))
			if !errors.Is(err, ErrOwnership) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want substring %q", err, test.want)
			}
		})
	}
}

func TestSplitLinesAndCRLFPreservation(t *testing.T) {
	data := []byte("\nabc\r\nlast")
	got := splitLines(data)
	want := []line{
		{start: 0, end: 1, content: ""},
		{start: 1, end: 6, content: "abc"},
		{start: 6, end: 10, content: "last"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lines=%+v want=%+v", got, want)
	}

	initial := []byte("onboarding=false\r\n# unmanaged\r\n")
	added, changed, err := replaceOwned(initial, "plugin.owner", []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}})
	if err != nil || !changed || !bytes.HasPrefix(added, initial) {
		t.Fatalf("CRLF add changed=%t error=%v bytes=%q", changed, err, added)
	}
	removed, changed, err := replaceOwned(added, "plugin.owner", nil)
	if err != nil || !changed || !bytes.Equal(removed, initial) {
		t.Fatalf("CRLF remove changed=%t error=%v bytes=%q", changed, err, removed)
	}
	crlfManaged := bytes.ReplaceAll([]byte(beginNone+"\n"+ownerPrefix+"plugin.owner\n[[keys.command]]\nkey = 'prefix+x'\ntype = 'plugin_action'\ncommand = 'plugin.owner.open'\n"+endMarker+"\n"), []byte("\n"), []byte("\r\n"))
	if _, err := parseManaged(crlfManaged); !errors.Is(err, ErrOwnership) {
		t.Fatalf("CRLF managed markers error=%v", err)
	}
}

func TestMarkerShapedMultilineStringContentIsNeverManaged(t *testing.T) {
	initial := []byte("basic = \"\"\"\n" + beginNone + "\n" + ownerPrefix + "plugin.owner\n" + endMarker + "\n\"\"\"\n" +
		"literal = '''\n" + beginLF + "\n" + ownerPrefix + "plugin.owner\n" + endMarker + "\n'''\n")
	path := writeConfig(t, initial, 0o600)
	host := Host{Runner: &fakeRunner{}, ConfigPath: path}
	snapshot, err := host.Read(context.Background(), "plugin.owner")
	if err != nil || len(snapshot.Bindings) != 0 {
		t.Fatalf("decoy snapshot=%+v error=%v", snapshot, err)
	}
	bindings := []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}
	if _, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: bindings}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); !bytes.HasPrefix(got, initial) {
		t.Fatalf("multiline string bytes changed:\ngot  %q\nwant prefix %q", got, initial)
	}
	applyBindings(t, host, "plugin.owner", nil)
	if got := readFile(t, path); !bytes.Equal(got, initial) {
		t.Fatalf("decoy config was not restored exactly:\ngot  %q\nwant %q", got, initial)
	}
}

func TestOwnedRoundTripProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		prefix := rapid.SampledFrom([]string{"", "onboarding=false\n", "onboarding=false", "# comment\n"}).Draw(t, "prefix")
		count := rapid.IntRange(1, 8).Draw(t, "count")
		bindings := make([]Binding, count)
		for i := range bindings {
			bindings[i] = Binding{
				Key:         fmt.Sprintf("prefix+%d", i),
				Action:      fmt.Sprintf("plugin.owner.action-%d", i),
				Description: rapid.StringMatching(`[A-Za-z0-9 ]{0,20}`).Draw(t, fmt.Sprintf("description-%d", i)),
			}
		}
		added, changed, err := replaceOwned([]byte(prefix), "plugin.owner", bindings)
		if err != nil || !changed {
			t.Fatalf("add changed=%t error=%v", changed, err)
		}
		section, err := parseManaged(added)
		if err != nil || !reflect.DeepEqual(section.bindings("plugin.owner"), bindings) {
			t.Fatalf("section=%+v error=%v", section, err)
		}
		removed, changed, err := replaceOwned(added, "plugin.owner", nil)
		if err != nil || !changed || !bytes.Equal(removed, []byte(prefix)) {
			t.Fatalf("remove changed=%t error=%v got=%q want=%q", changed, err, removed, prefix)
		}
	})
}

func TestEffectiveConfigPathAndHostValidation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERDR_CONFIG_PATH", filepath.Join(root, "explicit.toml"))
	if got, err := EffectiveConfigPath(); err != nil || got != filepath.Join(root, "explicit.toml") {
		t.Fatalf("path=%q error=%v", got, err)
	}
	for _, host := range []Host{
		{ConfigPath: filepath.Join(root, "config.toml")},
		{Runner: &fakeRunner{}, ConfigPath: "relative.toml"},
		{Runner: &fakeRunner{}, ConfigPath: filepath.Join(root, "config.toml"), Session: "default"},
		{Runner: &fakeRunner{}, Herdr: "/tmp/herdr=bad", ConfigPath: filepath.Join(root, "config.toml")},
	} {
		if _, err := host.Read(context.Background(), "plugin.owner"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("host=%+v error=%v", host, err)
		}
	}
	t.Setenv("HERDR_CONFIG_PATH", "")
	t.Setenv("XDG_CONFIG_HOME", root)
	if got, err := EffectiveConfigPath(); err != nil || got != filepath.Join(root, "herdr", "config.toml") {
		t.Fatalf("default path=%q error=%v", got, err)
	}
	path4096 := "/" + strings.Repeat("a", 4095)
	if got, err := validateConfigPath(path4096); err != nil || got != path4096 {
		t.Fatalf("4096-byte path=(%d, %v)", len(got), err)
	}
	for _, invalid := range []string{
		"/" + strings.Repeat("a", 4096),
		"/tmp/nul\x00path",
		"/tmp/../tmp/config.toml",
		"",
	} {
		if _, err := validateConfigPath(invalid); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid path length=%d error=%v", len(invalid), err)
		}
	}
}

func TestConfigAndBindingBounds(t *testing.T) {
	tooLarge := writeConfig(t, bytes.Repeat([]byte{'#'}, MaxConfigBytes+1), 0o600)
	host := Host{Runner: &fakeRunner{}, ConfigPath: tooLarge}
	if _, err := host.Read(context.Background(), "plugin.owner"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("large read error=%v", err)
	}

	path := writeConfig(t, bytes.Repeat([]byte{' '}, MaxConfigBytes), 0o600)
	host.ConfigPath = path
	snapshot, err := host.Read(context.Background(), "plugin.owner")
	if err != nil {
		t.Fatal(err)
	}
	_, err = host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized result error=%v", err)
	}

	base := writeConfig(t, []byte("onboarding=false\n"), 0o600)
	host.ConfigPath = base
	snapshot, _ = host.Read(context.Background(), "plugin.owner")
	requests := []Request{
		{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: make([]Binding, MaxBindings+1)},
		{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: strings.Repeat("x", MaxKeyBytes+1), Action: "plugin.owner.open"}}},
		{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open", Description: strings.Repeat("x", MaxDescription+1)}}},
	}
	for _, request := range requests {
		if _, err := host.Apply(context.Background(), request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("request=%+v error=%v", request, err)
		}
	}

	bindings := make([]Binding, MaxBindings)
	for i := range bindings {
		prefix := fmt.Sprintf("prefix+%03d-", i)
		bindings[i] = Binding{
			Key:         prefix + strings.Repeat("k", MaxKeyBytes-len(prefix)),
			Action:      "plugin.owner.open",
			Description: strings.Repeat("d", MaxDescription),
		}
	}
	if err := validateBindings("plugin.owner", bindings); err != nil {
		t.Fatalf("exact binding limits rejected: %v", err)
	}
	for name, binding := range map[string]Binding{
		"empty key":                 {Action: "plugin.owner.open"},
		"NUL key":                   {Key: "prefix+\x00", Action: "plugin.owner.open"},
		"newline key":               {Key: "prefix+x\n", Action: "plugin.owner.open"},
		"invalid UTF-8 key":         {Key: string([]byte{0xff}), Action: "plugin.owner.open"},
		"invalid owner action":      {Key: "prefix+x", Action: "plugin.owner.Bad"},
		"different owner action":    {Key: "prefix+x", Action: "plugin.other.open"},
		"NUL description":           {Key: "prefix+x", Action: "plugin.owner.open", Description: "bad\x00"},
		"invalid UTF-8 description": {Key: "prefix+x", Action: "plugin.owner.open", Description: string([]byte{0xff})},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateBindings("plugin.owner", []Binding{binding}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	for _, owner := range []string{"", "Plugin.owner", "plugin/owner", "a" + strings.Repeat("b", 64)} {
		if err := validateOwner(owner); err == nil {
			t.Fatalf("invalid owner %q accepted", owner)
		}
	}
}

func TestGeneratedCandidateConfigExactSizeBoundary(t *testing.T) {
	binding := []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}
	managed, changed, err := replaceOwned(nil, "plugin.owner", binding)
	if err != nil || !changed {
		t.Fatal(err)
	}
	makeInitial := func(total int) []byte {
		length := total - len(managed)
		if length < 2 {
			t.Fatalf("invalid fixture length %d", length)
		}
		return []byte("#" + strings.Repeat("x", length-2) + "\n")
	}
	for _, delta := range []int{0, 1} {
		t.Run(fmt.Sprintf("delta-%d", delta), func(t *testing.T) {
			path := writeConfig(t, makeInitial(MaxConfigBytes+delta), 0o600)
			runner := &fakeRunner{}
			host := Host{Runner: runner, ConfigPath: path}
			snapshot, err := host.Read(context.Background(), "plugin.owner")
			if err != nil {
				t.Fatal(err)
			}
			result, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: binding})
			if delta == 0 {
				if err != nil || !result.Changed || len(readFile(t, path)) != MaxConfigBytes {
					t.Fatalf("exact result=%+v size=%d error=%v", result, len(readFile(t, path)), err)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) || result.Changed || len(runner.snapshotCalls()) != 0 {
				t.Fatalf("plus-one result=%+v calls=%+v error=%v", result, runner.snapshotCalls(), err)
			}
		})
	}
}

func TestCanceledRequestDoesNotCreateCandidateOrRunCommand(t *testing.T) {
	path := writeConfig(t, []byte("onboarding=false\n"), 0o600)
	runner := &fakeRunner{}
	host := Host{Runner: runner, ConfigPath: path}
	snapshot, _ := host.Read(context.Background(), "plugin.owner")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := host.Apply(ctx, Request{Owner: "plugin.owner", Expected: snapshot.Revision, Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}})
	if !errors.Is(err, context.Canceled) || len(runner.snapshotCalls()) != 0 {
		t.Fatalf("error=%v calls=%+v", err, runner.snapshotCalls())
	}
	assertNoCandidates(t, filepath.Dir(path))
}

func applyBindings(t *testing.T, host Host, owner string, bindings []Binding) Result {
	t.Helper()
	snapshot, err := host.Read(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	result, err := host.Apply(context.Background(), Request{Owner: owner, Expected: snapshot.Revision, Bindings: bindings})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertSnapshot(t *testing.T, host Host, owner string, want []Binding) {
	t.Helper()
	snapshot, err := host.Read(context.Background(), owner)
	if err != nil || !reflect.DeepEqual(snapshot.Bindings, want) {
		t.Fatalf("snapshot=%+v error=%v want=%+v", snapshot, err, want)
	}
}

func writeConfig(t *testing.T, data []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func rawOwnerGroup(t *testing.T, data []byte, owner string) []byte {
	t.Helper()
	section, err := parseManaged(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range section.groups {
		if group.owner == owner {
			return append([]byte(nil), data[group.start:group.end]...)
		}
	}
	t.Fatalf("owner %q not found", owner)
	return nil
}

func configArg(args []string) string {
	for _, arg := range args {
		if strings.HasPrefix(arg, "HERDR_CONFIG_PATH=") {
			return strings.TrimPrefix(arg, "HERDR_CONFIG_PATH=")
		}
	}
	return ""
}

func slicesEnd(values []string, suffix ...string) bool {
	return len(values) >= len(suffix) && reflect.DeepEqual(values[len(values)-len(suffix):], suffix)
}

func assertNoCandidates(t *testing.T, directory string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(directory, ".herdr-plugin-kit-check-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("candidate files=%q error=%v", matches, err)
	}
}

func TestRollbackTimeoutIsBounded(t *testing.T) {
	if rollbackTimeout != 5*time.Second {
		t.Fatalf("rollback timeout=%v", rollbackTimeout)
	}
}
