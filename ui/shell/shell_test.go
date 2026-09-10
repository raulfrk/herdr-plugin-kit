package shell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

type memorySink struct {
	mu     sync.Mutex
	events []diagnostics.SemanticEvent
}

type discardSink struct{}

func (discardSink) RecordSemantic(diagnostics.SemanticEvent) error { return nil }

type failingSink struct{ calls int }

func (sink *failingSink) RecordSemantic(diagnostics.SemanticEvent) error {
	sink.calls++
	return errors.New("record failed")
}

type mutatingTextFitSink struct {
	calls  int
	events []diagnostics.SemanticEvent
}

func (sink *mutatingTextFitSink) RecordSemantic(event diagnostics.SemanticEvent) error {
	sink.calls++
	captured := event
	captured.TextFit = diagnostics.CloneTextFit(event.TextFit)
	sink.events = append(sink.events, captured)
	if event.TextFit != nil && len(event.TextFit.Observations) > 0 {
		event.TextFit.Observations[0].Instance = 999
	}
	return nil
}

func (sink *memorySink) RecordSemantic(event diagnostics.SemanticEvent) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.events = append(sink.events, event)
	return nil
}

func (sink *memorySink) snapshot() []diagnostics.SemanticEvent {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]diagnostics.SemanticEvent(nil), sink.events...)
}

type testSurface struct {
	layout  responsive.Layout
	state   diagnostics.ID
	results int
	texts   []TextEvent
	values  []any
}

type probeSurface struct {
	*testSurface
	effects  func(Event) []Effect
	render   func(RenderContext) (*view.Frame, error)
	events   []Event
	contexts []RenderContext
}

type filteredProbeSurface struct {
	*probeSurface
	timer   EventCode
	request RequestKey
}

func (surface *filteredProbeSurface) SuppressEventDiagnostics(event Event) bool {
	switch event := event.(type) {
	case TimerEvent:
		return event.Code == surface.timer
	case ResultEvent:
		return event.Key == surface.request
	default:
		return false
	}
}

func (surface *filteredProbeSurface) SuppressEffectDiagnostics(effect Effect) bool {
	return effect.RequestKey() == surface.request || effect.EventCode() == surface.timer
}

func (surface *probeSurface) Update(context EventContext, event Event) []Effect {
	surface.events = append(surface.events, event)
	surface.testSurface.Update(context, event)
	if surface.effects != nil {
		return surface.effects(event)
	}
	return nil
}

func (surface *probeSurface) Render(context RenderContext) (*view.Frame, error) {
	surface.contexts = append(surface.contexts, context)
	if surface.render != nil {
		return surface.render(context)
	}
	return view.NewFrame(context.Layout.Render.Columns, context.Layout.Render.Rows)
}

func newTestSurface(t *testing.T) *testSurface {
	return &testSurface{state: testID(t, "ready")}
}

func (surface *testSurface) Update(_ EventContext, event Event) []Effect {
	switch event := event.(type) {
	case ResizeEvent:
		surface.layout = event.Layout
	case ResultEvent:
		surface.results++
		surface.values = append(surface.values, event.Result.Value)
	case TextEvent:
		surface.texts = append(surface.texts, event)
	}
	return nil
}

func (surface *testSurface) Render(RenderContext) (*view.Frame, error) {
	return view.NewFrame(surface.layout.Render.Columns, surface.layout.Render.Rows)
}

func (surface *testSurface) DiagnosticState() diagnostics.VisualState {
	return diagnostics.VisualState{
		State: surface.state,
		Geometry: diagnostics.Geometry{
			ReportedColumns: surface.layout.Reported.Columns, ReportedRows: surface.layout.Reported.Rows,
			RenderColumns: surface.layout.Render.Columns, RenderRows: surface.layout.Render.Rows,
		},
	}
}

func testModel(t *testing.T) (*model, *testSurface, *memorySink) {
	t.Helper()
	palette, err := theme.Builtin("terminal")
	if err != nil {
		t.Fatal(err)
	}
	sink := &memorySink{}
	surface := newTestSurface(t)
	return newModel(ProgramOptions{PluginID: testID(t, "test-plugin"), Theme: palette, Events: sink}, surface), surface, sink
}

func testID(t *testing.T, value string) diagnostics.ID {
	t.Helper()
	id, err := diagnostics.NewID(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestResizeLatestGenerationSettlesAndPreservesExactDimensions(t *testing.T) {
	model, surface, sink := testModel(t)
	probe := &probeSurface{testSurface: surface}
	model.surface = probe
	for _, size := range []tea.WindowSizeMsg{{Width: 70, Height: 30}, {Width: 70, Height: 10}, {Width: 70, Height: 30}} {
		model.Update(size)
	}
	if model.resizeGeneration != 3 || model.layout.Reported != (responsive.Size{Columns: 70, Rows: 30}) {
		t.Fatalf("latest resize = generation %d layout %#v", model.resizeGeneration, model.layout)
	}
	if surface.layout.Render != (responsive.Size{Columns: 70, Rows: 30}) {
		t.Fatalf("surface layout = %#v", surface.layout)
	}
	if len(probe.contexts) != 3 || probe.contexts[2].Layout.Reported != (responsive.Size{Columns: 70, Rows: 30}) ||
		probe.contexts[2].Layout.Render != (responsive.Size{Columns: 70, Rows: 30}) {
		t.Fatalf("burst render contexts = %#v", probe.contexts)
	}
	model.Update(settleMessage{generation: 2, startedAt: time.Now()})
	if model.settled {
		t.Fatal("stale settle changed state")
	}
	if len(probe.contexts) != 3 {
		t.Fatalf("stale settle rendered %d frames, want 3", len(probe.contexts))
	}
	model.Update(settleMessage{generation: 3, startedAt: time.Now()})
	if !model.settled {
		t.Fatal("current settle did not change state")
	}
	if len(probe.contexts) != 4 || !probe.contexts[3].Settled ||
		probe.contexts[3].Layout.Reported != (responsive.Size{Columns: 70, Rows: 30}) {
		t.Fatalf("final settled render = %#v", probe.contexts)
	}
	if len(sink.snapshot()) == 0 {
		t.Fatal("resize produced no diagnostics")
	}
}

func TestUnicodeTextReachesSurfaceButNotPersistedDiagnostics(t *testing.T) {
	directory := t.TempDir()
	recorder, err := diagnostics.Open(diagnostics.DefaultConfig(directory))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	palette, _ := theme.Builtin("terminal")
	surface := newTestSurface(t)
	model := newModel(ProgramOptions{PluginID: testID(t, "test-plugin"), Theme: palette, Events: recorder}, surface)
	model.Update(tea.WindowSizeMsg{Width: 70, Height: 10})
	canary := "secret-slug_日本語_e\u0301_🙂_/private/path"
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(canary), Paste: true})
	if len(surface.texts) != 1 || surface.texts[0].Text != canary || !surface.texts[0].Paste {
		t.Fatalf("surface text delivery = %#v", surface.texts)
	}
	logData, err := readCanonicalLog(directory)
	if err != nil {
		t.Fatal(err)
	}
	report, err := recorder.Export(diagnostics.ReportOptions{EmbedSnapshots: true})
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"log": logData, "report": report} {
		if bytes.Contains(data, []byte(canary)) {
			t.Fatalf("%s contains text payload", name)
		}
	}
	var input diagnostics.Event
	for _, line := range bytes.Split(logData, []byte{'\n'}) {
		var event diagnostics.Event
		if json.Unmarshal(line, &event) == nil && event.Message == "input.text" {
			input = event
		}
	}
	wantGraphemes := utf8.RuneCountInString(canary) - 1 // e plus its combining accent form one cluster.
	if input.Details["bytes"] != float64(len(canary)) || input.Details["graphemes"] != float64(wantGraphemes) {
		t.Fatalf("persisted input measurements = %#v, want bytes %d graphemes %d", input.Details, len(canary), wantGraphemes)
	}
}

