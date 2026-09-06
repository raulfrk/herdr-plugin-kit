package diagnostics

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

var semanticIDPattern = regexp.MustCompile("^[a-z][a-z0-9._-]{0,63}$")

const semanticSchemaVersion = 1

// ID is a bounded identifier used by semantic diagnostics. Callers must create
// IDs only from static developer-defined values, never from user content.
type ID struct {
	value string
}

func NewID(value string) (ID, error) {
	if !semanticIDPattern.MatchString(value) {
		return ID{}, errors.New("invalid semantic identifier")
	}
	return ID{value: value}, nil
}

func (id ID) String() string { return id.value }
func (id ID) IsZero() bool   { return id.value == "" }

func (id ID) MarshalJSON() ([]byte, error) {
	return json.Marshal(id.value)
}

func (id *ID) UnmarshalJSON(data []byte) error {
	const invalid = "invalid semantic identifier JSON"
	var decoded any
	if id == nil || json.Unmarshal(data, &decoded) != nil {
		return errors.New(invalid)
	}
	switch value := decoded.(type) {
	case string:
		if value == "" {
			*id = ID{}
			return nil
		}
		candidate, err := NewID(value)
		if err != nil {
			return errors.New(invalid)
		}
		*id = candidate
		return nil
	case map[string]any:
		if len(value) == 0 {
			*id = ID{}
			return nil
		}
	}
	return errors.New(invalid)
}

type OutcomeCode string

const (
	OutcomeApplied   OutcomeCode = "applied"
	OutcomeIgnored   OutcomeCode = "ignored"
	OutcomeRejected  OutcomeCode = "rejected"
	OutcomeStale     OutcomeCode = "stale"
	OutcomeCancelled OutcomeCode = "cancelled"
	OutcomeFailed    OutcomeCode = "failed"
)

func (outcome OutcomeCode) Valid() bool {
	switch outcome {
	case OutcomeApplied, OutcomeIgnored, OutcomeRejected, OutcomeStale, OutcomeCancelled, OutcomeFailed:
		return true
	default:
		return false
	}
}

type Geometry struct {
	ReportedColumns int
	ReportedRows    int
	RenderColumns   int
	RenderRows      int
}

// VisualState is an allowlisted semantic UI projection. It deliberately has no
// free-form text fields.
type VisualState struct {
	Screen            ID
	Focus             ID
	Selection         ID
	State             ID
	Geometry          Geometry
	ResizeGeneration  uint64
	RequestGeneration uint64
	ItemCount         int
	SelectedIndex     int
	Pending           bool
	HasError          bool
}

type SemanticEvent struct {
	Level             Level
	Kind              Kind
	Plugin            ID
	Component         ID
	Action            ID
	Correlation       ID
	Code              ID
	Outcome           OutcomeCode
	Before            ID
	After             ID
	Generation        uint64
	RelatedGeneration uint64
	Count             int
	Bytes             int
	Graphemes         int
	Alt               bool
	Paste             bool
	Duration          time.Duration
	Geometry          Geometry
	Visual            *VisualState
}

type SemanticSink interface {
	RecordSemantic(SemanticEvent) error
}

func (r *Recorder) RecordSemantic(input SemanticEvent) error {
	_, err := r.RecordSemanticWithSequence(input)
	return err
}

