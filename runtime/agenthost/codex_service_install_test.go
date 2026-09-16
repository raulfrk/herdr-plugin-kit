package agenthost

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

type codexServiceInstallFixture struct {
	paths      codexServicePaths
	home       string
	executable string
	service    *CodexService
	runner     *codexServiceRunner
}

func newCodexServiceInstallFixture(t *testing.T) codexServiceInstallFixture {
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
	executable := filepath.Join(home, "packages", "standalone", "current", "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &codexServiceRunner{}
	service := NewCodexService(runner)
	service.resolvePaths = func() (codexServicePaths, error) { return paths, nil }
	service.resolveHome = func() (string, error) { return home, nil }
	service.nativeDaemonActive = func(context.Context, string) (bool, error) { return false, nil }
	service.euid = func() int { return 1000 }
	return codexServiceInstallFixture{paths: paths, home: home, executable: executable, service: service, runner: runner}
}

func (f codexServiceInstallFixture) normalInstallRunner() func(context.Context, string, []string) (command.Result, error) {
	return func(_ context.Context, executable string, args []string) (command.Result, error) {
		switch {
		case executable == f.executable && reflect.DeepEqual(args, []string{"--version"}):
			return command.Result{Stdout: []byte("codex-cli " + SupportedCodexVersion + "\n")}, nil
		case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "show-environment"}):
			return command.Result{}, nil
		case executable == "loginctl" && reflect.DeepEqual(args, []string{"show-user", "1000", "--property=Linger", "--value"}):
			return command.Result{Stdout: []byte("yes\n")}, nil
		case executable == "systemd-analyze" && reflect.DeepEqual(args, []string{"verify", f.paths.UnitPath}):
			return command.Result{}, nil
		case executable == "systemctl" && reflect.DeepEqual(args, codexSystemdShowArgs(f.paths.unitName())):
			return notFoundShow(), nil
		case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "daemon-reload"}):
			return command.Result{}, nil
		default:
			return command.Result{}, errors.New("unexpected command")
		}
	}
}

// stagedCodexServiceFixture models the exact systemd distinction that matters
// during a configuration update: the manager has loaded B while its still
// running process was launched from A.
type stagedCodexServiceFixture struct {
	codexServiceFixture
	targetHome       string
	targetExecutable string
	loaded           bool
	loadedDescriptor codexServiceDescriptor
	running          codexServiceDescriptor
	pid              int
	invocation       string
	stopped          bool
	resolvedHome     string
	handshakeErr     error
}

func newStagedCodexServiceFixture(t *testing.T) *stagedCodexServiceFixture {
	t.Helper()
	base := newCodexServiceFixture(t)
	targetHome := filepath.Join(t.TempDir(), "target-home")
	targetExecutable := filepath.Join(targetHome, "codex")
	if err := os.MkdirAll(targetHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetExecutable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := &stagedCodexServiceFixture{
		codexServiceFixture: base,
		targetHome:          targetHome,
		targetExecutable:    targetExecutable,
		loaded:              true,
		loadedDescriptor:    base.descriptor,
		running:             base.descriptor,
		pid:                 42,
		invocation:          "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		resolvedHome:        base.descriptor.CodexHome,
	}
	fixture.service.resolveHome = func() (string, error) { return fixture.resolvedHome, nil }
	fixture.service.readProcess = func(int) (codexServiceProcess, error) {
		return codexServiceProcess{
			Executable: fixture.running.Executable,
			Argv:       []string{fixture.running.Executable, "app-server", "--listen", "unix://" + fixture.running.SocketPath},
			Environment: map[string]string{
				"CODEX_HOME": fixture.running.CodexHome,
			},
			Cgroup: "/user.slice/user-1000.slice/user@1000.service/app.slice/" + fixture.paths.unitName(),
		}, nil
	}
	fixture.service.handshake = func(context.Context, string) (codexServiceHandshake, error) {
		if fixture.handshakeErr != nil {
			return codexServiceHandshake{}, fixture.handshakeErr
		}
		return codexServiceHandshake{UserAgent: "codex-cli/" + SupportedCodexVersion, CodexHome: fixture.running.CodexHome, PlatformFamily: "unix", PlatformOS: "linux"}, nil
	}
	fixture.runner.run = fixture.run
	return fixture
}

func (f *stagedCodexServiceFixture) targetOptions() CodexServiceInstallOptions {
	return CodexServiceInstallOptions{Executable: f.targetExecutable, CodexHome: f.targetHome}
}

func (f *stagedCodexServiceFixture) sourceOptions() CodexServiceInstallOptions {
	return CodexServiceInstallOptions{Executable: f.descriptor.Executable, CodexHome: f.descriptor.CodexHome}
}

func (f *stagedCodexServiceFixture) stage(t *testing.T) CodexServiceInstallResult {
	t.Helper()
	result, err := f.service.Install(context.Background(), f.targetOptions())
	if err != nil {
		t.Fatal(err)
	}
	f.resolvedHome = f.targetHome
	return result
}

func (f *stagedCodexServiceFixture) run(_ context.Context, executable string, args []string) (command.Result, error) {
	switch {
	case (executable == f.descriptor.Executable || executable == f.targetExecutable) && reflect.DeepEqual(args, []string{"--version"}):
		return command.Result{Stdout: []byte("codex-cli " + SupportedCodexVersion + "\n")}, nil
	case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "show-environment"}):
		return command.Result{}, nil
	case executable == "loginctl" && reflect.DeepEqual(args, []string{"show-user", "1000", "--property=Linger", "--value"}):
		return command.Result{Stdout: []byte("yes\n")}, nil
	case executable == "systemd-analyze" && reflect.DeepEqual(args, []string{"verify", f.paths.UnitPath}):
		return command.Result{}, nil
	case executable == "systemctl" && reflect.DeepEqual(args, codexSystemdShowArgs(f.paths.unitName())):
		if !f.loaded {
			return notFoundShow(), nil
		}
		if f.stopped {
			return stoppedShow(f.paths, f.loadedDescriptor), nil
		}
		return serviceShowForDescriptor(f.paths, f.loadedDescriptor, "active", "running", f.pid, f.invocation), nil
	case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "daemon-reload"}):
		if _, err := os.Lstat(f.paths.UnitPath); errors.Is(err, os.ErrNotExist) {
			f.loaded = false
			return command.Result{}, nil
		}
		descriptor, err := readOwnedCodexService(f.paths)
		if err != nil {
			return command.Result{}, err
		}
		f.loaded = true
		f.loadedDescriptor = descriptor
		return command.Result{}, nil
	case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "enable", f.paths.unitName()}):
		return command.Result{}, nil
	case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "start", f.paths.unitName()}):
		f.startDesired()
		return command.Result{}, nil
	case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "restart", f.paths.unitName()}):
		f.startDesired()
		return command.Result{}, nil
	case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "disable", f.paths.unitName()}):
		return command.Result{}, nil
	case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "stop", f.paths.unitName()}):
		f.stopped = true
		return command.Result{}, nil
	case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "is-enabled", f.paths.unitName()}):
		return command.Result{}, errors.New("not found")
	default:
		return command.Result{}, fmt.Errorf("unexpected command %s %q", executable, args)
	}
}