// Inspect canonical on-disk bytes so privacy checks do not rely on Export's
// separate sanitization. os.ReadDir provides lexical segment order.
func canonicalLogFiles(directory string) ([]string, error) {
	live := filepath.Join(directory, "events-v1")
	entries, err := os.ReadDir(live)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != 33 || !strings.HasPrefix(name, "events-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		ordinal, err := strconv.ParseUint(name[7:27], 10, 64)
		if err != nil || ordinal == 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("canonical log is not a regular file")
		}
		paths = append(paths, filepath.Join(live, name))
	}
	return paths, nil
}

func readCanonicalLog(directory string) ([]byte, error) {
	paths, err := canonicalLogFiles(directory)
	if err != nil {
		return nil, err
	}
	var data []byte
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		data = append(data, b...)
	}
	return data, nil
}

func TestReplacementCancelsWorkAndOnlyLatestResultReachesSurface(t *testing.T) {
	model, surface, sink := testModel(t)
	key, err := NewRequestKey("search")
	if err != nil {
		t.Fatal(err)
	}
	firstStarted := make(chan struct{})
	firstCancelled := make(chan struct{})
	releaseFirst := make(chan struct{})
	first, err := model.eventContext().Start(key, func(ctx context.Context) WorkResult {
		close(firstStarted)
		<-ctx.Done()
		close(firstCancelled)
		<-releaseFirst
		return WorkResult{Value: "old", Code: diagnostics.OutcomeCancelled}
	})
	if err != nil {
		t.Fatal(err)
	}
	firstCommand := model.commands([]Effect{first})[0]
	firstResult := make(chan tea.Msg, 1)
	go func() { firstResult <- firstCommand() }()
	<-firstStarted

	second, err := model.eventContext().Start(key, func(context.Context) WorkResult {
		return WorkResult{Value: "new", Code: diagnostics.OutcomeApplied}
	})
	if err != nil {
		t.Fatal(err)
	}
	secondCommand := model.commands([]Effect{second})[0]
	select {
	case <-firstCancelled:
	case <-time.After(time.Second):
		t.Fatal("replaced work did not observe cancellation")
	}
	model.Update(secondCommand())
	close(releaseFirst)
	model.Update(<-firstResult)
	if surface.results != 1 || len(surface.values) != 1 || surface.values[0] != "new" {
		t.Fatalf("delivered results = count %d values %#v", surface.results, surface.values)
	}
	foundStale := false
	for _, event := range sink.snapshot() {
		if event.Code.String() == "request.completed" && event.Outcome == diagnostics.OutcomeStale {
			foundStale = true
		}
	}
	if !foundStale {
		t.Fatal("stale result was not diagnosed")
	}
	foundLatest := false
	for _, event := range sink.snapshot() {
		if event.Code.String() == "request.completed" && event.Outcome == diagnostics.OutcomeApplied &&
			event.Action.String() == "search" && event.Correlation == second.correlation {
			foundLatest = true
		}
	}
	if !foundLatest {
		t.Fatal("latest result correlation was not diagnosed")
	}
}

func TestRequestDiagnosticsPreserveTypedOutcome(t *testing.T) {
	for _, test := range []struct {
		name      string
		code      diagnostics.OutcomeCode
		err       error
		want      diagnostics.OutcomeCode
		delivered int
	}{
		{name: "applied", code: diagnostics.OutcomeApplied, want: diagnostics.OutcomeApplied, delivered: 1},
		{name: "ignored", code: diagnostics.OutcomeIgnored, want: diagnostics.OutcomeIgnored, delivered: 1},
		{name: "rejected", code: diagnostics.OutcomeRejected, want: diagnostics.OutcomeRejected, delivered: 1},
		{name: "stale work outcome", code: diagnostics.OutcomeStale, want: diagnostics.OutcomeStale, delivered: 1},
		{name: "cancelled", code: diagnostics.OutcomeCancelled, want: diagnostics.OutcomeCancelled, delivered: 1},
		{name: "failed code", code: diagnostics.OutcomeFailed, want: diagnostics.OutcomeFailed, delivered: 1},
		{name: "error overrides code", code: diagnostics.OutcomeApplied, err: errors.New("bounded failure"), want: diagnostics.OutcomeFailed, delivered: 1},
		{name: "invalid code", code: diagnostics.OutcomeCode("invalid"), want: diagnostics.OutcomeRejected, delivered: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			model, surface, sink := testModel(t)
			key, _ := NewRequestKey("search")
			model.requestGenerations[key] = 1
			model.requestCorrelations[key] = testID(t, "c-1")
			model.requestCancels[key] = func() {}
			model.Update(resultMessage{ResultEvent{Key: key, Generation: 1, Result: WorkResult{Code: test.code, Err: test.err}}})
			if surface.results != test.delivered {
				t.Fatalf("surface results = %d, want %d", surface.results, test.delivered)
			}
			if _, exists := model.requestCorrelations[key]; exists {
				t.Fatal("completed request retained its correlation")
			}
			if _, exists := model.requestCancels[key]; exists {
				t.Fatal("completed request retained its cancel handle")
			}
			var got diagnostics.SemanticEvent
			for _, event := range sink.snapshot() {
				if event.Code.String() == "request.completed" {
					got = event
				}
			}
			if got.Code.IsZero() || got.Outcome != test.want {
				t.Fatalf("request diagnostic = %#v, want outcome %q", got, test.want)
			}
		})
	}
}

