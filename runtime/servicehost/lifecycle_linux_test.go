//go:build linux

package servicehost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

func TestLifecycleInstallIsOwnedAndIdempotent(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	result, err := lifecycle.Install(context.Background(), request)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !result.Changed || result.Version != request.Version {
		t.Fatalf("unexpected install result: %+v", result)
	}
	assertMode(t, result.UnitPath, 0o600)
	assertMode(t, result.DescriptorPath, 0o600)
	assertMode(t, result.Executable, 0o700)
	wantExecutable, err := os.ReadFile(request.SourceExecutable)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	gotExecutable, err := os.ReadFile(result.Executable)
	if err != nil {
		t.Fatalf("read installed executable: %v", err)
	}
	if !bytes.Equal(gotExecutable, wantExecutable) {
		t.Fatal("installed executable differs from source")
	}
	unit, err := os.ReadFile(result.UnitPath)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	for _, fragment := range []string{
		`RuntimeDirectory="` + lifecycle.config.Paths.RuntimeDirectoryName + `"`,
		`ExecStart="` + result.Executable + `" "serve" "--socket" "` + lifecycle.config.Paths.SocketPath + `"`,
		"Restart=on-failure",
		"TimeoutStopSec=70s",
	} {
		if !bytes.Contains(unit, []byte(fragment)) {
			t.Fatalf("unit missing %q:\n%s", fragment, unit)
		}
	}
	if calls := runner.snapshot(); len(calls) != 1 || calls[0] != "systemctl\x00--user\x00daemon-reload" {
		t.Fatalf("install commands = %q", calls)
	}

	again, err := lifecycle.Install(context.Background(), request)
	if err != nil {
		t.Fatalf("idempotent install: %v", err)
	}
	if again.Changed {
		t.Fatalf("idempotent install reported a change: %+v", again)
	}
	if calls := runner.snapshot(); len(calls) != 2 {
		t.Fatalf("idempotent install commands = %q", calls)
	}
}

func TestLifecycleInstallRequiresConsentWithoutMutation(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	request.Consent = false
	_, err := lifecycle.Install(context.Background(), request)
	if !errors.Is(err, ErrConsentRequired) {
		t.Fatalf("install error = %v, want ErrConsentRequired", err)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("commands without consent = %q", calls)
	}
	for _, path := range []string{lifecycle.config.Paths.UnitPath, lifecycle.config.Paths.DescriptorPath, lifecycle.config.Paths.BinaryDirectory} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("path mutated without consent %s: %v", path, err)
		}
	}
}

func TestLifecycleInstallRefusesUnownedConflict(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if err := os.MkdirAll(filepath.Dir(lifecycle.config.Paths.UnitPath), 0o700); err != nil {
		t.Fatalf("create unit directory: %v", err)
	}
	foreign := []byte("foreign unit\n")
	if err := os.WriteFile(lifecycle.config.Paths.UnitPath, foreign, 0o600); err != nil {
		t.Fatalf("write foreign unit: %v", err)
	}
	_, err := lifecycle.Install(context.Background(), request)
	if !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("install error = %v, want ErrLifecycleOwnership", err)
	}
	got, readErr := os.ReadFile(lifecycle.config.Paths.UnitPath)
	if readErr != nil || !bytes.Equal(got, foreign) {
		t.Fatalf("foreign unit changed: %q, %v", got, readErr)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("commands after conflict = %q", calls)
	}
}

func TestLifecycleInstallRejectsSymlinkExecutable(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	link := request.SourceExecutable + ".link"
	if err := os.Symlink(request.SourceExecutable, link); err != nil {
		t.Fatalf("create executable symlink: %v", err)
	}
	request.SourceExecutable = link
	if _, err := lifecycle.Install(context.Background(), request); err == nil {
		t.Fatal("symlink executable was accepted")
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("commands after symlink rejection = %q", calls)
	}
}

func TestLifecycleInstallRejectsChangedDescriptorMetadata(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	result, err := lifecycle.Install(context.Background(), request)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := os.Chmod(result.DescriptorPath, 0o640); err != nil {
		t.Fatalf("chmod descriptor: %v", err)
	}
	if _, err := lifecycle.Install(context.Background(), request); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("reinstall error = %v, want ErrLifecycleOwnership", err)
	}
	if calls := runner.snapshot(); len(calls) != 1 {
		t.Fatalf("commands after descriptor drift = %q", calls)
	}
}

func TestLifecycleInstallRejectsPrivateDirectoryModeDriftWithoutRepair(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	directory := filepath.Dir(lifecycle.config.Paths.DescriptorPath)
	if err := os.Chmod(directory, 0o750); err != nil {
		t.Fatalf("chmod descriptor directory: %v", err)
	}
	if _, err := lifecycle.Install(context.Background(), request); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("reinstall error = %v, want ErrLifecycleOwnership", err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("stat descriptor directory: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Fatalf("directory drift was silently repaired to %04o", got)
	}
	if calls := runner.snapshot(); len(calls) != 1 {
		t.Fatalf("commands after directory drift = %q", calls)
	}
}

func TestLifecycleInstallRollsBackFilesWhenReloadFails(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	runner.setError(errors.New("manager unavailable"))
	result, err := lifecycle.Install(context.Background(), request)
	if err == nil {
		t.Fatal("install unexpectedly succeeded")
	}
	for _, path := range []string{result.UnitPath, result.DescriptorPath, result.Executable} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("failed install retained %s: %v", path, statErr)
		}
	}
	runner.setError(nil)
	result, err = lifecycle.Install(context.Background(), request)
	if err != nil || !result.Changed {
		t.Fatalf("retry install = %+v, %v", result, err)
	}
}

func TestLifecycleInstallSerializesConcurrentMutation(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	runner.started = make(chan struct{})
	runner.release = make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := lifecycle.Install(context.Background(), request)
		firstDone <- err
	}()
	select {
	case <-runner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first install did not reach daemon reload")
	}
	if _, err := lifecycle.Install(context.Background(), request); !errors.Is(err, ErrLifecycleBusy) {
		t.Fatalf("concurrent install error = %v, want ErrLifecycleBusy", err)
	}
	close(runner.release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first install: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first install did not finish")
	}
}