func (f *stagedCodexServiceFixture) startDesired() {
	descriptor, err := readOwnedCodexService(f.paths)
	if err != nil {
		panic(err)
	}
	descriptor.PendingTransition = nil
	f.running = descriptor
	f.pid++
	f.invocation = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	f.stopped = false
	f.handshakeErr = nil
}

func (f *stagedCodexServiceFixture) setNaturallyRestartedDesired() {
	f.startDesired()
	f.pid++
	f.invocation = "cccccccccccccccccccccccccccccccc"
}

func TestCodexServiceInstallRendersOwnedUnitAndIsIdempotent(t *testing.T) {
	fixture := newCodexServiceInstallFixture(t)
	fixture.runner.run = fixture.normalInstallRunner()
	first, err := fixture.service.Install(context.Background(), CodexServiceInstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || first.RestartRequired || first.Executable != fixture.executable || first.CodexHome != fixture.home {
		t.Fatalf("install=%+v", first)
	}
	unit, err := os.ReadFile(fixture.paths.UnitPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"Environment=\"CODEX_HOME=" + fixture.home + "\"",
		"WorkingDirectory=%h", "RuntimeDirectory=herdr-plugin-kit", "RuntimeDirectoryMode=0700", "UMask=0077",
		"ExecStart=\"" + fixture.executable + "\" \"app-server\" \"--listen\" \"unix://%t/herdr-plugin-kit/codex.sock\"",
		"Restart=on-failure", "RestartSec=2s", "WantedBy=default.target",
	} {
		if !strings.Contains(string(unit), required) {
			t.Fatalf("unit is missing %q:\n%s", required, unit)
		}
	}
	if info, err := os.Stat(fixture.paths.UnitPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("unit mode=%v err=%v", info.Mode(), err)
	}
	if info, err := os.Stat(fixture.paths.DescriptorPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("descriptor mode=%v err=%v", info.Mode(), err)
	}

	fixture.runner.mu.Lock()
	fixture.runner.calls = nil
	fixture.runner.mu.Unlock()
	second, err := fixture.service.Install(context.Background(), CodexServiceInstallOptions{})
	if err != nil || second.Changed || second.RestartRequired {
		t.Fatalf("second install=%+v err=%v", second, err)
	}
	for _, call := range fixture.runner.observed() {
		if call.executable == "systemctl" && (len(call.args) > 1 && (call.args[1] == "start" || call.args[1] == "restart")) {
			t.Fatalf("idempotent install restarted service: %+v", call)
		}
	}
}

func TestCodexServiceInstallChangedConfigurationRequiresExplicitRestart(t *testing.T) {
	fixture := newCodexServiceInstallFixture(t)
	fixture.runner.run = fixture.normalInstallRunner()
	if _, err := fixture.service.Install(context.Background(), CodexServiceInstallOptions{}); err != nil {
		t.Fatal(err)
	}
	otherHome := filepath.Join(t.TempDir(), "other-home")
	otherExecutable := filepath.Join(otherHome, "codex")
	if err := os.MkdirAll(otherHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherExecutable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.runner.run = func(_ context.Context, executable string, args []string) (command.Result, error) {
		switch {
		case executable == otherExecutable && reflect.DeepEqual(args, []string{"--version"}):
			return command.Result{Stdout: []byte("codex-cli " + SupportedCodexVersion)}, nil
		case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "show-environment"}):
			return command.Result{}, nil
		case executable == "loginctl":
			return command.Result{Stdout: []byte("yes\n")}, nil
		case executable == "systemd-analyze":
			return command.Result{}, nil
		case executable == "systemctl" && reflect.DeepEqual(args, codexSystemdShowArgs(fixture.paths.unitName())):
			return serviceShowForDescriptor(fixture.paths, codexServiceDescriptor{Executable: fixture.executable, CodexHome: fixture.home, SocketPath: fixture.paths.SocketPath}, "inactive", "dead", 0, ""), nil
		case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "daemon-reload"}):
			return command.Result{}, nil
		default:
			return command.Result{}, errors.New("unexpected command")
		}
	}
	result, err := fixture.service.Install(context.Background(), CodexServiceInstallOptions{Executable: otherExecutable, CodexHome: otherHome})
	if err != nil || !result.Changed || !result.RestartRequired {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, call := range fixture.runner.observed() {
		if call.executable == "systemctl" && len(call.args) > 1 && (call.args[1] == "start" || call.args[1] == "restart") {
			t.Fatalf("configuration update restarted service: %+v", call)
		}
	}
}

