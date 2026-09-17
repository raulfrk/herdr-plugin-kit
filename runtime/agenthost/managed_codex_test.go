package agenthost

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/command"
	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
	"pgregory.net/rapid"
)

const (
	managedNewID      = "01a09f5c-37cc-77f1-9409-e9aa070ad8d0"
	managedSourceID   = "01a09f5c-37cc-77f1-9409-e9aa070ad8d1"
	managedChildID    = "01a09f5c-37cc-77f1-9409-e9aa070ad8d2"
	managedOriginalID = "01a09f5c-37cc-77f1-9409-e9aa070ad8d3"
	managedSecondID   = "01a09f5c-37cc-77f1-9409-e9aa070ad8d4"
)

type fakeManagedService struct {
	snapshot CodexServiceSnapshot
	err      error
	mu       sync.Mutex
	calls    int
}

func (f *fakeManagedService) Ensure(context.Context) (CodexServiceSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.snapshot, f.err
}

func (f *fakeManagedService) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeManagedVerifier struct {
	preflight    codexPreflight
	preflightErr error
	policies     []ManagedCodexPolicy
	constraints  codexConstraintSnapshot
	history      [32]byte
	historyErr   error
	confirm      codexThreadIdentity
	confirmErr   error
	read         []codexThreadIdentity
	recheck      codexThreadIdentity
	audit        EffectiveCodexPolicy
	audits       []EffectiveCodexPolicy
	auditErr     error
	reconcile    EffectiveCodexPolicy
	reconcileErr error
	reconciles   []EffectiveCodexPolicy
	recheckErr   error
	rechecks     []codexThreadIdentity
	recheckErrs  []error
	auditCalls   int
	armed        int
	closed       int
	deleted      []string
	confirmOp    ManagedOperation
	confirmSrc   string
	confirmID    string
}

func (f *fakeManagedVerifier) Preflight(_ context.Context, _ string, policy ManagedCodexPolicy) (codexPreflight, error) {
	f.policies = append(f.policies, policy)
	return f.preflight, f.preflightErr
}
func (f *fakeManagedVerifier) ReadConstraints(context.Context, string) (codexConstraintSnapshot, error) {
	return f.constraints, f.preflightErr
}
func (f *fakeManagedVerifier) Arm() error { f.armed++; return nil }
func (f *fakeManagedVerifier) ConfirmIdentity(_ context.Context, operation ManagedOperation, source, id string) (codexThreadIdentity, error) {
	f.confirmOp, f.confirmSrc, f.confirmID = operation, source, id
	return f.confirm, f.confirmErr
}
func (f *fakeManagedVerifier) ReadThread(context.Context, string) (codexThreadIdentity, error) {
	if len(f.read) == 0 {
		return codexThreadIdentity{}, errors.New("unexpected read")
	}
	value := f.read[0]
	f.read = f.read[1:]
	return value, nil
}
func (f *fakeManagedVerifier) RecheckIdentity(context.Context, string) (codexThreadIdentity, error) {
	if len(f.rechecks) != 0 {
		value := f.rechecks[0]
		f.rechecks = f.rechecks[1:]
		var err error
		if len(f.recheckErrs) != 0 {
			err = f.recheckErrs[0]
			f.recheckErrs = f.recheckErrs[1:]
		}
		return value, err
	}
	return f.recheck, f.recheckErr
}
func (f *fakeManagedVerifier) ReadEffective(context.Context, string) (EffectiveCodexPolicy, error) {
	f.auditCalls++
	if len(f.audits) != 0 {
		value := f.audits[0]
		f.audits = f.audits[1:]
		return value, f.auditErr
	}
	return f.audit, f.auditErr
}
func (f *fakeManagedVerifier) ReconcileEffective(_ context.Context, _ string, desired EffectiveCodexPolicy) (EffectiveCodexPolicy, error) {
	f.reconciles = append(f.reconciles, desired)
	if reflect.DeepEqual(f.reconcile, EffectiveCodexPolicy{}) {
		return desired, f.reconcileErr
	}
	return f.reconcile, f.reconcileErr
}
func (f *fakeManagedVerifier) ReadStableHistory(context.Context, string) ([32]byte, error) {
	return f.history, f.historyErr
}
func (f *fakeManagedVerifier) DeleteThread(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *fakeManagedVerifier) Close() error { f.closed++; return nil }

type fakeManagedVerifierFactory struct{ verifier managedVerifier }

func (f fakeManagedVerifierFactory) Connect(context.Context, string) (managedVerifier, error) {
	return f.verifier, nil
}

type fakeManagedTopology struct {
	target        sessionhost.CreatedTarget
	launches      []sessionhost.LaunchRequest
	runs          [][]string
	waits         []sessionhost.Origin
	verifications [][]string
	focuses       []sessionhost.Origin
	cleanups      []sessionhost.CreatedTarget
	stops         []sessionhost.ForegroundProcess
	verifyErr     error
	verifyErrs    []error
	waitErr       error
	waitErrs      []error
	runErr        error
	stopErr       error
	stopErrs      []error
	pid           int
	pids          []int
	inspected     sessionhost.ForegroundProcess
	inspectedSeq  []sessionhost.ForegroundProcess
	inspectErr    error
	inspectErrs   []error
	focusErr      error
}