func TestLifecycleStageUpgradeIsNondisruptiveAndIdempotent(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	installed, err := lifecycle.Install(context.Background(), request)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	unitBefore, err := os.ReadFile(installed.UnitPath)
	if err != nil {
		t.Fatalf("read installed unit: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate-v2")
	staged, err := lifecycle.StageUpgrade(context.Background(), upgrade)
	if err != nil {
		t.Fatalf("stage upgrade: %v", err)
	}
	if !staged.Changed || staged.CurrentVersion != request.Version || staged.PendingVersion != upgrade.Version {
		t.Fatalf("unexpected staged result: %+v", staged)
	}
	unitAfter, err := os.ReadFile(installed.UnitPath)
	if err != nil || !bytes.Equal(unitAfter, unitBefore) {
		t.Fatalf("staging changed active unit: equal=%v err=%v", bytes.Equal(unitAfter, unitBefore), err)
	}
	if calls := runner.snapshot(); len(calls) != 1 || !strings.Contains(calls[0], "daemon-reload") {
		t.Fatalf("staging mutated manager: %q", calls)
	}
	descriptor, exists, err := lifecycle.readDescriptor()
	if err != nil || !exists || descriptor.Pending == nil || descriptor.Pending.Version != upgrade.Version {
		t.Fatalf("pending descriptor = %+v, exists=%v err=%v", descriptor.Pending, exists, err)
	}
	if err := verifyExecutable(descriptor.Pending.Executable, descriptor.Pending.ExecutableSHA256); err != nil {
		t.Fatalf("verify staged executable: %v", err)
	}

	repeated, err := lifecycle.StageUpgrade(context.Background(), upgrade)
	if err != nil {
		t.Fatalf("repeat stage: %v", err)
	}
	if repeated.Changed {
		t.Fatalf("repeat stage changed state: %+v", repeated)
	}
}

func TestLifecycleStageUpgradeRefusesDifferentPendingCandidate(t *testing.T) {
	lifecycle, _, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := lifecycle.StageUpgrade(context.Background(), upgradeRequest(t, request, "2.0.0", "candidate-v2")); err != nil {
		t.Fatalf("stage first candidate: %v", err)
	}
	other := upgradeRequest(t, request, "3.0.0", "candidate-v3")
	if _, err := lifecycle.StageUpgrade(context.Background(), other); !errors.Is(err, ErrUpgradeRequired) {
		t.Fatalf("stage different candidate error = %v, want ErrUpgradeRequired", err)
	}
	if _, err := os.Lstat(filepath.Join(lifecycle.config.Paths.BinaryDirectory, other.Version)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("different candidate mutated binary inventory: %v", err)
	}
}

func TestLifecycleStageUpgradeRejectsConflictingCurrentVersion(t *testing.T) {
	lifecycle, _, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	conflict := request
	conflict.Arguments = append(append([]string(nil), request.Arguments...), "--changed")
	if _, err := lifecycle.StageUpgrade(context.Background(), conflict); !errors.Is(err, ErrUpgradeRequired) {
		t.Fatalf("stage same-version conflict error = %v, want ErrUpgradeRequired", err)
	}
	descriptor, exists, err := lifecycle.readDescriptor()
	if err != nil || !exists || descriptor.Pending != nil {
		t.Fatalf("same-version conflict changed descriptor: pending=%+v exists=%v err=%v", descriptor.Pending, exists, err)
	}
	if _, err := lifecycle.verifyInstallation(); err != nil {
		t.Fatalf("same-version conflict invalidated installation: %v", err)
	}
}

func TestLifecycleStageUpgradeRejectsNonPrivateCandidateDirectory(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate-v2")
	versionDirectory := filepath.Join(lifecycle.config.Paths.BinaryDirectory, upgrade.Version)
	if err := os.Mkdir(versionDirectory, 0o750); err != nil {
		t.Fatalf("create non-private version directory: %v", err)
	}
	contents, err := os.ReadFile(upgrade.SourceExecutable)
	if err != nil {
		t.Fatalf("read upgrade source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(versionDirectory, "service"), contents, 0o700); err != nil {
		t.Fatalf("write preexisting candidate: %v", err)
	}
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("stage non-private candidate error = %v, want ErrLifecycleOwnership", err)
	}
	descriptor, exists, err := lifecycle.readDescriptor()
	if err != nil || !exists || descriptor.Pending != nil {
		t.Fatalf("non-private candidate changed descriptor: pending=%+v exists=%v err=%v", descriptor.Pending, exists, err)
	}
	if calls := runner.snapshot(); len(calls) != 1 {
		t.Fatalf("non-private candidate mutated manager: %q", calls)
	}
}

func TestLifecycleStageUpgradeRequiresConsentWithoutMutation(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate-v2")
	upgrade.Consent = false
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); !errors.Is(err, ErrConsentRequired) {
		t.Fatalf("stage error = %v, want ErrConsentRequired", err)
	}
	if calls := runner.snapshot(); len(calls) != 1 {
		t.Fatalf("stage without consent mutated manager: %q", calls)
	}
	if _, err := os.Lstat(filepath.Join(lifecycle.config.Paths.BinaryDirectory, upgrade.Version)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage without consent mutated binary inventory: %v", err)
	}
}

func TestLifecycleActivateUpgradeRequiresConfirmationWithoutMutation(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate-v2")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatalf("stage upgrade: %v", err)
	}
	if _, err := lifecycle.ActivateUpgrade(context.Background(), UpgradeActivationRequest{}); !errors.Is(err, ErrConsentRequired) {
		t.Fatalf("activate error = %v, want ErrConsentRequired", err)
	}
	descriptor, exists, err := lifecycle.readDescriptor()
	if err != nil || !exists || descriptor.Pending == nil || descriptor.Pending.Version != upgrade.Version || descriptor.Transition != nil {
		t.Fatalf("unconfirmed activation changed descriptor: %+v exists=%v err=%v", descriptor, exists, err)
	}
	if calls := runner.snapshot(); len(calls) != 1 {
		t.Fatalf("unconfirmed activation mutated manager: %q", calls)
	}
}

func TestLifecycleActivateUpgradeDrainsStartsAndFinalizes(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate-v2")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatalf("stage upgrade: %v", err)
	}
	handler := &testMaintenanceHandler{}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, handler)
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	beforePID := managed.pid
	managed.nextVersion = upgrade.Version
	runner.setHook(managed.command)
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", beforePID))
	checkerCalls := 0
	result, err := lifecycle.ActivateUpgrade(context.Background(), UpgradeActivationRequest{
		Confirm: true,
		RollbackCheck: func(context.Context, UpgradeRelease, UpgradeRelease) (bool, error) {
			checkerCalls++
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("activate upgrade: %v", err)
	}
	if result.CurrentVersion != upgrade.Version || result.PreviousVersion != request.Version || result.RolledBack || result.RecoveryRequired || !result.Status.Active || result.Status.MainPID == beforePID {
		t.Fatalf("unexpected activation result: %+v", result)
	}
	if checkerCalls != 0 {
		t.Fatalf("successful activation ran rollback checker %d times", checkerCalls)
	}
	if got := handler.counts(); got != [2]int{1, 0} {
		t.Fatalf("maintenance counts = %v", got)
	}
	descriptor, exists, err := lifecycle.readDescriptor()
	if err != nil || !exists || descriptor.Version != upgrade.Version || descriptor.Previous == nil || descriptor.Previous.Version != request.Version || descriptor.Pending != nil || descriptor.Transition != nil {
		t.Fatalf("final descriptor = %+v exists=%v err=%v", descriptor, exists, err)
	}
	unit, err := os.ReadFile(lifecycle.config.Paths.UnitPath)
	if err != nil || !bytes.Contains(unit, []byte(descriptor.Executable)) {
		t.Fatalf("activated unit does not use candidate: %s err=%v", unit, err)
	}
}

func TestLifecycleActivateUpgradeSupportsSharedUnitDirectory(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := os.Chmod(filepath.Dir(lifecycle.config.Paths.UnitPath), 0o755); err != nil {
		t.Fatalf("chmod unit directory: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate-v2")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatalf("stage upgrade: %v", err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	managed.nextVersion = upgrade.Version
	runner.setHook(managed.command)
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	result, err := lifecycle.ActivateUpgrade(context.Background(), UpgradeActivationRequest{
		Confirm: true, RollbackCheck: func(context.Context, UpgradeRelease, UpgradeRelease) (bool, error) { return true, nil },
	})
	if err != nil || result.CurrentVersion != upgrade.Version || !result.Status.Active {
		t.Fatalf("activate from shared unit directory = %+v, %v", result, err)
	}
}

func TestLifecycleActivateUpgradeCleansUpAfterCallerCancellation(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate-v2")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatalf("stage upgrade: %v", err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	managed.nextVersion = upgrade.Version
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.setHook(func(executable string, arguments []string) error {
		err := managed.command(executable, arguments)
		if len(arguments) > 1 && arguments[1] == "start" && managed.version == upgrade.Version {
			cancel()
		}
		return err
	})
	result, err := lifecycle.ActivateUpgrade(ctx, UpgradeActivationRequest{
		Confirm: true,
		RollbackCheck: func(context.Context, UpgradeRelease, UpgradeRelease) (bool, error) {
			managed.nextVersion = request.Version
			managed.initialState = HealthReady
			managed.initialAccepting = true
			return true, nil
		},
	})
	if !errors.Is(err, ErrUpgradeRolledBack) || !result.RolledBack || result.RecoveryRequired || managed.cancel == nil {
		t.Fatalf("cancellation cleanup result = %+v err=%v running=%t", result, err, managed.cancel != nil)
	}
}

func TestLifecycleActivateUpgradeRestoresPreviousWhenCanceledAfterStop(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate-v2")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatalf("stage upgrade: %v", err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.setHook(func(executable string, arguments []string) error {
		err := managed.command(executable, arguments)
		if len(arguments) > 1 && arguments[1] == "stop" {
			cancel()
		}
		return err
	})
	result, err := lifecycle.ActivateUpgrade(ctx, UpgradeActivationRequest{
		Confirm: true, RollbackCheck: func(context.Context, UpgradeRelease, UpgradeRelease) (bool, error) { return true, nil },
	})
	if !errors.Is(err, context.Canceled) || result.RecoveryRequired || result.RolledBack || !result.Status.Active || managed.cancel == nil {
		t.Fatalf("post-stop cancellation result = %+v err=%v running=%t", result, err, managed.cancel != nil)
	}
	descriptor, exists, readErr := lifecycle.readDescriptor()
	if readErr != nil || !exists || descriptor.Version != request.Version || descriptor.Transition != nil {
		t.Fatalf("post-stop cancellation descriptor = %+v exists=%v err=%v", descriptor, exists, readErr)
	}
	assertManagedServiceReady(t, lifecycle)
}

func TestLifecycleActivateUpgradeRollsBackCompatibleReadinessFailure(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	lifecycle.config.ReadinessTimeout = 2 * time.Second
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate-v2")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatalf("stage upgrade: %v", err)
	}
	handler := &testMaintenanceHandler{}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, handler)
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	managed.nextVersion = "wrong-candidate-version"
	runner.setHook(managed.command)
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	checkerCalls := 0
	result, err := lifecycle.ActivateUpgrade(context.Background(), UpgradeActivationRequest{
		Confirm: true,
		RollbackCheck: func(_ context.Context, previous, candidate UpgradeRelease) (bool, error) {
			checkerCalls++
			if managed.cancel != nil {
				t.Fatal("rollback checker ran while candidate was still active")
			}
			if previous.Version != request.Version || candidate.Version != upgrade.Version {
				t.Fatalf("rollback releases = %+v -> %+v", previous, candidate)
			}
			managed.nextVersion = request.Version
			managed.initialState = HealthReady
			managed.initialAccepting = true
			return true, nil
		},
	})
	if !errors.Is(err, ErrUpgradeRolledBack) {
		t.Fatalf("activate error = %v, want ErrUpgradeRolledBack", err)
	}
	if !result.RolledBack || result.RecoveryRequired || result.CurrentVersion != request.Version || !result.Status.Active {
		t.Fatalf("rollback result = %+v", result)
	}
	if checkerCalls != 1 {
		t.Fatalf("rollback checker calls = %d", checkerCalls)
	}
	descriptor, exists, readErr := lifecycle.readDescriptor()
	if readErr != nil || !exists || descriptor.Version != request.Version || descriptor.Pending == nil || descriptor.Pending.Version != upgrade.Version || descriptor.Transition != nil {
		t.Fatalf("rollback descriptor = %+v exists=%v err=%v", descriptor, exists, readErr)
	}
	assertManagedServiceReady(t, lifecycle)
}

