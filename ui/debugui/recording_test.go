package debugui

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
)

func awaitRecordingCallback(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("recording callback did not start")
	}
}

type collectingSemanticSink struct {
	mu     sync.Mutex
	events []diagnostics.SemanticEvent
	err    error
}

func (sink *collectingSemanticSink) RecordSemantic(event diagnostics.SemanticEvent) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.events = append(sink.events, event)
	return sink.err
}

func recordingEvent(t testing.TB, code, screen string) diagnostics.SemanticEvent {
	t.Helper()
	return diagnostics.SemanticEvent{
		Level:   diagnostics.LevelInfo,
		Kind:    diagnostics.KindDiagnostic,
		Plugin:  uiID(t, "plugin"),
		Code:    uiID(t, code),
		Outcome: diagnostics.OutcomeApplied,
		Visual:  &diagnostics.VisualState{Screen: uiID(t, screen), State: uiID(t, "ready")},
	}
}

func TestRecordingRejectsNewestWithoutBlockingAndDrainsFIFO(t *testing.T) {
	recorder := uiRecorder(t)
	fallback := &collectingSemanticSink{}
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var codes []string
	sequence := uint64(0)
	recording, err := newRecording(recorder, nil, fallback, func(event diagnostics.SemanticEvent) (uint64, error) {
		if event.Code.String() == "queued-000" {
			close(entered)
			<-release
		}
		mu.Lock()
		defer mu.Unlock()
		codes = append(codes, event.Code.String())
		sequence++
		return sequence, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		_ = recording.Close()
	})
	if err := recording.RecordSemantic(recordingEvent(t, "queued-000", "debug.timeline")); err != nil {
		t.Fatal(err)
	}
	awaitRecordingCallback(t, entered)
	for index := 1; index <= recordingQueueCapacity; index++ {
		if err := recording.RecordSemantic(recordingEvent(t, "queued-"+threeDigits(index), "debug.timeline")); err != nil {
			t.Fatal(err)
		}
	}
	if err := recording.RecordSemantic(recordingEvent(t, "rejected", "debug.timeline")); err != nil {
		t.Fatalf("overflow must be reported in status, not returned to the shell: %v", err)
	}
	ordinary := recordingEvent(t, "ordinary", "plugin.main")
	if err := recording.RecordSemantic(ordinary); err != nil {
		t.Fatalf("overflow disabled the synchronous non-debugger path: %v", err)
	}
	status := recording.Status()
	if status.Pending != recordingQueueCapacity+1 || status.OverflowRejected != 1 {
		t.Fatalf("full queue status = %+v", status)
	}
	close(release)
	released = true
	if err := recording.Close(); err == nil {
		t.Fatal("close did not report the rejected debugger event")
	}
	status = recording.Status()
	if !status.Closed || status.Pending != 0 || status.Persisted != recordingQueueCapacity+1 || status.OverflowRejected != 1 || status.PersistenceFailed != 0 {
		t.Fatalf("drained queue status = %+v", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(codes) != recordingQueueCapacity+1 {
		t.Fatalf("persisted %d debugger events", len(codes))
	}
	for index, code := range codes {
		if want := "queued-" + threeDigits(index); code != want {
			t.Fatalf("FIFO[%d] = %q, want %q", index, code, want)
		}
	}
	fallback.mu.Lock()
	defer fallback.mu.Unlock()
	if len(fallback.events) != 1 || fallback.events[0].Code != ordinary.Code {
		t.Fatalf("synchronous fallback events = %+v", fallback.events)
	}
}

func TestRecordingCopiesSemanticValuesBeforeAdmissionAndReportsWriteFailure(t *testing.T) {
	recorder := uiRecorder(t)
	previews, err := diagnostics.OpenPreviewStore(t.TempDir(), diagnostics.DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = previews.Close() })
	entered := make(chan struct{})
	release := make(chan struct{})
	var got diagnostics.SemanticEvent
	recording, err := newRecording(recorder, previews, nil, func(event diagnostics.SemanticEvent) (uint64, error) {
		close(entered)
		<-release
		got = event
		return 41, errors.New("disk failed")
	})
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		_ = recording.Close()
	})
	event := recordingEvent(t, "copy", "debug.detail")
	wantScreen := event.Visual.Screen
	if err := recording.RecordSemantic(event); err != nil {
		t.Fatal(err)
	}
	awaitRecordingCallback(t, entered)
	event.Visual.Screen = uiID(t, "mutated.after.admission")
	close(release)
	released = true
	if err := recording.Close(); err == nil {
		t.Fatal("close did not report the persistence failure")
	}
	if got.Visual == nil || got.Visual.Screen != wantScreen {
		t.Fatalf("worker observed caller mutation: %+v", got.Visual)
	}
	status := recording.Status()
	if status.Pending != 0 || status.Persisted != 0 || status.PersistenceFailed != 1 || !status.Closed {
		t.Fatalf("failure status = %+v", status)
	}
	if _, ok := previews.Entry(41, *got.Visual); ok {
		t.Fatal("failed persistence published a preview")
	}
	if err := recording.RecordSemantic(recordingEvent(t, "late", "debug.help")); !errors.Is(err, diagnostics.ErrClosed) {
		t.Fatalf("post-close admission error = %v", err)
	}
}

func TestDebuggerSemanticClassificationIsExact(t *testing.T) {
	for _, screen := range []string{"debug.health", "debug.timeline", "debug.gallery", "debug.hud", "debug.detail", "debug.help"} {
		if !debuggerSemantic(recordingEvent(t, "event", screen)) {
			t.Fatalf("%s was not classified as debugger diagnostics", screen)
		}
	}
	for _, screen := range []string{"debug.other", "plugin.main"} {
		if debuggerSemantic(recordingEvent(t, "event", screen)) {
			t.Fatalf("%s was classified as debugger diagnostics", screen)
		}
	}
}

func TestRecordingCorrelatesPreviewWithReturnedSequence(t *testing.T) {
	recorder := uiRecorder(t)
	previews, err := diagnostics.OpenPreviewStore(t.TempDir(), diagnostics.DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = previews.Close() })
	recording, err := NewRecording(recorder, previews, nil)
	if err != nil {
		t.Fatal(err)
	}
	event := recordingEvent(t, "preview", "debug.gallery")
	if err := recording.RecordSemantic(event); err != nil {
		t.Fatal(err)
	}
	if err := recording.Close(); err != nil {
		t.Fatal(err)
	}
	page, err := recorder.Debug(diagnostics.DebugQuery{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Visual == nil {
		t.Fatalf("persisted debugger event = %+v", page.Events)
	}
	if _, ok := previews.Entry(page.Events[0].Sequence, *page.Events[0].Visual); !ok {
		t.Fatalf("preview was not correlated with sequence %d", page.Events[0].Sequence)
	}
}

func threeDigits(value int) string {
	return string([]byte{'0' + byte(value/100), '0' + byte(value/10%10), '0' + byte(value%10)})
}
