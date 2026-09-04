package form

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
	"pgregory.net/rapid"
)

type fakeApplier struct {
	token       ApplyToken
	applyErr    error
	rollbackErr error
	applied     Values
	rolledBack  []ApplyToken
}

type requestSink struct {
	completed chan string
}

func (sink requestSink) RecordSemantic(event diagnostics.SemanticEvent) error {
	if event.Code.String() == "request.completed" {
		sink.completed <- event.Action.String()
	}
	return nil
}

func (fake *fakeApplier) Apply(_ context.Context, values Values) (ApplyToken, error) {
	fake.applied = cloneValues(values)
	values["name"] = "mutated-by-applier"
	return fake.token, fake.applyErr
}

func (fake *fakeApplier) Rollback(_ context.Context, token ApplyToken) error {
	fake.rolledBack = append(fake.rolledBack, token)
	return fake.rollbackErr
}

func newForm(t testing.TB, applier Applier) *Model {
	t.Helper()
	model, err := New(Options{
		Fields: []Field{{ID: "name", Label: "Name", Value: "before"}, {ID: "token", Label: "Token", Value: "secret", Secret: true}},
		Validate: func(values Values) FieldErrors {
			if values["name"] == "" {
				return FieldErrors{"name": "required"}
			}
			return nil
		},
		Apply: applier,
	})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func frameRow(frame *view.Frame, row int) string {
	var text strings.Builder
	for column := 0; column < frame.Width(); column++ {
		cell, _ := frame.CellAt(column, row)
		if !cell.Continuation {
			if cell.Text == "" {
				text.WriteByte(' ')
			} else {
				text.WriteString(cell.Text)
			}
		}
	}
	return strings.TrimRight(text.String(), " ")
}

func waitRequest(t *testing.T, completed <-chan string, want string) {
	t.Helper()
	select {
	case got := <-completed:
		if got != want {
			t.Fatalf("completed request = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", want)
	}
}

func testPluginID(t *testing.T) diagnostics.ID {
	t.Helper()
	id, err := diagnostics.NewID("form-test")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestFormValidationSubmittedSnapshotAndRollback(t *testing.T) {
	fake := &fakeApplier{token: "rollback-1"}
	model := newForm(t, fake)
	model.buffers[0].Set("")
	if model.validate() || model.state != stateInvalid || model.errors["name"] != "required" {
		t.Fatalf("validation state=%s errors=%v", model.state, model.errors)
	}
	model.buffers[0].Set("submitted")
	model.errors = nil
	model.submitted = model.Values()
	model.rollbackValues = cloneValues(model.appliedValues)
	model.pendingApply, model.applyGeneration, model.state = true, 2, stateApplying
	model.Update(shell.EventContext{}, shell.TextEvent{Text: "-newer"})
	model.resolve(shell.ResultEvent{Key: model.applyKey, Generation: 1, Result: shell.WorkResult{Value: applyResult{token: fake.token}, Code: diagnostics.OutcomeApplied}})
	if !model.pendingApply {
		t.Fatal("stale apply completion changed pending state")
	}
	model.resolve(shell.ResultEvent{Key: model.applyKey, Generation: 2, Result: shell.WorkResult{Value: applyResult{token: fake.token}, Code: diagnostics.OutcomeApplied}})
	if model.pendingApply || model.state != stateEditing || model.Values()["name"] != "submitted-newer" || model.appliedValues["name"] != "submitted" {
		t.Fatalf("resolved concurrent edit state=%s values=%v applied=%v", model.state, model.Values(), model.appliedValues)
	}
	model.pendingRollback, model.rollbackGeneration, model.state = true, 1, stateRollingBack
	model.resolve(shell.ResultEvent{Key: model.rollbackKey, Generation: 1, Result: shell.WorkResult{Value: rollbackResult{}, Code: diagnostics.OutcomeApplied}})
	if model.state != stateRolledBack || model.Values()["name"] != "before" || model.rollbackToken != "" {
		t.Fatalf("rollback state=%s values=%v token=%q", model.state, model.Values(), model.rollbackToken)
	}
}

func TestEditingKeysFollowBufferAndFocusState(t *testing.T) {
	model := newForm(t, nil)
	model.buffers[0].Set("áb")

	model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyHome})
	model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyDelete})
	if got := model.Values()["name"]; got != "b" {
		t.Fatalf("delete at home = %q, want b", got)
	}
	model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnd})
	model.Update(shell.EventContext{}, shell.TextEvent{Text: "👩‍💻"})
	model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyLeft})
	model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyRight})
	model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyBackspace})
	if got := model.Values()["name"]; got != "b" || model.state != stateEditing {
		t.Fatalf("grapheme edit value=%q state=%s", got, model.state)
	}

	model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyTab})
	if model.focus != 1 {
		t.Fatalf("tab focus = %d", model.focus)
	}
	model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyDown})
	if model.focus != 0 {
		t.Fatalf("down did not wrap focus: %d", model.focus)
	}
	model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyUp})
	if model.focus != 1 {
		t.Fatalf("up did not wrap focus: %d", model.focus)
	}

	for _, key := range []shell.KeyCode{shell.KeyEscape, shell.KeyCtrlC} {
		if effects := model.Update(shell.EventContext{}, shell.KeyEvent{Code: key}); len(effects) != 1 {
			t.Fatalf("%s effects = %d, want one quit", key, len(effects))
		}
	}
}

