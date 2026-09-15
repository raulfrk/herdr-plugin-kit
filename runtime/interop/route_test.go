package interop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/raulfrk/herdr-plugin-kit/manifest"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/internal/herdrcmd"
	"pgregory.net/rapid"
)

type routeCommandRunnerFunc func(context.Context, string, []string) (command.Result, error)

func (f routeCommandRunnerFunc) Run(ctx context.Context, executable string, args []string) (command.Result, error) {
	return f(ctx, executable, args)
}

type routeSpecRunnerFunc func(context.Context, command.Spec) (command.Result, error)

func (f routeSpecRunnerFunc) RunSpec(ctx context.Context, spec command.Spec) (command.Result, error) {
	return f(ctx, spec)
}

type proofRouteProvider struct{}

func (proofRouteProvider) Catalog(context.Context, RouteOrigin) (RouteCatalog, error) {
	return RouteCatalog{RouteRevision: "rev-proof-1", Actions: []RouteAction{
		{ActionID: "new", Label: "New session", SuggestedPath: []string{"n", "a"}, Available: true},
		{ActionID: "fork", Label: "Fork session", SuggestedPath: []string{"f", "b"}, Available: false, Reason: "source is busy"},
	}}, nil
}

func (proofRouteProvider) Invoke(_ context.Context, invocation RouteInvocation) (RouteExecution, error) {
	if invocation.RouteRevision != "rev-proof-1" {
		return RouteExecution{}, &CallError{Category: Unavailable, Message: "catalog changed"}
	}
	if invocation.ActionID != "new" || !reflect.DeepEqual(invocation.Keys, []string{"p", "n", "a"}) {
		return RouteExecution{}, &CallError{Category: NotFound, Message: "action unavailable"}
	}
	return RouteExecution{Outcome: RouteCompleted, Focus: RouteFocusPreserved}, nil
}

func TestRouteProviderProcess(t *testing.T) {
	marker, root := routeHelperArgs(os.Args)
	if !marker {
		return
	}
	cwd, err := os.Getwd()
	if err != nil || cwd != root {
		fmt.Fprintf(os.Stderr, "provider cwd=%q want=%q error=%v", cwd, root, err)
		os.Exit(21)
	}
	if err := ServeRoute(context.Background(), os.Stdin, os.Stdout, time.Now(), proofRouteProvider{}); err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(22)
	}
	os.Exit(0)
}

func routeHelperArgs(args []string) (bool, string) {
	for index := range len(args) - 1 {
		if args[index] == "route-provider-helper" {
			return true, args[index+1]
		}
	}
	return false, ""
}