func TestCodexServiceInstallStagesActiveTransitionAndConfirmedRestartUsesDesiredConfiguration(t *testing.T) {
	fixture := newStagedCodexServiceFixture(t)
	staged := fixture.stage(t)
	if !staged.Changed || !staged.RestartRequired {
		t.Fatalf("staged install=%+v", staged)
	}
	descriptor, err := readOwnedCodexService(fixture.paths)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.PendingTransition == nil || descriptor.PendingTransition.MainPID != 42 || descriptor.PendingTransition.InvocationID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || descriptor.PendingTransition.Previous.Executable != fixture.descriptor.Executable || descriptor.PendingTransition.Previous.CodexHome != fixture.descriptor.CodexHome {
		t.Fatalf("pending transition=%+v", descriptor.PendingTransition)
	}

	fixture.runner.mu.Lock()
	fixture.runner.calls = nil
	fixture.runner.mu.Unlock()
	repeated, err := fixture.service.Install(context.Background(), fixture.targetOptions())
	if err != nil || repeated.Changed || !repeated.RestartRequired {
		t.Fatalf("repeated staged install=%+v err=%v", repeated, err)
	}
	descriptor, err = readOwnedCodexService(fixture.paths)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.PendingTransition == nil || descriptor.PendingTransition.MainPID != 42 || descriptor.PendingTransition.InvocationID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || descriptor.PendingTransition.Previous.Executable != fixture.descriptor.Executable {
		t.Fatalf("repeated stage replaced source receipt=%+v", descriptor.PendingTransition)
	}

	if snapshot, err := fixture.service.Ensure(context.Background()); !errors.Is(err, ErrCodexServiceDrift) || snapshot.State == CodexServiceReady {
		t.Fatalf("Ensure accepted previous process as desired: snapshot=%+v err=%v", snapshot, err)
	}
	if snapshot, err := fixture.service.Start(context.Background()); !errors.Is(err, ErrCodexServiceDrift) || snapshot.State == CodexServiceReady {
		t.Fatalf("Start accepted previous process as desired: snapshot=%+v err=%v", snapshot, err)
	}

	restarted, err := fixture.service.Restart(context.Background(), true)
	if err != nil || restarted.State != CodexServiceReady || restarted.CodexHome != fixture.targetHome || restarted.MainPID == 42 || restarted.InvocationID == "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("restart snapshot=%+v err=%v", restarted, err)
	}
	descriptor, err = readOwnedCodexService(fixture.paths)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.PendingTransition != nil {
		t.Fatalf("confirmed desired restart retained transition=%+v", descriptor.PendingTransition)
	}
}

func TestCodexServiceMaintenanceUsesExactOwnershipWithoutRequiringHealthySource(t *testing.T) {
	t.Run("restart recovers unresponsive desired process", func(t *testing.T) {
		fixture := newStagedCodexServiceFixture(t)
		fixture.stage(t)
		fixture.setNaturallyRestartedDesired()
		fixture.handshakeErr = errors.New("desired app server is unresponsive")
		restarted, err := fixture.service.Restart(context.Background(), true)
		if err != nil || restarted.State != CodexServiceReady || restarted.CodexHome != fixture.targetHome || restarted.MainPID == 42 {
			t.Fatalf("restart after unresponsive desired process snapshot=%+v err=%v", restarted, err)
		}
	})
	t.Run("restart recovers unresponsive receipt-bound source", func(t *testing.T) {
		fixture := newStagedCodexServiceFixture(t)
		fixture.stage(t)
		fixture.handshakeErr = errors.New("source app server is unresponsive")
		restarted, err := fixture.service.Restart(context.Background(), true)
		if err != nil || restarted.State != CodexServiceReady || restarted.CodexHome != fixture.targetHome || restarted.MainPID == 42 {
			t.Fatalf("restart after unresponsive source snapshot=%+v err=%v", restarted, err)
		}
	})
	t.Run("uninstall stops unresponsive receipt-bound source", func(t *testing.T) {
		fixture := newStagedCodexServiceFixture(t)
		fixture.stage(t)
		fixture.handshakeErr = errors.New("source app server is unresponsive")
		removed, err := fixture.service.Uninstall(context.Background(), true)
		if err != nil || !removed.RemovedUnit || !removed.RemovedDescriptor {
			t.Fatalf("uninstall after unresponsive source result=%+v err=%v", removed, err)
		}
	})
	for _, operation := range []struct {
		name string
		run  func(*stagedCodexServiceFixture) error
	}{
		{name: "restart", run: func(fixture *stagedCodexServiceFixture) error {
			_, err := fixture.service.Restart(context.Background(), true)
			return err
		}},
		{name: "uninstall", run: func(fixture *stagedCodexServiceFixture) error {
			_, err := fixture.service.Uninstall(context.Background(), true)
			return err
		}},
	} {
		t.Run("foreign receipt identity "+operation.name, func(t *testing.T) {
			fixture := newStagedCodexServiceFixture(t)
			fixture.stage(t)
			fixture.handshakeErr = errors.New("source app server is unresponsive")
			fixture.invocation = "dddddddddddddddddddddddddddddddd"
			fixture.runner.mu.Lock()
			fixture.runner.calls = nil
			fixture.runner.mu.Unlock()
			if err := operation.run(fixture); !errors.Is(err, ErrCodexServiceDrift) {
				t.Fatalf("foreign receipt identity error=%v", err)
			}
			assertNoCodexServiceManagerMutation(t, fixture.runner.observed())
		})
	}
}