func TestTimerAndSettleDiagnosticsCarryOriginatingGeneration(t *testing.T) {
	model, _, sink := testModel(t)
	code, _ := NewEventCode("refresh")
	effect, _ := model.eventContext().After(time.Second, code)
	model.commands([]Effect{effect})
	if _, err := model.eventContext().After(time.Second, code); err != nil {
		t.Fatal(err)
	}
	model.Update(timerMessage{TimerEvent{Code: code, Generation: effect.generation}})
	model.resizeGeneration = 4
	settleStarted := time.Now().Add(-time.Second)
	settleDurationLower := time.Since(settleStarted)
	model.Update(settleMessage{generation: 3, startedAt: settleStarted})
	settleDurationUpper := time.Since(settleStarted)
	events := sink.snapshot()
	assert := func(eventCode, action string, generation uint64, outcome diagnostics.OutcomeCode, duration time.Duration) {
		t.Helper()
		for _, event := range events {
			if event.Code.String() == eventCode && event.Action.String() == action &&
				event.RelatedGeneration == generation && event.Outcome == outcome && event.Duration == duration {
				return
			}
		}
		t.Fatalf("missing %s/%s generation %d outcome %s duration %s in %#v", eventCode, action, generation, outcome, duration, events)
	}
	assert("timer.scheduled", "refresh", 1, diagnostics.OutcomeApplied, time.Second)
	assert("timer.fired", "refresh", 1, diagnostics.OutcomeStale, 0)
	settleFound := false
	for _, event := range events {
		if event.Code.String() == "resize.settled" && event.Action.String() == "resize.settle" &&
			event.RelatedGeneration == 3 && event.Outcome == diagnostics.OutcomeStale {
			settleFound = event.Duration >= settleDurationLower && event.Duration <= settleDurationUpper
		}
	}
	if !settleFound {
		t.Fatalf("missing stale settle with monotonic duration in %#v", events)
	}
}

func TestDiagnosticFilterSuppressesMaintenanceEventAndWorkLifecycle(t *testing.T) {
	model, base, sink := testModel(t)
	timer, _ := NewEventCode("debug.maintenance")
	request, _ := NewRequestKey("debug.snapshot")
	probe := &filteredProbeSurface{probeSurface: &probeSurface{testSurface: base}, timer: timer, request: request}
	probe.effects = func(event Event) []Effect {
		if fired, ok := event.(TimerEvent); ok && fired.Code == timer {
			effect, err := model.eventContext().Start(request, func(context.Context) WorkResult {
				return WorkResult{Code: diagnostics.OutcomeApplied}
			})
			if err != nil {
				t.Fatal(err)
			}
			return []Effect{effect}
		}
		return nil
	}
	model.surface = probe
	model.timerGenerations[timer] = 1
	_, command := model.Update(timerMessage{TimerEvent{Code: timer, Generation: 1}})
	if command == nil {
		t.Fatal("suppressed maintenance work was not scheduled")
	}
	message := command()
	if batch, ok := message.(tea.BatchMsg); ok {
		if len(batch) != 1 {
			t.Fatalf("maintenance batch has %d commands", len(batch))
		}
		message = batch[0]()
	}
	model.Update(message)
	if events := sink.snapshot(); len(events) != 0 {
		t.Fatalf("suppressed maintenance produced diagnostics: %+v", events)
	}
	if base.results != 1 {
		t.Fatalf("suppressed work result deliveries = %d", base.results)
	}
}

func TestKeyAndTextDiagnosticsPreserveSafeModifiers(t *testing.T) {
	model, _, sink := testModel(t)
	model.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("private"), Paste: true, Alt: true})
	events := sink.snapshot()
	assert := func(code, action string, alt, paste bool) {
		t.Helper()
		for _, event := range events {
			if event.Code.String() == code && event.Action.String() == action && event.Alt == alt && event.Paste == paste {
				return
			}
		}
		t.Fatalf("missing input diagnostic %s/%s alt=%v paste=%v in %#v", code, action, alt, paste, events)
	}
	assert("input.key", "enter", true, false)
	assert("input.text", "", true, true)
}

func TestEffectValidationAndTimerGeneration(t *testing.T) {
	model, _, _ := testModel(t)
	eventContext := model.eventContext()
	if _, err := eventContext.Start(RequestKey{}, func(context.Context) WorkResult { return WorkResult{} }); err == nil {
		t.Fatal("empty request key accepted")
	}
	if _, err := eventContext.After(0, EventCode{}); err == nil {
		t.Fatal("invalid timer accepted")
	}
	code, err := NewEventCode("refresh")
	if err != nil {
		t.Fatal(err)
	}
	first, _ := eventContext.After(time.Second, code)
	second, _ := eventContext.After(time.Second, code)
	if first.generation != 1 || second.generation != 2 {
		t.Fatalf("timer generations = %d, %d", first.generation, second.generation)
	}
}

