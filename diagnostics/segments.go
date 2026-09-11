package diagnostics

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const segmentDirectory = "events-v1"
const stagingDirectory = ".events-v1-stage"
const segmentTarget = int64(256 << 10)

// Historical physical-line budget, including the delimiter when present.
const historicalLineLimit = int64(1073741825)

type segment struct {
	ordinal uint64
	path    string
	count   int
	bytes   int64
	dirty   bool
}

var boundaryName = regexp.MustCompile(`^\.boundary-[0-9]+\.tmp$`)

func segmentPath(dir string, ordinal uint64) string {
	return filepath.Join(dir, fmt.Sprintf("events-%020d.jsonl", ordinal))
}

func segmentOrdinal(name string) (uint64, bool) {
	if len(name) != 33 || !strings.HasPrefix(name, "events-") || !strings.HasSuffix(name, ".jsonl") {
		return 0, false
	}
	n, err := strconv.ParseUint(name[7:27], 10, 64)
	return n, err == nil && n > 0 && filepath.Base(segmentPath("", n)) == name
}

func privateStoragePath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if directory {
		if !info.IsDir() {
			return errors.New("diagnostics directory must be a real directory")
		}
		return os.Chmod(path, 0o700)
	}
	if !info.Mode().IsRegular() {
		return errors.New("diagnostics data must be a regular file")
	}
	return os.Chmod(path, 0o600)
}

func storageExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// Validate the whole owned directory before any cleanup. Root-level files
// outside these namespaces are not ours and are never scanned or deleted.
func segmentEntries(dir string, allowBoundary bool) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		_, canonical := segmentOrdinal(entry.Name())
		if !canonical && !(allowBoundary && boundaryName.MatchString(entry.Name())) {
			return nil, fmt.Errorf("unexpected diagnostics filename %q", entry.Name())
		}
		if err := privateStoragePath(filepath.Join(dir, entry.Name()), false); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func (r *Recorder) retentionDecision(records []*storedEvent, total int64, now time.Time) (int, int64) {
	drop := 0
	cutoff := now.Add(-r.config.MaxAge)
	for drop < len(records) && (records[drop].event.Time.Before(cutoff) || len(records)-drop > r.config.MaxEvents || total > r.config.MaxBytes) {
		total -= int64(len(records[drop].data))
		drop++
	}
	return drop, total
}

func (r *Recorder) decodeStored(line []byte) (*storedEvent, bool) {
	var event Event
	var previous time.Time
	if len(r.records) != 0 {
		previous = r.records[len(r.records)-1].event.Time
	}
	if !validStoredEvent(line, &event, r.next, previous) {
		return nil, false
	}
	event = r.sanitize(event)
	data, _ := json.Marshal(event)
	stored := &storedEvent{event: event, data: append(data, '\n')}
	if projected, ok := projectDebugEvent(event); ok {
		stored.debug = &projected
	}
	return stored, true
}

func (r *Recorder) admitLoaded(record *storedEvent) {
	r.records = append(r.records, record)
	r.bytes += int64(len(record.data))
	r.next, r.reportAt = record.event.Sequence, record.event.Time
}

func (r *Recorder) reportCorruption() {
	if r.health.CorruptRecords != 0 {
		r.health.Pressure = true
		r.health.LastError = fmt.Sprintf("recovered %d corrupt or truncated prior record(s)", r.health.CorruptRecords)
	}
}

// Legacy logs retain their established final-MaxBytes window and preceding
// delimiter rule. An unknown omitted prefix counts as one dropped observation.
func (r *Recorder) readLegacy(path string) (result error) {
	if err := privateStoragePath(path, false); err != nil {
		return err
	}
	f, err := r.storage.openFile(path, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, r.storage.closeFile(&f)) }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	start := max(int64(0), info.Size()-r.config.MaxBytes)
	boundary := start == 0
	if start > 0 {
		r.health.Dropped++
		r.health.Pressure = true
		var previous [1]byte
		if _, err := f.ReadAt(previous[:], start-1); err != nil {
			return err
		}
		boundary = previous[0] == '\n'
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(f, r.config.MaxBytes))
	if err != nil {
		return err
	}
	if !boundary {
		if at := bytes.IndexByte(data, '\n'); at >= 0 {
			data = data[at+1:]
		} else {
			data = nil
			r.health.CorruptRecords++
		}
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if record, ok := r.decodeStored(line); ok {
			r.admitLoaded(record)
		} else {
			r.health.CorruptRecords++
		}
	}
	r.reportCorruption()
	return nil
}

