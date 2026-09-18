package agenthost

import (
	"context"
	"errors"
	"fmt"
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
	run           runnerFunc
	stdout        string
	stdoutForSpec func(command.Spec) string
	spec          func(command.Spec)
	observed      chan struct{}
}

func (runner openCodePromptSpecRunner) Run(ctx context.Context, executable string, args []string) (command.Result, error) {
	return runner.run(ctx, executable, args)
}

func (runner openCodePromptSpecRunner) RunSpec(ctx context.Context, spec command.Spec) (command.Result, error) {
	if runner.spec != nil {
		runner.spec(spec)
	}
	stdout := runner.stdout
	if runner.stdoutForSpec != nil {
		stdout = runner.stdoutForSpec(spec)
	}
	if spec.StdoutObserver != nil && stdout != "" {
		_, _ = io.WriteString(spec.StdoutObserver, stdout)
	}
	if runner.observed != nil {
		close(runner.observed)
	}
	return runner.run(ctx, spec.Executable, spec.Args)
}

func ownedOpenCodeLine(spec command.Spec) string {
	return "opencode server listening on http://127.0.0.1:" + spec.Args[len(spec.Args)-1] + "\n"
}

func blockingOpenCodeRun(ctx context.Context, _ string, _ []string) (command.Result, error) {
	<-ctx.Done()
	return command.Result{Canceled: true}, &command.RunError{Kind: command.ErrorCanceled, Cause: ctx.Err()}
}

func fixedOpenCodeTUIReserve(port int) func() (int, error) {
	return func() (int, error) { return port, nil }
}

func TestOpenCodeTUIEndpointArgsAreLiteralAndBounded(t *testing.T) {
	args, err := openCodeTUIEndpointArgs(fixedOpenCodeTUIReserve(49153))
	if err != nil || !reflect.DeepEqual(args, []string{"--hostname", "127.0.0.1", "--port", "49153"}) {
		t.Fatalf("args=%q error=%v", args, err)
	}
	for _, reserve := range []func() (int, error){
		func() (int, error) { return 0, nil },
		func() (int, error) { return 65536, nil },
		func() (int, error) { return 0, errors.New("unavailable") },
	} {
		if args, err := openCodeTUIEndpointArgs(reserve); err == nil || args != nil {
			t.Fatalf("invalid reservation args=%q error=%v", args, err)
		}
	}
}

