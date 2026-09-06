package debugui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
)

type asyncProjectionObservation struct {
	event            shell.Event
	loaded           bool
	pending          projectionKind
	queued           projectionKind
	projectionFailed bool
	frozen           bool
	selectedSequence uint64
	selectedIndex    int
	topSequence      uint64
	anchorSelected   uint64
	windowStart      int
	windowLength     int
	evicted          bool
	retained         []uint64
	query            diagnostics.DebugQuery
	list             screen
	resultKind       projectionKind
	resultQuery      diagnostics.DebugQuery
	galleryCount     int
}

type asyncProjectionObserver struct {
	*Surface
	updates       chan asyncProjectionObservation
	overflow      atomic.Bool
	holdNext      atomic.Bool
	held          chan struct{}
	release       chan struct{}
	releaseOnce   sync.Once
	workMu        sync.Mutex
	workOverrides []asyncProjectionWorkOverride
}

type asyncProjectionWorkOverride struct {
	entered chan struct{}
	release <-chan struct{}
	result  func(context.Context, projectionIdentity, *diagnostics.DebugSnapshot) shell.WorkResult
}

func (observer *asyncProjectionObserver) overrideNext(work asyncProjectionWorkOverride) {
	observer.workMu.Lock()
	defer observer.workMu.Unlock()
	observer.workOverrides = append(observer.workOverrides, work)
}

func (observer *asyncProjectionObserver) takeOverride() (asyncProjectionWorkOverride, bool) {
	observer.workMu.Lock()
	defer observer.workMu.Unlock()
	if len(observer.workOverrides) == 0 {
		return asyncProjectionWorkOverride{}, false
	}
	work := observer.workOverrides[0]
	observer.workOverrides = observer.workOverrides[1:]
	return work, true
}

func (observer *asyncProjectionObserver) Update(events shell.EventContext, event shell.Event) []shell.Effect {
	if result, ok := event.(shell.ResultEvent); ok && result.Key == projectionRequest && observer.holdNext.CompareAndSwap(true, false) {
		select {
		case observer.held <- struct{}{}:
		default:
			observer.overflow.Store(true)
		}
		<-observer.release
	}
	effects := observer.Surface.Update(events, event)
	for index, effect := range effects {
		if effect.RequestKey() != projectionRequest {
			continue
		}
		override, ok := observer.takeOverride()
		if !ok {
			continue
		}
		identity := observer.pendingRequest
		pinned := observer.snapshot
		replacement, err := events.Start(projectionRequest, func(ctx context.Context) shell.WorkResult {
			if override.entered != nil {
				select {
				case override.entered <- struct{}{}:
				default:
				}
			}
			if override.release != nil {
				select {
				case <-override.release:
				case <-ctx.Done():
					return shell.WorkResult{Code: diagnostics.OutcomeFailed, Err: ctx.Err()}
				}
			}
			return override.result(ctx, identity, pinned)
		})
		if err == nil {
			effects[index] = replacement
		}
	}
	observation := asyncProjectionObservation{
		event: event, loaded: observer.loaded, pending: observer.pending, queued: observer.queued, projectionFailed: observer.projectionFailed, frozen: observer.frozen,
		topSequence: observer.anchors[observer.anchorIndex()].top, anchorSelected: observer.anchors[observer.anchorIndex()].selected, windowStart: observer.window.Start,
		windowLength: len(observer.window.Events), selectedIndex: observer.selected,
		evicted: observer.evicted, query: observer.query, list: observer.list,
	}
	if selected := observer.selectedEvent(); selected != nil {
		observation.selectedSequence = selected.Sequence
	}
	if result, ok := event.(shell.ResultEvent); ok {
		if value, ok := result.Result.Value.(projectionResult); ok {
			observation.resultKind = value.kind
			observation.resultQuery = value.query
		}
	}
	observation.galleryCount = len(observer.galleryIndices())
	if observer.view != nil {
		for _, retained := range observer.view.Window(diagnostics.DebugWindowQuery{Start: 0}).Events {
			observation.retained = append(observation.retained, retained.Sequence)
		}
	}
	select {
	case observer.updates <- observation:
	default:
		observer.overflow.Store(true)
	}
	return effects
}

func successfulProjectionWork(recorder *diagnostics.Recorder, previews *diagnostics.PreviewStore) func(context.Context, projectionIdentity, *diagnostics.DebugSnapshot) shell.WorkResult {
	return func(ctx context.Context, identity projectionIdentity, pinned *diagnostics.DebugSnapshot) shell.WorkResult {
		snapshot := pinned
		if identity.kind == projectionAcquire {
			var err error
			snapshot, err = recorder.DebugSnapshot(ctx, previews)
			if err != nil {
				return shell.WorkResult{Code: diagnostics.OutcomeFailed, Err: err}
			}
		}
		view, err := snapshot.Select(ctx, identity.query, identity.list == screenGallery)
		if err != nil {
			return shell.WorkResult{Code: diagnostics.OutcomeFailed, Err: err}
		}
		return shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{
			snapshot: snapshot, view: view, query: identity.query, kind: identity.kind, list: identity.list, epoch: identity.epoch,
		}}
	}
}

type asyncProgramHarness struct {
	t        *testing.T
	observer *asyncProjectionObserver
	program  *tea.Program
	done     chan error
	writer   *io.PipeWriter
}

func newAsyncProgramHarness(t *testing.T, options Options) *asyncProgramHarness {
	t.Helper()
	base, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	observer := &asyncProjectionObserver{Surface: base, updates: make(chan asyncProjectionObservation, 256)}
	input, writer := io.Pipe()
	program, err := shell.NewProgram(shell.ProgramOptions{PluginID: uiID(t, "plugin-a"), Theme: terminalTheme(t), Events: &collectingSemanticSink{}, Input: input, Output: io.Discard}, observer)
	if err != nil {
		t.Fatal(err)
	}
	harness := &asyncProgramHarness{t: t, observer: observer, program: program, done: make(chan error, 1), writer: writer}
	go func() { _, runErr := program.Run(); harness.done <- runErr }()
	t.Cleanup(func() {
		program.Kill()
		_ = writer.Close()
		select {
		case <-harness.done:
		case <-time.After(2 * time.Second):
			t.Error("async program did not stop")
		}
		if observer.overflow.Load() {
			t.Error("async observer overflowed")
		}
	})
	return harness
}

func (harness *asyncProgramHarness) await(match func(asyncProjectionObservation) bool) asyncProjectionObservation {
	harness.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case observation := <-harness.observer.updates:
			if match(observation) {
				return observation
			}
		case <-deadline:
			harness.t.Fatal("timed out awaiting async program observation")
		}
	}
}

func (harness *asyncProgramHarness) load() asyncProjectionObservation {
	harness.t.Helper()
	harness.program.Send(tea.WindowSizeMsg{Width: 80, Height: 18})
	return harness.await(func(observation asyncProjectionObservation) bool {
		result, ok := observation.event.(shell.ResultEvent)
		return ok && result.Key == projectionRequest && observation.loaded
	})
}

func projectionFor(t testing.TB, recorder *diagnostics.Recorder, query diagnostics.DebugQuery, list screen) (*diagnostics.DebugSnapshot, *diagnostics.DebugView) {
	t.Helper()
	snapshot, err := recorder.DebugSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := snapshot.Select(context.Background(), query, list == screenGallery)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, view
}

func loadedProjectionSurface(t testing.TB, recorder *diagnostics.Recorder) *Surface {
	t.Helper()
	surface, err := New(Options{Recorder: recorder, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	surface.list, surface.screen = screenTimeline, screenTimeline
	snapshot, view := projectionFor(t, recorder, surface.query, surface.list)
	surface.snapshot, surface.view, surface.loaded = snapshot, view, true
	surface.displayedQuery, surface.displayedList = surface.query, surface.list
	surface.rebuildWindow(false)
	return surface
}

func TestPendingLiveAcquisitionPreservesInputAnchorForDisplayedQuery(t *testing.T) {
	recorder := uiRecorder(t)
	for _, code := range []string{"oldest", "middle", "newest"} {
		addUIEvent(t, recorder, code, "", false)
	}
	surface := loadedProjectionSurface(t, recorder)
	surface.pending = projectionAcquire
	surface.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyDown})
	if event := surface.selectedEvent(); event == nil || event.Code.String() != "middle" {
		t.Fatalf("held-acquisition input selected %+v", event)
	}
	if got := surface.anchors[0].selected; got != 2 {
		t.Fatalf("input anchor during acquisition = %d, want 2", got)
	}
	addUIEvent(t, recorder, "later", "", false)
	snapshot, view := projectionFor(t, recorder, surface.query, screenTimeline)
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{
		snapshot: snapshot, view: view, query: surface.query, kind: projectionAcquire, list: screenTimeline, epoch: surface.projectionEpoch,
	}})
	if event := surface.selectedEvent(); event == nil || event.Sequence != 2 || event.Code.String() != "middle" {
		t.Fatalf("accepted live acquisition lost input anchor: %+v", event)
	}
}

func TestPendingSelectionCannotSaveStaleTimelineAsGalleryAnchor(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "timeline", "", true)
	surface := loadedProjectionSurface(t, recorder)
	surface.anchors[1] = inspectionAnchor{}
	surface.list, surface.screen, surface.pending = screenGallery, screenGallery, projectionSelect
	surface.saveAnchor()
	if surface.anchors[1] != (inspectionAnchor{}) {
		t.Fatalf("stale timeline projection overwrote gallery anchor: %+v", surface.anchors[1])
	}
}

