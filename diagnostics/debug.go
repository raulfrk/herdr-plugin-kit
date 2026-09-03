package diagnostics

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

const (
	DefaultDebugPageSize = 50
	MaxDebugPageSize     = 100
)

// DebugQuery selects a bounded page from the recorder's current retained
// events. All string filters are exact, static semantic identifiers.
type DebugQuery struct {
	Session     ID
	Level       Level
	Kind        Kind
	Component   ID
	Action      ID
	Code        ID
	Correlation ID
	Page        int
	PageSize    int
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
	if query.PageSize == 0 {
		query.PageSize = DefaultDebugPageSize
	}
	if query.PageSize < 1 || query.PageSize > MaxDebugPageSize {
		return DebugPage{}, errors.New("debug page size must be between 1 and 100")
	}
	if query.Level != "" && query.Level != LevelDebug && query.Level != LevelInfo && query.Level != LevelWarn && query.Level != LevelError {
		return DebugPage{}, errors.New("invalid debug level filter")
	}
	if query.Kind != "" && query.Kind != KindLifecycle && query.Kind != KindInteraction && query.Kind != KindDiagnostic {
		return DebugPage{}, errors.New("invalid debug kind filter")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return DebugPage{}, ErrClosed
	}

	page := DebugPage{Page: query.Page, PageSize: query.PageSize, Health: debugHealth(r.health)}
	page.Sessions = debugSessions(r.records)
	matching := make([]DebugEvent, 0, min(len(r.records), query.PageSize))
	const maxInt64 = int64(^uint64(0) >> 1)
	start := int64(len(r.records))
	if int64(query.Page) <= maxInt64/int64(query.PageSize) {
		start = int64(query.Page) * int64(query.PageSize)
	} else {
		start = int64(len(r.records))
	}
	for index := len(r.records) - 1; index >= 0; index-- {
		projected, ok := projectDebugEvent(r.records[index].event)
		if !ok || !matchesDebug(projected, query) {
			continue
		}
		page.Total++
		if int64(page.Total) <= start || len(matching) == query.PageSize {
			continue
		}
		matching = append(matching, projected)
	}
	page.Events = matching
	page.HasPrev = query.Page > 0
	page.HasNext = start < int64(page.Total) && int64(page.Total)-start > int64(query.PageSize)
	return page, nil
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
	if len(data) > limit && len(report.Sessions) > 0 {
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

func debugHealth(health Health) DebugHealth {
	return DebugHealth{
		Writable: health.Writable, Pressure: health.Pressure, Dropped: health.Dropped,
		CorruptRecords: health.CorruptRecords, Events: health.Events, Bytes: health.Bytes,
		MaxEvents: health.MaxEvents, MaxBytes: health.MaxBytes, UsageRatio: health.UsageRatio,
	}
}

func debugSessions(records []storedEvent) []DebugSession {
	byID := make(map[ID]DebugSession)
	for _, record := range records {
		projected, ok := projectDebugEvent(record.event)
		if !ok || projected.Session.IsZero() {
			continue
		}
		id := projected.Session
		session := byID[id]
		session.ID = id
		session.Events++
		if session.First.IsZero() || record.event.Time.Before(session.First) {
			session.First = record.event.Time
		}
		if record.event.Time.After(session.Last) {
			session.Last = record.event.Time
		}
		byID[id] = session
	}
	sessions := make([]DebugSession, 0, len(byID))
	for _, session := range byID {
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if !sessions[i].Last.Equal(sessions[j].Last) {
			return sessions[i].Last.After(sessions[j].Last)
		}
		return sessions[i].ID.String() < sessions[j].ID.String()
	})
	return sessions
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
	if nanoseconds, ok := int64Detail(details["duration_ns"]); ok && nanoseconds >= 0 {
		event.Duration = time.Duration(nanoseconds)
	}
	event.Geometry.ReportedColumns, _ = intDetail(details["reported_columns"])
	event.Geometry.ReportedRows, _ = intDetail(details["reported_rows"])
	event.Geometry.RenderColumns, _ = intDetail(details["render_columns"])
	event.Geometry.RenderRows, _ = intDetail(details["render_rows"])
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