func (f *fakeManagedTopology) Launch(_ context.Context, request sessionhost.LaunchRequest) (sessionhost.CreatedTarget, error) {
	f.launches = append(f.launches, request)
	return f.target, nil
}
func (f *fakeManagedTopology) RunInShell(_ context.Context, target sessionhost.Origin, _ string, argv []string) (bool, error) {
	f.waits = append(f.waits, target)
	if f.waitErr != nil {
		return false, f.waitErr
	}
	f.runs = append(f.runs, append([]string(nil), argv...))
	return true, f.runErr
}
func (f *fakeManagedTopology) WaitForShell(_ context.Context, target sessionhost.Origin, _ string) error {
	f.waits = append(f.waits, target)
	if len(f.waitErrs) != 0 {
		err := f.waitErrs[0]
		f.waitErrs = f.waitErrs[1:]
		return err
	}
	return f.waitErr
}
func (f *fakeManagedTopology) InspectForegroundProcess(context.Context, sessionhost.Origin) (sessionhost.ForegroundProcess, error) {
	if len(f.inspectErrs) != 0 {
		err := f.inspectErrs[0]
		f.inspectErrs = f.inspectErrs[1:]
		if err != nil {
			return sessionhost.ForegroundProcess{}, err
		}
	}
	if f.inspectErr != nil {
		return sessionhost.ForegroundProcess{}, f.inspectErr
	}
	if len(f.inspectedSeq) != 0 {
		value := f.inspectedSeq[0]
		f.inspectedSeq = f.inspectedSeq[1:]
		return value, nil
	}
	if f.inspected.PID != 0 {
		return f.inspected, nil
	}
	return sessionhost.ForegroundProcess{PID: 77, Name: "codex", CWD: "/tmp/project", Argv: []string{"codex"}}, nil
}
func (f *fakeManagedTopology) VerifyForegroundProcess(_ context.Context, _ sessionhost.Origin, cwd string, argv []string) (sessionhost.ForegroundProcess, error) {
	f.verifications = append(f.verifications, append([]string(nil), argv...))
	if len(f.verifyErrs) != 0 {
		err := f.verifyErrs[0]
		f.verifyErrs = f.verifyErrs[1:]
		if err != nil {
			return sessionhost.ForegroundProcess{}, err
		}
	}
	if f.verifyErr != nil {
		return sessionhost.ForegroundProcess{}, f.verifyErr
	}
	pid := f.pid
	if len(f.pids) != 0 {
		pid = f.pids[0]
		f.pids = f.pids[1:]
	}
	if pid == 0 {
		pid = 99
	}
	return sessionhost.ForegroundProcess{PID: pid, Name: "codex", CWD: cwd, Argv: append([]string(nil), argv...)}, nil
}
func (f *fakeManagedTopology) StopForegroundProcess(_ context.Context, _ sessionhost.Origin, cwd string, argv []string, pid int) error {
	f.stops = append(f.stops, sessionhost.ForegroundProcess{PID: pid, CWD: cwd, Argv: append([]string(nil), argv...)})
	if len(f.stopErrs) != 0 {
		err := f.stopErrs[0]
		f.stopErrs = f.stopErrs[1:]
		return err
	}
	return f.stopErr
}
func (f *fakeManagedTopology) FocusPane(_ context.Context, target sessionhost.Origin) error {
	f.focuses = append(f.focuses, target)
	return f.focusErr
}
func (f *fakeManagedTopology) Cleanup(_ context.Context, target sessionhost.CreatedTarget) error {
	f.cleanups = append(f.cleanups, target)
	return nil
}

func managedPolicy() ManagedCodexPolicy {
	return ManagedCodexPolicy{Model: "gpt-5.6-sol", ReasoningEffort: "medium", ApprovalPolicy: "never", SandboxMode: "danger-full-access"}
}

func managedPreflight(cwd string) codexPreflight {
	return codexPreflight{EffectiveCodexPolicy: EffectiveCodexPolicy{Model: "gpt-5.6-sol", ReasoningEffort: "medium", ApprovalPolicy: "never", ApprovalsReviewer: "user", SandboxMode: "danger-full-access", CWD: cwd}}
}

func managedServiceSnapshot() CodexServiceSnapshot {
	return CodexServiceSnapshot{State: CodexServiceReady, CodexHome: "/tmp/codex-home", SocketPath: "/tmp/codex-home/app-server-control/app-server-control.sock", CLIVersion: SupportedCodexVersion, AppServerVersion: SupportedCodexVersion}
}

func managedEmptyComposer(prefix string) []byte {
	return []byte(prefix + "\n\x1b[1m›\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m\n\n\x1b[2mstatus\x1b[0m")
}

func managedStatusDraft(prefix string) []byte {
	return []byte(prefix + "\n\x1b[1m›\x1b[0m /status\n\n  \x1b[1m\x1b[38;5;6m/status show current session configuration\x1b[0m\n  \x1b[1m\x1b[38;5;6mand token usage\x1b[0m\n  /\x1b[1mstatus\x1b[0mline configure which items appear in the\n  status line")
}

func managedOrigin() sessionhost.Origin {
	return sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1"}
}