func TestNoOpEditsPreserveStateAndFieldErrorsClearIndependently(t *testing.T) {
	model, err := New(Options{Fields: []Field{
		{ID: "first", Label: "First", Error: "first error"},
		{ID: "second", Label: "Second", Error: "second error"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyHome})
	model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyBackspace})
	if model.state != statePristine || len(model.errors) != 2 {
		t.Fatalf("no-op backspace state=%s errors=%v", model.state, model.errors)
	}
	model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnd})
	model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyDelete})
	if model.state != statePristine || len(model.errors) != 2 {
		t.Fatalf("no-op delete state=%s errors=%v", model.state, model.errors)
	}
	model.Update(shell.EventContext{}, shell.TextEvent{Text: "draft"})
	if model.state != stateEditing || model.errors["first"] != "" || model.errors["second"] != "second error" {
		t.Fatalf("field edit state=%s errors=%v", model.state, model.errors)
	}
}

func TestValidationAcceptsOnlyKnownNonEmptyFieldErrors(t *testing.T) {
	model, err := New(Options{
		Fields: []Field{{ID: "name", Label: "Name"}, {ID: "token", Label: "Token"}},
		Validate: func(Values) FieldErrors {
			return FieldErrors{"name": "required", "token": "", "unknown": "not a field"}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if model.validate() || model.state != stateInvalid {
		t.Fatalf("validation accepted: state=%s errors=%v", model.state, model.errors)
	}
	if len(model.errors) != 1 || model.errors["name"] != "required" {
		t.Fatalf("filtered errors = %v", model.errors)
	}
}

func TestApplyGuardsAndStartFailureAreObservable(t *testing.T) {
	t.Run("nil applier", func(t *testing.T) {
		model := newForm(t, nil)
		model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
		if model.state != statePristine || model.applyGeneration != 0 {
			t.Fatalf("nil apply state=%s generation=%d", model.state, model.applyGeneration)
		}
	})
	t.Run("validation", func(t *testing.T) {
		model := newForm(t, &fakeApplier{})
		model.buffers[0].Set("")
		model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
		if model.state != stateInvalid || model.errors["name"] != "required" || model.applyGeneration != 0 {
			t.Fatalf("invalid apply state=%s errors=%v generation=%d", model.state, model.errors, model.applyGeneration)
		}
	})
	for _, test := range []struct {
		name       string
		configure  func(*Model)
		wantState  state
		generation uint64
	}{
		{name: "pending apply", configure: func(model *Model) { model.pendingApply = true }, wantState: statePristine},
		{name: "pending rollback", configure: func(model *Model) { model.pendingRollback = true }, wantState: statePristine},
		{name: "compensation", configure: func(model *Model) { model.compensationNeeded = true }, wantState: statePristine},
		{name: "context rejects start", configure: func(*Model) {}, wantState: stateFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := newForm(t, &fakeApplier{})
			test.configure(model)
			model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
			if model.state != test.wantState || model.applyGeneration != test.generation {
				t.Fatalf("state=%s generation=%d", model.state, model.applyGeneration)
			}
		})
	}
}

func TestShellDrivesApplyThenExplicitRollback(t *testing.T) {
	fake := &fakeApplier{token: "rollback-1"}
	model := newForm(t, fake)
	palette, err := theme.Builtin("terminal")
	if err != nil {
		t.Fatal(err)
	}
	completed := make(chan string, 4)
	program, err := shell.NewProgram(shell.ProgramOptions{
		PluginID: testPluginID(t), Theme: palette, Events: requestSink{completed: completed},
		Input: bytes.NewReader(nil), Output: io.Discard,
	}, model)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, runErr := program.Run()
		done <- runErr
	}()
	program.Send(tea.WindowSizeMsg{Width: 80, Height: 18})
	program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("-draft")})
	program.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitRequest(t, completed, "form.apply")
	program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r"), Alt: true})
	waitRequest(t, completed, "form.rollback")
	program.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("form program did not stop")
	}
	if fake.applied["name"] != "before-draft" || len(fake.rolledBack) != 1 || fake.rolledBack[0] != "rollback-1" {
		t.Fatalf("apply=%v rollbacks=%v", fake.applied, fake.rolledBack)
	}
	if model.state != stateRolledBack || model.pendingApply || model.pendingRollback || model.applyGeneration != 1 || model.rollbackGeneration != 1 {
		t.Fatalf("final state=%s apply pending=%v g%d rollback pending=%v g%d", model.state, model.pendingApply, model.applyGeneration, model.pendingRollback, model.rollbackGeneration)
	}
	if got := model.Values()["name"]; got != "before" {
		t.Fatalf("rolled-back value = %q", got)
	}
}

