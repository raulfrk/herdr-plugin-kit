package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"time"
)

const (
	DefaultDebugPageSize = 50
	MaxDebugPageSize     = 100
)

// DebugQuery selects a bounded page from the recorder's current retained
// events. All string filters are exact, static semantic identifiers.
type DebugQuery struct {
	Session      ID
	Level        Level
	Kind         Kind
	Component    ID
	Action       ID
	Code         ID
	Correlation  ID
	Page         int
	PageSize     int
	HideDebugger bool
}

// DebugHealth deliberately omits the recorder's free-form last error.
type DebugHealth struct {
	Writable       bool
	Pressure       bool
	Dropped        uint64
	CorruptRecords uint64
	Events         int
	Bytes          int64
	MaxEvents      int
	MaxBytes       int64
	UsageRatio     float64
}

type DebugSession struct {
	ID     ID
	Events int
	First  time.Time
	Last   time.Time
}

// DebugEvent is the privacy-safe event projection used by interactive debug
// surfaces. It contains no detail map, raw snapshot, path, or error text.
type DebugEvent struct {
	Sequence          uint64
	Time              time.Time
	Level             Level
	Kind              Kind
	Session           ID
	Component         ID
	Action            ID
	Code              ID
	Correlation       ID
	Outcome           OutcomeCode
	Generation        uint64
	RelatedGeneration uint64
	Count             int
	Bytes             int
	Graphemes         int
	Duration          time.Duration
	Geometry          Geometry
	Visual            *VisualState
	TextFit           *TextFitReport `json:"text_fit,omitempty"`
}

type DebugPage struct {
	Sessions []DebugSession
	Health   DebugHealth
	Events   []DebugEvent
	Page     int
	PageSize int
	Total    int
	HasPrev  bool
	HasNext  bool
}

type DebugExportOptions struct {
	Query    DebugQuery
	MaxBytes int
	Previews *PreviewStore
}

type DebugReport struct {
	Version           int            `json:"version"`
	GeneratedAt       string         `json:"generated_at"`
	Health            DebugHealth    `json:"health"`
	Sessions          []DebugSession `json:"sessions,omitempty"`
	Events            []DebugEvent   `json:"events"`
	Previews          []PreviewEntry `json:"previews,omitempty"`
	PreviewsOmitted   int            `json:"previews_omitted,omitempty"`
	Truncated         bool           `json:"truncated"`
	TruncationReasons []string       `json:"truncation_reasons,omitempty"`
}

// Debug returns newest-first data from this recorder only. Returned slices and
// values do not alias recorder storage.
func (r *Recorder) Debug(query DebugQuery) (DebugPage, error) {
	if query.Page < 0 {
		return DebugPage{}, errors.New("debug page must not be negative")
	}
	var err error
	query, err = normalizeDebugQuery(query)
	if err != nil {
		return DebugPage{}, err
	}

	requestedPage := query.Page
	query.Page = 0
	snapshot, err := r.DebugSnapshot(context.Background(), nil)
	if err != nil {
		return DebugPage{}, err
	}
	view, err := snapshot.Select(context.Background(), query, false)
	if err != nil {
		return DebugPage{}, err
	}
	start := debugPageStart(requestedPage, query.PageSize, len(view.events))
	window := view.Window(DebugWindowQuery{Start: int(start)})
	page := DebugPage{Sessions: view.Sessions(), Health: view.Health(), Events: window.Events, Page: requestedPage, PageSize: query.PageSize, Total: window.Total, HasPrev: requestedPage > 0, HasNext: window.HasNext}
	return page, nil
}

func normalizeDebugQuery(query DebugQuery) (DebugQuery, error) {
	if query.PageSize == 0 {
		query.PageSize = DefaultDebugPageSize
	}
	if query.PageSize < 1 || query.PageSize > MaxDebugPageSize {
		return DebugQuery{}, errors.New("debug page size must be between 1 and 100")
	}
	if query.Level != "" && query.Level != LevelDebug && query.Level != LevelInfo && query.Level != LevelWarn && query.Level != LevelError {
		return DebugQuery{}, errors.New("invalid debug level filter")
	}
	if query.Kind != "" && query.Kind != KindLifecycle && query.Kind != KindInteraction && query.Kind != KindDiagnostic {
		return DebugQuery{}, errors.New("invalid debug kind filter")
	}
	return query, nil
}

func cloneDebugEvent(event DebugEvent) DebugEvent {
	if event.Visual != nil {
		visual := *event.Visual
		event.Visual = &visual
	}
	event.TextFit = CloneTextFit(event.TextFit)
	return event
}

