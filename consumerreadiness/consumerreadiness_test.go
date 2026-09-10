package consumerreadiness_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/config"
	"github.com/raulfrk/herdr-plugin-kit/documentstore"
	"github.com/raulfrk/herdr-plugin-kit/manifest"
	"github.com/raulfrk/herdr-plugin-kit/runtime/actionhost"
	"github.com/raulfrk/herdr-plugin-kit/runtime/agenthost"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/interop"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
	"github.com/raulfrk/herdr-plugin-kit/ui/interaction"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymapui"
)

func TestConsumerReadiness(t *testing.T) {
	t.Run("Recall", testRecall)
	t.Run("Codex Recall", testCodexRecall)
	t.Run("Attention Switcher", testAttentionSwitcher)
	t.Run("Session Switcher", testSessionSwitcher)
	t.Run("Plugin Configurator", testPluginConfigurator)
	t.Run("Action Finder", testActionFinder)
	t.Run("Shared plugin shortcuts", testSharedPluginShortcuts)
}

func testRecall(t *testing.T) {
	firstCursor := interaction.NewCursor("provider:v1/page/2?opaque=true")
	secondCursor := interaction.NewCursor("provider:v1/page/3?opaque=true")
	pages := map[string]interaction.Page{
		"": {
			Items: []interaction.Item{{Key: "recent", Label: "Résumé"}, {Key: "resume", Label: "Resume"}},
			Next:  firstCursor,
		},
		firstCursor.Token(): {
			Items: []interaction.Item{{Key: "tie-b", Label: "Alpha"}, {Key: "tie-a", Label: "Alpha"}},
			Next:  secondCursor,
		},
		secondCursor.Token(): {Items: []interaction.Item{{Key: "final", Label: "東京 session"}}},
	}

	var loaded []string
	cursor := interaction.Cursor{}
	for {
		page, ok := pages[cursor.Token()]
		if !ok {
			t.Fatalf("provider received decoded or changed cursor %q", cursor.Token())
		}
		for _, item := range page.Items {
			loaded = append(loaded, item.Key)
		}
		if page.Next.Empty() {
			break
		}
		cursor = page.Next
	}
	if want := []string{"recent", "resume", "tie-b", "tie-a", "final"}; !reflect.DeepEqual(loaded, want) {
		t.Fatalf("paged provider order = %v, want %v", loaded, want)
	}

	ranked := interaction.Rank("alp", []interaction.Candidate{{Key: "tie-b", Text: "Alpha"}, {Key: "tie-a", Text: "Alpha"}})
	if len(ranked) != 2 || ranked[0].Candidate.Key != "tie-b" || ranked[1].Candidate.Key != "tie-a" {
		t.Fatalf("equal-score provider order was not stable: %+v", ranked)
	}
}

