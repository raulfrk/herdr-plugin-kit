package agenthost

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func TestMatchesRequestedPolicyComparesEveryField(t *testing.T) {
	effective := managedPreflight("/tmp/project").EffectiveCodexPolicy
	if !matchesRequestedPolicy(effective, managedPolicy()) {
		t.Fatal("identical policy rejected")
	}
	for name, mutate := range map[string]func(*ManagedCodexPolicy){
		"model":     func(p *ManagedCodexPolicy) { p.Model = "other" },
		"reasoning": func(p *ManagedCodexPolicy) { p.ReasoningEffort = "high" },
		"approval":  func(p *ManagedCodexPolicy) { p.ApprovalPolicy = "on-request" },
		"sandbox":   func(p *ManagedCodexPolicy) { p.SandboxMode = "read-only" },
	} {
		requested := managedPolicy()
		mutate(&requested)
		if matchesRequestedPolicy(effective, requested) {
			t.Fatalf("%s difference accepted", name)
		}
	}
}

func TestExplicitSandboxArgvRequiresOneUnambiguousSandboxFlag(t *testing.T) {
	for _, test := range []struct {
		name string
		argv []string
		want bool
	}{
		{"short flag", []string{"codex", "-s", "read-only"}, true},
		{"long flag", []string{"codex", "--sandbox", "read-only"}, true},
		{"other flag before the value", []string{"codex", "--search", "read-only", "tail"}, false},
		{"different sandbox", []string{"codex", "-s", "workspace-write"}, false},
		{"repeated flag", []string{"codex", "-s", "read-only", "--sandbox", "read-only"}, false},
		{"unrelated config override", []string{"codex", "-c", "model=x", "-s", "read-only"}, true},
		{"default permissions override", []string{"codex", "-c", `default_permissions="x"`, "-s", "read-only"}, false},
		{"permissions override", []string{"codex", "-c", "permissions.net=1", "-s", "read-only"}, false},
		{"profile", []string{"codex", "-p", "x", "-s", "read-only"}, false},
		{"flag without a value", []string{"codex", "-s"}, false},
		{"config override without a value", []string{"codex", "-c"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := explicitSandboxArgv(test.argv, "read-only"); got != test.want {
				t.Fatalf("explicitSandboxArgv(%q)=%t, want %t", test.argv, got, test.want)
			}
		})
	}
}

func TestSameManagedTargetRequiresEveryField(t *testing.T) {
	target := managedOrigin()
	agent := managedAgent(target, "/tmp/project", managedNewID, false)
	if !sameManagedTarget(agent, target, "/tmp/project", false, managedNewID) {
		t.Fatal("matching agent rejected")
	}
	for name, mutate := range map[string]func(*Agent){
		"kind":         func(a *Agent) { a.Kind = "claude" },
		"session name": func(a *Agent) { a.SessionName = "other" },
		"workspace":    func(a *Agent) { a.WorkspaceID = "w2" },
		"tab":          func(a *Agent) { a.TabID = "w1:t9" },
		"pane":         func(a *Agent) { a.PaneID = "w1:p9" },
		"directory":    func(a *Agent) { a.CWD = "/tmp/other" },
		"focus":        func(a *Agent) { a.Focused = true },
		"no session":   func(a *Agent) { a.Session = nil },
		"source":       func(a *Agent) { a.Session.Source = "other" },
		"agent":        func(a *Agent) { a.Session.Agent = "other" },
		"kind of ref":  func(a *Agent) { a.Session.Kind = "name" },
		"thread":       func(a *Agent) { a.Session.Value = managedChildID },
	} {
		changed := managedAgent(target, "/tmp/project", managedNewID, false)
		mutate(&changed)
		if sameManagedTarget(changed, target, "/tmp/project", false, managedNewID) {
			t.Fatalf("%s difference accepted", name)
		}
	}
}

func TestManagedGetStartedAgentRefusesAnyUnsettledResponse(t *testing.T) {
	target := managedOrigin()
	agent := managedAgent(target, "/tmp/project", managedNewID, false)
	for _, test := range []struct {
		name   string
		mutate func(*agentInfoResponse)
		valid  bool
	}{
		{"settled", func(*agentInfoResponse) {}, true},
		{"command id", func(r *agentInfoResponse) { r.ID = "cli:agent:list" }, false},
		{"result type", func(r *agentInfoResponse) { r.Result.Type = "agent_list" }, false},
		{"launch pending", func(r *agentInfoResponse) { r.Result.Agent.LaunchPending = true }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var response agentInfoResponse
			response.ID, response.Result.Type, response.Result.Agent = "cli:agent:get", "agent_info", wireFromAgent(agent)
			test.mutate(&response)
			launcher := NewManagedCodexLauncher(Host{Runner: &scriptedRunner{results: []command.Result{jsonResult(t, response)}}, Session: "named"})
			got, err := launcher.getStartedAgent(context.Background(), target, "/tmp/project", false, managedNewID)
			if test.valid && (err != nil || got.PaneID != target.PaneID) {
				t.Fatalf("agent=%+v error=%v", got, err)
			}
			if !test.valid && !errors.Is(err, ErrManagedStale) {
				t.Fatalf("agent=%+v error=%v", got, err)
			}
		})
	}
}