func TestCodexServiceInstallRestoresPreviousPendingConfigurationWhenStoppedOrAbsent(t *testing.T) {
	for _, state := range []struct {
		name  string
		apply func(*stagedCodexServiceFixture)
	}{
		{name: "stopped", apply: func(fixture *stagedCodexServiceFixture) { fixture.stopped = true }},
		{name: "not found", apply: func(fixture *stagedCodexServiceFixture) { fixture.stopped, fixture.loaded = true, false }},
	} {
		t.Run(state.name, func(t *testing.T) {
			fixture := newStagedCodexServiceFixture(t)
			fixture.stage(t)
			state.apply(fixture)
			result, err := fixture.service.Install(context.Background(), fixture.sourceOptions())
			if err != nil || !result.Changed || !result.RestartRequired {
				t.Fatalf("restore source install result=%+v err=%v", result, err)
			}
			fixture.resolvedHome = fixture.descriptor.CodexHome
			descriptor, err := readOwnedCodexService(fixture.paths)
			if err != nil || descriptor.PendingTransition != nil || !sameCodexServiceLaunchConfiguration(descriptor.launchConfiguration(), fixture.descriptor.launchConfiguration()) {
				t.Fatalf("restored descriptor=%+v err=%v", descriptor, err)
			}
			started, err := fixture.service.Start(context.Background())
			if err != nil || started.State != CodexServiceReady || started.CodexHome != fixture.descriptor.CodexHome {
				t.Fatalf("restored source Start snapshot=%+v err=%v", started, err)
			}
		})
	}
}

func TestCodexServiceInstallPendingTransitionRejectsForeignInvocationBeforeWrite(t *testing.T) {
	fixture := newStagedCodexServiceFixture(t)
	fixture.stage(t)
	unitBefore, err := os.ReadFile(fixture.paths.UnitPath)
	if err != nil {
		t.Fatal(err)
	}
	descriptorBefore, err := os.ReadFile(fixture.paths.DescriptorPath)
	if err != nil {
		t.Fatal(err)
	}
	fixture.invocation = "dddddddddddddddddddddddddddddddd"
	fixture.runner.mu.Lock()
	fixture.runner.calls = nil
	fixture.runner.mu.Unlock()
	if _, err := fixture.service.Install(context.Background(), fixture.targetOptions()); !errors.Is(err, ErrCodexServiceDrift) {
		t.Fatalf("foreign invocation install error=%v", err)
	}
	unitAfter, err := os.ReadFile(fixture.paths.UnitPath)
	if err != nil || string(unitAfter) != string(unitBefore) {
		t.Fatalf("foreign invocation changed unit: err=%v before=%q after=%q", err, unitBefore, unitAfter)
	}
	descriptorAfter, err := os.ReadFile(fixture.paths.DescriptorPath)
	if err != nil || string(descriptorAfter) != string(descriptorBefore) {
		t.Fatalf("foreign invocation changed descriptor: err=%v before=%q after=%q", err, descriptorBefore, descriptorAfter)
	}
	for _, call := range fixture.runner.observed() {
		if call.executable == "systemd-analyze" || reflect.DeepEqual(call.args, []string{"--user", "daemon-reload"}) {
			t.Fatalf("foreign invocation reached service mutation: %+v", call)
		}
	}
}

func TestCodexServiceUninstallAcceptsExactPendingPreviousProcess(t *testing.T) {
	fixture := newStagedCodexServiceFixture(t)
	fixture.stage(t)
	result, err := fixture.service.Uninstall(context.Background(), true)
	if err != nil || !result.RemovedUnit || !result.RemovedDescriptor {
		t.Fatalf("pending uninstall result=%+v err=%v", result, err)
	}
}

func TestCodexServiceEnsureAcceptsNaturallyRestartedDesiredProcessAndClearsReceipt(t *testing.T) {
	fixture := newStagedCodexServiceFixture(t)
	fixture.stage(t)
	fixture.setNaturallyRestartedDesired()
	ready, err := fixture.service.Ensure(context.Background())
	if err != nil || ready.State != CodexServiceReady || ready.CodexHome != fixture.targetHome || ready.InvocationID != "cccccccccccccccccccccccccccccccc" {
		t.Fatalf("naturally restarted desired snapshot=%+v err=%v", ready, err)
	}
	descriptor, err := readOwnedCodexService(fixture.paths)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.PendingTransition != nil {
		t.Fatalf("desired process readiness retained transition=%+v", descriptor.PendingTransition)
	}
	for _, call := range fixture.runner.observed() {
		if call.executable == "systemctl" && len(call.args) > 1 && call.args[1] == "start" {
			t.Fatalf("Ensure restarted already-ready desired process: %+v", call)
		}
	}
}