func TestFastFreezeResumeRejectsOldLiveResult(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "initial", "", false)
	surface := loadedProjectionSurface(t, recorder)
	originalSnapshot := surface.snapshot
	surface.pending = projectionAcquire
	oldSnapshot, oldView := projectionFor(t, recorder, surface.query, screenTimeline)
	surface.text(shell.EventContext{}, 'p')
	surface.text(shell.EventContext{}, 'p')
	if surface.frozen || surface.projectionEpoch != 2 || surface.queued != projectionAcquire {
		t.Fatalf("fast freeze/resume state = frozen:%t epoch:%d queued:%d", surface.frozen, surface.projectionEpoch, surface.queued)
	}
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{
		snapshot: oldSnapshot, view: oldView, query: surface.query, kind: projectionAcquire, list: screenTimeline, epoch: 0,
	}})
	if surface.snapshot != originalSnapshot {
		t.Fatal("pre-freeze acquisition was accepted after resume")
	}
	newSnapshot, newView := projectionFor(t, recorder, surface.query, screenTimeline)
	surface.pending = projectionAcquire
	surface.projectionFailed = false
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{
		snapshot: newSnapshot, view: newView, query: surface.query, kind: projectionAcquire, list: screenTimeline, epoch: surface.projectionEpoch,
	}})
	if surface.snapshot != newSnapshot || surface.projectionFailed {
		t.Fatal("current post-resume acquisition was not accepted")
	}
}

func TestFreezeBeforeInitialDataPinsFirstSuccessfulSnapshot(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "first", "", false)
	surface, err := New(Options{Recorder: recorder})
	if err != nil {
		t.Fatal(err)
	}
	surface.pending = projectionAcquire
	surface.text(shell.EventContext{}, 'p')
	if !surface.frozen || surface.projectionEpoch != 0 {
		t.Fatalf("pre-load freeze state = frozen:%t epoch:%d", surface.frozen, surface.projectionEpoch)
	}
	snapshot, view := projectionFor(t, recorder, surface.query, surface.list)
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{
		snapshot: snapshot, view: view, query: surface.query, kind: projectionAcquire, list: surface.list, epoch: 0,
	}})
	if !surface.loaded || surface.snapshot != snapshot || !surface.frozen {
		t.Fatal("first successful snapshot was not pinned by a pre-load freeze")
	}
}

func TestLoadingGuardsDataDependentActions(t *testing.T) {
	recorder := uiRecorder(t)
	surface, err := New(Options{Recorder: recorder, Exporter: func(context.Context, []byte) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	before := surface.query
	for _, input := range []byte{'s', 'l', 'k', '0', '1', '2', '3', '4', 'c', '/', 'e'} {
		surface.text(shell.EventContext{}, input)
	}
	for _, key := range []shell.KeyCode{shell.KeyUp, shell.KeyDown, shell.KeyHome, shell.KeyEnd, shell.KeyPageUp, shell.KeyPageDown, shell.KeyLeft, shell.KeyRight, shell.KeyEnter} {
		surface.key(shell.EventContext{}, key)
	}
	if surface.query != before || surface.editing || surface.export != exportReady || surface.selected != 0 {
		t.Fatalf("loading action changed data state: query=%+v editing=%t export=%d selected=%d", surface.query, surface.editing, surface.export, surface.selected)
	}
	if got := surface.DiagnosticState().State.String(); got != "loading" {
		t.Fatalf("initial diagnostic state = %q", got)
	}
}

func TestProjectionRequestsCoalesceToSingleHighestPriorityFollowup(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "event", "", false)
	surface := loadedProjectionSurface(t, recorder)
	surface.pending = projectionSelect
	surface.requestProjection(shell.EventContext{}, projectionSelect)
	if surface.queued != projectionSelect || surface.pending != projectionSelect {
		t.Fatalf("coalesced select = pending:%d queued:%d", surface.pending, surface.queued)
	}
	surface.requestProjection(shell.EventContext{}, projectionAcquire)
	surface.requestProjection(shell.EventContext{}, projectionSelect)
	if surface.queued != projectionAcquire || surface.pending != projectionSelect {
		t.Fatalf("coalesced acquisition = pending:%d queued:%d", surface.pending, surface.queued)
	}
}

func TestExplicitFilterWithMissingSelectionResetsToFirstResultWithoutEvictionNotice(t *testing.T) {
	recorder := uiRecorder(t)
	for _, code := range []string{"oldest", "middle", "newest"} {
		addUIEvent(t, recorder, code, "", false)
	}
	surface := loadedProjectionSurface(t, recorder)
	surface.selected = 1
	surface.saveAnchor()
	query := surface.query
	query.Code = uiID(t, "oldest")
	snapshot, view := projectionFor(t, recorder, query, screenTimeline)
	surface.query = query
	surface.pending = projectionSelect
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{
		snapshot: snapshot, view: view, query: query, kind: projectionSelect, list: screenTimeline, epoch: surface.projectionEpoch,
	}})
	if surface.evicted || surface.window.Start != 0 || surface.selected != 0 {
		t.Fatalf("filter reset = evicted:%t start:%d selected:%d", surface.evicted, surface.window.Start, surface.selected)
	}
	if event := surface.selectedEvent(); event == nil || event.Code.String() != "oldest" {
		t.Fatalf("filter reset selection = %+v", event)
	}
}

func TestRenderDuringAcquisitionPublishesDisplayedVisibleTop(t *testing.T) {
	recorder := uiRecorder(t)
	for range 60 {
		addUIEvent(t, recorder, "event", "", false)
	}
	surface, err := New(Options{Recorder: recorder, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	surface.list, surface.screen = screenTimeline, screenTimeline
	surface.snapshot, surface.view = projectionFor(t, recorder, surface.query, surface.list)
	surface.loaded, surface.displayedQuery, surface.displayedList = true, surface.query, surface.list
	surface.rebuildWindow(false)
	surface.screen = screenTimeline
	surface.selected = 49
	surface.saveAnchor()
	renderAt(t, surface, responsive.Size{Columns: 80, Rows: 12}, 1)
	oldTop := surface.anchors[0].top
	surface.pending = projectionAcquire
	renderAt(t, surface, responsive.Size{Columns: 80, Rows: 24}, 2)
	newTop := surface.anchors[0].top
	if oldTop == newTop || newTop == 0 {
		t.Fatalf("held acquisition visible top did not advance: old=%d new=%d", oldTop, newTop)
	}
	snapshot, view := projectionFor(t, recorder, surface.query, screenTimeline)
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: snapshot, view: view, query: surface.query, kind: projectionAcquire, list: screenTimeline, epoch: surface.projectionEpoch}})
	if surface.window.Events[0].Sequence != newTop {
		t.Fatalf("accepted acquisition restored stale top: got=%d want=%d", surface.window.Events[0].Sequence, newTop)
	}
}

func TestStaleMissingSessionResultCannotClearCurrentSession(t *testing.T) {
	recorder := uiRecorder(t)
	addDetailedUIEvent(t, recorder, "session-a", "results", "open", "a", "", diagnostics.LevelInfo, diagnostics.KindInteraction, nil)
	addDetailedUIEvent(t, recorder, "session-b", "results", "open", "b", "", diagnostics.LevelInfo, diagnostics.KindInteraction, nil)
	surface := loadedProjectionSurface(t, recorder)
	queryA := surface.query
	queryA.Session = uiID(t, "removed-session")
	snapshot, view := projectionFor(t, recorder, queryA, screenTimeline)
	surface.pending = projectionAcquire
	surface.query.Session = uiID(t, "session-b")
	surface.projectionEpoch++
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: snapshot, view: view, query: queryA, kind: projectionAcquire, list: screenTimeline, epoch: 0}})
	if surface.query.Session.String() != "session-b" {
		t.Fatalf("stale missing session cleared current filter: %q", surface.query.Session)
	}
}

func TestFilterIntentSurvivesAcquirePromotion(t *testing.T) {
	recorder := uiRecorder(t)
	for _, code := range []string{"match", "other", "match", "other"} {
		addUIEvent(t, recorder, code, "", false)
	}
	surface := loadedProjectionSurface(t, recorder)
	surface.selected = 2
	surface.saveAnchor()
	query := surface.query
	query.Code = uiID(t, "match")
	snapshot, view := projectionFor(t, recorder, query, screenTimeline)
	surface.query = query
	surface.pending = projectionAcquire
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: snapshot, view: view, query: query, kind: projectionAcquire, list: screenTimeline, epoch: surface.projectionEpoch}})
	if surface.evicted || surface.selected != 0 || surface.window.Start != 0 || surface.selectedEvent().Sequence != 3 {
		t.Fatalf("promoted filter used live eviction semantics: evicted=%t selected=%d start=%d event=%+v", surface.evicted, surface.selected, surface.window.Start, surface.selectedEvent())
	}
}

func TestFrozenInitialCompletionDropsQueuedAcquire(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "first", "", false)
	surface, _ := New(Options{Recorder: recorder})
	surface.pending, surface.queued, surface.frozen = projectionAcquire, projectionAcquire, true
	snapshot, view := projectionFor(t, recorder, surface.query, surface.list)
	effects := surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: snapshot, view: view, query: surface.query, kind: projectionAcquire, list: surface.list, epoch: 0}})
	if len(effects) != 0 || surface.pending != projectionNone || surface.queued != projectionNone || surface.snapshot != snapshot {
		t.Fatalf("frozen initial completion scheduled replacement: effects=%d pending=%d queued=%d", len(effects), surface.pending, surface.queued)
	}
}

