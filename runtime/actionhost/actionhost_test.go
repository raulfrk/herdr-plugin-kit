package actionhost

import (
	"context"
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

func TestActionHostRejectsOversizedJSON(t *testing.T) {
	runner := &scriptedRunner{outputs: []string{strings.Repeat(" ", MaxJSONBytes+1)}}
	if _, err := (Host{Runner: runner}).List(context.Background(), ""); err == nil {
		t.Fatal("oversized JSON accepted")
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