func TestRouteProcessClientCatalogAndInvokeEndToEnd(t *testing.T) {
	root := t.TempDir()
	argv := []string{os.Args[0], "-test.run=^TestRouteProviderProcess$", "--", "route-provider-helper", root}
	pluginList := writeRouteFixture(t, root, argv)
	var mu sync.Mutex
	var herdrCalls [][]string
	herdr := routeCommandRunnerFunc(func(_ context.Context, executable string, args []string) (command.Result, error) {
		mu.Lock()
		herdrCalls = append(herdrCalls, append([]string{executable}, args...))
		mu.Unlock()
		return command.Result{Stdout: pluginList}, nil
	})
	correlations := []string{"catalog-correlation", "invoke-correlation"}
	client := RouteClient{
		Caller: RouteProcessCaller{
			HerdrRunner: herdr, ProviderRunner: command.ExecRunner{StdoutLimit: MaxRouteEnvelopeBytes, StderrLimit: MaxRouteEnvelopeBytes},
			Herdr: "/opt/herdr", Session: "proof.named",
		},
		NewCorrelation: func() (string, error) {
			value := correlations[0]
			correlations = correlations[1:]
			return value, nil
		},
	}
	origin := RouteOrigin{Session: "proof.named", Workspace: "w1", Tab: "w1:t2", Pane: "w1:p3"}
	catalog, err := client.Catalog(context.Background(), RouteCatalogCall{TargetPlugin: "proof.provider", Origin: origin, Deadline: time.Now().Add(5 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if catalog.TargetPlugin != "proof.provider" || catalog.RouteRevision != "rev-proof-1" || len(catalog.Actions) != 2 {
		t.Fatalf("catalog = %+v", catalog)
	}
	result, err := client.Invoke(context.Background(), RouteInvokeCall{TargetPlugin: "proof.provider", Origin: origin, RouteRevision: catalog.RouteRevision, ActionID: "new", Keys: []string{"p", "n", "a"}, Deadline: time.Now().Add(5 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if result != (RouteInvokeResult{TargetPlugin: "proof.provider", RouteRevision: "rev-proof-1", ActionID: "new", Outcome: RouteCompleted, Focus: RouteFocusPreserved}) {
		t.Fatalf("result = %+v", result)
	}
	wantCall := []string{"/opt/herdr", "--session", "proof.named", "plugin", "list", "--json"}
	for _, got := range herdrCalls {
		if !reflect.DeepEqual(got, wantCall) {
			t.Fatalf("Herdr call = %q, want %q", got, wantCall)
		}
	}
}

func TestRouteProcessDiscoveryRejectsMissingDisabledAndDriftedProviders(t *testing.T) {
	tests := map[string]func(*routePluginListEnvelope, string){
		"missing":     func(envelope *routePluginListEnvelope, _ string) { envelope.Result.Plugins[0].PluginID = "proof.other" },
		"disabled":    func(envelope *routePluginListEnvelope, _ string) { envelope.Result.Plugins[0].Enabled = false },
		"envelope id": func(envelope *routePluginListEnvelope, _ string) { envelope.ID = "other" },
		"result type": func(envelope *routePluginListEnvelope, _ string) { envelope.Result.Type = "other" },
		"unclean root": func(envelope *routePluginListEnvelope, root string) {
			envelope.Result.Plugins[0].PluginRoot = root + "/."
		},
		"manifest path": func(envelope *routePluginListEnvelope, root string) {
			envelope.Result.Plugins[0].ManifestPath = filepath.Join(root, "other.toml")
		},
		"title drift": func(envelope *routePluginListEnvelope, _ string) {
			envelope.Result.Plugins[0].Actions[0].Title = "Changed"
		},
		"description drift": func(envelope *routePluginListEnvelope, _ string) {
			value := "changed"
			envelope.Result.Plugins[0].Actions[0].Description = &value
		},
		"contexts drift": func(envelope *routePluginListEnvelope, _ string) {
			envelope.Result.Plugins[0].Actions[0].Contexts = []string{"pane"}
		},
		"argv drift": func(envelope *routePluginListEnvelope, _ string) {
			envelope.Result.Plugins[0].Actions[0].Command = []string{"/bin/false"}
		},
		"duplicate provider": func(envelope *routePluginListEnvelope, _ string) {
			envelope.Result.Plugins = append(envelope.Result.Plugins, envelope.Result.Plugins[0])
		},
		"duplicate route": func(envelope *routePluginListEnvelope, _ string) {
			envelope.Result.Plugins[0].Actions = append(envelope.Result.Plugins[0].Actions, envelope.Result.Plugins[0].Actions[0])
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			data := writeRouteFixture(t, root, []string{"./provider", "route"})
			var envelope routePluginListEnvelope
			if err := json.Unmarshal(data, &envelope); err != nil {
				t.Fatal(err)
			}
			mutate(&envelope, root)
			data, _ = json.Marshal(envelope)
			caller := RouteProcessCaller{HerdrRunner: routeCommandRunnerFunc(func(context.Context, string, []string) (command.Result, error) {
				return command.Result{Stdout: data}, nil
			}), ProviderRunner: routeSpecRunnerFunc(func(context.Context, command.Spec) (command.Result, error) {
				t.Fatal("provider executed")
				return command.Result{}, nil
			})}
			if _, _, err := caller.discover(context.Background(), "proof.provider"); err == nil {
				t.Fatal("invalid provider accepted")
			}
		})
	}
	t.Run("relative root", func(t *testing.T) {
		root := t.TempDir()
		data := writeRouteFixture(t, root, []string{"./provider", "route"})
		var envelope routePluginListEnvelope
		if err := json.Unmarshal(data, &envelope); err != nil {
			t.Fatal(err)
		}
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		relative, err := filepath.Rel(cwd, root)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.IsAbs(relative) {
			t.Fatalf("relative fixture path is absolute: %q", relative)
		}
		envelope.Result.Plugins[0].PluginRoot = relative
		envelope.Result.Plugins[0].ManifestPath = filepath.Join(relative, "herdr-plugin.toml")
		data, err = json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		assertRouteDiscoveryRejected(t, root, data)
	})
	t.Run("interface drift", func(t *testing.T) {
		root := t.TempDir()
		data := writeRouteFixture(t, root, []string{"./provider", "route"})
		manifestPath := filepath.Join(root, "plugin-kit.json")
		var kit manifest.Manifest
		file, err := os.Open(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		kit, err = manifest.Parse(file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		kit.Interfaces = nil
		changed, _ := json.Marshal(kit)
		if err := os.WriteFile(manifestPath, changed, 0o600); err != nil {
			t.Fatal(err)
		}
		caller := RouteProcessCaller{HerdrRunner: routeCommandRunnerFunc(func(context.Context, string, []string) (command.Result, error) {
			return command.Result{Stdout: data}, nil
		}), ProviderRunner: routeSpecRunnerFunc(func(context.Context, command.Spec) (command.Result, error) {
			t.Fatal("provider executed")
			return command.Result{}, nil
		})}
		if _, _, err := caller.discover(context.Background(), "proof.provider"); err == nil {
			t.Fatal("interface drift accepted")
		}
	})
	t.Run("Herdr manifest drift", func(t *testing.T) {
		root := t.TempDir()
		data := writeRouteFixture(t, root, []string{"./provider", "route"})
		manifestPath := filepath.Join(root, "herdr-plugin.toml")
		contents, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		var herdr routeDiskManifest
		if err := toml.Unmarshal(contents, &herdr); err != nil {
			t.Fatal(err)
		}
		herdr.Actions[0].Command = append(herdr.Actions[0].Command, "changed")
		changed, err := toml.Marshal(herdr)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manifestPath, changed, 0o600); err != nil {
			t.Fatal(err)
		}
		caller := RouteProcessCaller{HerdrRunner: routeCommandRunnerFunc(func(context.Context, string, []string) (command.Result, error) {
			return command.Result{Stdout: data}, nil
		}), ProviderRunner: routeSpecRunnerFunc(func(context.Context, command.Spec) (command.Result, error) {
			t.Fatal("provider executed")
			return command.Result{}, nil
		})}
		if _, _, err := caller.discover(context.Background(), "proof.provider"); err == nil {
			t.Fatal("Herdr manifest drift accepted")
		}
	})
	for name, mutate := range map[string]func(*manifest.Manifest){
		"interface id":        func(value *manifest.Manifest) { value.Interfaces[0].ID = "other-route" },
		"interface version":   func(value *manifest.Manifest) { value.Interfaces[0].Version++ },
		"interface direction": func(value *manifest.Manifest) { value.Interfaces[0].Direction = manifest.InterfaceRequires },
	} {
		t.Run("plugin-kit "+name+" drift", func(t *testing.T) {
			root := t.TempDir()
			data := writeRouteFixture(t, root, []string{"./provider", "route"})
			rewriteRouteKitFixture(t, root, mutate)
			assertRouteDiscoveryRejected(t, root, data)
		})
	}
	for name, mutate := range map[string]func(*routeDiskManifest){
		"identity":       func(value *routeDiskManifest) { value.ID = "proof.other" },
		"missing action": func(value *routeDiskManifest) { value.Actions = nil },
		"title":          func(value *routeDiskManifest) { value.Actions[0].Title = "Changed" },
		"description":    func(value *routeDiskManifest) { value.Actions[0].Description = "changed" },
		"contexts":       func(value *routeDiskManifest) { value.Actions[0].Contexts = []string{"pane"} },
	} {
		t.Run("Herdr "+name+" drift", func(t *testing.T) {
			root := t.TempDir()
			data := writeRouteFixture(t, root, []string{"./provider", "route"})
			rewriteRouteHerdrFixture(t, root, mutate)
			assertRouteDiscoveryRejected(t, root, data)
		})
	}
}

func assertRouteDiscoveryRejected(t *testing.T, root string, data []byte) {
	t.Helper()
	caller := RouteProcessCaller{HerdrRunner: routeCommandRunnerFunc(func(context.Context, string, []string) (command.Result, error) {
		return command.Result{Stdout: data}, nil
	}), ProviderRunner: routeSpecRunnerFunc(func(context.Context, command.Spec) (command.Result, error) {
		t.Fatal("provider executed")
		return command.Result{}, nil
	})}
	if _, _, err := caller.discover(context.Background(), "proof.provider"); err == nil {
		t.Fatalf("invalid provider accepted from %s", root)
	}
}

func rewriteRouteKitFixture(t *testing.T, root string, mutate func(*manifest.Manifest)) {
	t.Helper()
	path := filepath.Join(root, "plugin-kit.json")
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	value, err := manifest.Parse(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	mutate(&value)
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func rewriteRouteHerdrFixture(t *testing.T, root string, mutate func(*routeDiskManifest)) {
	t.Helper()
	path := filepath.Join(root, "herdr-plugin.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value routeDiskManifest
	if err := toml.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	mutate(&value)
	data, err = toml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRouteProcessCallerRejectsCrossSessionOriginsBeforeCommands(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		origin     string
	}{
		{name: "named to other named", configured: "named-a", origin: "named-b"},
		{name: "named to default", configured: "named-a", origin: ""},
		{name: "default to named", configured: "", origin: "named-a"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validRouteRequest(t, RouteMethodCatalog)
			request.Payload, _ = json.Marshal(routeCatalogPayload{Origin: RouteOrigin{Session: test.origin, Workspace: "w1", Tab: "w1:t1", Pane: "w1:p1"}})
			failCommand := func() {
				t.Fatal("external command executed")
			}
			caller := RouteProcessCaller{
				Session: test.configured,
				HerdrRunner: routeCommandRunnerFunc(func(context.Context, string, []string) (command.Result, error) {
					failCommand()
					return command.Result{}, nil
				}),
				ProviderRunner: routeSpecRunnerFunc(func(context.Context, command.Spec) (command.Result, error) {
					failCommand()
					return command.Result{}, nil
				}),
			}
			if _, err := caller.Call(context.Background(), request); err == nil {
				t.Fatal("cross-session origin accepted")
			}
		})
	}
}

func TestRouteProcessCallerRejectsEachMissingRunnerAndInvalidSession(t *testing.T) {
	request := validRouteRequest(t, RouteMethodCatalog)
	request.Payload, _ = json.Marshal(routeCatalogPayload{Origin: RouteOrigin{Workspace: "w1", Tab: "w1:t1", Pane: "w1:p1"}})
	validHerdr := routeCommandRunnerFunc(func(context.Context, string, []string) (command.Result, error) {
		t.Fatal("Herdr executed")
		return command.Result{}, nil
	})
	validProvider := routeSpecRunnerFunc(func(context.Context, command.Spec) (command.Result, error) {
		t.Fatal("provider executed")
		return command.Result{}, nil
	})
	for name, caller := range map[string]RouteProcessCaller{
		"Herdr runner":    {ProviderRunner: validProvider},
		"provider runner": {HerdrRunner: validHerdr},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := caller.Call(context.Background(), request); err == nil {
				t.Fatal("invalid caller accepted")
			}
		})
	}
	request.Payload, _ = json.Marshal(routeCatalogPayload{Origin: RouteOrigin{Session: "bad session", Workspace: "w1", Tab: "w1:t1", Pane: "w1:p1"}})
	caller := RouteProcessCaller{HerdrRunner: validHerdr, ProviderRunner: validProvider, Session: "bad session"}
	if _, err := caller.Call(context.Background(), request); err == nil {
		t.Fatal("invalid session accepted")
	}
}

func TestRouteProcessCallerRejectsMalformedAndOversizedResponses(t *testing.T) {
	root := t.TempDir()
	argv := []string{"./provider", "route"}
	pluginList := writeRouteFixture(t, root, argv)
	request := validRouteRequest(t, RouteMethodCatalog)
	payload, _ := json.Marshal(RouteCatalogResult{TargetPlugin: request.TargetPlugin, RouteRevision: "rev-1", Actions: []RouteAction{{ActionID: "new", Label: "New", SuggestedPath: []string{"n"}, Available: true}}})
	valid, _ := json.Marshal(Response{Version: Version, CorrelationID: request.CorrelationID, Payload: payload})
	tests := map[string]func([]byte) (command.Result, error){
		"empty": func([]byte) (command.Result, error) { return command.Result{}, nil },
		"trailing": func(data []byte) (command.Result, error) {
			return command.Result{Stdout: append(data, []byte(" {}")...)}, nil
		},
		"duplicate": func(data []byte) (command.Result, error) {
			return command.Result{Stdout: bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1)}, nil
		},
		"case": func(data []byte) (command.Result, error) {
			return command.Result{Stdout: bytes.Replace(data, []byte(`"version":1`), []byte(`"Version":1`), 1)}, nil
		},
		"truncated stdout": func(data []byte) (command.Result, error) {
			return command.Result{Stdout: data, StdoutTruncated: true}, nil
		},
		"truncated stderr": func(data []byte) (command.Result, error) {
			return command.Result{Stdout: data, StderrTruncated: true}, nil
		},
	}
	for name, produce := range tests {
		t.Run(name, func(t *testing.T) {
			caller := RouteProcessCaller{
				Session: "proof",
				HerdrRunner: routeCommandRunnerFunc(func(context.Context, string, []string) (command.Result, error) {
					return command.Result{Stdout: pluginList}, nil
				}),
				ProviderRunner: routeSpecRunnerFunc(func(_ context.Context, spec command.Spec) (command.Result, error) {
					if spec.Executable != argv[0] || !reflect.DeepEqual(spec.Args, argv[1:]) || spec.Dir != root || len(spec.Stdin) == 0 {
						t.Fatalf("spec = %+v", spec)
					}
					return produce(append([]byte(nil), valid...))
				}),
			}
			if _, err := caller.Call(context.Background(), request); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

func TestRouteProcessCallerHonorsEnvelopeDeadlineAndCancellation(t *testing.T) {
	root := t.TempDir()
	pluginList := writeRouteFixture(t, root, []string{"./provider"})
	started := make(chan struct{}, 1)
	provider := routeSpecRunnerFunc(func(ctx context.Context, _ command.Spec) (command.Result, error) {
		started <- struct{}{}
		<-ctx.Done()
		return command.Result{Canceled: true}, ctx.Err()
	})
	caller := RouteProcessCaller{Session: "proof", HerdrRunner: routeCommandRunnerFunc(func(context.Context, string, []string) (command.Result, error) {
		return command.Result{Stdout: pluginList}, nil
	}), ProviderRunner: provider}
	request := validRouteRequest(t, RouteMethodCatalog)
	request.Deadline = time.Now().Add(25 * time.Millisecond)
	begin := time.Now()
	if _, err := caller.Call(context.Background(), request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error = %v", err)
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("deadline took %v", elapsed)
	}
	<-started
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	request.Deadline = time.Now().Add(time.Minute)
	if _, err := caller.Call(canceled, request); err == nil {
		t.Fatal("cancellation accepted")
	} else {
		var callErr *CallError
		if !errors.As(err, &callErr) || callErr.Category != Canceled {
			t.Fatalf("cancellation error = %v", err)
		}
	}
}

func TestRouteProcessCallerConcurrentCorrelations(t *testing.T) {
	root := t.TempDir()
	pluginList := writeRouteFixture(t, root, []string{"./provider"})
	caller := RouteProcessCaller{
		Session: "proof",
		HerdrRunner: routeCommandRunnerFunc(func(context.Context, string, []string) (command.Result, error) {
			return command.Result{Stdout: pluginList}, nil
		}),
		ProviderRunner: routeSpecRunnerFunc(func(_ context.Context, spec command.Spec) (command.Result, error) {
			var request Request
			if err := json.Unmarshal(spec.Stdin, &request); err != nil {
				return command.Result{}, err
			}
			payload, _ := json.Marshal(RouteCatalogResult{TargetPlugin: request.TargetPlugin, RouteRevision: "rev-1", Actions: []RouteAction{}})
			data, _ := json.Marshal(Response{Version: Version, CorrelationID: request.CorrelationID, Payload: payload})
			return command.Result{Stdout: data}, nil
		}),
	}
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for index := range 24 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			request := validRouteRequest(t, RouteMethodCatalog)
			request.CorrelationID = fmt.Sprintf("correlation-%d", index)
			_, err := caller.Call(context.Background(), request)
			errs <- err
		}(index)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestRouteProcessInputFileAndObservationBounds(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "bounded")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), manifest.MaxManifestBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	directory, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	data, err := readRouteProviderFile(directory, "bounded")
	if err != nil || len(data) != manifest.MaxManifestBytes {
		t.Fatalf("exact manifest bound: length=%d error=%v", len(data), err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), manifest.MaxManifestBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRouteProviderFile(directory, "bounded"); err == nil {
		t.Fatal("oversized provider file accepted")
	}

	observation := []byte(`{"id":"cli:plugin","result":{"type":"plugin_list","plugins":[]}}`)
	observation = append(observation, bytes.Repeat([]byte(" "), herdrcmd.MaxJSONBytes-len(observation))...)
	var envelope routePluginListEnvelope
	if err := decodeHerdrObservation(observation, &envelope); err != nil {
		t.Fatalf("exact Herdr observation bound: %v", err)
	}
	if err := decodeHerdrObservation(append(observation, ' '), &envelope); err == nil {
		t.Fatal("oversized Herdr observation accepted")
	}
	if err := decodeHerdrObservation([]byte("[]"), &envelope); err == nil {
		t.Fatal("malformed Herdr observation accepted")
	}
}

func writeRouteFixture(t *testing.T, root string, argv []string) []byte {
	t.Helper()
	herdrData, err := toml.Marshal(routeDiskManifest{ID: "proof.provider", Actions: []routeDiskAction{{ID: ActionRouteActionID, Title: "Route plugin actions", Contexts: []string{"global"}, Command: argv}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "herdr-plugin.toml"), herdrData, 0o600); err != nil {
		t.Fatal(err)
	}
	kit := manifest.Manifest{
		PluginID: "proof.provider", Name: "Proof provider", Version: "0.0.0", Executable: argv[0],
		Actions:    []manifest.Action{{ID: ActionRouteActionID, Title: "Route plugin actions", Contexts: []string{"global"}, Command: argv}},
		Interfaces: []manifest.Interface{{ID: ActionRouteInterface, Version: ActionRouteInterfaceVersion, Direction: manifest.InterfaceProvides}},
	}
	data, err := json.Marshal(kit)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugin-kit.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	envelope := map[string]any{
		"id": "cli:plugin",
		"result": map[string]any{
			"type":                   "plugin_list",
			"diagnostic_added_later": true,
			"plugins": []any{map[string]any{
				"plugin_id": "proof.provider", "enabled": true, "plugin_root": root,
				"manifest_path": filepath.Join(root, "herdr-plugin.toml"), "name": "Proof provider", "version": "0.0.0",
				"actions": []any{map[string]any{"id": ActionRouteActionID, "title": "Route plugin actions", "contexts": []string{"global"}, "command": argv}},
			}},
		},
	}
	data, err = json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestServeRouteRejectsNonExactJSON(t *testing.T) {
	request := validRouteRequest(t, RouteMethodCatalog)
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func([]byte) []byte{
		"duplicate outer": func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1)
		},
		"case outer": func(data []byte) []byte { return bytes.Replace(data, []byte(`"version":1`), []byte(`"Version":1`), 1) },
		"trailing":   func(data []byte) []byte { return append(data, []byte(" {}")...) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if err := ServeRoute(context.Background(), bytes.NewReader(mutate(data)), &output, time.Now(), proofRouteProvider{}); err == nil || output.Len() != 0 {
				t.Fatalf("output=%q error=%v", output.String(), err)
			}
		})
	}
	duplicateOrigin := bytes.Replace(data, []byte(`"session":"proof"`), []byte(`"session":"proof","session":"other"`), 1)
	var output bytes.Buffer
	if err := ServeRoute(context.Background(), bytes.NewReader(duplicateOrigin), &output, time.Now(), proofRouteProvider{}); err != nil {
		t.Fatal(err)
	}
	var response Response
	decodeErr := json.Unmarshal(output.Bytes(), &response)
	if decodeErr != nil || response.Error == nil || response.Error.Category != InvalidRequest {
		t.Fatalf("response=%+v decode=%v", response, decodeErr)
	}
}

func TestRouteJSONDecoderBoundariesAndNestedTypes(t *testing.T) {
	exactEnvelope := append([]byte(`{}`), bytes.Repeat([]byte(" "), MaxRouteEnvelopeBytes-2)...)
	if data, err := readRouteEnvelope(bytes.NewReader(exactEnvelope)); err != nil || len(data) != MaxRouteEnvelopeBytes {
		t.Fatalf("exact envelope bound: length=%d error=%v", len(data), err)
	}
	if _, err := readRouteEnvelope(bytes.NewReader(append(exactEnvelope, ' '))); err == nil {
		t.Fatal("oversized envelope accepted")
	}

	type exactValue struct {
		Value int `json:"value"`
	}
	exactJSON := []byte(`{"value":1}`)
	exactJSON = append(exactJSON, bytes.Repeat([]byte(" "), MaxRouteEnvelopeBytes-len(exactJSON))...)
	var value exactValue
	if err := decodeExactJSON(exactJSON, []string{"value"}, &value); err != nil || value.Value != 1 {
		t.Fatalf("exact JSON bound: value=%+v error=%v", value, err)
	}
	if err := decodeExactJSON(append(exactJSON, ' '), []string{"value"}, &value); err == nil {
		t.Fatal("oversized exact JSON accepted")
	}
	for name, data := range map[string][]byte{
		"non-object":         []byte(`[]`),
		"malformed trailing": []byte(`{"value":1}]`),
		"wrong value type":   []byte(`{"value":"one"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := decodeExactJSON(data, []string{"value"}, &value); err == nil {
				t.Fatal("malformed exact JSON accepted")
			}
		})
	}
	if _, err := exactJSONObject([]byte(`[]`), []string{"value"}); err == nil {
		t.Fatal("non-object accepted as exact object")
	}
	if _, err := exactJSONObject([]byte(`{"value":1}]`), []string{"value"}); err == nil {
		t.Fatal("malformed trailing token accepted by exact object decoder")
	}

	duplicateError := []byte(`{"version":1,"correlation_id":"correlation","payload":null,"error":{"category":"internal","category":"internal","message":"failed"}}`)
	var response Response
	if err := decodeRouteEnvelopeResponse(duplicateError, &response); err == nil {
		t.Fatal("duplicate nested error field accepted")
	}
	var catalogPayload routeCatalogPayload
	if err := decodeRouteCatalogPayload([]byte(`{"origin":{"session":"proof","workspace":"w1","tab":"w1:t1","pane":1}}`), &catalogPayload); err == nil || !strings.Contains(err.Error(), "cannot unmarshal number") {
		t.Fatalf("wrong catalog origin type error = %v", err)
	}
	var invokePayload routeInvokePayload
	if err := decodeRouteInvokePayload([]byte(`{"origin":{"session":"proof","workspace":"w1","tab":"w1:t1","pane":1},"route_revision":"revision","action_id":"new","keys":["n"]}`), &invokePayload); err == nil || !strings.Contains(err.Error(), "cannot unmarshal number") {
		t.Fatalf("wrong invoke origin type error = %v", err)
	}
}

func TestServeRouteBoundsMaximumFieldCatalogueBeforeWriting(t *testing.T) {
	actions := make([]RouteAction, MaxRouteActions)
	for index := range actions {
		actions[index] = RouteAction{
			ActionID:      fmt.Sprintf("action-%d", index),
			Label:         strings.Repeat("l", maxRouteLabelBytes),
			SuggestedPath: routePath(index),
			Available:     false,
			Reason:        strings.Repeat("r", maxRouteReasonBytes),
		}
	}
	request := validRouteRequest(t, RouteMethodCatalog)
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	provider := fixedCatalogProvider{catalog: RouteCatalog{RouteRevision: strings.Repeat("v", maxRouteRevisionBytes), Actions: actions}}
	if err := ServeRoute(context.Background(), bytes.NewReader(data), &output, time.Now(), provider); err != nil {
		t.Fatal(err)
	}
	if output.Len() > MaxRouteEnvelopeBytes {
		t.Fatalf("response length = %d, want <= %d", output.Len(), MaxRouteEnvelopeBytes)
	}
	var response Response
	if err := decodeRouteEnvelopeResponse(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Category != PayloadTooLarge || len(response.Payload) != 0 {
		t.Fatalf("response = %+v", response)
	}
}

func TestServeRouteAndWriterExactResponseBounds(t *testing.T) {
	request := validRouteRequest(t, RouteMethodCatalog)
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for name, encodedSize := range map[string]int{"exact": MaxRouteEnvelopeBytes - 1, "one byte over": MaxRouteEnvelopeBytes} {
		t.Run(name, func(t *testing.T) {
			catalog := routeCatalogForEncodedResponseSize(t, request, encodedSize)
			var output bytes.Buffer
			if err := ServeRoute(context.Background(), bytes.NewReader(data), &output, time.Now(), fixedCatalogProvider{catalog: catalog}); err != nil {
				t.Fatal(err)
			}
			if output.Len() > MaxRouteEnvelopeBytes {
				t.Fatalf("response length = %d", output.Len())
			}
			var response Response
			if err := decodeRouteEnvelopeResponse(output.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if encodedSize == MaxRouteEnvelopeBytes-1 {
				if output.Len() != MaxRouteEnvelopeBytes || response.Error != nil {
					t.Fatalf("exact response: length=%d response=%+v", output.Len(), response)
				}
			} else if response.Error == nil || response.Error.Category != PayloadTooLarge {
				t.Fatalf("oversized response = %+v", response)
			}
		})
	}

	var output bytes.Buffer
	if err := writeRouteEnvelope(&output, make([]byte, MaxRouteEnvelopeBytes-1)); err != nil || output.Len() != MaxRouteEnvelopeBytes {
		t.Fatalf("exact writer bound: length=%d error=%v", output.Len(), err)
	}
	output.Reset()
	if err := writeRouteEnvelope(&output, make([]byte, MaxRouteEnvelopeBytes)); err == nil || output.Len() != 0 {
		t.Fatalf("oversized writer: length=%d error=%v", output.Len(), err)
	}
}

func routeCatalogForEncodedResponseSize(t *testing.T, request Request, want int) RouteCatalog {
	t.Helper()
	actions := make([]RouteAction, MaxRouteActions)
	for index := range actions {
		actions[index] = RouteAction{ActionID: fmt.Sprintf("action-%d", index), Label: "l", SuggestedPath: routePath(index), Available: false, Reason: "r"}
	}
	catalog := RouteCatalog{RouteRevision: "revision", Actions: actions}
	encodedSize := func() int {
		response := DispatchRoute(context.Background(), request, time.Now(), fixedCatalogProvider{catalog: catalog})
		data, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		return len(data)
	}
	remaining := want - encodedSize()
	for index := range catalog.Actions {
		labelBytes := min(remaining, maxRouteLabelBytes-len(catalog.Actions[index].Label))
		catalog.Actions[index].Label += strings.Repeat("l", labelBytes)
		remaining -= labelBytes
		reasonBytes := min(remaining, maxRouteReasonBytes-len(catalog.Actions[index].Reason))
		catalog.Actions[index].Reason += strings.Repeat("r", reasonBytes)
		remaining -= reasonBytes
	}
	if remaining != 0 {
		t.Fatalf("cannot construct %d-byte response; %d bytes remain", want, remaining)
	}
	if err := ValidateRouteCatalog(catalog); err != nil {
		t.Fatal(err)
	}
	if got := encodedSize(); got != want {
		t.Fatalf("encoded response length = %d, want %d", got, want)
	}
	return catalog
}

func TestServeRouteRejectsEscapeAmplifyingCorrelationWithoutOutput(t *testing.T) {
	request := validRouteRequest(t, RouteMethodCatalog)
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"correlation_id":"correlation"`), []byte(`"correlation_id":"`+strings.Repeat("<", 12_000)+`"`), 1)
	if len(data) > MaxRouteEnvelopeBytes {
		t.Fatalf("request length = %d, want <= %d", len(data), MaxRouteEnvelopeBytes)
	}
	var output bytes.Buffer
	if err := ServeRoute(context.Background(), bytes.NewReader(data), &output, time.Now(), proofRouteProvider{}); err == nil {
		t.Fatal("invalid correlation accepted")
	}
	if output.Len() != 0 {
		t.Fatalf("output length = %d, want 0", output.Len())
	}
}

func TestRouteClientRejectsResponseIdentityAndSchemaDrift(t *testing.T) {
	request := validRouteRequest(t, RouteMethodCatalog)
	validPayload, _ := json.Marshal(RouteCatalogResult{TargetPlugin: request.TargetPlugin, RouteRevision: "rev-proof-1", Actions: []RouteAction{{ActionID: "new", Label: "New", SuggestedPath: []string{"n"}, Available: true}}})
	tests := map[string]func(Response) Response{
		"correlation": func(response Response) Response { response.CorrelationID = "other"; return response },
		"target": func(response Response) Response {
			response.Payload = bytes.Replace(response.Payload, []byte(request.TargetPlugin), []byte("proof.other"), 1)
			return response
		},
		"duplicate payload field": func(response Response) Response {
			response.Payload = bytes.Replace(response.Payload, []byte(`"target_plugin":`), []byte(`"target_plugin":"proof.provider","target_plugin":`), 1)
			return response
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			response := mutate(Response{Version: Version, CorrelationID: request.CorrelationID, Payload: append([]byte(nil), validPayload...)})
			client := RouteClient{Caller: callerFunc(func(context.Context, Request) (Response, error) { return response, nil }), NewCorrelation: func() (string, error) { return request.CorrelationID, nil }}
			if _, err := client.Catalog(context.Background(), RouteCatalogCall{TargetPlugin: request.TargetPlugin, Origin: routeOrigin(), Deadline: time.Now().Add(time.Second)}); err == nil {
				t.Fatal("drift accepted")
			}
		})
	}
}

func TestRouteClientInvokeRejectsEachResponseIdentityDrift(t *testing.T) {
	call := RouteInvokeCall{TargetPlugin: "proof.provider", Origin: routeOrigin(), RouteRevision: "rev-proof-1", ActionID: "new", Keys: []string{"p", "n", "a"}, Deadline: time.Now().Add(time.Second)}
	for name, mutate := range map[string]func(*RouteInvokeResult){
		"target":   func(result *RouteInvokeResult) { result.TargetPlugin = "proof.other" },
		"revision": func(result *RouteInvokeResult) { result.RouteRevision = "other" },
		"action":   func(result *RouteInvokeResult) { result.ActionID = "fork" },
	} {
		t.Run(name, func(t *testing.T) {
			result := RouteInvokeResult{TargetPlugin: call.TargetPlugin, RouteRevision: call.RouteRevision, ActionID: call.ActionID, Outcome: RouteCompleted, Focus: RouteFocusPreserved}
			mutate(&result)
			payload, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			client := RouteClient{
				Caller: callerFunc(func(_ context.Context, request Request) (Response, error) {
					return Response{Version: Version, CorrelationID: request.CorrelationID, Payload: payload}, nil
				}),
				NewCorrelation: func() (string, error) { return "correlation", nil },
			}
			if _, err := client.Invoke(context.Background(), call); err == nil {
				t.Fatal("invoke identity drift accepted")
			}
		})
	}
}

func TestRouteClientDefaultCorrelationIsValid(t *testing.T) {
	client := RouteClient{Caller: callerFunc(func(_ context.Context, request Request) (Response, error) {
		if err := validateCorrelation(request.CorrelationID); err != nil {
			t.Fatalf("default correlation %q: %v", request.CorrelationID, err)
		}
		payload, err := json.Marshal(RouteCatalogResult{TargetPlugin: request.TargetPlugin, RouteRevision: "revision", Actions: []RouteAction{}})
		if err != nil {
			t.Fatal(err)
		}
		return Response{Version: Version, CorrelationID: request.CorrelationID, Payload: payload}, nil
	})}
	if _, err := client.Catalog(context.Background(), RouteCatalogCall{TargetPlugin: "proof.provider", Origin: routeOrigin(), Deadline: time.Now().Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
}

type callerFunc func(context.Context, Request) (Response, error)

func (f callerFunc) Call(ctx context.Context, request Request) (Response, error) {
	return f(ctx, request)
}

func TestValidateRouteRequestRejectsEachInterfaceFieldAndMissingPayload(t *testing.T) {
	for name, mutate := range map[string]func(*Request){
		"interface id":      func(request *Request) { request.Interface = "other-route" },
		"interface version": func(request *Request) { request.InterfaceVersion++ },
		"missing payload":   func(request *Request) { request.Payload = nil },
	} {
		t.Run(name, func(t *testing.T) {
			request := validRouteRequest(t, RouteMethodCatalog)
			mutate(&request)
			if err := validateRouteRequest(context.Background(), request, time.Now()); err == nil {
				t.Fatal("invalid route request accepted")
			}
		})
	}
}

func TestRouteValidationBoundsAndEnums(t *testing.T) {
	validAction := RouteAction{ActionID: "new", Label: "New", SuggestedPath: []string{"n"}, Available: true}
	validCatalog := RouteCatalog{RouteRevision: "rev-1", Actions: []RouteAction{validAction}}
	if err := ValidateRouteCatalog(validCatalog); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(RouteCatalog) RouteCatalog{
		"empty revision": func(value RouteCatalog) RouteCatalog { value.RouteRevision = ""; return value },
		"too many": func(value RouteCatalog) RouteCatalog {
			value.Actions = make([]RouteAction, MaxRouteActions+1)
			return value
		},
		"duplicate id": func(value RouteCatalog) RouteCatalog {
			value.Actions = append(value.Actions, validAction)
			return value
		},
		"duplicate path": func(value RouteCatalog) RouteCatalog {
			value.Actions = append(value.Actions, RouteAction{ActionID: "fork", Label: "Fork", SuggestedPath: []string{"n"}, Available: true})
			return value
		},
		"bad key":     func(value RouteCatalog) RouteCatalog { value.Actions[0].SuggestedPath = []string{"!"}; return value },
		"empty label": func(value RouteCatalog) RouteCatalog { value.Actions[0].Label = ""; return value },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateRouteCatalog(mutate(cloneRouteCatalog(validCatalog))); err == nil {
				t.Fatal("invalid catalog accepted")
			}
		})
	}
	for _, execution := range []RouteExecution{{Outcome: "unknown", Focus: RouteFocusPreserved}, {Outcome: RouteCompleted, Focus: "unknown"}} {
		if err := ValidateRouteExecution(execution); err == nil {
			t.Fatalf("invalid execution accepted: %+v", execution)
		}
	}
	invocation := RouteInvocation{Origin: routeOrigin(), RouteRevision: "rev-1", ActionID: "new", Keys: []string{"p", "n"}}
	if err := ValidateRouteInvocation(invocation); err != nil {
		t.Fatal(err)
	}
	for _, keys := range [][]string{nil, {"A"}, {"!"}, strings.Fields("0 1 2 3 4 5 6 7 8")} {
		changed := invocation
		changed.Keys = keys
		if err := ValidateRouteInvocation(changed); err == nil {
			t.Fatalf("invalid keys accepted: %q", keys)
		}
	}
}

func TestRouteCatalogAndInvocationExactBoundaries(t *testing.T) {
	actions := make([]RouteAction, MaxRouteActions)
	for index := range actions {
		actions[index] = RouteAction{ActionID: fmt.Sprintf("action-%d", index), Label: strings.Repeat("l", maxRouteLabelBytes), SuggestedPath: routePath(index), Available: true, Reason: strings.Repeat("r", maxRouteReasonBytes)}
	}
	catalog := RouteCatalog{RouteRevision: strings.Repeat("v", maxRouteRevisionBytes), Actions: actions}
	if err := ValidateRouteCatalog(catalog); err != nil {
		t.Fatal(err)
	}
	origin := RouteOrigin{Session: strings.Repeat("s", maxRouteOriginBytes), Workspace: strings.Repeat("w", maxRouteOriginBytes), Tab: strings.Repeat("t", maxRouteOriginBytes), Pane: strings.Repeat("p", maxRouteOriginBytes)}
	invocation := RouteInvocation{Origin: origin, RouteRevision: strings.Repeat("v", maxRouteRevisionBytes), ActionID: "action", Keys: []string{"0", "1", "2", "3", "4", "5", "6", "7"}}
	if err := ValidateRouteInvocation(invocation); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(RouteCatalog) RouteCatalog{
		"label":    func(value RouteCatalog) RouteCatalog { value.Actions[0].Label += "x"; return value },
		"reason":   func(value RouteCatalog) RouteCatalog { value.Actions[0].Reason += "x"; return value },
		"revision": func(value RouteCatalog) RouteCatalog { value.RouteRevision += "x"; return value },
		"path": func(value RouteCatalog) RouteCatalog {
			value.Actions[0].SuggestedPath = []string{"0", "1", "2", "3", "4", "5", "6", "7"}
			return value
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateRouteCatalog(mutate(cloneRouteCatalog(catalog))); err == nil {
				t.Fatal("over-bound catalog accepted")
			}
		})
	}
	invocation.Origin.Pane += "x"
	if err := ValidateRouteInvocation(invocation); err == nil {
		t.Fatal("over-bound origin accepted")
	}
}

func TestRouteCatalogueModelProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		count := rapid.IntRange(0, MaxRouteActions).Draw(t, "count")
		actions := make([]RouteAction, count)
		for index := range count {
			actions[index] = RouteAction{ActionID: fmt.Sprintf("action-%d", index), Label: fmt.Sprintf("Action %d", index), SuggestedPath: routePath(index), Available: index%2 == 0}
		}
		catalog := RouteCatalog{RouteRevision: "revision", Actions: actions}
		if err := ValidateRouteCatalog(catalog); err != nil {
			t.Fatalf("valid model rejected: %v", err)
		}
		if count > 0 {
			duplicate := cloneRouteCatalog(catalog)
			duplicate.Actions = append(duplicate.Actions, duplicate.Actions[0])
			if err := ValidateRouteCatalog(duplicate); err == nil {
				t.Fatal("duplicate model accepted")
			}
		}
	})
}

func routePath(index int) []string {
	alphabet := "0123456789abcdefghijklmnopqrstuvwxyz"
	value := index
	path := []string{string(alphabet[value%len(alphabet)])}
	for value /= len(alphabet); value > 0; value /= len(alphabet) {
		path = append([]string{string(alphabet[value%len(alphabet)])}, path...)
	}
	return path
}

func cloneRouteCatalog(value RouteCatalog) RouteCatalog {
	value.Actions = append([]RouteAction(nil), value.Actions...)
	for index := range value.Actions {
		value.Actions[index].SuggestedPath = append([]string(nil), value.Actions[index].SuggestedPath...)
	}
	return value
}

func validRouteRequest(t *testing.T, method string) Request {
	t.Helper()
	payload := any(routeCatalogPayload{Origin: routeOrigin()})
	if method == RouteMethodInvoke {
		payload = routeInvokePayload{Origin: routeOrigin(), RouteRevision: "rev-proof-1", ActionID: "new", Keys: []string{"p", "n", "a"}}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return Request{Version: Version, TargetPlugin: "proof.provider", Interface: ActionRouteInterface, InterfaceVersion: ActionRouteInterfaceVersion, Method: method, CorrelationID: "correlation", Deadline: time.Now().Add(time.Minute), Payload: data}
}

func routeOrigin() RouteOrigin {
	return RouteOrigin{Session: "proof", Workspace: "w1", Tab: "w1:t1", Pane: "w1:p1"}
}

func TestActionRouteDeclarationIsFixedAndIndependent(t *testing.T) {
	commandLine := []string{"./plugin", "action", "route"}
	action, contract, err := ActionRouteDeclaration(commandLine)
	if err != nil {
		t.Fatal(err)
	}
	commandLine[0] = "changed"
	if action.ID != ActionRouteActionID || action.Title != "Route plugin actions" || !reflect.DeepEqual(action.Contexts, []string{"global"}) || !reflect.DeepEqual(action.Command, []string{"./plugin", "action", "route"}) {
		t.Fatalf("action = %+v", action)
	}
	if contract != (manifest.Interface{ID: ActionRouteInterface, Version: ActionRouteInterfaceVersion, Direction: manifest.InterfaceProvides}) {
		t.Fatalf("contract = %+v", contract)
	}
	if _, _, err := ActionRouteDeclaration(nil); err == nil {
		t.Fatal("empty declaration accepted")
	}
}

func TestRouteProviderTypedErrorIsPreservedWithoutPayload(t *testing.T) {
	provider := errorRouteProvider{err: &CallError{Category: Unavailable, Message: "busy"}}
	response := DispatchRoute(context.Background(), validRouteRequest(t, RouteMethodCatalog), time.Now(), provider)
	if response.Error == nil || response.Error.Category != Unavailable || response.Error.Message != "busy" || len(response.Payload) != 0 {
		t.Fatalf("response = %+v", response)
	}
	provider.err = errors.New("secret detail")
	response = DispatchRoute(context.Background(), validRouteRequest(t, RouteMethodCatalog), time.Now(), provider)
	if response.Error == nil || response.Error.Category != Internal || strings.Contains(response.Error.Message, "secret") {
		t.Fatalf("response = %+v", response)
	}
}

func TestRouteDispatchRejectsStaleMissingAndUnavailableBeforeEffects(t *testing.T) {
	tests := []struct {
		name     string
		revision string
		actionID string
		want     ErrorCategory
	}{
		{name: "stale", revision: "old", actionID: "new", want: Unavailable},
		{name: "missing", revision: "rev-proof-1", actionID: "missing", want: NotFound},
		{name: "unavailable", revision: "rev-proof-1", actionID: "fork", want: Unavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &countingRouteProvider{}
			request := validRouteRequest(t, RouteMethodInvoke)
			payload := routeInvokePayload{Origin: routeOrigin(), RouteRevision: test.revision, ActionID: test.actionID, Keys: []string{"p", "n"}}
			request.Payload, _ = json.Marshal(payload)
			response := DispatchRoute(context.Background(), request, time.Now(), provider)
			if response.Error == nil || response.Error.Category != test.want || provider.invocations != 0 {
				t.Fatalf("response=%+v invocations=%d", response, provider.invocations)
			}
		})
	}
	request := validRouteRequest(t, RouteMethodInvoke)
	request.Payload, _ = json.Marshal(routeInvokePayload{Origin: routeOrigin(), RouteRevision: "rev-proof-1", ActionID: "fork", Keys: []string{"p", "f"}})
	response := DispatchRoute(context.Background(), request, time.Now(), &countingRouteProvider{})
	if response.Error == nil || response.Error.Message != "action-route action is unavailable: source is busy" {
		t.Fatalf("unavailable response = %+v", response)
	}
}

type countingRouteProvider struct{ invocations int }

func (*countingRouteProvider) Catalog(context.Context, RouteOrigin) (RouteCatalog, error) {
	return proofRouteProvider{}.Catalog(context.Background(), RouteOrigin{})
}

func (p *countingRouteProvider) Invoke(context.Context, RouteInvocation) (RouteExecution, error) {
	p.invocations++
	return RouteExecution{Outcome: RouteCompleted, Focus: RouteFocusPreserved}, nil
}

type errorRouteProvider struct{ err error }

type fixedCatalogProvider struct{ catalog RouteCatalog }

func (p fixedCatalogProvider) Catalog(context.Context, RouteOrigin) (RouteCatalog, error) {
	return p.catalog, nil
}

func (fixedCatalogProvider) Invoke(context.Context, RouteInvocation) (RouteExecution, error) {
	return RouteExecution{Outcome: RouteCompleted, Focus: RouteFocusPreserved}, nil
}

func (p errorRouteProvider) Catalog(context.Context, RouteOrigin) (RouteCatalog, error) {
	return RouteCatalog{}, p.err
}
func (p errorRouteProvider) Invoke(context.Context, RouteInvocation) (RouteExecution, error) {
	return RouteExecution{}, p.err
}
