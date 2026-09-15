package interop_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/internal/scaffold"
	"github.com/raulfrk/herdr-plugin-kit/manifest"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/interop"
)

func TestActionRouteLiveProviderProcess(t *testing.T) {
	root, ok := liveRouteHelperRoot(os.Args)
	if !ok {
		return
	}
	cwd, err := os.Getwd()
	if err != nil || cwd != root {
		fmt.Fprintf(os.Stderr, "cwd=%q want=%q error=%v", cwd, root, err)
		os.Exit(41)
	}
	if err := interop.ServeRoute(context.Background(), os.Stdin, os.Stdout, time.Now(), liveRouteProvider{}); err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(42)
	}
	os.Exit(0)
}

type liveRouteProvider struct{}

func (liveRouteProvider) Catalog(context.Context, interop.RouteOrigin) (interop.RouteCatalog, error) {
	return interop.RouteCatalog{RouteRevision: "live-revision", Actions: []interop.RouteAction{{ActionID: "run", Label: "Run live action", SuggestedPath: []string{"a"}, Available: true}}}, nil
}

func (liveRouteProvider) Invoke(context.Context, interop.RouteInvocation) (interop.RouteExecution, error) {
	return interop.RouteExecution{Outcome: interop.RouteCompleted, Focus: interop.RouteFocusPreserved}, nil
}

func liveRouteHelperRoot(args []string) (string, bool) {
	for index := range len(args) - 1 {
		if args[index] == "live-route-helper" {
			return args[index+1], true
		}
	}
	return "", false
}

func TestActionRouteLiveDefaultAndNamed(t *testing.T) {
	if os.Getenv("HERDR_PLUGIN_KIT_ACTION_ROUTE_LIVE") != "1" {
		t.Skip("set HERDR_PLUGIN_KIT_ACTION_ROUTE_LIVE=1 for isolated Herdr 0.8.2 action-route tests")
	}
	for _, session := range []string{"", "api6-action-route"} {
		name := session
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) { runActionRouteLive(t, session) })
	}
}

