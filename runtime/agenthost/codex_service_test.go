package agenthost

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

type codexServiceCall struct {
	executable string
	args       []string
}

type codexServiceRunner struct {
	mu    sync.Mutex
	calls []codexServiceCall
	run   func(context.Context, string, []string) (command.Result, error)
}

func (r *codexServiceRunner) Run(ctx context.Context, executable string, args []string) (command.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, codexServiceCall{executable: executable, args: append([]string(nil), args...)})
	r.mu.Unlock()
	if r.run == nil {
		return command.Result{}, errors.New("unexpected command")
	}
	return r.run(ctx, executable, args)
}

func (r *codexServiceRunner) observed() []codexServiceCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]codexServiceCall(nil), r.calls...)
}

type codexServiceFileInfo struct {
	mode fs.FileMode
	uid  uint32
	dir  bool
}

func (i codexServiceFileInfo) Name() string       { return "fixture" }
func (i codexServiceFileInfo) Size() int64        { return 0 }
func (i codexServiceFileInfo) Mode() fs.FileMode  { return i.mode }
func (i codexServiceFileInfo) ModTime() time.Time { return time.Time{} }
func (i codexServiceFileInfo) IsDir() bool        { return i.dir }
func (i codexServiceFileInfo) Sys() any           { return &syscall.Stat_t{Uid: i.uid} }

type codexServiceFixture struct {
	paths      codexServicePaths
	descriptor codexServiceDescriptor
	service    *CodexService
	runner     *codexServiceRunner
}

func newCodexServiceFixture(t *testing.T) codexServiceFixture {
	t.Helper()
	root := t.TempDir()
	paths := codexServicePaths{
		ConfigHome:     filepath.Join(root, "config"),
		KitConfigDir:   filepath.Join(root, "config", "herdr-plugin-kit"),
		UnitDirectory:  filepath.Join(root, "config", "systemd", "user"),
		DescriptorPath: filepath.Join(root, "config", "herdr-plugin-kit", codexServiceDescriptorName),
		UnitPath:       filepath.Join(root, "config", "systemd", "user", codexServiceUnitName),
		RuntimeDir:     filepath.Join(root, "runtime", "herdr-plugin-kit"),
		SocketPath:     filepath.Join(root, "runtime", "herdr-plugin-kit", "codex.sock"),
	}
	home := filepath.Join(root, "home")
	executable := filepath.Join(root, "release", "codex")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	descriptor := codexServiceDescriptor{
		SchemaVersion: codexServiceDescriptorVersion, UnitName: paths.unitName(), UnitPath: paths.UnitPath,
		Executable: executable, CodexHome: home, SocketPath: paths.SocketPath,
		RuntimeDirectory: paths.runtimeDirectoryName(), Version: SupportedCodexVersion,
	}
	writeOwnedCodexServiceFixture(t, paths, descriptor)
	runner := &codexServiceRunner{}
	service := NewCodexService(runner)
	service.resolveHome = func() (string, error) { return home, nil }
	service.resolvePaths = func() (codexServicePaths, error) { return paths, nil }
	service.nativeDaemonActive = func(context.Context, string) (bool, error) { return false, nil }
	service.euid = func() int { return 1000 }
	service.lstat = func(path string) (fs.FileInfo, error) {
		switch path {
		case paths.RuntimeDir:
			return codexServiceFileInfo{mode: os.ModeDir | 0o700, uid: 1000, dir: true}, nil
		case paths.SocketPath:
			return codexServiceFileInfo{mode: os.ModeSocket | 0o600, uid: 1000}, nil
		default:
			return nil, os.ErrNotExist
		}
	}
	service.readProcess = func(pid int) (codexServiceProcess, error) {
		return codexServiceProcess{
			Executable:  executable,
			Argv:        []string{executable, "app-server", "--listen", "unix://" + paths.SocketPath},
			Environment: map[string]string{"CODEX_HOME": home},
			Cgroup:      "/user.slice/user-1000.slice/user@1000.service/app.slice/" + codexServiceUnitName,
		}, nil
	}
	service.handshake = func(context.Context, string) (codexServiceHandshake, error) {
		return codexServiceHandshake{UserAgent: "codex-cli/" + SupportedCodexVersion, CodexHome: home, PlatformFamily: "unix", PlatformOS: "linux"}, nil
	}
	service.pollInterval = time.Millisecond
	return codexServiceFixture{paths: paths, descriptor: descriptor, service: service, runner: runner}
}