// Historical segmented records are bounded by the supported storage maximum,
// not a newly lowered MaxBytes. Retention, rather than corruption recovery,
// removes an otherwise valid old record that no longer fits the current quota.
func (r *Recorder) readSegment(s *segment) (result error) {
	f, err := r.storage.openFile(s.path, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, r.storage.closeFile(&f)) }()
	reader := bufio.NewReaderSize(f, 64<<10)
	return r.readSegmentRecords(reader, s, historicalLineLimit)
}

func (r *Recorder) readSegmentRecords(reader *bufio.Reader, s *segment, maxLineBytes int64) error {
	for {
		var line []byte
		var readErr error
		oversized := false
		for {
			part, err := reader.ReadSlice('\n')
			readErr = err
			if !oversized {
				if int64(len(line)+len(part)) > maxLineBytes {
					oversized, line = true, nil
				} else {
					line = append(line, part...)
				}
			}
			if err != bufio.ErrBufferFull {
				break
			}
		}
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		if oversized {
			r.health.CorruptRecords++
			s.dirty = true
		} else if len(line) != 0 {
			raw := bytes.TrimSuffix(line, []byte{'\n'})
			if len(bytes.TrimSpace(raw)) == 0 {
				s.dirty = true
			} else if record, ok := r.decodeStored(raw); ok {
				s.dirty = s.dirty || line[len(line)-1] != '\n' || !bytes.Equal(line, record.data)
				s.count++
				r.admitLoaded(record)
			} else {
				r.health.CorruptRecords++
				s.dirty = true
			}
		}
		if readErr == io.EOF {
			return nil
		}
	}
}

func identicalSuffix(live, legacy []*storedEvent) bool {
	if len(live) > len(legacy) {
		return false
	}
	for i, record := range live {
		if !bytes.Equal(record.data, legacy[len(legacy)-len(live)+i].data) {
			return false
		}
	}
	return true
}

