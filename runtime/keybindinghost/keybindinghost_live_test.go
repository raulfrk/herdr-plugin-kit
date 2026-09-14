package keybindinghost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

func TestKeybindingTransactionsLiveDefaultAndNamed(t *testing.T) {
	if os.Getenv("HERDR_PLUGIN_KIT_KEYBINDING_LIVE") != "1" {
		t.Skip("set HERDR_PLUGIN_KIT_KEYBINDING_LIVE=1 for the isolated Herdr 0.8.2 integration")
	}
	for _, session := range []string{"", "api8-named"} {
		name := session
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			runLiveKeybindingCase(t, session)
		})
	}
}

func runLiveKeybindingCase(t *testing.T, session string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	root, err := os.MkdirTemp("", "hpk-kb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove live-test root: %v", err)
		}
	})
	xdg, state := filepath.Join(root, "xdg"), filepath.Join(root, "state")
	if err := os.MkdirAll(filepath.Join(xdg, "herdr"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(root, "wrong.sock"))
	t.Setenv("HERDR_SESSION", "wrong")
	t.Setenv("HERDR_CLIENT_SOCKET_PATH", filepath.Join(root, "wrong-client.sock"))
	t.Setenv("HERDR_CONFIG_PATH", filepath.Join(root, "wrong-config.toml"))

	config := filepath.Join(xdg, "herdr", "config.toml")
	baseConfig := []byte("onboarding = false\n\n[update]\nversion_check = false\nmanifest_check = false\n")
	if err := os.WriteFile(config, baseConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	live := startLiveHerdr(t, ctx, xdg, state, config, session)
	id := "proof.default"
	if session != "" {
		id = "proof.named"
	}
	canary := filepath.Join(root, "canary")
	pluginRoot := filepath.Join(root, "plugin")
	writeLivePlugin(t, pluginRoot, id, canary)
	runLiveHerdr(t, ctx, live.env, session, "plugin", "link", pluginRoot, "--enabled")

	live.send(t, "\x02u")
	observeLogCount(t, ctx, live.env, session, id, 0, 100*time.Millisecond)
	host := Host{Runner: command.ExecRunner{}, ConfigPath: config, Session: session}
	applyBindings(t, host, id, []Binding{{Key: "prefix+u", Action: id + ".mark", Description: "Live proof"}})
	live.send(t, "\x02u")
	waitLogCount(t, ctx, live.env, session, id, 1)
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("canary: %v", err)
	}

	applyBindings(t, host, id, []Binding{{Key: "prefix+v", Action: id + ".mark", Description: "Changed live proof"}})
	live.send(t, "\x02u")
	observeLogCount(t, ctx, live.env, session, id, 1, 100*time.Millisecond)
	live.send(t, "\x02v")
	waitLogCount(t, ctx, live.env, session, id, 2)

	applyBindings(t, host, id, nil)
	live.send(t, "\x02v")
	observeLogCount(t, ctx, live.env, session, id, 2, 100*time.Millisecond)
	if got := readFile(t, config); !bytes.Equal(got, baseConfig) {
		t.Fatalf("remove did not restore exact bytes: %q", got)
	}
}

type liveHerdr struct {
	env        []string
	session    string
	server     *exec.Cmd
	serverDone <-chan error
	client     *exec.Cmd
	clientPTY  *os.File
	clientDone <-chan error
	writeMu    sync.Mutex
	outputMu   sync.Mutex
	output     []byte
}

