package agenthost

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const ompMutationValidStatus = `{"version":1,"harness":"omp","session_kind":"path","session_value":"/tmp/session.jsonl","status":"idle","state_seq":3}`

func TestOMPReadStatusValidatesProtocolFieldsIndependently(t *testing.T) {
	invalid := map[string]string{
		"version":               strings.Replace(ompMutationValidStatus, `"version":1`, `"version":2`, 1),
		"harness":               strings.Replace(ompMutationValidStatus, `"harness":"omp"`, `"harness":"other"`, 1),
		"session kind":          strings.Replace(ompMutationValidStatus, `"session_kind":"path"`, `"session_kind":"id"`, 1),
		"relative session path": strings.Replace(ompMutationValidStatus, `"/tmp/session.jsonl"`, `"session.jsonl"`, 1),
	}
	if got, err := readOMPStatus(context.Background(), serveOMPResponses(t, ompMutationValidStatus)); err != nil || got.StateSeq != 3 || got.Status != "idle" {
		t.Fatalf("valid status=%+v error=%v", got, err)
	}
	for name, payload := range invalid {
		if _, err := readOMPStatus(context.Background(), serveOMPResponses(t, payload)); err == nil {
			t.Errorf("status with an invalid %s was accepted", name)
		}
	}
}

func TestOMPReadStatusBoundsAndRejectsTrailingResponse(t *testing.T) {
	padded := func(size int) string {
		return ompMutationValidStatus + strings.Repeat(" ", size-len(ompMutationValidStatus))
	}
	if got, err := readOMPStatus(context.Background(), serveOMPResponses(t, padded(ompResponseBytes))); err != nil || got.StateSeq != 3 {
		t.Fatalf("exact-limit status=%+v error=%v", got, err)
	}
	for name, response := range map[string]string{
		"one byte over the limit": padded(ompResponseBytes + 1),
		"second JSON value":       ompMutationValidStatus + ` {}`,
	} {
		if _, err := readOMPStatus(context.Background(), serveOMPResponses(t, response)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// OMP reports "working" and "idle" with the same reason, so a change of state
// alone must still make the two samples unstable.
func TestAssessOMPRejectsStatusChangeWithUnchangedReasonAndSequence(t *testing.T) {
	target := nativeTarget("omp", "path", "/tmp/session.jsonl")
	working := strings.Replace(ompMutationValidStatus, `"status":"idle"`, `"status":"working"`, 1)
	socket := serveOMPResponses(t, working, ompMutationValidStatus)
	runner := &scriptedRunner{results: fourNativeIdentities(t, target)}
	probe := mustNativeProbe(t, Host{Runner: runner, Session: "named"}, NativeSources{OMPSockets: map[string]string{target.PaneID: socket}})
	got, err := probe.Assess(context.Background(), target)
	if !errors.Is(err, ErrStaleReport) || got != nativeUnknown(Unsettled) {
		t.Fatalf("assessment=%+v error=%v", got, err)
	}
}
