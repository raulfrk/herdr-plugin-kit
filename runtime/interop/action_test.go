package interop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/actionhost"
	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"pgregory.net/rapid"
)

type sequenceRunner struct {
	mu      sync.Mutex
	outputs [][]byte
}

func (r *sequenceRunner) Run(context.Context, string, []string) (command.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.outputs) == 0 {
		return command.Result{}, errors.New("unexpected Herdr call")
	}
	output := r.outputs[0]
	r.outputs = r.outputs[1:]
	return command.Result{Stdout: output}, nil
}

type fakeActionTransport struct {
	mu       sync.Mutex
	actions  []actionhost.Action
	receipts map[string]actionhost.Receipt
	calls    []string
	next     int
	wait     bool
	started  chan struct{}
	initial  func(*actionhost.Receipt)
}

func (f *fakeActionTransport) List(_ context.Context, pluginID string) ([]actionhost.Action, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "list:"+pluginID)
	return append([]actionhost.Action(nil), f.actions...), nil
}

func (f *fakeActionTransport) Invoke(_ context.Context, pluginID, actionID string) (actionhost.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	logID := fmt.Sprintf("log-%d", f.next)
	f.calls = append(f.calls, "invoke:"+pluginID+":"+actionID)
	action := actionID
	receipt := actionhost.Receipt{LogID: logID, PluginID: pluginID, ActionID: &action, Command: []string{"plugin"}, Status: actionhost.Running, StartedUnixMS: 1}
	if f.initial != nil {
		f.initial(&receipt)
	}
	return receipt, nil
}

func (f *fakeActionTransport) AwaitReceipt(ctx context.Context, initial actionhost.Receipt, _ time.Duration) (actionhost.Receipt, error) {
	if f.wait {
		close(f.started)
		<-ctx.Done()
		return actionhost.Receipt{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "await:"+initial.LogID)
	receipt, ok := f.receipts[initial.LogID]
	if !ok {
		return actionhost.Receipt{}, errors.New("missing receipt fixture")
	}
	return receipt, nil
}

func TestActionCapabilitiesExposeOnlyProvenTransport(t *testing.T) {
	want := ActionCapabilities{Discovery: true, Invocation: true, ReceiptCorrelation: true, ResponseEnvelope: true}
	if got := SupportedActionCapabilities(); got != want {
		t.Fatalf("capabilities = %+v, want %+v", got, want)
	}
}

func TestActionDiscoverySelectsOneExactIdentity(t *testing.T) {
	target := actionTarget()
	host := &fakeActionTransport{actions: []actionhost.Action{
		{PluginID: target.PluginID, ActionID: "other", Title: "Other", Command: []string{"plugin"}},
		{PluginID: "foreign.plugin", ActionID: target.ActionID, Title: "Foreign", Command: []string{"plugin"}},
		{PluginID: target.PluginID, ActionID: target.ActionID, Title: "Health", Command: []string{"plugin"}},
	}}
	action, err := (ActionClient{Host: host}).Discover(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if action.PluginID != target.PluginID || action.ActionID != target.ActionID || !reflect.DeepEqual(host.calls, []string{"list:target.plugin"}) {
		t.Fatalf("action=%+v calls=%v", action, host.calls)
	}

	host.actions = append(host.actions, action)
	if _, err := (ActionClient{Host: host}).Discover(context.Background(), target); err == nil {
		t.Fatal("duplicate exact action accepted")
	}
	host.actions = host.actions[:1]
	errTarget := target
	errTarget.ActionID = "missing"
	_, err = (ActionClient{Host: host}).Discover(context.Background(), errTarget)
	assertCallErrorCategory(t, err, NotFound)
}

func TestPropertyActionDiscoveryMatchesExactMultiplicity(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		target := actionTarget()
		matches := rapid.IntRange(0, 3).Draw(t, "matches")
		unrelated := rapid.IntRange(0, 20).Draw(t, "unrelated")
		actions := make([]actionhost.Action, 0, matches+unrelated)
		for i := range unrelated {
			actions = append(actions, actionhost.Action{PluginID: target.PluginID, ActionID: fmt.Sprintf("other-%d", i), Title: "Other", Command: []string{"plugin"}})
		}
		for range matches {
			actions = append(actions, actionhost.Action{PluginID: target.PluginID, ActionID: target.ActionID, Title: "Health", Command: []string{"plugin"}})
		}
		host := &fakeActionTransport{actions: actions}
		action, err := (ActionClient{Host: host}).Discover(context.Background(), target)
		if (err == nil) != (matches == 1) {
			t.Fatalf("matches=%d unrelated=%d action=%+v err=%v", matches, unrelated, action, err)
		}
		if err == nil && action.ActionID != target.ActionID {
			t.Fatalf("selected action = %+v", action)
		}
	})
}