func TestEffectKindsAndContextValidation(t *testing.T) {
	if _, err := NewRequestKey(""); err == nil {
		t.Fatal("empty request key accepted")
	}
	if _, err := NewEventCode(""); err == nil {
		t.Fatal("empty event code accepted")
	}
	if _, err := (EventContext{}).Start(RequestKey{}, func(context.Context) WorkResult { return WorkResult{} }); err == nil {
		t.Fatal("zero event context started work")
	}
	if _, err := (EventContext{}).After(time.Second, EventCode{}); err == nil {
		t.Fatal("zero event context scheduled timer")
	}

	model, _, _ := testModel(t)
	key, _ := NewRequestKey("search")
	code, _ := NewEventCode("refresh")
	context := model.eventContext()
	if _, err := context.Start(key, nil); err == nil {
		t.Fatal("nil work accepted")
	}
	for _, delay := range []time.Duration{0, -time.Nanosecond} {
		if _, err := context.After(delay, code); err == nil {
			t.Fatalf("timer delay %s accepted", delay)
		}
	}
	if _, err := context.After(time.Nanosecond, EventCode{}); err == nil {
		t.Fatal("empty timer event code accepted")
	}
}

func TestRequestEffectsIncrementGenerationAndCorrelation(t *testing.T) {
	model, _, _ := testModel(t)
	key, _ := NewRequestKey("search")
	work := func(context.Context) WorkResult { return WorkResult{Code: diagnostics.OutcomeApplied} }
	first, err := model.eventContext().Start(key, work)
	if err != nil {
		t.Fatal(err)
	}
	second, err := model.eventContext().Start(key, work)
	if err != nil {
		t.Fatal(err)
	}
	if first.generation != 1 || second.generation != 2 {
		t.Fatalf("request generations = %d, %d", first.generation, second.generation)
	}
	if first.correlation.String() != "c-1" || second.correlation.String() != "c-2" ||
		second.previousCorrelation != first.correlation {
		t.Fatalf("request correlations = %q, %q previous %q", first.correlation, second.correlation, second.previousCorrelation)
	}
}

func TestCommandsPreservePayloadsBatchQuitAndCancellationGeneration(t *testing.T) {
	model, _, sink := testModel(t)
	key, _ := NewRequestKey("search")
	code, _ := NewEventCode("refresh")
	work := func(context.Context) WorkResult { return WorkResult{Value: "done", Code: diagnostics.OutcomeApplied} }
	first, _ := model.eventContext().Start(key, work)
	second, _ := model.eventContext().Start(key, work)
	firstCommand := model.commands([]Effect{first})[0]
	secondCommand := model.commands([]Effect{second})[0]
	firstResult := firstCommand().(resultMessage)
	secondResult := secondCommand().(resultMessage)
	if firstResult.Generation != 1 || secondResult.Generation != 2 || secondResult.Result.Value != "done" {
		t.Fatalf("work command results = %#v, %#v", firstResult, secondResult)
	}

	var cancelled diagnostics.SemanticEvent
	for _, event := range sink.snapshot() {
		if event.Code.String() == "request.cancelled" {
			cancelled = event
		}
	}
	if cancelled.RelatedGeneration != 1 || cancelled.Correlation != first.correlation {
		t.Fatalf("cancellation diagnostic = %#v", cancelled)
	}

	timer, _ := model.eventContext().After(time.Nanosecond, code)
	timerResult := model.commands([]Effect{timer})[0]().(timerMessage)
	if timerResult.Code != code || timerResult.Generation != 1 {
		t.Fatalf("timer command = %#v", timerResult)
	}

	cancelledByQuit := false
	model.requestCancels[key] = func() { cancelledByQuit = true }
	quitCommand := model.commands([]Effect{Quit()})[0]
	quitMessage := quitCommand()
	if _, ok := quitMessage.(tea.QuitMsg); !ok || !cancelledByQuit {
		t.Fatalf("quit command type/cancellation = %T/%v", quitMessage, cancelledByQuit)
	}
}

func TestUnsupportedMessageIsDiagnosedWithoutSurfaceEvent(t *testing.T) {
	model, surface, sink := testModel(t)
	before := surface.results
	model.Update(struct{ Secret string }{Secret: "ordinary-private-canary"})
	if surface.results != before {
		t.Fatal("unsupported message reached surface")
	}
	events := sink.snapshot()
	if len(events) == 0 || events[len(events)-1].Code.String() != "event.unsupported" ||
		events[len(events)-1].Outcome != diagnostics.OutcomeRejected {
		t.Fatalf("unsupported diagnostics = %#v", events)
	}
}

func TestUnsupportedInputDoesNotUpdateOrRenderSurface(t *testing.T) {
	baseModel, base, sink := testModel(t)
	surface := &probeSurface{testSurface: base}
	baseModel.surface = surface

	_, command := baseModel.Update(tea.KeyMsg{Type: tea.KeyF1})
	if command != nil || len(surface.events) != 0 || len(surface.contexts) != 0 {
		t.Fatalf("unsupported key command/events/renders = %v/%d/%d", command != nil, len(surface.events), len(surface.contexts))
	}
	events := sink.snapshot()
	if got := events[len(events)-1]; got.Code.String() != "input.unsupported" || got.Outcome != diagnostics.OutcomeRejected {
		t.Fatalf("unsupported key diagnostic = %#v", got)
	}
}

func TestUpdateReturnsResizeCommandAndBatchesSurfaceCommands(t *testing.T) {
	model, base, _ := testModel(t)
	surface := &probeSurface{testSurface: base}
	model.surface = surface
	if resizeSettleDelay != 100*time.Millisecond {
		t.Fatalf("resize settle delay = %s", resizeSettleDelay)
	}
	_, resizeCommand := model.Update(tea.WindowSizeMsg{Width: 70, Height: 30})
	if resizeCommand == nil {
		t.Fatal("resize returned no settle command")
	}

	surface.effects = func(Event) []Effect { return []Effect{Quit(), Quit()} }
	_, command := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	message := command()
	batch, ok := message.(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("surface command batch = %T %#v", message, message)
	}
}

