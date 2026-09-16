package agenthost

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func TestManagedCodexPromptLiveNewAndForkUseSharedService(t *testing.T) {
	if os.Getenv("HERDR_PLUGIN_KIT_MANAGED_PROMPT_LIVE") != "1" {
		t.Skip("set HERDR_PLUGIN_KIT_MANAGED_PROMPT_LIVE=1 for the Herdr 0.8.2/Codex 0.154.0 prompt integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runner := command.ExecRunner{}
	sessionName := os.Getenv("HERDR_PLUGIN_KIT_LIVE_SESSION")
	if sessionName == "" {
		t.Fatal("HERDR_PLUGIN_KIT_LIVE_SESSION must name the task-owned live session")
	}
	directory, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	liveRunner := liveSessionRunner{Runner: runner, Session: sessionName}
	origin := createPromptLiveWorkspace(t, ctx, liveRunner, directory)
	origin.SessionName = sessionName
	host := Host{Runner: runner, Session: sessionName}
	topology := sessionhost.Host{Runner: runner, Session: sessionName}
	launcher := newManagedLiveLauncher(t, host)
	var operations []*ManagedCodex
	maintenanceAttempted := false
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		if !maintenanceAttempted {
			for i := len(operations) - 1; i >= 0; i-- {
				if cleanupErr := operations[i].Cleanup(cleanupCtx); cleanupErr != nil {
					t.Errorf("cleanup managed prompt thread %s: %v", operations[i].ThreadID, cleanupErr)
				}
			}
		}
		if result, cleanupErr := liveRunner.Run(cleanupCtx, "herdr", []string{"workspace", "close", origin.WorkspaceID}); cleanupErr != nil {
			t.Errorf("close managed prompt workspace: %v: %s", cleanupErr, result.Stderr)
		}
	})

	newCanary := "HPK-API5-MANAGED-NEW"
	newPrompt, _ := PreparePrompt("Reply exactly " + newCanary)
	managed, err := launcher.Launch(ctx, ManagedCodexRequest{
		Operation: ManagedNew, Origin: origin, Directory: directory, Placement: sessionhost.NewTab,
		TabLabel: "api5-managed-new", Policy: managedPolicy(), ReadyTimeout: 45 * time.Second, Prompt: &newPrompt,
	})
	if err != nil || managed.Prompt.State != PromptAcknowledged {
		t.Fatalf("managed New=%+v error=%v", managed, err)
	}
	operations = append(operations, managed)
	source := awaitLivePromptResult(t, ctx, host, topology, managed.Prompt, newCanary)

	forkCanary := "HPK-API5-MANAGED-FORK"
	forkPrompt, _ := PreparePrompt("Reply exactly " + forkCanary)
	forked, err := launcher.Launch(ctx, ManagedCodexRequest{
		Operation: ManagedFork, Origin: managed.Target.Origin, Source: source, Directory: directory,
		Placement: sessionhost.NewTab, TabLabel: "api5-managed-fork", Policy: managedPolicy(), ReadyTimeout: 45 * time.Second, Prompt: &forkPrompt,
	})
	if err != nil || forked.Prompt.State != PromptAcknowledged || forked.ThreadID == managed.ThreadID || forked.SourceThreadID != managed.ThreadID {
		t.Fatalf("managed Fork=%+v source=%+v error=%v", forked, managed, err)
	}
	operations = append(operations, forked)
	_ = awaitLivePromptResult(t, ctx, host, topology, forked.Prompt, forkCanary)
	sourceHistory, err := (ManagedCodexLauncher{Host: host}).readFullPaneHistory(ctx, managed.Target.Origin)
	if err != nil || strings.Contains(sourceHistory, forkCanary) {
		t.Fatalf("managed Fork prompt reached source: contains=%t error=%v", strings.Contains(sourceHistory, forkCanary), err)
	}
	// Only the explicitly disposable fixture may exercise disruptive uninstall.
	// Reuse these persisted conversations rather than spending another model turn.
	if os.Getenv("HERDR_PLUGIN_KIT_LIVE_CODEX_HOME") == "" {
		return
	}
	for {
		visible, readErr := launcher.readPaneANSI(ctx, forked.Target.PaneID, "visible", 220)
		plain := managedANSI.ReplaceAllString(visible, "")
		if readErr == nil && emptyCodexComposer(visible) && strings.Contains(plain, "• "+forkCanary) {
			break
		}
		if err := waitManaged(ctx, 50*time.Millisecond); err != nil {
			t.Fatalf("settle fork before maintenance: %v", err)
		}
	}
	configPath := filepath.Join(os.Getenv("HERDR_PLUGIN_KIT_LIVE_CODEX_HOME"), "config.toml")
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	service := launcher.service.(*CodexService)
	observer, err := newCodexLifecycle(ctx, managed.Service.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close() })
	maintenanceAttempted = true
	removed, err := service.Uninstall(ctx, true)
	if err != nil || !removed.RemovedUnit || !removed.RemovedDescriptor {
		t.Fatalf("isolated maintenance uninstall=%+v error=%v", removed, err)
	}
	if _, err := observer.ReadThread(ctx, managed.ThreadID); err == nil {
		t.Fatal("old observer stayed connected after service uninstall")
	}
	if _, err := service.Ensure(ctx); !errors.Is(err, ErrCodexServiceNotInstalled) {
		t.Fatalf("managed action after uninstall: %v", err)
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(configBefore, configAfter) {
		t.Fatalf("uninstall changed Codex configuration: %v", err)
	}
	argv := []string{"codex", "resume", "--strict-config", "--cd", directory, managed.ThreadID}
	resumed, err := topology.Launch(ctx, sessionhost.LaunchRequest{Origin: origin, Argv: argv, Directory: directory, Placement: sessionhost.NewTab, TabLabel: "ordinary-after-uninstall"})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := beginOrdinaryCodexBinding(ctx, launcher, ManagedNew, Agent{}, directory, managedPolicy(), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := binding.Finish(ctx, resumed, directory, argv)
	if err != nil || reopened.Session == nil || reopened.Session.Value != managed.ThreadID {
		t.Fatalf("ordinary reopen lost original identity: %+v error=%v", reopened, err)
	}
	history, err := launcher.readFullPaneHistory(ctx, resumed.Origin)
	if err != nil || !strings.Contains(history, newCanary) || strings.Contains(history, forkCanary) {
		t.Fatalf("ordinary reopen lost original history: %v", err)
	}
}
