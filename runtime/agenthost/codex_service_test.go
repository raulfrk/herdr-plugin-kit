package agenthost

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"pgregory.net/rapid"
)

type codexServiceRunner struct {
	mu      sync.Mutex
	calls   [][]string
	results []command.Result
	errors  []error
}

func (r *codexServiceRunner) Run(_ context.Context, executable string, args []string) (command.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{executable}, args...))
	if len(r.results) == 0 {
		return command.Result{}, errors.New("unexpected service command")
	}
	result := r.results[0]
	r.results = r.results[1:]
	var err error
	if len(r.errors) != 0 {
		err = r.errors[0]
		r.errors = r.errors[1:]
	}
	return result, err
}

type codexServiceFileInfo struct {
	mode fs.FileMode
	uid  uint32
}

func (i codexServiceFileInfo) Name() string       { return "app-server-control.sock" }
func (i codexServiceFileInfo) Size() int64        { return 0 }
func (i codexServiceFileInfo) Mode() fs.FileMode  { return i.mode }
func (i codexServiceFileInfo) ModTime() time.Time { return time.Time{} }
func (i codexServiceFileInfo) IsDir() bool        { return false }
func (i codexServiceFileInfo) Sys() any           { return &syscall.Stat_t{Uid: i.uid} }

func serviceReceipt(home, status string) []byte {
	pid := ""
	if status == "started" {
		pid = `,"pid":1234`
	}
	return []byte(`{"status":"` + status + `","backend":"pid"` + pid + `,"managedCodexPath":"` + filepath.Join(home, "packages", "standalone", "current", "bin", "codex") + `","managedCodexVersion":"0.154.0","socketPath":"` + filepath.Join(home, "app-server-control", "app-server-control.sock") + `","cliVersion":"0.154.0","appServerVersion":"0.154.0"}`)
}

func testCodexService(home string, runner command.Runner) *CodexService {
	service := NewCodexService(runner)
	service.resolveHome = func() (string, error) { return home, nil }
	service.evalPath = func(path string) (string, error) { return path, nil }
	service.lstat = func(string) (fs.FileInfo, error) {
		return codexServiceFileInfo{mode: os.ModeSocket | 0o600, uid: 1000}, nil
	}
	service.euid = func() int { return 1000 }
	service.handshake = func(context.Context, string) error { return nil }
	return service
}

func TestCodexServiceEnsureAndInspectUseExactNativeCommands(t *testing.T) {
	home := "/var/lib/test-codex"
	runner := &codexServiceRunner{results: []command.Result{
		{Stdout: serviceReceipt(home, "started")},
		{Stdout: serviceReceipt(home, "alreadyRunning")},
		{Stdout: serviceReceipt(home, "running")},
	}}
	service := testCodexService(home, runner)
	want := CodexServiceSnapshot{State: CodexServiceReady, CodexHome: home, SocketPath: filepath.Join(home, "app-server-control", "app-server-control.sock"), CLIVersion: SupportedCodexVersion, AppServerVersion: SupportedCodexVersion}
	for _, operation := range []func(context.Context) (CodexServiceSnapshot, error){service.Ensure, service.Ensure, service.Inspect} {
		got, err := operation(context.Background())
		if err != nil || got != want {
			t.Fatalf("snapshot=%+v err=%v", got, err)
		}
	}
	wantCalls := [][]string{
		{"codex", "app-server", "daemon", "start"},
		{"codex", "app-server", "daemon", "start"},
		{"codex", "app-server", "daemon", "version"},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("calls=%q", runner.calls)
	}
}

