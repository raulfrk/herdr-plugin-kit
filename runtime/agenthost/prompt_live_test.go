package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func TestPromptLiveOrdinaryNewAndForkUseExactNativeConversations(t *testing.T) {
	if os.Getenv("HERDR_PLUGIN_KIT_PROMPT_LIVE") != "1" {
		t.Skip("set HERDR_PLUGIN_KIT_PROMPT_LIVE=1 for the Herdr 0.8.2 supported-harness integration")
	}
	openCodeURL := os.Getenv("HERDR_PLUGIN_KIT_OPENCODE_URL")
	if openCodeURL == "" {
		t.Fatal("HERDR_PLUGIN_KIT_OPENCODE_URL must name the task-owned OpenCode 1.18.30 server")
	}
	openCodeBinary := os.Getenv("HERDR_PLUGIN_KIT_OPENCODE_BIN")
	if !filepath.IsAbs(openCodeBinary) || filepath.Clean(openCodeBinary) != openCodeBinary {
		t.Fatal("HERDR_PLUGIN_KIT_OPENCODE_BIN must name the clean absolute OpenCode 1.18.30 executable")
	}
	version, err := command.ExecRunner{}.Run(context.Background(), openCodeBinary, []string{"--version"})
	if err != nil || strings.TrimSpace(string(version.Stdout)) != "1.18.30" {
		t.Fatalf("HERDR_PLUGIN_KIT_OPENCODE_BIN must report 1.18.30: stdout=%q stderr=%q error=%v", version.Stdout, version.Stderr, err)
	}
	openCodeConfigDirectory := os.Getenv("OPENCODE_CONFIG_DIR")
	if !filepath.IsAbs(openCodeConfigDirectory) || filepath.Clean(openCodeConfigDirectory) != openCodeConfigDirectory {
		t.Fatal("OPENCODE_CONFIG_DIR must be the clean absolute task OpenCode configuration directory inherited by the task server and Herdr session")
	}
	openCodeDataHome := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(openCodeDataHome) || filepath.Clean(openCodeDataHome) != openCodeDataHome {
		t.Fatal("XDG_DATA_HOME must be the clean absolute task OpenCode data root inherited by the task server and Herdr session")
	}
	if os.Getenv("OPENCODE_TUI_CONFIG") != filepath.Join(openCodeConfigDirectory, "tui.jsonc") {
		t.Fatal("OPENCODE_TUI_CONFIG must name the task OpenCode TUI configuration inherited by the task Herdr session")
	}
	for _, relative := range []string{"herdr-tui-session.js", "tui.jsonc"} {
		integration := filepath.Join(openCodeConfigDirectory, relative)
		if info, statErr := os.Stat(integration); statErr != nil || !info.Mode().IsRegular() {
			t.Fatalf("task OpenCode profile must contain the Herdr 0.8.2 identity integration at %s: %v", integration, statErr)
		}
	}
	ompAgentDirectory := os.Getenv("HERDR_PLUGIN_KIT_OMP_AGENT_DIR")
	if !filepath.IsAbs(ompAgentDirectory) || filepath.Clean(ompAgentDirectory) != ompAgentDirectory {
		t.Fatal("HERDR_PLUGIN_KIT_OMP_AGENT_DIR must be the clean absolute task OMP agent directory")
	}
	if os.Getenv("PI_CODING_AGENT_DIR") != ompAgentDirectory {
		t.Fatal("PI_CODING_AGENT_DIR must equal HERDR_PLUGIN_KIT_OMP_AGENT_DIR and be inherited by the task Herdr session")
	}
	herdrOMPExtension := filepath.Join(ompAgentDirectory, "extensions", "herdr-omp-agent-state.ts")
	if info, statErr := os.Stat(herdrOMPExtension); statErr != nil || !info.Mode().IsRegular() {
		t.Fatalf("task OMP profile must contain the Herdr 0.8.2 identity extension at %s: %v", herdrOMPExtension, statErr)
	}
	liveCodexPolicy := ManagedCodexPolicy{
		Model:           os.Getenv("HERDR_PLUGIN_KIT_CODEX_MODEL"),
		ReasoningEffort: os.Getenv("HERDR_PLUGIN_KIT_CODEX_REASONING_EFFORT"),
		ApprovalPolicy:  os.Getenv("HERDR_PLUGIN_KIT_CODEX_APPROVAL_POLICY"),
		SandboxMode:     os.Getenv("HERDR_PLUGIN_KIT_CODEX_SANDBOX_MODE"),
	}
	if err := validateManagedPolicy(liveCodexPolicy); err != nil {
		t.Fatalf("explicit live Codex profile policy is invalid: %v", err)
	}
	ompExtension := filepath.Join(ompAgentDirectory, "extensions", "herdr-plugin-kit-status.ts")
	ompAddressExtension := filepath.Join(ompAgentDirectory, "extensions", "herdr-plugin-kit-status-address.ts")
	if err := os.MkdirAll(filepath.Dir(ompExtension), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ompExtension, OMPStatusExtension(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ompAddressExtension, OMPStatusAddressExtension(), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, extension := range []string{ompExtension, ompAddressExtension} {
			if err := os.Remove(extension); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("remove task OMP extension: %v", err)
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	runner := command.ExecRunner{}
	directory := t.TempDir()
	repositoryDirectory, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	sessionName := os.Getenv("HERDR_PLUGIN_KIT_LIVE_SESSION")
	if sessionName == "" {
		t.Fatal("HERDR_PLUGIN_KIT_LIVE_SESSION must name a task-owned session started with the reviewed OMP extensions")
	}
	liveRunner := liveSessionRunner{Runner: runner, Session: sessionName}
	origin := createPromptLiveWorkspace(t, ctx, liveRunner, directory)
	origin.SessionName = sessionName
	host := Host{Runner: runner, Session: sessionName}
	topology := sessionhost.Host{Runner: runner, Session: sessionName}
	server := OpenCodeServer{URL: openCodeURL, Directory: directory}

	var ompSessions []string
	var openCodeSessions []string
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		result, err := liveRunner.Run(cleanupCtx, "herdr", []string{"workspace", "close", origin.WorkspaceID})
		if err != nil {
			t.Errorf("close prompt live workspace: %v: %s", err, result.Stderr)
		}
		for _, session := range openCodeSessions {
			if err := (nativeOpenCodePromptSessions{}).Delete(cleanupCtx, server, session); err != nil {
				t.Errorf("delete task OpenCode session %s: %v", session, err)
			}
		}
		for _, session := range ompSessions {
			if !filepath.IsAbs(session) || filepath.Clean(session) != session {
				t.Errorf("refuse invalid task OMP session path %q", session)
				continue
			}
			if err := os.Remove(session); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("remove task OMP session %s: %v", session, err)
			}
		}
	})

	tests := []struct {
		kind        string
		argv        []string
		directory   string
		openCode    OpenCodeServer
		codexPolicy ManagedCodexPolicy
	}{
		{kind: "omp", argv: []string{"omp", "--model", "openai-codex/gpt-5.6-sol", "--no-skills", "--no-rules", "--no-tools"}, directory: directory},
		{kind: "opencode", argv: []string{openCodeBinary}, directory: directory, openCode: server},
		{kind: "codex", argv: []string{"codex"}, directory: repositoryDirectory, codexPolicy: liveCodexPolicy},
	}
	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			newCanary := "HPK-API5-NEW-" + strings.ToUpper(test.kind)
			prompt, _ := PreparePrompt("Reply exactly " + newCanary)
			started, err := NewPromptStarter(host).Start(ctx, prompt, PromptStartRequest{
				Origin: origin, Argv: test.argv, Directory: test.directory, Placement: sessionhost.NewTab,
				TabLabel:       "api5-" + test.kind + "-new",
				OpenCodeServer: test.openCode, CodexPolicy: test.codexPolicy, ReadyTimeout: 45 * time.Second,
			})
			if err != nil || started.State != PromptAcknowledged {
				t.Fatalf("new result=%+v error=%v", started, err)
			}
			source := awaitLivePromptResult(t, ctx, host, topology, started, newCanary)
			trackPromptLiveSession(t, source, &ompSessions, &openCodeSessions)

			forkCanary := "HPK-API5-FORK-" + strings.ToUpper(test.kind)
			forkPrompt, _ := PreparePrompt("Reply exactly " + forkCanary)
			forked, err := NewForker(host).Fork(ctx, ForkRequest{
				Source: source, Profile: ForkProfile{Executable: test.argv[0], Options: test.argv[1:]},
				Placement: sessionhost.NewTab, Prompt: &forkPrompt,
				OpenCodeServer: test.openCode, CodexPolicy: test.codexPolicy, ReadyTimeout: 45 * time.Second,
			})
			if err != nil || forked.Prompt.State != PromptAcknowledged {
				t.Fatalf("fork result=%+v error=%v", forked, err)
			}
			child := awaitLivePromptResult(t, ctx, host, topology, forked.Prompt, forkCanary)
			trackPromptLiveSession(t, child, &ompSessions, &openCodeSessions)
			sourceHistory, err := (ManagedCodexLauncher{Host: host}).readFullPaneHistory(ctx, sessionhost.Origin{
				SessionName: source.SessionName, WorkspaceID: source.WorkspaceID, TabID: source.TabID, PaneID: source.PaneID,
			})
			if err != nil || strings.Contains(sourceHistory, forkCanary) {
				t.Fatalf("fork prompt reached source: contains=%t error=%v", strings.Contains(sourceHistory, forkCanary), err)
			}
		})
	}
}