// RecordSemanticWithSequence records one semantic event and returns its exact
// committed sequence. A failed record returns sequence zero.
func (r *Recorder) RecordSemanticWithSequence(input SemanticEvent) (uint64, error) {
	if err := validateSemanticEvent(input); err != nil {
		return 0, err
	}
	details := map[string]any{
		"semantic_schema": semanticSchemaVersion,
		"outcome":         string(input.Outcome), "generation": input.Generation,
		"related_generation": input.RelatedGeneration, "count": input.Count,
		"bytes": input.Bytes, "graphemes": input.Graphemes,
		"alt": input.Alt, "paste": input.Paste,
		"duration_ns":      input.Duration.Nanoseconds(),
		"reported_columns": input.Geometry.ReportedColumns, "reported_rows": input.Geometry.ReportedRows,
		"render_columns": input.Geometry.RenderColumns, "render_rows": input.Geometry.RenderRows,
		"state_before": input.Before.String(), "state_after": input.After.String(),
	}
	event := Event{
		Level: input.Level, Kind: input.Kind, Plugin: input.Plugin.String(),
		Component: input.Component.String(), Action: input.Action.String(),
		CorrelationID: input.Correlation.String(), Message: input.Code.String(), Details: details,
	}
	if input.Visual != nil {
		encoded, err := encodeVisualState(*input.Visual)
		if err != nil {
			return 0, err
		}
		event.UISnapshot = &UISnapshot{Name: "semantic-ui", Text: string(encoded)}
	}
	stored, err := r.record(event)
	return stored.Sequence, err
}

func validateSemanticEvent(input SemanticEvent) error {
	if input.Code.IsZero() {
		return errors.New("semantic event code is empty")
	}
	for name, id := range map[string]ID{
		"plugin": input.Plugin, "component": input.Component, "action": input.Action,
		"correlation": input.Correlation, "before": input.Before, "after": input.After,
	} {
		if !id.IsZero() && !semanticIDPattern.MatchString(id.String()) {
			return fmt.Errorf("invalid %s identifier", name)
		}
	}
	if !input.Outcome.Valid() {
		return errors.New("invalid semantic outcome")
	}
	if input.Count < 0 || input.Bytes < 0 || input.Graphemes < 0 || input.Duration < 0 {
		return errors.New("semantic measurements must not be negative")
	}
	if input.Visual != nil {
		if err := validateVisualState(*input.Visual); err != nil {
			return err
		}
	}
	return nil
}

func validateVisualState(state VisualState) error {
	for name, id := range map[string]ID{
		"screen": state.Screen, "focus": state.Focus, "selection": state.Selection, "state": state.State,
	} {
		if !id.IsZero() && !semanticIDPattern.MatchString(id.String()) {
			return fmt.Errorf("invalid visual %s identifier", name)
		}
	}
	if state.ItemCount < 0 || state.SelectedIndex < 0 {
		return errors.New("visual counts must not be negative")
	}
	if state.Geometry.ReportedColumns < 0 || state.Geometry.ReportedRows < 0 ||
		state.Geometry.RenderColumns < 0 || state.Geometry.RenderRows < 0 {
		return errors.New("visual geometry must not be negative")
	}
	return nil
}

func encodeVisualState(state VisualState) ([]byte, error) {
	return json.Marshal(visualWireFromState(state))
}

type geometryWire struct {
	ReportedColumns int `json:"reported_columns"`
	ReportedRows    int `json:"reported_rows"`
	RenderColumns   int `json:"render_columns"`
	RenderRows      int `json:"render_rows"`
}

type visualWire struct {
	Screen            string       `json:"screen"`
	Focus             string       `json:"focus"`
	Selection         string       `json:"selection"`
	State             string       `json:"state"`
	Geometry          geometryWire `json:"geometry"`
	ResizeGeneration  uint64       `json:"resize_generation"`
	RequestGeneration uint64       `json:"request_generation"`
	ItemCount         int          `json:"item_count"`
	SelectedIndex     int          `json:"selected_index"`
	Pending           bool         `json:"pending"`
	HasError          bool         `json:"has_error"`
}

func visualWireFromState(state VisualState) visualWire {
	return visualWire{
		Screen: state.Screen.String(), Focus: state.Focus.String(), Selection: state.Selection.String(), State: state.State.String(),
		Geometry: geometryWire{
			ReportedColumns: state.Geometry.ReportedColumns, ReportedRows: state.Geometry.ReportedRows,
			RenderColumns: state.Geometry.RenderColumns, RenderRows: state.Geometry.RenderRows,
		},
		ResizeGeneration: state.ResizeGeneration, RequestGeneration: state.RequestGeneration,
		ItemCount: state.ItemCount, SelectedIndex: state.SelectedIndex, Pending: state.Pending, HasError: state.HasError,
	}
}