func TestFreezeConvertsQueuedAcquireToSelectionOnPinnedSnapshot(t *testing.T) {
	recorder := uiRecorder(t)
	for _, code := range []string{"other", "match"} {
		addUIEvent(t, recorder, code, "", false)
	}
	surface := loadedProjectionSurface(t, recorder)
	pinned := surface.snapshot
	oldQuery := surface.query
	oldSnapshot, oldView := projectionFor(t, recorder, oldQuery, screenTimeline)
	surface.query.Code = uiID(t, "match")
	surface.pending, surface.queued = projectionAcquire, projectionAcquire
	surface.pendingRequest = projectionIdentity{query: oldQuery, kind: projectionAcquire, list: screenTimeline, epoch: surface.projectionEpoch}
	surface.toggleFreeze(shell.EventContext{})
	if surface.queued != projectionSelect {
		t.Fatalf("freeze queued=%d, want select", surface.queued)
	}
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: oldSnapshot, view: oldView, query: oldQuery, kind: projectionAcquire, list: screenTimeline, epoch: 0}})
	if surface.snapshot != pinned || surface.query.Code.String() != "match" {
		t.Fatal("invalidated acquisition replaced the pinned snapshot or lost filter intent")
	}
	surface.projectionFailed = false // the empty test context cannot schedule the retained selection
	surface.pending = projectionSelect
	view, err := surface.snapshot.Select(context.Background(), surface.query, false)
	if err != nil {
		t.Fatal(err)
	}
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: surface.snapshot, view: view, query: surface.query, kind: projectionSelect, list: screenTimeline, epoch: surface.projectionEpoch}})
	if event := surface.selectedEvent(); event == nil || event.Code.String() != "match" || !surface.frozen {
		t.Fatalf("frozen selection not applied from pinned snapshot: %+v", event)
	}
}

func TestCurrentAcquisitionFailureRemainsVisibleAndRetryable(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "first", "", false)
	surface := loadedProjectionSurface(t, recorder)
	surface.pending = projectionAcquire
	surface.pendingRequest = projectionIdentity{query: surface.query, kind: projectionAcquire, list: surface.list, epoch: surface.projectionEpoch}
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeFailed, Err: errors.New("failed")})
	if !surface.projectionFailed || surface.DiagnosticState().State.String() != "projection-failed" || !surface.DiagnosticState().HasError {
		t.Fatalf("current failure not visible: failed=%t state=%+v", surface.projectionFailed, surface.DiagnosticState())
	}
	surface.text(shell.EventContext{}, 'r')
	if !surface.projectionFailed {
		t.Fatal("failed retry scheduling was not kept visible")
	}
}

func TestFreezeDuringInitialCrossListQueueStillLoadsCurrentProjection(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "first", "", false)
	surface, err := New(Options{Recorder: recorder})
	if err != nil {
		t.Fatal(err)
	}
	initialSnapshot, initialView := projectionFor(t, recorder, surface.query, screenTimeline)
	surface.pending = projectionAcquire
	surface.pendingRequest = projectionIdentity{query: surface.query, kind: projectionAcquire, list: screenTimeline, epoch: 0}
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "g"})
	surface.Update(shell.EventContext{}, shell.ResizeEvent{Layout: responsive.Resolve(responsive.Size{Columns: 80, Rows: 18}), Generation: 2})
	surface.Update(shell.EventContext{}, shell.TextEvent{Text: "p"})
	if !surface.frozen || surface.queued != projectionAcquire {
		t.Fatalf("pre-completion queue frozen=%t queued=%d", surface.frozen, surface.queued)
	}
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: initialSnapshot, view: initialView, query: diagnostics.DebugQuery{PageSize: diagnostics.DefaultDebugPageSize, HideDebugger: true}, kind: projectionAcquire, list: screenTimeline, epoch: 0}})
	if surface.loaded || surface.pending != projectionNone {
		t.Fatalf("stale initial completion loaded=%t pending=%d", surface.loaded, surface.pending)
	}
	currentSnapshot, currentView := projectionFor(t, recorder, surface.query, screenGallery)
	surface.pending = projectionAcquire
	surface.pendingRequest = projectionIdentity{query: surface.query, kind: projectionAcquire, list: screenGallery, epoch: 0}
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: currentSnapshot, view: currentView, query: surface.query, kind: projectionAcquire, list: screenGallery, epoch: 0}})
	if !surface.loaded || surface.snapshot != currentSnapshot || !surface.frozen || surface.pending != projectionNone || surface.queued != projectionNone {
		t.Fatalf("current first projection did not pin: loaded=%t frozen=%t pending=%d queued=%d", surface.loaded, surface.frozen, surface.pending, surface.queued)
	}
}

func TestCrossListChangedFilterUsesTargetListAnchorOwnership(t *testing.T) {
	recorder := uiRecorder(t)
	for index := 1; index <= 100; index++ {
		code := "other"
		if index == 90 || index == 100 {
			code = "match"
		}
		addUIEvent(t, recorder, code, "", false)
	}
	surface, err := New(Options{Recorder: recorder, PageSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	surface.list, surface.screen = screenTimeline, screenTimeline
	surface.snapshot, surface.view = projectionFor(t, recorder, surface.query, screenTimeline)
	surface.loaded, surface.displayedQuery, surface.displayedList = true, surface.query, screenTimeline
	surface.rebuildWindow(false)
	surface.selected = 5 // sequence 95
	surface.saveAnchor()
	surface.list, surface.screen = screenGallery, screenGallery
	surface.query.Code = uiID(t, "match")
	galleryView, selectErr := surface.snapshot.Select(context.Background(), surface.query, false)
	if selectErr != nil {
		t.Fatal(selectErr)
	}
	surface.pending = projectionSelect
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: surface.snapshot, view: galleryView, query: surface.query, kind: projectionSelect, list: screenGallery, epoch: 0}})
	surface.list, surface.screen = screenTimeline, screenTimeline
	timelineView, selectErr := surface.snapshot.Select(context.Background(), surface.query, false)
	if selectErr != nil {
		t.Fatal(selectErr)
	}
	surface.pending = projectionSelect
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: surface.snapshot, view: timelineView, query: surface.query, kind: projectionSelect, list: screenTimeline, epoch: 0}})
	if surface.evicted || surface.window.Start != 0 || surface.selected != 0 || surface.selectedEvent().Sequence != 100 {
		t.Fatalf("target-list changed filter did not reset: evicted=%t start=%d selected=%d event=%+v", surface.evicted, surface.window.Start, surface.selected, surface.selectedEvent())
	}
}

func TestCrossListUnchangedFilterAndMatchingSelectionPreserveAnchors(t *testing.T) {
	recorder := uiRecorder(t)
	for range 3 {
		addUIEvent(t, recorder, "event", "", true)
	}
	previews, err := diagnostics.OpenPreviewStore(t.TempDir(), diagnostics.DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = previews.Close() })
	page, err := recorder.Debug(diagnostics.DebugQuery{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range page.Events {
		if _, err := previews.Put(event.Sequence, *event.Visual); err != nil {
			t.Fatal(err)
		}
	}
	surface, err := New(Options{Recorder: recorder, Previews: previews, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := recorder.DebugSnapshot(context.Background(), previews)
	if err != nil {
		t.Fatal(err)
	}
	timeline, err := snapshot.Select(context.Background(), surface.query, false)
	if err != nil {
		t.Fatal(err)
	}
	surface.snapshot, surface.view, surface.loaded = snapshot, timeline, true
	surface.list, surface.screen, surface.displayedList, surface.displayedQuery = screenTimeline, screenTimeline, screenTimeline, surface.query
	surface.rebuildWindow(false)
	surface.selected = 1 // sequence 2
	surface.saveAnchor()

	gallery, err := snapshot.Select(context.Background(), surface.query, true)
	if err != nil {
		t.Fatal(err)
	}
	surface.list, surface.screen, surface.pending = screenGallery, screenGallery, projectionSelect
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: snapshot, view: gallery, query: surface.query, kind: projectionSelect, list: screenGallery, epoch: 0}})
	surface.selected = 2 // sequence 1
	surface.saveAnchor()
	surface.list, surface.screen, surface.pending = screenTimeline, screenTimeline, projectionSelect
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: snapshot, view: timeline, query: surface.query, kind: projectionSelect, list: screenTimeline, epoch: 0}})
	if surface.selectedEvent().Sequence != 2 || surface.evicted {
		t.Fatalf("unchanged query lost timeline anchor: event=%+v evicted=%t", surface.selectedEvent(), surface.evicted)
	}

	surface.query.Code = uiID(t, "event")
	filteredGallery, err := snapshot.Select(context.Background(), surface.query, true)
	if err != nil {
		t.Fatal(err)
	}
	surface.list, surface.screen, surface.pending = screenGallery, screenGallery, projectionSelect
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: snapshot, view: filteredGallery, query: surface.query, kind: projectionSelect, list: screenGallery, epoch: 0}})
	if surface.selectedEvent().Sequence != 1 || surface.evicted {
		t.Fatalf("matching gallery anchor was reset: event=%+v evicted=%t", surface.selectedEvent(), surface.evicted)
	}
}