func testCodexRecall(t *testing.T) {
	target := interop.ActionTarget{
		PluginID: "codex.recall", ActionID: "recall-latest",
		Interface: "recall.query", InterfaceVersion: 1, Method: "latest",
	}
	pluginManifest := manifest.Manifest{
		PluginID: target.PluginID, Name: "Codex Recall", Version: "0.1.0", Executable: "bin/codex-recall",
		Actions:    []manifest.Action{{ID: target.ActionID, Title: "Recall latest"}},
		Interfaces: []manifest.Interface{{ID: target.Interface, Version: target.InterfaceVersion, Direction: manifest.InterfaceProvides}},
	}
	if err := pluginManifest.Validate(); err != nil {
		t.Fatalf("consumer manifest does not express its action/interface contract: %v", err)
	}

	actionID := target.ActionID
	stdout := `{"version":1,"interface":"recall.query","interface_version":1,"method":"latest","payload":{"session_id":"session-42"}}`
	transport := &actionTransportFixture{
		actions: []actionhost.Action{{PluginID: target.PluginID, ActionID: target.ActionID, Title: "Recall latest", Command: []string{"codex-recall", "action"}}},
		terminal: actionhost.Receipt{
			LogID: "receipt:opaque/42", PluginID: target.PluginID, ActionID: &actionID,
			Command: []string{"codex-recall", "action"}, Status: actionhost.Succeeded, Stdout: &stdout,
		},
	}
	result, err := (interop.ActionClient{Host: transport, PollInterval: time.Millisecond}).Invoke(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(result.Response.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if result.LogID != "receipt:opaque/42" || payload.SessionID != "session-42" {
		t.Fatalf("typed interop result = %+v, payload = %+v", result, payload)
	}
}

func testAttentionSwitcher(t *testing.T) {
	stable := agentFixture("done", 1)
	changed := agentFixture("working", 2)
	runner := &queueRunner{outputs: [][]byte{
		agentListJSON(t, stable), agentInfoJSON(t, stable),
		agentListJSON(t, stable), []byte("Ask Codex"), agentListJSON(t, stable),
		agentListJSON(t, changed), []byte("process running"), agentListJSON(t, changed),
	}}
	host := agenthost.Host{Runner: runner, Herdr: "/opt/herdr", Session: "work"}
	agents, err := host.List(context.Background())
	if err != nil || len(agents) != 1 || agents[0].SessionName != "work" {
		t.Fatalf("agents = %+v, error = %v", agents, err)
	}
	focused, err := host.Focus(context.Background(), agents[0])
	if err != nil || !focused.Focused || focused.PaneID != agents[0].PaneID {
		t.Fatalf("focused = %+v, error = %v", focused, err)
	}
	assessment, err := agenthost.NewProbe(host).Assess(context.Background(), focused)
	if !errors.Is(err, agenthost.ErrStaleReport) || assessment.Status != agenthost.Unknown || assessment.Reason != agenthost.Unsettled || assessment.Stable {
		t.Fatalf("assessment = %+v, error = %v", assessment, err)
	}
}

func testSessionSwitcher(t *testing.T) {
	runner := &queueRunner{outputs: [][]byte{
		[]byte(`{"sessions":[{"name":"work","default":false,"running":true,"session_dir":"/tmp/work","socket_path":"/tmp/work/socket"}]}`),
		nil,
		[]byte(`{"id":"create-1","result":{"type":"workspace_created","workspace":{"workspace_id":"ws-42","number":1,"label":"kit","focused":true,"pane_count":1,"tab_count":1,"active_tab_id":"tab-1","agent_status":"unknown"},"tab":{},"root_pane":{}}}`),
	}}
	host := sessionhost.Host{Runner: runner, Herdr: "/opt/herdr"}
	sessions, err := host.List(context.Background())
	if err != nil || len(sessions) != 1 || sessions[0].Name != "work" || !sessions[0].Running {
		t.Fatalf("sessions = %+v, error = %v", sessions, err)
	}
	workspace, err := host.OpenAtDirectory(context.Background(), "review", "/tmp/plugin-kit")
	if err != nil || workspace.WorkspaceID != "ws-42" {
		t.Fatalf("workspace = %+v, error = %v", workspace, err)
	}
	wantCalls := [][]string{
		{"/opt/herdr", "session", "list", "--json"},
		{"/opt/herdr", "--session", "review"},
		{"/opt/herdr", "--session", "review", "workspace", "create", "--cwd", "/tmp/plugin-kit"},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("named-session command composition = %q, want %q", runner.calls, wantCalls)
	}

	stable := agentFixture("done", 1)
	agentRunner := &queueRunner{outputs: [][]byte{
		agentListJSON(t, stable),
		agentListJSON(t, stable), []byte("Ask Codex"), agentListJSON(t, stable),
		agentListJSON(t, stable), []byte("Ask Codex"), agentListJSON(t, stable),
	}}
	agentHost := agenthost.Host{Runner: agentRunner, Herdr: "/opt/herdr", Session: "review"}
	agents, err := agentHost.List(context.Background())
	if err != nil || len(agents) != 1 {
		t.Fatalf("session agents = %+v, error = %v", agents, err)
	}
	assessment, err := agenthost.NewProbe(agentHost).Assess(context.Background(), agents[0])
	if err != nil || !assessment.Stable || assessment.Status != agenthost.Done || assessment.Reason != agenthost.HostReport {
		t.Fatalf("session agent assessment = %+v, error = %v", assessment, err)
	}
}

func testPluginConfigurator(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the secure document store is currently supported on Linux")
	}
	type settings struct {
		Theme string `toml:"theme"`
	}
	loader := config.Loader[settings]{Validate: func(value settings) error {
		if value.Theme != "light" && value.Theme != "dark" {
			return fmt.Errorf("unsupported theme %q", value.Theme)
		}
		return nil
	}}

	root := t.TempDir()
	store, err := documentstore.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil && !errors.Is(err, documentstore.ErrClosed) {
			t.Errorf("close document store: %v", err)
		}
	})
	ctx := context.Background()
	const name = "plugin.toml"
	light := []byte("theme = 'light'\n")
	dark := []byte("theme = 'dark'\n")
	revision, err := store.Write(ctx, name, light, documentstore.Revision{})
	if err != nil {
		t.Fatal(err)
	}
	document, err := store.Read(ctx, name)
	if err != nil || !bytes.Equal(document.Bytes, light) || document.Revision != revision {
		t.Fatalf("checked create/read = %+v, error = %v", document, err)
	}
	if _, err := store.Write(ctx, name, dark, documentstore.Revision{}); !errors.Is(err, documentstore.ErrConflict) {
		t.Fatalf("unchecked overwrite error = %v, want conflict", err)
	}

	updates := make(chan config.Update[settings], 4)
	var currentMu sync.RWMutex
	current := settings{Theme: "light"}
	watcher, err := loader.Watch(ctx, filepath.Join(root, name), config.WatchOptions{PollInterval: 5 * time.Millisecond, Debounce: 5 * time.Millisecond}, func(update config.Update[settings]) {
		if update.Err == nil {
			currentMu.Lock()
			current = update.Value
			currentMu.Unlock()
		}
		updates <- update
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		watcher.Stop()
		<-watcher.Done()
	})

	invalidRevision, err := store.Write(ctx, name, []byte("theme = 'neon'\n"), revision)
	if err != nil {
		t.Fatal(err)
	}
	invalid := awaitUpdate(t, updates)
	if invalid.Err == nil {
		t.Fatal("invalid live configuration was applied")
	}
	currentMu.RLock()
	currentTheme := current.Theme
	currentMu.RUnlock()
	if currentTheme != "light" {
		t.Fatalf("invalid reload changed last valid state to %q", currentTheme)
	}

	darkRevision, err := store.Write(ctx, name, dark, invalidRevision)
	if err != nil {
		t.Fatal(err)
	}
	valid := awaitUpdate(t, updates)
	if valid.Err != nil || valid.Value.Theme != "dark" {
		t.Fatalf("valid update = %+v", valid)
	}
	if _, err := store.Write(ctx, name, light, darkRevision); err != nil {
		t.Fatal(err)
	}
	rolledBack := awaitUpdate(t, updates)
	currentMu.RLock()
	currentTheme = current.Theme
	currentMu.RUnlock()
	if rolledBack.Err != nil || rolledBack.Value.Theme != "light" || currentTheme != "light" {
		t.Fatalf("rollback update = %+v, current theme = %q", rolledBack, currentTheme)
	}
}