func ownedOpenCodeTestRunner(run runnerFunc) (openCodePromptSpecRunner, func(context.Context, time.Duration) error) {
	observed := make(chan struct{})
	waits := 0
	runner := openCodePromptSpecRunner{run: run, stdoutForSpec: ownedOpenCodeLine, observed: observed}
	wait := func(ctx context.Context, _ time.Duration) error {
		waits++
		if waits == 1 {
			select {
			case <-observed:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return errors.New("unexpected extra wait")
	}
	return runner, wait
}

func TestOpenCodePromptServerConstantsAndOwnershipOutputBounds(t *testing.T) {
	if openCodePromptServerTimeout != 30*time.Second || openCodePromptServerPoll != 50*time.Millisecond || openCodePromptHealthTimeout != time.Second || openCodePromptServerStop != 5*time.Second {
		t.Fatal("OpenCode prompt server timing contract changed")
	}
	observer := newOpenCodeOwnershipObserver("http://127.0.0.1:49152")
	line := append([]byte(nil), observer.wantLine...)
	if _, err := observer.Write(line[:len(line)/2]); err != nil || observer.ready() {
		t.Fatalf("partial ownership line ready=%t error=%v", observer.ready(), err)
	}
	if _, err := observer.Write(line[len(line)/2:]); err != nil || !observer.ready() {
		t.Fatalf("complete ownership line ready=%t error=%v", observer.ready(), err)
	}
	if written, err := observer.Write([]byte("ignored")); written != len("ignored") || err != nil || len(observer.output) != len(line) {
		t.Fatalf("post-match write=%d output=%d error=%v", written, len(observer.output), err)
	}

	capped := newOpenCodeOwnershipObserver("http://127.0.0.1:49152")
	padding := make([]byte, openCodeOwnershipOutputLimit)
	if _, err := capped.Write(padding); err != nil {
		t.Fatal(err)
	}
	if _, err := capped.Write(capped.wantLine); err != nil || capped.ready() || len(capped.output) != openCodeOwnershipOutputLimit {
		t.Fatalf("capped ready=%t output=%d error=%v", capped.ready(), len(capped.output), err)
	}
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
	waits := 0
	launcher := nativeOpenCodePromptServerLauncher{
		runner:  runner,
		reserve: func() (int, error) { return 49152, nil },
		health: func(context.Context, string) error {
			<-started
			return nil
		},
		wait: func(context.Context, time.Duration) error {
			<-started
			waits++
			if waits == 1 {
				return nil
			}
			return errors.New("unexpected wait after ownership")
		},
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
		{"", "/tmp/project"}, {"/opt/other", "/tmp/project"}, {"relative/opencode", "/tmp/project"},
		{"/opt/../opencode", "/tmp/project"}, {"/opt/\n/opencode", "/tmp/project"},
		{"opencode", ""}, {"opencode", "relative"}, {"opencode", "/tmp/../tmp/project"}, {"opencode", "/tmp/project\x00"},
	} {
		calls := 0
		run := runnerFunc(func(context.Context, string, []string) (command.Result, error) { calls++; return command.Result{}, nil })
		launcher := nativeOpenCodePromptServerLauncher{runner: openCodePromptSpecRunner{run: run}}
		if _, _, err := launcher.Start(context.Background(), input.executable, input.directory); err == nil || calls != 0 {
			t.Fatalf("input=%+v calls=%d error=%v", input, calls, err)
		}
	}
	if _, _, err := (nativeOpenCodePromptServerLauncher{}).Start(context.Background(), "opencode", "/tmp/project"); err == nil {
		t.Fatal("nil runner accepted")
	}
	unsupported := nativeOpenCodePromptServerLauncher{
		runner:  runnerFunc(func(context.Context, string, []string) (command.Result, error) { return command.Result{}, nil }),
		reserve: func() (int, error) { return 49152, nil },
	}
	if _, _, err := unsupported.Start(context.Background(), "opencode", "/tmp/project"); err == nil {
		t.Fatal("runner without observed startup accepted")
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

func TestOpenCodePromptServerPortReservationBoundsAndDefault(t *testing.T) {
	reserveErr := errors.New("reserve failed")
	invalid := []struct {
		name string
		port int
		err  error
	}{
		{"error", 49152, reserveErr}, {"zero", 0, nil}, {"too high", 65536, nil},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			launcher := nativeOpenCodePromptServerLauncher{
				runner:  openCodePromptSpecRunner{run: runnerFunc(func(context.Context, string, []string) (command.Result, error) { calls++; return command.Result{}, nil })},
				reserve: func() (int, error) { return test.port, test.err },
			}
			if _, _, err := launcher.Start(context.Background(), "opencode", "/tmp/project"); err == nil || calls != 0 {
				t.Fatalf("port=%d calls=%d error=%v", test.port, calls, err)
			}
		})
	}
	for _, port := range []int{1, 65535} {
		t.Run(fmt.Sprintf("accepted-%d", port), func(t *testing.T) {
			runner, wait := ownedOpenCodeTestRunner(blockingOpenCodeRun)
			launcher := nativeOpenCodePromptServerLauncher{
				runner:  runner,
				reserve: func() (int, error) { return port, nil },
				health:  func(context.Context, string) error { return nil },
				wait:    wait,
			}
			_, stop, err := launcher.Start(context.Background(), "opencode", "/tmp/project")
			if err != nil {
				t.Fatal(err)
			}
			if err := stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
	runner, wait := ownedOpenCodeTestRunner(blockingOpenCodeRun)
	launcher := nativeOpenCodePromptServerLauncher{
		runner: runner,
		health: func(context.Context, string) error { return nil },
		wait:   wait,
	}
	_, stop, err := launcher.Start(context.Background(), "opencode", "/tmp/project")
	if err != nil {
		t.Fatalf("default reserve: %v", err)
	}
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodePromptServerStopClassifiesRunnerCleanup(t *testing.T) {
	tests := []struct {
		name   string
		result command.Result
		err    error
		want   bool
	}{
		{"canceled", command.Result{Canceled: true}, &command.RunError{Kind: command.ErrorCanceled, Cause: context.Canceled}, false},
		{"cleanup incomplete", command.Result{Canceled: true, CleanupIncomplete: true}, &command.RunError{Kind: command.ErrorCleanup}, true},
		{"unexpected exit", command.Result{ExitCode: 1}, errors.New("exit"), true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := runnerFunc(func(ctx context.Context, _ string, _ []string) (command.Result, error) {
				<-ctx.Done()
				return test.result, test.err
			})
			runner, wait := ownedOpenCodeTestRunner(run)
			launcher := nativeOpenCodePromptServerLauncher{
				runner:  runner,
				reserve: func() (int, error) { return 49152, nil },
				health:  func(context.Context, string) error { return nil },
				wait:    wait,
			}
			_, stop, err := launcher.Start(context.Background(), "opencode", "/tmp/project")
			if err != nil {
				t.Fatal(err)
			}
			stopErr := stop(context.Background())
			if (stopErr != nil) != test.want {
				t.Fatalf("stop error=%v want=%t", stopErr, test.want)
			}
		})
	}
}

func TestOpenCodePromptServerDefaultsHealthWaitAndProbeTimeout(t *testing.T) {
	t.Run("health", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`{"healthy":true,"version":"1.18.30"}`))
		}))
		defer server.Close()
		port := server.Listener.Addr().(*net.TCPAddr).Port
		runner, wait := ownedOpenCodeTestRunner(blockingOpenCodeRun)
		launcher := nativeOpenCodePromptServerLauncher{
			runner:  runner,
			reserve: func() (int, error) { return port, nil },
			wait:    wait,
		}
		_, stop, err := launcher.Start(context.Background(), "opencode", "/tmp/project")
		if err != nil {
			t.Fatal(err)
		}
		if err := stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		launcher := nativeOpenCodePromptServerLauncher{
			runner:  openCodePromptSpecRunner{run: blockingOpenCodeRun},
			reserve: func() (int, error) { return 49152, nil },
			health:  func(context.Context, string) error { return nil },
		}
		if _, _, err := launcher.Start(ctx, "opencode", "/tmp/project"); err == nil {
			t.Fatal("canceled readiness accepted")
		}
	})
	for _, probe := range []time.Duration{0, -time.Second} {
		t.Run(fmt.Sprintf("probe-%s", probe), func(t *testing.T) {
			runner, wait := ownedOpenCodeTestRunner(blockingOpenCodeRun)
			launcher := nativeOpenCodePromptServerLauncher{
				runner:  runner,
				reserve: func() (int, error) { return 49152, nil },
				health: func(ctx context.Context, _ string) error {
					deadline, ok := ctx.Deadline()
					remaining := time.Until(deadline)
					if !ok || remaining < 500*time.Millisecond || remaining > 1100*time.Millisecond {
						return fmt.Errorf("probe deadline=%v remaining=%v", ok, remaining)
					}
					return nil
				},
				wait:  wait,
				probe: probe,
			}
			_, stop, err := launcher.Start(context.Background(), "opencode", "/tmp/project")
			if err != nil {
				t.Fatal(err)
			}
			if err := stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenCodePromptServerRetainsFirstHealthFailure(t *testing.T) {
	first, second, final := errors.New("first"), errors.New("second"), errors.New("final")
	healthCalls := 0
	observed := make(chan struct{})
	launcher := nativeOpenCodePromptServerLauncher{
		runner:  openCodePromptSpecRunner{run: blockingOpenCodeRun, stdoutForSpec: ownedOpenCodeLine, observed: observed},
		reserve: func() (int, error) { return 49152, nil },
		health: func(context.Context, string) error {
			healthCalls++
			if healthCalls == 1 {
				return first
			}
			return second
		},
		wait: func(context.Context, time.Duration) error {
			if healthCalls == 0 {
				<-observed
				return nil
			}
			if healthCalls == 1 {
				return nil
			}
			return final
		},
	}
	_, _, err := launcher.Start(context.Background(), "opencode", "/tmp/project")
	if !errors.Is(err, first) || !errors.Is(err, final) || errors.Is(err, second) || healthCalls != 2 {
		t.Fatalf("health=%d error=%v", healthCalls, err)
	}
}

func TestReserveOpenCodePromptPortReportsListenFailure(t *testing.T) {
	want := errors.New("listen failed")
	port, err := reserveOpenCodePromptPortWith(func(string, string) (net.Listener, error) { return nil, want })
	if port != 0 || !errors.Is(err, want) {
		t.Fatalf("port=%d error=%v", port, err)
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
		wait: func(context.Context, time.Duration) error {
			if calls == 0 {
				<-started
				return nil
			}
			if calls == 1 {
				return nil
			}
			return errors.New("unexpected wait after successful health")
		},
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
		{"status first success", `{"healthy":true,"version":"1.18.30"}`, 200, true},
		{"status last success", `{"healthy":true,"version":"1.18.30"}`, 299, true},
		{"status upper boundary", `{"healthy":true,"version":"1.18.30"}`, 300, false},
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