func multipageSurface(t testing.TB) (*Surface, *diagnostics.Recorder, *diagnostics.PreviewStore) {
	t.Helper()
	recorder := uiRecorder(t)
	for index := 1; index <= 120; index++ {
		code := "other"
		if index%10 == 0 {
			code = "match"
		}
		addUIEvent(t, recorder, code, "", true)
	}
	previews, err := diagnostics.OpenPreviewStore(t.TempDir(), diagnostics.DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = previews.Close() })
	page, err := recorder.Debug(diagnostics.DebugQuery{PageSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range page.Events {
		if _, err := previews.Put(event.Sequence, *event.Visual); err != nil {
			t.Fatal(err)
		}
	}
	surface, err := New(Options{Recorder: recorder, Previews: previews, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := recorder.DebugSnapshot(context.Background(), previews)
	if err != nil {
		t.Fatal(err)
	}
	view, err := snapshot.Select(context.Background(), surface.query, false)
	if err != nil {
		t.Fatal(err)
	}
	surface.snapshot, surface.view, surface.loaded = snapshot, view, true
	surface.list, surface.screen, surface.displayedList, surface.displayedQuery = screenTimeline, screenTimeline, screenTimeline, surface.query
	surface.rebuildWindow(false)
	return surface, recorder, previews
}

func TestPendingListSwitchCannotPageDisplayedTimelineIntoGalleryAnchor(t *testing.T) {
	surface, _, _ := multipageSurface(t)
	gallery, err := surface.snapshot.Select(context.Background(), surface.query, true)
	if err != nil {
		t.Fatal(err)
	}
	surface.list, surface.screen, surface.pending = screenGallery, screenGallery, projectionSelect
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: surface.snapshot, view: gallery, query: surface.query, kind: projectionSelect, list: screenGallery, epoch: 0}})
	surface.pageTo(20)
	renderAt(t, surface, responsive.Size{Columns: 80, Rows: 12}, 1)
	surface.saveAnchor()
	savedGallery := surface.anchors[1]

	timeline, err := surface.snapshot.Select(context.Background(), surface.query, false)
	if err != nil {
		t.Fatal(err)
	}
	surface.list, surface.screen, surface.pending = screenTimeline, screenTimeline, projectionSelect
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: surface.snapshot, view: timeline, query: surface.query, kind: projectionSelect, list: screenTimeline, epoch: 0}})
	surface.pageTo(10)
	renderAt(t, surface, responsive.Size{Columns: 80, Rows: 12}, 2)
	if !surface.page.HasPrev || !surface.page.HasNext {
		t.Fatalf("timeline flags prev=%t next=%t", surface.page.HasPrev, surface.page.HasNext)
	}
	surface.list, surface.screen, surface.pending = screenGallery, screenGallery, projectionSelect
	surface.pendingRequest = projectionIdentity{query: surface.query, kind: projectionSelect, list: screenGallery, epoch: 0}
	wantWindow, wantSelected, wantTimeline, wantRequest := surface.window, surface.selected, surface.anchors[0], surface.pendingRequest
	surface.key(shell.EventContext{}, shell.KeyPageDown)
	surface.key(shell.EventContext{}, shell.KeyPageUp)
	if surface.anchors[0] != wantTimeline || surface.anchors[1] != savedGallery || surface.window.Start != wantWindow.Start || surface.selected != wantSelected || surface.pendingRequest != wantRequest || surface.list != screenGallery {
		t.Fatalf("pending list paging mutated state: timeline=%+v gallery=%+v window=%d selected=%d", surface.anchors[0], surface.anchors[1], surface.window.Start, surface.selected)
	}
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: surface.snapshot, view: gallery, query: surface.query, kind: projectionSelect, list: screenGallery, epoch: 0}})
	if surface.anchors[1].selected != savedGallery.selected {
		t.Fatalf("accepted gallery lost saved anchor: got=%+v want=%+v", surface.anchors[1], savedGallery)
	}
}

func TestPendingQueryChangeCannotPageOldDisplayedView(t *testing.T) {
	for _, test := range []struct {
		name string
		key  shell.KeyCode
	}{{"page-up", shell.KeyPageUp}, {"page-down", shell.KeyPageDown}} {
		t.Run(test.name, func(t *testing.T) {
			surface, _, _ := multipageSurface(t)
			surface.pageTo(10)
			renderAt(t, surface, responsive.Size{Columns: 80, Rows: 12}, 1)
			if test.key == shell.KeyPageUp && !surface.page.HasPrev || test.key == shell.KeyPageDown && !surface.page.HasNext {
				t.Fatalf("required navigation unavailable: prev=%t next=%t", surface.page.HasPrev, surface.page.HasNext)
			}
			oldAnchor, oldWindow, oldSelected := surface.anchors[0], surface.window, surface.selected
			surface.query.Code = uiID(t, "match")
			filtered, err := surface.snapshot.Select(context.Background(), surface.query, false)
			if err != nil {
				t.Fatal(err)
			}
			surface.pending = projectionSelect
			surface.pendingRequest = projectionIdentity{query: surface.query, kind: projectionSelect, list: screenTimeline, epoch: 0}
			surface.key(shell.EventContext{}, test.key)
			if surface.anchors[0] != oldAnchor || surface.window.Start != oldWindow.Start || surface.selected != oldSelected {
				t.Fatalf("pending query paging mutated displayed view: anchor=%+v window=%d selected=%d", surface.anchors[0], surface.window.Start, surface.selected)
			}
			surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: surface.snapshot, view: filtered, query: surface.query, kind: projectionSelect, list: screenTimeline, epoch: 0}})
			if surface.evicted || surface.selectedEvent().Sequence != oldAnchor.selected || surface.selectedEvent().Code.String() != "match" {
				t.Fatalf("accepted filter did not preserve matching selection=%+v evicted=%t", surface.selectedEvent(), surface.evicted)
			}
		})
	}
}

func TestSameOwnerPendingLiveAcquireCanPageAndPreserveDestination(t *testing.T) {
	for _, test := range []struct {
		name      string
		key       shell.KeyCode
		wantStart int
	}{{"page-up", shell.KeyPageUp, 0}, {"page-down", shell.KeyPageDown, 20}} {
		t.Run(test.name, func(t *testing.T) {
			surface, recorder, previews := multipageSurface(t)
			surface.pageTo(10)
			renderAt(t, surface, responsive.Size{Columns: 80, Rows: 12}, 1)
			if test.key == shell.KeyPageUp && !surface.page.HasPrev || test.key == shell.KeyPageDown && !surface.page.HasNext {
				t.Fatalf("required navigation unavailable: prev=%t next=%t", surface.page.HasPrev, surface.page.HasNext)
			}
			surface.pending = projectionAcquire
			surface.pendingRequest = projectionIdentity{query: surface.query, kind: projectionAcquire, list: screenTimeline, epoch: surface.projectionEpoch}
			surface.key(shell.EventContext{}, test.key)
			renderAt(t, surface, responsive.Size{Columns: 80, Rows: 12}, 2)
			surface.saveAnchor()
			wantSelected, wantTop := surface.anchors[0].selected, surface.anchors[0].top
			if surface.window.Start != test.wantStart || wantSelected == 0 || wantTop == 0 {
				t.Fatalf("live paging destination start=%d want=%d selected=%d top=%d", surface.window.Start, test.wantStart, wantSelected, wantTop)
			}
			for range 3 {
				addUIEvent(t, recorder, "arrival", "", true)
			}
			snapshot, err := recorder.DebugSnapshot(context.Background(), previews)
			if err != nil {
				t.Fatal(err)
			}
			view, err := snapshot.Select(context.Background(), surface.query, false)
			if err != nil {
				t.Fatal(err)
			}
			surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeApplied, Value: projectionResult{snapshot: snapshot, view: view, query: surface.query, kind: projectionAcquire, list: screenTimeline, epoch: surface.projectionEpoch}})
			if surface.anchors[0].selected != wantSelected || surface.anchors[0].top != wantTop || surface.selectedEvent().Sequence != wantSelected || surface.window.Events[0].Sequence != wantTop {
				t.Fatalf("accepted live acquisition lost destination: anchor=%+v event=%+v top=%d", surface.anchors[0], surface.selectedEvent(), surface.window.Events[0].Sequence)
			}
		})
	}
}

func TestPageToWithNilViewIsNoOp(t *testing.T) {
	surface := &Surface{selected: 7, list: screenGallery, displayedList: screenTimeline, query: diagnostics.DebugQuery{PageSize: 10}}
	wantAnchors, wantWindow, wantPage, wantSelected := surface.anchors, surface.window, surface.page, surface.selected
	surface.pageTo(10)
	if surface.anchors != wantAnchors || surface.window.Start != wantWindow.Start || surface.page.Page != wantPage.Page || surface.selected != wantSelected {
		t.Fatal("nil-view paging mutated state")
	}
}

func TestInvalidatedAcquisitionFailureDoesNotHidePinnedProjection(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "first", "", false)
	surface := loadedProjectionSurface(t, recorder)
	surface.pending = projectionAcquire
	surface.pendingRequest = projectionIdentity{query: surface.query, kind: projectionAcquire, list: surface.list, epoch: surface.projectionEpoch}
	surface.toggleFreeze(shell.EventContext{})
	surface.applyProjection(shell.EventContext{}, shell.WorkResult{Code: diagnostics.OutcomeFailed, Err: errors.New("failed")})
	if surface.projectionFailed || surface.view == nil || !surface.loaded {
		t.Fatalf("invalidated failure hid pinned view: failed=%t loaded=%t", surface.projectionFailed, surface.loaded)
	}
}