func startLiveHerdr(t *testing.T, ctx context.Context, xdg, state, config, session string) *liveHerdr {
	t.Helper()
	environment := liveEnvironment(xdg, state, config)
	serverArgs := []string{"server"}
	if session != "" {
		serverArgs = append([]string{"--session", session}, serverArgs...)
	}
	server := exec.Command("herdr", serverArgs...)
	server.Env = environment
	var serverOutput bytes.Buffer
	server.Stdout, server.Stderr = &serverOutput, &serverOutput
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Wait() }()
	live := &liveHerdr{env: environment, session: session, server: server, serverDone: serverDone}
	t.Cleanup(func() { live.stop(t) })

	socket := filepath.Join(xdg, "herdr", "herdr.sock")
	if session != "" {
		socket = filepath.Join(xdg, "herdr", "sessions", session, "herdr.sock")
	}
	waitPath(t, ctx, socket)
	t.Logf("API socket ready for session %q", session)
	waitLiveServer(t, ctx, environment, session)
	t.Logf("API ready for session %q", session)
	clientSocket := filepath.Join(xdg, "herdr", "herdr-client.sock")
	if session != "" {
		clientSocket = filepath.Join(xdg, "herdr", "sessions", session, "herdr-client.sock")
	}
	waitPath(t, ctx, clientSocket)
	t.Logf("client socket ready for session %q", session)

	clientArgs := []string{}
	if session != "" {
		clientArgs = []string{"--session", session}
	}
	client := exec.Command("herdr", clientArgs...)
	client.Env = environment
	terminal, err := pty.StartWithSize(client, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("start client: %v; server output: %s", err, serverOutput.String())
	}
	clientDone := make(chan error, 1)
	go func() { clientDone <- client.Wait() }()
	live.client, live.clientPTY, live.clientDone = client, terminal, clientDone
	go live.serveTerminal(terminal)
	live.waitClientAlive(t, ctx)
	t.Logf("PTY client alive for session %q", session)
	return live
}

func (l *liveHerdr) send(t *testing.T, keys string) {
	t.Helper()
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	if _, err := io.WriteString(l.clientPTY, keys); err != nil {
		t.Fatal(err)
	}
}

func (l *liveHerdr) serveTerminal(terminal *os.File) {
	buffer := make([]byte, 4096)
	var pending []byte
	for {
		n, err := terminal.Read(buffer)
		if n > 0 {
			l.outputMu.Lock()
			l.output = append(l.output, buffer[:n]...)
			if len(l.output) > 32<<10 {
				l.output = append([]byte(nil), l.output[len(l.output)-(32<<10):]...)
			}
			l.outputMu.Unlock()
			pending = append(pending, buffer[:n]...)
			for {
				start := bytes.Index(pending, []byte("\x1b]"))
				if start < 0 {
					if len(pending) > 2 {
						pending = pending[len(pending)-2:]
					}
					break
				}
				end := bytes.Index(pending[start+2:], []byte("\x1b\\"))
				if end < 0 {
					pending = pending[start:]
					break
				}
				end += start + 2
				query := string(pending[start+2 : end])
				if response := terminalQueryResponse(query); response != "" {
					l.writeMu.Lock()
					_, _ = io.WriteString(terminal, response)
					l.writeMu.Unlock()
				}
				pending = pending[end+2:]
			}
		}
		if err != nil {
			return
		}
	}
}

func (l *liveHerdr) waitClientAlive(t *testing.T, ctx context.Context) {
	t.Helper()
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-l.clientDone:
		l.clientDone = nil
		l.outputMu.Lock()
		output := append([]byte(nil), l.output...)
		l.outputMu.Unlock()
		t.Fatalf("client exited during startup: %v: %q", err, output)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-timer.C:
	}
}

func terminalQueryResponse(query string) string {
	color := "rgb:0000/0000/0000"
	switch {
	case query == "10;?":
		color = "rgb:ffff/ffff/ffff"
	case query == "11;?":
	case strings.HasPrefix(query, "4;") && strings.HasSuffix(query, ";?"):
	default:
		return ""
	}
	return "\x1b]" + strings.TrimSuffix(query, "?") + color + "\x1b\\"
}

func (l *liveHerdr) stop(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if l.server.Process != nil {
		args := []string{"server", "stop"}
		if l.session != "" {
			args = append([]string{"--session", l.session}, args...)
		}
		cmd := exec.CommandContext(ctx, "herdr", args...)
		cmd.Env = l.env
		if err := cmd.Run(); err != nil {
			t.Errorf("stop Herdr session %q: %v", l.session, err)
		}
	}
	if l.clientPTY != nil {
		_ = l.clientPTY.Close()
	}
	waitProcess := func(name string, process *os.Process, done <-chan error) {
		if done == nil {
			return
		}
		select {
		case <-done:
		case <-ctx.Done():
			if process != nil {
				_ = process.Kill()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Errorf("%s process did not exit", name)
			}
		}
	}
	if l.client != nil {
		waitProcess("client", l.client.Process, l.clientDone)
	}
	if l.server != nil {
		waitProcess("server", l.server.Process, l.serverDone)
	}
}

