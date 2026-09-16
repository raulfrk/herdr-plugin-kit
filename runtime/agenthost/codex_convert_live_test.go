package agenthost

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func TestCodexConverterLiveAutomaticAndConfirmedCustomPreserveConversation(t *testing.T) {
	if os.Getenv("HERDR_PLUGIN_KIT_MANAGED_CODEX_LIVE") != "1" {
		t.Skip("set HERDR_PLUGIN_KIT_MANAGED_CODEX_LIVE=1 for the Herdr 0.8.2/Codex 0.154.0 integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	runner := command.ExecRunner{}
	sessionName := os.Getenv("HERDR_PLUGIN_KIT_LIVE_SESSION")
	var liveRunner command.Runner = runner
	if sessionName != "" {
		liveRunner = liveSessionRunner{Runner: runner, Session: sessionName}
	}
	directory, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	focusBefore, err := liveFocusedWorkspace(ctx, liveRunner)
	if err != nil {
		t.Fatal(err)
	}
	launcher := newManagedLiveLauncher(t, Host{Runner: runner, Session: sessionName})
	converter := NewCodexConverter(launcher.Host)
	converter.launcher = launcher
	service, err := launcher.serviceClient().Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, sandbox string
		custom        *ManagedCodexPolicy
	}{
		{name: "automatic", sandbox: "read-only"},
		{name: "confirmed-custom", sandbox: "danger-full-access", custom: &ManagedCodexPolicy{Model: "gpt-5.6-sol", ReasoningEffort: "high", ApprovalPolicy: "on-request", SandboxMode: "read-only"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			shell, createErr := createLiveConversionWorkspace(ctx, liveRunner, sessionName, directory)
			if createErr != nil {
				t.Fatal(createErr)
			}
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cleanupCancel()
				result, cleanupErr := liveRunner.Run(cleanupCtx, "herdr", []string{"workspace", "close", shell.WorkspaceID})
				if cleanupErr != nil {
					t.Errorf("close conversion workspace: %v: %s", cleanupErr, result.Stderr)
				}
			})
			sourcePolicy := ManagedCodexPolicy{Model: "gpt-5.6-sol", ReasoningEffort: "medium", ApprovalPolicy: "never", SandboxMode: test.sandbox}
			argv := []string{"codex", "-m", sourcePolicy.Model, "-c", "model_reasoning_effort=" + strconv.Quote(sourcePolicy.ReasoningEffort), "-a", sourcePolicy.ApprovalPolicy, "-s", sourcePolicy.SandboxMode, "--strict-config", "--cd", directory, "Reply exactly OK."}
			attempted, err := launcher.topologyHost().RunInShell(ctx, shell, directory, argv)
			if err != nil || !attempted {
				t.Fatalf("launch ordinary Codex attempted=%t error=%v", attempted, err)
			}
			process, err := launcher.waitForProcess(ctx, shell, directory, argv, 35*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			agent, err := awaitLiveOrdinaryCodex(ctx, launcher, shell, directory, argv, sourcePolicy)
			if err != nil {
				t.Fatal(err)
			}
			current, err := launcher.Host.find(ctx, shell.PaneID)
			if err != nil {
				t.Fatal(err)
			}
			verifier, err := newCodexLifecycle(ctx, service.SocketPath)
			if err != nil {
				t.Fatal(err)
			}
			history, err := verifier.ReadStableHistory(ctx, agent.Session.Value)
			if err != nil {
				verifier.Close()
				t.Fatal(err)
			}
			request := CodexConversionRequest{Source: current, ReadyTimeout: 35 * time.Second}
			if test.custom != nil {
				disclosure, prepareErr := converter.PrepareCustomization(ctx, current, *test.custom, 35*time.Second)
				if prepareErr != nil {
					verifier.Close()
					t.Fatal(prepareErr)
				}
				request.Customization = &CodexConversionCustomization{Desired: disclosure.Desired, ConfirmationToken: disclosure.ConfirmationToken}
			}
			converted, err := converter.Convert(ctx, request)
			if err != nil {
				verifier.Close()
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cleanupCancel()
				if converted.Managed != nil {
					_ = converted.Managed.Cleanup(cleanupCtx)
				}
			})
			wantArgv := []string{"codex", "resume", "--remote", "unix://" + service.SocketPath, "--cd", directory, agent.Session.Value}
			checkedHistory, historyErr := verifier.ReadStableHistory(ctx, agent.Session.Value)
			effective, policyErr := verifier.ReadEffective(ctx, agent.Session.Value)
			closeErr := verifier.Close()
			customMismatch := test.custom != nil && !matchesRequestedPolicy(effective, *test.custom)
			if historyErr != nil || policyErr != nil || closeErr != nil || checkedHistory != history || converted.Outcome != CodexConversionManaged || converted.ThreadID != agent.Session.Value || converted.Service != service || converted.Process.PID == process.PID || !reflect.DeepEqual(converted.Argv, wantArgv) || requiredPolicyDrift(effective, converted.Policy) != "" || customMismatch {
				t.Fatalf("converted=%+v effective=%+v historyErr=%v policyErr=%v closeErr=%v", converted, effective, historyErr, policyErr, closeErr)
			}
		})
	}
	focusAfter, err := liveFocusedWorkspace(ctx, liveRunner)
	if err != nil || focusAfter != focusBefore {
		t.Fatalf("focus changed: before=%q after=%q error=%v", focusBefore, focusAfter, err)
	}
	afterService, err := launcher.serviceClient().Ensure(ctx)
	if err != nil || afterService != service {
		t.Fatalf("service changed: before=%+v after=%+v error=%v", service, afterService, err)
	}
}

