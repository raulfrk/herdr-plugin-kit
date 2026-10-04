package agenthost

import (
	"strings"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/runtime/sessionhost"
)

type managedValidateRow struct {
	name      string
	operation ManagedOperation
	mutate    func(*ManagedCodexLauncher, *ManagedCodexRequest)
	// want is empty when the request must be accepted, otherwise a substring of the rejection.
	want string
}

func managedValidateBase(operation ManagedOperation) (ManagedCodexLauncher, ManagedCodexRequest) {
	origin := managedOrigin()
	request := ManagedCodexRequest{Operation: operation, Origin: origin, Directory: "/tmp/project", Placement: sessionhost.NewTab, Policy: managedPolicy()}
	switch operation {
	case ManagedFork:
		request.Placement, request.Direction = sessionhost.NewPane, "right"
		request.Source = managedAgent(origin, request.Directory, managedSourceID, false)
	case ManagedResume:
		request.Placement, request.ThreadID = "", managedOriginalID
	}
	return NewManagedCodexLauncher(Host{Runner: &scriptedRunner{}, Session: "named"}), request
}

func runManagedValidateRows(t *testing.T, rows []managedValidateRow) {
	t.Helper()
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			launcher, request := managedValidateBase(row.operation)
			if row.mutate != nil {
				row.mutate(&launcher, &request)
			}
			err := launcher.validate(request)
			switch {
			case row.want == "" && err != nil:
				t.Fatalf("valid request rejected: %v", err)
			case row.want != "" && (err == nil || !strings.Contains(err.Error(), row.want)):
				t.Fatalf("error=%v, want %q", err, row.want)
			}
		})
	}
}

// managedValidateResumePolicies pairs a source and target policy that match the base Resume request.
func managedValidateResumePolicies(request *ManagedCodexRequest) (*EffectiveCodexPolicy, *EffectiveCodexPolicy) {
	source := managedPreflight(request.Directory).EffectiveCodexPolicy
	target := source
	request.resumeSourcePolicy, request.resumeTargetPolicy = &source, &target
	return &source, &target
}

func TestManagedCodexValidateRunnerSessionAndDirectory(t *testing.T) {
	maxDirectory := "/" + strings.Repeat("a", maxPathBytes-1)
	runManagedValidateRows(t, []managedValidateRow{
		{"runner missing", ManagedNew, func(l *ManagedCodexLauncher, _ *ManagedCodexRequest) { l.Host.Runner = nil }, ErrManagedUnsupported.Error()},
		{"session differs from host", ManagedNew, func(l *ManagedCodexLauncher, _ *ManagedCodexRequest) { l.Host.Session = "other" }, ErrManagedUnsupported.Error()},
		{"default session", ManagedNew, func(l *ManagedCodexLauncher, r *ManagedCodexRequest) {
			l.Host.Session, r.Origin.SessionName = "", ""
		}, ""},
		{"invalid session name", ManagedNew, func(l *ManagedCodexLauncher, r *ManagedCodexRequest) {
			l.Host.Session, r.Origin.SessionName = "bad name", "bad name"
		}, "invalid session name"},
		{"directory at the byte limit", ManagedNew, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) { r.Directory = maxDirectory }, ""},
		{"directory over the byte limit", ManagedNew, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) { r.Directory = maxDirectory + "a" }, "directory must be clean"},
	})
}

func TestManagedCodexValidateReadinessTimeoutBounds(t *testing.T) {
	var rows []managedValidateRow
	for _, test := range []struct {
		timeout time.Duration
		valid   bool
	}{
		{0, true}, {-time.Nanosecond, false}, {time.Second, false}, {3 * time.Second, false}, {3*time.Second + time.Nanosecond, true},
		{10 * time.Second, true}, {MaxManagedReadyTimeout, true}, {MaxManagedReadyTimeout + time.Nanosecond, false},
	} {
		want := ""
		if !test.valid {
			want = "readiness timeout is out of range"
		}
		timeout := test.timeout
		rows = append(rows, managedValidateRow{timeout.String(), ManagedNew, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) { r.ReadyTimeout = timeout }, want})
	}
	runManagedValidateRows(t, rows)
}

func TestManagedCodexValidateResumeReconciliationPolicies(t *testing.T) {
	const invalid = "reconciliation policy is invalid"
	maxTier := strings.Repeat("a", maxIdentityBytes)
	runManagedValidateRows(t, []managedValidateRow{
		{"paired policies", ManagedResume, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) { managedValidateResumePolicies(r) }, ""},
		{"not a Resume", ManagedNew, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) { managedValidateResumePolicies(r) }, invalid},
		{"source directory differs", ManagedResume, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) {
			source, _ := managedValidateResumePolicies(r)
			source.CWD = "/tmp/other"
		}, invalid},
		{"target directory differs", ManagedResume, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) {
			_, target := managedValidateResumePolicies(r)
			target.CWD = "/tmp/other"
		}, invalid},
		{"target policy differs from the request", ManagedResume, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) {
			_, target := managedValidateResumePolicies(r)
			target.Model = "other-model"
		}, invalid},
		{"service tier at the byte limit", ManagedResume, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) {
			source, _ := managedValidateResumePolicies(r)
			source.ServiceTier = maxTier
		}, ""},
		{"service tier over the byte limit", ManagedResume, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) {
			source, _ := managedValidateResumePolicies(r)
			source.ServiceTier = maxTier + "a"
		}, "service tier is invalid"},
	})
}