func (r *Recorder) discardStage(path string) error {
	if err := privateStoragePath(path, true); err != nil {
		return err
	}
	entries, err := segmentEntries(path, false)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := r.storage.remove(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	if err := r.storage.remove(path); err != nil {
		return err
	}
	return r.storage.syncDirectory(r.config.Directory)
}

func (r *Recorder) openSegments() error {
	root := r.config.Directory
	live, legacy, stage := filepath.Join(root, segmentDirectory), filepath.Join(root, EventLogName), filepath.Join(root, stagingDirectory)
	hasLive, err := storageExists(live)
	if err != nil {
		return err
	}
	hasLegacy, err := storageExists(legacy)
	if err != nil {
		return err
	}
	hasStage, err := storageExists(stage)
	if err != nil {
		return err
	}
	now := r.reportAt
	if !hasLive {
		if hasStage {
			if err := r.discardStage(stage); err != nil {
				return err
			}
		}
		if hasLegacy {
			if err := r.readLegacy(legacy); err != nil {
				return err
			}
		}
		drop, total := r.retentionDecision(r.records, r.bytes, now)
		clear(r.records[:drop])
		r.records, r.bytes = r.records[drop:], total
		r.health.Dropped += uint64(drop)
		r.health.Pressure = r.health.Pressure || drop > 0
		if err := r.publishSegments(stage, live); err != nil {
			return err
		}
		if hasLegacy {
			if err := r.removeLegacy(legacy); err != nil {
				return err
			}
		}
	} else {
		if hasStage {
			return errors.New("live and staging diagnostics directories conflict")
		}
		if err := privateStoragePath(live, true); err != nil {
			return err
		}
		entries, err := segmentEntries(live, true)
		if err != nil {
			return err
		}
		var temporaries []string
		for _, entry := range entries {
			path := filepath.Join(live, entry.Name())
			ordinal, canonical := segmentOrdinal(entry.Name())
			if !canonical {
				temporaries = append(temporaries, path)
				continue
			}
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			s := segment{ordinal: ordinal, path: path, bytes: info.Size()}
			if err := r.readSegment(&s); err != nil {
				return err
			}
			r.segments = append(r.segments, s)
		}
		if len(r.segments) == 0 {
			return errors.New("live diagnostics directory has no segments")
		}
		drop, total := r.retentionDecision(r.records, r.bytes, now)
		if hasLegacy {
			if r.health.CorruptRecords != 0 {
				return errors.New("damaged live diagnostics conflict with legacy source")
			}
			prior := &Recorder{config: r.config, storage: r.storage}
			if err := prior.readLegacy(legacy); err != nil {
				return err
			}
			priorDrop, _ := prior.retentionDecision(prior.records, prior.bytes, now)
			if !identicalSuffix(r.records[drop:], prior.records[priorDrop:]) {
				return errors.New("legacy and live diagnostics histories conflict")
			}
		}
		// Establish survivor data AND namespace durability before any recovery
		// replacement, temp cleanup, legacy unlink, or expired-file deletion.
		for _, s := range r.segments {
			f, err := r.storage.openFile(s.path, os.O_RDWR)
			if err != nil {
				return err
			}
			syncErr := r.storage.sync(f)
			if err := errors.Join(syncErr, r.storage.closeFile(&f)); err != nil {
				return err
			}
		}
		if err := r.storage.syncDirectory(live); err != nil {
			return err
		}
		// Also finish an earlier Open that unlinked legacy but failed parent sync.
		if err := r.storage.syncDirectory(root); err != nil {
			return err
		}
		if hasLegacy {
			if err := r.removeLegacy(legacy); err != nil {
				return err
			}
		}
		for _, path := range temporaries {
			if err := r.storage.remove(path); err != nil {
				return err
			}
		}
		if len(temporaries) != 0 {
			if err := r.storage.syncDirectory(live); err != nil {
				return err
			}
		}
		r.file, err = r.storage.openFile(r.segments[len(r.segments)-1].path, os.O_RDWR|os.O_APPEND)
		if err != nil {
			return err
		}
		if err := r.trimSegments(r.records, drop); err != nil {
			return err
		}
		clear(r.records[:drop])
		r.records, r.bytes = r.records[drop:], total
		r.health.Dropped += uint64(drop)
		r.health.Pressure = r.health.Pressure || drop > 0
		r.reportCorruption()
	}
	if r.file == nil {
		r.file, err = r.storage.openFile(r.segments[len(r.segments)-1].path, os.O_RDWR|os.O_APPEND)
	}
	return err
}

func (r *Recorder) removeLegacy(path string) error {
	if err := r.storage.remove(path); err != nil {
		return err
	}
	return r.storage.syncDirectory(r.config.Directory)
}

func (r *Recorder) publishSegments(stage, live string) error {
	if err := r.storage.mkdir(stage, 0o700); err != nil {
		return err
	}
	var f *os.File
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	// Even an empty store has an active segment. Each segment is complete and
	// synced before the staging directory is published as one namespace.
	for i := range max(1, len(r.records)) {
		var data []byte
		if i < len(r.records) {
			data = r.records[i].data
		}
		if f == nil || r.segments[len(r.segments)-1].bytes+int64(len(data)) > min(segmentTarget, r.config.MaxBytes) {
			if f != nil {
				if err := r.storage.sync(f); err != nil {
					return err
				}
				if err := r.storage.closeFile(&f); err != nil {
					return err
				}
			}
			ordinal := uint64(len(r.segments) + 1)
			path := segmentPath(stage, ordinal)
			var err error
			f, err = r.storage.openFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|os.O_APPEND)
			if err != nil {
				return err
			}
			r.segments = append(r.segments, segment{ordinal: ordinal, path: path})
		}
		if len(data) != 0 {
			if err := r.storage.writeAll(f, data); err != nil {
				return err
			}
			s := &r.segments[len(r.segments)-1]
			s.count++
			s.bytes += int64(len(data))
		}
	}
	if err := r.storage.sync(f); err != nil {
		return err
	}
	if err := r.storage.closeFile(&f); err != nil {
		return err
	}
	if err := r.storage.syncDirectory(stage); err != nil {
		return err
	}
	if err := r.storage.rename(stage, live); err != nil {
		return err
	}
	if err := r.storage.syncDirectory(r.config.Directory); err != nil {
		return err
	}
	for i := range r.segments {
		r.segments[i].path = segmentPath(live, r.segments[i].ordinal)
	}
	return nil
}

