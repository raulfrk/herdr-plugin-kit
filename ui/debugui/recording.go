package debugui

import (
	"errors"
	"sync"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
)

const recordingQueueCapacity = 128

// RecordingStatus contains bounded counters for the debugger diagnostics FIFO.
type RecordingStatus struct {
	Pending, Persisted, OverflowRejected, PersistenceFailed uint64
	Closed                                                  bool
}

type queuedSemantic struct{ event diagnostics.SemanticEvent }

// Recording decouples debugger self-diagnostics from durable storage. Events
// from other surfaces retain the caller's synchronous fallback semantics.
type Recording struct {
	previews *diagnostics.PreviewStore
	fallback diagnostics.SemanticSink
	queue    chan queuedSemantic
	done     chan struct{}
	mu       sync.Mutex
	status   RecordingStatus
	write    func(diagnostics.SemanticEvent) (uint64, error)
}

// NewRecording starts one debugger diagnostics worker. Non-debugger events are
// sent synchronously to fallback, or recorder when fallback is nil.
func NewRecording(recorder *diagnostics.Recorder, previews *diagnostics.PreviewStore, fallback diagnostics.SemanticSink) (*Recording, error) {
	return newRecording(recorder, previews, fallback, nil)
}

func newRecording(recorder *diagnostics.Recorder, previews *diagnostics.PreviewStore, fallback diagnostics.SemanticSink, write func(diagnostics.SemanticEvent) (uint64, error)) (*Recording, error) {
	if recorder == nil {
		return nil, errors.New("debug recorder is nil")
	}
	if fallback == nil {
		fallback = recorder
	}
	r := &Recording{previews: previews, fallback: fallback, queue: make(chan queuedSemantic, recordingQueueCapacity), done: make(chan struct{})}
	if write == nil {
		write = recorder.RecordSemanticWithSequence
	}
	r.write = write
	go r.run()
	return r, nil
}

func (r *Recording) RecordSemantic(event diagnostics.SemanticEvent) error {
	if !debuggerSemantic(event) {
		return r.fallback.RecordSemantic(event)
	}
	event = copySemantic(event)
	r.mu.Lock()
	if r.status.Closed {
		r.mu.Unlock()
		return diagnostics.ErrClosed
	}
	select {
	case r.queue <- queuedSemantic{event: event}:
		r.status.Pending++
	default:
		r.status.OverflowRejected++
	}
	r.mu.Unlock()
	return nil
}

func (r *Recording) run() {
	defer close(r.done)
	for item := range r.queue {
		sequence, err := r.write(item.event)
		if err == nil && item.event.Visual != nil && r.previews != nil {
			_, _ = r.previews.Put(sequence, *item.event.Visual)
		}
		r.mu.Lock()
		r.status.Pending--
		if err != nil {
			r.status.PersistenceFailed++
		} else {
			r.status.Persisted++
		}
		r.mu.Unlock()
	}
}

// Status returns a consistent snapshot of bounded recording counters.
func (r *Recording) Status() RecordingStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// Close stops admission, drains accepted debugger events, and reports whether
// any debugger event was rejected or failed persistence.
func (r *Recording) Close() error {
	r.mu.Lock()
	if !r.status.Closed {
		r.status.Closed = true
		close(r.queue)
	}
	r.mu.Unlock()
	<-r.done
	status := r.Status()
	if status.OverflowRejected > 0 || status.PersistenceFailed > 0 {
		return errors.New("debug recording completed with rejected or failed events")
	}
	return nil
}

func debuggerSemantic(event diagnostics.SemanticEvent) bool {
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

func copySemantic(event diagnostics.SemanticEvent) diagnostics.SemanticEvent {
	if event.Visual != nil {
		visual := *event.Visual
		event.Visual = &visual
	}
	return event
}
