package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/agenthost"
)

func TestMainReportsErrorAndExits(t *testing.T) {
	const childEnv = "HERDR_PLUGIN_KIT_TEST_MAIN_ERROR"
	if os.Getenv(childEnv) == "1" {
		os.Args = []string{os.Args[0]}
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMainReportsErrorAndExits$")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	output, err := cmd.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
		t.Fatalf("main exit = %v, output = %q", err, output)
	}
	if !strings.Contains(string(output), "usage: herdr-plugin-kit new|validate|codex-service") {
		t.Fatalf("main output = %q", output)
	}
}

type fakeCodexServiceCLI struct {
	installOptions   agenthost.CodexServiceInstallOptions
	installResult    agenthost.CodexServiceInstallResult
	installErr       error
	snapshot         agenthost.CodexServiceSnapshot
	statusErr        error
	startErr         error
	restartConfirm   bool
	restartErr       error
	uninstallConfirm bool
	uninstallResult  agenthost.CodexServiceUninstallResult
	uninstallErr     error
	calls            []string
}

func (f *fakeCodexServiceCLI) Install(_ context.Context, options agenthost.CodexServiceInstallOptions) (agenthost.CodexServiceInstallResult, error) {
	f.calls = append(f.calls, "install")
	f.installOptions = options
	return f.installResult, f.installErr
}

func (f *fakeCodexServiceCLI) Inspect(context.Context) (agenthost.CodexServiceSnapshot, error) {
	f.calls = append(f.calls, "status")
	return f.snapshot, f.statusErr
}

func (f *fakeCodexServiceCLI) Start(context.Context) (agenthost.CodexServiceSnapshot, error) {
	f.calls = append(f.calls, "start")
	return f.snapshot, f.startErr
}

func (f *fakeCodexServiceCLI) Restart(_ context.Context, confirm bool) (agenthost.CodexServiceSnapshot, error) {
	f.calls = append(f.calls, "restart")
	f.restartConfirm = confirm
	return f.snapshot, f.restartErr
}

func (f *fakeCodexServiceCLI) Uninstall(_ context.Context, confirm bool) (agenthost.CodexServiceUninstallResult, error) {
	f.calls = append(f.calls, "uninstall")
	f.uninstallConfirm = confirm
	return f.uninstallResult, f.uninstallErr
}

func useFakeCodexServiceCLI(t *testing.T, fake *fakeCodexServiceCLI) {
	t.Helper()
	old := newCodexServiceCLI
	newCodexServiceCLI = func() codexServiceCLI { return fake }
	t.Cleanup(func() { newCodexServiceCLI = old })
}