func managedAgent(target sessionhost.Origin, cwd, id string, focused bool) Agent {
	return Agent{SessionName: target.SessionName, Kind: "codex", Status: "idle", CWD: cwd, WorkspaceID: target.WorkspaceID, TabID: target.TabID, PaneID: target.PaneID, Focused: focused, Session: &SessionRef{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: id}}
}

func managedAgentResult(t *testing.T, agent Agent) command.Result {
	t.Helper()
	wire := wireFromAgent(agent)
	var response agentInfoResponse
	response.ID, response.Result.Type, response.Result.Agent = "cli:agent:get", "agent_info", wire
	return jsonResult(t, response)
}

func newManagedHarness(t *testing.T, operation ManagedOperation, id string) (ManagedCodexLauncher, *fakeManagedService, *fakeManagedTopology, *fakeManagedVerifier, *scriptedRunner, ManagedCodexRequest) {
	t.Helper()
	origin := managedOrigin()
	target := sessionhost.CreatedTarget{Origin: sessionhost.Origin{SessionName: "named", WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p2"}, CreatedTab: true, DirectArgv: true}
	service := &fakeManagedService{snapshot: managedServiceSnapshot()}
	topology := &fakeManagedTopology{target: target}
	identity := codexThreadIdentity{ID: id, CWD: "/tmp/project", ModelProvider: "openai"}
	verifier := &fakeManagedVerifier{preflight: managedPreflight("/tmp/project"), confirm: identity, recheck: identity, audit: managedPreflight("/tmp/project").EffectiveCodexPolicy}
	runner := &scriptedRunner{}
	request := ManagedCodexRequest{Operation: operation, Origin: origin, Directory: "/tmp/project", Placement: sessionhost.NewTab, Policy: managedPolicy()}
	launcher := NewManagedCodexLauncher(Host{Runner: runner, Session: "named"})
	launcher.service, launcher.topology, launcher.verifiers = service, topology, fakeManagedVerifierFactory{verifier}
	launcher.discover = func(_ context.Context, got sessionhost.Origin, cwd string, _ []string, _ CodexServiceSnapshot, _ ManagedCodexPolicy) (Agent, string, error) {
		agent := managedAgent(got, cwd, id, false)
		agent.Session = nil
		return agent, id, nil
	}
	return launcher, service, topology, verifier, runner, request
}

func TestManagedCodexNewUsesSharedServiceAndLiteralBackgroundLaunch(t *testing.T) {
	launcher, service, topology, verifier, runner, request := newManagedHarness(t, ManagedNew, managedNewID)
	runner.results = []command.Result{{}, managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false)), managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false)), managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false))}
	managed, err := launcher.Launch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	wantArgv := []string{"codex", "-m", "gpt-5.6-sol", "-c", `model_reasoning_effort="medium"`, "-a", "never", "-s", "danger-full-access", "--strict-config", "--remote", "unix:///tmp/codex-home/app-server-control/app-server-control.sock", "--cd", "/tmp/project"}
	if service.callCount() != 1 || len(topology.launches) != 1 || topology.launches[0].Focus || !reflect.DeepEqual(topology.launches[0].Argv, wantArgv) || !reflect.DeepEqual(managed.Argv, wantArgv) || managed.Service != service.snapshot || managed.ThreadID != managedNewID || verifier.confirmID != managedNewID {
		t.Fatalf("managed=%+v service=%d launches=%+v verifier=%+v", managed, service.callCount(), topology.launches, verifier)
	}
	if err := managed.Cleanup(context.Background()); err != nil || len(topology.cleanups) != 1 {
		t.Fatalf("cleanup=%v calls=%v", err, topology.cleanups)
	}
}

func TestManagedCodexNewIncludesOnlyValidatedLiteralExtraArgv(t *testing.T) {
	launcher, service, topology, _, runner, request := newManagedHarness(t, ManagedNew, managedNewID)
	request.ExtraArgv = []string{"--search", "--enable", "responses_websockets", "--no-alt-screen"}
	runner.results = []command.Result{{}, managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false)), managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false)), managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false))}
	managed, err := launcher.Launch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{"codex", "--search", "--enable", "responses_websockets", "--no-alt-screen", "-m"}
	if service.callCount() != 1 || len(topology.launches) != 1 || len(managed.Argv) < len(wantPrefix) || !reflect.DeepEqual(managed.Argv[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("managed argv=%q launches=%+v service=%d", managed.Argv, topology.launches, service.callCount())
	}
}

func TestManagedCodexFocusesOnlyAfterFinalIdentityVerification(t *testing.T) {
	launcher, _, topology, _, runner, request := newManagedHarness(t, ManagedNew, managedNewID)
	request.Focus = true
	runner.results = []command.Result{
		{},
		managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false)),
		managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false)),
		managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false)),
		managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, true)),
	}
	managed, err := launcher.Launch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(topology.launches) != 1 || topology.launches[0].Focus || len(topology.focuses) != 1 || topology.focuses[0] != managed.Target.Origin {
		t.Fatalf("launches=%+v focuses=%+v", topology.launches, topology.focuses)
	}
}