func TestFilterBroadeningKeepsSelectedEventVisibleAtWindowBoundary(t *testing.T) {
	recorder := uiRecorder(t)
	record := func(code string, level diagnostics.Level) {
		t.Helper()
		event := diagnostics.SemanticEvent{
			Level: level, Kind: diagnostics.KindInteraction, Plugin: uiID(t, "plugin-a"),
			Component: uiID(t, "results"), Action: uiID(t, "open"), Code: uiID(t, code),
			Outcome: diagnostics.OutcomeApplied,
		}
		if err := recorder.RecordSemantic(event); err != nil {
			t.Fatal(err)
		}
	}
	record("selected-b", diagnostics.LevelError)
	for index := range 9 {
		record(fmt.Sprintf("filler-%d", index), diagnostics.LevelInfo)
	}
	record("top-a", diagnostics.LevelError)

	base, err := New(Options{Recorder: recorder, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	observer := &asyncProjectionObserver{Surface: base, updates: make(chan asyncProjectionObservation, 64)}
	input, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	program, err := shell.NewProgram(shell.ProgramOptions{PluginID: uiID(t, "plugin-a"), Theme: terminalTheme(t), Events: &collectingSemanticSink{}, Input: input, Output: io.Discard}, observer)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, runErr := program.Run(); done <- runErr }()
	t.Cleanup(func() {
		program.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("filter program did not stop")
		}
		if observer.overflow.Load() {
			t.Error("filter observer overflowed")
		}
	})
	program.Send(tea.WindowSizeMsg{Width: 80, Height: 18})
	await := func(match func(asyncProjectionObservation) bool) asyncProjectionObservation {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for {
			select {
			case observation := <-observer.updates:
				if match(observation) {
					return observation
				}
			case <-deadline:
				t.Fatal("timed out awaiting filter observation")
			}
		}
	}
	await(func(observation asyncProjectionObservation) bool {
		result, ok := observation.event.(shell.ResultEvent)
		return ok && result.Key == projectionRequest && observation.loaded
	})
	program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	await(func(observation asyncProjectionObservation) bool {
		_, ok := observation.event.(shell.TextEvent)
		return ok
	})
	levels := []diagnostics.Level{diagnostics.LevelDebug, diagnostics.LevelInfo, diagnostics.LevelWarn, diagnostics.LevelError}
	for _, level := range levels {
		program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
		await(func(observation asyncProjectionObservation) bool {
			result, ok := observation.event.(shell.ResultEvent)
			return ok && result.Key == projectionRequest && observation.query.Level == level
		})
	}
	program.Send(tea.KeyMsg{Type: tea.KeyEnd})
	anchored := await(func(observation asyncProjectionObservation) bool {
		_, ok := observation.event.(shell.KeyEvent)
		return ok && observation.selectedSequence == 1
	})
	if anchored.topSequence != 11 || !slices.Equal(anchored.retained, []uint64{11, 1}) {
		t.Fatalf("narrow filter observation=%+v", anchored)
	}
	program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	broadened := await(func(observation asyncProjectionObservation) bool {
		result, ok := observation.event.(shell.ResultEvent)
		return ok && result.Key == projectionRequest && observation.query.Level == ""
	})
	if broadened.selectedSequence != 1 || broadened.windowStart != 1 || broadened.selectedIndex != 9 || broadened.windowLength != 10 || broadened.evicted {
		t.Fatalf("broadened filter observation=%+v", broadened)
	}
	if observer.overflow.Load() {
		t.Fatal("filter observer overflowed")
	}
}

