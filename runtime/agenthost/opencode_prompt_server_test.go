package agenthost

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

type openCodePromptSpecRunner struct {
	run    runnerFunc
	stdout string
	spec   func(command.Spec)
}

func (runner openCodePromptSpecRunner) Run(ctx context.Context, executable string, args []string) (command.Result, error) {
	return runner.run(ctx, executable, args)
}

func (runner openCodePromptSpecRunner) RunSpec(ctx context.Context, spec command.Spec) (command.Result, error) {
	if runner.spec != nil {
		runner.spec(spec)
	}
	if spec.StdoutObserver != nil && runner.stdout != "" {
		_, _ = io.WriteString(spec.StdoutObserver, runner.stdout)
	}
	return runner.run(ctx, spec.Executable, spec.Args)
}

func TestOpenCodePromptServerUsesLiteralLoopbackArgvAndExactStop(t *testing.T) {
	var (
		mu        sync.Mutex
		calls     [][]string
		directory string
		started   = make(chan struct{})
		startOne  sync.Once
	)
	run := runnerFunc(func(ctx context.Context, executable string, args []string) (command.Result, error) {
		mu.Lock()
		calls = append(calls, append([]string{executable}, args...))
		mu.Unlock()
		startOne.Do(func() { close(started) })
		<-ctx.Done()
		return command.Result{Canceled: true}, &command.RunError{Kind: command.ErrorCanceled, Cause: ctx.Err()}
	})
	runner := openCodePromptSpecRunner{
		run: run, stdout: "Warning: unsecured\nopencode server listening on http://127.0.0.1:49152\n",
		spec: func(spec command.Spec) { directory = spec.Dir },
	}
	launcher := nativeOpenCodePromptServerLauncher{
		runner:  runner,
		reserve: func() (int, error) { return 49152, nil },
		health: func(context.Context, string) error {
			<-started
			return nil
		},
		wait: func(context.Context, time.Duration) error { return nil },
	}
	server, stop, err := launcher.Start(context.Background(), "/opt/opencode", "/tmp/project")
	if err != nil || server != (OpenCodeServer{URL: "http://127.0.0.1:49152", Directory: "/tmp/project"}) || stop == nil {
		t.Fatalf("server=%+v stop=%t error=%v", server, stop != nil, err)
	}
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := [][]string{{"/opt/opencode", "serve", "--hostname", "127.0.0.1", "--port", "49152"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%q want=%q", calls, want)
	}
	if directory != "/tmp/project" {
		t.Fatalf("directory=%q", directory)
	}
}

func TestOpenCodePromptServerRejectsInvalidInputAndEarlyExit(t *testing.T) {
	for _, input := range []struct{ executable, directory string }{
		{"", "/tmp/project"}, {"/opt/other", "/tmp/project"}, {"relative/opencode", "/tmp/project"}, {"opencode", "relative"},
	} {
		calls := 0
		run := runnerFunc(func(context.Context, string, []string) (command.Result, error) { calls++; return command.Result{}, nil })
		launcher := nativeOpenCodePromptServerLauncher{runner: openCodePromptSpecRunner{run: run}}
		if _, _, err := launcher.Start(context.Background(), input.executable, input.directory); err == nil || calls != 0 {
			t.Fatalf("input=%+v calls=%d error=%v", input, calls, err)
		}
	}
	launcher := nativeOpenCodePromptServerLauncher{
		runner: openCodePromptSpecRunner{run: runnerFunc(func(context.Context, string, []string) (command.Result, error) {
			return command.Result{ExitCode: 1}, errors.New("bind failed")
		})},
		reserve: func() (int, error) { return 49152, nil },
		health:  func(context.Context, string) error { return errors.New("not ready") },
		wait:    func(context.Context, time.Duration) error { return nil },
	}
	if _, _, err := launcher.Start(context.Background(), "opencode", "/tmp/project"); err == nil {
		t.Fatal("early server exit was accepted")
	}
}

func TestOpenCodePromptServerRetriesAConnectedButUnreadyHealthHandler(t *testing.T) {
	started := make(chan struct{})
	run := runnerFunc(func(ctx context.Context, _ string, _ []string) (command.Result, error) {
		close(started)
		<-ctx.Done()
		return command.Result{Canceled: true}, &command.RunError{Kind: command.ErrorCanceled, Cause: ctx.Err()}
	})
	runner := openCodePromptSpecRunner{run: run, stdout: "opencode server listening on http://127.0.0.1:49152\n"}
	calls := 0
	launcher := nativeOpenCodePromptServerLauncher{
		runner:  runner,
		reserve: func() (int, error) { return 49152, nil },
		health: func(ctx context.Context, _ string) error {
			<-started
			calls++
			if calls == 1 {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		},
		wait:  func(context.Context, time.Duration) error { return nil },
		probe: time.Millisecond,
	}
	_, stop, err := launcher.Start(context.Background(), "opencode", "/tmp/project")
	if err != nil || calls != 2 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodePromptServerRejectsHealthyCompetingListenerWithoutOwnedBindProof(t *testing.T) {
	var requests atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"healthy":true,"version":"1.18.30"}`))
	}))
	defer foreign.Close()
	port := foreign.Listener.Addr().(*net.TCPAddr).Port
	exited := make(chan struct{})
	run := runnerFunc(func(context.Context, string, []string) (command.Result, error) {
		close(exited)
		return command.Result{ExitCode: 1}, errors.New("bind failed")
	})
	launcher := nativeOpenCodePromptServerLauncher{
		runner:  openCodePromptSpecRunner{run: run},
		reserve: func() (int, error) { return port, nil },
		wait:    func(context.Context, time.Duration) error { <-exited; return nil },
	}
	server, stop, err := launcher.Start(context.Background(), "opencode", "/tmp/project")
	if err == nil || server != (OpenCodeServer{}) || stop != nil || requests.Load() != 0 {
		t.Fatalf("server=%+v stop=%t requests=%d error=%v", server, stop != nil, requests.Load(), err)
	}
}

func TestOpenCodePromptServerHealthRequiresExactSupportedVersion(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		want       bool
	}{
		{"supported", `{"healthy":true,"version":"1.18.30"}`, http.StatusOK, true},
		{"wrong version", `{"healthy":true,"version":"1.18.31"}`, http.StatusOK, false},
		{"unhealthy", `{"healthy":false,"version":"1.18.30"}`, http.StatusOK, false},
		{"trailing", `{"healthy":true,"version":"1.18.30"}{}`, http.StatusOK, false},
		{"status", `{"healthy":true,"version":"1.18.30"}`, http.StatusServiceUnavailable, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			err := readOpenCodePromptServerHealth(context.Background(), server.URL)
			if (err == nil) != test.want {
				t.Fatalf("error=%v want success=%t", err, test.want)
			}
		})
	}
}
