package diagnostics

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/ui/snapshot"
)

type storedEvent struct {
	event Event
	data  []byte
	debug *DebugEvent
}

// Recorder serializes reservations and storage updates so one process may
// safely accept events from concurrent plugins without overcommitting quota.
type Recorder struct {
	mu       sync.Mutex
	config   Config
	file     *os.File
	segments []segment
	storage  storageHooks
	records  []*storedEvent
	next     uint64
	bytes    int64
	health   Health
	reportAt time.Time
	closed   bool
}

func Open(config Config) (*Recorder, error) {
	return openRecorder(config, defaultStorageHooks())
}

func openRecorder(config Config, storage storageHooks) (*Recorder, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if err := os.MkdirAll(config.Directory, 0o700); err != nil {
		return nil, fmt.Errorf("create diagnostics directory: %w", err)
	}
	if err := privateStoragePath(config.Directory, true); err != nil {
		return nil, fmt.Errorf("secure diagnostics directory: %w", err)
	}
	r := &Recorder{
		config: config, storage: storage, reportAt: config.Now().UTC(),
		health: Health{Writable: true, MaxEvents: config.MaxEvents, MaxBytes: config.MaxBytes},
	}
	if err := r.openSegments(); err != nil {
		if r.file != nil {
			_ = r.file.Close()
			r.file = nil
		}
		return nil, fmt.Errorf("open diagnostics storage: %w", err)
	}
	r.updateUsage()
	return r, nil
}

func validStoredEvent(line []byte, event *Event, previousSequence uint64, previousTime time.Time) bool {
	if json.Unmarshal(line, event) != nil {
		return false
	}
	if event.Version != EventSchemaVersion {
		return false
	}
	if event.Sequence <= previousSequence {
		return false
	}
	if event.Time.IsZero() {
		return false
	}
	if event.Time.Before(previousTime) {
		return false
	}
	return validateEvent(*event) == nil
}

func (r *Recorder) record(input Event) (Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return Event{}, ErrClosed
	}
	if !r.health.Writable {
		return Event{}, ErrStorage
	}
	event, err := r.normalize(input)
	if err != nil {
		return Event{}, err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return Event{}, fmt.Errorf("encode diagnostics event: %w", err)
	}
	data = append(data, '\n')
	if int64(len(data)) > r.config.MaxBytes {
		r.health.Dropped++
		r.health.Pressure = true
		return Event{}, ErrEventTooLarge
	}

	projected, projectedOK := projectDebugEvent(event)
	var cached *DebugEvent
	if projectedOK {
		cached = &projected
	}
	pending := &storedEvent{event: event, data: data, debug: cached}
	staged := append(r.records, pending)
	drop, total := r.retentionDecision(staged, r.bytes+int64(len(data)), event.Time)
	err = r.appendSegment(data, drop > 0)
	if err == nil && drop != 0 {
		err = r.trimSegments(staged, drop)
	}
	if err != nil {
		// append may reuse capacity, but never changes the previously published
		// slice length or its immutable records. Do not claim disk rollback.
		staged[len(r.records)] = nil
		r.fail(err)
		r.updateUsage()
		return Event{}, fmt.Errorf("persist diagnostics event: %w", err)
	}
	clear(staged[:drop])
	r.records = staged[drop:]
	r.bytes = total
	r.health.Dropped += uint64(drop)
	r.health.Pressure = r.health.Pressure || drop > 0
	r.next = event.Sequence
	r.reportAt = event.Time
	r.updateUsage()
	return event, nil
}

func (r *Recorder) normalize(input Event) (Event, error) {
	if err := validateEvent(input); err != nil {
		return Event{}, err
	}
	if r.next == ^uint64(0) {
		return Event{}, errors.New("diagnostics sequence exhausted")
	}
	input.Version = EventSchemaVersion
	input.Sequence = r.next + 1
	input.Time = r.config.Now().UTC()
	if len(r.records) > 0 && input.Time.Before(r.records[len(r.records)-1].event.Time) {
		input.Time = r.records[len(r.records)-1].event.Time
	}
	input = r.sanitize(input)
	return input, nil
}