func TestLifecycleActivateUpgradeLeavesIncompatibleStateStopped(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	lifecycle.config.ReadinessTimeout = 250 * time.Millisecond
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate-v2")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatalf("stage upgrade: %v", err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	managed.nextVersion = "wrong-candidate-version"
	runner.setHook(managed.command)
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	checkerCalls := 0
	result, err := lifecycle.ActivateUpgrade(context.Background(), UpgradeActivationRequest{
		Confirm: true,
		RollbackCheck: func(context.Context, UpgradeRelease, UpgradeRelease) (bool, error) {
			checkerCalls++
			if managed.cancel != nil {
				t.Fatal("compatibility checker ran while candidate was active")
			}
			return false, nil
		},
	})
	if !errors.Is(err, ErrUpgradeRecoveryRequired) {
		t.Fatalf("activate error = %v, want ErrUpgradeRecoveryRequired", err)
	}
	if !result.RecoveryRequired || result.RolledBack || checkerCalls != 1 {
		t.Fatalf("incompatible result = %+v checkerCalls=%d", result, checkerCalls)
	}
	descriptor, exists, readErr := lifecycle.readDescriptor()
	if readErr != nil || !exists || descriptor.Transition == nil || !descriptor.Transition.RecoveryRequired {
		t.Fatalf("recovery descriptor = %+v exists=%v err=%v", descriptor, exists, readErr)
	}
	if managed.cancel != nil {
		t.Fatal("incompatible activation left a service running")
	}
	if _, err := lifecycle.Status(context.Background()); !errors.Is(err, ErrUpgradeRecoveryRequired) {
		t.Fatalf("status error = %v, want ErrUpgradeRecoveryRequired", err)
	}
}

func TestLifecycleActivateUpgradeStopsFailedRollback(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	lifecycle.config.ReadinessTimeout = 150 * time.Millisecond
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate-v2")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatalf("stage upgrade: %v", err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	managed.nextVersion = "wrong-candidate-version"
	runner.setHook(managed.command)
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	result, err := lifecycle.ActivateUpgrade(context.Background(), UpgradeActivationRequest{
		Confirm: true,
		RollbackCheck: func(context.Context, UpgradeRelease, UpgradeRelease) (bool, error) {
			if managed.cancel != nil {
				t.Fatal("rollback checker ran while candidate was active")
			}
			managed.nextVersion = "wrong-rollback-version"
			return true, nil
		},
	})
	if !errors.Is(err, ErrUpgradeRecoveryRequired) {
		t.Fatalf("activate error = %v, want ErrUpgradeRecoveryRequired", err)
	}
	if !result.RecoveryRequired || result.RolledBack {
		t.Fatalf("failed rollback result = %+v", result)
	}
	if managed.cancel != nil {
		t.Fatal("failed rollback left the old process running")
	}
	descriptor, exists, readErr := lifecycle.readDescriptor()
	if readErr != nil || !exists || descriptor.Transition == nil || !descriptor.Transition.RecoveryRequired {
		t.Fatalf("failed rollback descriptor = %+v exists=%v err=%v", descriptor, exists, readErr)
	}
}

func TestLifecycleActivateUpgradeStopsRollbackAfterCleanupDeadline(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	lifecycle.upgradeCleanupTimeout = func() time.Duration { return 50 * time.Millisecond }
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate-v2")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatalf("stage upgrade: %v", err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	managed.nextVersion = "wrong-candidate-version"
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	runner.setHook(func(executable string, arguments []string) error {
		if len(arguments) > 1 && arguments[1] == "start" && managed.nextVersion == request.Version {
			timer := time.NewTimer(100 * time.Millisecond)
			<-timer.C
		}
		return managed.command(executable, arguments)
	})
	result, err := lifecycle.ActivateUpgrade(context.Background(), UpgradeActivationRequest{
		Confirm: true,
		RollbackCheck: func(context.Context, UpgradeRelease, UpgradeRelease) (bool, error) {
			managed.nextVersion = request.Version
			return true, nil
		},
	})
	if !errors.Is(err, ErrUpgradeRecoveryRequired) || !result.RecoveryRequired {
		t.Fatalf("expired cleanup result = %+v err=%v", result, err)
	}
	if managed.cancel != nil {
		t.Fatal("expired rollback cleanup left old process running")
	}
}

func TestLifecycleActivateUpgradeReportsPostSwitchRecovery(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate-v2")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatalf("stage upgrade: %v", err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	runner.setHook(func(executable string, arguments []string) error {
		if len(arguments) > 1 && arguments[1] == "daemon-reload" {
			return errors.New("reload failed")
		}
		return managed.command(executable, arguments)
	})
	result, err := lifecycle.ActivateUpgrade(context.Background(), UpgradeActivationRequest{
		Confirm: true, RollbackCheck: func(context.Context, UpgradeRelease, UpgradeRelease) (bool, error) { return true, nil },
	})
	if !errors.Is(err, ErrUpgradeRecoveryRequired) || !result.RecoveryRequired {
		t.Fatalf("post-switch failure result = %+v err=%v", result, err)
	}
	if managed.cancel != nil {
		t.Fatal("post-switch reload failure left previous service running")
	}
}

func TestLifecycleInstallRejectsUnfinishedUpgradeTransition(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate-v2")
	if _, err := lifecycle.StageUpgrade(context.Background(), upgrade); err != nil {
		t.Fatalf("stage upgrade: %v", err)
	}
	descriptor, exists, err := lifecycle.readDescriptor()
	if err != nil || !exists || descriptor.Pending == nil {
		t.Fatalf("read staged descriptor: exists=%v err=%v", exists, err)
	}
	transition := descriptor
	transition.Transition = &lifecycleTransition{
		Phase: upgradePreviousStopped, Previous: releaseFromDescriptor(descriptor), Candidate: *cloneLifecycleRelease(*descriptor.Pending),
	}
	if err := lifecycle.replaceDescriptor(descriptor, transition); err != nil {
		t.Fatalf("write transition fixture: %v", err)
	}
	if _, err := lifecycle.Install(context.Background(), request); !errors.Is(err, ErrUpgradeRecoveryRequired) {
		t.Fatalf("install error = %v, want ErrUpgradeRecoveryRequired", err)
	}
	if calls := runner.snapshot(); len(calls) != 1 {
		t.Fatalf("install during transition mutated manager: %q", calls)
	}
}