func TestApplyWorkClonesValuesAndAutomaticallyRollsBackPartialFailure(t *testing.T) {
	fake := &fakeApplier{token: "partial", applyErr: errors.New("failed")}
	snapshot := Values{"name": "safe"}
	result := applyWork(fake, snapshot)(context.Background())
	if result.Err == nil || result.Err.Error() != "form apply failed" || len(fake.rolledBack) != 1 || fake.rolledBack[0] != "partial" {
		t.Fatalf("partial apply result=%+v rollbacks=%v", result, fake.rolledBack)
	}
	if snapshot["name"] != "safe" || fake.applied["name"] != "safe" {
		t.Fatalf("apply values aliased: snapshot=%v applied=%v", snapshot, fake.applied)
	}
}

func TestApplyWorkOutcomeMatrix(t *testing.T) {
	for _, test := range []struct {
		name           string
		fake           fakeApplier
		wantErr        bool
		wantAuto       bool
		wantRollbacks  int
		wantApplyError bool
	}{
		{name: "success", fake: fakeApplier{token: "saved"}},
		{name: "failure without token", fake: fakeApplier{applyErr: errors.New("rejected")}, wantErr: true, wantApplyError: true},
		{name: "compensated partial failure", fake: fakeApplier{token: "partial", applyErr: errors.New("partial")}, wantErr: true, wantAuto: true, wantRollbacks: 1, wantApplyError: true},
		{name: "uncompensated partial failure", fake: fakeApplier{token: "partial", applyErr: errors.New("partial"), rollbackErr: errors.New("rollback")}, wantErr: true, wantRollbacks: 1, wantApplyError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := test.fake
			result := applyWork(&fake, Values{"name": "candidate"})(context.Background())
			applied, ok := result.Value.(applyResult)
			if !ok {
				t.Fatalf("result value type = %T", result.Value)
			}
			if (result.Err != nil) != test.wantErr || applied.applyFailed != test.wantApplyError || applied.autoRolledBack != test.wantAuto {
				t.Fatalf("result=%+v applied=%+v", result, applied)
			}
			if applied.token != fake.token || len(fake.rolledBack) != test.wantRollbacks {
				t.Fatalf("token=%q rollbacks=%v", applied.token, fake.rolledBack)
			}
			wantCode := diagnostics.OutcomeApplied
			if test.wantErr {
				wantCode = diagnostics.OutcomeFailed
			}
			if result.Code != wantCode {
				t.Fatalf("outcome = %s, want %s", result.Code, wantCode)
			}
		})
	}
}

