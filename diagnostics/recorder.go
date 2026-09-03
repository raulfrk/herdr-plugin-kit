package diagnostics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/raulfrk/herdr-plugin-kit/ui/snapshot"
)

type storedEvent struct {
	event Event
	data  []byte
}

// Recorder serializes reservations and storage updates so one process may
// safely accept events from concurrent plugins without overcommitting quota.
type Recorder struct {
	mu       sync.Mutex
	config   Config
	file     *os.File
	records  []storedEvent
	next     uint64
	bytes    int64
	health   Health
	reportAt time.Time
	closed   bool
}

func Open(config Config) (*Recorder, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if err := os.MkdirAll(config.Directory, 0o700); err != nil {
		return nil, fmt.Errorf("create diagnostics directory: %w", err)
	}
	directoryInfo, err := os.Lstat(config.Directory)
	if err != nil {
		return nil, fmt.Errorf("inspect diagnostics directory: %w", err)
	}
	if directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() {
		return nil, fmt.Errorf("diagnostics directory must be a real directory, not a symlink or special file")
	}
	if err := os.Chmod(config.Directory, 0o700); err != nil {
		return nil, fmt.Errorf("secure diagnostics directory: %w", err)
	}
	path := filepath.Join(config.Directory, EventLogName)
	if eventInfo, inspectErr := os.Lstat(path); inspectErr == nil {
		if eventInfo.Mode()&os.ModeSymlink != 0 || !eventInfo.Mode().IsRegular() {
			return nil, fmt.Errorf("diagnostics event log must be a regular file, not a symlink or special file")
		}
	} else if !os.IsNotExist(inspectErr) {
		return nil, fmt.Errorf("inspect diagnostics event log: %w", inspectErr)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open diagnostics event log: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, fmt.Errorf("secure diagnostics event log: %w", err)
	}
	now := config.Now().UTC()
	recorder := &Recorder{
		config: config, file: file,
		health:   Health{Writable: true, MaxEvents: config.MaxEvents, MaxBytes: config.MaxBytes},
		reportAt: now,
	}
	if err := recorder.load(now); err != nil {
		file.Close()
		return nil, err
	}
	return recorder, nil
}

func (r *Recorder) load(now time.Time) error {
	info, err := r.file.Stat()
	if err != nil {
		return fmt.Errorf("stat diagnostics event log: %w", err)
	}
	start := int64(0)
	readLimit := r.config.MaxBytes
	if info.Size() > r.config.MaxBytes {
		start = info.Size() - r.config.MaxBytes
		readLimit++
		r.health.Dropped++
		r.health.Pressure = true
	}
	seek := start
	if seek > 0 {
		seek--
	}
	if _, err := r.file.Seek(seek, 0); err != nil {
		return fmt.Errorf("seek diagnostics event log: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(r.file, readLimit))
	if err != nil {
		return fmt.Errorf("read diagnostics event log: %w", err)
	}
	corrupt := uint64(0)
	changed := start > 0
	if start > 0 {
		if len(data) > 0 && data[0] == '\n' {
			data = data[1:]
		} else if boundary := bytes.IndexByte(data, '\n'); boundary >= 0 {
			data = data[boundary+1:]
		} else {
			data = nil
			corrupt++
		}
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		changed = true
	}
	var previousSequence uint64
	var previousTime time.Time
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event Event
		if json.Unmarshal(line, &event) != nil || event.Version != EventSchemaVersion || event.Sequence <= previousSequence || event.Time.IsZero() || event.Time.Before(previousTime) || validateEvent(event) != nil {
			corrupt++
			continue
		}
		event = r.sanitize(event)
		canonical, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			corrupt++
			continue
		}
		encoded := append(canonical, '\n')
		if !bytes.Equal(encoded, append(append([]byte(nil), line...), '\n')) {
			changed = true
		}
		r.records = append(r.records, storedEvent{event: event, data: encoded})
		r.bytes += int64(len(encoded))
		r.next = event.Sequence
		previousSequence = event.Sequence
		previousTime = event.Time
		r.reportAt = event.Time
	}
	if corrupt > 0 {
		r.health.CorruptRecords = corrupt
		r.health.LastError = fmt.Sprintf("recovered %d corrupt or truncated prior record(s)", corrupt)
		r.health.Pressure = true
	}
	changed = corrupt > 0 || changed
	changed = r.applyRetention(now) || changed
	if changed {
		if err := r.rewrite(); err != nil {
			return fmt.Errorf("recover diagnostics event log: %w", err)
		}
	} else if _, err := r.file.Seek(0, 2); err != nil {
		return fmt.Errorf("seek diagnostics append position: %w", err)
	}
	r.updateUsage()
	return nil
}