func TestPromptLiveOwnedOpenCodeServerStopsAndConversationContinues(t *testing.T) {
	if os.Getenv("HERDR_PLUGIN_KIT_PROMPT_LIVE") != "1" {
		t.Skip("set HERDR_PLUGIN_KIT_PROMPT_LIVE=1 for the Herdr 0.8.2 supported-harness integration")
	}
	executable := os.Getenv("HERDR_PLUGIN_KIT_OPENCODE_BIN")
	version, err := command.ExecRunner{}.Run(context.Background(), executable, []string{"--version"})
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable || err != nil || strings.TrimSpace(string(version.Stdout)) != SupportedOpenCodeVersion {
		t.Fatalf("HERDR_PLUGIN_KIT_OPENCODE_BIN must name OpenCode %s: stdout=%q error=%v", SupportedOpenCodeVersion, version.Stdout, err)
	}
	configDirectory := os.Getenv("OPENCODE_CONFIG_DIR")
	dataHome := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(configDirectory) || filepath.Clean(configDirectory) != configDirectory || !filepath.IsAbs(dataHome) || filepath.Clean(dataHome) != dataHome || os.Getenv("OPENCODE_TUI_CONFIG") != filepath.Join(configDirectory, "tui.jsonc") {
		t.Fatal("owned OpenCode live test requires isolated absolute config/data paths and exact TUI config")
	}
	for _, relative := range []string{"herdr-tui-session.js", "tui.jsonc"} {
		if info, statErr := os.Stat(filepath.Join(configDirectory, relative)); statErr != nil || !info.Mode().IsRegular() {
			t.Fatalf("missing task OpenCode identity integration %s: %v", relative, statErr)
		}
	}
	sessionName := os.Getenv("HERDR_PLUGIN_KIT_LIVE_SESSION")
	if sessionName == "" {
		t.Fatal("HERDR_PLUGIN_KIT_LIVE_SESSION must name the task-owned session")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runner := command.ExecRunner{}
	liveRunner := liveSessionRunner{Runner: runner, Session: sessionName}
	directory := t.TempDir()
	origin := createPromptLiveWorkspace(t, ctx, liveRunner, directory)
	origin.SessionName = sessionName
	host := Host{Runner: runner, Session: sessionName}
	topology := sessionhost.Host{Runner: runner, Session: sessionName}
	var createdSession string
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		_, workspaceErr := liveRunner.Run(cleanupCtx, "herdr", []string{"workspace", "close", origin.WorkspaceID})
		if workspaceErr != nil {
			t.Errorf("close owned-server workspace: %v", workspaceErr)
		}
		if createdSession == "" {
			return
		}
		launcher := nativeOpenCodePromptServerLauncher{runner: runner}
		server, stop, startErr := launcher.Start(cleanupCtx, executable, directory)
		if startErr != nil {
			t.Errorf("restart cleanup OpenCode server: %v", startErr)
			return
		}
		if deleteErr := (nativeOpenCodePromptSessions{}).Delete(cleanupCtx, server, createdSession); deleteErr != nil {
			t.Errorf("delete owned-server OpenCode session: %v", deleteErr)
		}
		if stopErr := stop(cleanupCtx); stopErr != nil {
			t.Errorf("stop cleanup OpenCode server: %v", stopErr)
		}
	})
	prompt, _ := PreparePrompt("Reply exactly HPK-API15-OWNED-SERVER")
	started, err := NewPromptStarter(host).Start(ctx, prompt, PromptStartRequest{
		Origin: origin, Argv: []string{executable}, Directory: directory,
		Placement: sessionhost.NewTab, TabLabel: "api15-owned-opencode", ReadyTimeout: 45 * time.Second,
		StartOpenCodeServer: true,
	})
	if err != nil || started.State != PromptAcknowledged || started.Agent.Session == nil {
		t.Fatalf("owned-server start=%+v error=%v", started, err)
	}
	createdSession = started.Agent.Session.Value
	before := awaitLivePromptResult(t, ctx, host, topology, started, "HPK-API15-OWNED-SERVER")
	if err := readOpenCodePromptServerHealth(ctx, started.Target.OpenCodeServer.URL); err == nil {
		t.Fatal("task-owned OpenCode server remained reachable after prompt")
	}
	second := "Reply exactly HPK-API15-AFTER-SERVER-STOP"
	if result, promptErr := liveRunner.Run(ctx, "herdr", []string{"agent", "prompt", before.PaneID, second, "--wait", "--timeout", "45000"}); promptErr != nil {
		t.Fatalf("prompt after owned server stop: %v: %s", promptErr, result.Stderr)
	}
	after, err := host.find(ctx, before.PaneID)
	if err != nil || !sameImmutable(before, after) || after.Session == nil || after.Session.Value != createdSession {
		t.Fatalf("conversation changed after owned server stop: before=%+v after=%+v error=%v", before, after, err)
	}
	if _, err := topology.VerifyForegroundProcess(ctx, started.Target.Created.Origin, directory, started.Target.Argv); err != nil {
		t.Fatalf("OpenCode process drift after owned server stop: %v", err)
	}
}

