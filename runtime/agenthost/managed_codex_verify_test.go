package agenthost

import (
	"context"
	"errors"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

type readOnlyGuardService struct {
	*fakeManagedService
	inspections int
}

func (s *readOnlyGuardService) Inspect(context.Context) (CodexServiceSnapshot, error) {
	s.inspections++
	return s.snapshot, s.err
}

func TestVerifySafeSessionChangeMakesOneReadOnlyAttempt(t *testing.T) {
	for _, state := range []string{"safe", "draft", "working"} {
		t.Run(state, func(t *testing.T) {
			source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
			current := source
			if state == "working" {
				current.Status = "working"
			}
			composer := managedEmptyComposer("answer")
			if state == "draft" {
				composer = []byte("answer\n› first draft line\n  second line\n status")
			}
			runner := &scriptedRunner{results: []command.Result{managedAgentResult(t, current), {Stdout: composer}, managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")}}}
			service := &readOnlyGuardService{fakeManagedService: &fakeManagedService{snapshot: managedServiceSnapshot()}}
			topology := &fakeManagedTopology{inspected: sessionProcess(source.CWD)}
			launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
			launcher.service = service
			launcher.topology = topology
			snapshot, err := launcher.VerifySafeSessionChange(context.Background(), source)
			wantReads := 4
			if state == "draft" {
				wantReads = 2
			}
			if state == "working" {
				wantReads = 1
			}
			if (state == "safe" && err != nil) || (state != "safe" && !errors.Is(err, ErrManagedPaneBusy)) {
				t.Fatal("unexpected decision", err)
			}
			if state == "safe" && (snapshot.Agent.PaneID != source.PaneID || snapshot.Process.PID != 77 || snapshot.Service != service.snapshot) {
				t.Fatal("successful verification lost its proven snapshot")
			}
			if len(runner.calls) != wantReads || service.callCount() != 0 || service.inspections != 1 || hasPaneInput(runner.calls) || len(topology.stops) != 0 {
				t.Fatal("retry or mutation occurred", runner.calls)
			}
		})
	}
}

type cancelAfterGuardRead struct {
	*scriptedRunner
	cancel context.CancelFunc
}

func (runner *cancelAfterGuardRead) Run(ctx context.Context, executable string, args []string) (command.Result, error) {
	result, err := runner.scriptedRunner.Run(ctx, executable, args)
	if len(runner.calls) == 4 {
		runner.cancel()
	}
	return result, err
}

func TestVerifySafeSessionChangeRechecksCancellationBeforeSuccess(t *testing.T) {
	for _, readFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled after stable read", true: "read error remains authoritative"}[readFails], func(t *testing.T) {
			source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			readErr := errors.New("pane read failed")
			runner := &cancelAfterGuardRead{scriptedRunner: &scriptedRunner{results: []command.Result{managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")}, managedAgentResult(t, source), {Stdout: managedEmptyComposer("answer")}}}, cancel: cancel}
			want := error(context.Canceled)
			if readFails {
				runner.errs = []error{nil, nil, nil, readErr}
				want = readErr
			}
			launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
			launcher.service = &readOnlyGuardService{fakeManagedService: &fakeManagedService{snapshot: managedServiceSnapshot()}}
			launcher.topology = &fakeManagedTopology{inspected: sessionProcess(source.CWD)}
			if _, err := launcher.VerifySafeSessionChange(ctx, source); !errors.Is(err, want) {
				t.Fatal("late cancellation or failed observation became successful", err)
			}
		})
	}
}

func TestVerifySafeSessionChangeRefusesChangedThreadAndProcess(t *testing.T) {
	for _, scenario := range []string{"thread", "process-directory", "process-read-error"} {
		t.Run(scenario, func(t *testing.T) {
			source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
			current := source
			current.Session = &SessionRef{Source: source.Session.Source, Agent: source.Session.Agent, Kind: source.Session.Kind, Value: source.Session.Value}
			topology := &fakeManagedTopology{inspected: sessionProcess(source.CWD)}
			want := error(ErrManagedStale)
			switch scenario {
			case "thread":
				current.Session.Value = "00000000-0000-4000-8000-000000000099"
			case "process-directory":
				topology.inspected.CWD = "/another-directory"
			case "process-read-error":
				topology.inspectErr = errors.New("process unavailable")
				want = topology.inspectErr
			}
			runner := &scriptedRunner{results: []command.Result{managedAgentResult(t, current), {Stdout: managedEmptyComposer("answer")}, managedAgentResult(t, current), {Stdout: managedEmptyComposer("answer")}}}
			launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
			launcher.service = &readOnlyGuardService{fakeManagedService: &fakeManagedService{snapshot: managedServiceSnapshot()}}
			launcher.topology = topology
			if _, err := launcher.VerifySafeSessionChange(context.Background(), source); !errors.Is(err, want) || hasPaneInput(runner.calls) || len(topology.stops) != 0 {
				t.Fatal("changed or unavailable source was accepted", err)
			}
		})
	}
}

func TestVerifySafeSessionChangeCancellationAndUnavailableServiceDoNotEnsure(t *testing.T) {
	source := managedAgent(managedOrigin(), "/tmp/project", managedSourceID, false)
	for _, scenario := range []string{"canceled", "unavailable", "unsupported-inspection"} {
		t.Run(scenario, func(t *testing.T) {
			runner := &scriptedRunner{}
			service := &readOnlyGuardService{fakeManagedService: &fakeManagedService{snapshot: managedServiceSnapshot()}}
			launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
			launcher.service = service
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "canceled" {
				cancel()
			}
			if scenario == "unavailable" {
				service.err = ErrCodexServiceUnavailable
			}
			if scenario == "unsupported-inspection" {
				launcher.service = service.fakeManagedService
			}
			_, err := launcher.VerifySafeSessionChange(ctx, source)
			if err == nil || len(runner.calls) != 0 || service.callCount() != 0 {
				t.Fatal("verification repaired or probed unavailable service", err)
			}
			if scenario == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation lost", err)
			}
		})
	}
}