func isDebuggerVisual(event DebugEvent) bool {
	if event.Visual == nil {
		return false
	}
	switch event.Visual.Screen.String() {
	case "debug.health", "debug.timeline", "debug.gallery", "debug.hud", "debug.detail", "debug.help":
		return true
	default:
		return false
	}
}

// ExportDebug returns only the allowlisted debug projection. It never exports
// free-form errors, event details, metadata, raw snapshots, or ScreenshotRef.
func (r *Recorder) ExportDebug(options DebugExportOptions) ([]byte, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrClosed
	}
	limit, maximum := options.MaxBytes, r.config.MaxReportBytes
	generatedAt := r.reportAt
	r.mu.Unlock()
	if limit == 0 {
		limit = maximum
	}
	if limit < 1024 || limit > maximum {
		return nil, fmt.Errorf("debug report max bytes must be between 1024 and %d", maximum)
	}
	page, err := r.Debug(options.Query)
	if err != nil {
		return nil, err
	}
	report := DebugReport{
		Version: ReportSchemaVersion, GeneratedAt: generatedAt.Format("2006-01-02T15:04:05.000000000Z07:00"),
		Health: page.Health, Sessions: page.Sessions, Events: page.Events,
	}
	for _, event := range page.Events {
		if event.Visual == nil {
			continue
		}
		if options.Previews == nil {
			report.PreviewsOmitted++
			continue
		}
		entry, ok := options.Previews.Entry(event.Sequence, *event.Visual)
		if !ok {
			report.PreviewsOmitted++
			continue
		}
		report.Previews = append(report.Previews, entry)
	}
	encode := func() ([]byte, error) { return json.Marshal(report) }
	data, err := encode()
	if err != nil {
		return nil, fmt.Errorf("encode debug report: %w", err)
	}
	for len(data) > limit && len(report.Events) > 0 {
		removed := report.Events[len(report.Events)-1].Sequence
		report.Events = report.Events[:len(report.Events)-1]
		for index, entry := range report.Previews {
			if entry.Sequence == removed {
				report.Previews = append(report.Previews[:index], report.Previews[index+1:]...)
				break
			}
		}
		report.Truncated = true
		report.TruncationReasons = []string{"oldest_events_omitted"}
		data, _ = encode()
	}
	if len(data) > limit {
		report.Sessions = nil
		report.Truncated = true
		report.TruncationReasons = append(report.TruncationReasons, "session_index_omitted")
		data, _ = encode()
	}
	if len(data) > limit {
		return nil, errors.New("debug report byte limit is too small for the bounded report envelope")
	}
	return data, nil
}

func debugPageStart(page, pageSize, total int) int64 {
	high, low := bits.Mul64(uint64(page), uint64(pageSize))
	if high != 0 {
		return int64(total)
	}
	return int64(min(low, uint64(total)))
}

func debugHealth(health Health) DebugHealth {
	return DebugHealth{
		Writable: health.Writable, Pressure: health.Pressure, Dropped: health.Dropped,
		CorruptRecords: health.CorruptRecords, Events: health.Events, Bytes: health.Bytes,
		MaxEvents: health.MaxEvents, MaxBytes: health.MaxBytes, UsageRatio: health.UsageRatio,
	}
}

func projectDebugEvent(event Event) (DebugEvent, bool) {
	if schema, ok := intDetail(event.Details["semantic_schema"]); !ok || schema != semanticSchemaVersion {
		return DebugEvent{}, false
	}
	code, err := NewID(event.Message)
	if err != nil {
		return DebugEvent{}, false
	}
	projected := DebugEvent{Sequence: event.Sequence, Time: event.Time, Level: event.Level, Kind: event.Kind, Code: code}
	for _, field := range []struct {
		raw    string
		target *ID
	}{{event.Plugin, &projected.Session}, {event.Component, &projected.Component}, {event.Action, &projected.Action}, {event.CorrelationID, &projected.Correlation}} {
		raw, target := field.raw, field.target
		if raw == "" {
			continue
		}
		id, idErr := NewID(raw)
		if idErr != nil {
			return DebugEvent{}, false
		}
		*target = id
	}
	projectSemanticDetails(&projected, event.Details)
	if event.UISnapshot != nil && event.UISnapshot.Name == "semantic-ui" {
		projected.Visual = decodeVisual(event.UISnapshot.Text)
	}
	return projected, true
}

