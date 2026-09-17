package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func TestManagedCodexLiveNewForkAndResumeUseSharedServiceAndExactPaneIdentity(t *testing.T) {
	if os.Getenv("HERDR_PLUGIN_KIT_MANAGED_CODEX_LIVE") != "1" {
		t.Skip("set HERDR_PLUGIN_KIT_MANAGED_CODEX_LIVE=1 for the Herdr 0.8.2/Codex 0.154.0 integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	runner := command.ExecRunner{}
	sessionName := os.Getenv("HERDR_PLUGIN_KIT_LIVE_SESSION")
	var liveRunner command.Runner = runner
	if sessionName != "" {
		liveRunner = liveSessionRunner{Runner: runner, Session: sessionName}
	}
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	directory, err = filepath.Abs(filepath.Join(directory, "..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	var origin sessionhost.Origin
	var taskTabs []string
	workspaceID := os.Getenv("HERDR_PLUGIN_KIT_LIVE_WORKSPACE")
	if workspaceID != "" {
		origin, err = createLiveShellTab(ctx, liveRunner, sessionName, workspaceID, directory)
		if err != nil {
			t.Fatal(err)
		}
		taskTabs = append(taskTabs, origin.TabID)
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cleanupCancel()
			for i := len(taskTabs) - 1; i >= 0; i-- {
				result, cleanupErr := liveRunner.Run(cleanupCtx, "herdr", []string{"tab", "close", taskTabs[i]})
				if cleanupErr != nil {
					t.Errorf("close task-owned live tab %s: %v: %s", taskTabs[i], cleanupErr, result.Stderr)
				}
			}
		})
	} else {
		create, createErr := liveRunner.Run(ctx, "herdr", []string{"workspace", "create", "--cwd", directory, "--label", fmt.Sprintf("hpk-api10-live-%d", time.Now().UnixNano()), "--no-focus"})
		if createErr != nil {
			t.Fatalf("create isolated workspace: %v: %s", createErr, create.Stderr)
		}
		var response struct {
			ID     string `json:"id"`
			Result struct {
				Type     string `json:"type"`
				RootPane struct {
					PaneID      string `json:"pane_id"`
					WorkspaceID string `json:"workspace_id"`
					TabID       string `json:"tab_id"`
				} `json:"root_pane"`
			} `json:"result"`
		}
		if json.Unmarshal(create.Stdout, &response) != nil || response.ID != "cli:workspace:create" || response.Result.Type != "workspace_created" || response.Result.RootPane.PaneID == "" || response.Result.RootPane.WorkspaceID == "" || response.Result.RootPane.TabID == "" {
			t.Fatalf("unexpected workspace response: %s", create.Stdout)
		}
		origin = sessionhost.Origin{SessionName: sessionName, WorkspaceID: response.Result.RootPane.WorkspaceID, TabID: response.Result.RootPane.TabID, PaneID: response.Result.RootPane.PaneID}
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cleanupCancel()
			result, cleanupErr := liveRunner.Run(cleanupCtx, "herdr", []string{"workspace", "close", origin.WorkspaceID})
			if cleanupErr != nil {
				t.Errorf("close isolated workspace: %v: %s", cleanupErr, result.Stderr)
			}
		})
	}
	launcher := newManagedLiveLauncher(t, Host{Runner: runner, Session: sessionName})
	var operations []*ManagedCodex
	cleaned := false
	cleanupOperations := func() {
		if cleaned {
			return
		}
		cleaned = true
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		for i := len(operations) - 1; i >= 0; i-- {
			if cleanupErr := operations[i].Cleanup(cleanupCtx); cleanupErr != nil {
				t.Errorf("cleanup managed operation %s: %v", operations[i].ThreadID, cleanupErr)
			}
		}
	}
	t.Cleanup(cleanupOperations)

	managed, err := launcher.Launch(ctx, ManagedCodexRequest{
		Operation: ManagedNew, Origin: origin, Directory: directory,
		Placement: sessionhost.NewTab, TabLabel: "managed-live", Policy: managedPolicy(), ExtraArgv: []string{"--search"},
	})
	if errors.Is(err, ErrManagedNarrow) {
		t.Skip("live Herdr terminal is too narrow for Codex to render a complete session UUID; widen it and rerun")
	}
	if err != nil {
		t.Fatal(err)
	}
	operations = append(operations, managed)
	if !validCodexThreadID(managed.ThreadID) || managed.Agent.Session == nil || managed.Agent.Session.Value != managed.ThreadID || managed.Target.SessionName != sessionName || managed.Target.WorkspaceID != origin.WorkspaceID || managed.Service.State != CodexServiceReady || len(managed.Argv) < 2 || managed.Argv[1] != "--search" {
		t.Fatalf("managed=%+v", managed)
	}
	if err := makeLiveCodexSourcePersistent(ctx, launcher, managed); err != nil {
		t.Fatalf("persist live fork source: %v", err)
	}
	forked, err := launcher.Launch(ctx, ManagedCodexRequest{
		Operation: ManagedFork, Origin: managed.Target.Origin, Source: managed.Agent, Directory: directory,
		Placement: sessionhost.NewPane, Direction: "down", InheritSourcePolicy: true,
	})
	if errors.Is(err, ErrManagedNarrow) {
		t.Skip("live Herdr terminal is too narrow for Codex to render a complete fork session UUID; widen it and rerun")
	}
	if err != nil {
		t.Fatal(err)
	}
	operations = append(operations, forked)
	if forked.ThreadID == managed.ThreadID || forked.SourceThreadID != managed.ThreadID || forked.Service != managed.Service || forked.Target.TabID != managed.Target.TabID || forked.Target.PaneID == managed.Target.PaneID {
		t.Fatalf("source=%+v fork=%+v", managed, forked)
	}
	if err := managed.Cleanup(ctx); err != nil {
		t.Fatalf("close original TUI before exact resume: %v", err)
	}
	resumeOrigin, err := createLiveShellTab(ctx, liveRunner, sessionName, origin.WorkspaceID, directory)
	if err != nil {
		t.Fatal(err)
	}
	if workspaceID != "" {
		taskTabs = append(taskTabs, resumeOrigin.TabID)
	}
	resumed, err := launcher.Launch(ctx, ManagedCodexRequest{
		Operation: ManagedResume, Origin: resumeOrigin, Directory: directory,
		ThreadID: managed.ThreadID, SourceArgv: managed.Argv, Policy: managedPolicy(),
	})
	if errors.Is(err, ErrManagedNarrow) {
		t.Skip("live Herdr terminal is too narrow for Codex to render a complete resumed session UUID; widen it and rerun")
	}
	if err != nil {
		t.Fatal(err)
	}
	operations = append(operations, resumed)
	if resumed.ThreadID != managed.ThreadID || resumed.SourceThreadID != managed.ThreadID || resumed.Service != managed.Service || resumed.Target.Origin != resumeOrigin {
		t.Fatalf("source=%+v resumed=%+v", managed, resumed)
	}

	service := NewCodexService(runner)
	if configured, ok := launcher.service.(*CodexService); ok {
		service = configured
	}
	beforeCleanup, err := service.Inspect(ctx)
	if err != nil || beforeCleanup != managed.Service {
		t.Fatalf("shared service before cleanup=%+v error=%v", beforeCleanup, err)
	}
	cleanupOperations()
	afterCleanup, err := service.Inspect(ctx)
	if err != nil || afterCleanup != managed.Service {
		t.Fatalf("shared service after cleanup=%+v error=%v", afterCleanup, err)
	}
}

