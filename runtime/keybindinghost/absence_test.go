package keybindinghost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/documentstore"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

func TestAbsentConfigReadNoopAndFirstCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	runner := &fakeRunner{}
	host := Host{Runner: runner, ConfigPath: path, Session: "named"}
	ctx := context.Background()
	current, err := host.Read(ctx, "plugin.owner")
	if err != nil || current.Revision != (documentstore.Revision{}) || len(current.Bindings) != 0 {
		t.Fatalf("absent snapshot: %+v %v", current, err)
	}
	result, err := host.Apply(ctx, Request{Owner: "plugin.owner"})
	if err != nil || result.Changed || result.Activated || result.Revision != (documentstore.Revision{}) || len(runner.snapshotCalls()) != 0 {
		t.Fatalf("read-only no-op: %+v %v", result, err)
	}
	assertConfigAbsent(t, path)
	result, err = host.Activate(ctx, Request{Owner: "plugin.owner"})
	if err != nil || !result.Activated || result.Changed || result.Revision != (documentstore.Revision{}) {
		t.Fatalf("activate defaults: %+v %v", result, err)
	}
	assertConfigAbsent(t, path)
	desired := []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}
	result, err = host.Apply(ctx, Request{Owner: "plugin.owner", Bindings: desired})
	if err != nil || !result.Changed || !result.Activated || result.Revision == (documentstore.Revision{}) {
		t.Fatalf("checked first creation: %+v %v", result, err)
	}
	assertSnapshot(t, host, "plugin.owner", desired)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("first creation permissions: %v %v", info, err)
	}
	if _, err := host.Apply(ctx, Request{Owner: "plugin.owner", Bindings: desired}); !errors.Is(err, ErrDrift) {
		t.Fatalf("stale absence overwritten: %v", err)
	}
	assertSnapshot(t, host, "plugin.owner", desired)
}

func assertConfigAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected absent configuration: %v", err)
	}
}

func TestAbsentConfigPublicationGuards(t *testing.T) {
	for _, scenario := range []string{"stale", "check failure", "canceled", "concurrent creation"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := error(ErrDrift)
			runner := &fakeRunner{run: func(_ context.Context, _ string, args []string, _ int) (command.Result, error) {
				if !slicesEnd(args, "config", "check") {
					t.Fatal("refused publication attempted reload")
				}
				switch scenario {
				case "check failure":
					return command.Result{ExitCode: 1}, &command.RunError{Kind: command.ErrorExit, Cause: errors.New("rejected")}
				case "canceled":
					cancel()
				case "concurrent creation":
					if err := os.WriteFile(path, []byte("onboarding=false\n# external\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return command.Result{Stdout: []byte("config: ok\n")}, nil
			}}
			request := Request{Owner: "plugin.owner", Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}}
			switch scenario {
			case "stale":
				request.Expected = documentstore.Revision{1}
			case "check failure":
				want = ErrCheck
			case "canceled":
				want = context.Canceled
			}
			result, err := (Host{Runner: runner, ConfigPath: path}).Apply(ctx, request)
			if !errors.Is(err, want) || result.Changed || result.Activated {
				t.Fatalf("result=%+v err=%v want=%v", result, err, want)
			}
			if scenario == "concurrent creation" {
				if got := string(readFile(t, path)); got != "onboarding=false\n# external\n" {
					t.Fatal("concurrent document overwritten")
				}
			} else {
				assertConfigAbsent(t, path)
			}
		})
	}
}

func TestAbsentConfigRealRollbackAndConflict(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "restore absence", true: "preserve external write"}[conflict], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			reloads := 0
			runner := &fakeRunner{run: func(_ context.Context, _ string, args []string, _ int) (command.Result, error) {
				if slicesEnd(args, "config", "check") {
					return command.Result{Stdout: []byte("config: ok\n")}, nil
				}
				reloads++
				if reloads == 1 {
					if conflict {
						if err := os.WriteFile(path, []byte("onboarding=false\n# external\n"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					return command.Result{ExitCode: 1}, &command.RunError{Kind: command.ErrorExit, Cause: errors.New("reload failure")}
				}
				return command.Result{Stdout: []byte(reloadOK)}, nil
			}}
			result, err := (Host{Runner: runner, ConfigPath: path}).Apply(context.Background(), Request{Owner: "plugin.owner", Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}})
			if conflict {
				if !errors.Is(err, ErrRollback) || !errors.Is(err, documentstore.ErrConflict) || result.RolledBack || result.EffectiveRestored || reloads != 1 || string(readFile(t, path)) != "onboarding=false\n# external\n" {
					t.Fatalf("external change lost: %+v %v reloads=%d", result, err, reloads)
				}
			} else {
				if !errors.Is(err, ErrReload) || !result.RolledBack || !result.EffectiveRestored || result.Revision != (documentstore.Revision{}) || reloads != 2 {
					t.Fatalf("rollback: %+v %v reloads=%d", result, err, reloads)
				}
				assertConfigAbsent(t, path)
			}
		})
	}
}