func TestCodexServiceDefaultAndAttentionShapedConsumersConvergeOnOneIdentity(t *testing.T) {
	home := "/var/lib/test-codex"
	const consumers = 12
	results := make([]command.Result, consumers)
	for i := range results {
		results[i].Stdout = serviceReceipt(home, "alreadyRunning")
	}
	runner := &codexServiceRunner{results: results}
	// The first two instances model the default generated consumer and the
	// Attention-shaped generated consumer; the rest exercise concurrent peers.
	services := make([]*CodexService, consumers)
	for i := range services {
		services[i] = testCodexService(home, runner)
	}
	snapshots := make(chan CodexServiceSnapshot, consumers)
	errorsSeen := make(chan error, consumers)
	var group sync.WaitGroup
	for _, service := range services {
		group.Add(1)
		go func() {
			defer group.Done()
			snapshot, err := service.Ensure(context.Background())
			snapshots <- snapshot
			errorsSeen <- err
		}()
	}
	group.Wait()
	close(snapshots)
	close(errorsSeen)
	var first CodexServiceSnapshot
	for snapshot := range snapshots {
		if first == (CodexServiceSnapshot{}) {
			first = snapshot
		}
		if snapshot != first || snapshot.State != CodexServiceReady {
			t.Fatalf("snapshots diverged: first=%+v got=%+v", first, snapshot)
		}
	}
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(runner.calls) != consumers {
		t.Fatalf("calls=%d", len(runner.calls))
	}
}

func TestCodexServiceInspectDoesNotRepairAndNextEnsureCanRepair(t *testing.T) {
	home := "/var/lib/test-codex"
	runner := &codexServiceRunner{
		results: []command.Result{{}, {Stdout: serviceReceipt(home, "started")}},
		errors:  []error{errors.New("daemon is not running"), nil},
	}
	service := testCodexService(home, runner)
	degraded, err := service.Inspect(context.Background())
	if degraded != (CodexServiceSnapshot{State: CodexServiceDegraded, CodexHome: home}) || !errors.Is(err, ErrCodexServiceUnavailable) {
		t.Fatalf("degraded=%+v err=%v", degraded, err)
	}
	ready, err := service.Ensure(context.Background())
	if err != nil || ready.State != CodexServiceReady {
		t.Fatalf("ready=%+v err=%v", ready, err)
	}
	if got := runner.calls; len(got) != 2 || got[0][3] != "version" || got[1][3] != "start" {
		t.Fatalf("calls=%q", got)
	}
}