func TestNewProgramRejectsIncompleteOptions(t *testing.T) {
	palette, _ := theme.Builtin("terminal")
	surface := newTestSurface(t)
	if _, err := NewProgram(ProgramOptions{Theme: palette, Events: &memorySink{}}, surface); err == nil {
		t.Fatal("empty plugin ID accepted")
	}
	if _, err := NewProgram(ProgramOptions{PluginID: testID(t, "sample"), Theme: palette}, surface); err == nil {
		t.Fatal("nil sink accepted")
	}
	if _, err := NewProgram(ProgramOptions{PluginID: testID(t, "sample"), Events: &memorySink{}}, surface); err == nil {
		t.Fatal("empty theme accepted")
	}
	if _, err := NewProgram(ProgramOptions{PluginID: testID(t, "sample"), Theme: palette, Events: &memorySink{}}, nil); err == nil {
		t.Fatal("nil surface accepted")
	}
}

func TestNewProgramForwardsInputAndOutput(t *testing.T) {
	palette, _ := theme.Builtin("terminal")
	input := bytes.NewBufferString("x")
	var output bytes.Buffer
	base := newTestSurface(t)
	surface := &probeSurface{testSurface: base}
	surface.effects = func(event Event) []Effect {
		if _, ok := event.(TextEvent); ok {
			return []Effect{Quit()}
		}
		return nil
	}
	program, err := NewProgram(ProgramOptions{
		PluginID: testID(t, "sample"), Theme: palette, Events: &memorySink{}, Input: input, Output: &output,
	}, surface)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := program.Run(); err != nil {
		t.Fatal(err)
	}
	if len(base.texts) != 1 || base.texts[0].Text != "x" {
		t.Fatalf("forwarded input = %#v", base.texts)
	}
	if output.Len() == 0 {
		t.Fatal("forwarded output is empty")
	}
}

type invalidFrameSurface struct {
	*testSurface
	nilFrame bool
}

func (surface invalidFrameSurface) Render(RenderContext) (*view.Frame, error) {
	if surface.nilFrame {
		return nil, nil
	}
	return view.NewFrame(1, 1)
}

func TestInvalidSurfaceFrameRecoversAndReportsFailure(t *testing.T) {
	for _, nilFrame := range []bool{true, false} {
		model, surface, sink := testModel(t)
		model.surface = invalidFrameSurface{testSurface: surface, nilFrame: nilFrame}
		model.Update(tea.WindowSizeMsg{Width: 70, Height: 30})
		if model.View() == "" {
			t.Fatal("recovery view is empty")
		}
		found := false
		for _, event := range sink.snapshot() {
			if event.Code.String() == "render.completed" && event.Outcome == diagnostics.OutcomeFailed {
				found = true
			}
		}
		if !found {
			t.Fatalf("render contract failure was not diagnosed: %#v", sink.snapshot())
		}
	}
}

func TestTranslateKeySupportsCompleteShellKeySet(t *testing.T) {
	tests := []struct {
		input tea.KeyType
		want  KeyCode
	}{
		{tea.KeyEnter, KeyEnter},
		{tea.KeyBackspace, KeyBackspace},
		{tea.KeyTab, KeyTab},
		{tea.KeyEsc, KeyEscape},
		{tea.KeyUp, KeyUp},
		{tea.KeyDown, KeyDown},
		{tea.KeyLeft, KeyLeft},
		{tea.KeyRight, KeyRight},
		{tea.KeyHome, KeyHome},
		{tea.KeyEnd, KeyEnd},
		{tea.KeyPgUp, KeyPageUp},
		{tea.KeyPgDown, KeyPageDown},
		{tea.KeyDelete, KeyDelete},
		{tea.KeySpace, KeySpace},
		{tea.KeyCtrlC, KeyCtrlC},
	}
	for _, test := range tests {
		t.Run(string(test.want), func(t *testing.T) {
			got, ok := translateKey(tea.KeyMsg{Type: test.input})
			if !ok || got != test.want {
				t.Fatalf("translation = %q, %v", got, ok)
			}
		})
	}
	if got, ok := translateKey(tea.KeyMsg{Type: tea.KeyF1}); ok || got != "" {
		t.Fatalf("unsupported key translation = %q, %v", got, ok)
	}
}

func TestRenderPassesContextAndPreservesValidSurfaceFrame(t *testing.T) {
	model, base, sink := testModel(t)
	model.layout = responsive.Resolve(responsive.Size{Columns: 70, Rows: 30})
	model.focused = true
	model.settled = true
	model.resizeGeneration = 7
	surface := &probeSurface{testSurface: base}
	surface.render = func(context RenderContext) (*view.Frame, error) {
		frame, err := view.NewFrame(context.Layout.Render.Columns, context.Layout.Render.Rows)
		if err == nil {
			frame.PutText(0, 0, "surface", view.Style{})
		}
		return frame, err
	}
	model.surface = surface
	model.render(time.Now())

	if len(surface.contexts) != 1 {
		t.Fatalf("render contexts = %d", len(surface.contexts))
	}
	context := surface.contexts[0]
	if context.Layout != model.layout || context.Theme.ID != model.options.Theme.ID || !context.Focused ||
		!context.Settled || context.ResizeGeneration != 7 {
		t.Fatalf("render context = %#v", context)
	}
	if !strings.Contains(model.View(), "surface") {
		t.Fatalf("valid surface frame replaced: %q", model.View())
	}
	events := sink.snapshot()
	if got := events[len(events)-1]; got.Code.String() != "render.completed" || got.Outcome != diagnostics.OutcomeApplied {
		t.Fatalf("valid render diagnostic = %#v", got)
	}
}