func TestAbsentConfigRollbackPublicationResults(t *testing.T) {
	for _, scenario := range []string{"remove conflict", "remove fsync", "restore reload", "committed write error"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			written := documentstore.Revision{2}
			injected := errors.New("injected durability failure")
			store := &fakeDocumentStore{readErr: os.ErrNotExist}
			store.write = func(_ context.Context, _ string, _ []byte, expected documentstore.Revision, call int) (documentstore.Revision, error) {
				if expected != (documentstore.Revision{}) || call != 1 {
					t.Fatal("first publication lost absence or rollback wrote empty bytes")
				}
				if scenario == "committed write error" {
					return written, injected
				}
				return written, nil
			}
			store.remove = func(ctx context.Context, name string, expected documentstore.Revision, limit int) (bool, error) {
				deadline, ok := ctx.Deadline()
				remaining := time.Until(deadline)
				if name != "config.toml" || expected != written || limit != MaxConfigBytes || ctx.Err() != nil || !ok || remaining < 4*time.Second || remaining > 5*time.Second {
					t.Fatal("rollback lost exact revision, bound, or independent budget")
				}
				switch scenario {
				case "remove conflict":
					return false, documentstore.ErrConflict
				case "remove fsync":
					return true, injected
				default:
					return true, nil
				}
			}
			reloads := 0
			runner := &fakeRunner{run: func(_ context.Context, _ string, args []string, _ int) (command.Result, error) {
				if slicesEnd(args, "config", "check") {
					return command.Result{Stdout: []byte("config: ok\n")}, nil
				}
				reloads++
				if (reloads == 1 && scenario != "committed write error") || scenario == "restore reload" {
					return command.Result{ExitCode: 1}, &command.RunError{Kind: command.ErrorExit, Cause: errors.New("reload failed")}
				}
				return command.Result{Stdout: []byte(reloadOK)}, nil
			}}
			host := Host{Runner: runner, ConfigPath: path, openStore: func(string) (documentStore, string, error) { return store, "config.toml", nil }}
			result, err := host.Apply(context.Background(), Request{Owner: "plugin.owner", Bindings: []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}})
			wantErr, wantReloads := error(ErrRollback), 2
			if scenario == "committed write error" {
				wantErr, wantReloads = ErrWrite, 1
			} else if scenario == "remove conflict" {
				wantReloads = 1
			}
			if !errors.Is(err, wantErr) || !result.Changed || result.RolledBack != (scenario != "remove conflict") || result.EffectiveRestored != (scenario != "remove conflict" && scenario != "restore reload") || reloads != wantReloads || store.writes != 1 {
				t.Fatalf("result=%+v err=%v reloads=%d writes=%d", result, err, reloads, store.writes)
			}
			if result.RolledBack && result.Revision != (documentstore.Revision{}) {
				t.Fatal("published removal did not report absence")
			}
		})
	}
}