func (r *Recorder) appendSegment(data []byte, retaining bool) error {
	live := filepath.Join(r.config.Directory, segmentDirectory)
	active := &r.segments[len(r.segments)-1]
	rotating := active.bytes > 0 && active.bytes+int64(len(data)) > min(segmentTarget, r.config.MaxBytes)
	if rotating {
		if active.ordinal == ^uint64(0) {
			return errors.New("diagnostics segment ordinal exhausted")
		}
		if err := r.storage.sync(r.file); err != nil {
			return err
		}
		if err := r.storage.closeFile(&r.file); err != nil {
			return err
		}
		next := segment{ordinal: active.ordinal + 1, path: segmentPath(live, active.ordinal+1)}
		f, err := r.storage.openFile(next.path, os.O_CREATE|os.O_EXCL|os.O_RDWR|os.O_APPEND)
		if err != nil {
			return err
		}
		r.file = f
		r.segments = append(r.segments, next)
		active = &r.segments[len(r.segments)-1]
	}
	if err := r.storage.writeAll(r.file, data); err != nil {
		return err
	}
	active.count++
	active.bytes += int64(len(data))
	if retaining || rotating {
		if err := r.storage.sync(r.file); err != nil {
			return err
		}
	}
	if rotating {
		return r.storage.syncDirectory(live)
	}
	return nil
}

func (r *Recorder) replaceSegment(s *segment, records []*storedEvent) error {
	live := filepath.Join(r.config.Directory, segmentDirectory)
	f, err := r.storage.temporary(live)
	if err != nil {
		return err
	}
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	name := f.Name()
	var size int64
	for _, record := range records {
		if err := r.storage.writeAll(f, record.data); err != nil {
			return err
		}
		size += int64(len(record.data))
	}
	if err := r.storage.sync(f); err != nil {
		return err
	}
	if err := r.storage.closeFile(&f); err != nil {
		return err
	}
	if err := r.storage.rename(name, s.path); err != nil {
		return err
	}
	if err := r.storage.syncDirectory(live); err != nil {
		return err
	}
	if s.ordinal == r.segments[len(r.segments)-1].ordinal {
		if err := r.storage.closeFile(&r.file); err != nil {
			return err
		}
		r.file, err = r.storage.openFile(s.path, os.O_RDWR|os.O_APPEND)
		if err != nil {
			return err
		}
	}
	s.count, s.bytes, s.dirty = len(records), size, false
	return nil
}

func (r *Recorder) trimSegments(staged []*storedEvent, drop int) error {
	// Publish all surviving suffixes first. Only then may whole expired files
	// be unlinked; the last segment remains as the empty active file if needed.
	offset, expired := 0, 0
	for i := range r.segments {
		s := &r.segments[i]
		end := offset + s.count
		if end <= drop && i != len(r.segments)-1 {
			expired = i + 1
			offset = end
			continue
		}
		start := min(max(offset, drop), end)
		if s.dirty || start > offset || (s.count == 0 && s.bytes > 0) {
			if err := r.replaceSegment(s, staged[start:end]); err != nil {
				return err
			}
		}
		offset = end
	}
	for _, s := range r.segments[:expired] {
		if err := r.storage.remove(s.path); err != nil {
			return err
		}
	}
	if expired > 0 {
		if err := r.storage.syncDirectory(filepath.Join(r.config.Directory, segmentDirectory)); err != nil {
			return err
		}
		clear(r.segments[:expired])
		r.segments = r.segments[expired:]
	}
	return nil
}
