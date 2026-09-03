package form

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	if model.layout != newest || model.resizeGeneration != 2 {
		t.Fatalf("stale resize committed: %+v g%d", model.layout, model.resizeGeneration)
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
