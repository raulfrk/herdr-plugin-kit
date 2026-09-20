package sessionhost

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

// Change one observed field at a time: accepting any of these receipts could
// focus or close a different target, or deliver a command before readiness.
func TestLaunchRejectsEachCreationReceiptDrift(t *testing.T) {
	for _, placement := range []Placement{NewTab, NewPane} {
		for _, field := range []string{"type", "workspace", "tab", "pane", "cwd", "focused", "malformed"} {
			t.Run(string(placement)+"/"+field, func(t *testing.T) {
				origin := Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"}
				request := LaunchRequest{Origin: origin, Argv: []string{"agent"}, Directory: "/tmp/project", Placement: placement}
				runner := &scriptedRunner{outputs: []string{paneJSON("w1:p1", "w1", "w1:t1", "/tmp", true)}}
				host := Host{Runner: runner}
				if placement == NewTab {
					result := layoutResult(request.Argv).(map[string]any)
					layout := result["layout"].(map[string]any)
					root := layout["root"].(map[string]any)
					switch field {
					case "type":
						result["type"] = "wrong"
					case "workspace":
						layout["workspace_id"] = "other"
					case "tab":
						layout["tab_id"] = "w1:t1"
					case "pane":
						root["pane_id"] = ""
					case "cwd":
						root["cwd"] = "/other"
					case "focused":
						layout["focused_pane_id"] = "other"
					case "malformed":
						root["type"] = "split"
					}
					host.api = &fakeAPI{result: result}
					runner.outputs = append(runner.outputs, sessionListJSON("default", "/tmp/herdr.sock", false), paneJSON("w1:p1", "w1", "w1:t1", "/tmp", true))
				} else {
					request.Direction = "right"
					result := paneResult("w1:p2", "w1", "w1:t1", "/tmp/project", false).(map[string]any)
					pane := result["pane"].(map[string]any)
					switch field {
					case "type":
						result["type"] = "wrong"
					case "workspace":
						pane["workspace_id"] = "other"
					case "tab":
						pane["tab_id"] = "other"
					case "pane":
						pane["pane_id"] = "w1:p1"
					case "cwd":
						pane["cwd"] = "/other"
					case "focused":
						pane["focused"] = true
					case "malformed":
						result["pane"] = 42
					}
					encoded, _ := json.Marshal(map[string]any{"id": "test", "result": result})
					runner.outputs = append(runner.outputs, string(encoded))
				}
				_, err := host.Launch(context.Background(), request)
				if !errors.Is(err, ErrStaleTarget) {
					t.Fatalf("receipt accepted: %v", err)
				}
				for _, call := range runner.calls {
					if len(call) > 4 && call[3] == "pane" && (call[4] == "run" || call[4] == "process-info") {
						t.Fatalf("continued launch after invalid receipt: %q", call)
					}
				}
			})
		}
	}
}

func TestFocusRejectsReceiptAndPostObservationDrift(t *testing.T) {
	for _, field := range []string{"type", "pane", "workspace", "tab", "focused", "post-unfocused", "post-error"} {
		t.Run(field, func(t *testing.T) {
			target := Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}
			result := paneResult("w1:p2", "w1", "w1:t1", "/tmp", true).(map[string]any)
			pane := result["pane"].(map[string]any)
			switch field {
			case "type":
				result["type"] = "wrong"
			case "pane":
				pane["pane_id"] = "other"
			case "workspace":
				pane["workspace_id"] = "other"
			case "tab":
				pane["tab_id"] = "other"
			case "focused":
				pane["focused"] = false
			}
			outputs := []string{paneJSON("w1:p2", "w1", "w1:t1", "/tmp", false), sessionListJSON("default", "/tmp/herdr.sock", false)}
			if field == "post-unfocused" {
				outputs = append(outputs, paneJSON("w1:p2", "w1", "w1:t1", "/tmp", false))
			}
			if field == "post-error" {
				outputs = append(outputs, `{}`)
			}
			if field != "post-error" && field != "post-unfocused" {
				outputs = append(outputs, paneJSON("w1:p2", "w1", "w1:t1", "/tmp", true))
			}
			err := (Host{Runner: &scriptedRunner{outputs: outputs}, api: &fakeAPI{result: result}}).FocusPane(context.Background(), target)
			if !errors.Is(err, ErrStaleTarget) {
				t.Fatalf("drift accepted: %v", err)
			}
		})
	}
}