func TestApplyResolutionFailureMatrix(t *testing.T) {
	for _, test := range []struct {
		name             string
		result           shell.WorkResult
		wantToken        ApplyToken
		wantCompensation bool
	}{
		{name: "missing payload", result: shell.WorkResult{Code: diagnostics.OutcomeApplied}},
		{name: "work error", result: shell.WorkResult{Value: applyResult{token: "partial"}, Err: errors.New("failed"), Code: diagnostics.OutcomeFailed}, wantToken: "partial", wantCompensation: true},
		{name: "declared failure", result: shell.WorkResult{Value: applyResult{applyFailed: true}, Code: diagnostics.OutcomeFailed}},
		{name: "already compensated", result: shell.WorkResult{Value: applyResult{token: "partial", applyFailed: true, autoRolledBack: true}, Code: diagnostics.OutcomeFailed}},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := newForm(t, &fakeApplier{})
			model.submitted = Values{"name": "candidate", "token": "secret"}
			model.pendingApply = true
			model.applyGeneration = 3
			model.resolve(shell.ResultEvent{Key: model.applyKey, Generation: 3, Result: test.result})
			if model.pendingApply || model.state != stateFailed || model.submitted != nil {
				t.Fatalf("state=%s pending=%v submitted=%v", model.state, model.pendingApply, model.submitted)
			}
			if model.rollbackToken != test.wantToken || model.compensationNeeded != test.wantCompensation {
				t.Fatalf("token=%q compensation=%v", model.rollbackToken, model.compensationNeeded)
			}
		})
	}
}

func TestApplyResolutionUsesSubmittedGenerationAndValueSnapshot(t *testing.T) {
	for _, test := range []struct {
		name      string
		liveValue string
		wantState state
	}{
		{name: "unchanged", liveValue: "submitted", wantState: stateApplied},
		{name: "edited while applying", liveValue: "newer", wantState: stateEditing},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := newForm(t, &fakeApplier{})
			model.submitted = Values{"name": "submitted", "token": "secret"}
			model.rollbackValues = Values{"name": "before", "token": "secret"}
			model.buffers[0].Set(test.liveValue)
			model.pendingApply = true
			model.applyGeneration = 2
			model.resolve(shell.ResultEvent{Key: model.applyKey, Generation: 2, Result: shell.WorkResult{
				Value: applyResult{token: "rollback"}, Code: diagnostics.OutcomeApplied,
			}})
			if model.pendingApply || model.state != test.wantState || model.rollbackToken != "rollback" || model.submitted != nil {
				t.Fatalf("state=%s pending=%v token=%q submitted=%v", model.state, model.pendingApply, model.rollbackToken, model.submitted)
			}
			if model.appliedValues["name"] != "submitted" || model.Values()["name"] != test.liveValue {
				t.Fatalf("applied=%v live=%v", model.appliedValues, model.Values())
			}
		})
	}
}