func TestCodexServiceInstallRejectsNativeDaemonAndConflictingFiles(t *testing.T) {
	t.Run("native daemon", func(t *testing.T) {
		fixture := newCodexServiceInstallFixture(t)
		fixture.service.nativeDaemonActive = func(context.Context, string) (bool, error) { return true, nil }
		if _, err := fixture.service.Install(context.Background(), CodexServiceInstallOptions{}); !errors.Is(err, ErrCodexServiceMigrationRequired) {
			t.Fatalf("err=%v", err)
		}
		if calls := fixture.runner.observed(); len(calls) != 0 {
			t.Fatalf("native-daemon rejection ran command: %+v", calls)
		}
	})
	t.Run("dropin", func(t *testing.T) {
		fixture := newCodexServiceInstallFixture(t)
		fixture.runner.run = fixture.normalInstallRunner()
		if err := ensureCodexServiceDirectories(fixture.paths); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(fixture.paths.UnitPath+".d", 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.service.Install(context.Background(), CodexServiceInstallOptions{}); !errors.Is(err, ErrCodexServiceOwnership) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("invalid version", func(t *testing.T) {
		fixture := newCodexServiceInstallFixture(t)
		fixture.runner.run = func(_ context.Context, executable string, args []string) (command.Result, error) {
			if executable == fixture.executable && reflect.DeepEqual(args, []string{"--version"}) {
				return command.Result{Stdout: []byte("codex-cli 0.153.0")}, nil
			}
			return command.Result{}, errors.New("unexpected command")
		}
		if _, err := fixture.service.Install(context.Background(), CodexServiceInstallOptions{}); !errors.Is(err, ErrCodexServiceIncompatible) {
			t.Fatalf("err=%v", err)
		}
		if _, err := os.Stat(fixture.paths.UnitPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid binary wrote unit: %v", err)
		}
	})
}

func TestCodexServiceLifecycleOwnershipRejectsUnsafeDirectoriesAndFilesBeforeMutation(t *testing.T) {
	t.Run("unsafe directory", func(t *testing.T) {
		fixture := newCodexServiceInstallFixture(t)
		if err := os.MkdirAll(fixture.paths.KitConfigDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(fixture.paths.KitConfigDir, 0o777); err != nil {
			t.Fatal(err)
		}
		fixture.runner.run = fixture.normalInstallRunner()
		if _, err := fixture.service.Install(context.Background(), CodexServiceInstallOptions{}); !errors.Is(err, ErrCodexServiceOwnership) {
			t.Fatalf("unsafe directory install error=%v", err)
		}
		if _, err := os.Lstat(fixture.paths.UnitPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unsafe directory wrote unit: %v", err)
		}
		if _, err := os.Lstat(fixture.paths.DescriptorPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unsafe directory wrote descriptor: %v", err)
		}
		assertNoCodexServiceManagerMutation(t, fixture.runner.observed())
	})
	t.Run("unsafe owned file", func(t *testing.T) {
		for _, path := range []string{"unit", "descriptor"} {
			t.Run(path, func(t *testing.T) {
				fixture := newCodexServiceFixture(t)
				file := fixture.paths.UnitPath
				if path == "descriptor" {
					file = fixture.paths.DescriptorPath
				}
				before, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(file, 0o666); err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.service.Ensure(context.Background()); !errors.Is(err, ErrCodexServiceOwnership) {
					t.Fatalf("unsafe file Ensure error=%v", err)
				}
				after, err := os.ReadFile(file)
				if err != nil || string(after) != string(before) {
					t.Fatalf("unsafe file changed: err=%v before=%q after=%q", err, before, after)
				}
				assertNoCodexServiceManagerMutation(t, fixture.runner.observed())
			})
		}
	})
}

func TestOwnedSafeCodexServiceLifecyclePathRequiresOwnerAndNoGroupOrWorldWrite(t *testing.T) {
	for _, test := range []struct {
		name string
		info codexServiceFileInfo
		want bool
	}{
		{name: "safe", info: codexServiceFileInfo{mode: 0o755, uid: 1000}, want: true},
		{name: "foreign owner", info: codexServiceFileInfo{mode: 0o700, uid: 1001}, want: false},
		{name: "group writable", info: codexServiceFileInfo{mode: 0o720, uid: 1000}, want: false},
		{name: "world writable", info: codexServiceFileInfo{mode: 0o707, uid: 1000}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ownedSafeCodexServiceLifecyclePath(test.info, 1000); got != test.want {
				t.Fatalf("ownedSafeCodexServiceLifecyclePath(%#o, uid=%d)=%t want %t", test.info.mode, test.info.uid, got, test.want)
			}
		})
	}
}

func assertNoCodexServiceManagerMutation(t *testing.T, calls []codexServiceCall) {
	t.Helper()
	for _, call := range calls {
		if call.executable != "systemctl" && call.executable != "systemd-analyze" {
			continue
		}
		if call.executable == "systemd-analyze" || (len(call.args) > 1 && (call.args[1] == "daemon-reload" || call.args[1] == "enable" || call.args[1] == "start" || call.args[1] == "restart" || call.args[1] == "disable" || call.args[1] == "stop")) {
			t.Fatalf("ownership refusal reached manager mutation: %+v", call)
		}
	}
}

func TestCodexServiceInstallRefusesForeignLoadedUnitBeforeWriting(t *testing.T) {
	fixture := newCodexServiceInstallFixture(t)
	normal := fixture.normalInstallRunner()
	fixture.runner.run = func(ctx context.Context, executable string, args []string) (command.Result, error) {
		if executable == "systemctl" && reflect.DeepEqual(args, codexSystemdShowArgs(fixture.paths.unitName())) {
			foreign := serviceShowForDescriptor(fixture.paths, codexServiceDescriptor{
				Executable: fixture.executable, CodexHome: fixture.home, SocketPath: fixture.paths.SocketPath,
			}, "inactive", "dead", 0, "")
			foreign.Stdout = []byte(strings.Replace(string(foreign.Stdout), "FragmentPath="+fixture.paths.UnitPath, "FragmentPath=/etc/systemd/user/"+fixture.paths.unitName(), 1))
			return foreign, nil
		}
		return normal(ctx, executable, args)
	}
	if _, err := fixture.service.Install(context.Background(), CodexServiceInstallOptions{}); !errors.Is(err, ErrCodexServiceOwnership) {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Lstat(fixture.paths.UnitPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign unit check wrote unit: %v", err)
	}
	if _, err := os.Lstat(fixture.paths.DescriptorPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign unit check wrote descriptor: %v", err)
	}
	for _, call := range fixture.runner.observed() {
		if call.executable == "systemd-analyze" || reflect.DeepEqual(call.args, []string{"--user", "daemon-reload"}) {
			t.Fatalf("foreign unit check mutated manager: %+v", call)
		}
	}
}

func TestCodexServiceStartRejectsNativeDaemonBeforeServiceCommands(t *testing.T) {
	fixture := newCodexServiceFixture(t)
	fixture.service.nativeDaemonActive = func(context.Context, string) (bool, error) { return true, nil }
	if _, err := fixture.service.Start(context.Background()); !errors.Is(err, ErrCodexServiceMigrationRequired) {
		t.Fatalf("err=%v", err)
	}
	if calls := fixture.runner.observed(); len(calls) != 0 {
		t.Fatalf("start touched service despite native daemon: %+v", calls)
	}
}

func TestCodexServiceStartAndRestartRefuseChangedLoadedExecStart(t *testing.T) {
	for _, operation := range []struct {
		name string
		run  func(*CodexService) error
	}{
		{name: "start", run: func(service *CodexService) error { _, err := service.Start(context.Background()); return err }},
		{name: "restart", run: func(service *CodexService) error { _, err := service.Restart(context.Background(), true); return err }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			fixture := newCodexServiceFixture(t)
			fixture.runner.run = changedExecStartRunner(fixture)
			if err := operation.run(fixture.service); !errors.Is(err, ErrCodexServiceOwnership) {
				t.Fatalf("err=%v", err)
			}
			for _, call := range fixture.runner.observed() {
				if call.executable == "systemctl" && len(call.args) >= 3 && (call.args[1] == "enable" || call.args[1] == "start" || call.args[1] == "restart") {
					t.Fatalf("changed inventory reached lifecycle mutation: %+v", call)
				}
			}
		})
	}
}

func changedExecStartRunner(fixture codexServiceFixture) func(context.Context, string, []string) (command.Result, error) {
	return func(_ context.Context, executable string, args []string) (command.Result, error) {
		switch {
		case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "show-environment"}):
			return command.Result{}, nil
		case executable == "loginctl" && reflect.DeepEqual(args, []string{"show-user", "1000", "--property=Linger", "--value"}):
			return command.Result{Stdout: []byte("yes\n")}, nil
		case executable == "systemctl" && reflect.DeepEqual(args, codexSystemdShowArgs(fixture.paths.unitName())):
			show := serviceShowForDescriptor(fixture.paths, fixture.descriptor, "inactive", "dead", 0, "")
			show.Stdout = []byte(strings.Replace(string(show.Stdout), "path="+fixture.descriptor.Executable, "path=/foreign/codex", 1))
			return show, nil
		default:
			return command.Result{}, errors.New("unexpected command")
		}
	}
}

func TestCodexServiceUninstallRequiresConfirmationAndPreservesCodexData(t *testing.T) {
	fixture := newCodexServiceFixture(t)
	auth := filepath.Join(fixture.descriptor.CodexHome, "auth.json")
	history := filepath.Join(fixture.descriptor.CodexHome, "history.jsonl")
	if err := os.WriteFile(auth, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(history, []byte("conversation"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Uninstall(context.Background(), false); !errors.Is(err, ErrCodexServiceConfirmationRequired) {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(fixture.paths.UnitPath); err != nil {
		t.Fatalf("unconfirmed uninstall removed unit: %v", err)
	}
	fixture.runner.run = uninstallRunner(fixture, false)
	result, err := fixture.service.Uninstall(context.Background(), true)
	if err != nil || !result.RemovedUnit || !result.RemovedDescriptor {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, path := range []string{auth, history} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("uninstall removed retained Codex data %q: %v", path, err)
		}
	}
	if _, err := os.Stat(fixture.paths.RuntimeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture runtime directory should not have been created or removed: %v", err)
	}
}

func TestCodexServiceUninstallRetriesAfterDaemonReloadFailure(t *testing.T) {
	fixture := newCodexServiceFixture(t)
	fixture.runner.run = uninstallRunner(fixture, true)
	first, err := fixture.service.Uninstall(context.Background(), true)
	if !first.RemovedUnit || first.RemovedDescriptor || !errors.Is(err, ErrCodexServiceUnavailable) {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err := os.Stat(fixture.paths.DescriptorPath); err != nil {
		t.Fatalf("failed uninstall lost descriptor: %v", err)
	}
	fixture.runner.run = uninstallRetryRunner(fixture)
	second, err := fixture.service.Uninstall(context.Background(), true)
	if err != nil || second.RemovedUnit || !second.RemovedDescriptor {
		t.Fatalf("second=%+v err=%v", second, err)
	}
}

func uninstallRunner(fixture codexServiceFixture, failReload bool) func(context.Context, string, []string) (command.Result, error) {
	stopped := false
	return func(_ context.Context, executable string, args []string) (command.Result, error) {
		switch {
		case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "show-environment"}):
			return command.Result{}, nil
		case executable == "loginctl" && reflect.DeepEqual(args, []string{"show-user", "1000", "--property=Linger", "--value"}):
			return command.Result{Stdout: []byte("yes\n")}, nil
		case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "disable", codexServiceUnitName}):
			return command.Result{}, nil
		case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "stop", codexServiceUnitName}):
			stopped = true
			return command.Result{}, nil
		case executable == "systemctl" && reflect.DeepEqual(args, codexSystemdShowArgs(codexServiceUnitName)):
			if _, err := os.Stat(fixture.paths.UnitPath); errors.Is(err, os.ErrNotExist) {
				return notFoundShow(), nil
			}
			if stopped {
				return stoppedShow(fixture.paths, fixture.descriptor), nil
			}
			return serviceShowForDescriptor(fixture.paths, fixture.descriptor, "active", "running", 42, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), nil
		case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "daemon-reload"}):
			if failReload {
				return command.Result{}, errors.New("reload failed")
			}
			return command.Result{}, nil
		case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "is-enabled", codexServiceUnitName}):
			return command.Result{}, errors.New("not found")
		default:
			return command.Result{}, errors.New("unexpected command")
		}
	}
}