func TestLiveRetentionEvictsOnlySelectedAndKeepsTopAnchor(t *testing.T) {
	config := diagnostics.DefaultConfig(t.TempDir())
	config.MaxEvents = 2
	recorder, err := diagnostics.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	addUIEvent(t, recorder, "event-1", "", false)
	addUIEvent(t, recorder, "event-2", "", false)
	base, err := New(Options{Recorder: recorder, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	observer := &asyncProjectionObserver{
		Surface: base, updates: make(chan asyncProjectionObservation, 64),
		held: make(chan struct{}, 1), release: make(chan struct{}),
	}
	input, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	programEvents := &collectingSemanticSink{}
	program, err := shell.NewProgram(shell.ProgramOptions{PluginID: uiID(t, "plugin-a"), Theme: terminalTheme(t), Events: programEvents, Input: input, Output: io.Discard}, observer)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, runErr := program.Run(); done <- runErr }()
	t.Cleanup(func() {
		observer.releaseOnce.Do(func() { close(observer.release) })
		program.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("retention program did not stop")
		}
		if observer.overflow.Load() {
			t.Error("retention observer overflowed")
		}
	})
	program.Send(tea.WindowSizeMsg{Width: 80, Height: 18})

	await := func(match func(asyncProjectionObservation) bool) asyncProjectionObservation {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for {
			select {
			case observation := <-observer.updates:
				if match(observation) {
					return observation
				}
			case <-deadline:
				t.Fatal("timed out awaiting retention observation")
			}
		}
	}
	initial := await(func(observation asyncProjectionObservation) bool {
		result, ok := observation.event.(shell.ResultEvent)
		return ok && result.Key == projectionRequest && observation.loaded
	})
	if !slices.Equal(initial.retained, []uint64{2, 1}) || initial.selectedSequence != 2 || initial.windowStart != 0 {
		t.Fatalf("initial retention observation=%+v", initial)
	}
	program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	await(func(observation asyncProjectionObservation) bool {
		text, ok := observation.event.(shell.TextEvent)
		return ok && text.Text == "t"
	})
	program.Send(tea.KeyMsg{Type: tea.KeyEnd})
	anchored := await(func(observation asyncProjectionObservation) bool {
		_, ok := observation.event.(shell.KeyEvent)
		return ok && observation.selectedSequence == 1
	})
	if !slices.Equal(anchored.retained, []uint64{2, 1}) || anchored.topSequence != 2 || anchored.windowStart != 0 {
		t.Fatalf("pre-eviction observation=%+v", anchored)
	}

	observer.holdNext.Store(true)
	addUIEvent(t, recorder, "event-3", "", false)
	select {
	case <-observer.held:
	case <-time.After(2 * time.Second):
		t.Fatal("live acquisition completion was not held")
	}
	page, err := recorder.Debug(diagnostics.DebugQuery{PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := []uint64{page.Events[0].Sequence, page.Events[1].Sequence}; !slices.Equal(got, []uint64{3, 2}) {
		t.Fatalf("recorder retention before release=%v, want [3 2]", got)
	}
	observer.releaseOnce.Do(func() { close(observer.release) })
	accepted := await(func(observation asyncProjectionObservation) bool {
		result, ok := observation.event.(shell.ResultEvent)
		return ok && result.Key == projectionRequest && slices.Equal(observation.retained, []uint64{3, 2})
	})
	if accepted.selectedSequence != 2 || accepted.topSequence != 2 || accepted.windowStart != 1 || !accepted.evicted {
		t.Fatalf("accepted retention observation=%+v", accepted)
	}
	if observer.overflow.Load() {
		t.Fatal("retention observer overflowed")
	}
}

func TestRebuildWindowHonorsPositionZeroAnchorsAndPageMetadata(t *testing.T) {
	recorder := uiRecorder(t)
	for index := range 25 {
		addUIEvent(t, recorder, fmt.Sprintf("event-%d", index), "", false)
	}
	surface := loadedProjectionSurface(t, recorder)
	anchor := &surface.anchors[0]
	first := surface.view.Window(diagnostics.DebugWindowQuery{Start: 0}).Events[0].Sequence

	anchor.selected, anchor.top, anchor.start = 0, first, 17
	surface.rebuildWindow(false)
	if surface.window.Start != 0 || surface.page.Page != 0 || anchor.top != first {
		t.Fatalf("top-at-zero rebuild: start=%d page=%d anchor=%+v", surface.window.Start, surface.page.Page, *anchor)
	}

	anchor.selected, anchor.top, anchor.start = first, 0, 17
	surface.rebuildWindow(false)
	if surface.window.Start != 0 || surface.selected != 0 || surface.selectedEvent() == nil || surface.selectedEvent().Sequence != first {
		t.Fatalf("selection-at-zero rebuild: start=%d index=%d selected=%+v", surface.window.Start, surface.selected, surface.selectedEvent())
	}

	pageTwo := surface.view.Window(diagnostics.DebugWindowQuery{Start: 20})
	anchor.selected, anchor.top, anchor.start = pageTwo.Events[0].Sequence, pageTwo.Events[0].Sequence, 20
	surface.rebuildWindow(false)
	if surface.window.Start != 20 || surface.page.Page != 2 || surface.selected != 0 {
		t.Fatalf("page metadata rebuild: start=%d page=%d selected=%d", surface.window.Start, surface.page.Page, surface.selected)
	}
}

func TestFilterBoundaryRebuildDistinguishesMissingTopAndMissingSelection(t *testing.T) {
	recorder := uiRecorder(t)
	addDetailedUIEvent(t, recorder, "plugin-a", "results", "open", "selected-b", "", diagnostics.LevelError, diagnostics.KindInteraction, nil)
	for index := range 10 {
		addDetailedUIEvent(t, recorder, "plugin-a", "results", "open", fmt.Sprintf("filler-%d", index), "", diagnostics.LevelInfo, diagnostics.KindInteraction, nil)
	}
	addDetailedUIEvent(t, recorder, "plugin-a", "results", "open", "top-a", "", diagnostics.LevelError, diagnostics.KindInteraction, nil)
	surface, err := New(Options{Recorder: recorder, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	surface.list, surface.screen = screenTimeline, screenTimeline
	surface.query.Level = diagnostics.LevelError
	surface.snapshot, surface.view = projectionFor(t, recorder, surface.query, screenTimeline)
	surface.loaded, surface.displayedQuery, surface.displayedList = true, surface.query, screenTimeline
	surface.rebuildWindow(false)
	surface.selected, surface.visibleTop[0] = 1, 12
	surface.saveAnchor()

	surface.query.Level = ""
	_, broad := projectionFor(t, recorder, surface.query, screenTimeline)
	surface.view, surface.displayedQuery = broad, surface.query
	surface.rebuildWindow(true)
	if surface.selectedEvent() == nil || surface.selectedEvent().Sequence != 1 || surface.window.Start != 2 || surface.selected != 9 || surface.evicted {
		t.Fatalf("second-boundary broadening: selected=%+v start=%d index=%d evicted=%t", surface.selectedEvent(), surface.window.Start, surface.selected, surface.evicted)
	}

	surface.anchors[0] = inspectionAnchor{selected: 1, top: 12, query: diagnostics.DebugQuery{Level: diagnostics.LevelError, PageSize: 10, HideDebugger: true}, queryOwned: true}
	surface.query.Level = diagnostics.LevelError
	queryWithoutTop := surface.query
	queryWithoutTop.Code = uiID(t, "selected-b")
	_, selectedOnly := projectionFor(t, recorder, queryWithoutTop, screenTimeline)
	surface.query, surface.displayedQuery, surface.view = queryWithoutTop, queryWithoutTop, selectedOnly
	surface.rebuildWindow(true)
	if surface.selectedEvent() == nil || surface.selectedEvent().Sequence != 1 || surface.evicted {
		t.Fatalf("missing top disturbed surviving selection: selected=%+v evicted=%t", surface.selectedEvent(), surface.evicted)
	}

	surface.anchors[0] = inspectionAnchor{selected: 1, top: 12, query: queryWithoutTop, queryOwned: true}
	queryWithoutSelected := diagnostics.DebugQuery{Level: diagnostics.LevelInfo, PageSize: 10, HideDebugger: true}
	_, withoutSelected := projectionFor(t, recorder, queryWithoutSelected, screenTimeline)
	surface.query, surface.displayedQuery, surface.view = queryWithoutSelected, queryWithoutSelected, withoutSelected
	surface.rebuildWindow(true)
	if surface.window.Start != 0 || surface.selectedEvent() == nil || surface.selectedEvent().Sequence != 11 || surface.evicted {
		t.Fatalf("missing selection did not reset: selected=%+v start=%d evicted=%t", surface.selectedEvent(), surface.window.Start, surface.evicted)
	}
}

func TestRenderPublishesVisibleTopOnlyForDisplayedOwner(t *testing.T) {
	recorder := uiRecorder(t)
	for range 20 {
		addUIEvent(t, recorder, "event", "", false)
	}
	surface := loadedProjectionSurface(t, recorder)
	surface.selected = 9
	surface.anchors[0].top, surface.visibleTop[0] = 777, 777

	surface.query.Level = diagnostics.LevelError
	renderAt(t, surface, responsive.Size{Columns: 80, Rows: 12}, 1)
	if surface.anchors[0].top != 777 || surface.visibleTop[0] != 777 {
		t.Fatalf("stale query published top: anchor=%d visible=%d", surface.anchors[0].top, surface.visibleTop[0])
	}

	surface.query = surface.displayedQuery
	surface.list = screenGallery
	renderAt(t, surface, responsive.Size{Columns: 80, Rows: 12}, 2)
	if surface.anchors[0].top != 777 || surface.visibleTop[0] != 777 {
		t.Fatalf("stale list published timeline top: anchor=%d visible=%d", surface.anchors[0].top, surface.visibleTop[0])
	}

	surface.list = surface.displayedList
	renderAt(t, surface, responsive.Size{Columns: 80, Rows: 12}, 3)
	if surface.anchors[0].top == 777 || surface.visibleTop[0] == 777 || surface.anchors[0].top == 0 {
		t.Fatalf("current owner did not publish top: anchor=%d visible=%d", surface.anchors[0].top, surface.visibleTop[0])
	}
}

func TestAnchorAndDiagnosticStateRespectCurrentOwnership(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "event", "", false)
	surface := loadedProjectionSurface(t, recorder)
	surface.anchors[0] = inspectionAnchor{selected: 99, top: 98, start: 7}
	surface.query.Level = diagnostics.LevelError
	surface.saveAnchor()
	if surface.anchors[0].selected != 99 || surface.anchors[0].top != 98 || surface.anchors[0].start != 7 {
		t.Fatalf("stale owner changed anchor: %+v", surface.anchors[0])
	}
	surface.query = surface.displayedQuery
	surface.anchors[0].top, surface.visibleTop[0] = 98, 0
	surface.saveAnchor()
	if surface.anchors[0].top != 98 {
		t.Fatalf("zero visible top erased retained anchor: %+v", surface.anchors[0])
	}
	surface.export = exportReady
	if state := surface.DiagnosticState(); state.Pending {
		t.Fatalf("ready diagnostic state reported pending: %+v", state)
	}
	surface.export = exportPending
	if state := surface.DiagnosticState(); !state.Pending {
		t.Fatalf("pending export was absent from diagnostic state: %+v", state)
	}
}

func TestGalleryRequiresVisualPayloadEvenWhenPreviewIsEligible(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "preview", "", true)
	page, err := recorder.Debug(diagnostics.DebugQuery{})
	if err != nil {
		t.Fatal(err)
	}
	previews, err := diagnostics.OpenPreviewStore(t.TempDir(), diagnostics.DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = previews.Close() })
	if _, err := previews.Put(page.Events[0].Sequence, *page.Events[0].Visual); err != nil {
		t.Fatal(err)
	}
	surface, err := New(Options{Recorder: recorder, Previews: previews})
	if err != nil {
		t.Fatal(err)
	}
	surface.list, surface.screen = screenGallery, screenGallery
	surface.snapshot, err = recorder.DebugSnapshot(context.Background(), previews)
	if err != nil {
		t.Fatal(err)
	}
	surface.view, err = surface.snapshot.Select(context.Background(), surface.query, true)
	if err != nil {
		t.Fatal(err)
	}
	surface.loaded, surface.displayedQuery, surface.displayedList = true, surface.query, screenGallery
	surface.rebuildWindow(false)
	if !surface.view.PreviewEligible(surface.page.Events[0].Sequence) {
		t.Fatal("fixture event is not preview eligible")
	}
	surface.page.Events[0].Visual = nil
	if indices := surface.galleryIndices(); len(indices) != 0 {
		t.Fatalf("gallery indices = %v, want none without visual payload", indices)
	}
	eligible := page.Events[0]
	omitted := eligible
	omitted.Visual = nil
	surface.page.Events = []diagnostics.DebugEvent{omitted, eligible, omitted}
	surface.selected = 2
	surface.key(shell.EventContext{}, shell.KeyHome)
	if surface.selected != 1 {
		t.Fatalf("gallery home selected=%d, want only eligible index 1", surface.selected)
	}
	surface.selected = 0
	surface.key(shell.EventContext{}, shell.KeyEnd)
	if surface.selected != 1 {
		t.Fatalf("gallery end selected=%d, want only eligible index 1", surface.selected)
	}
	surface.list, surface.page.Events, surface.selected = screenTimeline, nil, 7
	surface.key(shell.EventContext{}, shell.KeyEnd)
	if surface.selected != 7 {
		t.Fatalf("timeline end changed empty selection to %d", surface.selected)
	}
}

func TestRealProgramCompletesInitialProjectionWithValidEventContext(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "initial", "", false)
	base, err := New(Options{Recorder: recorder})
	if err != nil {
		t.Fatal(err)
	}
	observer := &asyncProjectionObserver{Surface: base, updates: make(chan asyncProjectionObservation, 32)}
	input, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	events := &collectingSemanticSink{}
	program, err := shell.NewProgram(shell.ProgramOptions{PluginID: uiID(t, "plugin-a"), Theme: terminalTheme(t), Events: events, Input: input, Output: io.Discard}, observer)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, runErr := program.Run(); done <- runErr }()
	t.Cleanup(func() {
		program.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("debug program did not stop")
		}
		if observer.overflow.Load() {
			t.Error("initial projection observer overflowed")
		}
	})
	program.Send(tea.WindowSizeMsg{Width: 80, Height: 18})

	deadline := time.After(2 * time.Second)
	for {
		select {
		case observation := <-observer.updates:
			result, ok := observation.event.(shell.ResultEvent)
			if !ok || result.Key != projectionRequest {
				continue
			}
			if !observation.loaded || observation.pending != projectionNone || observation.projectionFailed || observation.selectedSequence != 1 {
				t.Fatalf("initial async projection observation=%+v", observation)
			}
			if observer.overflow.Load() {
				t.Fatal("initial projection observer overflowed")
			}
			return
		case <-deadline:
			t.Fatal("real program did not complete initial projection")
		}
	}
}

func TestRealProgramPromotesInitialGalleryAndKeepsLoadedSelectionOnSnapshot(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "preview", "", true)
	page, err := recorder.Debug(diagnostics.DebugQuery{})
	if err != nil {
		t.Fatal(err)
	}
	previews, err := diagnostics.OpenPreviewStore(t.TempDir(), diagnostics.DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = previews.Close() })
	if _, err := previews.Put(page.Events[0].Sequence, *page.Events[0].Visual); err != nil {
		t.Fatal(err)
	}
	addUIEvent(t, recorder, "semantic-only", "", false)
	base, err := New(Options{Recorder: recorder, Previews: previews})
	if err != nil {
		t.Fatal(err)
	}
	observer := &asyncProjectionObserver{Surface: base, updates: make(chan asyncProjectionObservation, 32)}
	input, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	program, err := shell.NewProgram(shell.ProgramOptions{PluginID: uiID(t, "plugin-a"), Theme: terminalTheme(t), Events: &collectingSemanticSink{}, Input: input, Output: io.Discard}, observer)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, runErr := program.Run(); done <- runErr }()
	t.Cleanup(func() {
		program.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("gallery promotion program did not stop")
		}
		if observer.overflow.Load() {
			t.Error("gallery promotion observer overflowed")
		}
	})
	await := func(match func(asyncProjectionObservation) bool) asyncProjectionObservation {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for {
			select {
			case observation := <-observer.updates:
				if match(observation) {
					return observation
				}
			case <-deadline:
				t.Fatal("timed out awaiting gallery promotion observation")
			}
		}
	}
	program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	initial := await(func(observation asyncProjectionObservation) bool {
		result, ok := observation.event.(shell.ResultEvent)
		return ok && result.Key == projectionRequest
	})
	if !initial.loaded || initial.projectionFailed || initial.resultKind != projectionAcquire || initial.list != screenGallery || initial.galleryCount != 1 || !slices.Equal(initial.retained, []uint64{1}) {
		t.Fatalf("initial gallery promotion=%+v", initial)
	}
	program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	selection := await(func(observation asyncProjectionObservation) bool {
		result, ok := observation.event.(shell.ResultEvent)
		return ok && result.Key == projectionRequest && observation.query.Level == diagnostics.LevelDebug
	})
	if selection.projectionFailed || selection.resultKind != projectionSelect || selection.list != screenGallery {
		t.Fatalf("loaded gallery selection=%+v", selection)
	}
	if observer.overflow.Load() {
		t.Fatal("gallery promotion observer overflowed")
	}
}