func writeOwnedCodexServiceFixture(t *testing.T, paths codexServicePaths, descriptor codexServiceDescriptor) {
	t.Helper()
	if err := ensureCodexServiceDirectories(paths); err != nil {
		t.Fatal(err)
	}
	unit, err := renderCodexServiceUnit(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := marshalCodexServiceDescriptor(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.UnitPath, unit, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.DescriptorPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func itoa(value int) string { return fmt.Sprintf("%d", value) }

func TestCodexServiceInspectValidatesStableServiceIdentity(t *testing.T) {
	fixture := newCodexServiceFixture(t)
	fixture.runner.run = func(_ context.Context, executable string, args []string) (command.Result, error) {
		if executable != "systemctl" || !reflect.DeepEqual(args, codexSystemdShowArgs(fixture.descriptor.UnitName)) {
			return command.Result{}, errors.New("unexpected command")
		}
		return serviceShowForDescriptor(fixture.paths, fixture.descriptor, "active", "running", 1462121, "32f79e6311fb4b3595300a5c76c8a0aa"), nil
	}
	snapshot, err := fixture.service.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := CodexServiceSnapshot{State: CodexServiceReady, CodexHome: fixture.descriptor.CodexHome, SocketPath: fixture.descriptor.SocketPath, CLIVersion: SupportedCodexVersion, AppServerVersion: SupportedCodexVersion, MainPID: 1462121, InvocationID: "32f79e6311fb4b3595300a5c76c8a0aa"}
	if snapshot != want {
		t.Fatalf("snapshot=%+v want=%+v", snapshot, want)
	}
	if calls := fixture.runner.observed(); len(calls) != 1 {
		t.Fatalf("calls=%+v", calls)
	}
}

func TestCodexServiceInspectIsReadOnlyWhenStopped(t *testing.T) {
	fixture := newCodexServiceFixture(t)
	fixture.runner.run = func(_ context.Context, executable string, args []string) (command.Result, error) {
		if executable != "systemctl" || !reflect.DeepEqual(args, codexSystemdShowArgs(fixture.descriptor.UnitName)) {
			return command.Result{}, errors.New("unexpected command")
		}
		return serviceShowForDescriptor(fixture.paths, fixture.descriptor, "inactive", "dead", 0, ""), nil
	}
	snapshot, err := fixture.service.Inspect(context.Background())
	if snapshot.State != CodexServiceDegraded || !errors.Is(err, ErrCodexServiceUnavailable) {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	if calls := fixture.runner.observed(); len(calls) != 1 || calls[0].args[1] != "show" {
		t.Fatalf("Inspect mutated service: %+v", calls)
	}
}

func TestCodexServiceEnsureStartsOnlyStoppedOwnedUnit(t *testing.T) {
	fixture := newCodexServiceFixture(t)
	var shows int
	fixture.runner.run = func(_ context.Context, executable string, args []string) (command.Result, error) {
		switch {
		case executable == "systemctl" && reflect.DeepEqual(args, codexSystemdShowArgs(fixture.descriptor.UnitName)):
			shows++
			if shows == 1 {
				return serviceShowForDescriptor(fixture.paths, fixture.descriptor, "inactive", "dead", 0, ""), nil
			}
			return serviceShowForDescriptor(fixture.paths, fixture.descriptor, "active", "running", 42, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), nil
		case executable == fixture.descriptor.Executable && reflect.DeepEqual(args, []string{"--version"}):
			return command.Result{Stdout: []byte("codex-cli " + SupportedCodexVersion)}, nil
		case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "start", codexServiceUnitName}):
			return command.Result{}, nil
		default:
			return command.Result{}, errors.New("unexpected command")
		}
	}
	snapshot, err := fixture.service.Ensure(context.Background())
	if err != nil || snapshot.State != CodexServiceReady || snapshot.MainPID != 42 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	calls := fixture.runner.observed()
	if len(calls) != 4 || !reflect.DeepEqual(calls[2].args, []string{"--user", "start", codexServiceUnitName}) {
		t.Fatalf("calls=%+v", calls)
	}
}

func TestCodexServiceEnsureWaitsForSocketAfterOwnedProcessStarts(t *testing.T) {
	fixture := newCodexServiceFixture(t)
	var shows, socketChecks int
	fixture.service.lstat = func(path string) (fs.FileInfo, error) {
		switch path {
		case fixture.paths.RuntimeDir:
			return codexServiceFileInfo{mode: os.ModeDir | 0o700, uid: 1000, dir: true}, nil
		case fixture.paths.SocketPath:
			socketChecks++
			if socketChecks == 1 {
				return nil, os.ErrNotExist
			}
			return codexServiceFileInfo{mode: os.ModeSocket | 0o600, uid: 1000}, nil
		default:
			return nil, os.ErrNotExist
		}
	}
	fixture.runner.run = func(_ context.Context, executable string, args []string) (command.Result, error) {
		switch {
		case executable == "systemctl" && reflect.DeepEqual(args, codexSystemdShowArgs(fixture.descriptor.UnitName)):
			shows++
			if shows == 1 {
				return serviceShowForDescriptor(fixture.paths, fixture.descriptor, "inactive", "dead", 0, ""), nil
			}
			return serviceShowForDescriptor(fixture.paths, fixture.descriptor, "active", "running", 42, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), nil
		case executable == fixture.descriptor.Executable && reflect.DeepEqual(args, []string{"--version"}):
			return command.Result{Stdout: []byte("codex-cli " + SupportedCodexVersion)}, nil
		case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "start", codexServiceUnitName}):
			return command.Result{}, nil
		default:
			return command.Result{}, errors.New("unexpected command")
		}
	}
	snapshot, err := fixture.service.Ensure(context.Background())
	if err != nil || snapshot.State != CodexServiceReady || socketChecks < 2 {
		t.Fatalf("snapshot=%+v socket checks=%d err=%v", snapshot, socketChecks, err)
	}
}