func createPromptLiveWorkspace(t *testing.T, ctx context.Context, runner command.Runner, directory string) sessionhost.Origin {
	t.Helper()
	created, err := runner.Run(ctx, "herdr", []string{"workspace", "create", "--cwd", directory, "--label", "hpk-api5-prompt-live", "--no-focus"})
	if err != nil {
		t.Fatalf("create prompt live workspace: %v: %s", err, created.Stderr)
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
	if err := json.Unmarshal(created.Stdout, &response); err != nil || response.ID != "cli:workspace:create" || response.Result.Type != "workspace_created" || response.Result.RootPane.PaneID == "" || response.Result.RootPane.WorkspaceID == "" || response.Result.RootPane.TabID == "" {
		t.Fatalf("unexpected workspace response: %s", created.Stdout)
	}
	return sessionhost.Origin{WorkspaceID: response.Result.RootPane.WorkspaceID, TabID: response.Result.RootPane.TabID, PaneID: response.Result.RootPane.PaneID}
}

func awaitLivePromptResult(t *testing.T, ctx context.Context, host Host, topology sessionhost.Host, result PromptResult, canary string) Agent {
	t.Helper()
	for {
		agent, err := host.find(ctx, result.Agent.PaneID)
		if err == nil && sameImmutable(result.Agent, agent) && !agent.Focused && promptReadyStatus(agent.Status) {
			history, readErr := (ManagedCodexLauncher{Host: host}).readFullPaneHistory(ctx, result.Target.Created.Origin)
			if readErr == nil && strings.Count(history, canary) >= 1 {
				if _, processErr := topology.VerifyForegroundProcess(ctx, result.Target.Created.Origin, result.Target.Directory, result.Target.Argv); processErr != nil {
					t.Fatalf("prompt target process drift: %v", processErr)
				}
				return agent
			}
		}
		if err := waitManaged(ctx, 100*time.Millisecond); err != nil {
			t.Fatalf("wait for prompt %q: %v", canary, err)
		}
	}
}

func trackPromptLiveSession(t *testing.T, agent Agent, ompSessions, openCodeSessions *[]string) {
	t.Helper()
	if agent.Session == nil {
		t.Fatalf("prompt target has no session: %+v", agent)
	}
	switch strings.ToLower(agent.Kind) {
	case "omp":
		*ompSessions = append(*ompSessions, agent.Session.Value)
	case "opencode":
		*openCodeSessions = append(*openCodeSessions, agent.Session.Value)
	case "codex":
		// Ordinary Codex is not owned by the shared server. Preserve its
		// conversation in the task CODEX_HOME; cleanup owns only its topology.
	default:
		t.Fatalf("unexpected prompt harness %q", agent.Kind)
	}
}