func TestLifecycleUninstallDrainsAndRemovesOnlyServiceInventory(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	installed, err := lifecycle.Install(context.Background(), request)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	upgrade := upgradeRequest(t, request, "2.0.0", "candidate-v2")
	staged, err := lifecycle.StageUpgrade(context.Background(), upgrade)
	if err != nil {
		t.Fatalf("stage upgrade: %v", err)
	}
	preserved := filepath.Join(filepath.Dir(lifecycle.config.Paths.BinaryDirectory), "preserved-state.json")
	if err := os.WriteFile(preserved, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write preserved state: %v", err)
	}
	handler := &testMaintenanceHandler{}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, handler)
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	runner.setHook(func(executable string, arguments []string) error {
		if err := managed.command(executable, arguments); err != nil {
			return err
		}
		if len(arguments) > 1 && arguments[1] == "daemon-reload" {
			runner.setStdout(lifecycleNotFoundOutput(lifecycle))
		}
		return nil
	})
	result, err := lifecycle.Uninstall(context.Background(), UninstallRequest{Confirm: true})
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !result.RemovedUnit || !result.RemovedDescriptor || len(result.RemovedExecutables) != 2 || result.AlreadyAbsent {
		t.Fatalf("unexpected uninstall result: %+v", result)
	}
	if got := handler.counts(); got != [2]int{1, 0} {
		t.Fatalf("maintenance counts = %v", got)
	}
	for _, path := range []string{installed.UnitPath, installed.DescriptorPath, installed.Executable, staged.Executable, lifecycle.config.Paths.BinaryDirectory} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("uninstall retained service resource %s: %v", path, err)
		}
	}
	if contents, err := os.ReadFile(preserved); err != nil || string(contents) != "keep" {
		t.Fatalf("uninstall changed preserved state: %q, %v", contents, err)
	}
	if _, err := os.Lstat(installed.DescriptorPath + ".lock"); err != nil {
		t.Fatalf("uninstall removed retained lifecycle lock: %v", err)
	}
	repeated, err := lifecycle.Uninstall(context.Background(), UninstallRequest{Confirm: true})
	if err != nil || !repeated.AlreadyAbsent {
		t.Fatalf("repeat uninstall = %+v, %v", repeated, err)
	}
}

func TestLifecycleUninstallRequiresConfirmationWithoutMutation(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	installed, err := lifecycle.Install(context.Background(), request)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := lifecycle.Uninstall(context.Background(), UninstallRequest{}); !errors.Is(err, ErrConsentRequired) {
		t.Fatalf("uninstall error = %v, want ErrConsentRequired", err)
	}
	for _, path := range []string{installed.UnitPath, installed.DescriptorPath, installed.Executable} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("unconfirmed uninstall changed %s: %v", path, err)
		}
	}
	if calls := runner.snapshot(); len(calls) != 1 {
		t.Fatalf("unconfirmed uninstall mutated manager: %q", calls)
	}
}

func TestLifecycleUninstallRetriesAfterReloadFailure(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	installed, err := lifecycle.Install(context.Background(), request)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "disabled", 0))
	failReload := true
	runner.setHook(func(_ string, arguments []string) error {
		if len(arguments) > 1 && arguments[1] == "daemon-reload" {
			if failReload {
				failReload = false
				return errors.New("reload failed")
			}
			runner.setStdout(lifecycleNotFoundOutput(lifecycle))
		}
		return nil
	})
	if _, err := lifecycle.Uninstall(context.Background(), UninstallRequest{Confirm: true}); !errors.Is(err, ErrLifecycleUnavailable) {
		t.Fatalf("first uninstall error = %v, want ErrLifecycleUnavailable", err)
	}
	if _, err := os.Lstat(installed.DescriptorPath); err != nil {
		t.Fatalf("partial uninstall removed retry receipt: %v", err)
	}
	for _, path := range []string{installed.UnitPath, installed.Executable} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial uninstall retained removed resource %s: %v", path, err)
		}
	}
	result, err := lifecycle.Uninstall(context.Background(), UninstallRequest{Confirm: true})
	if err != nil || !result.RemovedDescriptor {
		t.Fatalf("retry uninstall = %+v, %v", result, err)
	}
}

func TestLifecycleUninstallMissingUnitRefusesActiveManager(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	installed, err := lifecycle.Install(context.Background(), request)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", managed.pid))
	if err := os.Remove(installed.UnitPath); err != nil {
		t.Fatalf("remove unit fixture: %v", err)
	}
	if _, err := lifecycle.Uninstall(context.Background(), UninstallRequest{Confirm: true}); err == nil {
		t.Fatal("uninstall accepted active manager with missing unit")
	}
	if _, err := os.Lstat(installed.Executable); err != nil {
		t.Fatalf("unsafe uninstall removed executable: %v", err)
	}
}

func TestLifecycleUninstallRemovesAlreadyEmptyVersionDirectory(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	installed, err := lifecycle.Install(context.Background(), request)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := os.Remove(installed.Executable); err != nil {
		t.Fatalf("remove executable fixture: %v", err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "disabled", 0))
	runner.setHook(func(_ string, arguments []string) error {
		if len(arguments) > 1 && arguments[1] == "daemon-reload" {
			runner.setStdout(lifecycleNotFoundOutput(lifecycle))
		}
		return nil
	})
	result, err := lifecycle.Uninstall(context.Background(), UninstallRequest{Confirm: true})
	if err != nil || !result.RemovedDescriptor {
		t.Fatalf("uninstall empty version directory = %+v, %v", result, err)
	}
	if _, err := os.Lstat(lifecycle.config.Paths.BinaryDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty binary inventory remains: %v", err)
	}
}

func TestRemoveCapturedLifecycleFileRejectsSameByteReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned")
	contents := []byte("same")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write original: %v", err)
	}
	captured, err := inspectCreatedFile(path)
	if err != nil {
		t.Fatalf("capture original: %v", err)
	}
	replacement := filepath.Join(filepath.Dir(path), "replacement")
	if err := os.WriteFile(replacement, contents, 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("replace original: %v", err)
	}
	if err := removeCapturedLifecycleFile(captured, path, contents, 0o600); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("same-byte replacement removal error = %v, want ErrLifecycleOwnership", err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("replacement was removed: %q, %v", got, err)
	}
}

func TestLifecyclePurgeRequiresSeparateRecoveryLossConfirmation(t *testing.T) {
	lifecycle, runner, request, purgePaths := lifecyclePurgeFixture(t)
	installAndUninstallForPurge(t, lifecycle, runner, request)
	createPurgeData(t, purgePaths)
	if _, err := lifecycle.Purge(context.Background(), PurgeRequest{}); !errors.Is(err, ErrConsentRequired) {
		t.Fatalf("unconfirmed purge error = %v, want ErrConsentRequired", err)
	}
	if _, err := lifecycle.Purge(context.Background(), PurgeRequest{Confirm: true}); !errors.Is(err, ErrPurgeRecoveryConsentRequired) {
		t.Fatalf("purge without recovery confirmation error = %v, want ErrPurgeRecoveryConsentRequired", err)
	}
	for _, path := range purgePaths {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("unconfirmed purge changed %s: %v", path, err)
		}
	}
}