func (r *Recorder) Record(input Event) (Event, error) {
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

	previousRecords := append([]storedEvent(nil), r.records...)
	previousBytes := r.bytes
	previousDropped := r.health.Dropped
	before := len(r.records)
	r.records = append(r.records, storedEvent{event: event, data: data})
	r.bytes += int64(len(data))
	changed := r.applyRetention(event.Time)
	if changed || len(r.records) != before+1 {
		err = r.rewrite()
	} else {
		_, err = r.file.Write(data)
		if err == nil {
			err = r.file.Sync()
		}
	}
	if err != nil {
		r.records = previousRecords
		r.bytes = previousBytes
		r.health.Dropped = previousDropped
		r.fail(err)
		r.updateUsage()
		return Event{}, fmt.Errorf("persist diagnostics event: %w", err)
	}
	r.next = event.Sequence
	r.reportAt = event.Time
	r.health.Writable = true
	r.updateUsage()
	return event, nil
}

func (r *Recorder) normalize(input Event) (Event, error) {
	if err := validateEvent(input); err != nil {
		return Event{}, err
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
		copy.Truncated = copy.Truncated || nameTruncated || textTruncated
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
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}

func (r *Recorder) applyRetention(now time.Time) bool {
	changed := false
	cutoff := now.Add(-r.config.MaxAge)
	for len(r.records) > 0 && r.records[0].event.Time.Before(cutoff) {
		r.dropOldest()
		changed = true
	}
	for len(r.records) > r.config.MaxEvents || r.bytes > r.config.MaxBytes {
		r.dropOldest()
		changed = true
	}
	if changed {
		r.health.Pressure = true
	}
	return changed
}

func (r *Recorder) dropOldest() {
	if len(r.records) == 0 {
		return
	}
	r.bytes -= int64(len(r.records[0].data))
	r.records[0] = storedEvent{}
	r.records = r.records[1:]
	r.health.Dropped++
}

func (r *Recorder) rewrite() error {
	temporary, err := os.CreateTemp(r.config.Directory, ".events-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	remove := true
	defer func() {
		temporary.Close()
		if remove {
			os.Remove(name)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	for _, record := range r.records {
		if _, err := temporary.Write(record.data); err != nil {
			return err
		}
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := r.file.Close(); err != nil {
		return err
	}
	path := filepath.Join(r.config.Directory, EventLogName)
	if err := os.Rename(name, path); err != nil {
		return err
	}
	remove = false
	r.file, err = os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	directory, err := os.Open(r.config.Directory)
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil {
		return err
	}
	return closeErr
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
	previousRecords := append([]storedEvent(nil), r.records...)
	previousBytes := r.bytes
	previousDropped := r.health.Dropped
	now := r.config.Now().UTC()
	if !r.applyRetention(now) {
		return nil
	}
	if err := r.rewrite(); err != nil {
		r.records = previousRecords
		r.bytes = previousBytes
		r.health.Dropped = previousDropped
		r.fail(err)
		r.updateUsage()
		return fmt.Errorf("clean diagnostics event log: %w", err)
	}
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
	if err := r.file.Sync(); err != nil {
		r.fail(err)
		_ = r.file.Close()
		return fmt.Errorf("sync diagnostics event log: %w", err)
	}
	if err := r.file.Close(); err != nil {
		r.fail(err)
		return fmt.Errorf("close diagnostics event log: %w", err)
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