func runActionRouteLive(t *testing.T, session string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	root, err := os.MkdirTemp("/tmp", "hpk-r-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove live root: %v", err)
		}
	})
	xdg, state := filepath.Join(root, "xdg"), filepath.Join(root, "state")
	if err := os.MkdirAll(filepath.Join(xdg, "herdr"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(xdg, "herdr", "config.toml")
	if err := os.WriteFile(config, []byte("onboarding = false\n\n[update]\nversion_check = false\nmanifest_check = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := routeLiveEnvironment(xdg, state, config)
	runner := routeLiveRunner{environment: environment}
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
	done := make(chan struct{})
	var serverErr error
	go func() {
		serverErr = server.Wait()
		close(done)
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_, _ = runner.Run(stopCtx, "herdr", routeLiveArgs(session, "server", "stop"))
		select {
		case <-done:
		case <-stopCtx.Done():
			_ = server.Process.Kill()
			<-done
		}
	}
	t.Cleanup(stop)
	waitRouteLiveServer(t, ctx, runner, session, &serverOutput, done, &serverErr)

	pluginID := "proof.route-default"
	if session != "" {
		pluginID = "proof.route-named"
	}
	pluginRoot := filepath.Join(root, "provider")
	if err := scaffold.Generate(scaffold.Options{ID: pluginID, Name: "Live route provider", Output: pluginRoot}); err != nil {
		t.Fatal(err)
	}
	argv := []string{os.Args[0], "-test.run=^TestActionRouteLiveProviderProcess$", "--", "live-route-helper", pluginRoot}
	extendLiveRoute(t, pluginRoot, argv)
	if err := scaffold.Validate(pluginRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(ctx, "herdr", routeLiveArgs(session, "plugin", "link", pluginRoot, "--enabled")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = runner.Run(cleanupCtx, "herdr", routeLiveArgs(session, "plugin", "unlink", pluginID))
	})

	client := interop.RouteClient{Caller: interop.RouteProcessCaller{HerdrRunner: runner, ProviderRunner: command.ExecRunner{StdoutLimit: interop.MaxRouteEnvelopeBytes, StderrLimit: interop.MaxRouteEnvelopeBytes}, Session: session}}
	origin := interop.RouteOrigin{Session: session, Workspace: "w1", Tab: "w1:t1", Pane: "w1:p1"}
	catalog, err := client.Catalog(ctx, interop.RouteCatalogCall{TargetPlugin: pluginID, Origin: origin, Deadline: time.Now().Add(5 * time.Second)})
	if err != nil || catalog.RouteRevision != "live-revision" || len(catalog.Actions) != 1 {
		t.Fatalf("catalog=%+v error=%v", catalog, err)
	}
	result, err := client.Invoke(ctx, interop.RouteInvokeCall{TargetPlugin: pluginID, Origin: origin, RouteRevision: catalog.RouteRevision, ActionID: "run", Keys: []string{"p", "a"}, Deadline: time.Now().Add(5 * time.Second)})
	if err != nil || result.Outcome != interop.RouteCompleted || result.Focus != interop.RouteFocusPreserved {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if _, err := runner.Run(ctx, "herdr", routeLiveArgs(session, "plugin", "disable", pluginID)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Catalog(ctx, interop.RouteCatalogCall{TargetPlugin: pluginID, Origin: origin, Deadline: time.Now().Add(5 * time.Second)}); err == nil {
		t.Fatal("disabled live provider accepted")
	}
	if _, err := runner.Run(ctx, "herdr", routeLiveArgs(session, "plugin", "enable", pluginID)); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(ctx, "herdr", routeLiveArgs(session, "plugin", "unlink", pluginID)); err != nil {
		t.Fatal(err)
	}
	stop()
}

type routeLiveRunner struct{ environment []string }

func (r routeLiveRunner) Run(ctx context.Context, executable string, args []string) (command.Result, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = r.environment
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := command.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: -1}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		return result, fmt.Errorf("%s %q: %w: %s", executable, args, err, stderr.String())
	}
	return result, nil
}

func waitRouteLiveServer(t *testing.T, ctx context.Context, runner routeLiveRunner, session string, serverOutput *bytes.Buffer, done <-chan struct{}, serverErr *error) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var lastResult command.Result
	var lastErr error
	for {
		result, err := runner.Run(ctx, "herdr", routeLiveArgs(session, "status", "server", "--json"))
		lastResult, lastErr = result, err
		if err == nil {
			var status struct {
				Status     string `json:"status"`
				Running    bool   `json:"running"`
				Version    string `json:"version"`
				Compatible bool   `json:"compatible"`
			}
			if json.Unmarshal(result.Stdout, &status) == nil && status.Status == "running" && status.Running && status.Version == "0.8.2" && status.Compatible {
				return
			}
		}
		select {
		case <-done:
			t.Fatalf("Herdr server exited before readiness: %v; server=%q", *serverErr, serverOutput.String())
		case <-ctx.Done():
			t.Fatalf("wait for Herdr 0.8.2: %v; last stdout=%q stderr=%q error=%v; server=%q", ctx.Err(), lastResult.Stdout, lastResult.Stderr, lastErr, serverOutput.String())
		case <-ticker.C:
		}
	}
}

func routeLiveArgs(session string, args ...string) []string {
	if session == "" {
		return args
	}
	return append([]string{"--session", session}, args...)
}

func routeLiveEnvironment(xdg, state, config string) []string {
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

func extendLiveRoute(t *testing.T, root string, argv []string) {
	t.Helper()
	kitPath := filepath.Join(root, "plugin-kit.json")
	file, err := os.Open(kitPath)
	if err != nil {
		t.Fatal(err)
	}
	kit, err := manifest.Parse(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	action, contract, err := interop.ActionRouteDeclaration(argv)
	if err != nil {
		t.Fatal(err)
	}
	kit.Actions = append(kit.Actions, action)
	kit.Interfaces = append(kit.Interfaces, contract)
	data, _ := json.MarshalIndent(kit, "", "  ")
	if err := os.WriteFile(kitPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, "herdr-plugin.toml")
	herdrData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	quoted := make([]string, len(argv))
	for index, value := range argv {
		quoted[index] = strconv.Quote(value)
	}
	addition := "\n[[actions]]\nid = \"route\"\ntitle = \"Route plugin actions\"\ncontexts = [\"global\"]\ncommand = [" + strings.Join(quoted, ", ") + "]\n"
	if err := os.WriteFile(manifestPath, append(herdrData, addition...), 0o644); err != nil {
		t.Fatal(err)
	}
}