func TestRealProgramRestoresIndependentTimelineAndGalleryAnchors(t *testing.T) {
	recorder := uiRecorder(t)
	visual := &diagnostics.VisualState{Screen: uiID(t, "search"), Focus: uiID(t, "results"), State: uiID(t, "ready")}
	addDetailedUIEvent(t, recorder, "plugin-a", "results", "open", "old-visual", "", diagnostics.LevelInfo, diagnostics.KindInteraction, visual)
	addDetailedUIEvent(t, recorder, "plugin-a", "results", "open", "middle", "", diagnostics.LevelInfo, diagnostics.KindInteraction, nil)
	addDetailedUIEvent(t, recorder, "plugin-a", "results", "open", "new-visual", "", diagnostics.LevelInfo, diagnostics.KindInteraction, visual)
	page, err := recorder.Debug(diagnostics.DebugQuery{})
	if err != nil {
		t.Fatal(err)
	}
	previews, err := diagnostics.OpenPreviewStore(t.TempDir(), diagnostics.DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = previews.Close() })
	for _, event := range page.Events {
		if event.Visual != nil {
			if _, err := previews.Put(event.Sequence, *event.Visual); err != nil {
				t.Fatal(err)
			}
		}
	}
	harness := newAsyncProgramHarness(t, Options{Recorder: recorder, Previews: previews, PageSize: 10})
	harness.load()
	harness.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	harness.await(func(observation asyncProjectionObservation) bool {
		text, ok := observation.event.(shell.TextEvent)
		return ok && text.Text == "t"
	})
	harness.program.Send(tea.KeyMsg{Type: tea.KeyEnd})
	timeline := harness.await(func(observation asyncProjectionObservation) bool {
		_, ok := observation.event.(shell.KeyEvent)
		return ok && observation.selectedSequence == 1
	})
	if timeline.topSequence != 3 {
		t.Fatalf("timeline anchor=%+v", timeline)
	}
	harness.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	harness.await(func(observation asyncProjectionObservation) bool {
		result, ok := observation.event.(shell.ResultEvent)
		return ok && result.Key == projectionRequest && observation.list == screenGallery
	})
	harness.program.Send(tea.KeyMsg{Type: tea.KeyEnd})
	gallery := harness.await(func(observation asyncProjectionObservation) bool {
		_, ok := observation.event.(shell.KeyEvent)
		return ok && observation.selectedSequence == 1
	})
	if gallery.topSequence != 3 {
		t.Fatalf("gallery anchor=%+v", gallery)
	}
	harness.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	restoredTimeline := harness.await(func(observation asyncProjectionObservation) bool {
		result, ok := observation.event.(shell.ResultEvent)
		return ok && result.Key == projectionRequest && observation.list == screenTimeline
	})
	if restoredTimeline.selectedSequence != 1 || restoredTimeline.topSequence != 3 {
		t.Fatalf("restored timeline=%+v", restoredTimeline)
	}
	harness.program.Send(tea.KeyMsg{Type: tea.KeyHome})
	harness.await(func(observation asyncProjectionObservation) bool {
		_, ok := observation.event.(shell.KeyEvent)
		return ok && observation.selectedSequence == 3
	})
	harness.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	restoredGallery := harness.await(func(observation asyncProjectionObservation) bool {
		result, ok := observation.event.(shell.ResultEvent)
		return ok && result.Key == projectionRequest && observation.list == screenGallery
	})
	if restoredGallery.selectedSequence != 1 || restoredGallery.topSequence != 3 {
		t.Fatalf("restored gallery=%+v", restoredGallery)
	}
	if harness.observer.overflow.Load() {
		t.Fatal("anchor observer overflowed")
	}
}

func TestRealProgramFrozenSelectionUsesPinnedSnapshotThenResumeAcquiresArrival(t *testing.T) {
	recorder := uiRecorder(t)
	addUIEvent(t, recorder, "initial", "", false)
	harness := newAsyncProgramHarness(t, Options{Recorder: recorder})
	harness.load()
	harness.program.Send(tea.KeyMsg{Type: tea.KeySpace})
	harness.await(func(observation asyncProjectionObservation) bool {
		key, ok := observation.event.(shell.KeyEvent)
		return ok && key.Code == shell.KeySpace
	})
	addDetailedUIEvent(t, recorder, "plugin-a", "results", "open", "arrival", "", diagnostics.LevelError, diagnostics.KindInteraction, nil)
	for range 4 {
		harness.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	}
	pinned := harness.await(func(observation asyncProjectionObservation) bool {
		result, ok := observation.event.(shell.ResultEvent)
		return ok && result.Key == projectionRequest && observation.resultQuery.Level == diagnostics.LevelError
	})
	if len(pinned.retained) != 0 || pinned.resultKind != projectionSelect {
		t.Fatalf("frozen selection escaped pinned snapshot: %+v", pinned)
	}
	harness.program.Send(tea.KeyMsg{Type: tea.KeySpace})
	resumed := harness.await(func(observation asyncProjectionObservation) bool {
		result, ok := observation.event.(shell.ResultEvent)
		return ok && result.Key == projectionRequest && slices.Equal(observation.retained, []uint64{2})
	})
	if resumed.resultKind != projectionAcquire || resumed.selectedSequence != 2 {
		t.Fatalf("resumed acquisition=%+v", resumed)
	}
	if harness.observer.overflow.Load() {
		t.Fatal("freeze observer overflowed")
	}
}

func TestRealProgramCurrentFailureShapesRecoverWithSuccessfulProjection(t *testing.T) {
	for _, test := range []struct {
		name   string
		result shell.WorkResult
	}{
		{name: "error-with-applied-code", result: shell.WorkResult{Code: diagnostics.OutcomeApplied, Err: errors.New("injected projection error")}},
		{name: "non-applied-code-without-error", result: shell.WorkResult{Code: diagnostics.OutcomeFailed}},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := uiRecorder(t)
			addUIEvent(t, recorder, "event", "", false)
			harness := newAsyncProgramHarness(t, Options{Recorder: recorder})
			harness.load()
			harness.observer.overrideNext(asyncProjectionWorkOverride{result: func(context.Context, projectionIdentity, *diagnostics.DebugSnapshot) shell.WorkResult {
				return test.result
			}})
			harness.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
			failed := harness.await(func(observation asyncProjectionObservation) bool {
				result, ok := observation.event.(shell.ResultEvent)
				return ok && result.Key == projectionRequest && observation.projectionFailed
			})
			if failed.pending != projectionNone {
				t.Fatalf("failed projection remained pending: %+v", failed)
			}
			harness.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
			recovered := harness.await(func(observation asyncProjectionObservation) bool {
				result, ok := observation.event.(shell.ResultEvent)
				return ok && result.Key == projectionRequest && !observation.projectionFailed
			})
			if recovered.resultKind != projectionSelect {
				t.Fatalf("recovery projection=%+v", recovered)
			}
			if harness.observer.overflow.Load() {
				t.Fatal("failure observer overflowed")
			}
		})
	}
}

