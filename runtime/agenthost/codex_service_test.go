package agenthost

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
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
		{userAgent: "codex-cli/0.154.01 other/0.154.0\tlinux", match: true},
	} {
		if got := codexUserAgentHasExactVersion(test.userAgent, SupportedCodexVersion); got != test.match {
			t.Fatalf("userAgent=%q match=%t, want %t", test.userAgent, got, test.match)
		}
	}
}

func TestNewCodexServiceUsesBoundedReadinessDefaults(t *testing.T) {
	service := NewCodexService(&codexServiceRunner{})
	if service.timeout != 10*time.Second || service.pollInterval != 50*time.Millisecond {
		t.Fatalf("timeout=%s pollInterval=%s", service.timeout, service.pollInterval)
	}
}

func TestCodexServiceShowUnitCombinesRunnerAndReceiptFailures(t *testing.T) {
	runFailure := errors.New("systemctl failed")
	runner := &codexServiceRunner{run: func(context.Context, string, []string) (command.Result, error) {
		return command.Result{Stdout: []byte("not a systemctl property list\n")}, runFailure
	}}
	_, err := NewCodexService(runner).showUnit(context.Background(), "fixture.service")
	if !errors.Is(err, ErrCodexServiceUnavailable) || !errors.Is(err, ErrCodexServiceReceipt) || !errors.Is(err, runFailure) {
		t.Fatalf("show error=%v", err)
	}
}

func TestCgroupContainsExactUnitComponent(t *testing.T) {
	const unit = "herdr-plugin-kit-codex.service"
	for _, test := range []struct {
		name, cgroup string
		contains     bool
	}{
		{name: "systemd hierarchy", cgroup: "0::/user.slice/user-1000.slice/user@1000.service/app.slice/" + unit, contains: true},
		{name: "legacy hierarchy", cgroup: "7:name=systemd:/user.slice/" + unit, contains: true},
		{name: "plain path", cgroup: "/user.slice/" + unit, contains: true},
		{name: "bare post-separator path", cgroup: "0::" + unit, contains: true},
		{name: "partial component", cgroup: "0::/user.slice/" + unit + ".d", contains: false},
		{name: "different component", cgroup: "0::/user.slice/other.service", contains: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := cgroupContainsUnit(test.cgroup, unit); got != test.contains {
				t.Fatalf("contains=%t, want %t for %q", got, test.contains, test.cgroup)
			}
		})
	}
}

func TestReadCodexServiceProcessAtParsesBoundedFixture(t *testing.T) {
	root := t.TempDir()
	executable := writeCodexServiceProcessFixture(t, root, 42)
	process, err := readCodexServiceProcessAt(root, 42)
	if err != nil {
		t.Fatal(err)
	}
	if process.Executable != executable {
		t.Fatalf("executable=%q, want %q", process.Executable, executable)
	}
	wantArgv := []string{executable, "app-server", "--listen", "unix:///tmp/fixture.sock"}
	if !reflect.DeepEqual(process.Argv, wantArgv) {
		t.Fatalf("argv=%q, want %q", process.Argv, wantArgv)
	}
	wantEnvironment := map[string]string{"CODEX_HOME": "/tmp/fixture-home", "FIXTURE": "yes"}
	if !reflect.DeepEqual(process.Environment, wantEnvironment) {
		t.Fatalf("environment did not contain expected fixture entries")
	}
	if process.Cgroup != "0::/user.slice/fixture.service\n" {
		t.Fatalf("cgroup=%q", process.Cgroup)
	}
}