// managedCleanupErrTopology fails Cleanup with scripted errors and otherwise records like the shared fake.
type managedCleanupErrTopology struct {
	*fakeManagedTopology
	cleanupErrs []error
}

func (t *managedCleanupErrTopology) Cleanup(ctx context.Context, target sessionhost.CreatedTarget) error {
	err := t.fakeManagedTopology.Cleanup(ctx, target)
	if len(t.cleanupErrs) != 0 {
		err = t.cleanupErrs[0]
		t.cleanupErrs = t.cleanupErrs[1:]
	}
	return err
}

func TestManagedCleanupKeepsCreatedPaneUntilItsRemovalSucceeds(t *testing.T) {
	topology := &managedCleanupErrTopology{fakeManagedTopology: &fakeManagedTopology{}, cleanupErrs: []error{errors.New("close failed")}}
	cleanup := &managedCleanup{topology: topology, target: sessionhost.CreatedTarget{Origin: managedOrigin()}, created: true}
	if err := cleanup.stopTarget(context.Background()); err == nil || !cleanup.created {
		t.Fatalf("failed removal error=%v created=%t", err, cleanup.created)
	}
	if err := cleanup.stopTarget(context.Background()); err != nil || cleanup.created {
		t.Fatalf("retry error=%v created=%t", err, cleanup.created)
	}
	if err := cleanup.stopTarget(context.Background()); err != nil || len(topology.cleanups) != 2 {
		t.Fatalf("repeat error=%v removals=%d", err, len(topology.cleanups))
	}
}

func TestManagedCleanupKeepsResumeOwnershipUntilShellIsReady(t *testing.T) {
	topology := &fakeManagedTopology{waitErrs: []error{errors.New("shell busy")}}
	cleanup := &managedCleanup{topology: topology, target: sessionhost.CreatedTarget{Origin: managedOrigin()}, resume: true, launchOwned: true, pid: 101, cwd: "/tmp/project", argv: []string{"codex"}}
	if err := cleanup.stopTarget(context.Background()); err == nil || !cleanup.launchOwned {
		t.Fatalf("busy shell error=%v owned=%t", err, cleanup.launchOwned)
	}
	if err := cleanup.stopTarget(context.Background()); err != nil || cleanup.launchOwned || len(topology.stops) != 2 {
		t.Fatalf("retry error=%v owned=%t stops=%v", err, cleanup.launchOwned, topology.stops)
	}
	if err := cleanup.stopTarget(context.Background()); err != nil || len(topology.stops) != 2 {
		t.Fatalf("repeat error=%v stops=%v", err, topology.stops)
	}
}

// managedTimedShellTopology stamps every shell-readiness probe and cancels the
// reconciliation after a fixed number of probes.
type managedTimedShellTopology struct {
	*fakeManagedTopology
	probes []time.Time
	cancel context.CancelFunc
	last   int
}

func (t *managedTimedShellTopology) WaitForShell(ctx context.Context, target sessionhost.Origin, cwd string) error {
	t.probes = append(t.probes, time.Now())
	if len(t.probes) == t.last {
		t.cancel()
	}
	return t.fakeManagedTopology.WaitForShell(ctx, target, cwd)
}

func TestManagedCleanupReconciliationPausesBetweenUnsettledAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		topology := &managedTimedShellTopology{
			fakeManagedTopology: &fakeManagedTopology{verifyErr: sessionhost.ErrStaleTarget, waitErr: errors.New("shell busy")},
			cancel:              cancel, last: 4,
		}
		cleanup := &managedCleanup{topology: topology, target: sessionhost.CreatedTarget{Origin: managedOrigin()}, resume: true, launchOwned: true, cwd: "/tmp/project", argv: []string{"codex"}}
		if err := cleanup.reconcilePending(ctx); !errors.Is(err, ErrManagedStale) {
			t.Fatalf("error=%v", err)
		}
		if len(topology.probes) < 4 {
			t.Fatalf("probes=%d", len(topology.probes))
		}
		for i := 1; i < 4; i++ {
			if gap := topology.probes[i].Sub(topology.probes[i-1]); gap != 25*time.Millisecond {
				t.Fatalf("probe %d followed the previous one after %v, want 25ms", i, gap)
			}
		}
	})
}
