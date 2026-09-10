// Package diagnostics provides a private, bounded local event recorder and
// deterministic debug-report export for plugin-kit hosts.
package diagnostics

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"time"
)

const (
	EventSchemaVersion  = 1
	ReportSchemaVersion = 1
	EventLogName        = "events.jsonl" // Legacy import filename; use Export for full history.
)

var (
	ErrClosed        = errors.New("diagnostics recorder is closed")
	ErrEventTooLarge = errors.New("diagnostics event exceeds byte retention budget")
	ErrStorage       = errors.New("diagnostics storage is unavailable")
)

type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

type Kind string

const (
	KindLifecycle   Kind = "lifecycle"
	KindInteraction Kind = "interaction"
	KindDiagnostic  Kind = "diagnostic"
)

// Event is the stable persisted and report schema. Callers record through
// SemanticSink; Event remains public so reports and legacy logs can be decoded.
type Event struct {
	Version          int            `json:"version"`
	Sequence         uint64         `json:"sequence"`
	Time             time.Time      `json:"time"`
	Level            Level          `json:"level"`
	Kind             Kind           `json:"kind"`
	Plugin           string         `json:"plugin,omitempty"`
	Component        string         `json:"component,omitempty"`
	Action           string         `json:"action,omitempty"`
	CorrelationID    string         `json:"correlation_id,omitempty"`
	Message          string         `json:"message"`
	Details          map[string]any `json:"details,omitempty"`
	DetailsTruncated bool           `json:"details_truncated,omitempty"`
	UISnapshot       *UISnapshot    `json:"ui_snapshot,omitempty"`
	Screenshot       *ScreenshotRef `json:"screenshot,omitempty"`
}

type UISnapshot struct {
	Name      string `json:"name,omitempty"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ScreenshotRef refers to a caller-managed screenshot without copying image
// bytes into the diagnostics store.
type ScreenshotRef struct {
	Name      string `json:"name,omitempty"`
	Path      string `json:"path,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

type Config struct {
	Directory        string           `json:"directory"`
	MaxEvents        int              `json:"max_events"`
	MaxBytes         int64            `json:"max_bytes"`
	MaxAge           time.Duration    `json:"max_age"`
	MaxDetailBytes   int              `json:"max_detail_bytes"`
	MaxSnapshotBytes int              `json:"max_snapshot_bytes"`
	MaxReportBytes   int              `json:"max_report_bytes"`
	Now              func() time.Time `json:"-"`
}

func DefaultConfig(directory string) Config {
	return Config{
		Directory: directory, MaxEvents: 100_000, MaxBytes: 128 << 20,
		MaxAge: 14 * 24 * time.Hour, MaxDetailBytes: 1 << 20,
		MaxSnapshotBytes: 2 << 20, MaxReportBytes: 32 << 20,
	}
}

func (config Config) Validate() error {
	if config.Directory == "" {
		return errors.New("diagnostics directory is empty")
	}
	if config.MaxEvents <= 0 {
		return errors.New("max events must be positive")
	}
	if config.MaxBytes < 1024 {
		return errors.New("max bytes must be at least 1024")
	}
	if config.MaxBytes > 1<<30 {
		return errors.New("max bytes must not exceed 1 GiB")
	}
	if config.MaxAge <= 0 {
		return errors.New("max age must be positive")
	}
	if config.MaxDetailBytes <= 0 {
		return errors.New("max detail bytes must be positive")
	}
	if config.MaxSnapshotBytes <= 0 {
		return errors.New("max snapshot bytes must be positive")
	}
	if config.MaxReportBytes < 1024 {
		return errors.New("max report bytes must be at least 1024")
	}
	return nil
}

func validateEvent(event Event) error {
	switch event.Level {
	case LevelDebug, LevelInfo, LevelWarn, LevelError:
	default:
		return fmt.Errorf("unsupported diagnostics level %q", event.Level)
	}
	switch event.Kind {
	case KindLifecycle, KindInteraction, KindDiagnostic:
	default:
		return fmt.Errorf("unsupported diagnostics kind %q", event.Kind)
	}
	if event.Message == "" {
		return errors.New("diagnostics message is empty")
	}
	if err := validateJSONValue(event.Details); err != nil {
		return fmt.Errorf("diagnostics details: %w", err)
	}
	return nil
}

type containerIdentity struct {
	kind reflect.Kind
	ptr  uintptr
}

type jsonValidation struct {
	nodes  int
	active map[containerIdentity]struct{}
}

func validateJSONValue(value any) error {
	validation := jsonValidation{active: make(map[containerIdentity]struct{})}
	return validation.walk(value, "$", 0)
}

func (validation *jsonValidation) walk(value any, path string, depth int) error {
	validation.nodes++
	if depth > 16 {
		return fmt.Errorf("maximum depth 16 exceeded at %s", path)
	}
	if validation.nodes > 4096 {
		return fmt.Errorf("maximum node count 4096 exceeded at %s", path)
	}
	switch value := value.(type) {
	case nil, bool, string,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return nil
	case float32:
		if math.IsInf(float64(value), 0) || math.IsNaN(float64(value)) {
			return errors.New("diagnostics details contain non-finite number")
		}
		return nil
	case float64:
		if math.IsInf(value, 0) || math.IsNaN(value) {
			return errors.New("diagnostics details contain non-finite number")
		}
		return nil
	case []any:
		identity := containerIdentity{kind: reflect.Slice, ptr: reflect.ValueOf(value).Pointer()}
		if err := validation.enter(identity, path); err != nil {
			return err
		}
		defer delete(validation.active, identity)
		for index, item := range value {
			if err := validation.walk(item, fmt.Sprintf("%s[%d]", path, index), depth+1); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		identity := containerIdentity{kind: reflect.Map, ptr: reflect.ValueOf(value).Pointer()}
		if err := validation.enter(identity, path); err != nil {
			return err
		}
		defer delete(validation.active, identity)
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := validation.walk(value[key], path+"."+boundedPathKey(key), depth+1); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("diagnostics details contain unsupported %T", value)
	}
}

func (validation *jsonValidation) enter(identity containerIdentity, path string) error {
	if _, exists := validation.active[identity]; exists {
		return fmt.Errorf("container cycle detected at %s", path)
	}
	validation.active[identity] = struct{}{}
	return nil
}

func boundedPathKey(key string) string {
	const limit = 64
	if len(key) > limit {
		key = key[:limit] + "..."
	}
	return fmt.Sprintf("%q", key)
}

type Health struct {
	Writable       bool    `json:"writable"`
	LastError      string  `json:"last_error,omitempty"`
	Pressure       bool    `json:"pressure"`
	Dropped        uint64  `json:"dropped"`
	CorruptRecords uint64  `json:"corrupt_records"`
	Events         int     `json:"events"`
	Bytes          int64   `json:"bytes"`
	MaxEvents      int     `json:"max_events"`
	MaxBytes       int64   `json:"max_bytes"`
	UsageRatio     float64 `json:"usage_ratio"`
}
