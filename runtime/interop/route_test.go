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
		"missing":  func(envelope *routePluginListEnvelope, _ string) { envelope.Result.Plugins[0].PluginID = "proof.other" },
		"disabled": func(envelope *routePluginListEnvelope, _ string) { envelope.Result.Plugins[0].Enabled = false },
		"manifest path": func(envelope *routePluginListEnvelope, root string) {
			envelope.Result.Plugins[0].ManifestPath = filepath.Join(root, "other.toml")
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

type callerFunc func(context.Context, Request) (Response, error)

func (f callerFunc) Call(ctx context.Context, request Request) (Response, error) {
	return f(ctx, request)
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
