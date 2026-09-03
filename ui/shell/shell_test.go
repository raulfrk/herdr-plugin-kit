package shell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
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
	for _, size := range []tea.WindowSizeMsg{{Width: 70, Height: 30}, {Width: 70, Height: 10}, {Width: 70, Height: 30}} {
		model.Update(size)
	}
	if model.resizeGeneration != 3 || model.layout.Reported != (responsive.Size{Columns: 70, Rows: 30}) {
		t.Fatalf("latest resize = generation %d layout %#v", model.resizeGeneration, model.layout)
	}
	if surface.layout.Render != (responsive.Size{Columns: 70, Rows: 30}) {
		t.Fatalf("surface layout = %#v", surface.layout)
	}
	model.Update(settleMessage{generation: 2})
	if model.settled {
		t.Fatal("stale settle changed state")
	}
	model.Update(settleMessage{generation: 3})
	if !model.settled {
		t.Fatal("current settle did not change state")
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
	logData, err := os.ReadFile(filepath.Join(directory, diagnostics.EventLogName))
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
	model.Update(settleMessage{generation: 3})
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
	assert("resize.settled", "resize.settle", 3, diagnostics.OutcomeStale, 0)
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
	if first.correlation.IsZero() || second.correlation.IsZero() || first.correlation == second.correlation ||
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
	tests := []struct {
		name        string
		render      func(RenderContext) (*view.Frame, error)
		wantMessage string
	}{
		{
			name: "error with exact frame",
			render: func(context RenderContext) (*view.Frame, error) {
				frame, _ := view.NewFrame(context.Layout.Render.Columns, context.Layout.Render.Rows)
				return frame, errors.New("private render detail")
			},
			wantMessage: "UI unavailable",
		},
		{name: "nil frame", render: func(RenderContext) (*view.Frame, error) { return nil, nil }, wantMessage: "Window 70x30"},
		{
			name: "wrong width only",
			render: func(context RenderContext) (*view.Frame, error) {
				return view.NewFrame(context.Layout.Render.Columns-1, context.Layout.Render.Rows)
			},
			wantMessage: "Window 70x30",
		},
		{
			name: "wrong height only",
			render: func(context RenderContext) (*view.Frame, error) {
				return view.NewFrame(context.Layout.Render.Columns, context.Layout.Render.Rows-1)
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
	if os.Getenv("HERDR_PLUGIN_KIT_TIMING") != "1" {
		t.Skip("run make test-timing for isolated wall-clock verification")
	}
	if raceEnabled || testing.CoverMode() != "" {
		t.Skip("wall-clock latency is measured without instrumentation")
	}
	palette, _ := theme.Builtin("terminal")
	surface := newTestSurface(t)
	model := newModel(ProgramOptions{
		PluginID: testID(t, "timing"), Theme: palette, Events: discardSink{},
	}, surface)
	random := rand.New(rand.NewSource(1))
	durations := make([]time.Duration, 0, 200)
	for range 200 {
		for range 3 {
			model.Update(tea.WindowSizeMsg{
				Width:  40 + random.Intn(461),
				Height: 10 + random.Intn(191),
			})
		}
		started := time.Now()
		model.Update(tea.WindowSizeMsg{Width: 500, Height: 200})
		durations = append(durations, time.Since(started))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95 := durations[(len(durations)*95+99)/100-1]
	evidence, _ := json.Marshal(map[string]any{
		"seed": 1, "traces": len(durations), "p95_view_ns": p95.Nanoseconds(),
	})
	t.Log(string(evidence))
	if p95 > 50*time.Millisecond {
		t.Fatalf("p95 final resize-to-view = %s, want <= 50ms", p95)
	}
}