func TestManagedCodexServiceFailureStopsBeforeVerifierAndTopology(t *testing.T) {
	launcher, service, topology, verifier, _, request := newManagedHarness(t, ManagedNew, managedNewID)
	service.err = errors.New("service unavailable")
	if _, err := launcher.Launch(context.Background(), request); err == nil || !strings.Contains(err.Error(), "service unavailable") {
		t.Fatalf("error=%v", err)
	}
	if service.callCount() != 1 || verifier.armed != 0 || len(topology.launches) != 0 || len(topology.runs) != 0 {
		t.Fatalf("service=%d verifier=%+v topology=%+v", service.callCount(), verifier, topology)
	}
}

func TestManagedCodexResumePrelaunchFailureNeverAdoptsExistingProcess(t *testing.T) {
	launcher, _, topology, _, _, request := newManagedHarness(t, ManagedResume, managedOriginalID)
	request.ThreadID, request.Placement = managedOriginalID, ""
	topology.waitErr = errors.New("shell not ready")
	if _, err := launcher.Launch(context.Background(), request); err == nil || errors.Is(err, ErrManagedCleanup) {
		t.Fatalf("error=%v", err)
	}
	if len(topology.runs) != 0 || len(topology.stops) != 0 || len(topology.verifications) != 0 {
		t.Fatalf("runs=%v stops=%v verifications=%v", topology.runs, topology.stops, topology.verifications)
	}
}

func TestManagedCodexResumeShellGuardFailureSendsNoInput(t *testing.T) {
	launcher, _, topology, verifier, _, request := newManagedHarness(t, ManagedResume, managedOriginalID)
	request.ThreadID, request.Placement = managedOriginalID, ""
	verifier.read = []codexThreadIdentity{{ID: managedOriginalID, CWD: request.Directory, ModelProvider: "openai"}}
	guardCalls := 0
	request.resumeShellGuard = func(context.Context) error {
		guardCalls++
		return ErrManagedPaneBusy
	}
	if _, err := launcher.Launch(context.Background(), request); !errors.Is(err, ErrManagedPaneBusy) {
		t.Fatalf("error=%v", err)
	}
	if guardCalls != 1 || len(topology.runs) != 0 || len(topology.stops) != 0 || len(topology.cleanups) != 0 {
		t.Fatalf("guard=%d runs=%v stops=%v cleanups=%v", guardCalls, topology.runs, topology.stops, topology.cleanups)
	}
}

func TestManagedCleanupNeverSubstitutesCapturedPID(t *testing.T) {
	topology := &fakeManagedTopology{stopErr: errors.New("captured process changed"), pid: 202}
	cleanup := &managedCleanup{
		topology: topology, target: sessionhost.CreatedTarget{Origin: managedOrigin()},
		resume: true, launchOwned: true, pid: 101, cwd: "/tmp/project", argv: []string{"codex"},
	}
	if err := cleanup.run(context.Background()); !errors.Is(err, ErrManagedCleanup) {
		t.Fatalf("error=%v", err)
	}
	if len(topology.stops) != 1 || topology.stops[0].PID != 101 || len(topology.verifications) != 0 {
		t.Fatalf("stops=%v verifications=%v", topology.stops, topology.verifications)
	}
}

func TestManagedTimeoutConstants(t *testing.T) {
	if DefaultManagedReadyTimeout != 30*time.Second || MaxManagedReadyTimeout != 5*time.Minute || managedCleanupTimeout != 5*time.Second {
		t.Fatalf("timeouts=%v %v %v", DefaultManagedReadyTimeout, MaxManagedReadyTimeout, managedCleanupTimeout)
	}
}

func TestValidateManagedSourceArgvRejectsEveryBound(t *testing.T) {
	tooMany := make([]string, sessionhost.MaxArgv+1)
	for i := range tooMany {
		tooMany[i] = "x"
	}
	tooMany[0] = "codex"
	for _, argv := range [][]string{
		{}, tooMany, {"other"}, {"codex", strings.Repeat("x", sessionhost.MaxArgBytes+1)}, {"codex", string([]byte{0xff})}, {"codex", "bad\x00arg"},
	} {
		if validateManagedSourceArgv(argv) == nil {
			t.Fatalf("argv accepted: %#v", argv)
		}
	}
	if validateManagedSourceArgv(nil) != nil || validateManagedSourceArgv([]string{"/usr/bin/codex", "ok"}) != nil {
		t.Fatal("valid source argv rejected")
	}
}

func TestManagedCleanupReconcilesOnlyPostAttemptProcess(t *testing.T) {
	topology := &fakeManagedTopology{
		verifyErrs: []error{sessionhost.ErrStaleTarget, nil, sessionhost.ErrStaleTarget},
		waitErrs:   []error{nil, nil},
		pids:       []int{303},
	}
	cleanup := &managedCleanup{topology: topology, target: sessionhost.CreatedTarget{Origin: managedOrigin()}, resume: true, launchOwned: true, cwd: "/tmp/project", argv: []string{"codex"}}
	if err := cleanup.run(context.Background()); err != nil || cleanup.launchOwned || len(topology.stops) != 1 || topology.stops[0].PID != 303 {
		t.Fatalf("error=%v owned=%t stops=%v", err, cleanup.launchOwned, topology.stops)
	}
}