func TestReadCodexServiceProcessAtRejectsInvalidObservations(t *testing.T) {
	if _, err := readCodexServiceProcessAt(t.TempDir(), 0); err == nil {
		t.Fatal("invalid PID unexpectedly succeeded")
	}
	for _, name := range []string{"exe", "cmdline", "environ", "cgroup"} {
		t.Run("missing "+name, func(t *testing.T) {
			root := t.TempDir()
			writeCodexServiceProcessFixture(t, root, 42)
			if err := os.Remove(filepath.Join(root, "42", name)); err != nil {
				t.Fatal(err)
			}
			if _, err := readCodexServiceProcessAt(root, 42); err == nil {
				t.Fatalf("missing %s unexpectedly succeeded", name)
			}
		})
	}
	for _, test := range []struct {
		name, file string
		data       []byte
	}{
		{name: "empty argv", file: "cmdline", data: []byte{0, 0}},
		{name: "duplicate environment", file: "environ", data: []byte("CODEX_HOME=/tmp/fixture-home\x00CODEX_HOME=/tmp/other\x00")},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeCodexServiceProcessFixture(t, root, 42)
			if err := os.WriteFile(filepath.Join(root, "42", test.file), test.data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readCodexServiceProcessAt(root, 42); err == nil {
				t.Fatal("invalid process observation unexpectedly succeeded")
			}
		})
	}
}

