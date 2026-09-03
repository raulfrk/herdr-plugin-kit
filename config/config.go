// Package config provides strict typed TOML loading and live reload.
package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Loader decodes a caller-owned configuration type over useful defaults.
type Loader[T any] struct {
	Defaults func() T
	Validate func(T) error
}

func (l Loader[T]) Decode(data []byte) (T, error) {
	var value T
	if l.Defaults != nil {
		value = l.Defaults()
	}
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			keys := make([]string, 0, len(strict.Errors))
			for i := range strict.Errors {
				keys = append(keys, strings.Join(strict.Errors[i].Key(), "."))
			}
			return value, fmt.Errorf("decode TOML: unknown fields: %s", strings.Join(keys, ", "))
		}
		return value, fmt.Errorf("decode TOML: %w", err)
	}
	if l.Validate != nil {
		if err := l.Validate(value); err != nil {
			return value, fmt.Errorf("validate config: %w", err)
		}
	}
	return value, nil
}

func (l Loader[T]) Load(path string) (T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("read config: %w", err)
	}
	return l.Decode(data)
}

type Update[T any] struct {
	Value      T
	Err        error
	DetectedAt time.Time
	AppliedAt  time.Time
}

type WatchOptions struct {
	PollInterval time.Duration
	Debounce     time.Duration
}

// Watcher polls content rather than metadata, so atomic same-size replacements
// are detected even when timestamps are preserved.
type Watcher struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// Stop requests shutdown and is safe to call concurrently or from a callback.
// Callbacks run independently and may overlap. Done closes after polling stops
// and no more callbacks can be dispatched; it does not wait for callbacks that
// have already started.
func (w *Watcher) Stop()                 { w.once.Do(w.cancel) }
func (w *Watcher) Done() <-chan struct{} { return w.done }

func (l Loader[T]) Watch(ctx context.Context, path string, options WatchOptions, callback func(Update[T])) (*Watcher, error) {
	if callback == nil {
		return nil, errors.New("config callback is nil")
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 100 * time.Millisecond
	}
	if options.Debounce < 0 {
		return nil, errors.New("config debounce is negative")
	}
	if options.Debounce == 0 {
		options.Debounce = 100 * time.Millisecond
	}
	initial, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read initial config: %w", err)
	}
	if _, err := l.Decode(initial); err != nil {
		return nil, fmt.Errorf("initial config: %w", err)
	}
	child, cancel := context.WithCancel(ctx)
	w := &Watcher{cancel: cancel, done: make(chan struct{})}
	go l.watch(child, path, options, sha256.Sum256(initial), callback, w.done)
	return w, nil
}

func (l Loader[T]) watch(ctx context.Context, path string, options WatchOptions, delivered [sha256.Size]byte, callback func(Update[T]), done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(options.PollInterval)
	defer ticker.Stop()
	var pending [sha256.Size]byte
	var pendingData []byte
	var pendingReadErr error
	var detected time.Time
	hasPending := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			data, readErr := os.ReadFile(path)
			digest := sha256.Sum256(data)
			if readErr != nil {
				digest = sha256.Sum256([]byte("read-error:" + readErr.Error()))
			}
			if digest == delivered {
				hasPending = false
				continue
			}
			if !hasPending || digest != pending {
				pending, pendingData, pendingReadErr, detected, hasPending = digest, append([]byte(nil), data...), readErr, time.Now(), true
				continue
			}
			if time.Since(detected) < options.Debounce {
				continue
			}
			var value T
			loadErr := pendingReadErr
			if loadErr == nil {
				value, loadErr = l.Decode(pendingData)
			}
			if loadErr != nil {
				loadErr = fmt.Errorf("reload config: %w", loadErr)
			}
			update := Update[T]{Value: value, Err: loadErr, DetectedAt: detected, AppliedAt: time.Now()}
			delivered, hasPending = pending, false
			select {
			case <-ctx.Done():
				return
			default:
				dispatch(callback, update)
			}
		}
	}
}

func dispatch[T any](callback func(Update[T]), update Update[T]) {
	started := make(chan struct{})
	go func() {
		close(started)
		callback(update)
	}()
	<-started
}
