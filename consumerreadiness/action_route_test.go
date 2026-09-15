package consumerreadiness_test

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

	"github.com/raulfrk/herdr-plugin-kit/internal/scaffold"
	"github.com/raulfrk/herdr-plugin-kit/manifest"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/interop"
)

type generatedRouteProvider struct{}

func (generatedRouteProvider) Catalog(context.Context, interop.RouteOrigin) (interop.RouteCatalog, error) {
	return interop.RouteCatalog{RouteRevision: "generated-revision", Actions: []interop.RouteAction{{ActionID: "run", Label: "Run generated action", SuggestedPath: []string{"a"}, Available: true}}}, nil
}

func (generatedRouteProvider) Invoke(context.Context, interop.RouteInvocation) (interop.RouteExecution, error) {
	return interop.RouteExecution{Outcome: interop.RouteCompleted, Focus: interop.RouteFocusPreserved}, nil
}

func TestActionRouteGeneratedProviderProcess(t *testing.T) {
	root, ok := generatedRouteHelperRoot(os.Args)
	if !ok {
		return
	}
	cwd, err := os.Getwd()
	if err != nil || cwd != root {
		fmt.Fprintf(os.Stderr, "cwd=%q want=%q error=%v", cwd, root, err)
		os.Exit(31)
	}
	if err := interop.ServeRoute(context.Background(), os.Stdin, os.Stdout, time.Now(), generatedRouteProvider{}); err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(32)
	}
	os.Exit(0)
}

func generatedRouteHelperRoot(args []string) (string, bool) {
	for index := range len(args) - 1 {
		if args[index] == "generated-route-helper" {
			return args[index+1], true
		}
	}
	return "", false
}

func TestTwoGeneratedProvidersInteroperateThroughActionRoute(t *testing.T) {
	type fixture struct {
		id, root string
		argv     []string
	}
	fixtures := make([]fixture, 0, 2)
	for _, id := range []string{"generated.route-one", "generated.route-two"} {
		root := filepath.Join(t.TempDir(), id)
		if err := scaffold.Generate(scaffold.Options{ID: id, Name: id, Description: "Generated action-route provider", Output: root}); err != nil {
			t.Fatal(err)
		}
		argv := []string{os.Args[0], "-test.run=^TestActionRouteGeneratedProviderProcess$", "--", "generated-route-helper", root}
		extendGeneratedRoute(t, root, argv)
		if err := scaffold.Validate(root); err != nil {
			t.Fatalf("validate %s: %v", id, err)
		}
		fixtures = append(fixtures, fixture{id: id, root: root, argv: argv})
	}
	plugins := make([]any, 0, len(fixtures))
	for _, fixture := range fixtures {
		plugins = append(plugins, map[string]any{
			"plugin_id": fixture.id, "enabled": true, "plugin_root": fixture.root,
			"manifest_path": filepath.Join(fixture.root, "herdr-plugin.toml"),
			"actions":       []any{map[string]any{"id": interop.ActionRouteActionID, "title": "Route plugin actions", "contexts": []string{"global"}, "command": fixture.argv}},
		})
	}
	listing, err := json.Marshal(map[string]any{"id": "cli:plugin", "result": map[string]any{"type": "plugin_list", "plugins": plugins}})
	if err != nil {
		t.Fatal(err)
	}
	runner := generatedHerdrRunner(func(_ context.Context, executable string, args []string) (command.Result, error) {
		if executable != "/opt/herdr" || !reflect.DeepEqual(args, []string{"--session", "generated.named", "plugin", "list", "--json"}) {
			t.Fatalf("Herdr call = %q %q", executable, args)
		}
		return command.Result{Stdout: listing}, nil
	})
	client := interop.RouteClient{Caller: interop.RouteProcessCaller{HerdrRunner: runner, ProviderRunner: command.ExecRunner{StdoutLimit: interop.MaxRouteEnvelopeBytes, StderrLimit: interop.MaxRouteEnvelopeBytes}, Herdr: "/opt/herdr", Session: "generated.named"}}
	origin := interop.RouteOrigin{Session: "generated.named", Workspace: "w1", Tab: "w1:t1", Pane: "w1:p1"}
	for _, fixture := range fixtures {
		catalog, err := client.Catalog(context.Background(), interop.RouteCatalogCall{TargetPlugin: fixture.id, Origin: origin, Deadline: time.Now().Add(5 * time.Second)})
		if err != nil {
			t.Fatalf("catalog %s: %v", fixture.id, err)
		}
		result, err := client.Invoke(context.Background(), interop.RouteInvokeCall{TargetPlugin: fixture.id, Origin: origin, RouteRevision: catalog.RouteRevision, ActionID: "run", Keys: []string{"p", "a"}, Deadline: time.Now().Add(5 * time.Second)})
		if err != nil {
			t.Fatalf("invoke %s: %v", fixture.id, err)
		}
		if result.TargetPlugin != fixture.id || result.Outcome != interop.RouteCompleted || result.Focus != interop.RouteFocusPreserved {
			t.Fatalf("result %s = %+v", fixture.id, result)
		}
	}
}

type generatedHerdrRunner func(context.Context, string, []string) (command.Result, error)

func (f generatedHerdrRunner) Run(ctx context.Context, executable string, args []string) (command.Result, error) {
	return f(ctx, executable, args)
}

func extendGeneratedRoute(t *testing.T, root string, argv []string) {
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
	data, err := json.MarshalIndent(kit, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kitPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, "herdr-plugin.toml")
	herdrData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	addition := "\n[[actions]]\nid = \"route\"\ntitle = \"Route plugin actions\"\ncontexts = [\"global\"]\ncommand = [" + quoteTOML(argv) + "]\n"
	if err := os.WriteFile(manifestPath, append(herdrData, addition...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func quoteTOML(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = strconv.Quote(value)
	}
	return strings.Join(quoted, ", ")
}
