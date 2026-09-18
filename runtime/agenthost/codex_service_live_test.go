package agenthost

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

// This opt-in check owns a transient task unit, temporary CODEX_HOME, and
// private socket. It never installs the production-named unit, touches an
// existing native daemon, or changes the user's real Codex home.
func TestCodexServiceLiveIsolatedSystemdFixture(t *testing.T) {
	if os.Getenv("HERDR_PLUGIN_KIT_CODEX_SERVICE_LIVE") != "1" {
		t.Skip("set HERDR_PLUGIN_KIT_CODEX_SERVICE_LIVE=1 for the isolated systemd App Server fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	installedHome, err := effectiveCodexHome()
	if err != nil {
		t.Skipf("supported installed Codex home is unavailable: %v", err)
	}
	executable, err := filepath.EvalSymlinks(filepath.Join(installedHome, "packages", "standalone", "current", "bin", "codex"))
	if err != nil {
		t.Skipf("supported installed Codex executable is unavailable: %v", err)
	}
	for _, name := range []string{"auth.json", "config.toml"} {
		source := filepath.Join(installedHome, name)
		if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(source, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	runner := command.ExecRunner{}
	version, err := runner.Run(ctx, executable, []string{"--version"})
	if err != nil || !strings.Contains(string(version.Stdout), SupportedCodexVersion) {
		t.Skipf("supported Codex %s is unavailable: result=%q err=%v", SupportedCodexVersion, version.Stdout, err)
	}
	unit := fmt.Sprintf("hpk-codex-service-live-%d.service", time.Now().UnixNano())
	socket := canonicalCodexServiceSocket(root)
	stop := func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, _ = runner.Run(cleanupCtx, "systemctl", []string{"--user", "stop", unit})
	}
	t.Cleanup(stop)
	if _, err := runner.Run(ctx, "systemd-run", []string{
		"--user", "--unit=" + unit,
		"--property=Type=exec", "--property=UMask=0077", "--property=Restart=no", "--property=TimeoutStopSec=10s",
		"--setenv=CODEX_HOME=" + root,
		executable, "app-server", "--listen", "unix://",
	}); err != nil {
		t.Fatal(err)
	}
	waitForLiveServiceSocket(t, ctx, socket)
	info, err := os.Lstat(socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket info=%v err=%v", info, err)
	}
	initialized, err := readCodexServiceHandshake(ctx, socket)
	if err != nil || !strings.Contains(initialized.UserAgent, "/"+SupportedCodexVersion) || initialized.CodexHome != root || initialized.PlatformFamily != "unix" || !strings.EqualFold(initialized.PlatformOS, "linux") {
		t.Fatalf("initialize=%+v err=%v", initialized, err)
	}
	showResult, err := runner.Run(ctx, "systemctl", codexSystemdShowArgs(unit))
	if err != nil {
		t.Fatal(err)
	}
	show, err := parseCodexSystemdShow(showResult.Stdout, showResult.StdoutTruncated || showResult.StderrTruncated)
	if err != nil || show.ActiveState != "active" || show.SubState != "running" || show.MainPID <= 0 || !validInvocationID(show.InvocationID) {
		t.Fatalf("show=%+v err=%v", show, err)
	}
	t.Logf("isolated fixture unit=%s socket=%s pid=%d invocation=%s", unit, socket, show.MainPID, show.InvocationID)
	process, err := readCodexServiceProcess(show.MainPID)
	if err != nil || process.Executable != executable || !sameStrings(process.Argv, []string{executable, "app-server", "--listen", "unix://"}) || process.Environment["CODEX_HOME"] != root || !strings.Contains(process.Cgroup, "/"+unit) {
		t.Fatalf("process executable=%q argv=%q CODEX_HOME=%q cgroup=%q err=%v", process.Executable, process.Argv, process.Environment["CODEX_HOME"], process.Cgroup, err)
	}
	if _, err := runner.Run(ctx, "systemctl", []string{"--user", "stop", unit}); err != nil {
		t.Fatal(err)
	}
	stopped, err := runner.Run(ctx, "systemctl", []string{"--user", "show", unit, "--property=ActiveState,SubState,MainPID", "--no-page"})
	if err != nil {
		t.Fatal(err)
	}
	active, substate, pid, err := parseStoppedFixtureShow(stopped.Stdout, stopped.StdoutTruncated || stopped.StderrTruncated)
	if err != nil || active != "inactive" || substate != "dead" || pid != 0 {
		t.Fatalf("stopped state=%s/%s pid=%d err=%v", active, substate, pid, err)
	}
	t.Logf("isolated fixture stopped unit=%s state=%s/%s pid=%d", unit, active, substate, pid)
}

// isolatedCodexServiceLifecycleFixture is deliberately test-only. It uses a
// uniquely named linked unit because exercising the canonical user's unit is
// a maintenance operation, not live-test setup. Other package live tests can
// inject Service into ManagedCodexLauncher.service. suppliedHome is optional;
// when non-empty it must already be a task-owned home and is never modified or
// removed by this helper.
type isolatedCodexServiceLifecycleFixture struct {
	Service    *CodexService
	Paths      codexServicePaths
	Home       string
	Executable string
}

func newIsolatedCodexServiceLifecycleFixture(t *testing.T, suppliedHome string) isolatedCodexServiceLifecycleFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	var err error
	installExecutable := ""
	if suppliedHome != "" {
		home, err = canonicalExistingDirectory(suppliedHome, true)
		if err != nil {
			t.Fatalf("task-owned CODEX_HOME: %v", err)
		}
		installExecutable, err = osexec.LookPath("codex")
		if err != nil {
			t.Fatalf("pinned Codex executable on PATH: %v", err)
		}
		installExecutable, err = filepath.EvalSymlinks(installExecutable)
		if err != nil {
			t.Fatalf("canonical pinned Codex executable: %v", err)
		}
	} else {
		installedHome, homeErr := effectiveCodexHome()
		if homeErr != nil {
			t.Skipf("supported installed Codex home is unavailable: %v", homeErr)
		}
		executable, executableErr := filepath.EvalSymlinks(filepath.Join(installedHome, "packages", "standalone", "current", "bin", "codex"))
		if executableErr != nil {
			t.Skipf("supported installed Codex executable is unavailable: %v", executableErr)
		}
		linkedExecutable := filepath.Join(home, "packages", "standalone", "current", "bin", "codex")
		if err := os.MkdirAll(filepath.Dir(linkedExecutable), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(executable, linkedExecutable); err != nil {
			t.Fatal(err)
		}
		installExecutable = linkedExecutable
	}
	runner := command.ExecRunner{}
	version, err := runner.Run(ctx, installExecutable, []string{"--version"})
	if err != nil || !strings.Contains(string(version.Stdout), SupportedCodexVersion) {
		t.Skipf("supported Codex %s is unavailable: result=%q err=%v", SupportedCodexVersion, version.Stdout, err)
	}
	runtimeRoot := os.Getenv("XDG_RUNTIME_DIR")
	runtimeInfo, err := os.Lstat(runtimeRoot)
	if !cleanAbsolute(runtimeRoot) || err != nil || runtimeInfo.Mode()&os.ModeSymlink != 0 || !runtimeInfo.IsDir() {
		t.Skipf("safe user runtime root is unavailable: %q: %v", runtimeRoot, err)
	}
	name := fmt.Sprintf("hpk-codex-service-lifecycle-%d-%d", os.Getpid(), time.Now().UnixNano())
	unitName := name + ".service"
	configHome := filepath.Join(root, "config")
	unitDirectory := filepath.Join(runtimeRoot, "systemd", "user")
	unitDirectoryInfo, err := os.Lstat(unitDirectory)
	if err != nil || unitDirectoryInfo.Mode()&os.ModeSymlink != 0 || !unitDirectoryInfo.IsDir() {
		t.Skipf("safe user runtime unit directory is unavailable: %q: %v", unitDirectory, err)
	}
	paths := codexServicePaths{
		UnitName:             unitName,
		RuntimeDirectoryName: name,
		ConfigHome:           configHome,
		KitConfigDir:         filepath.Join(configHome, "herdr-plugin-kit"),
		UnitDirectory:        unitDirectory,
		DescriptorPath:       filepath.Join(configHome, "herdr-plugin-kit", codexServiceDescriptorName),
		UnitPath:             filepath.Join(unitDirectory, unitName),
		RuntimeDir:           filepath.Join(runtimeRoot, name),
		SocketPath:           canonicalCodexServiceSocket(home),
	}
	service := NewCodexService(runner)
	service.resolvePaths = func() (codexServicePaths, error) { return paths, nil }
	service.resolveHome = func() (string, error) { return home, nil }
	service.timeout = 20 * time.Second
	service.pollInterval = 50 * time.Millisecond
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		// Every command and file names only this test's unique runtime unit.
		// The descriptor is under t.TempDir; systemd owns RuntimeDirectory.
		_, _ = runner.Run(cleanupCtx, "systemctl", []string{"--user", "disable", unitName})
		_, _ = runner.Run(cleanupCtx, "systemctl", []string{"--user", "stop", unitName})
		_, _ = runner.Run(cleanupCtx, "systemctl", []string{"--user", "reset-failed", unitName})
		if state, stateErr := inspectCodexServiceInstallation(paths); stateErr == nil && state.unitExists {
			if removeErr := removeExactOwnedFile(paths.UnitPath, state.unitBytes); removeErr != nil {
				t.Errorf("remove exact isolated runtime unit: %v", removeErr)
			}
		}
		_, _ = runner.Run(cleanupCtx, "systemctl", []string{"--user", "daemon-reload"})
	})
	if _, err := service.Install(ctx, CodexServiceInstallOptions{Executable: installExecutable, CodexHome: home}); err != nil {
		t.Fatalf("install isolated lifecycle fixture: %v", err)
	}
	executable, err := filepath.EvalSymlinks(installExecutable)
	if err != nil {
		t.Fatal(err)
	}
	return isolatedCodexServiceLifecycleFixture{Service: service, Paths: paths, Home: home, Executable: executable}
}

func TestCodexServiceLiveIsolatedLifecycle(t *testing.T) {
	if os.Getenv("HERDR_PLUGIN_KIT_CODEX_SERVICE_LIVE") != "1" {
		t.Skip("set HERDR_PLUGIN_KIT_CODEX_SERVICE_LIVE=1 for the isolated systemd lifecycle fixture")
	}
	fixture := newIsolatedCodexServiceLifecycleFixture(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ready, err := fixture.Service.Ensure(ctx)
	if err != nil {
		result, showErr := (command.ExecRunner{}).Run(ctx, "systemctl", codexSystemdShowArgs(fixture.Paths.unitName()))
		show, parseErr := parseCodexSystemdShow(result.Stdout, result.StdoutTruncated || result.StderrTruncated)
		runtimeInfo, runtimeErr := os.Lstat(fixture.Paths.RuntimeDir)
		socketInfo, socketErr := os.Lstat(fixture.Paths.SocketPath)
		process, processErr := readCodexServiceProcess(show.MainPID)
		t.Fatalf("Ensure error=%v showErr=%v parseErr=%v load=%q state=%q/%q fragment=%q execStartPresent=%t CODEX_HOMEMatches=%t runtime(mode=%#o owner=%t err=%v) socket(mode=%#o owner=%t err=%v) argv=%q processErr=%v", err, showErr, parseErr, show.LoadState, show.ActiveState, show.SubState, show.FragmentPath, show.ExecStart != "", systemdEnvironmentContains(show.Environment, "CODEX_HOME", fixture.Home), liveFixtureMode(runtimeInfo), runtimeInfo != nil && ownedBy(runtimeInfo, os.Geteuid()), runtimeErr, liveFixtureMode(socketInfo), socketInfo != nil && ownedBy(socketInfo, os.Geteuid()), socketErr, process.Argv, processErr)
	}
	if ready.State != CodexServiceReady || ready.SocketPath != fixture.Paths.SocketPath || ready.CodexHome != fixture.Home || ready.MainPID <= 0 || !validInvocationID(ready.InvocationID) {
		t.Fatalf("Ensure snapshot=%+v", ready)
	}
	inspected, err := fixture.Service.Inspect(ctx)
	if err != nil || inspected != ready {
		t.Fatalf("Inspect snapshot=%+v err=%v, want %+v", inspected, err, ready)
	}
	// Stage B from a second hard-link path to the exact same pinned binary.
	// EvalSymlinks deliberately preserves a hard-link path, so this proves the
	// active A process remains receipt-bound while systemd has loaded B.
	updatedExecutable := filepath.Join(filepath.Dir(fixture.Paths.ConfigHome), "pinned-codex-b")
	if err := os.Link(fixture.Executable, updatedExecutable); err != nil {
		t.Skipf("isolated staged-update hard link is unavailable: %v", err)
	}
	staged, err := fixture.Service.Install(ctx, CodexServiceInstallOptions{Executable: updatedExecutable, CodexHome: fixture.Home})
	if err != nil || !staged.Changed || !staged.RestartRequired {
		t.Fatalf("staged update result=%+v err=%v", staged, err)
	}
	descriptor, err := readOwnedCodexService(fixture.Paths)
	if err != nil || descriptor.PendingTransition == nil || descriptor.PendingTransition.MainPID != ready.MainPID || descriptor.PendingTransition.InvocationID != ready.InvocationID || descriptor.PendingTransition.Previous.Executable != fixture.Executable {
		t.Fatalf("staged update receipt=%+v err=%v", descriptor.PendingTransition, err)
	}
	if snapshot, ensureErr := fixture.Service.Ensure(ctx); !errors.Is(ensureErr, ErrCodexServiceDrift) || snapshot.State == CodexServiceReady {
		t.Fatalf("staged update Ensure accepted A as B: snapshot=%+v err=%v", snapshot, ensureErr)
	}

	restarted, err := fixture.Service.Restart(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.State != CodexServiceReady || restarted.MainPID <= 0 || !validInvocationID(restarted.InvocationID) || restarted.InvocationID == ready.InvocationID {
		t.Fatalf("Restart snapshot=%+v before=%+v", restarted, ready)
	}
	descriptor, err = readOwnedCodexService(fixture.Paths)
	if err != nil || descriptor.PendingTransition != nil {
		t.Fatalf("confirmed staged restart receipt=%+v err=%v", descriptor.PendingTransition, err)
	}
	inspected, err = fixture.Service.Inspect(ctx)
	if err != nil || inspected != restarted {
		t.Fatalf("staged restart Inspect snapshot=%+v err=%v, want %+v", inspected, err, restarted)
	}
	removed, err := fixture.Service.Uninstall(ctx, true)
	if err != nil {
		showResult, showErr := (command.ExecRunner{}).Run(ctx, "systemctl", codexSystemdShowArgs(fixture.Paths.unitName()))
		show, parseErr := parseCodexSystemdShow(showResult.Stdout, showResult.StdoutTruncated || showResult.StderrTruncated)
		enabledResult, enabledErr := (command.ExecRunner{}).Run(ctx, "systemctl", []string{"--user", "is-enabled", fixture.Paths.unitName()})
		t.Fatalf("Uninstall error=%v result=%+v showErr=%v parseErr=%v load=%q unitFileState=%q active=%q/%q enabled=%q enabledErr=%v", err, removed, showErr, parseErr, show.LoadState, show.UnitFileState, show.ActiveState, show.SubState, strings.TrimSpace(string(enabledResult.Stdout)), enabledErr)
	}
	if !removed.RemovedUnit || !removed.RemovedDescriptor || removed.AlreadyAbsent {
		t.Fatalf("Uninstall result=%+v", removed)
	}
	if _, err := os.Lstat(fixture.Paths.UnitPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit remains after uninstall: %v", err)
	}
	if _, err := os.Lstat(fixture.Paths.DescriptorPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descriptor remains after uninstall: %v", err)
	}
	if info, err := os.Stat(fixture.Home); err != nil || !info.IsDir() {
		t.Fatalf("uninstall changed temporary CODEX_HOME: info=%v err=%v", info, err)
	}
	t.Logf("isolated lifecycle fixture unit=%s socket=%s initial=%d/%s restarted=%d/%s removed=%t/%t", fixture.Paths.unitName(), fixture.Paths.SocketPath, ready.MainPID, ready.InvocationID, restarted.MainPID, restarted.InvocationID, removed.RemovedUnit, removed.RemovedDescriptor)
}

func liveFixtureMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode()
}

func parseStoppedFixtureShow(raw []byte, truncated bool) (string, string, int, error) {
	if truncated {
		return "", "", 0, errors.New("truncated systemctl stop observation")
	}
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return "", "", 0, errors.New("malformed systemctl stop observation")
		}
		values[key] = value
	}
	pid, err := strconv.Atoi(values["MainPID"])
	if err != nil || values["ActiveState"] == "" || values["SubState"] == "" {
		return "", "", 0, errors.New("incomplete systemctl stop observation")
	}
	return values["ActiveState"], values["SubState"], pid, nil
}

func waitForLiveServiceSocket(t *testing.T, ctx context.Context, socket string) {
	t.Helper()
	if err := awaitLiveServiceSocket(ctx, socket); err != nil {
		t.Fatal(err)
	}
}

func awaitLiveServiceSocket(ctx context.Context, socket string) error {
	for {
		info, err := os.Lstat(socket)
		if err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf("service socket %q was not ready: %w", socket, ctx.Err())
		case <-timer.C:
		}
	}
}