func TestLifecyclePurgeRemovesPinnedDataOnly(t *testing.T) {
	lifecycle, runner, request, purgePaths := lifecyclePurgeFixture(t)
	installAndUninstallForPurge(t, lifecycle, runner, request)
	createPurgeData(t, purgePaths)
	unrelated := filepath.Join(filepath.Dir(purgePaths[0]), "unrelated.json")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write unrelated data: %v", err)
	}
	result, err := lifecycle.Purge(context.Background(), PurgeRequest{Confirm: true, ConfirmRecoveryLoss: true})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(result.RemovedPaths) != len(purgePaths) || !result.RemovedManifest || result.AlreadyAbsent {
		t.Fatalf("unexpected purge result: %+v", result)
	}
	for _, path := range purgePaths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("purge retained pinned path %s: %v", path, err)
		}
	}
	if contents, err := os.ReadFile(unrelated); err != nil || string(contents) != "keep" {
		t.Fatalf("purge changed unrelated data: %q, %v", contents, err)
	}
	if _, err := os.Lstat(lifecycle.config.Paths.DescriptorPath + ".lock"); err != nil {
		t.Fatalf("purge removed lifecycle lock: %v", err)
	}
	repeated, err := lifecycle.Purge(context.Background(), PurgeRequest{Confirm: true, ConfirmRecoveryLoss: true})
	if err != nil || !repeated.AlreadyAbsent {
		t.Fatalf("repeat purge = %+v, %v", repeated, err)
	}
}

func TestLifecyclePurgeRejectsChangedPathsAndSymlinksWithoutDeletion(t *testing.T) {
	lifecycle, runner, request, purgePaths := lifecyclePurgeFixture(t)
	installAndUninstallForPurge(t, lifecycle, runner, request)
	createPurgeData(t, purgePaths)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(purgePaths[1], "link")); err != nil {
		t.Fatalf("create purge symlink: %v", err)
	}
	result, err := lifecycle.Purge(context.Background(), PurgeRequest{Confirm: true, ConfirmRecoveryLoss: true})
	if !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("symlink purge error = %v, want ErrLifecycleOwnership", err)
	}
	if len(result.RemovedPaths) != 0 {
		t.Fatalf("rejected purge reported removed paths: %+v", result)
	}
	if _, err := os.Lstat(purgePaths[0]); err != nil {
		t.Fatalf("symlink rejection partially deleted earlier path: %v", err)
	}

	changed := lifecycle.config
	changed.Paths.PurgePaths = []string{filepath.Join(t.TempDir(), "different")}
	changedLifecycle, err := NewLifecycle(changed, runner)
	if err != nil {
		t.Fatalf("new changed lifecycle: %v", err)
	}
	if _, err := changedLifecycle.Purge(context.Background(), PurgeRequest{Confirm: true, ConfirmRecoveryLoss: true}); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("changed-path purge error = %v, want ErrLifecycleOwnership", err)
	}
	if _, err := os.Lstat(purgePaths[0]); err != nil {
		t.Fatalf("changed-path rejection deleted pinned data: %v", err)
	}
}

func TestLifecyclePurgeRejectsIntermediateSymlinkAncestor(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	config := lifecycle.config
	linkParent := filepath.Join(t.TempDir(), "pinned")
	config.Paths.PurgePaths = []string{filepath.Join(linkParent, "link", "state")}
	lifecycle, err := NewLifecycle(config, runner)
	if err != nil {
		t.Fatalf("new purge lifecycle: %v", err)
	}
	installAndUninstallForPurge(t, lifecycle, runner, request)
	outside := t.TempDir()
	targetState := filepath.Join(outside, "state")
	if err := os.Mkdir(targetState, 0o700); err != nil {
		t.Fatalf("create target state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(targetState, "history.json"), []byte("keep"), 0o600); err != nil {
		t.Fatalf("write target state: %v", err)
	}
	if err := os.Mkdir(linkParent, 0o700); err != nil {
		t.Fatalf("create pinned parent: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(linkParent, "link")); err != nil {
		t.Fatalf("create ancestor symlink: %v", err)
	}
	if _, err := lifecycle.Purge(context.Background(), PurgeRequest{Confirm: true, ConfirmRecoveryLoss: true}); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("ancestor-symlink purge error = %v, want ErrLifecycleOwnership", err)
	}
	if _, err := os.Lstat(filepath.Join(targetState, "history.json")); err != nil {
		t.Fatalf("ancestor-symlink purge deleted target data: %v", err)
	}
}

func TestLifecycleInstallAcceptsMatchingRetainedPurgeManifest(t *testing.T) {
	lifecycle, runner, request, _ := lifecyclePurgeFixture(t)
	installAndUninstallForPurge(t, lifecycle, runner, request)
	result, err := lifecycle.Install(context.Background(), request)
	if err != nil || !result.Changed {
		t.Fatalf("reinstall with retained manifest = %+v, %v", result, err)
	}
	expected, err := lifecycle.purgeManifestBytes()
	if err != nil {
		t.Fatalf("marshal purge manifest: %v", err)
	}
	if err := verifyLifecycleFile(lifecycle.purgeManifestPath(), expected, 0o600); err != nil {
		t.Fatalf("reinstall changed retained manifest: %v", err)
	}
}

func TestCollectLifecyclePurgeEntriesCountsParentInLimit(t *testing.T) {
	if validPurgeEntryBudget(0) || !validPurgeEntryBudget(1) {
		t.Fatal("purge entry budget boundary changed")
	}
	if remainingPurgeEntries(0) != maxLifecyclePurgeEntries || remainingPurgeEntries(1) != maxLifecyclePurgeEntries-1 {
		t.Fatal("purge entry remaining budget changed")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod purge root: %v", err)
	}
	for _, name := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if _, _, err := collectLifecyclePurgeEntries(root, uint32(os.Getuid()), 2); err == nil {
		t.Fatal("purge inventory exceeded parent-inclusive entry limit")
	}
}

func lifecyclePurgeFixture(t *testing.T) (*Lifecycle, *lifecycleRunner, InstallRequest, []string) {
	t.Helper()
	lifecycle, runner, request := lifecycleFixture(t)
	config := lifecycle.config
	purgePaths := []string{
		filepath.Join(filepath.Dir(config.Paths.DescriptorPath), "settings.json"),
		filepath.Join(filepath.Dir(config.Paths.BinaryDirectory), "state"),
	}
	config.Paths.PurgePaths = append([]string(nil), purgePaths...)
	lifecycle, err := NewLifecycle(config, runner)
	if err != nil {
		t.Fatalf("new purge lifecycle: %v", err)
	}
	return lifecycle, runner, request, purgePaths
}

func installAndUninstallForPurge(t *testing.T, lifecycle *Lifecycle, runner *lifecycleRunner, request InstallRequest) {
	t.Helper()
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "disabled", 0))
	runner.setHook(func(_ string, arguments []string) error {
		if len(arguments) > 1 && arguments[1] == "daemon-reload" {
			runner.setStdout(lifecycleNotFoundOutput(lifecycle))
		}
		return nil
	})
	if _, err := lifecycle.Uninstall(context.Background(), UninstallRequest{Confirm: true}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
}

func createPurgeData(t *testing.T, paths []string) {
	t.Helper()
	if err := os.WriteFile(paths[0], []byte("settings"), 0o600); err != nil {
		t.Fatalf("write purge settings: %v", err)
	}
	if err := os.Mkdir(paths[1], 0o700); err != nil {
		t.Fatalf("create purge state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(paths[1], "history.json"), []byte("history"), 0o600); err != nil {
		t.Fatalf("write purge state: %v", err)
	}
}

func upgradeRequest(t *testing.T, installed InstallRequest, version, contents string) InstallRequest {
	t.Helper()
	request := installed
	request.Version = version
	request.SourceExecutable = filepath.Join(t.TempDir(), "service")
	if err := os.WriteFile(request.SourceExecutable, []byte(contents), 0o700); err != nil {
		t.Fatalf("write upgrade source: %v", err)
	}
	if err := os.Chmod(request.SourceExecutable, 0o700); err != nil {
		t.Fatalf("chmod upgrade source: %v", err)
	}
	return request
}

func TestLifecycleStatusIsReadOnlyWhenAbsent(t *testing.T) {
	lifecycle, runner, _ := lifecycleFixture(t)
	status, err := lifecycle.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Installed {
		t.Fatalf("absent service reported installed: %+v", status)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("read-only absent status commands = %q", calls)
	}
	for _, path := range []string{lifecycle.config.Paths.DescriptorPath, lifecycle.config.Paths.UnitPath, lifecycle.config.Paths.BinaryDirectory} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("status mutated absent path %s: %v", path, err)
		}
	}
}