func uninstallRetryRunner(fixture codexServiceFixture) func(context.Context, string, []string) (command.Result, error) {
	return func(_ context.Context, executable string, args []string) (command.Result, error) {
		switch {
		case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "show-environment"}):
			return command.Result{}, nil
		case executable == "loginctl":
			return command.Result{Stdout: []byte("yes\n")}, nil
		case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "daemon-reload"}):
			return command.Result{}, nil
		case executable == "systemctl" && reflect.DeepEqual(args, codexSystemdShowArgs(codexServiceUnitName)):
			return notFoundShow(), nil
		case executable == "systemctl" && reflect.DeepEqual(args, []string{"--user", "is-enabled", codexServiceUnitName}):
			return command.Result{Stdout: []byte("not-found\n")}, errors.New("not found")
		default:
			return command.Result{}, errors.New("unexpected command")
		}
	}
}

func stoppedShow(paths codexServicePaths, descriptor codexServiceDescriptor) command.Result {
	result := serviceShowForDescriptor(paths, descriptor, "inactive", "dead", 0, "")
	result.Stdout = []byte(strings.Replace(string(result.Stdout), "UnitFileState=enabled", "UnitFileState=disabled", 1))
	return result
}

func notFoundShow() command.Result {
	return command.Result{Stdout: []byte("LoadState=not-found\nUnitFileState=not-found\nActiveState=inactive\nSubState=dead\nMainPID=0\nInvocationID=\nFragmentPath=\nEnvironment=\n")}
}