func TestRunCodexServiceInstallForwardsAbsoluteOptions(t *testing.T) {
	fake := &fakeCodexServiceCLI{installResult: agenthost.CodexServiceInstallResult{UnitPath: "/config/systemd/user/herdr-plugin-kit-codex.service", DescriptorPath: "/config/herdr-plugin-kit/codex-service.json", Changed: true}}
	useFakeCodexServiceCLI(t, fake)
	var stdout, stderr bytes.Buffer
	err := run([]string{"codex-service", "install", "--codex", "/opt/codex", "--codex-home", "/opt/home"}, &stdout, &stderr)
	if err != nil || fake.installOptions != (agenthost.CodexServiceInstallOptions{Executable: "/opt/codex", CodexHome: "/opt/home"}) {
		t.Fatalf("err=%v options=%+v", err, fake.installOptions)
	}
	if !strings.Contains(stdout.String(), "installed /config/systemd/user/herdr-plugin-kit-codex.service") || !strings.Contains(stdout.String(), "codex-service start") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestRunCodexServiceInstallReportsOutstandingRestartWhenFilesAreUnchanged(t *testing.T) {
	fake := &fakeCodexServiceCLI{installResult: agenthost.CodexServiceInstallResult{
		UnitPath:        "/config/systemd/user/herdr-plugin-kit-codex.service",
		DescriptorPath:  "/config/herdr-plugin-kit/codex-service.json",
		RestartRequired: true,
	}}
	useFakeCodexServiceCLI(t, fake)
	var stdout, stderr bytes.Buffer
	if err := run([]string{"codex-service", "install"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "installation unchanged") || !strings.Contains(stdout.String(), "staged configuration remains pending") || !strings.Contains(stdout.String(), "restart --confirm-disconnect") || strings.Contains(stdout.String(), "codex-service start") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestRunCodexServiceStatusAndStart(t *testing.T) {
	fake := &fakeCodexServiceCLI{snapshot: agenthost.CodexServiceSnapshot{State: agenthost.CodexServiceReady, SocketPath: "/home/example/.codex/app-server-control/app-server-control.sock", MainPID: 42, InvocationID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	useFakeCodexServiceCLI(t, fake)
	var stdout, stderr bytes.Buffer
	if err := run([]string{"codex-service", "status"}, &stdout, &stderr); err != nil || !strings.Contains(stdout.String(), "state=ready") {
		t.Fatalf("status stdout=%q err=%v", stdout.String(), err)
	}
	stdout.Reset()
	if err := run([]string{"codex-service", "start"}, &stdout, &stderr); err != nil || !strings.Contains(stdout.String(), "started shared Codex service") {
		t.Fatalf("start stdout=%q err=%v", stdout.String(), err)
	}
	if strings.Join(fake.calls, ",") != "status,start" {
		t.Fatalf("calls=%q", fake.calls)
	}
}

func TestRunCodexServiceMaintenanceRequiresConfirmation(t *testing.T) {
	fake := &fakeCodexServiceCLI{snapshot: agenthost.CodexServiceSnapshot{MainPID: 42, InvocationID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, uninstallResult: agenthost.CodexServiceUninstallResult{UnitPath: "/unit", DescriptorPath: "/descriptor", RemovedUnit: true, RemovedDescriptor: true, Retained: []string{"CODEX_HOME data"}}}
	useFakeCodexServiceCLI(t, fake)
	var stdout, stderr bytes.Buffer
	if err := run([]string{"codex-service", "restart"}, &stdout, &stderr); err == nil || len(fake.calls) != 0 || !strings.Contains(err.Error(), "--confirm-disconnect") {
		t.Fatalf("missing restart confirmation err=%v calls=%q", err, fake.calls)
	}
	if err := run([]string{"codex-service", "restart", "--confirm-disconnect"}, &stdout, &stderr); err != nil || !fake.restartConfirm || !strings.Contains(stderr.String(), "managed clients will disconnect") {
		t.Fatalf("restart err=%v confirm=%v stderr=%q", err, fake.restartConfirm, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if err := run([]string{"codex-service", "uninstall", "--confirm-disconnect"}, &stdout, &stderr); err != nil || !fake.uninstallConfirm {
		t.Fatalf("uninstall err=%v confirm=%v", err, fake.uninstallConfirm)
	}
	if !strings.Contains(stdout.String(), "removed /unit") || !strings.Contains(stdout.String(), "retained: CODEX_HOME data") || !strings.Contains(stderr.String(), "managed clients will disconnect") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunGeneratesThenValidates(t *testing.T) {
	output := filepath.Join(t.TempDir(), "plugin")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"new", "--id", "example.cli", "--name", "CLI", "--output", output}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "generated ") || stderr.Len() != 0 {
		t.Fatalf("new output = %q, stderr = %q", stdout.String(), stderr.String())
	}
	stdout.Reset()
	if err := run([]string{"validate", output}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "valid ") {
		t.Fatalf("validate output = %q", stdout.String())
	}
}

func TestRunRejectsUnknownAndMalformedCommands(t *testing.T) {
	for name, args := range map[string][]string{
		"empty": {}, "unknown": {"unknown"}, "new positional": {"new", "extra"},
		"validate missing": {"validate"}, "validate extra": {"validate", "one", "two"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
				t.Fatal("malformed command succeeded")
			}
		})
	}
}