func TestCodexServiceReceiptIsStrictAndBounded(t *testing.T) {
	home := "/var/lib/test-codex"
	valid := serviceReceipt(home, "alreadyRunning")
	cases := map[string]command.Result{
		"empty":                         {},
		"malformed":                     {Stdout: []byte(`{"status":`)},
		"unknown field":                 {Stdout: []byte(strings.Replace(string(valid), `"backend"`, `"extra":true,"backend"`, 1))},
		"case-variant status":           {Stdout: []byte(strings.Replace(string(valid), `"status"`, `"Status"`, 1))},
		"duplicate status":              {Stdout: []byte(strings.Replace(string(valid), `"status":"alreadyRunning"`, `"status":"started","status":"alreadyRunning"`, 1))},
		"conflicting duplicate version": {Stdout: []byte(strings.Replace(string(valid), `"appServerVersion":"0.154.0"`, `"appServerVersion":"0.153.0","appServerVersion":"0.154.0"`, 1))},
		"trailing value":                {Stdout: append(append([]byte(nil), valid...), []byte(` {}`)...)},
		"stdout truncated":              {Stdout: valid, StdoutTruncated: true},
		"stderr truncated":              {Stdout: valid, StderrTruncated: true},
		"oversized":                     {Stdout: []byte(strings.Repeat("x", maxCodexServiceReceiptBytes+1))},
		"unsupported status":            {Stdout: serviceReceipt(home, "running")},
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &codexServiceRunner{results: []command.Result{result}}
			_, err := testCodexService(home, runner).Ensure(context.Background())
			if !errors.Is(err, ErrCodexServiceReceipt) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	rapid.Check(t, func(t *rapid.T) {
		suffix := rapid.StringMatching(`[A-Za-z0-9]{1,64}`).Draw(t, "suffix")
		result := command.Result{Stdout: append(append([]byte(nil), valid...), []byte(suffix)...)}
		if _, err := decodeCodexServiceReceipt(result); !errors.Is(err, ErrCodexServiceReceipt) {
			t.Fatalf("suffix=%q err=%v", suffix, err)
		}
	})
}

func TestCodexServiceRejectsVersionBackendPIDAndIdentityDrift(t *testing.T) {
	home := "/var/lib/test-codex"
	tests := map[string]struct {
		receipt []byte
		mutate  func(*CodexService)
		want    error
	}{
		"CLI version":                 {receipt: []byte(strings.Replace(string(serviceReceipt(home, "alreadyRunning")), `"cliVersion":"0.154.0"`, `"cliVersion":"0.153.0"`, 1)), want: ErrCodexServiceIncompatible},
		"managed version":             {receipt: []byte(strings.Replace(string(serviceReceipt(home, "alreadyRunning")), `"managedCodexVersion":"0.154.0"`, `"managedCodexVersion":"0.153.0"`, 1)), want: ErrCodexServiceIncompatible},
		"server version":              {receipt: []byte(strings.Replace(string(serviceReceipt(home, "alreadyRunning")), `"appServerVersion":"0.154.0"`, `"appServerVersion":"0.153.0"`, 1)), want: ErrCodexServiceIncompatible},
		"backend":                     {receipt: []byte(strings.Replace(string(serviceReceipt(home, "alreadyRunning")), `"backend":"pid"`, `"backend":"systemd"`, 1)), want: ErrCodexServiceDrift},
		"PID on already running":      {receipt: []byte(strings.Replace(string(serviceReceipt(home, "alreadyRunning")), `"backend":"pid"`, `"backend":"pid","pid":4`, 1)), want: ErrCodexServiceReceipt},
		"null PID on already running": {receipt: []byte(strings.Replace(string(serviceReceipt(home, "alreadyRunning")), `"backend":"pid"`, `"backend":"pid","pid":null`, 1)), want: ErrCodexServiceReceipt},
		"missing started PID":         {receipt: []byte(strings.Replace(string(serviceReceipt(home, "started")), `,"pid":1234`, ``, 1)), want: ErrCodexServiceReceipt},
		"managed path":                {receipt: []byte(strings.Replace(string(serviceReceipt(home, "alreadyRunning")), filepath.Join(home, "packages"), "/wrong/packages", 1)), want: ErrCodexServiceDrift},
		"socket path":                 {receipt: []byte(strings.Replace(string(serviceReceipt(home, "alreadyRunning")), filepath.Join(home, "app-server-control"), "/wrong/control", 1)), want: ErrCodexServiceDrift},
		"socket mode": {receipt: serviceReceipt(home, "alreadyRunning"), mutate: func(s *CodexService) {
			s.lstat = func(string) (fs.FileInfo, error) {
				return codexServiceFileInfo{mode: os.ModeSocket | 0o660, uid: 1000}, nil
			}
		}, want: ErrCodexServiceDrift},
		"socket type": {receipt: serviceReceipt(home, "alreadyRunning"), mutate: func(s *CodexService) {
			s.lstat = func(string) (fs.FileInfo, error) { return codexServiceFileInfo{mode: 0o600, uid: 1000}, nil }
		}, want: ErrCodexServiceDrift},
		"socket owner": {receipt: serviceReceipt(home, "alreadyRunning"), mutate: func(s *CodexService) { s.euid = func() int { return 1001 } }, want: ErrCodexServiceDrift},
		"handshake": {receipt: serviceReceipt(home, "alreadyRunning"), mutate: func(s *CodexService) {
			s.handshake = func(context.Context, string) error { return errors.New("handshake failed") }
		}, want: ErrCodexServiceUnavailable},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			runner := &codexServiceRunner{results: []command.Result{{Stdout: test.receipt}}}
			service := testCodexService(home, runner)
			if test.mutate != nil {
				test.mutate(service)
			}
			snapshot, err := service.Ensure(context.Background())
			if snapshot.State != CodexServiceDegraded || !errors.Is(err, test.want) {
				t.Fatalf("snapshot=%+v err=%v want=%v", snapshot, err, test.want)
			}
		})
	}
}

func TestCodexServiceCancellationAndSurface(t *testing.T) {
	runner := &codexServiceRunner{}
	service := testCodexService("/var/lib/test-codex", runner)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snapshot, err := service.Ensure(ctx)
	if snapshot.State != CodexServiceDegraded || !errors.Is(err, context.Canceled) || !errors.Is(err, ErrCodexServiceUnavailable) || len(runner.calls) != 0 {
		t.Fatalf("snapshot=%+v err=%v calls=%q", snapshot, err, runner.calls)
	}
	typeOfService := reflect.TypeOf(service)
	for _, forbidden := range []string{"Stop", "Restart", "Bootstrap", "Update", "Watch"} {
		if _, exists := typeOfService.MethodByName(forbidden); exists {
			t.Fatalf("unexpected public operation %s", forbidden)
		}
	}
}