func TestManagedCodexValidateNewRejectsEachExistingThreadField(t *testing.T) {
	const rejected = "managed New does not accept an existing thread"
	runManagedValidateRows(t, []managedValidateRow{
		{"fresh", ManagedNew, nil, ""},
		{"thread id", ManagedNew, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) { r.ThreadID = managedChildID }, rejected},
		{"source pane", ManagedNew, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) { r.Source.PaneID = "w1:p3" }, rejected},
		{"source argv", ManagedNew, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) { r.SourceArgv = []string{"codex"} }, rejected},
	})
}

func TestManagedCodexValidateForkSourceIdentityRejectsEachMismatchAlone(t *testing.T) {
	const rejected = "managed Fork source identity is invalid"
	fork := func(name string, mutate func(*ManagedCodexRequest)) managedValidateRow {
		return managedValidateRow{name, ManagedFork, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) { mutate(r) }, rejected}
	}
	runManagedValidateRows(t, []managedValidateRow{
		{"matching source", ManagedFork, nil, ""},
		fork("thread id", func(r *ManagedCodexRequest) { r.ThreadID = managedChildID }),
		fork("source argv", func(r *ManagedCodexRequest) { r.SourceArgv = []string{"codex"} }),
		fork("invalid target", func(r *ManagedCodexRequest) { r.Source.Status = "bogus" }),
		fork("no session", func(r *ManagedCodexRequest) { r.Source.Session = nil }),
		fork("not Codex", func(r *ManagedCodexRequest) { r.Source.Kind = "claude" }),
		fork("session source", func(r *ManagedCodexRequest) { r.Source.Session.Source = "other" }),
		fork("session agent", func(r *ManagedCodexRequest) { r.Source.Session.Agent = "other" }),
		fork("session kind", func(r *ManagedCodexRequest) { r.Source.Session.Kind = "name" }),
		fork("session name", func(r *ManagedCodexRequest) { r.Source.SessionName = "other" }),
		fork("workspace", func(r *ManagedCodexRequest) { r.Source.WorkspaceID = "w2" }),
		fork("tab", func(r *ManagedCodexRequest) { r.Source.TabID = "w1:t9" }),
		fork("pane", func(r *ManagedCodexRequest) { r.Source.PaneID = "w1:p9" }),
	})
}

func TestManagedCodexValidateForkSourceDirectory(t *testing.T) {
	const rejected = "managed Fork source CWD is invalid"
	maxDirectory := "/" + strings.Repeat("a", maxPathBytes-1)
	source := func(name, cwd, want string) managedValidateRow {
		return managedValidateRow{name, ManagedFork, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) { r.Source.CWD = cwd }, want}
	}
	runManagedValidateRows(t, []managedValidateRow{
		source("at the byte limit", maxDirectory, ""),
		source("over the byte limit", maxDirectory+"a", rejected),
		source("relative", "relative/path", rejected),
		source("unclean", "/tmp//project", rejected),
		source("NUL byte", "/tmp/project\x00", rejected),
	})
}

func TestManagedCodexValidateResumeSourceArgvAndPlacement(t *testing.T) {
	maxLabel := strings.Repeat("a", maxIdentityBytes)
	label := func(name, value, want string) managedValidateRow {
		return managedValidateRow{name, ManagedNew, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) { r.TabLabel = value }, want}
	}
	direction := func(value, want string) managedValidateRow {
		return managedValidateRow{"direction " + value, ManagedNew, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) {
			r.Placement, r.Direction = sessionhost.NewPane, value
		}, want}
	}
	runManagedValidateRows(t, []managedValidateRow{
		{"resume without captured argv", ManagedResume, nil, ""},
		{"resume with captured argv", ManagedResume, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) {
			r.SourceArgv = []string{"codex", "-s", "read-only"}
		}, ""},
		{"resume with foreign argv", ManagedResume, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) { r.SourceArgv = []string{"other"} }, "source argv is invalid"},
		label("label at the byte limit", maxLabel, ""),
		label("label over the byte limit", maxLabel+"a", "tab label is invalid"),
		label("label with a newline", "two\nlines", "tab label is invalid"),
		direction("right", ""),
		direction("down", ""),
		direction("left", "direction must be right or down"),
		direction("", "direction must be right or down"),
	})
}

func TestManagedCodexValidatePolicyTokenBytes(t *testing.T) {
	maxToken := strings.Repeat("a", maxIdentityBytes)
	runManagedValidateRows(t, []managedValidateRow{
		{"model at the byte limit", ManagedNew, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) { r.Policy.Model = maxToken }, ""},
		{"model over the byte limit", ManagedNew, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) { r.Policy.Model = maxToken + "a" }, "model is invalid"},
		{"reasoning effort at the byte limit", ManagedNew, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) { r.Policy.ReasoningEffort = maxToken }, ""},
		{"reasoning effort over the byte limit", ManagedNew, func(_ *ManagedCodexLauncher, r *ManagedCodexRequest) { r.Policy.ReasoningEffort = maxToken + "a" }, "reasoning effort is invalid"},
	})
}

func TestValidateManagedSourceArgvAcceptsExactBounds(t *testing.T) {
	atLimit := make([]string, sessionhost.MaxArgv)
	for i := range atLimit {
		atLimit[i] = "x"
	}
	atLimit[0] = "codex"
	if err := validateManagedSourceArgv(atLimit); err != nil {
		t.Fatalf("argument count at the limit rejected: %v", err)
	}
	if err := validateManagedSourceArgv([]string{"codex", strings.Repeat("x", sessionhost.MaxArgBytes)}); err != nil {
		t.Fatalf("argument at the byte limit rejected: %v", err)
	}
}
