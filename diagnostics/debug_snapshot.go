package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

// DebugSnapshot is an immutable point-in-time set of safe event projections.
type DebugSnapshot struct {
	events         []*DebugEvent
	sessions       []DebugSession
	health         DebugHealth
	generatedAt    time.Time
	previewEntries map[uint64]PreviewEntry
	maxReportBytes int
}

// DebugView is an immutable filtered selection from a DebugSnapshot.
type DebugView struct {
	events         []*DebugEvent
	sessions       []DebugSession
	health         DebugHealth
	generatedAt    time.Time
	previewEntries map[uint64]PreviewEntry
	pageSize       int
	maxReportBytes int
}

// DebugWindowQuery selects a bounded view window by offset or top sequence.
type DebugWindowQuery struct {
	Start       int
	TopSequence uint64
}

// DebugWindow is a copied bounded window and its navigation metadata.
type DebugWindow struct {
	Events                          []DebugEvent
	Start, Total                    int
	HasPrev, HasNext, AnchorMissing bool
}

// DebugSnapshot captures the recorder's current safe projections and preview
// eligibility without retaining the recorder lock during index preparation.
func (r *Recorder) DebugSnapshot(ctx context.Context, previews *PreviewStore) (*DebugSnapshot, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrClosed
	}
	refs := make([]*DebugEvent, 0, len(r.records))
	for _, record := range r.records {
		if record.debug != nil {
			refs = append(refs, record.debug)
		}
	}
	health, generatedAt, maxReportBytes := debugHealth(r.health), r.reportAt, r.config.MaxReportBytes
	r.mu.Unlock()
	snapshot := &DebugSnapshot{events: refs, health: health, generatedAt: generatedAt, previewEntries: make(map[uint64]PreviewEntry), maxReportBytes: maxReportBytes}
	snapshot.sessions = debugSessionsFromRefs(ctx, refs)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if previews != nil {
		for index, event := range refs {
			if index&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if event.Visual == nil {
				continue
			}
			if entry, ok := previews.Entry(event.Sequence, *event.Visual); ok {
				snapshot.previewEntries[event.Sequence] = entry
			}
		}
	}
	return snapshot, nil
}

func debugSessionsFromRefs(ctx context.Context, refs []*DebugEvent) []DebugSession {
	byID := make(map[ID]DebugSession)
	for index, event := range refs {
		if index&1023 == 0 && ctx.Err() != nil {
			return nil
		}
		if event.Session.IsZero() {
			continue
		}
		session := byID[event.Session]
		session.ID = event.Session
		session.Events++
		if session.First.IsZero() || event.Time.Before(session.First) {
			session.First = event.Time
		}
		if event.Time.After(session.Last) {
			session.Last = event.Time
		}
		byID[event.Session] = session
	}
	sessions := make([]DebugSession, 0, len(byID))
	for _, session := range byID {
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if !sessions[i].Last.Equal(sessions[j].Last) {
			return sessions[i].Last.After(sessions[j].Last)
		}
		return strings.Compare(sessions[i].ID.String(), sessions[j].ID.String()) < 0
	})
	return sessions
}

// Health returns the bounded health captured with the snapshot.
func (s *DebugSnapshot) Health() DebugHealth {
	if s == nil {
		return DebugHealth{}
	}
	return s.health
}

// Sessions returns a copy of the snapshot's session index.
func (s *DebugSnapshot) Sessions() []DebugSession {
	if s == nil {
		return nil
	}
	return append([]DebugSession(nil), s.sessions...)
}

// Select creates an immutable newest-first filtered view. Page must be zero;
// callers apply PageSize when requesting windows.
func (s *DebugSnapshot) Select(ctx context.Context, query DebugQuery, galleryOnly bool) (*DebugView, error) {
	if s == nil {
		return nil, errors.New("nil debug snapshot")
	}
	if query.Page != 0 {
		return nil, errors.New("snapshot selection requires page zero")
	}
	if query.PageSize == 0 {
		query.PageSize = DefaultDebugPageSize
	}
	if query.PageSize < 1 || query.PageSize > MaxDebugPageSize {
		return nil, errors.New("debug page size must be between 1 and 100")
	}
	if query.Level != "" && query.Level != LevelDebug && query.Level != LevelInfo && query.Level != LevelWarn && query.Level != LevelError {
		return nil, errors.New("invalid debug level filter")
	}
	if query.Kind != "" && query.Kind != KindLifecycle && query.Kind != KindInteraction && query.Kind != KindDiagnostic {
		return nil, errors.New("invalid debug kind filter")
	}
	events := make([]*DebugEvent, 0, len(s.events))
	for i := len(s.events) - 1; i >= 0; i-- {
		if len(events)&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		event := s.events[i]
		_, previewEligible := s.previewEntries[event.Sequence]
		if query.HideDebugger && isDebuggerVisual(*event) || galleryOnly && (event.Visual == nil || !previewEligible) || !matchesDebug(*event, query) {
			continue
		}
		events = append(events, event)
	}
	entries := make(map[uint64]PreviewEntry, len(s.previewEntries))
	for sequence, entry := range s.previewEntries {
		entries[sequence] = entry
	}
	return &DebugView{events: events, sessions: append([]DebugSession(nil), s.sessions...), health: s.health, generatedAt: s.generatedAt, previewEntries: entries, pageSize: query.PageSize, maxReportBytes: s.maxReportBytes}, nil
}