func testActionFinder(t *testing.T) {
	actionID := "open"
	exitCode := 0
	finished := uint64(20)
	running := actionhost.Receipt{LogID: "log/opaque-7", PluginID: "recall.plugin", ActionID: &actionID, Command: []string{"recall", "open"}, Status: actionhost.Running, StartedUnixMS: 10}
	succeeded := running
	succeeded.Status, succeeded.ExitCode, succeeded.FinishedUnixMS = actionhost.Succeeded, &exitCode, &finished
	action := actionhost.Action{PluginID: "recall.plugin", ActionID: actionID, Title: "Open recall", Command: []string{"recall", "open"}}
	runner := &queueRunner{outputs: [][]byte{
		marshalJSON(t, map[string]any{"id": "list", "result": map[string]any{"type": "plugin_action_list", "actions": []actionhost.Action{action}}}),
		marshalJSON(t, map[string]any{"id": "invoke", "result": map[string]any{"type": "plugin_action_invoked", "action": action, "context": map[string]any{}, "log": running}}),
		marshalJSON(t, map[string]any{"id": "logs", "result": map[string]any{"type": "plugin_log_list", "logs": []actionhost.Receipt{succeeded}}}),
	}}
	host := actionhost.Host{Runner: runner, Herdr: "/opt/herdr"}
	actions, err := host.List(context.Background(), "recall.plugin")
	if err != nil || len(actions) != 1 || actions[0].ActionID != actionID {
		t.Fatalf("actions = %+v, error = %v", actions, err)
	}
	receipt, err := host.Invoke(context.Background(), "recall.plugin", actionID)
	if err != nil || receipt.LogID != "log/opaque-7" || receipt.Terminal() {
		t.Fatalf("initial receipt = %+v, error = %v", receipt, err)
	}
	receipt, err = host.Receipt(context.Background(), "recall.plugin", actionID, receipt.LogID)
	if err != nil || receipt.LogID != "log/opaque-7" || !receipt.Terminal() || receipt.Status != actionhost.Succeeded {
		t.Fatalf("terminal receipt = %+v, error = %v", receipt, err)
	}

	stdout := `{"version":1,"interface":"actions.run","interface_version":1,"method":"invoke","payload":{"ok":true}}`
	target := interop.ActionTarget{
		PluginID: action.PluginID, ActionID: action.ActionID,
		Interface: "actions.run", InterfaceVersion: 1, Method: "invoke",
	}
	typedReceipt := succeeded
	typedReceipt.Stdout = &stdout
	result, err := (interop.ActionClient{
		Host:         &actionTransportFixture{actions: []actionhost.Action{action}, terminal: typedReceipt},
		PollInterval: time.Millisecond,
	}).Invoke(context.Background(), target)
	if err != nil || result.LogID != receipt.LogID || result.Response.Method != "invoke" {
		t.Fatalf("typed action result = %+v, error = %v", result, err)
	}
	capabilities := interop.SupportedActionCapabilities()
	if capabilities.RequestEnvelope || capabilities.CallerCorrelation {
		t.Fatalf("unsupported dynamic action inputs were advertised: %+v", capabilities)
	}
}