func TestFailedAutomaticRollbackRetainsTokenForExplicitRetry(t *testing.T) {
	fake := &fakeApplier{token: "partial", applyErr: errors.New("failed"), rollbackErr: errors.New("still partial")}
	model := newForm(t, fake)
	model.submitted = model.Values()
	model.rollbackValues = cloneValues(model.appliedValues)
	model.pendingApply, model.applyGeneration, model.state = true, 1, stateApplying
	result := applyWork(fake, model.submitted)(context.Background())
	model.resolve(shell.ResultEvent{Key: model.applyKey, Generation: 1, Result: result})
	if model.state != stateFailed || model.rollbackToken != "partial" || len(fake.rolledBack) != 1 {
		t.Fatalf("failed rollback state=%s token=%q calls=%v", model.state, model.rollbackToken, fake.rolledBack)
	}
	model.Update(shell.EventContext{}, shell.TextEvent{Text: "x"})
	model.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
	if model.state != stateFailed || model.applyGeneration != 1 || model.rollbackToken != "partial" {
		t.Fatalf("unresolved compensation was replaced: state=%s generation=%d token=%q", model.state, model.applyGeneration, model.rollbackToken)
	}
	fake.rollbackErr = nil
	retry := rollbackWork(fake, model.rollbackToken)(context.Background())
	model.pendingRollback, model.rollbackGeneration = true, 1
	model.resolve(shell.ResultEvent{Key: model.rollbackKey, Generation: 1, Result: retry})
	if model.state != stateRolledBack || model.rollbackToken != "" || len(fake.rolledBack) != 2 {
		t.Fatalf("retry state=%s token=%q calls=%v", model.state, model.rollbackToken, fake.rolledBack)
	}
}

func TestRollbackWorkAndResolutionFailuresPreserveRecoverability(t *testing.T) {
	fake := &fakeApplier{rollbackErr: errors.New("unavailable")}
	result := rollbackWork(fake, "saved")(context.Background())
	if result.Err == nil || result.Err.Error() != "form rollback failed" || result.Code != diagnostics.OutcomeFailed || len(fake.rolledBack) != 1 {
		t.Fatalf("rollback result=%+v calls=%v", result, fake.rolledBack)
	}

	model := newForm(t, fake)
	model.rollbackToken = "saved"
	model.rollbackValues = Values{"name": "before", "token": "secret"}
	model.buffers[0].Set("after")
	model.pendingRollback = true
	model.rollbackGeneration = 2
	model.resolve(shell.ResultEvent{Key: model.rollbackKey, Generation: 1, Result: shell.WorkResult{Value: rollbackResult{}, Code: diagnostics.OutcomeApplied}})
	if !model.pendingRollback || model.Values()["name"] != "after" {
		t.Fatalf("stale rollback changed state: pending=%v values=%v", model.pendingRollback, model.Values())
	}
	model.resolve(shell.ResultEvent{Key: model.rollbackKey, Generation: 2, Result: result})
	if model.pendingRollback || model.state != stateFailed || model.rollbackToken != "saved" || model.Values()["name"] != "after" {
		t.Fatalf("failed rollback state=%s pending=%v token=%q values=%v", model.state, model.pendingRollback, model.rollbackToken, model.Values())
	}
}

func TestAltRollbackRequiresEverySafetyCondition(t *testing.T) {
	for _, test := range []struct {
		name                string
		configure           func(*Model)
		wantState           state
		wantPendingApply    bool
		wantPendingRollback bool
	}{
		{name: "not alt", configure: func(*Model) {}, wantState: stateEditing},
		{name: "wrong text", configure: func(*Model) {}, wantState: stateEditing},
		{name: "no token", configure: func(model *Model) { model.rollbackToken = "" }, wantState: stateEditing},
		{name: "apply pending", configure: func(model *Model) { model.pendingApply = true }, wantState: statePristine, wantPendingApply: true},
		{name: "rollback pending", configure: func(model *Model) { model.pendingRollback = true }, wantState: statePristine, wantPendingRollback: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := newForm(t, &fakeApplier{})
			model.rollbackToken = "saved"
			test.configure(model)
			event := shell.TextEvent{Text: "r", Alt: true}
			if test.name == "not alt" {
				event.Alt = false
			}
			if test.name == "wrong text" {
				event.Text = "x"
			}
			model.Update(shell.EventContext{}, event)
			if model.state != test.wantState || model.pendingApply != test.wantPendingApply || model.pendingRollback != test.wantPendingRollback || model.rollbackGeneration != 0 {
				t.Fatalf("state=%s apply pending=%v rollback pending=%v generation=%d", model.state, model.pendingApply, model.pendingRollback, model.rollbackGeneration)
			}
		})
	}

	model := newForm(t, &fakeApplier{})
	model.rollbackToken = "saved"
	model.Update(shell.EventContext{}, shell.TextEvent{Text: "r", Alt: true})
	if model.state != stateFailed || model.pendingRollback || model.rollbackGeneration != 0 {
		t.Fatalf("start failure state=%s pending=%v generation=%d", model.state, model.pendingRollback, model.rollbackGeneration)
	}
}