func TestRenderContractFailuresRecoverIndependently(t *testing.T) {
	discardedID := testID(t, "discarded-contract-frame")
	instrument := func(frame *view.Frame) *view.Frame {
		_ = frame.PutTextBox(view.TextBoxOptions{Element: discardedID, Width: 1, Height: 1, Mode: view.TextClip}, []string{"x"}, view.Style{})
		return frame
	}
	tests := []struct {
		name        string
		render      func(RenderContext) (*view.Frame, error)
		wantMessage string
	}{
		{
			name: "error with exact frame",
			render: func(context RenderContext) (*view.Frame, error) {
				frame, _ := view.NewFrame(context.Layout.Render.Columns, context.Layout.Render.Rows)
				return instrument(frame), errors.New("private render detail")
			},
			wantMessage: "UI unavailable",
		},
		{name: "nil frame", render: func(RenderContext) (*view.Frame, error) { return nil, nil }, wantMessage: "Window 70x30"},
		{
			name: "wrong width only",
			render: func(context RenderContext) (*view.Frame, error) {
				frame, err := view.NewFrame(context.Layout.Render.Columns-1, context.Layout.Render.Rows)
				return instrument(frame), err
			},
			wantMessage: "Window 70x30",
		},
		{
			name: "wrong height only",
			render: func(context RenderContext) (*view.Frame, error) {
				frame, err := view.NewFrame(context.Layout.Render.Columns, context.Layout.Render.Rows-1)
				return instrument(frame), err
			},
			wantMessage: "Window 70x30",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model, base, sink := testModel(t)
			model.layout = responsive.Resolve(responsive.Size{Columns: 70, Rows: 30})
			model.surface = &probeSurface{testSurface: base, render: test.render}
			model.render(time.Now())
			if !strings.Contains(model.View(), test.wantMessage) {
				t.Fatalf("recovery view = %q, want %q", model.View(), test.wantMessage)
			}
			events := sink.snapshot()
			if got := events[len(events)-1]; got.Outcome != diagnostics.OutcomeFailed {
				t.Fatalf("contract failure diagnostic = %#v", got)
			} else if got.TextFit == nil || len(got.TextFit.Observations) != 2 || got.TextFit.Observations[0].Element == discardedID || got.TextFit.Observations[1].Element == discardedID {
				t.Fatalf("contract failure text fit = %+v", got.TextFit)
			}
		})
	}
}

func TestRecoveryAndProjectedRendering(t *testing.T) {
	t.Run("recovery replaces valid surface", func(t *testing.T) {
		model, base, sink := testModel(t)
		model.layout = responsive.Resolve(responsive.Size{Columns: 39, Rows: 9})
		surface := &probeSurface{testSurface: base}
		surface.render = func(context RenderContext) (*view.Frame, error) {
			frame, _ := view.NewFrame(context.Layout.Render.Columns, context.Layout.Render.Rows)
			frame.PutText(0, 0, "surface", view.Style{})
			return frame, nil
		}
		model.surface = surface
		model.render(time.Now())
		if !strings.Contains(model.View(), "Window 39x9") || !strings.Contains(model.View(), "expand to at least 40x10") ||
			strings.Contains(model.View(), "surface") {
			t.Fatalf("recovery view = %q", model.View())
		}
		events := sink.snapshot()
		if got := events[len(events)-1]; got.Outcome != diagnostics.OutcomeApplied {
			t.Fatalf("valid recovery render diagnostic = %#v", got)
		}
	})

	t.Run("projected frame clears screen", func(t *testing.T) {
		model, base, _ := testModel(t)
		model.layout = responsive.Resolve(responsive.Size{Columns: 501, Rows: 201})
		model.surface = &probeSurface{testSurface: base}
		model.render(time.Now())
		if !strings.HasPrefix(model.View(), "\x1b[2J\x1b[H") {
			t.Fatalf("projected render prefix = %q", model.View()[:min(len(model.View()), 20)])
		}
	})
}

func TestRenderCompletedAttachesOnlySelectedFrameTextFit(t *testing.T) {
	model, base, _ := testModel(t)
	sink := &mutatingTextFitSink{}
	model.options.Events = sink
	model.layout = responsive.Resolve(responsive.Size{Columns: 70, Rows: 30})
	instrumented := true
	model.surface = &probeSurface{testSurface: base, render: func(context RenderContext) (*view.Frame, error) {
		frame, err := view.NewFrame(context.Layout.Render.Columns, context.Layout.Render.Rows)
		if err != nil {
			return nil, err
		}
		if instrumented {
			err = frame.PutTextBox(view.TextBoxOptions{Element: testID(t, "surface-title"), Instance: 4, Width: 3, Height: 1, Mode: view.TextClip}, []string{"title"}, view.Style{})
		}
		return frame, err
	}}

	model.render(time.Now())
	if sink.calls != 1 || len(sink.events) != 1 {
		t.Fatalf("render diagnostics calls = %d events = %d", sink.calls, len(sink.events))
	}
	first := sink.events[0]
	if first.Code.String() != "render.completed" || first.Outcome != diagnostics.OutcomeApplied || first.TextFit == nil || len(first.TextFit.Observations) != 1 {
		t.Fatalf("instrumented render event = %+v", first)
	}
	observation := first.TextFit.Observations[0]
	if observation.Element.String() != "surface-title" || observation.Instance != 4 || !observation.Clipped {
		t.Fatalf("instrumented text fit = %+v", observation)
	}

	instrumented = false
	model.render(time.Now())
	if sink.calls != 2 || sink.events[1].TextFit != nil {
		t.Fatalf("uninstrumented render event = %+v, calls = %d", sink.events[1], sink.calls)
	}
	if first.TextFit.Observations[0].Instance != 4 {
		t.Fatal("sink mutation changed an admitted or later text-fit report")
	}
}