func liveEnvironment(xdg, state, config string) []string {
	environment := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "HERDR_") || name == "XDG_CONFIG_HOME" || name == "XDG_STATE_HOME" {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, "XDG_CONFIG_HOME="+xdg, "XDG_STATE_HOME="+state, "HERDR_CONFIG_PATH="+config)
}

func runLiveHerdr(t *testing.T, ctx context.Context, environment []string, session string, args ...string) []byte {
	t.Helper()
	if session != "" {
		args = append([]string{"--session", session}, args...)
	}
	cmd := exec.CommandContext(ctx, "herdr", args...)
	cmd.Env = environment
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herdr %q: %v: %s", args, err, output)
	}
	return output
}

func waitLiveServer(t *testing.T, ctx context.Context, environment []string, session string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		args := []string{"status", "server", "--json"}
		if session != "" {
			args = append([]string{"--session", session}, args...)
		}
		cmd := exec.CommandContext(ctx, "herdr", args...)
		cmd.Env = environment
		if output, err := cmd.CombinedOutput(); err == nil {
			var status struct {
				Status     string `json:"status"`
				Running    bool   `json:"running"`
				Version    string `json:"version"`
				Compatible bool   `json:"compatible"`
			}
			if json.Unmarshal(output, &status) == nil && status.Status == "running" && status.Running && status.Version == "0.8.2" && status.Compatible {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for Herdr API session %q: %v", session, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitPath(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for %s: %v", path, ctx.Err())
		case <-ticker.C:
		}
	}
}

func writeLivePlugin(t *testing.T, root, id, canary string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`id = %q
name = "API8 proof"
version = "0.0.0"
min_herdr_version = "0.8.2"
platforms = ["linux"]

[[actions]]
id = "mark"
title = "Mark binding"
contexts = ["global", "workspace", "tab", "pane"]
command = ["/usr/bin/touch", %q]
`, id, canary)
	if err := os.WriteFile(filepath.Join(root, "herdr-plugin.toml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitLogCount(t *testing.T, ctx context.Context, environment []string, session, plugin string, want int) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		total, succeeded := liveLogCounts(t, ctx, environment, session, plugin)
		if total == want && succeeded == want {
			return
		} else if total > want {
			t.Fatalf("%s total log count=%d want=%d", plugin, total, want)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for %s log %d: %v", plugin, want, ctx.Err())
		case <-ticker.C:
		}
	}
}

func observeLogCount(t *testing.T, parent context.Context, environment []string, session, plugin string, want int, duration time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, duration)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if total, _ := liveLogCounts(t, parent, environment, session, plugin); total != want {
			t.Fatalf("%s log count=%d want unchanged=%d", plugin, total, want)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func liveLogCounts(t *testing.T, ctx context.Context, environment []string, session, plugin string) (total, succeeded int) {
	t.Helper()
	output := runLiveHerdr(t, ctx, environment, session, "plugin", "log", "list", "--plugin", plugin, "--limit", "10")
	var response struct {
		ID     string `json:"id"`
		Result struct {
			Type string `json:"type"`
			Logs []struct {
				PluginID string `json:"plugin_id"`
				ActionID string `json:"action_id"`
				Status   string `json:"status"`
				ExitCode int    `json:"exit_code"`
			} `json:"logs"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &response); err != nil || response.ID != "cli:plugin" || response.Result.Type != "plugin_log_list" {
		t.Fatalf("invalid plugin log response: %v: %s", err, output)
	}
	for _, log := range response.Result.Logs {
		if log.PluginID != plugin || log.ActionID != "mark" || (log.Status != "running" && log.Status != "succeeded") || log.ExitCode != 0 {
			t.Fatalf("unexpected plugin log: %+v", log)
		}
		if log.Status == "succeeded" {
			succeeded++
		}
	}
	return len(response.Result.Logs), succeeded
}
