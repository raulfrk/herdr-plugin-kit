package diagnostics

import (
	"encoding/json"
	"errors"
	"fmt"
)

type ReportOptions struct {
	MaxBytes         int
	ConfigMetadata   map[string]any
	ManifestMetadata map[string]any
	EmbedSnapshots   bool
}

type Report struct {
	Version           int            `json:"version"`
	GeneratedAt       string         `json:"generated_at"`
	ConfigMetadata    map[string]any `json:"config_metadata,omitempty"`
	ManifestMetadata  map[string]any `json:"manifest_metadata,omitempty"`
	Health            Health         `json:"health"`
	Events            []Event        `json:"events"`
	Truncated         bool           `json:"truncated"`
	TruncationReasons []string       `json:"truncation_reasons,omitempty"`
}

// Export returns deterministic JSON for the recorder's current state. If the
// report is too large, oldest events, embedded snapshots, then metadata are
// removed in that order and the omissions are named in TruncationReasons.
func (r *Recorder) Export(options ReportOptions) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	limit := options.MaxBytes
	if limit == 0 {
		limit = r.config.MaxReportBytes
	}
	if limit < 1024 || limit > r.config.MaxReportBytes {
		return nil, fmt.Errorf("report max bytes must be between 1024 and %d", r.config.MaxReportBytes)
	}
	configMetadata, err := redactMetadata(options.ConfigMetadata)
	if err != nil {
		return nil, fmt.Errorf("config metadata: %w", err)
	}
	manifestMetadata, err := redactMetadata(options.ManifestMetadata)
	if err != nil {
		return nil, fmt.Errorf("manifest metadata: %w", err)
	}
	report := Report{
		Version: ReportSchemaVersion, GeneratedAt: r.reportAt.Format("2006-01-02T15:04:05.000000000Z07:00"),
		ConfigMetadata: configMetadata, ManifestMetadata: manifestMetadata,
		Health: r.health, Events: make([]Event, len(r.records)),
	}
	for i := range r.records {
		report.Events[i] = r.records[i].event
		if !options.EmbedSnapshots {
			report.Events[i].UISnapshot = nil
		}
	}
	encoded, err := fitReport(&report, limit)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func redactMetadata(metadata map[string]any) (map[string]any, error) {
	if metadata == nil {
		return nil, nil
	}
	if err := validateJSONValue(metadata); err != nil {
		return nil, err
	}
	redacted, _ := redactValue(metadata).(map[string]any)
	return redacted, nil
}

func fitReport(report *Report, limit int) ([]byte, error) {
	encode := func() ([]byte, error) { return json.Marshal(report) }
	data, err := encode()
	if err != nil {
		return nil, fmt.Errorf("encode diagnostics report: %w", err)
	}
	if len(data) <= limit {
		return data, nil
	}
	if len(report.Events) > 1 {
		addReason(report, "oldest_events_omitted")
		for range len(report.Events) - 1 {
			report.Events[0] = Event{}
			report.Events = report.Events[1:]
			data, _ = encode()
			if len(data) <= limit {
				return data, nil
			}
		}
	}
	hadSnapshots := false
	for i := range report.Events {
		if report.Events[i].UISnapshot != nil {
			hadSnapshots = true
			report.Events[i].UISnapshot = nil
		}
	}
	if hadSnapshots {
		addReason(report, "embedded_snapshots_omitted")
		data, _ = encode()
		if len(data) <= limit {
			return data, nil
		}
	}
	if report.ConfigMetadata != nil || report.ManifestMetadata != nil {
		addReason(report, "metadata_omitted")
		report.ConfigMetadata = nil
		report.ManifestMetadata = nil
		data, _ = encode()
		if len(data) <= limit {
			return data, nil
		}
	}
	if len(data) > limit && len(report.Events) != 0 {
		addReason(report, "oversized_last_event_omitted")
		report.Events = nil
		data, _ = encode()
	}
	if len(data) > limit {
		return nil, errors.New("report byte limit is too small for the bounded report envelope")
	}
	return data, nil
}

func addReason(report *Report, reason string) {
	report.Truncated = true
	report.TruncationReasons = append(report.TruncationReasons, reason)
}