func createLiveConversionWorkspace(ctx context.Context, runner command.Runner, sessionName, directory string) (sessionhost.Origin, error) {
	created, err := runner.Run(ctx, "herdr", []string{"workspace", "create", "--cwd", directory, "--label", fmt.Sprintf("hpk-kvw-live-%d", time.Now().UnixNano()), "--no-focus"})
	if err != nil {
		return sessionhost.Origin{}, fmt.Errorf("create conversion workspace: %w: %s", err, created.Stderr)
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
	if json.Unmarshal(created.Stdout, &workspace) != nil || workspace.ID != "cli:workspace:create" || workspace.Result.Type != "workspace_created" || workspace.Result.RootPane.PaneID == "" || workspace.Result.RootPane.WorkspaceID == "" || workspace.Result.RootPane.TabID == "" {
		return sessionhost.Origin{}, fmt.Errorf("unexpected conversion workspace response: %s", created.Stdout)
	}
	return sessionhost.Origin{SessionName: sessionName, WorkspaceID: workspace.Result.RootPane.WorkspaceID, TabID: workspace.Result.RootPane.TabID, PaneID: workspace.Result.RootPane.PaneID}, nil
}

func awaitLiveOrdinaryCodex(ctx context.Context, launcher ManagedCodexLauncher, target sessionhost.Origin, directory string, argv []string, policy ManagedCodexPolicy) (Agent, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var prior Agent
	for {
		wire, agent, err := launcher.readManagedAgent(ctx, target, directory)
		_, processErr := launcher.topologyHost().VerifyForegroundProcess(ctx, target, directory, argv)
		visible, readErr := launcher.readPaneANSI(ctx, target.PaneID, "visible", managedStatusLines)
		// The standalone fixture intentionally does not install a hook or
		// autoattach ordinary Codex. Wait for the actual inline reply before
		// using the same guarded status discovery available to consumers.
		if err == nil && processErr == nil && readErr == nil && managedPaneQuiescent(wire) && agent.Session == nil && emptyCodexComposer(visible) {
			for _, line := range strings.Split(managedANSI.ReplaceAllString(visible, ""), "\n") {
				if strings.TrimSpace(line) == "• OK" {
					observed, id, discoverErr := launcher.discoverManagedIdentity(ctx, target, directory, argv, CodexServiceSnapshot{}, policy)
					if discoverErr != nil {
						return Agent{}, discoverErr
					}
					return launcher.bindManagedIdentity(ctx, target, directory, observed, id)
				}
			}
		}
		if err == nil && processErr == nil && readErr == nil && !wire.Focused && !wire.LaunchPending && managedPaneQuiescent(wire) && agent.Session != nil && validManagedSession(agent.Session) && sameManagedTarget(agent, target, directory, false, agent.Session.Value) && emptyCodexComposer(visible) {
			if prior.Session != nil && sameImmutable(prior, agent) && *prior.Session == *agent.Session {
				return agent, nil
			}
			prior = agent
		} else {
			prior = Agent{}
		}
		if err := waitManaged(ctx, 50*time.Millisecond); err != nil {
			return Agent{}, fmt.Errorf("wait for completed ordinary turn and pane identity: %w", err)
		}
	}
}

func liveFocusedWorkspace(ctx context.Context, runner command.Runner) (string, error) {
	result, err := runner.Run(ctx, "herdr", []string{"api", "snapshot"})
	if err != nil {
		return "", err
	}
	var response struct {
		ID     string `json:"id"`
		Result struct {
			Type     string `json:"type"`
			Snapshot struct {
				FocusedWorkspaceID string `json:"focused_workspace_id"`
			} `json:"snapshot"`
		} `json:"result"`
	}
	if json.Unmarshal(result.Stdout, &response) != nil || response.ID != "cli:api:snapshot" || response.Result.Type != "session_snapshot" || response.Result.Snapshot.FocusedWorkspaceID == "" {
		return "", ErrManagedStale
	}
	return response.Result.Snapshot.FocusedWorkspaceID, nil
}
