package actionhost

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"pgregory.net/rapid"
)

type scriptedRunner struct {
	mu      sync.Mutex
	outputs []string
	calls   [][]string
}

type resultRunner struct {
	result command.Result
}

func (r *resultRunner) Run(_ context.Context, _ string, _ []string) (command.Result, error) {
	return r.result, nil
}

func (r *scriptedRunner) Run(_ context.Context, executable string, args []string) (command.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{executable}, args...))
	if len(r.outputs) == 0 {
		return command.Result{}, fmt.Errorf("unexpected call")
	}
	out := r.outputs[0]
	r.outputs = r.outputs[1:]
	return command.Result{Stdout: []byte(out), ExitCode: 0}, nil
}

func TestListUsesStablePluginIdentity(t *testing.T) {
	runner := &scriptedRunner{outputs: []string{`{"id":"cli:plugin","result":{"type":"plugin_action_list","actions":[{"plugin_id":"demo.plugin","action_id":"open","title":"Open","command":["bin"]}]}}`}}
	actions, err := (Host{Runner: runner, Herdr: "/bin/herdr"}).List(context.Background(), "demo.plugin")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].PluginID != "demo.plugin" || actions[0].ActionID != "open" {
		t.Fatalf("actions = %+v", actions)
	}
	want := []string{"/bin/herdr", "plugin", "action", "list", "--plugin", "demo.plugin"}
	if !reflect.DeepEqual(runner.calls[0], want) {
		t.Fatalf("call = %q, want %q", runner.calls[0], want)
	}
}

func TestListRejectsInvalidPluginFilterBeforeRunningHerdr(t *testing.T) {
	runner := &scriptedRunner{}
	if _, err := (Host{Runner: runner}).List(context.Background(), "Invalid Plugin"); err == nil {
		t.Fatal("invalid plugin filter accepted")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("Herdr called %d times for invalid input", len(runner.calls))
	}
}

func TestListRejectsChangedPluginIdentity(t *testing.T) {
	action := validAction()
	action.PluginID = "other.plugin"
	runner := &scriptedRunner{outputs: []string{listJSON(t, action)}}
	if _, err := (Host{Runner: runner}).List(context.Background(), "demo.plugin"); err == nil {
		t.Fatal("action from a different plugin accepted")
	}
}

func TestListActionBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Action)
		wantErr bool
	}{
		{name: "empty title", mutate: func(action *Action) { action.Title = "" }, wantErr: true},
		{name: "128-byte title", mutate: func(action *Action) { action.Title = strings.Repeat("t", 128) }},
		{name: "128 command elements", mutate: func(action *Action) { action.Command = repeatedCommand(128) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			action := validAction()
			tc.mutate(&action)
			runner := &scriptedRunner{outputs: []string{listJSON(t, action)}}
			_, err := (Host{Runner: runner}).List(context.Background(), "demo.plugin")
			if (err != nil) != tc.wantErr {
				t.Fatalf("List() error = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

func TestInvokePreservesExactLogID(t *testing.T) {
	runner := &scriptedRunner{outputs: []string{invokeJSON("running", "opaque receipt/☃", nil)}}
	receipt, err := (Host{Runner: runner}).Invoke(context.Background(), "demo.plugin", "open")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.LogID != "opaque receipt/☃" {
		t.Fatalf("log id = %q", receipt.LogID)
	}
	want := []string{"herdr", "plugin", "action", "invoke", "open", "--plugin", "demo.plugin"}
	if !reflect.DeepEqual(runner.calls[0], want) {
		t.Fatalf("call = %q, want %q", runner.calls[0], want)
	}
}

func TestReceiptSelectsExactLogID(t *testing.T) {
	wanted := receiptJSON("succeeded", "wanted receipt", intPointer(0))
	unrelated := receiptJSON("running", "newer-unrelated", nil)
	runner := &scriptedRunner{outputs: []string{fmt.Sprintf(`{"id":"cli:plugin","result":{"type":"plugin_log_list","logs":[%s,%s]}}`, unrelated, wanted)}}
	receipt, err := (Host{Runner: runner}).Receipt(context.Background(), "demo.plugin", "open", "wanted receipt")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.LogID != "wanted receipt" || receipt.Status != Succeeded {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestInvokeRejectsChangedIdentity(t *testing.T) {
	response := strings.Replace(invokeJSON("running", "receipt-1", nil), `"plugin_id":"demo.plugin"`, `"plugin_id":"other.plugin"`, 1)
	if _, err := (Host{Runner: &scriptedRunner{outputs: []string{response}}}).Invoke(context.Background(), "demo.plugin", "open"); err == nil {
		t.Fatal("changed plugin identity accepted")
	}
}

func TestInvokeRejectsMalformedAction(t *testing.T) {
	response := strings.Replace(invokeJSON("running", "receipt-1", nil), `"command":["bin"]`, `"command":[]`, 1)
	if _, err := (Host{Runner: &scriptedRunner{outputs: []string{response}}}).Invoke(context.Background(), "demo.plugin", "open"); err == nil {
		t.Fatal("invoke response with an empty action command was accepted")
	}
}

func TestInvokeReceiptBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Receipt)
		wantErr bool
	}{
		{name: "empty log ID", mutate: func(receipt *Receipt) { receipt.LogID = "" }, wantErr: true},
		{name: "missing action ID", mutate: func(receipt *Receipt) { receipt.ActionID = nil }, wantErr: true},
		{name: "empty command", mutate: func(receipt *Receipt) { receipt.Command = nil }, wantErr: true},
		{name: "128 command elements", mutate: func(receipt *Receipt) { receipt.Command = repeatedCommand(128) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			receipt := validReceipt()
			tc.mutate(&receipt)
			runner := &scriptedRunner{outputs: []string{invokeResponseJSON(t, validAction(), receipt)}}
			_, err := (Host{Runner: runner}).Invoke(context.Background(), "demo.plugin", "open")
			if (err != nil) != tc.wantErr {
				t.Fatalf("Invoke() error = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

func TestActionHostRejectsOversizedJSON(t *testing.T) {
	runner := &scriptedRunner{outputs: []string{strings.Repeat(" ", MaxJSONBytes+1)}}
	if _, err := (Host{Runner: runner}).List(context.Background(), ""); err == nil {
		t.Fatal("oversized JSON accepted")
	}
}

func TestActionHostAcceptsJSONAtSizeLimit(t *testing.T) {
	response := `{"id":"cli:plugin","result":{"type":"plugin_action_list","actions":[]}}`
	response += strings.Repeat(" ", MaxJSONBytes-len(response))
	runner := &scriptedRunner{outputs: []string{response}}
	if _, err := (Host{Runner: runner}).List(context.Background(), ""); err != nil {
		t.Fatalf("JSON at size limit rejected: %v", err)
	}
}

func TestActionHostRejectsEitherTruncatedStream(t *testing.T) {
	validJSON := []byte(`{"id":"cli:plugin","result":{"type":"plugin_action_list","actions":[]}}`)
	tests := []struct {
		name   string
		result command.Result
	}{
		{name: "stdout", result: command.Result{Stdout: validJSON, StdoutTruncated: true}},
		{name: "stderr", result: command.Result{Stdout: validJSON, StderrTruncated: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &resultRunner{result: tc.result}
			if _, err := (Host{Runner: runner}).List(context.Background(), ""); err == nil {
				t.Fatal("truncated Herdr output accepted")
			}
		})
	}
}

func TestStrictJSONRejectsUnknownField(t *testing.T) {
	runner := &scriptedRunner{outputs: []string{`{"id":"cli:plugin","result":{"type":"plugin_action_list","actions":[],"future":true}}`}}
	if _, err := (Host{Runner: runner}).List(context.Background(), ""); err == nil {
		t.Fatal("unknown response field accepted")
	}
}

func TestModelReceiptLifecycle(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		polls := rapid.IntRange(1, 6).Draw(t, "running_polls")
		terminal := rapid.SampledFrom([]Status{Succeeded, Failed}).Draw(t, "terminal")
		outputs := make([]string, polls)
		for i := 0; i < polls-1; i++ {
			outputs[i] = logsJSON("running", "receipt-7", nil)
		}
		exit := 0
		if terminal == Failed {
			exit = 9
		}
		outputs[polls-1] = logsJSON(string(terminal), "receipt-7", &exit)
		runner := &scriptedRunner{outputs: outputs}
		actionID := "open"
		initial := Receipt{LogID: "receipt-7", PluginID: "demo.plugin", ActionID: &actionID, Command: []string{"bin"}, Status: Running, StartedUnixMS: 1}
		got, err := (Host{Runner: runner}).AwaitReceipt(context.Background(), initial, time.Nanosecond)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != terminal || got.LogID != initial.LogID || len(runner.calls) != polls {
			t.Fatalf("terminal=%s got=%+v polls=%d", terminal, got, len(runner.calls))
		}
	})
}

func TestAwaitReceiptHonorsCancellation(t *testing.T) {
	actionID := "open"
	initial := Receipt{LogID: "receipt-7", PluginID: "demo.plugin", ActionID: &actionID, Command: []string{"bin"}, Status: Running, StartedUnixMS: 1}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (Host{Runner: &scriptedRunner{}}).AwaitReceipt(ctx, initial, time.Hour)
	if err != context.Canceled {
		t.Fatalf("error = %v", err)
	}
}

func TestAwaitReceiptRejectsZeroInterval(t *testing.T) {
	actionID := "open"
	initial := Receipt{LogID: "receipt-7", PluginID: "demo.plugin", ActionID: &actionID, Command: []string{"bin"}, Status: Running, StartedUnixMS: 1}
	runner := &scriptedRunner{}
	if _, err := (Host{Runner: runner}).AwaitReceipt(context.Background(), initial, 0); err == nil {
		t.Fatal("zero poll interval accepted")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("Herdr called %d times for invalid interval", len(runner.calls))
	}
}

func TestReceiptLogIDBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		logID   string
		wantErr bool
	}{
		{name: "empty", logID: "", wantErr: true},
		{name: "NUL", logID: "receipt\x00id", wantErr: true},
		{name: "129 bytes", logID: strings.Repeat("r", 129), wantErr: true},
		{name: "128 bytes", logID: strings.Repeat("r", 128)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &scriptedRunner{}
			if !tc.wantErr {
				receipt := validReceipt()
				receipt.LogID = tc.logID
				runner.outputs = []string{logsResponseJSON(t, receipt)}
			}
			_, err := (Host{Runner: runner}).Receipt(context.Background(), "demo.plugin", "open", tc.logID)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Receipt() error = %v, wantErr %t", err, tc.wantErr)
			}
			if tc.wantErr && len(runner.calls) != 0 {
				t.Fatalf("Herdr called %d times for invalid log ID", len(runner.calls))
			}
		})
	}
}

func invokeJSON(status, logID string, exit *int) string {
	return fmt.Sprintf(`{"id":"cli:plugin","result":{"type":"plugin_action_invoked","action":{"plugin_id":"demo.plugin","action_id":"open","title":"Open","command":["bin"]},"context":{},"log":%s}}`, receiptJSON(status, logID, exit))
}

func logsJSON(status, logID string, exit *int) string {
	return fmt.Sprintf(`{"id":"cli:plugin","result":{"type":"plugin_log_list","logs":[%s]}}`, receiptJSON(status, logID, exit))
}

func receiptJSON(status, logID string, exit *int) string {
	exitJSON := "null"
	finished := "null"
	if exit != nil {
		exitJSON = fmt.Sprint(*exit)
		finished = "2"
	}
	return fmt.Sprintf(`{"log_id":%q,"plugin_id":"demo.plugin","action_id":"open","command":["bin"],"status":%q,"started_unix_ms":1,"finished_unix_ms":%s,"exit_code":%s,"stdout":"","stderr":"","error":null,"event":null}`, logID, status, finished, exitJSON)
}

func intPointer(value int) *int { return &value }

func validAction() Action {
	return Action{PluginID: "demo.plugin", ActionID: "open", Title: "Open", Command: []string{"bin"}}
}

func validReceipt() Receipt {
	actionID := "open"
	return Receipt{LogID: "receipt-1", PluginID: "demo.plugin", ActionID: &actionID, Command: []string{"bin"}, Status: Running, StartedUnixMS: 1}
}

func repeatedCommand(count int) []string {
	command := make([]string, count)
	for i := range command {
		command[i] = "arg"
	}
	return command
}

func listJSON(t *testing.T, actions ...Action) string {
	t.Helper()
	response := struct {
		ID     string `json:"id"`
		Result struct {
			Type    string   `json:"type"`
			Actions []Action `json:"actions"`
		} `json:"result"`
	}{ID: "cli:plugin"}
	response.Result.Type = "plugin_action_list"
	response.Result.Actions = actions
	return marshalJSON(t, response)
}

func invokeResponseJSON(t *testing.T, action Action, receipt Receipt) string {
	t.Helper()
	var response invokeResponse
	response.ID = "cli:plugin"
	response.Result.Type = "plugin_action_invoked"
	response.Result.Action = action
	response.Result.Log = receipt
	return marshalJSON(t, response)
}

func logsResponseJSON(t *testing.T, receipts ...Receipt) string {
	t.Helper()
	response := struct {
		ID     string `json:"id"`
		Result struct {
			Type string    `json:"type"`
			Logs []Receipt `json:"logs"`
		} `json:"result"`
	}{ID: "cli:plugin"}
	response.Result.Type = "plugin_log_list"
	response.Result.Logs = receipts
	return marshalJSON(t, response)
}

func marshalJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