func TestManagedCleanupReportsReconciliationStopFailure(t *testing.T) {
	topology := &fakeManagedTopology{verifyErrs: []error{sessionhost.ErrStaleTarget, nil}, stopErrs: []error{errors.New("stop failed")}}
	cleanup := &managedCleanup{topology: topology, target: sessionhost.CreatedTarget{Origin: managedOrigin()}, resume: true, launchOwned: true, cwd: "/tmp/project", argv: []string{"codex"}}
	if err := cleanup.run(context.Background()); !errors.Is(err, ErrManagedCleanup) {
		t.Fatalf("error=%v", err)
	}
}

func TestManagedCleanupCancellationCoversUnsettledReconciliationStates(t *testing.T) {
	for _, shellFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("shell-ready-%t", shellFirst), func(t *testing.T) {
			verifyErrs := []error{sessionhost.ErrStaleTarget, sessionhost.ErrStaleTarget}
			waitErrs := []error{errors.New("busy")}
			if shellFirst {
				verifyErrs = []error{sessionhost.ErrStaleTarget, nil, nil, sessionhost.ErrStaleTarget}
				waitErrs = []error{nil, errors.New("changed"), errors.New("busy")}
			}
			topology := &fakeManagedTopology{verifyErrs: verifyErrs, waitErrs: waitErrs}
			cleanup := &managedCleanup{topology: topology, target: sessionhost.CreatedTarget{Origin: managedOrigin()}, resume: true, launchOwned: true, cwd: "/tmp/project", argv: []string{"codex"}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			defer cancel()
			if err := cleanup.run(ctx); !errors.Is(err, ErrManagedCleanup) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestManagedCodexValidatesIdentityBeforeReportingIt(t *testing.T) {
	launcher, _, topology, verifier, runner, request := newManagedHarness(t, ManagedNew, managedNewID)
	verifier.confirmErr = errors.New("unknown thread")
	if _, err := launcher.Launch(context.Background(), request); err == nil {
		t.Fatal("expected identity failure")
	}
	if len(runner.calls) != 0 || len(topology.cleanups) != 1 {
		t.Fatalf("calls=%v cleanups=%v", runner.calls, topology.cleanups)
	}
}

func TestManagedCodexPolicyPreflightFailureStopsBeforeTopology(t *testing.T) {
	launcher, _, topology, verifier, _, request := newManagedHarness(t, ManagedNew, managedNewID)
	verifier.preflightErr = errors.New("unsupported requirement")
	if _, err := launcher.Launch(context.Background(), request); !errors.Is(err, ErrManagedUnsupported) {
		t.Fatalf("error=%v", err)
	}
	if verifier.armed != 0 || len(topology.launches) != 0 || len(topology.runs) != 0 {
		t.Fatalf("verifier=%+v topology=%+v", verifier, topology)
	}
	verifier.preflightErr = ErrManagedPolicyDrift
	if _, err := launcher.Launch(context.Background(), request); !errors.Is(err, ErrManagedPolicyDrift) {
		t.Fatalf("policy drift error=%v", err)
	}
}

func TestManagedCodexConcurrentProfilesReuseOneSharedServiceSnapshot(t *testing.T) {
	shared := &fakeManagedService{snapshot: managedServiceSnapshot()}
	type result struct {
		managed *ManagedCodex
		err     error
	}
	results := make(chan result, 2)
	for i, reasoning := range []string{"medium", "high"} {
		launcher, _, topology, verifier, runner, request := newManagedHarness(t, ManagedNew, []string{managedNewID, managedSecondID}[i])
		launcher.service = shared
		request.Policy.ReasoningEffort = reasoning
		verifier.preflight.ReasoningEffort = reasoning
		runner.results = []command.Result{{}, managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, verifier.confirm.ID, false)), managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, verifier.confirm.ID, false)), managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, verifier.confirm.ID, false))}
		go func() {
			managed, err := launcher.Launch(context.Background(), request)
			results <- result{managed: managed, err: err}
		}()
	}
	seen := map[string]bool{}
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.managed.Service != shared.snapshot {
			t.Fatalf("service=%+v want=%+v", result.managed.Service, shared.snapshot)
		}
		seen[result.managed.Policy.ReasoningEffort] = true
	}
	if shared.callCount() != 2 || !seen["medium"] || !seen["high"] {
		t.Fatalf("calls=%d profiles=%v", shared.callCount(), seen)
	}
}

func TestManagedCodexForkOmitsPermissionOverridesAndPreservesSource(t *testing.T) {
	launcher, _, topology, verifier, runner, request := newManagedHarness(t, ManagedFork, managedChildID)
	request.Placement, request.Direction = sessionhost.NewPane, "right"
	source := managedAgent(request.Origin, request.Directory, managedSourceID, false)
	request.Source = source
	verifier.confirm.ForkedFromID = managedSourceID
	verifier.recheck.ForkedFromID = managedSourceID
	verifier.read = []codexThreadIdentity{{ID: managedSourceID, CWD: request.Directory, ModelProvider: "openai"}, {ID: managedSourceID, CWD: request.Directory, ModelProvider: "openai"}}
	runner.results = []command.Result{
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("header")},
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("header")},
		{},
		managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedChildID, false)),
		managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedChildID, false)),
		jsonResult(t, listResponse(wireFromAgent(source))),
		managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedChildID, false)),
	}
	managed, err := launcher.Launch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "fork", "-m", "gpt-5.6-sol", "-c", `model_reasoning_effort="medium"`, "--strict-config", "--remote", "unix:///tmp/codex-home/app-server-control/app-server-control.sock", "--cd", "/tmp/project", managedSourceID}
	if !reflect.DeepEqual(managed.Argv, want) || verifier.confirmSrc != managedSourceID || verifier.confirmOp != ManagedFork || managed.SourceThreadID != managedSourceID {
		t.Fatalf("managed=%+v verifier=%+v", managed, verifier)
	}
}

