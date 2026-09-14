package agenthost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

func TestCodexServiceLiveSharedLifecycle(t *testing.T) {
	if os.Getenv("HERDR_PLUGIN_KIT_CODEX_SERVICE_LIVE") != "1" {
		t.Skip("set HERDR_PLUGIN_KIT_CODEX_SERVICE_LIVE=1 for the isolated Codex 0.154.0 daemon integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, err := os.MkdirTemp("", "hpk-codex-service-live-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(userHome, ".codex")
	for _, name := range []string{"auth.json", "config.toml"} {
		if err := os.Symlink(filepath.Join(installed, name), filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	standalone := filepath.Join(root, "packages", "standalone")
	if err := os.MkdirAll(standalone, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(installed, "packages", "standalone", "current"), filepath.Join(standalone, "current")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", root)
	runner := command.ExecRunner{}
	stop := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_, stopErr := runner.Run(stopCtx, "codex", []string{"app-server", "daemon", "stop"})
		if stopErr != nil {
			t.Errorf("stop isolated daemon: %v", stopErr)
		}
	}
	t.Cleanup(func() {
		stop()
		// The native stop receipt covers the daemon. Give its independently
		// spawned cache work a short quiescence window before removing CODEX_HOME.
		timer := time.NewTimer(200 * time.Millisecond)
		<-timer.C
		deadline := time.Now().Add(2 * time.Second)
		for {
			err := os.RemoveAll(root)
			if err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("remove isolated CODEX_HOME: %v", err)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	services := []*CodexService{NewCodexService(runner), NewCodexService(runner)}
	snapshots := make([]CodexServiceSnapshot, len(services))
	errorsSeen := make([]error, len(services))
	var group sync.WaitGroup
	for i, service := range services {
		group.Add(1)
		go func() {
			defer group.Done()
			snapshots[i], errorsSeen[i] = service.Ensure(ctx)
		}()
	}
	group.Wait()
	for i, err := range errorsSeen {
		if err != nil {
			t.Fatalf("Ensure[%d]: %v", i, err)
		}
	}
	if snapshots[0] != snapshots[1] || snapshots[0].State != CodexServiceReady || snapshots[0].CLIVersion != SupportedCodexVersion || snapshots[0].AppServerVersion != SupportedCodexVersion {
		t.Fatalf("snapshots=%+v", snapshots)
	}

	inspected, err := NewCodexService(runner).Inspect(ctx)
	if err != nil || inspected != snapshots[0] {
		t.Fatalf("Inspect after callers: snapshot=%+v err=%v", inspected, err)
	}
	stop()
	degraded, err := services[0].Inspect(ctx)
	if degraded.State != CodexServiceDegraded || !errors.Is(err, ErrCodexServiceUnavailable) {
		t.Fatalf("Inspect stopped daemon: snapshot=%+v err=%v", degraded, err)
	}
	repaired, err := services[1].Ensure(ctx)
	if err != nil || repaired.State != CodexServiceReady || repaired.SocketPath != snapshots[0].SocketPath {
		t.Fatalf("repair: snapshot=%+v err=%v", repaired, err)
	}
}
