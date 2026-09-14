package agenthost

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func TestOrdinaryCodexLiveBindsZeroTurnNewThroughSharedDaemon(t *testing.T) {
	if os.Getenv("HERDR_PLUGIN_KIT_MANAGED_CODEX_LIVE") != "1" {
		t.Skip("set HERDR_PLUGIN_KIT_MANAGED_CODEX_LIVE=1 for the Herdr 0.8.2/Codex 0.154.0 integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	runner := command.ExecRunner{}
	directory, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	createdWorkspace, err := runner.Run(ctx, "herdr", []string{"workspace", "create", "--cwd", directory, "--label", fmt.Sprintf("hpk-api5-zero-turn-%d", time.Now().UnixNano()), "--no-focus"})
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
		WorkspaceID: workspace.Result.RootPane.WorkspaceID,
		TabID:       workspace.Result.RootPane.TabID,
		PaneID:      workspace.Result.RootPane.PaneID,
	}
	launcher := NewManagedCodexLauncher(Host{Runner: runner})
	binding, err := beginOrdinaryCodexBinding(ctx, launcher, ManagedNew, Agent{}, directory, managedPolicy(), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var created sessionhost.CreatedTarget
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if created.PaneID != "" {
			if cleanupErr := (sessionhost.Host{Runner: runner}).Cleanup(cleanupCtx, created); cleanupErr != nil {
				t.Errorf("cleanup ordinary target: %v", cleanupErr)
			}
		}
		if cleanupErr := binding.CleanupThread(cleanupCtx); cleanupErr != nil {
			t.Errorf("cleanup zero-turn thread: %v", cleanupErr)
		}
		if closeErr := binding.Close(); closeErr != nil {
			t.Errorf("close verifier: %v", closeErr)
		}
	})
	created, err = (sessionhost.Host{Runner: runner}).Launch(ctx, sessionhost.LaunchRequest{
		Origin: origin, Argv: []string{"codex"}, Directory: directory,
		Placement: sessionhost.NewTab, TabLabel: "ordinary-zero-turn", Focus: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := binding.Finish(ctx, created, directory, []string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	if agent.Session == nil || !validManagedSession(agent.Session) || agent.CWD != directory || agent.Focused {
		t.Fatalf("zero-turn agent=%+v", agent)
	}
}