func TestLaunchDoesNotCleanPartiallyDecodedOrUnownedReceipt(t *testing.T) {
	for _, receipt := range []string{
		`{"id":"test","result":{"type":"pane_info","pane":{"pane_id":"w1:p2","workspace_id":"w1","tab_id":"w1:t1","cwd":42}}}`,
		paneJSON("w1:p2", "other", "w1:t1", "/tmp/project", false),
		paneJSON("w1:p2", "w1", "other", "/tmp/project", false),
		paneJSON("w1:p1", "w1", "w1:t1", "/tmp/project", false),
	} {
		runner := &scriptedRunner{outputs: []string{paneJSON("w1:p1", "w1", "w1:t1", "/tmp", true), receipt}}
		_, err := (Host{Runner: runner}).Launch(context.Background(), LaunchRequest{Origin: Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"}, Argv: []string{"agent"}, Directory: "/tmp/project", Placement: NewPane, Direction: "right"})
		if !errors.Is(err, ErrStaleTarget) || len(runner.calls) != 2 {
			t.Fatalf("untrusted receipt caused additional action: err=%v calls=%q", err, runner.calls)
		}
	}
}

func TestCleanupAcknowledgementAndDeadline(t *testing.T) {
	target := CreatedTarget{Origin: Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"}}
	for _, ack := range []string{okJSON(), `true`, `{"id":"test","result":{"type":"wrong"}}`} {
		runner := &scriptedRunner{outputs: []string{paneJSON("w1:p2", "w1", "w1:t1", "/tmp", false), ack}}
		err := (Host{Runner: runner}).Cleanup(context.Background(), target)
		if (err == nil) != (ack == okJSON()) {
			t.Fatalf("ack %s error %v", ack, err)
		}
	}
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	runner := runnerFunc(func(ctx context.Context, _ string, args []string) (command.Result, error) {
		deadline, ok := ctx.Deadline()
		remaining := time.Until(deadline)
		if ctx.Err() != nil || !ok || remaining < 4*time.Second || remaining > 5*time.Second {
			t.Fatalf("cleanup context: %v %s", ctx.Err(), remaining)
		}
		out := okJSON()
		plain := withoutSessionSelection(args)
		if len(plain) >= 2 && plain[0] == "pane" && plain[1] == "get" {
			out = paneJSON("w1:p2", "w1", "w1:t1", "/tmp", false)
		}
		return command.Result{Stdout: []byte(out)}, nil
	})
	if err := (Host{Runner: runner}).cleanupAfterFailure(parent, target); err != nil {
		t.Fatal(err)
	}
}

func TestShellReadinessRejectsEachInvalidObservation(t *testing.T) {
	for _, field := range []string{"pane", "zero-pid", "negative-pid", "group", "multiple", "pid", "cwd", "shell"} {
		t.Run(field, func(t *testing.T) {
			var response map[string]any
			if err := json.Unmarshal([]byte(processInfoJSON("w1:p2", "/tmp", "zsh", true)), &response); err != nil {
				t.Fatal(err)
			}
			info := response["result"].(map[string]any)["process_info"].(map[string]any)
			processes := info["foreground_processes"].([]any)
			process := processes[0].(map[string]any)
			pending := false
			switch field {
			case "pane":
				info["pane_id"] = "other"
			case "zero-pid":
				info["shell_pid"] = 0
			case "negative-pid":
				info["shell_pid"] = -1
			case "group":
				info["foreground_process_group_id"] = 102
				pending = true
			case "multiple":
				info["foreground_processes"] = append(processes, process)
				pending = true
			case "pid":
				process["pid"] = 102
				pending = true
			case "cwd":
				process["cwd"] = "/other"
			case "shell":
				process["name"] = "fish"
			}
			encoded, _ := json.Marshal(response)
			waited := false
			host := Host{Runner: &scriptedRunner{outputs: []string{string(encoded)}}, wait: func(ctx context.Context, d time.Duration) error {
				waited = true
				deadline, ok := ctx.Deadline()
				remaining := time.Until(deadline)
				if d != 25*time.Millisecond || !ok || remaining < 4*time.Second || remaining > 5*time.Second {
					t.Fatalf("readiness timing %s %s", d, remaining)
				}
				return context.DeadlineExceeded
			}}
			err := host.waitForShell(context.Background(), Origin{PaneID: "w1:p2"}, "/tmp")
			if err == nil || waited != pending {
				t.Fatalf("err=%v waited=%v", err, waited)
			}
		})
	}
}

func TestLaunchContractBoundaries(t *testing.T) {
	base := LaunchRequest{Origin: Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"}, Argv: []string{"agent"}, Directory: "/tmp", Placement: NewTab}
	for _, size := range []int{256, 257} {
		r := base
		r.TabLabel = strings.Repeat("x", size)
		if err := (Host{}).validateLaunch(r); (err == nil) != (size == 256) {
			t.Fatalf("label size %d: %v", size, err)
		}
		r = base
		r.Origin.PaneID = strings.Repeat("x", size)
		if err := (Host{}).validateLaunch(r); (err == nil) != (size == 256) {
			t.Fatalf("ID size %d: %v", size, err)
		}
	}
	for _, size := range []int{128, 129} {
		r := base
		r.Argv = make([]string, size)
		r.Argv[0] = "agent"
		if err := (Host{}).validateLaunch(r); (err == nil) != (size == 128) {
			t.Fatalf("argc %d: %v", size, err)
		}
	}
	for _, size := range []int{4096, 4097} {
		r := base
		r.Argv = []string{strings.Repeat("x", size)}
		if err := (Host{}).validateLaunch(r); (err == nil) != (size == 4096) {
			t.Fatalf("arg size %d: %v", size, err)
		}
	}
	for _, field := range []string{"label", "pane", "empty-pane", "session"} {
		r := base
		host := Host{}
		switch field {
		case "label":
			r.TabLabel = "x\x00"
		case "pane":
			r.Origin.PaneID = "x\x00"
		case "empty-pane":
			r.Origin.PaneID = ""
		case "session":
			host.Session = "../bad"
			r.Origin.SessionName = host.Session
		}
		if err := host.validateLaunch(r); err == nil {
			t.Fatalf("accepted %s", field)
		}
	}
	for _, size := range []int{65529, 65530} {
		_, err := encodeExec([]string{strings.Repeat("x", size)})
		if (err == nil) != (size == 65529) {
			t.Fatalf("encoded boundary %d: %v", size, err)
		}
	}
	r := base
	r.Placement = NewPane
	r.Direction = "right"
	r.Argv = make([]string, 20)
	for i := range r.Argv {
		r.Argv[i] = strings.Repeat("x", 4096)
	}
	if err := (Host{}).validateLaunch(r); err == nil {
		t.Fatal("oversized terminal command accepted")
	}
}

func TestSessionSocketSelectionAndInvalidPaths(t *testing.T) {
	sessions := `{"sessions":[{"name":"default","default":true,"running":true,"socket_path":"/tmp/default.sock"},{"name":"named","default":false,"running":true,"socket_path":"/tmp/named.sock"}]}`
	for _, name := range []string{"", "named"} {
		want := "/tmp/default.sock"
		if name != "" {
			want = "/tmp/named.sock"
		}
		got, err := (Host{Session: name, Runner: &scriptedRunner{outputs: []string{sessions}}}).Socket(context.Background())
		if err != nil || got != want {
			t.Fatalf("%q: %q %v", name, got, err)
		}
	}
	for _, path := range []string{"", "relative", "/tmp/../bad", "/tmp/\x00bad"} {
		output := sessionListJSON("default", path, false)
		if _, err := (Host{Runner: &scriptedRunner{outputs: []string{output}}}).sessionSocket(context.Background()); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
}

func TestCleanupOwnershipRequiresCompleteDistinctLocation(t *testing.T) {
	origin := Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"}
	for _, tab := range []bool{false, true} {
		valid := CreatedTarget{Origin: origin}
		valid.PaneID = "w1:p2"
		if tab {
			valid.TabID = "w1:t2"
		}
		check := plausibleCreatedPane
		if tab {
			check = plausibleCreatedTab
		}
		if !check(valid, origin) {
			t.Fatal("valid ownership rejected")
		}
		for _, field := range []string{"session", "workspace", "tab", "empty-tab", "pane"} {
			bad := valid
			switch field {
			case "session":
				bad.SessionName = "other"
			case "workspace":
				bad.WorkspaceID = "other"
			case "tab":
				if tab {
					bad.TabID = origin.TabID
				} else {
					bad.TabID = "other"
				}
			case "empty-tab":
				bad.TabID = ""
			case "pane":
				bad.PaneID = ""
			}
			if check(bad, origin) {
				t.Fatalf("accepted ownership tab=%v field=%s", tab, field)
			}
		}
	}
	runner := &scriptedRunner{outputs: []string{paneJSON("w1:p2", "w1", "other", "/tmp", false)}}
	_, err := (Host{Runner: runner}).currentPane(context.Background(), Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"})
	if !errors.Is(err, ErrStaleTarget) || !reflect.DeepEqual(runner.calls, [][]string{{"herdr", "--session", "default", "pane", "get", "w1:p2"}}) {
		t.Fatalf("location drift %v", err)
	}
}

func TestNamedSessionDoesNotSelectAnotherOrDefaultSession(t *testing.T) {
	for _, output := range []string{sessionListJSON("other", "/tmp/other.sock", true), sessionListJSON("named", "/tmp/default.sock", false)} {
		_, err := (Host{Session: "named", Runner: &scriptedRunner{outputs: []string{output}}}).sessionSocket(context.Background())
		if !errors.Is(err, ErrStaleTarget) {
			t.Fatalf("incorrect session accepted: %s err=%v", output, err)
		}
	}
}

func TestCurrentPaneRejectsDifferentPaneInSameTab(t *testing.T) {
	runner := &scriptedRunner{outputs: []string{paneJSON("w1:p3", "w1", "w1:t1", "/tmp", false)}}
	_, err := (Host{Runner: runner}).currentPane(context.Background(), Origin{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2"})
	if !errors.Is(err, ErrStaleTarget) {
		t.Fatalf("different pane accepted: %v", err)
	}
}