func TestRecoveryAttachesActualFrameTextFitAndPreservesRendering(t *testing.T) {
	model, base, sink := testModel(t)
	model.layout = responsive.Resolve(responsive.Size{Columns: 39, Rows: 9})
	model.surface = &probeSurface{testSurface: base, render: func(context RenderContext) (*view.Frame, error) {
		frame, err := view.NewFrame(context.Layout.Render.Columns, context.Layout.Render.Rows)
		if err != nil {
			return nil, err
		}
		_ = frame.PutTextBox(view.TextBoxOptions{Element: testID(t, "discarded-frame"), Width: 1, Height: 1, Mode: view.TextClip}, []string{"x"}, view.Style{})
		return frame, nil
	}}
	model.render(time.Now())

	events := sink.snapshot()
	got := events[len(events)-1]
	if got.Outcome != diagnostics.OutcomeApplied || got.TextFit == nil || len(got.TextFit.Observations) != 2 {
		t.Fatalf("recovery render event = %+v", got)
	}
	if got.TextFit.Observations[0].Element.String() != "recovery-message" || got.TextFit.Observations[1].Element.String() != "recovery-help" {
		t.Fatalf("recovery text-fit IDs = %+v", got.TextFit.Observations)
	}
	for _, observation := range got.TextFit.Observations {
		if observation.Element.String() == "discarded-frame" {
			t.Fatal("discarded surface frame metadata leaked into recovery")
		}
	}

	expected, _ := view.NewFrame(39, 9)
	style := view.Style{Foreground: model.options.Theme.Text, Background: model.options.Theme.Background}
	expected.Fill(0, 0, 39, 9, style)
	expected.PutText(0, 0, "Window 39x9 · expand to at least 40x10", style)
	expected.PutText(0, 8, "q quit · d debug", style)
	if want := view.ANSI(expected); model.View() != want {
		t.Fatalf("instrumented recovery rendering changed\n got: %q\nwant: %q", model.View(), want)
	}
}

func TestRecoveryFrameClampsAndFooterRows(t *testing.T) {
	model, _, _ := testModel(t)
	for _, test := range []struct {
		name       string
		render     responsive.Size
		wantWidth  int
		wantHeight int
	}{
		{name: "negative width", render: responsive.Size{Columns: -1, Rows: 2}, wantWidth: 1, wantHeight: 2},
		{name: "negative height", render: responsive.Size{Columns: 2, Rows: -1}, wantWidth: 2, wantHeight: 1},
		{name: "zero width", render: responsive.Size{Columns: 0, Rows: 2}, wantWidth: 1, wantHeight: 2},
		{name: "zero height", render: responsive.Size{Columns: 2, Rows: 0}, wantWidth: 2, wantHeight: 1},
		{name: "unit dimensions", render: responsive.Size{Columns: 1, Rows: 1}, wantWidth: 1, wantHeight: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			model.layout = responsive.Layout{Reported: responsive.Size{Columns: 70, Rows: 30}, Render: test.render, Class: responsive.Compact}
			frame := model.recoveryFrame(nil)
			if frame.Width() != test.wantWidth || frame.Height() != test.wantHeight {
				t.Fatalf("recovery frame = %dx%d", frame.Width(), frame.Height())
			}
		})
	}

	model.layout = responsive.Layout{
		Reported: responsive.Size{Columns: 70, Rows: 30}, Render: responsive.Size{Columns: 40, Rows: 1}, Class: responsive.Compact,
	}
	oneRow := model.recoveryFrame(nil)
	if strings.Contains(frameRow(oneRow, 0), "q quit") {
		t.Fatalf("one-row recovery contains footer: %q", frameRow(oneRow, 0))
	}
	model.layout.Render.Rows = 2
	twoRows := model.recoveryFrame(nil)
	if !strings.Contains(frameRow(twoRows, 0), "Window 70x30") || !strings.Contains(frameRow(twoRows, 1), "q quit · d debug") {
		t.Fatalf("two-row recovery = %q / %q", frameRow(twoRows, 0), frameRow(twoRows, 1))
	}
}

func frameRow(frame *view.Frame, row int) string {
	var text strings.Builder
	for column := 0; column < frame.Width(); column++ {
		cell, _ := frame.CellAt(column, row)
		text.WriteString(cell.Text)
	}
	return text.String()
}

func TestDiagnosticFailureLatches(t *testing.T) {
	t.Run("invalid event code", func(t *testing.T) {
		model, surface, sink := testModel(t)
		state := surface.DiagnosticState()
		model.record("", diagnostics.OutcomeApplied, state, state, nil, 0)
		model.record("valid", diagnostics.OutcomeApplied, state, state, nil, 0)
		if !model.diagnosticFailed || len(sink.snapshot()) != 0 {
			t.Fatalf("invalid-code latch = %v, events %#v", model.diagnosticFailed, sink.snapshot())
		}
	})

	t.Run("sink failure", func(t *testing.T) {
		model, surface, _ := testModel(t)
		sink := &failingSink{}
		model.options.Events = sink
		state := surface.DiagnosticState()
		model.record("first", diagnostics.OutcomeApplied, state, state, nil, 0)
		model.record("second", diagnostics.OutcomeApplied, state, state, nil, 0)
		if !model.diagnosticFailed || sink.calls != 1 {
			t.Fatalf("sink-failure latch = %v, calls %d", model.diagnosticFailed, sink.calls)
		}
	})
}