func TestRealProgramHeldCompletionCoalescingAndFrozenConversions(t *testing.T) {
	t.Run("select-acquire-select-priority", func(t *testing.T) {
		recorder := uiRecorder(t)
		addUIEvent(t, recorder, "event", "", false)
		harness := newAsyncProgramHarness(t, Options{Recorder: recorder})
		harness.load()
		entered, release := make(chan struct{}, 1), make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		harness.observer.overrideNext(asyncProjectionWorkOverride{entered: entered, release: release, result: successfulProjectionWork(recorder, nil)})
		harness.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("selection work did not enter hold")
		}
		maintenance := harness.await(func(observation asyncProjectionObservation) bool {
			timer, ok := observation.event.(shell.TimerEvent)
			return ok && timer.Code == maintenanceCode && observation.queued == projectionAcquire
		})
		if maintenance.pending != projectionSelect {
			t.Fatalf("maintenance coalescing=%+v", maintenance)
		}
		harness.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
		third := harness.await(func(observation asyncProjectionObservation) bool {
			text, ok := observation.event.(shell.TextEvent)
			return ok && text.Text == "l" && observation.query.Level == diagnostics.LevelInfo
		})
		if third.pending != projectionSelect || third.queued != projectionAcquire {
			t.Fatalf("select-acquire-select priority=%+v", third)
		}
		releaseOnce.Do(func() { close(release) })
		accepted := harness.await(func(observation asyncProjectionObservation) bool {
			result, ok := observation.event.(shell.ResultEvent)
			return ok && result.Key == projectionRequest && observation.resultKind == projectionAcquire && observation.resultQuery.Level == diagnostics.LevelInfo
		})
		if accepted.pending != projectionNone || accepted.queued != projectionNone || accepted.projectionFailed {
			t.Fatalf("coalesced acquisition=%+v", accepted)
		}
	})

	for _, test := range []struct {
		name      string
		change    func(*asyncProgramHarness)
		wantList  screen
		wantLevel diagnostics.Level
	}{
		{name: "query-only-mismatch", change: func(h *asyncProgramHarness) { h.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}) }, wantList: screenTimeline, wantLevel: diagnostics.LevelDebug},
		{name: "list-only-mismatch", change: func(h *asyncProgramHarness) { h.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}}) }, wantList: screenGallery},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := uiRecorder(t)
			addUIEvent(t, recorder, "event", "", false)
			harness := newAsyncProgramHarness(t, Options{Recorder: recorder})
			harness.load()
			entered, release := make(chan struct{}, 1), make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			harness.observer.overrideNext(asyncProjectionWorkOverride{entered: entered, release: release, result: successfulProjectionWork(recorder, nil)})
			harness.await(func(observation asyncProjectionObservation) bool {
				select {
				case <-entered:
					return true
				default:
					return false
				}
			})
			test.change(harness)
			harness.await(func(observation asyncProjectionObservation) bool {
				_, text := observation.event.(shell.TextEvent)
				return text && observation.list == test.wantList && observation.query.Level == test.wantLevel
			})
			harness.program.Send(tea.KeyMsg{Type: tea.KeySpace})
			frozen := harness.await(func(observation asyncProjectionObservation) bool {
				key, ok := observation.event.(shell.KeyEvent)
				return ok && key.Code == shell.KeySpace && observation.frozen
			})
			if frozen.queued != projectionSelect {
				t.Fatalf("frozen mismatch queue=%+v", frozen)
			}
			releaseOnce.Do(func() { close(release) })
			converted := harness.await(func(observation asyncProjectionObservation) bool {
				result, ok := observation.event.(shell.ResultEvent)
				return ok && result.Key == projectionRequest && observation.resultKind == projectionSelect && observation.resultQuery.Level == test.wantLevel && observation.list == test.wantList
			})
			if !converted.frozen || converted.pending != projectionNone || converted.projectionFailed {
				t.Fatalf("converted frozen selection=%+v", converted)
			}
		})
	}

	t.Run("frozen-acquisition-drop", func(t *testing.T) {
		recorder := uiRecorder(t)
		addUIEvent(t, recorder, "initial", "", false)
		harness := newAsyncProgramHarness(t, Options{Recorder: recorder})
		harness.load()
		addUIEvent(t, recorder, "arrival", "", false)
		entered, release := make(chan struct{}, 1), make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		harness.observer.overrideNext(asyncProjectionWorkOverride{entered: entered, release: release, result: successfulProjectionWork(recorder, nil)})
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("acquisition work did not enter hold")
		}
		harness.program.Send(tea.KeyMsg{Type: tea.KeySpace})
		harness.await(func(observation asyncProjectionObservation) bool {
			key, ok := observation.event.(shell.KeyEvent)
			return ok && key.Code == shell.KeySpace && observation.frozen
		})
		releaseOnce.Do(func() { close(release) })
		dropped := harness.await(func(observation asyncProjectionObservation) bool {
			result, ok := observation.event.(shell.ResultEvent)
			return ok && result.Key == projectionRequest && observation.resultKind == projectionAcquire
		})
		if !dropped.frozen || !slices.Equal(dropped.retained, []uint64{1}) || dropped.pending != projectionNone || dropped.queued != projectionNone {
			t.Fatalf("frozen acquisition was not dropped: %+v", dropped)
		}
	})
}

func TestRealProgramRetentionRemovesSelectedSessionAndCompletesRecovery(t *testing.T) {
	config := diagnostics.DefaultConfig(t.TempDir())
	config.MaxEvents = 3
	recorder, err := diagnostics.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	addDetailedUIEvent(t, recorder, "session-a", "results", "open", "a-1", "", diagnostics.LevelInfo, diagnostics.KindInteraction, nil)
	addDetailedUIEvent(t, recorder, "session-b", "results", "open", "b-1", "", diagnostics.LevelInfo, diagnostics.KindInteraction, nil)
	harness := newAsyncProgramHarness(t, Options{Recorder: recorder, PageSize: 10})
	harness.load()
	harness.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	harness.await(func(observation asyncProjectionObservation) bool {
		text, ok := observation.event.(shell.TextEvent)
		return ok && text.Text == "t"
	})
	harness.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	filtered := harness.await(func(observation asyncProjectionObservation) bool {
		result, ok := observation.event.(shell.ResultEvent)
		return ok && result.Key == projectionRequest && !observation.query.Session.IsZero()
	})
	if !slices.Equal(filtered.retained, []uint64{2}) || filtered.selectedSequence != 2 {
		t.Fatalf("selected session filter=%+v", filtered)
	}
	for index := range 3 {
		addDetailedUIEvent(t, recorder, "session-a", "results", "open", fmt.Sprintf("a-%d", index+2), "", diagnostics.LevelInfo, diagnostics.KindInteraction, nil)
	}
	recovered := harness.await(func(observation asyncProjectionObservation) bool {
		result, ok := observation.event.(shell.ResultEvent)
		return ok && result.Key == projectionRequest && observation.query.Session.IsZero() && observation.pending == projectionNone && slices.Equal(observation.retained, []uint64{5, 4, 3})
	})
	if recovered.selectedSequence != 5 || recovered.projectionFailed || recovered.resultKind != projectionSelect {
		t.Fatalf("session recovery=%+v", recovered)
	}
	if harness.observer.overflow.Load() {
		t.Fatal("session recovery observer overflowed")
	}
}

func TestRealProgramDisplayedOwnerTransitionsPreserveTargetAnchorsUntilAcceptance(t *testing.T) {
	t.Run("query-only", func(t *testing.T) {
		recorder := uiRecorder(t)
		for index := range 3 {
			addUIEvent(t, recorder, fmt.Sprintf("event-%d", index), "", false)
		}
		harness := newAsyncProgramHarness(t, Options{Recorder: recorder, PageSize: 10})
		harness.load()
		harness.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
		harness.await(func(observation asyncProjectionObservation) bool {
			text, ok := observation.event.(shell.TextEvent)
			return ok && text.Text == "t"
		})
		harness.program.Send(tea.KeyMsg{Type: tea.KeyEnd})
		harness.await(func(observation asyncProjectionObservation) bool {
			_, ok := observation.event.(shell.KeyEvent)
			return ok && observation.selectedSequence == 1 && observation.topSequence == 3
		})
		entered, release := make(chan struct{}, 1), make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		harness.observer.overrideNext(asyncProjectionWorkOverride{entered: entered, release: release, result: successfulProjectionWork(recorder, nil)})
		harness.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("query selection did not enter hold")
		}
		harness.program.Send(tea.KeyMsg{Type: tea.KeyHome})
		during := harness.await(func(observation asyncProjectionObservation) bool {
			_, ok := observation.event.(shell.KeyEvent)
			return ok
		})
		if during.anchorSelected != 1 || during.topSequence != 3 || during.selectedSequence != 3 {
			t.Fatalf("query transition changed target anchor=%+v", during)
		}
		releaseOnce.Do(func() { close(release) })
		accepted := harness.await(func(observation asyncProjectionObservation) bool {
			result, ok := observation.event.(shell.ResultEvent)
			return ok && result.Key == projectionRequest && observation.resultKind == projectionSelect
		})
		if accepted.selectedSequence != 1 || accepted.topSequence != 3 || len(accepted.retained) != 3 {
			t.Fatalf("accepted query owner=%+v", accepted)
		}
	})

	t.Run("list-only", func(t *testing.T) {
		recorder := uiRecorder(t)
		visual := &diagnostics.VisualState{Screen: uiID(t, "search"), State: uiID(t, "ready")}
		for index := range 3 {
			addDetailedUIEvent(t, recorder, "plugin-a", "results", "open", fmt.Sprintf("event-%d", index), "", diagnostics.LevelInfo, diagnostics.KindInteraction, visual)
		}
		page, err := recorder.Debug(diagnostics.DebugQuery{})
		if err != nil {
			t.Fatal(err)
		}
		previews, err := diagnostics.OpenPreviewStore(t.TempDir(), diagnostics.DefaultPreviewLimits())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = previews.Close() })
		for _, event := range page.Events {
			if _, err := previews.Put(event.Sequence, *event.Visual); err != nil {
				t.Fatal(err)
			}
		}
		harness := newAsyncProgramHarness(t, Options{Recorder: recorder, Previews: previews, PageSize: 10})
		harness.load()
		harness.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
		harness.await(func(observation asyncProjectionObservation) bool {
			result, ok := observation.event.(shell.ResultEvent)
			return ok && result.Key == projectionRequest && observation.list == screenGallery
		})
		harness.program.Send(tea.KeyMsg{Type: tea.KeyEnd})
		harness.await(func(observation asyncProjectionObservation) bool {
			_, ok := observation.event.(shell.KeyEvent)
			return ok && observation.selectedSequence == 1 && observation.topSequence == 3
		})
		harness.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
		harness.await(func(observation asyncProjectionObservation) bool {
			result, ok := observation.event.(shell.ResultEvent)
			return ok && result.Key == projectionRequest && observation.list == screenTimeline
		})
		entered, release := make(chan struct{}, 1), make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		harness.observer.overrideNext(asyncProjectionWorkOverride{entered: entered, release: release, result: successfulProjectionWork(recorder, previews)})
		harness.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("gallery selection did not enter hold")
		}
		harness.program.Send(tea.KeyMsg{Type: tea.KeyHome})
		during := harness.await(func(observation asyncProjectionObservation) bool {
			_, ok := observation.event.(shell.KeyEvent)
			return ok
		})
		if during.anchorSelected != 1 || during.topSequence != 3 {
			t.Fatalf("list transition changed gallery anchor=%+v", during)
		}
		releaseOnce.Do(func() { close(release) })
		accepted := harness.await(func(observation asyncProjectionObservation) bool {
			result, ok := observation.event.(shell.ResultEvent)
			return ok && result.Key == projectionRequest && observation.list == screenGallery
		})
		if accepted.selectedSequence != 1 || accepted.topSequence != 3 || accepted.galleryCount != 3 {
			t.Fatalf("accepted gallery owner=%+v", accepted)
		}
	})
}