func TestFormRecoveryStaleResizeSecretAndDiagnosticPrivacy(t *testing.T) {
	canary := "secret-value-/private/path"
	model, err := New(Options{Fields: []Field{{ID: "secret", Label: "Secret", Value: canary, Secret: true}}})
	if err != nil {
		t.Fatal(err)
	}
	newest := responsive.Resolve(responsive.Size{Columns: 110, Rows: 24})
	older := responsive.Resolve(responsive.Size{Columns: 40, Rows: 10})
	model.Update(shell.EventContext{}, shell.ResizeEvent{Layout: newest, Generation: 2})
	model.Update(shell.EventContext{}, shell.ResizeEvent{Layout: older, Generation: 1})
	model.Update(shell.EventContext{}, shell.ResizeEvent{Layout: older, Generation: 2})
	if model.layout != newest || model.resizeGeneration != 2 {
		t.Fatalf("stale or duplicate resize committed: %+v g%d", model.layout, model.resizeGeneration)
	}
	state := model.DiagnosticState()
	if state.Geometry.RenderColumns != 110 || strings.Contains(state.Focus.String()+state.State.String(), canary) {
		t.Fatalf("unsafe diagnostic state: %+v", state)
	}
	palette, _ := theme.Builtin("terminal")
	recovery := responsive.Resolve(responsive.Size{Columns: 20, Rows: 5})
	frame, err := model.Render(shell.RenderContext{Layout: recovery, Theme: palette})
	if err != nil || !strings.Contains(view.ANSI(frame), "Need 40×10") || strings.Contains(view.ANSI(frame), canary) {
		t.Fatalf("recovery frame err=%v text=%q", err, view.ANSI(frame))
	}
	supported, _ := model.Render(shell.RenderContext{Layout: newest, Theme: palette})
	if text := view.ANSI(supported); strings.Contains(text, canary) || !strings.Contains(text, strings.Repeat("•", 26)) {
		t.Fatalf("secret render = %q", text)
	}
}

func TestRecoveryFrameHonorsTinyBoundariesAndReportedGeometry(t *testing.T) {
	model := newForm(t, nil)
	palette, _ := theme.Builtin("terminal")
	for _, test := range []struct {
		name       string
		layout     responsive.Layout
		wantRows   map[int]string
		wantAbsent string
	}{
		{
			name: "zero render", layout: responsive.Resolve(responsive.Size{}),
			wantRows: map[int]string{}, wantAbsent: "CONFIGURATION",
		},
		{
			name: "one cell", layout: responsive.Resolve(responsive.Size{Columns: 1, Rows: 1}),
			wantRows: map[int]string{0: ""}, wantAbsent: "Terminal",
		},
		{
			name: "narrow body has no writable width", layout: responsive.Resolve(responsive.Size{Columns: 2, Rows: 3}),
			wantRows: map[int]string{0: "", 1: "", 2: ""}, wantAbsent: "Terminal",
		},
		{
			name: "exact three recovery lines", layout: responsive.Resolve(responsive.Size{Columns: 39, Rows: 5}),
			wantRows: map[int]string{
				0: " CONFIGURATION", 2: " Terminal is too small",
				3: " Need 40×10 · received 39×5", 4: " Resize to continue; your draft is pr…",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame, err := model.Render(shell.RenderContext{Layout: test.layout, Theme: palette})
			if err != nil || frame.Width() != test.layout.Render.Columns || frame.Height() != test.layout.Render.Rows {
				t.Fatalf("frame=%v err=%v layout=%+v", frame, err, test.layout)
			}
			for row, want := range test.wantRows {
				if got := frameRow(frame, row); got != want {
					t.Fatalf("row %d = %q, want %q", row, got, want)
				}
			}
			if test.wantAbsent != "" && strings.Contains(view.ANSI(frame), test.wantAbsent) {
				t.Fatalf("frame unexpectedly contains %q", test.wantAbsent)
			}
		})
	}
}