func TestCodexServiceEnsureReportsBoundedSocketReadinessTimeout(t *testing.T) {
	fixture := newCodexServiceFixture(t)
	fixture.service.timeout = 20 * time.Millisecond
	fixture.service.pollInterval = time.Millisecond
	fixture.service.lstat = func(path string) (fs.FileInfo, error) {
		if path == fixture.paths.RuntimeDir {
			return codexServiceFileInfo{mode: os.ModeDir | 0o700, uid: 1000, dir: true}, nil
		}
		return nil, os.ErrNotExist
	}
	var shows int
	fixture.runner.run = func(_ context.Context, executable string, args []string) (command.Result, error) {
		switch {
		case executable == "systemctl" && reflect.DeepEqual(args, codexSystemdShowArgs(fixture.descriptor.UnitName)):
			shows++
			if shows == 1 {
				return serviceShowForDescriptor(fixture.paths, fixture.descriptor, "inactive", "dead", 0, ""), nil
			}
			return serviceShowForDescriptor(fixture.paths, fixture.descriptor, "active", "running", 42, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), nil
		case executable == fixture.descriptor.Executable && reflect.DeepEqual(args, []string{"--version"}):
			return command.Result{Stdout: []byte("codex-cli " + SupportedCodexVersion)}, nil
		case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "start", codexServiceUnitName}):
			return command.Result{}, nil
		default:
			return command.Result{}, errors.New("unexpected command")
		}
	}
	if _, err := fixture.service.Ensure(context.Background()); !errors.Is(err, ErrCodexServiceUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
}