func TestLifecycleStatusParsesSupportedSystemdFields(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "enabled", 4321))
	status, err := lifecycle.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !status.Installed || !status.Enabled || !status.Active || status.MainPID != 4321 || status.FragmentPath != lifecycle.config.Paths.UnitPath {
		t.Fatalf("unexpected status: %+v", status)
	}
	calls := runner.snapshot()
	if len(calls) != 2 || !strings.Contains(calls[1], "systemctl\x00--user\x00show\x00test-service.service") {
		t.Fatalf("status commands = %q", calls)
	}
}

func TestLifecycleStatusRejectsLoadedUnitPathDrift(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	raw := lifecycleShowOutput(lifecycle, "active", "running", "enabled", 4321)
	raw = bytes.Replace(raw, []byte(lifecycle.config.Paths.UnitPath), []byte(filepath.Join(t.TempDir(), "foreign.service")), 1)
	runner.setStdout(raw)
	if _, err := lifecycle.Status(context.Background()); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("status error = %v, want ErrLifecycleOwnership", err)
	}
}

func TestLifecycleEnableDisableAreExplicitAndObserved(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "enabled", 0))
	enabled, err := lifecycle.Enable(context.Background())
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !enabled.Enabled || enabled.Active {
		t.Fatalf("unexpected enabled status: %+v", enabled)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "disabled", 0))
	disabled, err := lifecycle.Disable(context.Background())
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if disabled.Enabled || disabled.Active {
		t.Fatalf("unexpected disabled status: %+v", disabled)
	}
	calls := runner.snapshot()
	wantFragments := []string{"daemon-reload", "\x00show\x00", "\x00enable\x00", "\x00show\x00", "\x00show\x00", "\x00disable\x00", "\x00show\x00"}
	if len(calls) != len(wantFragments) {
		t.Fatalf("action commands = %q", calls)
	}
	for index, fragment := range wantFragments {
		if !strings.Contains(calls[index], fragment) {
			t.Fatalf("command %d = %q, want fragment %q", index, calls[index], fragment)
		}
	}
}

func TestLifecycleStartAndDrainedStop(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	handler := &testMaintenanceHandler{}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, handler)
	defer managed.stop()
	runner.setHook(managed.command)
	runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "disabled", 0))

	started, err := lifecycle.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !started.Active || started.MainPID != os.Getpid() {
		t.Fatalf("unexpected started status: %+v", started)
	}
	stopped, err := lifecycle.Stop(context.Background())
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped.Active || stopped.MainPID != 0 {
		t.Fatalf("unexpected stopped status: %+v", stopped)
	}
	if got := handler.counts(); got != [2]int{1, 0} {
		t.Fatalf("maintenance counts = %v", got)
	}
	if _, err := os.Lstat(lifecycle.config.Paths.SocketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stop left socket: %v", err)
	}
	calls := runner.snapshot()
	want := []string{"daemon-reload", "\x00show\x00", "\x00start\x00", "\x00show\x00", "\x00show\x00", "\x00stop\x00", "\x00show\x00"}
	if len(calls) != len(want) {
		t.Fatalf("start/stop commands = %q", calls)
	}
	for index, fragment := range want {
		if !strings.Contains(calls[index], fragment) {
			t.Fatalf("command %d = %q, want %q", index, calls[index], fragment)
		}
	}
}

func TestLifecycleRestartDrainsAndReplacesProcess(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	handler := &testMaintenanceHandler{}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, handler)
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	beforePID := managed.pid
	runner.setHook(managed.command)
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "disabled", beforePID))

	restarted, err := lifecycle.Restart(context.Background())
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if !restarted.Active || restarted.MainPID == beforePID {
		t.Fatalf("restart did not replace active process: before=%d after=%+v", beforePID, restarted)
	}
	if got := handler.counts(); got != [2]int{1, 0} {
		t.Fatalf("maintenance counts = %v", got)
	}
}

func TestLifecycleRestartFailureResumesAdmissions(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	handler := &testMaintenanceHandler{}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, handler)
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	managed.failRestart = true
	runner.setHook(managed.command)
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "disabled", managed.pid))

	if _, err := lifecycle.Restart(context.Background()); !errors.Is(err, ErrLifecycleUnavailable) {
		t.Fatalf("restart error = %v, want ErrLifecycleUnavailable", err)
	}
	if got := handler.counts(); got != [2]int{1, 1} {
		t.Fatalf("maintenance counts = %v", got)
	}
	assertManagedServiceReady(t, lifecycle)
}

func TestLifecycleRestartRejectsUnchangedProcessAndResumes(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	handler := &testMaintenanceHandler{}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, handler)
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	managed.ignoreRestart = true
	runner.setHook(managed.command)
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "disabled", managed.pid))

	status, err := lifecycle.Restart(context.Background())
	if err == nil || !status.Active || status.MainPID != managed.pid {
		t.Fatalf("restart result = %+v, %v; want unchanged-process error", status, err)
	}
	if got := handler.counts(); got != [2]int{1, 1} {
		t.Fatalf("maintenance counts = %v", got)
	}
	assertManagedServiceReady(t, lifecycle)
}

func assertManagedServiceReady(t *testing.T, lifecycle *Lifecycle) {
	t.Helper()
	client := dialTestClient(t, lifecycle.config.Paths.SocketPath, lifecycle.config.ProtocolVersion)
	defer client.Close()
	health, err := client.Health(context.Background())
	if err != nil || !health.Accepting || health.State != HealthReady {
		t.Fatalf("service admissions not ready: %+v, %v", health, err)
	}
}

func TestLifecycleStopAbortsBeforeSystemdWhenDrainFails(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	handler := &testMaintenanceHandler{drain: func(context.Context) error { return errors.New("work remains") }}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, handler)
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "disabled", os.Getpid()))

	if _, err := lifecycle.Stop(context.Background()); grpcstatus.Code(err) != codes.Aborted {
		t.Fatalf("stop error = %v, want drain Aborted", err)
	}
	if got := handler.counts(); got != [2]int{1, 1} {
		t.Fatalf("maintenance counts = %v", got)
	}
	client := dialTestClient(t, lifecycle.config.Paths.SocketPath, lifecycle.config.ProtocolVersion)
	defer client.Close()
	health, err := client.Health(context.Background())
	if err != nil || !health.Accepting || health.State != HealthReady {
		t.Fatalf("service admissions not restored: %+v, %v", health, err)
	}
	for _, call := range runner.snapshot() {
		if strings.Contains(call, "\x00stop\x00") {
			t.Fatalf("systemd stop ran after drain failure: %q", call)
		}
	}
}

func TestLifecycleStopFailureResumesAdmissions(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	handler := &testMaintenanceHandler{}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, handler)
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	managed.failStop = true
	runner.setHook(managed.command)
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "disabled", os.Getpid()))

	if _, err := lifecycle.Stop(context.Background()); !errors.Is(err, ErrLifecycleUnavailable) {
		t.Fatalf("stop error = %v, want ErrLifecycleUnavailable", err)
	}
	if got := handler.counts(); got != [2]int{1, 1} {
		t.Fatalf("maintenance counts = %v", got)
	}
	client := dialTestClient(t, lifecycle.config.Paths.SocketPath, lifecycle.config.ProtocolVersion)
	defer client.Close()
	health, err := client.Health(context.Background())
	if err != nil || !health.Accepting || health.State != HealthReady {
		t.Fatalf("stop failure did not resume service: %+v, %v", health, err)
	}
}

func TestLifecycleStopThatLeavesServiceActiveResumesAdmissions(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	handler := &testMaintenanceHandler{}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, handler)
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	managed.ignoreStop = true
	runner.setHook(managed.command)
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "disabled", os.Getpid()))

	status, err := lifecycle.Stop(context.Background())
	if err == nil || !status.Active {
		t.Fatalf("stop result = %+v, %v; want active error", status, err)
	}
	if got := handler.counts(); got != [2]int{1, 1} {
		t.Fatalf("maintenance counts = %v", got)
	}
	client := dialTestClient(t, lifecycle.config.Paths.SocketPath, lifecycle.config.ProtocolVersion)
	defer client.Close()
	health, healthErr := client.Health(context.Background())
	if healthErr != nil || !health.Accepting || health.State != HealthReady {
		t.Fatalf("still-active service was not resumed: %+v, %v", health, healthErr)
	}
}