func writeCodexServiceProcessFixture(t *testing.T, root string, pid int) string {
	t.Helper()
	processDirectory := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(processDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "codex")
	if err := os.WriteFile(executable, []byte("fixture executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(processDirectory, "exe")); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"cmdline": []byte(executable + "\x00app-server\x00--listen\x00unix:///tmp/fixture.sock\x00"),
		"environ": []byte("CODEX_HOME=/tmp/fixture-home\x00FIXTURE=yes\x00ignored\x00=ignored\x00"),
		"cgroup":  []byte("0::/user.slice/fixture.service\n"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(processDirectory, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return executable
}

func TestReadSmallProcFileBoundsObservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observation")
	if _, err := readSmallProcFile(path); err == nil {
		t.Fatal("missing observation unexpectedly succeeded")
	}
	if err := os.WriteFile(path, make([]byte, maxCodexServiceReceiptBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := readSmallProcFile(path)
	if err != nil || len(data) != maxCodexServiceReceiptBytes {
		t.Fatalf("bounded observation length=%d err=%v", len(data), err)
	}
	if err := os.WriteFile(path, make([]byte, maxCodexServiceReceiptBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSmallProcFile(path); err == nil {
		t.Fatal("oversized observation unexpectedly succeeded")
	}
}

func TestSplitNULDropsEmptyEntriesAndTrailingTerminator(t *testing.T) {
	for _, test := range []struct {
		name string
		data []byte
		want []string
	}{
		{name: "empty", data: nil, want: []string{}},
		{name: "unterminated", data: []byte("one"), want: []string{"one"}},
		{name: "empty entries", data: []byte("\x00one\x00\x00two\x00"), want: []string{"one", "two"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := splitNUL(test.data); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("split=%q, want %q", got, test.want)
			}
		})
	}
}

func TestCanonicalExistingDirectory(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	file := filepath.Join(root, "file")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDirectory, link); err != nil {
		t.Fatal(err)
	}
	if got, err := canonicalExistingDirectory(link, true); err != nil || got != realDirectory {
		t.Fatalf("canonical symlink=%q err=%v", got, err)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(workingDirectory, realDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := canonicalExistingDirectory(relative, false); err != nil || got != realDirectory {
		t.Fatalf("canonical relative=%q err=%v", got, err)
	}
	for _, test := range []struct {
		name            string
		path            string
		requireAbsolute bool
	}{
		{name: "empty", path: "", requireAbsolute: true},
		{name: "NUL", path: string([]byte{'x', 0}), requireAbsolute: false},
		{name: "relative required", path: relative, requireAbsolute: true},
		{name: "file", path: file, requireAbsolute: true},
		{name: "missing", path: filepath.Join(root, "missing"), requireAbsolute: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := canonicalExistingDirectory(test.path, test.requireAbsolute); err == nil {
				t.Fatal("invalid directory unexpectedly succeeded")
			}
		})
	}
}

func TestCanonicalNativeDaemonActive(t *testing.T) {
	// Unix socket paths are limited to 108 bytes on Linux. This test needs a
	// real socket, including a retained stale inode, so keep its task-owned
	// root independent of a potentially long Go test temporary directory.
	home, err := os.MkdirTemp("/tmp", "hpk-codex-native-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(home); err != nil {
			t.Errorf("remove native daemon test root: %v", err)
		}
	})
	controlDirectory := filepath.Join(home, "app-server-control")
	if err := os.Mkdir(controlDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(controlDirectory, "app-server-control.sock")
	if active, err := canonicalNativeDaemonActive(context.Background(), home); err != nil || active {
		t.Fatalf("missing socket active=%t err=%v", active, err)
	}
	if err := os.WriteFile(socket, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if active, err := canonicalNativeDaemonActive(context.Background(), home); err != nil || active {
		t.Fatalf("regular file active=%t err=%v", active, err)
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "target")
	if err := os.WriteFile(target, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, socket); err != nil {
		t.Fatal(err)
	}
	if active, err := canonicalNativeDaemonActive(context.Background(), home); err != nil || active {
		t.Fatalf("symlink active=%t err=%v", active, err)
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socket)
	}()
	accepted := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			acceptErr = connection.Close()
		}
		accepted <- acceptErr
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if active, err := canonicalNativeDaemonActive(ctx, home); err != nil || !active {
		t.Fatalf("listening socket active=%t err=%v", active, err)
	}
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(socket)
	if err != nil {
		t.Fatalf("stale socket stat: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("stale socket mode=%#o", info.Mode())
	}
	connection, dialErr := (&net.Dialer{}).DialContext(context.Background(), "unix", socket)
	if connection != nil {
		_ = connection.Close()
		t.Fatal("stale socket unexpectedly accepted a connection")
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		t.Fatalf("stale socket dial error=%v, want ECONNREFUSED", dialErr)
	}
	if active, err := canonicalNativeDaemonActive(context.Background(), home); err != nil || active {
		t.Fatalf("refused socket active=%t err=%v", active, err)
	}
	if _, err := canonicalNativeDaemonActive(context.Background(), string([]byte{0})); err == nil {
		t.Fatal("invalid native home unexpectedly succeeded")
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

func TestCodexServiceUsableRequiresEveryDependency(t *testing.T) {
	var nilService *CodexService
	if nilService.usable() {
		t.Fatal("nil service is usable")
	}
	base := &CodexService{
		runner:       &codexServiceRunner{},
		resolveHome:  func() (string, error) { return "/fixture/home", nil },
		resolvePaths: func() (codexServicePaths, error) { return codexServicePaths{}, nil },
	}
	for _, test := range []struct {
		name   string
		mutate func(*CodexService)
		want   bool
	}{
		{name: "complete", want: true},
		{name: "runner", mutate: func(service *CodexService) { service.runner = nil }},
		{name: "home resolver", mutate: func(service *CodexService) { service.resolveHome = nil }},
		{name: "path resolver", mutate: func(service *CodexService) { service.resolvePaths = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := *base
			if test.mutate != nil {
				test.mutate(&service)
			}
			if got := service.usable(); got != test.want {
				t.Fatalf("usable=%t, want %t", got, test.want)
			}
		})
	}
}

func TestCodexServiceWithTimeoutUsesDefaultForNonpositiveValues(t *testing.T) {
	for _, test := range []struct {
		name, want string
		timeout    time.Duration
		expected   time.Duration
	}{
		{name: "zero", timeout: 0, expected: codexServiceTimeout},
		{name: "negative", timeout: -time.Second, expected: codexServiceTimeout},
		{name: "configured", timeout: 75 * time.Millisecond, expected: 75 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := NewCodexService(&codexServiceRunner{})
			service.timeout = test.timeout
			started := time.Now()
			ctx, cancel := service.withTimeout(context.Background())
			defer cancel()
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("bounded context has no deadline")
			}
			got := deadline.Sub(started)
			if got < test.expected-100*time.Millisecond || got > test.expected+100*time.Millisecond {
				t.Fatalf("deadline duration=%s, want approximately %s", got, test.expected)
			}
		})
	}
}

func fixtureCodexSystemdShow(t *testing.T, fixture codexServiceFixture, state, substate string, pid int, invocation string) codexSystemdShow {
	t.Helper()
	show, err := parseCodexSystemdShow(serviceShowForDescriptor(fixture.paths, fixture.descriptor, state, substate, pid, invocation).Stdout, false)
	if err != nil {
		t.Fatal(err)
	}
	return show
}

func TestCodexServiceIdentityChecksRejectOneMismatchedFieldAtATime(t *testing.T) {
	fixture := newCodexServiceFixture(t)
	loaded := fixtureCodexSystemdShow(t, fixture, "active", "running", 42, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if !loadedCodexServiceUnitMatches(fixture.paths, fixture.descriptor, loaded) {
		t.Fatal("valid loaded unit was rejected")
	}
	for _, test := range []struct {
		name   string
		mutate func(*codexSystemdShow)
	}{
		{name: "load state", mutate: func(show *codexSystemdShow) { show.LoadState = "not-found" }},
		{name: "fragment", mutate: func(show *codexSystemdShow) { show.FragmentPath = "/foreign.service" }},
		{name: "ExecStart", mutate: func(show *codexSystemdShow) { show.ExecStart = "{ path=/foreign ; argv[]=/foreign ; }" }},
		{name: "environment", mutate: func(show *codexSystemdShow) { show.Environment = "CODEX_HOME=/foreign" }},
	} {
		t.Run("loaded unit "+test.name, func(t *testing.T) {
			show := loaded
			test.mutate(&show)
			if loadedCodexServiceUnitMatches(fixture.paths, fixture.descriptor, show) {
				t.Fatalf("accepted mismatched %s", test.name)
			}
		})
	}

	for _, test := range []struct {
		name   string
		mutate func(*codexSystemdShow)
	}{
		{name: "active state", mutate: func(show *codexSystemdShow) { show.ActiveState = "inactive" }},
		{name: "substate", mutate: func(show *codexSystemdShow) { show.SubState = "dead" }},
		{name: "zero PID", mutate: func(show *codexSystemdShow) { show.MainPID = 0 }},
		{name: "invocation", mutate: func(show *codexSystemdShow) { show.InvocationID = "not-a-valid-invocation" }},
	} {
		t.Run("running process "+test.name, func(t *testing.T) {
			caseFixture := newCodexServiceFixture(t)
			show := fixtureCodexSystemdShow(t, caseFixture, "active", "running", 42, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			test.mutate(&show)
			readCalls, handshakeCalls := 0, 0
			caseFixture.service.readProcess = func(int) (codexServiceProcess, error) {
				readCalls++
				return codexServiceProcess{}, errors.New("process reader must not run")
			}
			caseFixture.service.handshake = func(context.Context, string) (codexServiceHandshake, error) {
				handshakeCalls++
				return codexServiceHandshake{}, errors.New("handshake must not run")
			}
			snapshot, err := caseFixture.service.inspectRunningConfiguration(context.Background(), caseFixture.paths, caseFixture.descriptor, CodexServiceSnapshot{State: CodexServiceDegraded}, show)
			if !errors.Is(err, ErrCodexServiceUnavailable) || snapshot.State != CodexServiceDegraded {
				t.Fatalf("snapshot=%+v err=%v", snapshot, err)
			}
			if readCalls != 0 || handshakeCalls != 0 {
				t.Fatalf("invalid %s reached process=%d handshake=%d", test.name, readCalls, handshakeCalls)
			}
		})
	}
}

func TestCodexServiceEnsureDoesNotStartUnsafeStates(t *testing.T) {
	t.Run("drifted but startable inventory", func(t *testing.T) {
		fixture := newCodexServiceFixture(t)
		starts := 0
		fixture.runner.run = func(_ context.Context, executable string, args []string) (command.Result, error) {
			switch {
			case executable == "systemctl" && reflect.DeepEqual(args, codexSystemdShowArgs(fixture.paths.unitName())):
				result := serviceShowForDescriptor(fixture.paths, fixture.descriptor, "inactive", "dead", 0, "")
				result.Stdout = []byte(strings.Replace(string(result.Stdout), "path="+fixture.descriptor.Executable, "path=/foreign/codex", 1))
				return result, nil
			case executable == fixture.descriptor.Executable && reflect.DeepEqual(args, []string{"--version"}):
				return command.Result{Stdout: []byte("codex-cli " + SupportedCodexVersion)}, nil
			case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "start", fixture.paths.unitName()}):
				starts++
				return command.Result{}, nil
			default:
				return command.Result{}, errors.New("unexpected command")
			}
		}
		_, err := fixture.service.Ensure(context.Background())
		if !errors.Is(err, ErrCodexServiceDrift) || starts != 0 {
			t.Fatalf("drifted Ensure err=%v starts=%d", err, starts)
		}
	})

	t.Run("unavailable active process", func(t *testing.T) {
		fixture := newCodexServiceFixture(t)
		fixture.service.timeout = 10 * time.Millisecond
		fixture.service.pollInterval = time.Millisecond
		fixture.service.lstat = func(path string) (fs.FileInfo, error) {
			if path == fixture.paths.RuntimeDir {
				return codexServiceFileInfo{mode: os.ModeDir | 0o700, uid: 1000, dir: true}, nil
			}
			return nil, os.ErrNotExist
		}
		starts := 0
		fixture.runner.run = func(_ context.Context, executable string, args []string) (command.Result, error) {
			switch {
			case executable == "systemctl" && reflect.DeepEqual(args, codexSystemdShowArgs(fixture.paths.unitName())):
				return serviceShowForDescriptor(fixture.paths, fixture.descriptor, "active", "running", 42, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), nil
			case executable == fixture.descriptor.Executable && reflect.DeepEqual(args, []string{"--version"}):
				return command.Result{Stdout: []byte("codex-cli " + SupportedCodexVersion)}, nil
			case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "start", fixture.paths.unitName()}):
				starts++
				return command.Result{}, nil
			default:
				return command.Result{}, errors.New("unexpected command")
			}
		}
		_, err := fixture.service.Ensure(context.Background())
		if !errors.Is(err, ErrCodexServiceUnavailable) || starts != 0 {
			t.Fatalf("unavailable active Ensure err=%v starts=%d", err, starts)
		}
	})
}

func TestCodexServiceAwaitReadyUsesFallbackForNonpositiveInterval(t *testing.T) {
	fixture := newCodexServiceFixture(t)
	fixture.service.pollInterval = -time.Millisecond
	fixture.service.lstat = func(path string) (fs.FileInfo, error) {
		if path == fixture.paths.RuntimeDir {
			return codexServiceFileInfo{mode: os.ModeDir | 0o700, uid: 1000, dir: true}, nil
		}
		return nil, os.ErrNotExist
	}
	shows := 0
	fixture.runner.run = func(_ context.Context, executable string, args []string) (command.Result, error) {
		if executable != "systemctl" || !reflect.DeepEqual(args, codexSystemdShowArgs(fixture.paths.unitName())) {
			return command.Result{}, errors.New("unexpected command")
		}
		shows++
		return serviceShowForDescriptor(fixture.paths, fixture.descriptor, "active", "running", 42, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := fixture.service.awaitReady(ctx, fixture.paths, fixture.descriptor, CodexServiceSnapshot{State: CodexServiceDegraded})
	if !errors.Is(err, ErrCodexServiceUnavailable) || !errors.Is(err, context.DeadlineExceeded) || shows != 1 {
		t.Fatalf("await error=%v show calls=%d", err, shows)
	}
}

func TestCodexServiceShowUnitFailsClosedForTruncationAndRunnerErrors(t *testing.T) {
	fixture := newCodexServiceFixture(t)
	valid := serviceShowForDescriptor(fixture.paths, fixture.descriptor, "active", "running", 42, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	runFailure := errors.New("systemctl failed")
	for _, test := range []struct {
		name       string
		result     command.Result
		runErr     error
		wantError  error
		wantAbsent bool
	}{
		{name: "stdout truncated", result: command.Result{Stdout: valid.Stdout, StdoutTruncated: true}, wantError: ErrCodexServiceReceipt},
		{name: "stderr truncated", result: command.Result{Stdout: valid.Stdout, StderrTruncated: true}, wantError: ErrCodexServiceReceipt},
		{name: "loaded runner error", result: valid, runErr: runFailure, wantError: ErrCodexServiceUnavailable},
		{name: "not found runner error", result: notFoundShow(), runErr: runFailure, wantAbsent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture.runner.run = func(context.Context, string, []string) (command.Result, error) { return test.result, test.runErr }
			show, err := fixture.service.showUnit(context.Background(), fixture.paths.unitName())
			if test.wantAbsent {
				if err != nil || show.LoadState != "not-found" {
					t.Fatalf("show=%+v err=%v", show, err)
				}
				return
			}
			if !errors.Is(err, test.wantError) {
				t.Fatalf("show=%+v err=%v, want %v", show, err, test.wantError)
			}
			if test.runErr != nil && !errors.Is(err, test.runErr) {
				t.Fatalf("runner failure was not retained: %v", err)
			}
		})
	}
}

func TestCodexServiceValidateSocketRejectsEachOwnershipDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		target string
		info   codexServiceFileInfo
		err    error
		want   error
	}{
		{name: "missing runtime directory", target: "runtime", err: os.ErrNotExist, want: ErrCodexServiceUnavailable},
		{name: "runtime stat error", target: "runtime", err: errors.New("runtime failure"), want: ErrCodexServiceDrift},
		{name: "runtime symlink", target: "runtime", info: codexServiceFileInfo{mode: os.ModeDir | os.ModeSymlink | 0o700, uid: 1000, dir: true}, want: ErrCodexServiceDrift},
		{name: "runtime not directory", target: "runtime", info: codexServiceFileInfo{mode: 0o700, uid: 1000}, want: ErrCodexServiceDrift},
		{name: "runtime wrong mode", target: "runtime", info: codexServiceFileInfo{mode: os.ModeDir | 0o755, uid: 1000, dir: true}, want: ErrCodexServiceDrift},
		{name: "runtime wrong owner", target: "runtime", info: codexServiceFileInfo{mode: os.ModeDir | 0o700, uid: 2000, dir: true}, want: ErrCodexServiceDrift},
		{name: "missing socket", target: "socket", err: os.ErrNotExist, want: ErrCodexServiceUnavailable},
		{name: "socket stat error", target: "socket", err: errors.New("socket failure"), want: ErrCodexServiceDrift},
		{name: "socket symlink", target: "socket", info: codexServiceFileInfo{mode: os.ModeSocket | os.ModeSymlink | 0o600, uid: 1000}, want: ErrCodexServiceDrift},
		{name: "not socket", target: "socket", info: codexServiceFileInfo{mode: 0o600, uid: 1000}, want: ErrCodexServiceDrift},
		{name: "socket wrong mode", target: "socket", info: codexServiceFileInfo{mode: os.ModeSocket | 0o640, uid: 1000}, want: ErrCodexServiceDrift},
		{name: "socket wrong owner", target: "socket", info: codexServiceFileInfo{mode: os.ModeSocket | 0o600, uid: 2000}, want: ErrCodexServiceDrift},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCodexServiceFixture(t)
			fixture.service.lstat = func(path string) (fs.FileInfo, error) {
				if (test.target == "runtime" && path == fixture.paths.RuntimeDir) || (test.target == "socket" && path == fixture.paths.SocketPath) {
					if test.err != nil {
						return nil, test.err
					}
					return test.info, nil
				}
				if path == fixture.paths.RuntimeDir {
					return codexServiceFileInfo{mode: os.ModeDir | 0o700, uid: 1000, dir: true}, nil
				}
				return codexServiceFileInfo{mode: os.ModeSocket | 0o600, uid: 1000}, nil
			}
			if err := fixture.service.validateSocket(fixture.paths, fixture.descriptor); !errors.Is(err, test.want) {
				t.Fatalf("validate socket error=%v, want %v", err, test.want)
			}
		})
	}
}

func TestParseCodexSystemdShowRejectsBoundaryAndMissingFieldDrift(t *testing.T) {
	valid := notFoundShow().Stdout
	for _, test := range []struct {
		name      string
		raw       []byte
		truncated bool
	}{
		{name: "empty", raw: nil},
		{name: "truncated", raw: valid, truncated: true},
		{name: "oversized", raw: append(append([]byte(nil), valid...), make([]byte, maxCodexServiceReceiptBytes)...)},
		{name: "loaded without ExecStart", raw: []byte(strings.Replace(string(valid), "LoadState=not-found", "LoadState=loaded", 1))},
		{name: "negative PID", raw: []byte(strings.Replace(string(valid), "MainPID=0", "MainPID=-1", 1))},
		{name: "malformed PID", raw: []byte(strings.Replace(string(valid), "MainPID=0", "MainPID=not-a-pid", 1))},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseCodexSystemdShow(test.raw, test.truncated); !errors.Is(err, ErrCodexServiceReceipt) {
				t.Fatalf("parse error=%v", err)
			}
		})
	}

	exactLimit := append([]byte(nil), valid...)
	remaining := maxCodexServiceReceiptBytes - len(exactLimit) - len("Ignored=\n")
	if remaining <= 0 {
		t.Fatal("not-found systemctl fixture unexpectedly exceeds receipt limit")
	}
	exactLimit = append(exactLimit, []byte("Ignored="+strings.Repeat("x", remaining)+"\n")...)
	if len(exactLimit) != maxCodexServiceReceiptBytes {
		t.Fatalf("exact receipt length=%d", len(exactLimit))
	}
	show, err := parseCodexSystemdShow(exactLimit, false)
	if err != nil || show.LoadState != "not-found" || show.MainPID != 0 {
		t.Fatalf("exact limit show=%+v err=%v", show, err)
	}
}