func TestCodexServiceRejectsDifferentConsumerHomeAndNativeDaemon(t *testing.T) {
	fixture := newCodexServiceFixture(t)
	fixture.service.resolveHome = func() (string, error) { return filepath.Join(t.TempDir(), "other"), nil }
	if snapshot, err := fixture.service.Ensure(context.Background()); snapshot.State != CodexServiceDegraded || !errors.Is(err, ErrCodexServiceDrift) {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	if calls := fixture.runner.observed(); len(calls) != 0 {
		t.Fatalf("home mismatch ran command: %+v", calls)
	}

	fixture = newCodexServiceFixture(t)
	fixture.service.nativeDaemonActive = func(context.Context, string) (bool, error) { return true, nil }
	if snapshot, err := fixture.service.Inspect(context.Background()); snapshot.State != CodexServiceDegraded || !errors.Is(err, ErrCodexServiceMigrationRequired) {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	if calls := fixture.runner.observed(); len(calls) != 0 {
		t.Fatalf("native coexistence ran service command: %+v", calls)
	}
}

func TestCodexServiceRejectsProcessAndInitializeDrift(t *testing.T) {
	for name, mutate := range map[string]func(*codexServiceFixture){
		"process argv": func(f *codexServiceFixture) {
			f.service.readProcess = func(int) (codexServiceProcess, error) {
				return codexServiceProcess{Executable: f.descriptor.Executable, Argv: []string{"wrong"}, Environment: map[string]string{"CODEX_HOME": f.descriptor.CodexHome}, Cgroup: "/" + codexServiceUnitName}, nil
			}
		},
		"process cgroup suffix": func(f *codexServiceFixture) {
			f.service.readProcess = func(int) (codexServiceProcess, error) {
				return codexServiceProcess{Executable: f.descriptor.Executable, Argv: []string{f.descriptor.Executable, "app-server", "--listen", "unix://" + f.descriptor.SocketPath}, Environment: map[string]string{"CODEX_HOME": f.descriptor.CodexHome}, Cgroup: "/user.slice/" + codexServiceUnitName + "-foreign"}, nil
			}
		},
		"initialize metadata": func(f *codexServiceFixture) {
			f.service.handshake = func(context.Context, string) (codexServiceHandshake, error) {
				return codexServiceHandshake{UserAgent: "codex-cli/0.153.0", CodexHome: f.descriptor.CodexHome, PlatformFamily: "unix", PlatformOS: "linux"}, nil
			}
		},
		"initialize version prefix": func(f *codexServiceFixture) {
			f.service.handshake = func(context.Context, string) (codexServiceHandshake, error) {
				return codexServiceHandshake{UserAgent: "codex-cli/0.154.01", CodexHome: f.descriptor.CodexHome, PlatformFamily: "unix", PlatformOS: "linux"}, nil
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newCodexServiceFixture(t)
			fixture.runner.run = func(_ context.Context, _ string, _ []string) (command.Result, error) {
				return serviceShowForDescriptor(fixture.paths, fixture.descriptor, "active", "running", 42, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), nil
			}
			mutate(&fixture)
			_, err := fixture.service.Inspect(context.Background())
			if (name == "process argv" || name == "process cgroup suffix") && !errors.Is(err, ErrCodexServiceDrift) {
				t.Fatalf("err=%v", err)
			}
			if name != "process argv" && name != "process cgroup suffix" && !errors.Is(err, ErrCodexServiceIncompatible) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestParseCodexSystemdShowUsesStableFields(t *testing.T) {
	raw := []byte("MainPID=1462121\nExecStart={ path=/home/raul/.codex/packages/standalone/releases/0.154.0-x86_64-unknown-linux-musl/bin/codex ; argv[]=/home/raul/.codex/packages/standalone/releases/0.154.0-x86_64-unknown-linux-musl/bin/codex app-server --listen unix:///tmp/example.sock ; ignore_errors=no ; start_time=[Mon 2026-09-16] ; stop_time=[n/a] ; pid=1462121 ; code=(null) ; status=0/0 }\nEnvironment=CODEX_HOME=/tmp/example\nActiveState=active\nSubState=running\nInvocationID=32f79e6311fb4b3595300a5c76c8a0aa\nLoadState=loaded\nUnitFileState=enabled\nFragmentPath=/tmp/config/systemd/user/herdr-plugin-kit-codex.service\n")
	show, err := parseCodexSystemdShow(raw, false)
	if err != nil || show.MainPID != 1462121 || show.InvocationID != "32f79e6311fb4b3595300a5c76c8a0aa" || show.ActiveState != "active" {
		t.Fatalf("show=%+v err=%v", show, err)
	}
	if _, err := parseCodexSystemdShow([]byte("MainPID=1\n"), false); !errors.Is(err, ErrCodexServiceReceipt) {
		t.Fatalf("missing properties error=%v", err)
	}
	absent := []byte("MainPID=0\nEnvironment=\nLoadState=not-found\nActiveState=inactive\nSubState=dead\nFragmentPath=\nUnitFileState=\nInvocationID=\n")
	show, err = parseCodexSystemdShow(absent, false)
	if err != nil || show.LoadState != "not-found" || show.ExecStart != "" {
		t.Fatalf("not-found show=%+v err=%v", show, err)
	}
}

func TestSystemdEnvironmentContainsOnlyOneExactAssignment(t *testing.T) {
	for _, test := range []struct {
		name, raw, want string
		match           bool
	}{
		{name: "unquoted exact", raw: "CODEX_HOME=/tmp/home", want: "/tmp/home", match: true},
		{name: "prefix is not exact", raw: "CODEX_HOME=/tmp/home-other", want: "/tmp/home", match: false},
		// This is the form observed from systemctl show for a value with a space.
		{name: "quoted space", raw: `FOO=bar "CODEX_HOME=/tmp/hpk env space"`, want: "/tmp/hpk env space", match: true},
		{name: "escaped space", raw: `CODEX_HOME=/tmp/hpk\x20env\x20space`, want: "/tmp/hpk env space", match: true},
		{name: "duplicate assignment", raw: "CODEX_HOME=/tmp/home CODEX_HOME=/tmp/home", want: "/tmp/home", match: false},
		{name: "malformed quote", raw: `"CODEX_HOME=/tmp/home`, want: "/tmp/home", match: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := systemdEnvironmentContains(test.raw, "CODEX_HOME", test.want); got != test.match {
				t.Fatalf("match=%t, want %t for %q", got, test.match, test.raw)
			}
		})
	}
}

func TestCodexUserAgentRequiresExactPinnedVersionToken(t *testing.T) {
	for _, test := range []struct {
		userAgent string
		match     bool
	}{
		{userAgent: "codex-cli/0.154.0", match: true},
		{userAgent: "codex-cli/0.154.0 (linux)", match: true},
		{userAgent: "codex-cli/0.154.01", match: false},
		{userAgent: "codex-cli/0.154.0+build", match: false},
		{userAgent: "codex-cli/0.153.0", match: false},
	} {
		if got := codexUserAgentHasExactVersion(test.userAgent, SupportedCodexVersion); got != test.match {
			t.Fatalf("userAgent=%q match=%t, want %t", test.userAgent, got, test.match)
		}
	}
}

func TestCodexServiceHandshakeRequiresPinnedMetadata(t *testing.T) {
	const request = `{"id":1,"method":"initialize","params":{"clientInfo":{"name":"herdr-plugin-kit","version":"0.3.0"},"capabilities":{"experimentalApi":true}}}`
	t.Run("valid", func(t *testing.T) {
		initialized := make(chan struct{})
		socket := unixServer(t, func(ctx context.Context, connection *websocket.Conn) {
			defer close(initialized)
			receive(t, ctx, connection, request)
			send(t, ctx, connection, `{"id":1,"result":{"userAgent":"codex-cli/0.154.0","codexHome":"/private/home","platformFamily":"unix","platformOs":"linux"}}`)
			receive(t, ctx, connection, `{"method":"initialized"}`)
		})
		got, err := readCodexServiceHandshake(context.Background(), socket)
		if err != nil || got.CodexHome != "/private/home" || got.PlatformOS != "linux" {
			t.Fatalf("got=%+v err=%v", got, err)
		}
		select {
		case <-initialized:
		case <-time.After(time.Second):
			t.Fatal("initialized notification was not received")
		}
	})
	t.Run("missing metadata", func(t *testing.T) {
		socket := unixServer(t, func(ctx context.Context, connection *websocket.Conn) {
			receive(t, ctx, connection, request)
			send(t, ctx, connection, `{"id":1,"result":{"userAgent":"codex-cli/0.154.0"}}`)
		})
		if _, err := readCodexServiceHandshake(context.Background(), socket); !errors.Is(err, errCodexProtocol) {
			t.Fatalf("err=%v", err)
		}
	})
}

func codexSystemdShowArgs(unit string) []string {
	return []string{"--user", "show", unit, "--property=LoadState,UnitFileState,ActiveState,SubState,MainPID,InvocationID,FragmentPath,ExecStart,Environment", "--no-page"}
}

func serviceShowForDescriptor(paths codexServicePaths, descriptor codexServiceDescriptor, state, substate string, pid int, invocation string) command.Result {
	return command.Result{Stdout: []byte("LoadState=loaded\n" +
		"UnitFileState=enabled\n" +
		"ActiveState=" + state + "\n" +
		"SubState=" + substate + "\n" +
		"MainPID=" + itoa(pid) + "\n" +
		"InvocationID=" + invocation + "\n" +
		"FragmentPath=" + paths.UnitPath + "\n" +
		"ExecStart={ path=" + descriptor.Executable + " ; argv[]=" + descriptor.Executable + " app-server --listen unix://" + descriptor.SocketPath + " ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=" + itoa(pid) + " ; code=(null) ; status=0/0 }\n" +
		"Environment=CODEX_HOME=" + descriptor.CodexHome + "\n")}
}
