//go:build linux

package servicehost

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLifecycleEnabledStartRefusal(t *testing.T) {
	for _, kind := range []string{"foreign", "reordered", "disabled", "canceled", "foreign-manager"} {
		t.Run(kind, func(t *testing.T) {
			lifecycle, runner, request := lifecycleFixture(t)
			request.Arguments = []string{"serve", "--binding", "expected"}
			if _, err := lifecycle.Install(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "enabled", 0))
			arguments := slices.Clone(request.Arguments)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := ErrLifecycleOwnership
			switch kind {
			case "foreign":
				arguments[2] = "other"
			case "reordered":
				arguments[0], arguments[1] = arguments[1], arguments[0]
			case "disabled":
				runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "disabled", 0))
				want = ErrLifecycleDisabled
			case "canceled":
				cancel()
				want = context.Canceled
			case "foreign-manager":
				runner.setStdout([]byte(strings.ReplaceAll(string(lifecycleShowOutput(lifecycle, "inactive", "dead", "enabled", 0)), lifecycle.config.Paths.UnitPath, "/other/unit")))
			}
			if _, err := lifecycle.StartEnabled(ctx, arguments); !errors.Is(err, want) {
				t.Fatalf("error=%v want=%v", err, want)
			}
			for _, call := range runner.snapshot() {
				if strings.Contains(call, "\x00start\x00") {
					t.Fatalf("refusal started service: %q", call)
				}
			}
		})
	}
}

func TestLifecycleEnabledStartAfterDisable(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "enabled", 0))
	observed, err := lifecycle.Status(context.Background())
	if err != nil || !observed.Enabled {
		t.Fatalf("status=%+v %v", observed, err)
	}
	runner.setHook(func(_ string, arguments []string) error {
		if len(arguments) > 1 && arguments[1] == "disable" {
			runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "disabled", 0))
		}
		return nil
	})
	if _, err := lifecycle.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.StartEnabled(context.Background(), request.Arguments); !errors.Is(err, ErrLifecycleDisabled) {
		t.Fatalf("error=%v", err)
	}
	for _, call := range runner.snapshot() {
		if strings.Contains(call, "\x00start\x00") {
			t.Fatal("stale enabled status bypassed Disable")
		}
	}
}

func TestLifecycleEnabledStartHoldsLockAcrossPreconditions(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	other, err := NewLifecycle(lifecycle.config, runner)
	if err != nil {
		t.Fatal(err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	defer managed.stop()
	runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "enabled", 0))
	checked := false
	runner.setHook(func(executable string, arguments []string) error {
		if len(arguments) > 1 && arguments[1] == "show" && !checked {
			checked = true
			if _, err := other.Disable(context.Background()); !errors.Is(err, ErrLifecycleBusy) {
				t.Fatalf("Disable interleaved: %v", err)
			}
		}
		return managed.command(executable, arguments)
	})
	result, err := lifecycle.StartEnabled(context.Background(), request.Arguments)
	if err != nil || !checked || !result.Active || result.MainPID != os.Getpid() {
		t.Fatalf("result=%+v checked=%t %v", result, checked, err)
	}
}

func TestLifecycleEnabledStartRequiresReadiness(t *testing.T) {
	lifecycle, runner, request := lifecycleFixture(t)
	lifecycle.config.ReadinessTimeout = 50 * time.Millisecond
	if _, err := lifecycle.Install(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	managed := newManagedLifecycleService(t, lifecycle, runner, request.Version, lifecycle.config.ServiceName, &testMaintenanceHandler{})
	managed.initialState, managed.initialAccepting = HealthStarting, false
	defer managed.stop()
	runner.setHook(managed.command)
	runner.setStdout(lifecycleShowOutput(lifecycle, "inactive", "dead", "enabled", 0))
	if _, err := lifecycle.StartEnabled(context.Background(), request.Arguments); !errors.Is(err, ErrLifecycleUnavailable) {
		t.Fatalf("readiness error=%v", err)
	}
}
