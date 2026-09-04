// Package form provides the shared Bento Command configuration form surface.
package form

import (
	"context"
	"errors"
	"fmt"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/interaction"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

type Values map[string]string
type FieldErrors map[string]string

type Field struct {
	ID     string
	Label  string
	Value  string
	Error  string
	Secret bool
}

type Validator func(Values) FieldErrors
type ApplyToken string

type Applier interface {
	Apply(context.Context, Values) (ApplyToken, error)
	Rollback(context.Context, ApplyToken) error
}

type Options struct {
	Fields   []Field
	Validate Validator
	Apply    Applier
}

type state string

const (
	statePristine    state = "pristine"
	stateEditing     state = "editing"
	stateInvalid     state = "invalid"
	stateApplying    state = "applying"
	stateApplied     state = "applied"
	stateFailed      state = "failed"
	stateRollingBack state = "rolling-back"
	stateRolledBack  state = "rolled-back"
)

type applyResult struct {
	token          ApplyToken
	applyFailed    bool
	autoRolledBack bool
}

type rollbackResult struct{}

type Model struct {
	options            Options
	fields             []Field
	buffers            []interaction.Buffer
	errors             FieldErrors
	focus              int
	layout             responsive.Layout
	resizeGeneration   uint64
	applyGeneration    uint64
	rollbackGeneration uint64
	pendingApply       bool
	pendingRollback    bool
	state              state
	submitted          Values
	appliedValues      Values
	rollbackValues     Values
	rollbackToken      ApplyToken
	compensationNeeded bool
	applyKey           shell.RequestKey
	rollbackKey        shell.RequestKey
}

var _ shell.Surface = (*Model)(nil)

func New(options Options) (*Model, error) {
	if len(options.Fields) == 0 {
		return nil, errors.New("form has no fields")
	}
	seen := make(map[string]struct{}, len(options.Fields))
	model := &Model{options: options, fields: append([]Field(nil), options.Fields...), state: statePristine}
	model.buffers = make([]interaction.Buffer, len(model.fields))
	model.errors = make(FieldErrors)
	for index, field := range model.fields {
		if _, err := diagnostics.NewID(field.ID); err != nil {
			return nil, fmt.Errorf("form field %d has invalid ID: %w", index, err)
		}
		if field.Label == "" {
			return nil, fmt.Errorf("form field %q has an empty label", field.ID)
		}
		if _, exists := seen[field.ID]; exists {
			return nil, fmt.Errorf("duplicate form field ID %q", field.ID)
		}
		seen[field.ID] = struct{}{}
		model.buffers[index] = interaction.NewBuffer(field.Value)
		if field.Error != "" {
			model.errors[field.ID] = field.Error
		}
	}
	model.appliedValues = model.Values()
	model.applyKey, _ = shell.NewRequestKey("form.apply")
	model.rollbackKey, _ = shell.NewRequestKey("form.rollback")
	return model, nil
}

func (model *Model) Values() Values {
	values := make(Values, len(model.fields))
	for index, field := range model.fields {
		values[field.ID] = model.buffers[index].Text()
	}
	return values
}

func cloneValues(values Values) Values {
	cloned := make(Values, len(values))
	for id, value := range values {
		cloned[id] = value
	}
	return cloned
}

func (model *Model) Update(events shell.EventContext, event shell.Event) []shell.Effect {
	switch event := event.(type) {
	case shell.ResizeEvent:
		if event.Generation <= model.resizeGeneration {
			return nil
		}
		model.layout, model.resizeGeneration = event.Layout, event.Generation
	case shell.TextEvent:
		if event.Alt && event.Text == "r" && model.rollbackToken != "" && !model.pendingApply && !model.pendingRollback {
			return model.startRollback(events)
		}
		model.buffers[model.focus].Insert(event.Text)
		model.changed()
	case shell.KeyEvent:
		return model.key(events, event.Code)
	case shell.ResultEvent:
		model.resolve(event)
	}
	return nil
}

func (model *Model) key(events shell.EventContext, key shell.KeyCode) []shell.Effect {
	switch key {
	case shell.KeyCtrlC, shell.KeyEscape:
		return []shell.Effect{shell.Quit()}
	case shell.KeyTab, shell.KeyDown:
		model.focus = (model.focus + 1) % len(model.fields)
	case shell.KeyUp:
		model.focus = (model.focus + len(model.fields) - 1) % len(model.fields)
	case shell.KeyLeft:
		model.buffers[model.focus].MoveLeft()
	case shell.KeyRight:
		model.buffers[model.focus].MoveRight()
	case shell.KeyHome:
		model.buffers[model.focus].MoveHome()
	case shell.KeyEnd:
		model.buffers[model.focus].MoveEnd()
	case shell.KeyBackspace:
		if model.buffers[model.focus].Backspace() {
			model.changed()
		}
	case shell.KeyDelete:
		if model.buffers[model.focus].Delete() {
			model.changed()
		}
	case shell.KeyEnter:
		return model.startApply(events)
	}
	return nil
}

func (model *Model) changed() {
	delete(model.errors, model.fields[model.focus].ID)
	if !model.pendingApply && !model.pendingRollback && !model.compensationNeeded {
		model.state = stateEditing
	}
}

func (model *Model) validate() bool {
	model.errors = make(FieldErrors)
	if model.options.Validate != nil {
		for id, message := range model.options.Validate(model.Values()) {
			if _, exists := model.fieldIndex(id); exists && message != "" {
				model.errors[id] = message
			}
		}
	}
	if len(model.errors) != 0 {
		model.state = stateInvalid
		return false
	}
	return true
}

func (model *Model) fieldIndex(id string) (int, bool) {
	for index, field := range model.fields {
		if field.ID == id {
			return index, true
		}
	}
	return 0, false
}

func (model *Model) startApply(events shell.EventContext) []shell.Effect {
	if model.options.Apply == nil || model.pendingApply || model.pendingRollback || model.compensationNeeded || !model.validate() {
		return nil
	}
	model.submitted = model.Values()
	model.rollbackValues = cloneValues(model.appliedValues)
	snapshot, applier := cloneValues(model.submitted), model.options.Apply
	effect, err := events.Start(model.applyKey, applyWork(applier, snapshot))
	if err != nil {
		model.state = stateFailed
		return nil
	}
	model.pendingApply = true
	model.applyGeneration++
	model.state = stateApplying
	return []shell.Effect{effect}
}

func applyWork(applier Applier, snapshot Values) shell.Work {
	return func(ctx context.Context) shell.WorkResult {
		token, err := applier.Apply(ctx, cloneValues(snapshot))
		result := applyResult{token: token, applyFailed: err != nil}
		if err != nil && token != "" {
			result.autoRolledBack = applier.Rollback(ctx, token) == nil
		}
		if err != nil {
			return shell.WorkResult{Value: result, Code: diagnostics.OutcomeFailed, Err: errors.New("form apply failed")}
		}
		return shell.WorkResult{Value: result, Code: diagnostics.OutcomeApplied}
	}
}

func (model *Model) startRollback(events shell.EventContext) []shell.Effect {
	token, applier := model.rollbackToken, model.options.Apply
	effect, err := events.Start(model.rollbackKey, rollbackWork(applier, token))
	if err != nil {
		model.state = stateFailed
		return nil
	}
	model.pendingRollback = true
	model.rollbackGeneration++
	model.state = stateRollingBack
	return []shell.Effect{effect}
}

func rollbackWork(applier Applier, token ApplyToken) shell.Work {
	return func(ctx context.Context) shell.WorkResult {
		if err := applier.Rollback(ctx, token); err != nil {
			return shell.WorkResult{Code: diagnostics.OutcomeFailed, Err: errors.New("form rollback failed")}
		}
		return shell.WorkResult{Value: rollbackResult{}, Code: diagnostics.OutcomeApplied}
	}
}

func (model *Model) resolve(event shell.ResultEvent) {
	if event.Key == model.applyKey {
		if !model.pendingApply || event.Generation != model.applyGeneration {
			return
		}
		model.pendingApply = false
		result, ok := event.Result.Value.(applyResult)
		if !ok || event.Result.Err != nil || result.applyFailed {
			if ok && result.token != "" && !result.autoRolledBack {
				model.rollbackToken = result.token
				model.compensationNeeded = true
			}
			model.state = stateFailed
			model.submitted = nil
			return
		}
		model.rollbackToken = result.token
		applied := cloneValues(model.submitted)
		model.appliedValues = cloneValues(applied)
		model.submitted = nil
		if valuesEqual(model.Values(), applied) {
			model.state = stateApplied
		} else {
			model.state = stateEditing
		}
		return
	}
	if event.Key == model.rollbackKey {
		if !model.pendingRollback || event.Generation != model.rollbackGeneration {
			return
		}
		model.pendingRollback = false
		if event.Result.Err != nil {
			model.state = stateFailed
			return
		}
		for index, field := range model.fields {
			model.buffers[index].Set(model.rollbackValues[field.ID])
		}
		model.appliedValues = cloneValues(model.rollbackValues)
		model.rollbackToken = ""
		model.compensationNeeded = false
		model.state = stateRolledBack
	}
}

func valuesEqual(left, right Values) bool {
	if len(left) != len(right) {
		return false
	}
	for id, value := range left {
		if right[id] != value {
			return false
		}
	}
	return true
}

func (model *Model) Render(context shell.RenderContext) (*view.Frame, error) {
	return model.render(context)
}

func (model *Model) DiagnosticState() diagnostics.VisualState {
	screen, _ := diagnostics.NewID("form")
	focus, _ := diagnostics.NewID(model.fields[model.focus].ID)
	stateID, _ := diagnostics.NewID(string(model.state))
	return diagnostics.VisualState{
		Screen: screen, Focus: focus, State: stateID, Geometry: diagnostics.Geometry{
			ReportedColumns: model.layout.Reported.Columns, ReportedRows: model.layout.Reported.Rows,
			RenderColumns: model.layout.Render.Columns, RenderRows: model.layout.Render.Rows,
		},
		ResizeGeneration: model.resizeGeneration, RequestGeneration: max(model.applyGeneration, model.rollbackGeneration),
		ItemCount: len(model.fields), SelectedIndex: model.focus,
		Pending: model.pendingApply || model.pendingRollback, HasError: len(model.errors) != 0 || model.state == stateFailed,
	}
}