func TestSemanticLayoutsKeepChromeFocusAndErrorsWithinBounds(t *testing.T) {
	fields := make([]Field, 9)
	for index := range fields {
		fields[index] = Field{ID: "field-" + string(rune('a'+index)), Label: "Field " + string(rune('A'+index)), Value: "value"}
	}
	fields[7].Error = "boundary error"
	model, err := New(Options{Fields: fields})
	if err != nil {
		t.Fatal(err)
	}
	model.focus = 7
	model.state = stateFailed
	model.rollbackToken = "saved"
	palette, _ := theme.Builtin("terminal")

	for _, test := range []struct {
		name          string
		size          responsive.Size
		class         responsive.Class
		visibleLabels []string
		hiddenLabels  []string
		focusRow      int
		errorRow      int
	}{
		{
			name: "compact lower boundary", size: responsive.Size{Columns: 40, Rows: 10}, class: responsive.Compact,
			visibleLabels: []string{"Field F", "Field G", "Field H"}, hiddenLabels: []string{"Field E", "Field I"}, focusRow: 6, errorRow: 7,
		},
		{
			name: "compact upper boundary", size: responsive.Size{Columns: 79, Rows: 17}, class: responsive.Compact,
			visibleLabels: []string{"Field B", "Field H"}, hiddenLabels: []string{"Field A", "Field I"}, focusRow: 14, errorRow: 15,
		},
		{
			name: "standard lower boundary", size: responsive.Size{Columns: 80, Rows: 18}, class: responsive.Standard,
			visibleLabels: []string{"Field B", "Field H"}, hiddenLabels: []string{"Field A", "Field I"}, focusRow: 14, errorRow: 15,
		},
		{
			name: "standard upper boundary", size: responsive.Size{Columns: 109, Rows: 23}, class: responsive.Standard,
			visibleLabels: []string{"Field A", "Field H", "Field I"}, focusRow: 16, errorRow: 17,
		},
		{
			name: "wide lower boundary", size: responsive.Size{Columns: 110, Rows: 24}, class: responsive.Wide,
			visibleLabels: []string{"Field A", "Field H", "Field I"}, focusRow: 16, errorRow: 17,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			layout := responsive.Resolve(test.size)
			if layout.Class != test.class {
				t.Fatalf("class = %s, want %s", layout.Class, test.class)
			}
			frame, err := model.Render(shell.RenderContext{Layout: layout, Theme: palette})
			if err != nil {
				t.Fatal(err)
			}
			if got := frameRow(frame, 0); !strings.HasPrefix(got, " CONFIGURATION") {
				t.Fatalf("header row = %q", got)
			}
			if got := frameRow(frame, 1); !strings.Contains(got, "failed · Alt-r rollback") {
				t.Fatalf("status row = %q", got)
			}
			if got := frameRow(frame, frame.Height()-1); !strings.HasPrefix(got, "Tab/↑↓ field · Enter apply") {
				t.Fatalf("help row = %q", got)
			}
			all := view.ANSI(frame)
			for _, label := range test.visibleLabels {
				if !strings.Contains(all, label) {
					t.Errorf("missing visible label %q", label)
				}
			}
			for _, label := range test.hiddenLabels {
				if strings.Contains(all, label) {
					t.Errorf("unexpected hidden label %q", label)
				}
			}
			if got := frameRow(frame, test.focusRow); !strings.HasPrefix(got, "▌ Field H: value") {
				t.Fatalf("focus row %d = %q", test.focusRow, got)
			}
			if got := frameRow(frame, test.errorRow); !strings.HasPrefix(got, "  ! boundary error") {
				t.Fatalf("error row %d = %q", test.errorRow, got)
			}
			focusCell, _ := frame.CellAt(0, test.focusRow)
			if !focusCell.Style.Bold || focusCell.Style.Background != palette.ActiveRowBackground {
				t.Fatalf("focus style = %+v", focusCell.Style)
			}
			errorCell, _ := frame.CellAt(2, test.errorRow)
			if errorCell.Style.Foreground != palette.Red || errorCell.Style.Background != palette.ActiveRowBackground {
				t.Fatalf("error style = %+v", errorCell.Style)
			}
			afterError, _ := frame.CellAt(0, test.errorRow+1)
			if test.errorRow+1 < frame.Height()-1 && afterError.Style.Background == palette.ActiveRowBackground {
				t.Fatalf("focus background leaked below two-row field at row %d", test.errorRow+1)
			}
		})
	}
}