func TestLifecycleStopResumesAfterLostDrainResponse(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	handler := &testMaintenanceHandler{}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, handler)
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "disabled", managed.pid))
	lostResponse := errors.New("drain response lost")
	lifecycle.afterDrainResponse = func() error { return lostResponse }
	if _, err := lifecycle.Stop(context.Background()); !errors.Is(err, lostResponse) {
		t.Fatalf("stop error = %v, want lost response", err)
	}
	if got := handler.counts(); got != [2]int{1, 1} {
		t.Fatalf("maintenance counts = %v", got)
	}
	assertManagedServiceReady(t, lifecycle)
	for _, call := range runner.snapshot() {
		if strings.Contains(call, "\x00stop\x00") {
			t.Fatalf("systemd stop ran after uncertain drain: %q", call)
		}
	}
}

func TestLifecycleStopRejectsNonterminalManagerStateAndResumes(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	handler := &testMaintenanceHandler{}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, handler)
	defer managed.stop()
	if err := managed.start(); err != nil {
		t.Fatalf("start fixture service: %v", err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "active", "running", "disabled", managed.pid))
	runner.setHook(func(_ string, arguments []string) error {
		if len(arguments) > 1 && arguments[1] == "stop" {
			runner.setStdout(lifecycleShowOutput(lifecycle, "deactivating", "stop-sigterm", "disabled", managed.pid))
		}
		return nil
	})
	status, err := lifecycle.Stop(context.Background())
	if err == nil || status.ActiveState != "deactivating" || status.MainPID != managed.pid {
		t.Fatalf("stop result = %+v, %v; want nonterminal error", status, err)
	}
	if got := handler.counts(); got != [2]int{1, 1} {
		t.Fatalf("maintenance counts = %v", got)
	}
	assertManagedServiceReady(t, lifecycle)
}

func TestLifecycleStartRejectsWrongServiceIdentity(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, "other-service", &testMaintenanceHandler{})
	defer managed.stop()
	runner.setHook(managed.command)
	runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "disabled", 0))
	if _, err := lifecycle.Start(context.Background()); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("start error = %v, want ErrLifecycleOwnership", err)
	}
}

func TestLifecycleMutationsPreflightManagerOwnership(t *testing.T) {
	for _, action := range []string{"enable", "start", "restart"} {
		t.Run(action, func(t *testing.T) {
			lifecycle, runner, request := lifecycleFixture(t)
			lifecycle.config.ReadinessTimeout = 50 * time.Millisecond
			if _, err := lifecycle.Install(context.Background(), request); err != nil {
				t.Fatalf("install: %v", err)
			}
			foreign := filepath.Join(t.TempDir(), lifecycle.config.UnitName)
			raw := bytes.Replace(lifecycleShowOutput(lifecycle, "inactive", "dead", "disabled", 0), []byte(lifecycle.config.Paths.UnitPath), []byte(foreign), 1)
			runner.setStdout(raw)
			var err error
			switch action {
			case "enable":
				_, err = lifecycle.Enable(context.Background())
			case "start":
				_, err = lifecycle.Start(context.Background())
			case "restart":
				_, err = lifecycle.Restart(context.Background())
			}
			if !errors.Is(err, ErrLifecycleOwnership) {
				t.Fatalf("%s error = %v, want ErrLifecycleOwnership", action, err)
			}
			for _, call := range runner.snapshot()[1:] {
				if strings.Contains(call, "\x00"+action+"\x00") {
					t.Fatalf("%s mutated foreign manager unit: %q", action, call)
				}
			}
		})
	}
}

func TestLifecycleStartRequiresReadyAndAcceptingHealth(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	lifecycle.config.ReadinessTimeout = 80 * time.Millisecond
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	managed.initialState = HealthStarting
	managed.initialAccepting = false
	defer managed.stop()
	runner.setHook(managed.command)
	runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "disabled", 0))
	if _, err := lifecycle.Start(context.Background()); !errors.Is(err, ErrLifecycleUnavailable) {
		t.Fatalf("start error = %v, want ErrLifecycleUnavailable", err)
	}
}

func TestLifecycleStartWaitsForReadyTransition(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	lifecycle.config.ReadinessTimeout = 2 * time.Second
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatalf("install: %v", err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	managed.initialState = HealthStarting
	managed.readyDelay = 30 * time.Millisecond
	defer managed.stop()
	runner.setHook(managed.command)
	runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "disabled", 0))
	status, err := lifecycle.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !status.Active {
		t.Fatalf("service not active after ready transition: %+v", status)
	}
}

func lifecycleShowOutput(lifecycle *Lifecycle, active, sub, unitFile string, pid int) []byte {
	return []byte(fmt.Sprintf("MainPID=%d\nResult=success\nId=%s\nLoadState=loaded\nActiveState=%s\nSubState=%s\nFragmentPath=%s\nUnitFileState=%s\n",
		pid, lifecycle.config.UnitName, active, sub, lifecycle.config.Paths.UnitPath, unitFile))
}

func lifecycleNotFoundOutput(lifecycle *Lifecycle) []byte {
	return []byte(fmt.Sprintf("MainPID=0\nResult=success\nId=%s\nLoadState=not-found\nActiveState=inactive\nSubState=dead\nFragmentPath=\nUnitFileState=disabled\n", lifecycle.config.UnitName))
}

func lifecycleFixture(t *testing.T) (*Lifecycle, *lifecycleRunner, InstallRequest) {
	t.Helper()
	root := t.TempDir()
	paths := LifecyclePaths{
		UnitPath:             filepath.Join(root, "config", "systemd", "user", "test-service.service"),
		DescriptorPath:       filepath.Join(root, "config", "herdr", "plugins", "test-service", "service.json"),
		BinaryDirectory:      filepath.Join(root, "data", "herdr", "plugins", "test-service", "service"),
		RuntimeDirectory:     filepath.Join(root, "r"),
		RuntimeDirectoryName: "r",
		SocketPath:           filepath.Join(root, "r", "s"),
	}
	runner := &lifecycleRunner{}
	lifecycle, err := NewLifecycle(LifecycleConfig{
		UnitName: "test-service.service", ServiceName: "test-service", Description: "Test Service", ProtocolVersion: 1,
		ClientName: "test-lifecycle", Paths: paths,
	}, runner)
	if err != nil {
		t.Fatalf("new lifecycle: %v", err)
	}
	source := filepath.Join(root, "build", "test-service")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatalf("create source directory: %v", err)
	}
	if err := os.WriteFile(source, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.Chmod(source, 0o700); err != nil {
		t.Fatalf("chmod source: %v", err)
	}
	return lifecycle, runner, InstallRequest{
		Consent: true, SourceExecutable: source, Version: "1.0.0",
		Arguments: []string{"serve", "--socket", paths.SocketPath},
	}
}

func TestLifecycleInstallRejectsDotVersionEscape(t *testing.T) {
	for _, version := range []string{".", ".."} {
		t.Run(version, func(t *testing.T) {
			lifecycle, runner, request := lifecycleFixture(t)
			request.Version = version
			if _, err := lifecycle.Install(context.Background(), request); err == nil {
				t.Fatalf("version %q was accepted", version)
			}
			if calls := runner.snapshot(); len(calls) != 0 {
				t.Fatalf("commands after version rejection = %q", calls)
			}
			if _, err := os.Lstat(lifecycle.config.Paths.BinaryDirectory); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("binary directory mutated for version %q: %v", version, err)
			}
		})
	}
}

func TestLifecycleInstallRejectsReplacementExecutableSymlink(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	result, err := lifecycle.Install(context.Background(), request)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	identical := filepath.Join(filepath.Dir(request.SourceExecutable), "identical")
	contents, err := os.ReadFile(request.SourceExecutable)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	if err := os.WriteFile(identical, contents, 0o700); err != nil {
		t.Fatalf("write identical executable: %v", err)
	}
	if err := os.Chmod(identical, 0o700); err != nil {
		t.Fatalf("chmod identical executable: %v", err)
	}
	if err := os.Remove(result.Executable); err != nil {
		t.Fatalf("remove installed executable: %v", err)
	}
	if err := os.Symlink(identical, result.Executable); err != nil {
		t.Fatalf("replace executable with symlink: %v", err)
	}
	if _, err := lifecycle.Install(context.Background(), request); !errors.Is(err, ErrLifecycleOwnership) {
		t.Fatalf("reinstall error = %v, want ErrLifecycleOwnership", err)
	}
	if calls := runner.snapshot(); len(calls) != 1 {
		t.Fatalf("commands after executable replacement = %q", calls)
	}
}

