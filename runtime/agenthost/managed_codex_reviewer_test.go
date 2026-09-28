package agenthost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

func TestManagedCodexNewAppliesExplicitReviewerWithoutChangingSnapshot(t *testing.T) {
	for _, reviewer := range []string{"user", "auto_review"} {
		for name, inherited := range map[string]json.RawMessage{
			"omitted": nil, "null": json.RawMessage(`null`),
			"user": json.RawMessage(`"user"`), "automatic": json.RawMessage(`"auto_review"`),
		} {
			t.Run(reviewer+"/"+name, func(t *testing.T) {
				launcher, service, topology, verifier, runner, request := newManagedHarness(t, ManagedNew, managedNewID)
				request.ApprovalsReviewer = reviewer
				verifier.constraints = conversionConstraints()
				delete(verifier.constraints.Config, "approvals_reviewer")
				if inherited != nil {
					verifier.constraints.Config["approvals_reviewer"] = inherited
				}
				before, err := json.Marshal(verifier.constraints)
				if err != nil {
					t.Fatal(err)
				}
				runner.results = []command.Result{{}, managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false)), managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false)), managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false))}
				managed, err := launcher.Launch(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
				want := []string{service.snapshot.Executable, "-m", request.Policy.Model, "-c", `model_reasoning_effort="medium"`, "-c", managedSkipUpdateConfig, "-a", request.Policy.ApprovalPolicy, "-s", request.Policy.SandboxMode, "-c", "approvals_reviewer=" + reviewer, "--strict-config", "--remote", "unix://" + service.snapshot.SocketPath, "--cd", request.Directory}
				after, err := json.Marshal(verifier.constraints)
				if err != nil || !bytes.Equal(before, after) || len(verifier.policies) != 0 || !reflect.DeepEqual(managed.Argv, want) || managed.Policy.ApprovalsReviewer != reviewer || !matchesRequestedPolicy(managed.Policy, request.Policy) || managed.Policy.CWD != request.Directory || len(topology.launches) != 1 || topology.launches[0].Focus {
					t.Fatalf("managed=%+v policies=%+v launch=%+v snapshot changed=%t err=%v", managed, verifier.policies, topology.launches, !bytes.Equal(before, after), err)
				}
				if err := managed.Cleanup(context.Background()); err != nil || len(topology.cleanups) != 1 {
					t.Fatalf("cleanup=%v targets=%+v", err, topology.cleanups)
				}
			})
		}
	}
}

func TestManagedCodexReviewerOverrideRejectsInvalidAndNonNewRequestsBeforeEnsure(t *testing.T) {
	for _, tc := range []struct {
		operation ManagedOperation
		reviewer  string
	}{
		{ManagedNew, "automatic"}, {ManagedNew, " user"}, {ManagedNew, "user\x00"},
		{ManagedFork, "user"}, {ManagedFork, "auto_review"}, {ManagedResume, "user"}, {ManagedResume, "auto_review"},
	} {
		t.Run(string(tc.operation)+"/"+tc.reviewer, func(t *testing.T) {
			launcher, service, topology, _, runner, request := newManagedHarness(t, tc.operation, managedNewID)
			request.ApprovalsReviewer = tc.reviewer
			if tc.operation == ManagedFork {
				request.Source = managedAgent(request.Origin, request.Directory, managedSourceID, false)
			}
			if tc.operation == ManagedResume {
				request.ThreadID, request.Placement = managedOriginalID, ""
			}
			if _, err := launcher.Launch(context.Background(), request); err == nil || service.callCount() != 0 || len(topology.launches) != 0 || len(topology.runs) != 0 || len(runner.calls) != 0 {
				t.Fatalf("err=%v ensure=%d topology=%+v calls=%+v", err, service.callCount(), topology, runner.calls)
			}
		})
	}
}