func TestQuoteSystemdValueEscapesLiteralPaths(t *testing.T) {
	got, err := quoteSystemdValue(`/tmp/a b/%c"d`, false)
	if err != nil || got != `"/tmp/a b/%%c\"d"` {
		t.Fatalf("quoted=%q err=%v", got, err)
	}
	if _, err := quoteSystemdValue("bad\nvalue", false); err == nil {
		t.Fatal("newline was accepted")
	}
}

func TestSystemdExecStartMatchesExactPathAndArgv(t *testing.T) {
	fixture := newCodexServiceFixture(t)
	show := serviceShowForDescriptor(fixture.paths, fixture.descriptor, "inactive", "dead", 0, "")
	parsed, err := parseCodexSystemdShow(show.Stdout, false)
	if err != nil || !systemdExecStartMatches(parsed.ExecStart, fixture.descriptor) {
		t.Fatalf("exact ExecStart match=%t err=%v", systemdExecStartMatches(parsed.ExecStart, fixture.descriptor), err)
	}
	changed := strings.Replace(parsed.ExecStart, fixture.descriptor.Executable, fixture.descriptor.Executable+"-foreign", 1)
	if systemdExecStartMatches(changed, fixture.descriptor) {
		t.Fatalf("accepted changed ExecStart %q", changed)
	}
}