func TestAbsentConfigConcurrentCreators(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var group sync.WaitGroup
	results := make(chan error, 2)
	for _, key := range []string{"prefix+x", "prefix+y"} {
		group.Go(func() {
			_, err := (Host{Runner: &fakeRunner{}, ConfigPath: path}).Apply(ctx, Request{Owner: "plugin.owner", Bindings: []Binding{{Key: key, Action: "plugin.owner.open"}}})
			results <- err
		})
	}
	group.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrDrift) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestRestoreAbsenceOnlyRemovesExactOwnedConfiguration(t *testing.T) {
	bindings := []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}}
	owned, _, err := replaceOwned(nil, "plugin.owner", bindings)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := replaceOwned(nil, "plugin.other", []Binding{{Key: "prefix+z", Action: "plugin.other.open"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"owned", "native config", "other owner", "comment tail", "empty file", "stale", "zero expectation", "nonempty request", "activate"} {
		t.Run(scenario, func(t *testing.T) {
			data := owned
			want := error(ErrOwnership)
			switch scenario {
			case "owned":
				want = nil
			case "native config":
				data, _, err = replaceOwned([]byte("onboarding=false\n"), "plugin.owner", bindings)
			case "other owner":
				data, _, err = replaceOwned(other, "plugin.owner", bindings)
			case "comment tail":
				data = append(append([]byte(nil), owned...), []byte("# preserve native comment\n")...)
			case "empty file":
				data = nil
			case "stale":
				want = ErrDrift
			case "zero expectation", "nonempty request", "activate":
				want = ErrInvalid
			}
			if err != nil {
				t.Fatal(err)
			}
			path := writeConfig(t, data, 0600)
			runner := &fakeRunner{}
			host := Host{Runner: runner, ConfigPath: path, Session: "named"}
			current, err := host.Read(context.Background(), "plugin.owner")
			if err != nil {
				t.Fatal(err)
			}
			request := Request{Owner: "plugin.owner", Expected: current.Revision, RestoreAbsence: true}
			if scenario == "stale" {
				request.Expected = documentstore.Revision{1}
			} else if scenario == "zero expectation" {
				request.Expected = documentstore.Revision{}
			} else if scenario == "nonempty request" {
				request.Bindings = bindings
			}
			var result Result
			if scenario == "activate" {
				result, err = host.Activate(context.Background(), request)
			} else {
				result, err = host.Apply(context.Background(), request)
			}
			if !errors.Is(err, want) {
				t.Fatalf("result=%+v err=%v want=%v", result, err, want)
			}
			if scenario == "owned" {
				if !result.Changed || !result.Activated || !result.RolledBack || !result.EffectiveRestored || result.Revision != (documentstore.Revision{}) || len(runner.snapshotCalls()) != 2 {
					t.Fatalf("restoration: %+v", result)
				}
				assertConfigAbsent(t, path)
			} else if result.Changed || len(runner.snapshotCalls()) != 0 || string(readFile(t, path)) != string(data) {
				t.Fatalf("refusal modified or activated configuration: %+v", result)
			}
		})
	}
}

func TestRestoreAbsenceReportsPartialRecoveryAndIgnoresLateCancellation(t *testing.T) {
	for _, scenario := range []string{"remove conflict", "remove fsync", "reload failure", "late cancellation"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := filepath.Join(t.TempDir(), "config.toml")
			owned, _, err := replaceOwned(nil, "plugin.owner", []Binding{{Key: "prefix+x", Action: "plugin.owner.open"}})
			if err != nil {
				t.Fatal(err)
			}
			published := documentstore.Revision{2}
			injected := errors.New("directory fsync failed after unlink")
			store := &fakeDocumentStore{document: documentstore.Document{Bytes: owned, Revision: published}}
			store.remove = func(_ context.Context, _ string, expected documentstore.Revision, bound int) (bool, error) {
				if expected != published || bound != MaxConfigBytes {
					t.Fatal("unchecked removal")
				}
				switch scenario {
				case "remove conflict":
					return false, documentstore.ErrConflict
				case "remove fsync":
					return true, injected
				case "late cancellation":
					cancel()
				}
				return true, nil
			}
			reloads := 0
			runner := &fakeRunner{run: func(recovery context.Context, _ string, args []string, _ int) (command.Result, error) {
				if slicesEnd(args, "config", "check") {
					return command.Result{Stdout: []byte("config: ok\n")}, nil
				}
				reloads++
				deadline, ok := recovery.Deadline()
				remaining := time.Until(deadline)
				if recovery.Err() != nil || !ok || remaining < 4*time.Second || remaining > 5*time.Second {
					t.Fatal("recovery did not retain independent bounded budget")
				}
				if scenario == "reload failure" {
					return command.Result{ExitCode: 1}, &command.RunError{Kind: command.ErrorExit, Cause: errors.New("reload failed")}
				}
				return command.Result{Stdout: []byte(reloadOK)}, nil
			}}
			host := Host{Runner: runner, ConfigPath: path, openStore: func(string) (documentStore, string, error) { return store, "config.toml", nil }}
			result, err := host.Apply(ctx, Request{Owner: "plugin.owner", Expected: published, RestoreAbsence: true})
			removed, activated := scenario != "remove conflict", scenario != "remove conflict" && scenario != "reload failure"
			wantErr := error(ErrRollback)
			if scenario == "late cancellation" {
				wantErr = nil
			}
			wantReloads := 1
			if !removed {
				wantReloads = 0
			}
			if !errors.Is(err, wantErr) || result.Changed != removed || result.RolledBack != removed || result.Activated != activated || result.EffectiveRestored != activated || reloads != wantReloads || store.writes != 0 {
				t.Fatalf("result=%+v err=%v reloads=%d writes=%d", result, err, reloads, store.writes)
			}
			if removed && result.Revision != (documentstore.Revision{}) {
				t.Fatal("published absence lost")
			}
		})
	}
}
