package agenthost

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func TestOrdinaryCodexLiveBindsPromptedNewAndForkThroughSharedService(t *testing.T) {
	if os.Getenv("HERDR_PLUGIN_KIT_MANAGED_CODEX_LIVE") != "1" {
		t.Skip("set HERDR_PLUGIN_KIT_MANAGED_CODEX_LIVE=1 for the Herdr 0.8.2/Codex 0.154.0 integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var trace []string
	rawRunner := runnerFunc(func(ctx context.Context, exe string, args []string) (command.Result, error) {
		result, err := (command.ExecRunner{}).Run(ctx, exe, args)
		trace = append(trace, fmt.Sprintf("%q: %q error=%v", args, result.Stdout, err))
		if len(trace) > 12 {
			trace = trace[1:]
		}
		return result, err
	})
	var runner command.Runner = rawRunner
	sessionName := os.Getenv("HERDR_PLUGIN_KIT_LIVE_SESSION")
	if sessionName != "" {
		runner = liveSessionRunner{Runner: runner, Session: sessionName}
	}
	directory, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	createdWorkspace, err := runner.Run(ctx, "herdr", []string{"workspace", "create", "--cwd", directory, "--label", fmt.Sprintf("hpk-ordinary-prompt-%d", time.Now().UnixNano()), "--no-focus"})
	if err != nil {
		t.Fatalf("create isolated workspace: %v: %s", err, createdWorkspace.Stderr)
	}
	var workspace struct {
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
	if json.Unmarshal(createdWorkspace.Stdout, &workspace) != nil || workspace.ID != "cli:workspace:create" || workspace.Result.Type != "workspace_created" || workspace.Result.RootPane.WorkspaceID == "" {
		t.Fatalf("unexpected workspace response: %s", createdWorkspace.Stdout)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		result, cleanupErr := runner.Run(cleanupCtx, "herdr", []string{"workspace", "close", workspace.Result.RootPane.WorkspaceID})
		if cleanupErr != nil {
			t.Errorf("close isolated workspace: %v: %s", cleanupErr, result.Stderr)
		}
	})

	origin := sessionhost.Origin{
		SessionName: sessionName,
		WorkspaceID: workspace.Result.RootPane.WorkspaceID,
		TabID:       workspace.Result.RootPane.TabID,
		PaneID:      workspace.Result.RootPane.PaneID,
	}
	launcher := NewManagedCodexLauncher(Host{Runner: rawRunner, Session: sessionName})
	var created sessionhost.CreatedTarget
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if created.PaneID != "" {
			if cleanupErr := (sessionhost.Host{Runner: rawRunner, Session: sessionName}).Cleanup(cleanupCtx, created); cleanupErr != nil {
				t.Errorf("cleanup ordinary target: %v", cleanupErr)
			}
		}
	})
	prompt, prepareErr := PreparePrompt("Reply exactly OK.")
	if prepareErr != nil {
		t.Fatal(prepareErr)
	}
	result, startErr := NewPromptStarter(Host{Runner: rawRunner, Session: sessionName}).Start(ctx, prompt, PromptStartRequest{
		Origin: origin, Argv: []string{"codex", "--yolo"}, Directory: directory,
		Placement: sessionhost.NewTab, TabLabel: "ordinary-prompted", Focus: false,
		ObserveCodexPolicy: true, ReadyTimeout: 30 * time.Second,
	})
	created, agent, err := result.Target.Created, result.Agent, startErr
	if err == nil && result.State != PromptAcknowledged {
		err = fmt.Errorf("ordinary prompt state=%s", result.State)
	}
	if err != nil {
		t.Logf("last initial binding observations:\n%s", strings.Join(trace, "\n"))
		diagnosticCtx, diagnosticCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer diagnosticCancel()
		for _, args := range [][]string{
			{"agent", "get", created.PaneID},
			{"pane", "process-info", "--pane", created.PaneID},
			{"pane", "read", created.PaneID, "--source", "visible", "--lines", "60"},
		} {
			observed, diagnosticErr := runner.Run(diagnosticCtx, "herdr", args)
			t.Logf("ordinary startup diagnostic %v: %s %s error=%v", args, observed.Stdout, observed.Stderr, diagnosticErr)
		}
		t.Fatal(err)
	}
	if agent.Session == nil || !validManagedSession(agent.Session) || agent.CWD != directory || agent.Focused {
		t.Fatalf("prompted agent=%+v", agent)
	}
	settle := func(pane, answer string) {
		for {
			visible, err := launcher.readPaneANSI(ctx, pane, "visible", 220)
			plain := managedANSI.ReplaceAllString(visible, "")
			found := false
			for _, line := range strings.Split(plain, "\n") {
				found = found || strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "• ")) == answer
			}
			if err == nil && found && emptyCodexComposer(visible) {
				return
			}
			if err := waitManaged(ctx, 50*time.Millisecond); err != nil {
				t.Fatalf("settle disposable reply %q: %v; last visible=%q", answer, err, plain)
			}
		}
	}
	settle(created.PaneID, "OK")
	host := Host{Runner: rawRunner, Session: sessionName}
	source, err := host.find(ctx, created.PaneID)
	if err != nil {
		t.Fatal(err)
	}
	forkPrompt, _ := PreparePrompt("Reply exactly ORDINARY-FORK-OK.")
	forker := NewForker(host)
	forker.managed.discoverObserved = func(ctx context.Context, target sessionhost.Origin, cwd string, argv []string, service CodexServiceSnapshot, policy ManagedCodexPolicy) (Agent, managedStatusObservation, error) {
		agent, observed, err := launcher.discoverManagedIdentityObservation(ctx, target, cwd, argv, service, policy, true)
		if err != nil {
			t.Logf("last discovery observations:\n%s", strings.Join(trace, "\n"))
			t.Logf("fork discovery expected argv=%q: %v", argv, err)
			for _, args := range [][]string{
				{"agent", "get", target.PaneID},
				{"pane", "process-info", "--pane", target.PaneID},
				{"pane", "read", target.PaneID, "--source", "visible", "--lines", "20", "--format", "ansi"},
			} {
				observed, diagnosticErr := runner.Run(ctx, "herdr", args)
				t.Logf("fork diagnostic %v: %s %s error=%v", args, observed.Stdout, observed.Stderr, diagnosticErr)
			}
		}
		return agent, observed, err
	}
	forked, err := forker.Fork(ctx, ForkRequest{
		Source: source, Profile: ForkProfile{Executable: "codex"},
		Placement: sessionhost.NewPane, Direction: "down", Prompt: &forkPrompt,
		ObserveCodexPolicy: true,
	})
	if err != nil || forked.Prompt.State != PromptAcknowledged {
		t.Fatalf("ordinary fork state=%s error=%v", forked.Prompt.State, err)
	}
	if !validManagedSession(forked.Child.Session) || forked.Child.Session.Value == source.Session.Value || forked.Child.TabID != source.TabID || forked.Child.PaneID == source.PaneID {
		t.Fatalf("fork did not create distinct same-tab conversation: %+v", forked)
	}
	settle(forked.Child.PaneID, "ORDINARY-FORK-OK.")
	current, err := host.find(ctx, source.PaneID)
	if err != nil || !sameImmutable(source, current) {
		t.Fatalf("fork source changed: %+v error=%v", current, err)
	}
}