// An explicit task home selects a disposable service instead of the installed
// user service. It must be the same temporary home inherited by the task Herdr
// session, so ordinary source histories remain available during conversion.
func newManagedLiveLauncher(t *testing.T, host Host) ManagedCodexLauncher {
	t.Helper()
	launcher := NewManagedCodexLauncher(host)
	home := os.Getenv("HERDR_PLUGIN_KIT_LIVE_CODEX_HOME")
	if home == "" {
		return launcher
	}
	if home != os.Getenv("CODEX_HOME") || !cleanAbsolute(home) || !strings.HasPrefix(home, filepath.Clean(os.TempDir())+string(filepath.Separator)) {
		t.Fatal("HERDR_PLUGIN_KIT_LIVE_CODEX_HOME must equal CODEX_HOME and name the task-owned temporary home inherited by Herdr")
	}
	fixture := newIsolatedCodexServiceLifecycleFixture(t, home)
	launcher.service = fixture.Service
	promptHost := NewPromptHost(host)
	promptHost.managed = launcher
	launcher.deliver = promptHost.Deliver
	return launcher
}

func createLiveShellTab(ctx context.Context, runner command.Runner, sessionName, workspaceID, directory string) (sessionhost.Origin, error) {
	result, err := runner.Run(ctx, "herdr", []string{"tab", "create", "--workspace", workspaceID, "--cwd", directory, "--label", "resume-shell", "--no-focus"})
	if err != nil {
		return sessionhost.Origin{}, fmt.Errorf("create live resume shell: %w: %s", err, result.Stderr)
	}
	var response struct {
		ID     string `json:"id"`
		Result struct {
			Type     string `json:"type"`
			RootPane struct {
				PaneID      string `json:"pane_id"`
				WorkspaceID string `json:"workspace_id"`
				TabID       string `json:"tab_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	if json.Unmarshal(result.Stdout, &response) != nil || response.ID != "cli:tab:create" || response.Result.Type != "tab_created" || response.Result.RootPane.PaneID == "" || response.Result.RootPane.WorkspaceID != workspaceID || response.Result.RootPane.TabID == "" {
		return sessionhost.Origin{}, fmt.Errorf("unexpected live shell response: %s", result.Stdout)
	}
	return sessionhost.Origin{SessionName: sessionName, WorkspaceID: workspaceID, TabID: response.Result.RootPane.TabID, PaneID: response.Result.RootPane.PaneID}, nil
}

type liveSessionRunner struct {
	command.Runner
	Session string
}

func (r liveSessionRunner) Run(ctx context.Context, executable string, args []string) (command.Result, error) {
	if executable == "herdr" {
		args = append([]string{"--session", r.Session}, args...)
	}
	return r.Runner.Run(ctx, executable, args)
}

func makeLiveCodexSourcePersistent(ctx context.Context, launcher ManagedCodexLauncher, managed *ManagedCodex) error {
	const prompt = "Reply exactly OK."
	if managed == nil || managed.Agent.Session == nil {
		return ErrManagedStale
	}
	if err := launcher.runHerdr(ctx, "pane", "send-text", managed.Target.PaneID, prompt); err != nil {
		return fmt.Errorf("send prompt text: %w", err)
	}
	seenDraft := false
	for {
		visible, err := launcher.readPaneANSI(ctx, managed.Target.PaneID, "visible", managedStatusLines)
		if err != nil {
			return fmt.Errorf("read prompt composer: %w", err)
		}
		composer, ok := codexComposer(managedANSI.ReplaceAllString(visible, ""))
		if ok && composer == prompt {
			if seenDraft {
				break
			}
			seenDraft = true
		} else if !ok || composer != "Ask Codex to do anything" {
			return fmt.Errorf("unexpected prompt composer %q: %w", composer, ErrManagedStale)
		} else {
			seenDraft = false
		}
		if err := waitManaged(ctx, 25*time.Millisecond); err != nil {
			return err
		}
	}
	if err := launcher.runHerdr(ctx, "pane", "send-keys", managed.Target.PaneID, "enter"); err != nil {
		return fmt.Errorf("submit prompt: %w", err)
	}
	verifier, err := newCodexLifecycle(ctx, managed.Service.SocketPath)
	if err != nil {
		return fmt.Errorf("connect persistence verifier: %w", err)
	}
	defer verifier.Close()
	sawActivity := false
	var lastAgentStatus, lastComposer string
	for {
		wire, agent, err := launcher.readManagedAgent(ctx, managed.Target.Origin, managed.Policy.CWD)
		if err != nil || wire.Focused || wire.LaunchPending || agent.Session == nil || agent.Session.Value != managed.ThreadID {
			if err == nil {
				err = ErrManagedStale
			}
			return fmt.Errorf("read prompted agent: %w", err)
		}
		lastAgentStatus = wire.AgentStatus
		if wire.AgentStatus != "idle" {
			sawActivity = true
		}
		if sawActivity && (wire.AgentStatus == "done" || wire.AgentStatus == "idle") {
			visible, readErr := launcher.readPaneANSI(ctx, managed.Target.PaneID, "visible", managedStatusLines)
			if readErr != nil {
				return readErr
			}
			lastComposer, _ = codexComposer(managedANSI.ReplaceAllString(visible, ""))
			identity, identityErr := verifier.ReadThread(ctx, managed.ThreadID)
			if emptyCodexComposer(visible) && identityErr == nil && identity.ID == managed.ThreadID && identity.CWD == managed.Policy.CWD {
				return nil
			}
		}
		if err := waitManaged(ctx, 50*time.Millisecond); err != nil {
			return fmt.Errorf("wait for persisted quiescent source: agent=%q composer=%q: %w", lastAgentStatus, lastComposer, err)
		}
	}
}