func TestManagedCodexForkCanInheritAuthoritativeSourcePolicy(t *testing.T) {
	launcher, _, topology, verifier, runner, request := newManagedHarness(t, ManagedFork, managedChildID)
	source := managedAgent(request.Origin, request.Directory, managedSourceID, false)
	request.Source, request.Policy, request.InheritSourcePolicy = source, ManagedCodexPolicy{}, true
	request.ExtraArgv = []string{"--search"}
	sourcePolicy := managedPreflight(request.Directory).EffectiveCodexPolicy
	sourcePolicy.Model, sourcePolicy.ReasoningEffort = "gpt-5.6-terra", "high"
	sourcePolicy.ApprovalPolicy, sourcePolicy.SandboxMode = "on-request", "workspace-write"
	verifier.preflight = codexPreflight{EffectiveCodexPolicy: sourcePolicy}
	verifier.audits = []EffectiveCodexPolicy{sourcePolicy, sourcePolicy}
	verifier.confirm.ForkedFromID = managedSourceID
	verifier.recheck.ForkedFromID = managedSourceID
	verifier.read = []codexThreadIdentity{{ID: managedSourceID, CWD: request.Directory, ModelProvider: "openai"}, {ID: managedSourceID, CWD: request.Directory, ModelProvider: "openai"}}
	runner.results = []command.Result{
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("header")},
		managedAgentResult(t, source), {Stdout: managedEmptyComposer("header")}, {},
		managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedChildID, false)),
		managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedChildID, false)),
		jsonResult(t, listResponse(wireFromAgent(source))),
		managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedChildID, false)),
	}
	managed, err := launcher.Launch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	wantPolicy := ManagedCodexPolicy{Model: "gpt-5.6-terra", ReasoningEffort: "high", ApprovalPolicy: "on-request", SandboxMode: "workspace-write"}
	wantArgv := []string{"codex", "fork", "--search", "-m", "gpt-5.6-terra", "-c", `model_reasoning_effort="high"`, "--strict-config", "--remote", "unix:///tmp/codex-home/app-server-control/app-server-control.sock", "--cd", "/tmp/project", managedSourceID}
	if !reflect.DeepEqual(verifier.policies, []ManagedCodexPolicy{wantPolicy}) || !reflect.DeepEqual(managed.Argv, wantArgv) || managed.SourceThreadID != managedSourceID {
		t.Fatalf("preflight=%+v managed=%+v", verifier.policies, managed)
	}
}

func TestManagedCodexRejectsUnsafeExtraArgvAndInvalidInheritanceBeforeEnsure(t *testing.T) {
	unsafe := [][]string{
		{"--remote", "unix:///tmp/other"}, {"--cd", "/tmp/other"}, {"-m", "other"},
		{"--sandbox", "read-only"}, {"fork"}, {"--enable"}, {"--search", "--search"},
		{"--enable", "feature", "--enable", "feature"}, {"--enable", "bad value"}, {"--", "prompt"},
		{"--enable", "feature", "--disable", "feature"},
	}
	for _, argv := range unsafe {
		launcher, service, _, _, _, request := newManagedHarness(t, ManagedNew, managedNewID)
		request.ExtraArgv = argv
		if _, err := launcher.Launch(context.Background(), request); err == nil || service.callCount() != 0 {
			t.Fatalf("argv=%q error=%v ensure=%d", argv, err, service.callCount())
		}
	}
	for _, request := range []ManagedCodexRequest{
		{Operation: ManagedNew, Origin: managedOrigin(), Directory: "/tmp/project", Placement: sessionhost.NewTab, InheritSourcePolicy: true},
		{Operation: ManagedFork, Origin: managedOrigin(), Directory: "/tmp/project", Placement: sessionhost.NewTab, InheritSourcePolicy: true, Policy: managedPolicy()},
	} {
		service := &fakeManagedService{snapshot: managedServiceSnapshot()}
		launcher := NewManagedCodexLauncher(Host{Runner: &scriptedRunner{}, Session: "named"})
		launcher.service = service
		if _, err := launcher.Launch(context.Background(), request); err == nil || service.callCount() != 0 {
			t.Fatalf("request=%+v error=%v ensure=%d", request, err, service.callCount())
		}
	}
}

func TestForkPolicyAllowsOnlyExplicitLegacySandboxToImplicitBuiltinProvenance(t *testing.T) {
	expected := managedPreflight("/tmp/project").EffectiveCodexPolicy
	actual := expected
	actual.PermissionProfile = ":danger-full-access"
	if drift := forkPolicyDrift(actual, expected, []string{"codex", "-s", "danger-full-access"}); drift != "" {
		t.Fatalf("explicit legacy sandbox drift=%q", drift)
	}
	if drift := forkPolicyDrift(actual, expected, []string{"codex"}); drift != "permission_profile" {
		t.Fatalf("missing source flag drift=%q", drift)
	}
	expected.PermissionProfile = "team-profile"
	if drift := forkPolicyDrift(actual, expected, []string{"codex", "-s", "danger-full-access"}); drift != "permission_profile" {
		t.Fatalf("named profile drift=%q", drift)
	}
}