func projectSemanticDetails(event *DebugEvent, details map[string]any) {
	if outcome, ok := details["outcome"].(string); ok {
		candidate := OutcomeCode(outcome)
		if candidate.Valid() {
			event.Outcome = candidate
		}
	}
	event.Generation, _ = unsignedDetail(details["generation"])
	event.RelatedGeneration, _ = unsignedDetail(details["related_generation"])
	event.Count, _ = intDetail(details["count"])
	event.Bytes, _ = intDetail(details["bytes"])
	event.Graphemes, _ = intDetail(details["graphemes"])
	if nanoseconds, ok := int64Detail(details["duration_ns"]); ok {
		if nanoseconds >= 0 {
			event.Duration = time.Duration(nanoseconds)
		}
	}
	event.Geometry.ReportedColumns, _ = intDetail(details["reported_columns"])
	event.Geometry.ReportedRows, _ = intDetail(details["reported_rows"])
	event.Geometry.RenderColumns, _ = intDetail(details["render_columns"])
	event.Geometry.RenderRows, _ = intDetail(details["render_rows"])
	if raw, ok := details["text_fit"]; ok {
		encoded, err := json.Marshal(raw)
		if err == nil && textFitFieldsNonNull(encoded) {
			decoder := json.NewDecoder(bytes.NewReader(encoded))
			decoder.DisallowUnknownFields()
			var report TextFitReport
			if decoder.Decode(&report) == nil && decoder.Decode(&struct{}{}) == io.EOF && validateTextFit(report) == nil {
				event.TextFit = CloneTextFit(&report)
			}
		}
	}
}

// Missing optional fields retain their defaults; explicit null is unmeasured.
func textFitFieldsNonNull(encoded []byte) bool {
	var shape struct {
		Omitted      json.RawMessage              `json:"omitted"`
		Observations []map[string]json.RawMessage `json:"observations"`
	}
	if json.Unmarshal(encoded, &shape) != nil || bytes.Equal(shape.Omitted, []byte("null")) {
		return false
	}
	for _, observation := range shape.Observations {
		for _, value := range observation {
			if bytes.Equal(value, []byte("null")) {
				return false
			}
		}
	}
	return true
}

func decodeVisual(raw string) *VisualState {
	var wire visualWire
	if json.Unmarshal([]byte(raw), &wire) != nil {
		return nil
	}
	state := VisualState{
		Geometry:         Geometry{ReportedColumns: wire.Geometry.ReportedColumns, ReportedRows: wire.Geometry.ReportedRows, RenderColumns: wire.Geometry.RenderColumns, RenderRows: wire.Geometry.RenderRows},
		ResizeGeneration: wire.ResizeGeneration, RequestGeneration: wire.RequestGeneration,
		ItemCount: wire.ItemCount, SelectedIndex: wire.SelectedIndex, Pending: wire.Pending, HasError: wire.HasError,
	}
	for _, field := range []struct {
		raw    string
		target *ID
	}{{wire.Screen, &state.Screen}, {wire.Focus, &state.Focus}, {wire.Selection, &state.Selection}, {wire.State, &state.State}} {
		raw, target := field.raw, field.target
		if raw == "" {
			continue
		}
		id, err := NewID(raw)
		if err != nil {
			return nil
		}
		*target = id
	}
	if validateVisualState(state) != nil {
		return nil
	}
	return &state
}

func matchesDebug(event DebugEvent, query DebugQuery) bool {
	return (query.Session.IsZero() || event.Session == query.Session) &&
		(query.Level == "" || event.Level == query.Level) &&
		(query.Kind == "" || event.Kind == query.Kind) &&
		(query.Component.IsZero() || event.Component == query.Component) &&
		(query.Action.IsZero() || event.Action == query.Action) &&
		(query.Code.IsZero() || event.Code == query.Code) &&
		(query.Correlation.IsZero() || event.Correlation == query.Correlation)
}

func unsignedDetail(value any) (uint64, bool) {
	signed, ok := int64Detail(value)
	if !ok || signed < 0 {
		return 0, false
	}
	return uint64(signed), true
}

func intDetail(value any) (int, bool) {
	signed, ok := int64Detail(value)
	converted := int(signed)
	return converted, ok && int64(converted) == signed && converted >= 0
}

func int64Detail(value any) (int64, bool) {
	switch value := value.(type) {
	case int:
		return int64(value), true
	case int64:
		return value, true
	case uint64:
		if value <= uint64(^uint64(0)>>1) {
			return int64(value), true
		}
	case float64:
		converted := int64(value)
		if float64(converted) == value {
			return converted, true
		}
	}
	return 0, false
}