func TestErrorAtBottomBoundaryDoesNotOverwriteHelp(t *testing.T) {
	model, err := New(Options{Fields: []Field{{ID: "only", Label: "Only", Error: "invalid"}}})
	if err != nil {
		t.Fatal(err)
	}
	palette, _ := theme.Builtin("terminal")
	layout := responsive.Layout{Reported: responsive.Size{Columns: 40, Rows: 4}, Render: responsive.Size{Columns: 40, Rows: 4}, Class: responsive.Compact}
	frame, err := model.Render(shell.RenderContext{Layout: layout, Theme: palette})
	if err != nil {
		t.Fatal(err)
	}
	if got := frameRow(frame, 2); !strings.HasPrefix(got, "▌ Only:") {
		t.Fatalf("field row = %q", got)
	}
	if got := frameRow(frame, 3); !strings.HasPrefix(got, "Tab/↑↓ field") || strings.Contains(got, "invalid") {
		t.Fatalf("bottom help row = %q", got)
	}
}

func TestFormValuesAreClonedProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		value := rapid.String().Draw(t, "value")
		model, err := New(Options{Fields: []Field{{ID: "value", Label: "Value", Value: value}}})
		if err != nil {
			t.Fatal(err)
		}
		copy := model.Values()
		copy["value"] = "changed-outside"
		if model.Values()["value"] != value {
			t.Fatal("Values aliases form state")
		}
	})
}

func TestFormResponsiveMatrixAcrossThemesPreservesDraftAndFocus(t *testing.T) {
	model := newForm(t, nil)
	model.buffers[0].Set("draft")
	model.focus = 1
	sizes := []responsive.Size{
		{Columns: 20, Rows: 5}, {Columns: 40, Rows: 10}, {Columns: 48, Rows: 18}, {Columns: 48, Rows: 30},
		{Columns: 78, Rows: 10}, {Columns: 78, Rows: 20}, {Columns: 80, Rows: 18}, {Columns: 110, Rows: 24},
		{Columns: 500, Rows: 200}, {Columns: 900, Rows: 300},
	}
	for _, themeID := range theme.IDs() {
		palette, _ := theme.Builtin(themeID)
		for generation, size := range sizes {
			layout := responsive.Resolve(size)
			model.Update(shell.EventContext{}, shell.ResizeEvent{Layout: layout, Generation: uint64(generation + 1)})
			frame, err := model.Render(shell.RenderContext{Layout: layout, Theme: palette})
			if err != nil || frame.Width() != layout.Render.Columns || frame.Height() != layout.Render.Rows {
				t.Fatalf("theme=%s size=%+v frame=%v err=%v", themeID, size, frame, err)
			}
			if model.Values()["name"] != "draft" || model.focus != 1 {
				t.Fatalf("theme=%s size=%+v lost state values=%v focus=%d", themeID, size, model.Values(), model.focus)
			}
			text := view.ANSI(frame)
			if layout.Class == responsive.Recovery {
				if !strings.Contains(text, "Need 40×10") {
					t.Fatalf("theme=%s size=%+v missing Recovery explanation", themeID, size)
				}
			} else if !strings.Contains(text, "▌ Token") {
				t.Fatalf("theme=%s size=%+v missing visible focused field", themeID, size)
			}
		}
	}
}

func TestFormRejectsInvalidShape(t *testing.T) {
	for _, fields := range [][]Field{nil, {{ID: "Bad", Label: "Bad"}}, {{ID: "same", Label: "One"}, {ID: "same", Label: "Two"}}, {{ID: "valid"}}} {
		if _, err := New(Options{Fields: fields}); err == nil {
			t.Fatalf("accepted fields %#v", fields)
		}
	}
}