func TestManagedCodexResumeRunsExactIDInCallerProvedShell(t *testing.T) {
	launcher, _, topology, verifier, runner, request := newManagedHarness(t, ManagedResume, managedOriginalID)
	request.ThreadID, request.Placement = managedOriginalID, ""
	topology.target = sessionhost.CreatedTarget{Origin: request.Origin}
	verifier.read = []codexThreadIdentity{{ID: managedOriginalID, CWD: request.Directory, ModelProvider: "openai"}}
	runner.results = []command.Result{{}, managedAgentResult(t, managedAgent(request.Origin, request.Directory, managedOriginalID, false)), managedAgentResult(t, managedAgent(request.Origin, request.Directory, managedOriginalID, false)), managedAgentResult(t, managedAgent(request.Origin, request.Directory, managedOriginalID, false))}
	managed, err := launcher.Launch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "resume", "--remote", "unix:///tmp/codex-home/app-server-control/app-server-control.sock", "--cd", "/tmp/project", managedOriginalID}
	if len(topology.launches) != 0 || len(topology.runs) != 1 || !reflect.DeepEqual(topology.runs[0], want) || verifier.confirmSrc != managedOriginalID || managed.ThreadID != managedOriginalID {
		t.Fatalf("managed=%+v runs=%v verifier=%+v", managed, topology.runs, verifier)
	}
	if err := managed.Cleanup(context.Background()); err != nil || len(topology.stops) != 1 || len(topology.cleanups) != 0 {
		t.Fatalf("cleanup=%v stops=%v panes=%v", err, topology.stops, topology.cleanups)
	}
}

func TestManagedCodexConversionResumeReconcilesPolicyWithoutArgvOverrides(t *testing.T) {
	launcher, service, topology, verifier, runner, request := newManagedHarness(t, ManagedResume, managedOriginalID)
	request.ThreadID, request.Placement = managedOriginalID, ""
	source := managedPreflight(request.Directory).EffectiveCodexPolicy
	source.ModelProvider = "openai"
	source.MultiAgentMode = "explicitRequestOnly"
	target := source
	target.SandboxMode = "read-only"
	target.ReasoningEffort = "high"
	request.Policy = ManagedCodexPolicy{Model: target.Model, ReasoningEffort: target.ReasoningEffort, ApprovalPolicy: target.ApprovalPolicy, SandboxMode: target.SandboxMode}
	request.resumeSourcePolicy, request.resumeTargetPolicy = &source, &target
	var discoveryPolicy ManagedCodexPolicy
	launcher.discover = func(_ context.Context, got sessionhost.Origin, cwd string, _ []string, _ CodexServiceSnapshot, policy ManagedCodexPolicy) (Agent, string, error) {
		discoveryPolicy = policy
		agent := managedAgent(got, cwd, managedOriginalID, false)
		agent.Session = nil
		return agent, managedOriginalID, nil
	}
	verifier.read = []codexThreadIdentity{{ID: managedOriginalID, CWD: request.Directory, ModelProvider: "openai"}}
	drifted := target
	drifted.SandboxMode = "danger-full-access"
	verifier.audits = []EffectiveCodexPolicy{source, drifted}
	verifier.reconcile = target
	verifier.recheck.Model, verifier.recheck.ReasoningEffort = target.Model, target.ReasoningEffort
	runner.results = []command.Result{{}, managedAgentResult(t, managedAgent(request.Origin, request.Directory, managedOriginalID, false)), managedAgentResult(t, managedAgent(request.Origin, request.Directory, managedOriginalID, false)), managedAgentResult(t, managedAgent(request.Origin, request.Directory, managedOriginalID, false))}
	managed, err := launcher.Launch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "resume", "--remote", "unix://" + service.snapshot.SocketPath, "--cd", request.Directory, managedOriginalID}
	wantDiscovery := ManagedCodexPolicy{Model: source.Model, ReasoningEffort: source.ReasoningEffort, ApprovalPolicy: source.ApprovalPolicy, SandboxMode: source.SandboxMode}
	if !reflect.DeepEqual(discoveryPolicy, wantDiscovery) || !reflect.DeepEqual(managed.Argv, want) || len(topology.runs) != 1 || !reflect.DeepEqual(topology.runs[0], want) || len(verifier.reconciles) != 1 || requiredPolicyDrift(verifier.reconciles[0], target) != "" || requiredPolicyDrift(managed.Policy, target) != "" {
		t.Fatalf("managed=%+v discovery=%+v runs=%q reconciles=%+v", managed, discoveryPolicy, topology.runs, verifier.reconciles)
	}
}