// Health returns the bounded health captured by the view's snapshot.
func (v *DebugView) Health() DebugHealth {
	if v == nil {
		return DebugHealth{}
	}
	return v.health
}

// Sessions returns a copy of the view's session index.
func (v *DebugView) Sessions() []DebugSession {
	if v == nil {
		return nil
	}
	return append([]DebugSession(nil), v.sessions...)
}

// PreviewEligible reports whether preview provenance was captured for sequence.
func (v *DebugView) PreviewEligible(sequence uint64) bool {
	if v == nil {
		return false
	}
	_, ok := v.previewEntries[sequence]
	return ok
}

// Locate returns an exact sequence position when present. Otherwise it returns
// the nearest older position, or the nearest newer position when no older event
// exists. An empty view returns -1, false.
func (v *DebugView) Locate(sequence uint64) (int, bool) {
	if v == nil || len(v.events) == 0 {
		return -1, false
	}
	index := sort.Search(len(v.events), func(i int) bool { return v.events[i].Sequence <= sequence })
	if index == len(v.events) {
		return len(v.events) - 1, false
	}
	return index, v.events[index].Sequence == sequence
}

// Window returns copied events and navigation metadata for a bounded view span.
func (v *DebugView) Window(query DebugWindowQuery) DebugWindow {
	if v == nil {
		return DebugWindow{}
	}
	start, missing := query.Start, false
	if query.TopSequence != 0 {
		var exact bool
		start, exact = v.Locate(query.TopSequence)
		missing = !exact
	}
	if start < 0 {
		start = 0
	}
	if start > len(v.events) {
		start = len(v.events)
	}
	size := v.pageSize
	end := min(len(v.events), start+size)
	events := make([]DebugEvent, end-start)
	for i := range events {
		events[i] = cloneDebugEvent(*v.events[start+i])
	}
	return DebugWindow{Events: events, Start: start, Total: len(v.events), HasPrev: start > 0, HasNext: end < len(v.events), AnchorMissing: missing}
}

// ExportWindow exports exactly the captured window membership while
// revalidating preview provenance against the current preview store.
func (v *DebugView) ExportWindow(ctx context.Context, window DebugWindow, maxBytes int, previews *PreviewStore) ([]byte, error) {
	if v == nil {
		return nil, errors.New("nil debug view")
	}
	if maxBytes == 0 {
		maxBytes = v.maxReportBytes
	}
	if maxBytes < 1024 {
		return nil, errors.New("debug report byte limit is too small")
	}
	if maxBytes > v.maxReportBytes {
		return nil, errors.New("debug report byte limit exceeds recorder maximum")
	}
	reportEvents := make([]DebugEvent, len(window.Events))
	for i := range window.Events {
		reportEvents[i] = cloneDebugEvent(window.Events[i])
	}
	report := DebugReport{Version: ReportSchemaVersion, GeneratedAt: v.generatedAt.Format("2006-01-02T15:04:05.000000000Z07:00"), Health: v.health, Sessions: v.Sessions(), Events: reportEvents}
	for _, event := range window.Events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if event.Visual == nil {
			continue
		}
		expected, eligible := v.previewEntries[event.Sequence]
		if !eligible || previews == nil {
			report.PreviewsOmitted++
			continue
		}
		current, ok := previews.Entry(event.Sequence, *event.Visual)
		if !ok || current != expected {
			report.PreviewsOmitted++
			continue
		}
		report.Previews = append(report.Previews, current)
	}
	for {
		data, err := json.Marshal(report)
		if err != nil {
			return nil, err
		}
		if len(data) <= maxBytes {
			return data, nil
		}
		if len(report.Events) == 0 {
			if report.Sessions != nil {
				report.Sessions = nil
				report.Truncated = true
				report.TruncationReasons = append(report.TruncationReasons, "session_index_omitted")
				continue
			}
			return nil, errors.New("debug report byte limit is too small for envelope")
		}
		removed := report.Events[len(report.Events)-1].Sequence
		report.Events = report.Events[:len(report.Events)-1]
		for i, entry := range report.Previews {
			if entry.Sequence == removed {
				report.Previews = append(report.Previews[:i], report.Previews[i+1:]...)
				break
			}
		}
		report.Truncated = true
		report.TruncationReasons = []string{"oldest_events_omitted"}
	}
}