func TestWorstSupportedResizeBurstP95(t *testing.T) {
	palette, _ := theme.Builtin("terminal")
	rowID := testID(t, "resize-row")
	// More than 500 columns, including wide and combining graphemes.
	line := strings.Repeat("界e\u0301🙂abc", 80)
	for _, phase := range []string{"functional", "timing"} {
		t.Run(phase, func(t *testing.T) {
			measuring := phase == "timing"
			if measuring && (os.Getenv("HERDR_PLUGIN_KIT_TIMING") != "1" || raceEnabled || testing.CoverMode() != "") {
				t.Skip("isolated timing requires HERDR_PLUGIN_KIT_TIMING=1 without race/coverage")
			}
			for _, variant := range []string{"empty-baseline", "unicode-baseline", "annotated-memory", "annotated-count-retention", "annotated-byte-retention"} {
				t.Run(variant, func(t *testing.T) {
					annotated := strings.HasPrefix(variant, "annotated-")
					memory := &memorySink{}
					var sink diagnostics.SemanticSink = memory
					var recorder *diagnostics.Recorder
					var directory string
					limit := 50 * time.Millisecond
					if strings.HasSuffix(variant, "retention") {
						directory = t.TempDir()
						config := diagnostics.DefaultConfig(directory)
						if variant == "annotated-count-retention" {
							config.MaxEvents = 32
						} else {
							config.MaxBytes = 512 << 10
						}
						var err error
						recorder, err = diagnostics.Open(config)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if err := recorder.Close(); err != nil {
								t.Error(err)
							}
						})
						sink = recorder
						// Accepted revised integrated resize threshold.
						limit = 100 * time.Millisecond
					}
					surface := &probeSurface{testSurface: newTestSurface(t)}
					surface.render = func(ctx RenderContext) (*view.Frame, error) {
						frame, err := view.NewFrame(ctx.Layout.Render.Columns, ctx.Layout.Render.Rows)
						if err != nil {
							return nil, err
						}
						if variant != "empty-baseline" {
							for row := 0; row < frame.Height(); row++ {
								if annotated {
									if err := frame.PutTextBox(view.TextBoxOptions{Element: rowID, Instance: row, Y: row, Width: frame.Width(), Height: 1, Mode: view.TextTruncate}, []string{line}, view.Style{}); err != nil {
										return nil, err
									}
								} else {
									frame.PutText(0, row, view.Truncate(line, frame.Width(), "…"), view.Style{})
								}
							}
						}
						return frame, nil
					}
					model := newModel(ProgramOptions{PluginID: testID(t, "timing"), Theme: palette, Events: sink}, surface)
					random := rand.New(rand.NewSource(1))
					traces := 16
					if measuring {
						traces = 200
					}
					durations := make([]time.Duration, 0, traces)
					var allocatedBytes, allocations uint64
					boundedRenders := 0
					var boundedReport *diagnostics.TextFitReport
					for range traces {
						for range 3 {
							model.Update(tea.WindowSizeMsg{Width: 40 + random.Intn(461), Height: 10 + random.Intn(191)})
						}
						if measuring {
							var before, after runtime.MemStats
							runtime.ReadMemStats(&before)
							started := time.Now()
							model.Update(tea.WindowSizeMsg{Width: 500, Height: 200})
							_ = model.View()
							durations = append(durations, time.Since(started))
							runtime.ReadMemStats(&after)
							allocatedBytes += after.TotalAlloc - before.TotalAlloc
							allocations += after.Mallocs - before.Mallocs
						} else {
							model.Update(tea.WindowSizeMsg{Width: 500, Height: 200})
						}
						if measuring && recorder == nil {
							// Outside the latency and allocation measurements.
							for _, event := range memory.events {
								if event.Code.String() == "render.completed" {
									boundedRenders++
									boundedReport = event.TextFit
									if annotated != (event.TextFit != nil) {
										t.Fatal("unexpected render report presence")
									}
								} else if event.TextFit != nil {
									t.Fatal("report attached to non-render event")
								}
							}
							clear(memory.events)
							memory.events = memory.events[:0]
						}
					}
					if model.diagnosticFailed {
						t.Fatal("recorder failed during resize burst")
					}
					if variant != "empty-baseline" {
						expected, _ := view.NewFrame(500, 200)
						for row := 0; row < 200; row++ {
							expected.PutText(0, row, view.Truncate(line, 500, "…"), view.Style{})
						}
						if model.View() != view.ANSI(expected) {
							t.Fatal("Unicode resize output differs from existing rendering")
						}
					}
					var report *diagnostics.TextFitReport
					var retainedBytes int
					if recorder == nil {
						renders := boundedRenders
						report = boundedReport
						for _, event := range memory.snapshot() {
							if event.Code.String() == "render.completed" {
								renders++
								report = event.TextFit
							} else if event.TextFit != nil {
								t.Fatal("report attached to non-render event")
							}
						}
						if renders != traces*4 {
							t.Fatalf("render count=%d want=%d", renders, traces*4)
						}
					} else {
						paths, err := canonicalLogFiles(directory)
						if err != nil || len(paths) == 0 {
							t.Fatalf("segments=%v err=%v", paths, err)
						}
						lastOrdinal, err := strconv.ParseUint(filepath.Base(paths[len(paths)-1])[7:27], 10, 64)
						if err != nil || lastOrdinal <= 1 {
							t.Fatalf("rotation not exercised: %v", paths)
						}
						data, err := readCanonicalLog(directory)
						if err != nil {
							t.Fatal(err)
						}
						retainedBytes = len(data)
						lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
						var first, last diagnostics.Event
						if err := json.Unmarshal(lines[0], &first); err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal(lines[len(lines)-1], &last); err != nil {
							t.Fatal(err)
						}
						if first.Sequence <= 1 || last.Message != "render.completed" {
							t.Fatalf("retention/final render missing: first=%d last=%s", first.Sequence, last.Message)
						}
						page, err := recorder.Debug(diagnostics.DebugQuery{PageSize: 1})
						if err != nil || len(page.Events) != 1 {
							t.Fatalf("debug projection err=%v page=%+v", err, page)
						}
						report = page.Events[0].TextFit
						t.Logf("rotation last_ordinal=%d retained_events=%d first_sequence=%d retained_wire_bytes=%d", lastOrdinal, len(lines), first.Sequence, retainedBytes)
					}
					if annotated {
						if report == nil || len(report.Observations) != 200 || report.Omitted != 0 {
							t.Fatalf("final annotated report=%+v", report)
						}
						for row, observation := range report.Observations {
							if observation.Element != rowID || observation.Instance != row || observation.AvailableColumns != 500 || !observation.Truncated {
								t.Fatalf("row %d report=%+v", row, observation)
							}
						}
					} else if report != nil {
						t.Fatal("baseline unexpectedly measured")
					}
					if measuring {
						sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
						p95 := durations[(len(durations)*95+99)/100-1]
						payloadBytes := 0
						if report != nil {
							payload, err := json.Marshal(report)
							if err != nil {
								t.Fatal(err)
							}
							payloadBytes = len(payload)
						}
						t.Logf("seed=1 traces=%d p95_view=%s max_view=%s mean_allocations=%d mean_allocated_bytes=%d text_fit_json_bytes=%d ansi_bytes=%d (allocations are process-wide deltas for final updates; no CPU measurement)", traces, p95, durations[len(durations)-1], allocations/uint64(traces), allocatedBytes/uint64(traces), payloadBytes, len(model.View()))
						if p95 > limit {
							t.Fatalf("p95 final resize-to-view=%s want<=%s", p95, limit)
						}
					}
				})
			}
		})
	}
}