func TestCodexServiceAppliesInternalDeadline(t *testing.T) {
	if codexServiceTimeout != 10*time.Second {
		t.Fatalf("default timeout=%v", codexServiceTimeout)
	}
	runner := runnerFunc(func(ctx context.Context, _ string, _ []string) (command.Result, error) {
		<-ctx.Done()
		return command.Result{}, ctx.Err()
	})
	service := testCodexService("/var/lib/test-codex", runner)
	service.timeout = 5 * time.Millisecond
	started := time.Now()
	snapshot, err := service.Ensure(context.Background())
	if elapsed := time.Since(started); elapsed > time.Second || snapshot.State != CodexServiceDegraded || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrCodexServiceUnavailable) {
		t.Fatalf("elapsed=%v snapshot=%+v err=%v", elapsed, snapshot, err)
	}
}

func TestEffectiveCodexHomeCanonicalizesExistingDirectory(t *testing.T) {
	realHome := t.TempDir()
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "codex-home")
	if err := os.Symlink(realHome, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", link)
	got, err := effectiveCodexHome()
	if err != nil || got != realHome {
		t.Fatalf("home=%q err=%v", got, err)
	}
}

func TestEffectiveCodexHomeRejectsMissingUserHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", "")
	if _, err := effectiveCodexHome(); err == nil {
		t.Fatal("expected missing user home error")
	}
}

func TestCodexServiceCanonicalizesReceiptPaths(t *testing.T) {
	realHome := "/srv/codex"
	linkedHome := "/linked/codex"
	runner := &codexServiceRunner{results: []command.Result{{Stdout: serviceReceipt(linkedHome, "alreadyRunning")}}}
	service := testCodexService(realHome, runner)
	service.evalPath = func(path string) (string, error) {
		return strings.Replace(path, linkedHome, realHome, 1), nil
	}
	snapshot, err := service.Ensure(context.Background())
	if err != nil || snapshot.SocketPath != filepath.Join(realHome, "app-server-control", "app-server-control.sock") || snapshot.CodexHome != realHome {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
}

func TestInspectCodexServiceSocketProtocol(t *testing.T) {
	const initialize = `{"id":1,"method":"initialize","params":{"clientInfo":{"name":"herdr-plugin-kit","version":"0.3.0"},"capabilities":{"experimentalApi":true}}}`
	t.Run("valid", func(t *testing.T) {
		initialized := make(chan struct{})
		socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
			defer close(initialized)
			receive(t, ctx, conn, initialize)
			send(t, ctx, conn, `{"id":1,"result":{"userAgent":"codex-tui/0.154.0"}}`)
			receive(t, ctx, conn, `{"method":"initialized"}`)
		})
		if err := inspectCodexServiceSocket(context.Background(), socket); err != nil {
			t.Fatal(err)
		}
		select {
		case <-initialized:
		case <-time.After(time.Second):
			t.Fatal("initialized notification was not received")
		}
	})
	t.Run("empty user agent", func(t *testing.T) {
		socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
			receive(t, ctx, conn, initialize)
			send(t, ctx, conn, `{"id":1,"result":{}}`)
		})
		if err := inspectCodexServiceSocket(context.Background(), socket); !errors.Is(err, errCodexProtocol) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("response failure", func(t *testing.T) {
		socket := unixServer(t, func(ctx context.Context, conn *websocket.Conn) {
			receive(t, ctx, conn, initialize)
			conn.CloseNow()
		})
		if err := inspectCodexServiceSocket(context.Background(), socket); err == nil {
			t.Fatal("expected response failure")
		}
	})
	t.Run("dial failure", func(t *testing.T) {
		if err := inspectCodexServiceSocket(context.Background(), filepath.Join(t.TempDir(), "missing.sock")); err == nil {
			t.Fatal("expected dial failure")
		}
	})
}