func TestManagedCodexReviewerConstraintsAndMalformedConfigRefuseBeforeTopology(t *testing.T) {
	for _, tc := range []struct {
		name, reviewer string
		inherited      json.RawMessage
		requirements   json.RawMessage
	}{
		{"disallowed reviewer", "auto_review", nil, json.RawMessage(`{"allowedApprovalsReviewers":["user"]}`)},
		{"mandatory automatic", "user", nil, json.RawMessage(`{"autoReview":{"requiredOnModels":["gpt-5.6-sol"]}}`)},
		{"malformed inherited", "user", json.RawMessage(`{"invalid":true}`), json.RawMessage(`null`)},
		{"unknown inherited", "user", json.RawMessage(`"future"`), json.RawMessage(`null`)},
		{"unknown requirements", "user", nil, json.RawMessage(`{"future":true}`)},
		{"approval restriction", "auto_review", nil, json.RawMessage(`{"allowedApprovalPolicies":["on-request"]}`)},
		{"sandbox restriction", "auto_review", nil, json.RawMessage(`{"allowedSandboxModes":["read-only"]}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launcher, _, topology, verifier, runner, request := newManagedHarness(t, ManagedNew, managedNewID)
			request.ApprovalsReviewer = tc.reviewer
			verifier.constraints = conversionConstraints()
			delete(verifier.constraints.Config, "approvals_reviewer")
			if tc.inherited != nil {
				verifier.constraints.Config["approvals_reviewer"] = tc.inherited
			}
			verifier.constraints.Requirements["requirements"] = tc.requirements
			if _, err := launcher.Launch(context.Background(), request); !errors.Is(err, ErrManagedUnsupported) || verifier.armed != 0 || len(topology.launches) != 0 || len(topology.runs) != 0 || len(runner.calls) != 0 || len(verifier.policies) != 0 {
				t.Fatalf("err=%v armed=%d topology=%+v calls=%+v", err, verifier.armed, topology, runner.calls)
			}
		})
	}
}

func TestManagedCodexInheritedReviewerOmissionStillRefusesBeforeTopology(t *testing.T) {
	launcher, _, topology, verifier, _, request := newManagedHarness(t, ManagedNew, managedNewID)
	snapshot := conversionConstraints()
	delete(snapshot.Config, "approvals_reviewer")
	verifier.preflight, verifier.preflightErr = snapshot.validate(request.Directory, request.Policy)
	if _, err := launcher.Launch(context.Background(), request); !errors.Is(err, ErrManagedUnsupported) || len(verifier.policies) != 1 || len(topology.launches) != 0 || verifier.armed != 0 {
		t.Fatalf("err=%v policies=%+v topology=%+v armed=%d", err, verifier.policies, topology, verifier.armed)
	}
}

func TestManagedCodexExplicitReviewerPreservesIdentityFailureCleanup(t *testing.T) {
	launcher, _, topology, verifier, runner, request := newManagedHarness(t, ManagedNew, managedNewID)
	request.ApprovalsReviewer = "auto_review"
	verifier.constraints = conversionConstraints()
	verifier.confirmErr = errors.New("unknown thread")
	if _, err := launcher.Launch(context.Background(), request); err == nil || len(topology.cleanups) != 1 || len(verifier.deleted) != 0 || len(runner.calls) != 0 {
		t.Fatalf("err=%v cleanups=%+v deleted=%+v calls=%+v", err, topology.cleanups, verifier.deleted, runner.calls)
	}
}

func TestCodexNewReviewerValidationRejectsMissingSnapshotAndInvalidChoice(t *testing.T) {
	for _, reviewer := range []string{"", "unknown", "user", "auto_review"} {
		if _, err := (codexConstraintSnapshot{}).validateNew("/tmp/project", managedPolicy(), reviewer); err == nil {
			t.Fatalf("missing snapshot accepted with reviewer %q", reviewer)
		}
	}
	for _, reviewer := range []string{"", "unknown"} {
		if _, err := conversionConstraints().validateNew("/tmp/project", managedPolicy(), reviewer); err == nil {
			t.Fatalf("invalid choice %q accepted", reviewer)
		}
	}
}

func TestManagedCodexReviewerArgumentBudgetIsCheckedBeforeEnsure(t *testing.T) {
	for _, tc := range []struct {
		name, reviewer string
		count          int
		wantErr        bool
	}{
		{"explicit boundary", "user", sessionhost.MaxArgv - 18, false},
		{"automatic boundary", "auto_review", sessionhost.MaxArgv - 18, false},
		{"explicit one over", "user", sessionhost.MaxArgv - 17, true},
		{"automatic two over", "auto_review", sessionhost.MaxArgv - 16, true},
		{"inherited boundary", "", sessionhost.MaxArgv - 16, false},
		{"inherited one over", "", sessionhost.MaxArgv - 15, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launcher, service, topology, _, _, request := newManagedHarness(t, ManagedNew, managedNewID)
			request.ApprovalsReviewer = tc.reviewer
			if tc.count%2 != 0 {
				request.ExtraArgv = append(request.ExtraArgv, "--search")
			}
			for index := 0; len(request.ExtraArgv) < tc.count; index++ {
				request.ExtraArgv = append(request.ExtraArgv, "--enable", fmt.Sprintf("feature-%d", index))
			}
			if err := launcher.validate(request); (err != nil) != tc.wantErr {
				t.Fatalf("count=%d reviewer=%q err=%v", tc.count, tc.reviewer, err)
			}
			if tc.wantErr {
				if _, err := launcher.Launch(context.Background(), request); err == nil || service.callCount() != 0 || len(topology.launches) != 0 {
					t.Fatalf("err=%v ensure=%d launches=%+v", err, service.callCount(), topology.launches)
				}
			} else if argv := composeManagedTUIArgv(request, service.snapshot, ""); len(argv) != sessionhost.MaxArgv {
				t.Fatalf("boundary argv count=%d, want=%d", len(argv), sessionhost.MaxArgv)
			}
		})
	}
}