func TestManagedCodexConversionResumeUnknownSettingsOutcomeCleansExactAttempt(t *testing.T) {
	launcher, _, topology, verifier, runner, request := newManagedHarness(t, ManagedResume, managedOriginalID)
	request.ThreadID, request.Placement = managedOriginalID, ""
	source := managedPreflight(request.Directory).EffectiveCodexPolicy
	source.ModelProvider = "openai"
	target := source
	target.SandboxMode = "read-only"
	request.Policy = ManagedCodexPolicy{Model: target.Model, ReasoningEffort: target.ReasoningEffort, ApprovalPolicy: target.ApprovalPolicy, SandboxMode: target.SandboxMode}
	request.resumeSourcePolicy, request.resumeTargetPolicy = &source, &target
	verifier.read = []codexThreadIdentity{{ID: managedOriginalID, CWD: request.Directory, ModelProvider: "openai"}}
	verifier.audits = []EffectiveCodexPolicy{source, source}
	verifier.reconcileErr = ErrManagedSettingsUnknown
	runner.results = []command.Result{{}, managedAgentResult(t, managedAgent(request.Origin, request.Directory, managedOriginalID, false)), managedAgentResult(t, managedAgent(request.Origin, request.Directory, managedOriginalID, false))}
	managed, err := launcher.Launch(context.Background(), request)
	if !errors.Is(err, ErrManagedSettingsUnknown) || managed != nil || len(verifier.reconciles) != 1 || len(topology.stops) != 1 || topology.stops[0].PID != 99 || len(topology.cleanups) != 0 {
		t.Fatalf("managed=%+v error=%v reconciles=%d stops=%+v cleanups=%+v", managed, err, len(verifier.reconciles), topology.stops, topology.cleanups)
	}
}

func TestResumePolicyCanonicalizationRequiresCapturedLegacySourceArgv(t *testing.T) {
	expected := managedPreflight("/tmp/project").EffectiveCodexPolicy
	actual := expected
	actual.PermissionProfile = ":danger-full-access"
	if drift := forkPolicyDrift(actual, expected, []string{"codex", "-s", "danger-full-access"}); drift != "" {
		t.Fatalf("captured legacy sandbox drift=%q", drift)
	}
	for _, argv := range [][]string{nil, {"codex", "-p", "custom", "-s", "danger-full-access"}, {"codex", "-c", "default_permissions=\"custom\"", "-s", "danger-full-access"}} {
		if drift := forkPolicyDrift(actual, expected, argv); drift != "permission_profile" {
			t.Fatalf("argv=%v drift=%q", argv, drift)
		}
	}
}

func TestManagedCodexFailureDeletesOnlyExactProvenChild(t *testing.T) {
	launcher, service, topology, verifier, runner, request := newManagedHarness(t, ManagedNew, managedNewID)
	verifier.recheck = codexThreadIdentity{ID: "drift", CWD: request.Directory, ModelProvider: "openai"}
	runner.results = []command.Result{{}, managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false)), managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false)), managedAgentResult(t, managedAgent(topology.target.Origin, request.Directory, managedNewID, false))}
	if _, err := launcher.Launch(context.Background(), request); !errors.Is(err, ErrManagedStale) {
		t.Fatalf("error=%v", err)
	}
	if service.callCount() != 1 || !reflect.DeepEqual(verifier.deleted, []string{managedNewID}) || len(topology.cleanups) != 1 {
		t.Fatalf("service=%d deleted=%v cleanup=%v", service.callCount(), verifier.deleted, topology.cleanups)
	}
}

func TestManagedCodexUnknownIdentityNeverDeletesSharedThread(t *testing.T) {
	launcher, _, topology, verifier, _, request := newManagedHarness(t, ManagedNew, managedNewID)
	launcher.discover = func(context.Context, sessionhost.Origin, string, []string, CodexServiceSnapshot, ManagedCodexPolicy) (Agent, string, error) {
		return Agent{}, "", ErrManagedNarrow
	}
	if _, err := launcher.Launch(context.Background(), request); !errors.Is(err, ErrManagedNarrow) || !errors.Is(err, ErrManagedCleanup) {
		t.Fatalf("error=%v", err)
	}
	if len(verifier.deleted) != 0 || len(topology.cleanups) != 1 {
		t.Fatalf("deleted=%v cleanup=%v", verifier.deleted, topology.cleanups)
	}
}

func TestManagedCodexRejectsInvalidInputsBeforeEnsureProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		request := ManagedCodexRequest{Operation: ManagedNew, Origin: managedOrigin(), Directory: rapid.SampledFrom([]string{"", "relative", "/tmp/../tmp/project"}).Draw(t, "directory"), Placement: sessionhost.NewTab, Policy: managedPolicy()}
		service := &fakeManagedService{snapshot: managedServiceSnapshot()}
		launcher := NewManagedCodexLauncher(Host{Runner: &scriptedRunner{}, Session: "named"})
		launcher.service = service
		if _, err := launcher.Launch(context.Background(), request); err == nil || service.callCount() != 0 {
			t.Fatalf("directory=%q error=%v calls=%d", request.Directory, err, service.callCount())
		}
	})
}

func wireFromAgent(agent Agent) wireAgent {
	wire := wireAgent{Agent: "codex", AgentStatus: agent.Status, CWD: agent.CWD, Focused: agent.Focused, WorkspaceID: agent.WorkspaceID, TabID: agent.TabID, PaneID: agent.PaneID, Revision: 1, StateChangeSeq: 1}
	if agent.Session != nil {
		wire.AgentSession = &wireSession{Source: agent.Session.Source, Agent: agent.Session.Agent, Kind: agent.Session.Kind, Value: agent.Session.Value}
	}
	return wire
}