func TestCodexServiceStartableAndInvocationIDRequireExactValues(t *testing.T) {
	for _, test := range []struct {
		name  string
		show  codexSystemdShow
		valid bool
	}{
		{name: "inactive loaded", show: codexSystemdShow{LoadState: "loaded", ActiveState: "inactive"}, valid: true},
		{name: "failed loaded", show: codexSystemdShow{LoadState: "loaded", ActiveState: "failed"}, valid: true},
		{name: "active loaded", show: codexSystemdShow{LoadState: "loaded", ActiveState: "active"}},
		{name: "inactive not loaded", show: codexSystemdShow{LoadState: "not-found", ActiveState: "inactive"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.show.startable(); got != test.valid {
				t.Fatalf("startable=%t, want %t", got, test.valid)
			}
		})
	}
	for _, test := range []struct {
		name, value string
		valid       bool
	}{
		{name: "lowercase hex", value: "0123456789abcdef0123456789abcdef", valid: true},
		{name: "short", value: "0123456789abcdef"},
		{name: "uppercase", value: "0123456789ABCDEF0123456789ABCDEF"},
		{name: "nonhex", value: "0123456789abcdef0123456789abcdeg"},
	} {
		t.Run("invocation "+test.name, func(t *testing.T) {
			if got := validInvocationID(test.value); got != test.valid {
				t.Fatalf("validInvocationID(%q)=%t, want %t", test.value, got, test.valid)
			}
		})
	}
}

