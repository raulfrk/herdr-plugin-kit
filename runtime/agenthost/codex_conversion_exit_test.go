package agenthost

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
)

func TestCodexConverterUsesOneNativeExitWithoutSignal(t *testing.T) {
	for _, session := range []string{"", "named"} {
		t.Run("session="+session, func(t *testing.T) {
			h := newConversionHarness(t)
			h.source.SessionName = session
			h.converter.launcher.Host.Session = session
			h.runner.results = []command.Result{
				managedAgentResult(t, h.source), {Stdout: managedEmptyComposer("answer")},
				managedAgentResult(t, h.source), {Stdout: managedEmptyComposer("answer")}, {},
			}
			h.topology.pids = []int{77}
			managed := h.managedResult()
			managed.Agent.SessionName, managed.Target.SessionName = session, session
			h.converter.launch = func(_ context.Context, request ManagedCodexRequest) (*ManagedCodex, error) {
				h.launches = append(h.launches, request)
				return managed, nil
			}
			result, err := h.converter.Convert(context.Background(), h.request())
			if err != nil || result.Outcome != CodexConversionManaged {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			wireSession := session
			if wireSession == "" {
				wireSession = "default"
			}
			want := []string{"herdr", "--session", wireSession, "pane", "send-keys", h.source.PaneID, "ctrl+d"}
			if conversionExitCount(h.runner.calls) != 1 || len(h.topology.stops) != 0 || len(h.launches) != 1 ||
				!reflect.DeepEqual(h.runner.calls[len(h.runner.calls)-1], want) {
				t.Fatalf("exit calls=%+v signals=%+v launches=%+v", h.runner.calls, h.topology.stops, h.launches)
			}
		})
	}
}

func conversionExitCount(calls [][]string) int {
	count := 0
	for _, call := range calls {
		if len(call) >= 4 && call[len(call)-4] == "pane" && call[len(call)-3] == "send-keys" && call[len(call)-1] == "ctrl+d" {
			count++
		}
	}
	return count
}

func TestCodexConverterExitFailureIsUncertainAndNeverRetried(t *testing.T) {
	failure := errors.New("exit response lost")
	for _, mode := range []string{"dispatch", "response", "canceled", "shell"} {
		t.Run(mode, func(t *testing.T) {
			h := newConversionHarness(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "dispatch":
				h.runner.errs = []error{nil, nil, nil, nil, failure}
			case "response":
				h.runner.results[4].Stdout = []byte("unexpected acknowledgement")
			case "canceled":
				h.converter.launcher.Host.Runner = runnerFunc(func(ctx context.Context, executable string, args []string) (command.Result, error) {
					result, err := h.runner.Run(ctx, executable, args)
					if conversionExitCount(h.runner.calls) == 1 {
						cancel()
					}
					return result, err
				})
			case "shell":
				h.topology.waitErr = context.DeadlineExceeded
			}
			result, err := h.converter.Convert(ctx, h.request())
			if !errors.Is(err, ErrCodexConversionExitUnverified) || result.ThreadID != h.identity.ID || result.Outcome != "" ||
				result.ManagedAttempts != 0 || result.FallbackUsed || conversionExitCount(h.runner.calls) != 1 ||
				len(h.topology.stops) != 0 || len(h.topology.runs) != 0 || len(h.launches) != 0 || h.shellCaptures != 0 {
				t.Fatalf("result=%+v error=%v calls=%v topology=%+v", result, err, h.runner.calls, h.topology)
			}
			if mode == "dispatch" && !errors.Is(err, failure) || mode == "canceled" && !errors.Is(err, context.Canceled) ||
				mode == "shell" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("exit failure lost underlying cause: %v", err)
			}
		})
	}
}

func TestCodexConverterRejectsChangedProcessBeforeNativeExit(t *testing.T) {
	for _, mode := range []string{"pid", "verification", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			h := newConversionHarness(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "pid":
				h.topology.pids = []int{78}
			case "verification":
				h.topology.verifyErr = ErrStaleReport
			case "canceled":
				h.converter.launcher.Host.Runner = runnerFunc(func(ctx context.Context, executable string, args []string) (command.Result, error) {
					result, err := h.runner.Run(ctx, executable, args)
					if len(h.runner.calls) == 4 {
						cancel()
					}
					return result, err
				})
			}
			result, err := h.converter.Convert(ctx, h.request())
			if err == nil || errors.Is(err, ErrCodexConversionExitUnverified) || !reflect.DeepEqual(result, CodexConversionResult{}) ||
				hasPaneInput(h.runner.calls) || len(h.topology.stops) != 0 || len(h.topology.runs) != 0 || len(h.launches) != 0 {
				t.Fatalf("result=%+v error=%v calls=%v topology=%+v", result, err, h.runner.calls, h.topology)
			}
		})
	}
}

func TestCodexConverterRejectsChangedCaptureBeforeNativeExit(t *testing.T) {
	for _, changed := range []string{"service", "thread"} {
		t.Run(changed, func(t *testing.T) {
			h := newConversionHarness(t)
			service, before := h.service.snapshot, h.identity
			if changed == "service" {
				service.MainPID++
			} else {
				before.ID = managedSecondID
			}
			result, err := h.converter.stopCaptureResume(context.Background(), h.request(), h.request().ReadyTimeout,
				service, h.verifier, conversionConstraints(), before, h.history, managedStatusObservation{})
			if !errors.Is(err, ErrManagedStale) || !reflect.DeepEqual(result, CodexConversionResult{}) ||
				hasPaneInput(h.runner.calls) || len(h.topology.stops) != 0 || len(h.topology.runs) != 0 || len(h.launches) != 0 {
				t.Fatalf("result=%+v error=%v calls=%v topology=%+v", result, err, h.runner.calls, h.topology)
			}
		})
	}
}