type queueRunner struct {
	mu      sync.Mutex
	outputs [][]byte
	calls   [][]string
}

func (runner *queueRunner) Run(_ context.Context, executable string, args []string) (command.Result, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.calls = append(runner.calls, append([]string{executable}, args...))
	if len(runner.outputs) == 0 {
		return command.Result{}, errors.New("unexpected command")
	}
	output := runner.outputs[0]
	runner.outputs = runner.outputs[1:]
	return command.Result{Stdout: output, ExitCode: 0}, nil
}

type actionTransportFixture struct {
	actions  []actionhost.Action
	terminal actionhost.Receipt
}

func (fixture *actionTransportFixture) List(context.Context, string) ([]actionhost.Action, error) {
	return append([]actionhost.Action(nil), fixture.actions...), nil
}

func (fixture *actionTransportFixture) Invoke(_ context.Context, _, _ string) (actionhost.Receipt, error) {
	actionID := fixture.terminal.ActionID
	return actionhost.Receipt{
		LogID: fixture.terminal.LogID, PluginID: fixture.terminal.PluginID, ActionID: actionID,
		Command: append([]string(nil), fixture.terminal.Command...), Status: actionhost.Running, StartedUnixMS: 1,
	}, nil
}

func (fixture *actionTransportFixture) AwaitReceipt(context.Context, actionhost.Receipt, time.Duration) (actionhost.Receipt, error) {
	return fixture.terminal, nil
}

func agentFixture(status string, sequence uint64) map[string]any {
	return map[string]any{
		"agent": "claude", "agent_status": status, "focused": true,
		"pane_id": "ws-1:pane-1", "revision": 1, "state_change_seq": sequence,
		"tab_id": "tab-1", "workspace_id": "ws-1", "cwd": "/tmp/plugin-kit",
	}
}

func agentListJSON(t *testing.T, agent map[string]any) []byte {
	t.Helper()
	return marshalJSON(t, map[string]any{"id": "agents", "result": map[string]any{"type": "agent_list", "agents": []any{agent}}})
}

func agentInfoJSON(t *testing.T, agent map[string]any) []byte {
	t.Helper()
	return marshalJSON(t, map[string]any{"id": "agent", "result": map[string]any{"type": "agent_info", "agent": agent}})
}

func marshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func awaitUpdate[T any](t *testing.T, updates <-chan config.Update[T]) config.Update[T] {
	t.Helper()
	select {
	case update := <-updates:
		return update
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for configuration update")
		return config.Update[T]{}
	}
}

func testSharedPluginShortcuts(t *testing.T) {
	picker, err := interaction.NewPicker(interaction.PickerOptions{Title: "Example", Load: func(context.Context, string, interaction.Cursor) (interaction.Page, error) {
		return interaction.Page{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	surface, err := keymapui.New(keymapui.Options{Main: picker, ConfigDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer surface.Close()
	document := keymap.Document{Version: 1, Bindings: []keymap.Binding{{Context: "interaction.picker.results", Action: "picker.next", Sequences: []keymap.Sequence{{"g", "r"}}}}}
	if err := surface.Runtime().Apply(document); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if id, _ := surface.Runtime().Step(surface.KeymapContext(), "j", now); id != "" {
		t.Fatal("cleared default still resolves")
	}
	if id, consumed := surface.Runtime().Step(surface.KeymapContext(), "g", now); id != "" || !consumed {
		t.Fatal("sequence prefix not retained")
	}
	if id, _ := surface.Runtime().Step(surface.KeymapContext(), "r", now); id != "picker.next" {
		t.Fatalf("sequence action = %q", id)
	}
}