func (r *Recorder) sanitize(input Event) Event {
	input.Plugin = snapshot.Redact(input.Plugin)
	input.Component = snapshot.Redact(input.Component)
	input.Action = snapshot.Redact(input.Action)
	input.CorrelationID = snapshot.Redact(input.CorrelationID)
	input.Message = snapshot.Redact(input.Message)
	if input.Details != nil {
		redacted, _ := redactValue(input.Details).(map[string]any)
		input.Details = redacted
		encoded, _ := json.Marshal(input.Details)
		if len(encoded) > r.config.MaxDetailBytes {
			marker := map[string]any{"_truncated": true}
			markerBytes, _ := json.Marshal(marker)
			if len(markerBytes) <= r.config.MaxDetailBytes {
				input.Details = marker
			} else {
				input.Details = nil
			}
			input.DetailsTruncated = true
		}
	}
	if input.UISnapshot != nil {
		copy := *input.UISnapshot
		copy.Name = snapshot.Redact(copy.Name)
		copy.Text = snapshot.Redact(copy.Text)
		var nameTruncated, textTruncated bool
		copy.Name, nameTruncated = truncate(copy.Name, r.config.MaxSnapshotBytes, false)
		copy.Text, textTruncated = truncate(copy.Text, r.config.MaxSnapshotBytes-len(copy.Name), false)
		if nameTruncated {
			copy.Truncated = true
		}
		if textTruncated {
			copy.Truncated = true
		}
		input.UISnapshot = &copy
	}
	if input.Screenshot != nil {
		copy := *input.Screenshot
		copy.Name = snapshot.Redact(copy.Name)
		copy.Path = snapshot.Redact(copy.Path)
		copy.MediaType = snapshot.Redact(copy.MediaType)
		copy.SHA256 = snapshot.Redact(copy.SHA256)
		input.Screenshot = &copy
	}
	return input
}

func truncate(value string, limit int, already bool) (string, bool) {
	if len(value) <= limit {
		return value, already
	}
	return strings.ToValidUTF8(value[:limit], ""), true
}

func syncDirectory(path string) error {
	return defaultStorageHooks().syncDirectory(path)
}

func (r *Recorder) Cleanup() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if !r.health.Writable {
		return ErrStorage
	}
	now := r.config.Now().UTC()
	drop, total := r.retentionDecision(r.records, r.bytes, now)
	if drop == 0 {
		return nil
	}
	err := r.storage.sync(r.file)
	if err == nil {
		err = r.trimSegments(r.records, drop)
	}
	if err != nil {
		r.fail(err)
		r.updateUsage()
		return fmt.Errorf("clean diagnostics event log: %w", err)
	}
	clear(r.records[:drop])
	r.records = r.records[drop:]
	r.bytes = total
	r.health.Dropped += uint64(drop)
	r.health.Pressure = true
	r.updateUsage()
	r.reportAt = now
	return nil
}

func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.file == nil {
		return nil
	}
	syncErr := r.storage.sync(r.file)
	err := errors.Join(syncErr, r.storage.closeFile(&r.file))
	if err != nil {
		r.fail(err)
		return fmt.Errorf("close diagnostics storage: %w", err)
	}
	return nil
}

func (r *Recorder) Health() Health {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.health
}

func (r *Recorder) fail(err error) {
	r.health.Writable = false
	r.health.LastError = snapshot.Redact(err.Error())
	r.health.Dropped++
}

func (r *Recorder) updateUsage() {
	r.health.Events = len(r.records)
	r.health.Bytes = r.bytes
	r.health.UsageRatio = float64(r.bytes) / float64(r.config.MaxBytes)
	if len(r.records) == r.config.MaxEvents || r.bytes*5 >= r.config.MaxBytes*4 {
		r.health.Pressure = true
	}
}