func TestActionInvokeReturnsExactReceiptAndEnvelope(t *testing.T) {
	target := actionTarget()
	stdout := actionResponseJSON(t, target, json.RawMessage(`{"ok":true}`), nil)
	host := &fakeActionTransport{
		actions:  []actionhost.Action{{PluginID: target.PluginID, ActionID: target.ActionID, Title: "Health", Command: []string{"plugin"}}},
		receipts: map[string]actionhost.Receipt{"log-1": terminalReceipt(target, "log-1", actionhost.Succeeded, &stdout)},
	}
	result, err := (ActionClient{Host: host, PollInterval: time.Millisecond}).Invoke(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if result.LogID != "log-1" || !bytes.Equal(result.Response.Payload, json.RawMessage(`{"ok":true}`)) {
		t.Fatalf("result = %+v", result)
	}
	wantCalls := []string{"list:target.plugin", "invoke:target.plugin:health-check", "await:log-1"}
	if !reflect.DeepEqual(host.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", host.calls, wantCalls)
	}
}

func TestActionInvokePreservesValidTypedResponseError(t *testing.T) {
	target := actionTarget()
	stdout := actionResponseJSON(t, target, nil, &CallError{Category: Unavailable, Message: "provider is restarting"})
	host := &fakeActionTransport{
		actions:  []actionhost.Action{{PluginID: target.PluginID, ActionID: target.ActionID, Title: "Health", Command: []string{"plugin"}}},
		receipts: map[string]actionhost.Receipt{"log-1": terminalReceipt(target, "log-1", actionhost.Succeeded, &stdout)},
	}
	result, err := (ActionClient{Host: host, PollInterval: time.Millisecond}).Invoke(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if result.Response.Error == nil || result.Response.Error.Category != Unavailable || len(result.Response.Payload) != 0 {
		t.Fatalf("response = %+v", result.Response)
	}
}

func TestConcurrentActionInvocationsKeepReceiptIsolation(t *testing.T) {
	target := actionTarget()
	stdout := actionResponseJSON(t, target, json.RawMessage(`{"ok":true}`), nil)
	host := &fakeActionTransport{
		actions: []actionhost.Action{{PluginID: target.PluginID, ActionID: target.ActionID, Title: "Health", Command: []string{"plugin"}}},
		receipts: map[string]actionhost.Receipt{
			"log-1": terminalReceipt(target, "log-1", actionhost.Succeeded, &stdout),
			"log-2": terminalReceipt(target, "log-2", actionhost.Succeeded, &stdout),
		},
	}
	client := ActionClient{Host: host, PollInterval: time.Nanosecond}
	results := make(chan ActionResult, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			result, err := client.Invoke(context.Background(), target)
			results <- result
			errs <- err
		}()
	}
	seen := map[string]bool{}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		seen[(<-results).LogID] = true
	}
	if !seen["log-1"] || !seen["log-2"] || len(seen) != 2 {
		t.Fatalf("receipt IDs = %v", seen)
	}
}