func TestCodexServiceHandshakeRejectsEachMissingMetadataField(t *testing.T) {
	const request = `{"id":1,"method":"initialize","params":{"clientInfo":{"name":"herdr-plugin-kit","version":"0.3.0"},"capabilities":{"experimentalApi":true}}}`
	for _, missing := range []string{"userAgent", "codexHome", "platformFamily", "platformOs"} {
		t.Run(missing, func(t *testing.T) {
			userAgent, home, family, platform := "codex-cli/"+SupportedCodexVersion, "/fixture/home", "unix", "linux"
			switch missing {
			case "userAgent":
				userAgent = ""
			case "codexHome":
				home = ""
			case "platformFamily":
				family = ""
			case "platformOs":
				platform = ""
			}
			socket := unixServer(t, func(ctx context.Context, connection *websocket.Conn) {
				receive(t, ctx, connection, request)
				send(t, ctx, connection, fmt.Sprintf(`{"id":1,"result":{"userAgent":%q,"codexHome":%q,"platformFamily":%q,"platformOs":%q}}`, userAgent, home, family, platform))
			})
			if _, err := readCodexServiceHandshake(context.Background(), socket); !errors.Is(err, errCodexProtocol) {
				t.Fatalf("missing %s handshake error=%v", missing, err)
			}
		})
	}
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