func TestLifecycleUnitPreservesLiteralDollarArgument(t *testing.T) {
	lifecycle, _, request := lifecycleFixture(t)
	request.Arguments = append(request.Arguments, `${HOME}`, `$literal`)
	result, err := lifecycle.Install(context.Background(), request)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	unit, err := os.ReadFile(result.UnitPath)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if !bytes.Contains(unit, []byte(`"$${HOME}" "$$literal"`)) {
		t.Fatalf("unit does not preserve literal dollars:\n%s", unit)
	}
}

func TestLifecycleRejectsDollarRuntimeDirectoryName(t *testing.T) {
	lifecycle, runner, _ := lifecycleFixture(t)
	config := lifecycle.config
	runtimeRoot := strings.TrimSuffix(config.Paths.RuntimeDirectory, filepath.FromSlash(config.Paths.RuntimeDirectoryName))
	config.Paths.RuntimeDirectoryName = "herdr/plugins/service$test"
	config.Paths.RuntimeDirectory = filepath.Join(runtimeRoot, filepath.FromSlash(config.Paths.RuntimeDirectoryName))
	config.Paths.SocketPath = filepath.Join(config.Paths.RuntimeDirectory, "service.sock")
	if _, err := NewLifecycle(config, runner); err == nil {
		t.Fatal("dollar-bearing runtime directory name was accepted")
	}
}

func TestLifecycleDescriptorSizeBoundary(t *testing.T) {
	lifecycle, _, request := lifecycleFixture(t)
	descriptor, source, err := lifecycle.descriptorForInstall(request)
	if err != nil {
		t.Fatalf("build descriptor: %v", err)
	}
	_ = source.Close()
	descriptor.Arguments = []string{"x"}
	base, err := marshalLifecycleDescriptor(descriptor)
	if err != nil {
		t.Fatalf("marshal base descriptor: %v", err)
	}
	argumentLength := 1 + maxLifecycleDescriptor - len(base)
	request.Arguments = []string{strings.Repeat("x", argumentLength)}
	result, err := lifecycle.Install(context.Background(), request)
	if err != nil {
		t.Fatalf("exact-limit descriptor install: %v", err)
	}
	descriptorBytes, err := os.ReadFile(result.DescriptorPath)
	if err != nil {
		t.Fatalf("read descriptor: %v", err)
	}
	if len(descriptorBytes) != maxLifecycleDescriptor {
		t.Fatalf("descriptor size = %d, want %d", len(descriptorBytes), maxLifecycleDescriptor)
	}

	tooLargeLifecycle, tooLargeRunner, tooLargeRequest := lifecycleFixture(t)
	tooLargeRequest.Arguments = []string{strings.Repeat("x", argumentLength+1)}
	if _, err := tooLargeLifecycle.Install(context.Background(), tooLargeRequest); err == nil {
		t.Fatal("oversized descriptor was accepted")
	}
	if calls := tooLargeRunner.snapshot(); len(calls) != 0 {
		t.Fatalf("commands after oversized descriptor = %q", calls)
	}
	for _, path := range []string{tooLargeLifecycle.config.Paths.UnitPath, tooLargeLifecycle.config.Paths.DescriptorPath, tooLargeLifecycle.config.Paths.BinaryDirectory} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("oversized descriptor mutated %s: %v", path, err)
		}
	}
}

type lifecycleRunner struct {
	mu      sync.Mutex
	calls   []string
	err     error
	started chan struct{}
	release chan struct{}
	once    sync.Once
	stdout  []byte
	hook    func(string, []string) error
}

func (r *lifecycleRunner) Run(ctx context.Context, executable string, arguments []string) (command.Result, error) {
	if err := ctx.Err(); err != nil {
		return command.Result{Canceled: true}, err
	}
	r.mu.Lock()
	r.calls = append(r.calls, executable+"\x00"+string(bytes.Join(stringBytes(arguments), []byte{0})))
	err := r.err
	started := r.started
	release := r.release
	stdout := append([]byte(nil), r.stdout...)
	hook := r.hook
	r.mu.Unlock()
	if started != nil {
		r.once.Do(func() { close(started) })
		select {
		case <-release:
		case <-ctx.Done():
			return command.Result{Canceled: true}, ctx.Err()
		}
	}
	if hook != nil {
		hookErr := hook(executable, append([]string(nil), arguments...))
		if err == nil {
			err = hookErr
		}
	}
	return command.Result{Stdout: stdout}, err
}

func (r *lifecycleRunner) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *lifecycleRunner) setError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *lifecycleRunner) setStdout(stdout []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stdout = append([]byte(nil), stdout...)
}

func (r *lifecycleRunner) setHook(hook func(string, []string) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hook = hook
}

type managedLifecycleService struct {
	t                *testing.T
	lifecycle        *Lifecycle
	runner           *lifecycleRunner
	version          string
	serviceName      string
	handler          MaintenanceHandler
	server           *Server
	cancel           context.CancelFunc
	done             chan error
	failStop         bool
	ignoreStop       bool
	failRestart      bool
	ignoreRestart    bool
	pid              int
	nextVersion      string
	initialState     HealthState
	initialAccepting bool
	readyDelay       time.Duration
}

func newManagedLifecycleService(t *testing.T, lifecycle *Lifecycle, runner *lifecycleRunner, version, serviceName string, handler MaintenanceHandler) *managedLifecycleService {
	t.Helper()
	return &managedLifecycleService{t: t, lifecycle: lifecycle, runner: runner, version: version, serviceName: serviceName, handler: handler}
}

func (m *managedLifecycleService) command(_ string, arguments []string) error {
	if len(arguments) < 2 || arguments[0] != "--user" {
		return nil
	}
	switch arguments[1] {
	case "start":
		if m.nextVersion != "" {
			m.version = m.nextVersion
			m.nextVersion = ""
		}
		if err := m.start(); err != nil {
			return err
		}
		m.runner.setStdout(lifecycleShowOutput(m.lifecycle, "active", "running", "disabled", m.pid))
	case "stop":
		if m.failStop {
			return errors.New("stop failed")
		}
		if m.ignoreStop {
			return nil
		}
		m.stop()
		m.runner.setStdout(lifecycleShowOutput(m.lifecycle, "inactive", "dead", "disabled", 0))
	case "restart":
		if m.failRestart {
			return errors.New("restart failed")
		}
		if m.ignoreRestart {
			return nil
		}
		m.stop()
		if err := m.start(); err != nil {
			return err
		}
		m.runner.setStdout(lifecycleShowOutput(m.lifecycle, "active", "running", "disabled", m.pid))
	}
	return nil
}

func (m *managedLifecycleService) start() error {
	if m.cancel != nil {
		return nil
	}
	state := m.initialState
	accepting := m.initialAccepting
	if state == 0 {
		state = HealthReady
		accepting = true
	}
	server, err := NewServer(ServerConfig{
		SocketPath: m.lifecycle.config.Paths.SocketPath, ProtocolVersion: m.lifecycle.config.ProtocolVersion,
		ServiceName: m.serviceName, ServiceVersion: m.version, Maintenance: m.handler,
		InitialState: state, InitialAccepting: accepting,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.server = server
	m.pid++
	if m.pid == 1 {
		m.pid = os.Getpid()
	}
	m.done = make(chan error, 1)
	go func() { m.done <- server.Serve(ctx) }()
	if m.readyDelay > 0 {
		go func() {
			timer := time.NewTimer(m.readyDelay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
			case <-timer.C:
				_, _ = server.UpdateStatus(HealthReady, true, "ready")
			}
		}()
	}
	return nil
}

func (m *managedLifecycleService) stop() {
	if m.cancel == nil {
		return
	}
	m.cancel()
	select {
	case err := <-m.done:
		if err != nil {
			m.t.Errorf("fixture service stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		m.t.Error("fixture service did not stop")
	}
	m.cancel = nil
	m.server = nil
	m.done = nil
}

func stringBytes(values []string) [][]byte {
	result := make([][]byte, len(values))
	for index, value := range values {
		result[index] = []byte(value)
	}
	return result
}