func TestActionInvokeHonorsCancellationWhileAwaitingReceipt(t *testing.T) {
	target := actionTarget()
	host := &fakeActionTransport{actions: []actionhost.Action{{PluginID: target.PluginID, ActionID: target.ActionID, Title: "Health", Command: []string{"plugin"}}}, wait: true, started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	defer cancel()
	go func() {
		_, err := (ActionClient{Host: host, PollInterval: time.Hour}).Invoke(ctx, target)
		result <- err
	}()
	select {
	case <-host.started:
	case <-time.After(time.Second):
		t.Fatal("Invoke did not enter receipt waiting")
	}
	cancel()
	var err error
	select {
	case err = <-result:
	case <-time.After(time.Second):
		t.Fatal("Invoke did not return after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestActionInvokeRejectsInvalidTransportOutcomes(t *testing.T) {
	target := actionTarget()
	valid := actionResponseJSON(t, target, json.RawMessage(`{}`), nil)
	tests := []struct {
		name    string
		status  actionhost.Status
		stdout  *string
		mutate  func(*ActionResponse)
		wantCat ErrorCategory
	}{
		{name: "failed action", status: actionhost.Failed, stdout: &valid, wantCat: Internal},
		{name: "missing stdout", status: actionhost.Succeeded, wantCat: InvalidRequest},
		{name: "wrong envelope version", status: actionhost.Succeeded, mutate: func(r *ActionResponse) { r.Version++ }},
		{name: "changed interface", status: actionhost.Succeeded, mutate: func(r *ActionResponse) { r.Interface = "other" }},
		{name: "changed interface version", status: actionhost.Succeeded, mutate: func(r *ActionResponse) { r.InterfaceVersion++ }},
		{name: "changed method", status: actionhost.Succeeded, mutate: func(r *ActionResponse) { r.Method = "other" }},
		{name: "payload and error", status: actionhost.Succeeded, mutate: func(r *ActionResponse) { r.Error = &CallError{Category: Internal, Message: "failed"} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stdout := tc.stdout
			if stdout == nil && tc.status == actionhost.Succeeded && tc.mutate != nil {
				response := validActionResponse(target, json.RawMessage(`{}`), nil)
				tc.mutate(&response)
				encoded, err := json.Marshal(response)
				if err != nil {
					t.Fatal(err)
				}
				value := string(encoded)
				stdout = &value
			}
			host := &fakeActionTransport{
				actions:  []actionhost.Action{{PluginID: target.PluginID, ActionID: target.ActionID, Title: "Health", Command: []string{"plugin"}}},
				receipts: map[string]actionhost.Receipt{"log-1": terminalReceipt(target, "log-1", tc.status, stdout)},
			}
			_, err := (ActionClient{Host: host, PollInterval: time.Millisecond}).Invoke(context.Background(), target)
			if err == nil {
				t.Fatal("invalid action outcome accepted")
			}
			if tc.wantCat != "" {
				assertCallErrorCategory(t, err, tc.wantCat)
			}
		})
	}
}

func TestActionInvokeRejectsChangedReceiptIdentity(t *testing.T) {
	target := actionTarget()
	stdout := actionResponseJSON(t, target, json.RawMessage(`{}`), nil)
	tests := []struct {
		name          string
		mutateInitial func(*actionhost.Receipt)
		mutateFinal   func(*actionhost.Receipt)
	}{
		{name: "empty initial log", mutateInitial: func(r *actionhost.Receipt) { r.LogID = "" }},
		{name: "initial plugin", mutateInitial: func(r *actionhost.Receipt) { r.PluginID = "other.plugin" }},
		{name: "initial action", mutateInitial: func(r *actionhost.Receipt) { other := "other"; r.ActionID = &other }},
		{name: "missing initial action", mutateInitial: func(r *actionhost.Receipt) { r.ActionID = nil }},
		{name: "final log", mutateFinal: func(r *actionhost.Receipt) { r.LogID = "different-log" }},
		{name: "final plugin", mutateFinal: func(r *actionhost.Receipt) { r.PluginID = "other.plugin" }},
		{name: "final action", mutateFinal: func(r *actionhost.Receipt) { other := "other"; r.ActionID = &other }},
		{name: "missing final action", mutateFinal: func(r *actionhost.Receipt) { r.ActionID = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			receipt := terminalReceipt(target, "log-1", actionhost.Succeeded, &stdout)
			if tc.mutateFinal != nil {
				tc.mutateFinal(&receipt)
			}
			host := &fakeActionTransport{
				actions:  []actionhost.Action{{PluginID: target.PluginID, ActionID: target.ActionID, Title: "Health", Command: []string{"plugin"}}},
				receipts: map[string]actionhost.Receipt{"log-1": receipt},
				initial:  tc.mutateInitial,
			}
			if _, err := (ActionClient{Host: host, PollInterval: time.Millisecond}).Invoke(context.Background(), target); err == nil {
				t.Fatal("changed receipt identity accepted")
			}
			if tc.mutateInitial != nil && len(host.calls) != 2 {
				t.Fatalf("invalid initial receipt reached polling: calls = %v", host.calls)
			}
		})
	}
}

func TestDecodeActionResponseIsStrictAndBounded(t *testing.T) {
	target := actionTarget()
	valid := []byte(actionResponseJSON(t, target, json.RawMessage(`{}`), nil))
	for name, input := range map[string][]byte{
		"unknown field":  append(valid[:len(valid)-1], []byte(`,"future":true}`)...),
		"trailing value": append(append([]byte(nil), valid...), []byte(` {}`)...),
		"oversized":      bytes.Repeat([]byte{' '}, MaxActionEnvelopeBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeActionResponse(bytes.NewReader(input), target); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}

	if MaxActionEnvelopeBytes != 64<<10 {
		t.Fatalf("MaxActionEnvelopeBytes = %d", MaxActionEnvelopeBytes)
	}
	exact := append(append([]byte(nil), valid...), bytes.Repeat([]byte{' '}, MaxActionEnvelopeBytes-len(valid))...)
	if _, err := DecodeActionResponse(bytes.NewReader(exact), target); err != nil {
		t.Fatalf("exactly maximum action envelope rejected: %v", err)
	}
}

func TestMaximumActionEnvelopePassesThroughActionHost(t *testing.T) {
	target := actionTarget()
	action := actionhost.Action{PluginID: target.PluginID, ActionID: target.ActionID, Title: "Health", Command: []string{"plugin"}}
	initial := actionhost.Receipt{LogID: "log-1", PluginID: target.PluginID, ActionID: &target.ActionID, Command: []string{"plugin"}, Status: actionhost.Running, StartedUnixMS: 1}
	response := []byte(actionResponseJSON(t, target, json.RawMessage(`{"ok":true}`), nil))
	response = append(response, bytes.Repeat([]byte{' '}, MaxActionEnvelopeBytes-len(response))...)
	stdout := string(response)
	terminal := terminalReceipt(target, "log-1", actionhost.Succeeded, &stdout)

	runner := &sequenceRunner{outputs: [][]byte{
		marshalWire(t, map[string]any{"id": "cli:plugin", "result": map[string]any{"type": "plugin_action_list", "actions": []actionhost.Action{action}}}),
		marshalWire(t, map[string]any{"id": "cli:plugin", "result": map[string]any{"type": "plugin_action_invoked", "action": action, "log": initial}}),
		marshalWire(t, map[string]any{"id": "cli:plugin", "result": map[string]any{"type": "plugin_log_list", "logs": []actionhost.Receipt{terminal}}}),
	}}
	if len(runner.outputs[2]) <= MaxActionEnvelopeBytes || len(runner.outputs[2]) > actionhost.MaxJSONBytes {
		t.Fatalf("wrapped receipt size = %d, action envelope = %d, host bound = %d", len(runner.outputs[2]), MaxActionEnvelopeBytes, actionhost.MaxJSONBytes)
	}
	result, err := (ActionClient{Host: actionhost.Host{Runner: runner}, PollInterval: time.Nanosecond}).Invoke(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if result.LogID != "log-1" || !bytes.Equal(result.Response.Payload, json.RawMessage(`{"ok":true}`)) {
		t.Fatalf("result = %+v", result)
	}
}

func TestActionTargetAndClientFailBeforeHostUse(t *testing.T) {
	host := &fakeActionTransport{}
	target := actionTarget()
	target.InterfaceVersion = 0
	if _, err := (ActionClient{Host: host}).Discover(context.Background(), target); err == nil {
		t.Fatal("zero interface version accepted")
	}
	if len(host.calls) != 0 {
		t.Fatalf("host calls = %v", host.calls)
	}
	for name, interval := range map[string]time.Duration{"zero": 0, "negative": -1} {
		t.Run(name+" poll interval", func(t *testing.T) {
			_, err := (ActionClient{Host: host, PollInterval: interval}).Invoke(context.Background(), actionTarget())
			if err == nil || err.Error() != "poll interval must be positive" {
				t.Fatalf("poll interval error = %v", err)
			}
			if len(host.calls) != 0 {
				t.Fatalf("invalid poll interval reached host: %v", host.calls)
			}
		})
	}
}

func TestActionResponseUsesSharedTypedErrorValidation(t *testing.T) {
	target := actionTarget()
	encoded := actionResponseJSON(t, target, nil, &CallError{Category: "unknown", Message: "failed"})
	if _, err := DecodeActionResponse(strings.NewReader(encoded), target); err == nil {
		t.Fatal("action response bypassed typed-error validation")
	}
}

func actionTarget() ActionTarget {
	return ActionTarget{PluginID: "target.plugin", ActionID: "health-check", Interface: "health", InterfaceVersion: 1, Method: "check"}
}

func validActionResponse(target ActionTarget, payload json.RawMessage, callErr *CallError) ActionResponse {
	return ActionResponse{Version: Version, Interface: target.Interface, InterfaceVersion: target.InterfaceVersion, Method: target.Method, Payload: payload, Error: callErr}
}

func actionResponseJSON(t *testing.T, target ActionTarget, payload json.RawMessage, callErr *CallError) string {
	t.Helper()
	encoded, err := json.Marshal(validActionResponse(target, payload, callErr))
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func terminalReceipt(target ActionTarget, logID string, status actionhost.Status, stdout *string) actionhost.Receipt {
	finished, exit := uint64(2), 0
	return actionhost.Receipt{LogID: logID, PluginID: target.PluginID, ActionID: &target.ActionID, Command: []string{"plugin"}, Status: status, StartedUnixMS: 1, FinishedUnixMS: &finished, ExitCode: &exit, Stdout: stdout}
}

func marshalWire(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