func TestCodexServiceLifecycleLockBlocksSubprocessMaintenance(t *testing.T) {
	const child = "HERDR_PLUGIN_KIT_LIFECYCLE_LOCK_CHILD"
	const config = "HERDR_PLUGIN_KIT_LIFECYCLE_LOCK_CONFIG"
	if os.Getenv(child) == "1" {
		paths := lockTestPaths(os.Getenv(config))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		lock, err := acquireCodexServiceLifecycleLock(ctx, paths, false, os.Geteuid(), time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		fmt.Fprintln(os.Stdout, "locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}

	fixture := newCodexServiceFixture(t)
	paths := fixture.paths
	cmd := exec.Command(os.Args[0], "-test.run=^TestCodexServiceLifecycleLockBlocksSubprocessMaintenance$")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), child+"=1", config+"="+paths.ConfigHome)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	if scanner := bufio.NewScanner(stdout); !scanner.Scan() || scanner.Text() != "locked" {
		t.Fatalf("child did not acquire lifecycle lock: %v", scanner.Err())
	}
	blockedCtx, blockedCancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	_, err = fixture.service.Ensure(blockedCtx)
	blockedCancel()
	if !errors.Is(err, ErrCodexServiceUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ensure while external maintenance lock held error=%v", err)
	}
	blockedCtx, blockedCancel = context.WithTimeout(context.Background(), 25*time.Millisecond)
	_, err = fixture.service.Uninstall(blockedCtx, true)
	blockedCancel()
	if !errors.Is(err, ErrCodexServiceUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Uninstall while external maintenance lock held error=%v", err)
	}
	if _, err := os.Lstat(paths.UnitPath); err != nil {
		t.Fatalf("blocked maintenance changed unit: %v", err)
	}
	if _, err := os.Lstat(paths.DescriptorPath); err != nil {
		t.Fatalf("blocked maintenance changed descriptor: %v", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	availableCtx, availableCancel := context.WithTimeout(context.Background(), time.Second)
	lock, err := acquireCodexServiceLifecycleLock(availableCtx, paths, false, os.Geteuid(), time.Millisecond)
	availableCancel()
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexServiceLifecycleLockHonorsSameProcessDeadline(t *testing.T) {
	fixture := newCodexServiceFixture(t)
	heldCtx, heldCancel := context.WithTimeout(context.Background(), time.Second)
	defer heldCancel()
	lock, err := fixture.service.lifecycleLock(heldCtx, fixture.paths, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	blockedCtx, blockedCancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer blockedCancel()
	_, err = fixture.service.Ensure(blockedCtx)
	if !errors.Is(err, ErrCodexServiceUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ensure while same-process lifecycle lock held error=%v", err)
	}
	if calls := fixture.runner.observed(); len(calls) != 0 {
		t.Fatalf("same-process blocked Ensure ran commands: %+v", calls)
	}
}

func TestCodexServiceLifecycleLockMissRemainsNotInstalledWhenConcurrentInstallWins(t *testing.T) {
	for _, operation := range []struct {
		name string
		run  func(*CodexService) (CodexServiceSnapshot, error)
	}{
		{name: "Ensure", run: func(service *CodexService) (CodexServiceSnapshot, error) { return service.Ensure(context.Background()) }},
		{name: "Start", run: func(service *CodexService) (CodexServiceSnapshot, error) { return service.Start(context.Background()) }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			fixture := newCodexServiceFixture(t)
			if err := os.Remove(fixture.paths.UnitPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(fixture.paths.DescriptorPath); err != nil {
				t.Fatal(err)
			}
			installed := false
			fixture.service.afterLifecycleLockMiss = func() {
				writeOwnedCodexServiceFixture(t, fixture.paths, fixture.descriptor)
				installed = true
			}
			snapshot, err := operation.run(fixture.service)
			if !installed || err == nil || !errors.Is(err, ErrCodexServiceNotInstalled) || snapshot.State != CodexServiceDegraded {
				t.Fatalf("concurrent install result installed=%t snapshot=%+v err=%v", installed, snapshot, err)
			}
			if _, err := os.Stat(fixture.paths.DescriptorPath); err != nil {
				t.Fatalf("test interleaving did not install descriptor: %v", err)
			}
			if calls := fixture.runner.observed(); len(calls) != 0 {
				t.Fatalf("lock-miss branch ran service commands after concurrent install: %+v", calls)
			}
		})
	}
}

func lockTestPaths(configHome string) codexServicePaths {
	kitConfigDir := filepath.Join(configHome, "herdr-plugin-kit")
	return codexServicePaths{
		ConfigHome: configHome, KitConfigDir: kitConfigDir,
		UnitDirectory:  filepath.Join(configHome, "systemd", "user"),
		DescriptorPath: filepath.Join(kitConfigDir, codexServiceDescriptorName),
		UnitPath:       filepath.Join(configHome, "systemd", "user", codexServiceUnitName),
		RuntimeDir:     filepath.Join(filepath.Dir(configHome), "runtime", "herdr-plugin-kit"),
		SocketPath:     filepath.Join(filepath.Dir(configHome), "runtime", "herdr-plugin-kit", "codex.sock"),
	}
}

func TestDefaultCodexServicePathsUsesConfiguredRootsAndRejectsUnsafeRuntimeRoot(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	runtimeRoot := filepath.Join(root, "runtime")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	paths, err := defaultCodexServicePaths()
	if err != nil {
		t.Fatal(err)
	}
	if paths.ConfigHome != configHome || paths.KitConfigDir != filepath.Join(configHome, "herdr-plugin-kit") || paths.UnitDirectory != filepath.Join(configHome, "systemd", "user") || paths.UnitPath != filepath.Join(configHome, "systemd", "user", codexServiceUnitName) || paths.DescriptorPath != filepath.Join(configHome, "herdr-plugin-kit", codexServiceDescriptorName) || paths.RuntimeDir != filepath.Join(runtimeRoot, "herdr-plugin-kit") || paths.SocketPath != filepath.Join(runtimeRoot, "herdr-plugin-kit", "codex.sock") {
		t.Fatalf("unexpected paths: %+v", paths)
	}

	t.Run("uses user config fallback", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
		paths, err := defaultCodexServicePaths()
		if err != nil {
			t.Fatal(err)
		}
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(home, ".config"); paths.ConfigHome != want {
			t.Fatalf("config home=%q, want %q", paths.ConfigHome, want)
		}
	})

	t.Run("rejects relative config home", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "relative")
		t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
		if _, err := defaultCodexServicePaths(); err == nil {
			t.Fatal("relative XDG_CONFIG_HOME unexpectedly succeeded")
		}
	})

	for _, test := range []struct {
		name    string
		runtime string
	}{
		{name: "missing", runtime: filepath.Join(root, "missing")},
		{name: "regular file", runtime: filepath.Join(root, "runtime-file")},
		{name: "symlink", runtime: filepath.Join(root, "runtime-link")},
	} {
		t.Run("rejects runtime "+test.name, func(t *testing.T) {
			if test.name == "regular file" {
				if err := os.WriteFile(test.runtime, []byte("fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.name == "symlink" {
				if err := os.Symlink(runtimeRoot, test.runtime); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("XDG_CONFIG_HOME", configHome)
			t.Setenv("XDG_RUNTIME_DIR", test.runtime)
			if _, err := defaultCodexServicePaths(); err == nil {
				t.Fatalf("unsafe runtime root %q unexpectedly succeeded", test.runtime)
			}
		})
	}
}

func TestRestoreOwnedUnitReplacesOrRemovesOnlyExactCurrentContents(t *testing.T) {
	const current = "[Service]\nExecStart=current\n"
	const previous = "[Service]\nExecStart=previous\n"
	writeCurrent := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "fixture.service")
		if err := os.WriteFile(path, []byte(current), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("restores previous unit", func(t *testing.T) {
		path := writeCurrent(t)
		if err := restoreOwnedUnit(path, []byte(current), []byte(previous), true); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != previous {
			t.Fatalf("restored unit=%q err=%v", got, err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("restored unit mode=%#o err=%v", info.Mode().Perm(), err)
		}
	})

	t.Run("removes newly written unit", func(t *testing.T) {
		path := writeCurrent(t)
		if err := restoreOwnedUnit(path, []byte(current), nil, false); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("removed unit stat error=%v", err)
		}
	})

	for _, test := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{name: "missing", setup: func(t *testing.T, path string) {}},
		{name: "changed", setup: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unsafe permissions", setup: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(current), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o666); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run("refuses "+test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.service")
			test.setup(t, path)
			if err := restoreOwnedUnit(path, []byte(current), []byte(previous), true); !errors.Is(err, ErrCodexServiceOwnership) {
				t.Fatalf("restore error=%v", err)
			}
		})
	}
}
