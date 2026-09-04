package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
	"github.com/rivo/uniseg"
)

const resizeSettleDelay = 100 * time.Millisecond

type Surface interface {
	Update(EventContext, Event) []Effect
	Render(RenderContext) (*view.Frame, error)
	DiagnosticState() diagnostics.VisualState
}

type RenderContext struct {
	Layout           responsive.Layout
	Theme            theme.Palette
	Focused          bool
	ResizeGeneration uint64
	Settled          bool
}

type ProgramOptions struct {
	PluginID diagnostics.ID
	Theme    theme.Palette
	Events   diagnostics.SemanticSink
	Input    io.Reader
	Output   io.Writer
}

func NewProgram(options ProgramOptions, surface Surface) (*tea.Program, error) {
	if options.PluginID.IsZero() {
		return nil, errors.New("shell plugin ID is empty")
	}
	if options.Theme.ID == "" {
		return nil, errors.New("shell theme is empty")
	}
	if options.Events == nil {
		return nil, errors.New("shell semantic event sink is nil")
	}
	if surface == nil {
		return nil, errors.New("shell surface is nil")
	}
	model := newModel(options, surface)
	programOptions := []tea.ProgramOption{tea.WithAltScreen()}
	if options.Input != nil {
		programOptions = append(programOptions, tea.WithInput(options.Input))
	}
	if options.Output != nil {
		programOptions = append(programOptions, tea.WithOutput(options.Output))
	}
	return tea.NewProgram(model, programOptions...), nil
}

type settleMessage struct {
	generation uint64
	startedAt  time.Time
}
type resultMessage struct{ ResultEvent }
type timerMessage struct{ TimerEvent }

type model struct {
	options             ProgramOptions
	surface             Surface
	layout              responsive.Layout
	focused             bool
	settled             bool
	resizeGeneration    uint64
	requestGenerations  map[RequestKey]uint64
	requestCancels      map[RequestKey]context.CancelFunc
	requestCorrelations map[RequestKey]diagnostics.ID
	timerGenerations    map[EventCode]uint64
	correlation         uint64
	rendered            string
	diagnosticFailed    bool
}

func newModel(options ProgramOptions, surface Surface) *model {
	return &model{
		options: options, surface: surface, layout: responsive.Resolve(responsive.Size{}),
		requestGenerations: make(map[RequestKey]uint64), requestCancels: make(map[RequestKey]context.CancelFunc),
		requestCorrelations: make(map[RequestKey]diagnostics.ID), timerGenerations: make(map[EventCode]uint64),
	}
}

func (model *model) Init() tea.Cmd { return tea.EnableReportFocus }

func (model *model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	receivedAt := time.Now()
	before := model.surface.DiagnosticState()
	context := model.eventContext()
	var event Event
	var diagnosticEvent Event
	var code string
	var outcome = diagnostics.OutcomeApplied
	var extra tea.Cmd
	var eventDuration time.Duration
	needsRender := false

	switch message := message.(type) {
	case tea.WindowSizeMsg:
		model.resizeGeneration++
		model.layout = responsive.Resolve(responsive.Size{Columns: message.Width, Rows: message.Height})
		model.settled = false
		event = ResizeEvent{Layout: model.layout, Generation: model.resizeGeneration}
		code = "resize.received"
		needsRender = true
		generation := model.resizeGeneration
		extra = tea.Tick(resizeSettleDelay, func(time.Time) tea.Msg {
			return settleMessage{generation: generation, startedAt: receivedAt}
		})
	case tea.KeyMsg:
		if message.Type == tea.KeyRunes {
			event = TextEvent{Text: string(message.Runes), Paste: message.Paste, Alt: message.Alt}
			code = "input.text"
			needsRender = true
		} else if key, ok := translateKey(message); ok {
			event = KeyEvent{Code: key, Alt: message.Alt}
			code = "input.key"
			needsRender = true
		} else {
			code, outcome = "input.unsupported", diagnostics.OutcomeRejected
		}
	case tea.FocusMsg:
		model.focused = true
		event, code = FocusEvent{Focused: true}, "focus.gained"
		needsRender = true
	case tea.BlurMsg:
		model.focused = false
		event, code = FocusEvent{Focused: false}, "focus.lost"
		needsRender = true
	case settleMessage:
		code = "resize.settled"
		eventDuration = receivedAt.Sub(message.startedAt)
		settleCode, _ := NewEventCode("resize.settle")
		diagnosticEvent = TimerEvent{Code: settleCode, Generation: message.generation}
		if message.generation != model.resizeGeneration {
			outcome = diagnostics.OutcomeStale
		} else {
			model.settled = true
			needsRender = true
		}
	case resultMessage:
		code = "request.completed"
		current := model.requestGenerations[message.Key]
		if message.Generation != current {
			outcome = diagnostics.OutcomeStale
			diagnosticEvent = message.ResultEvent
		} else {
			delete(model.requestCancels, message.Key)
			delete(model.requestCorrelations, message.Key)
			diagnosticEvent = message.ResultEvent
			if message.Result.Err == nil && !message.Result.Code.Valid() {
				outcome = diagnostics.OutcomeRejected
			} else if message.Result.Err != nil {
				event = message.ResultEvent
				needsRender = true
				outcome = diagnostics.OutcomeFailed
			} else {
				event = message.ResultEvent
				needsRender = true
				outcome = message.Result.Code
			}
		}
	case timerMessage:
		code = "timer.fired"
		diagnosticEvent = message.TimerEvent
		if message.Generation != model.timerGenerations[message.Code] {
			outcome = diagnostics.OutcomeStale
		} else {
			event = message.TimerEvent
			needsRender = true
		}
	default:
		code, outcome = "event.unsupported", diagnostics.OutcomeRejected
	}

	var effects []Effect
	if event != nil {
		effects = model.surface.Update(context, event)
	}
	if diagnosticEvent == nil {
		diagnosticEvent = event
	}
	after := model.surface.DiagnosticState()
	model.record(code, outcome, before, after, diagnosticEvent, eventDuration)
	commands := model.commands(effects)
	if extra != nil {
		commands = append(commands, extra)
	}
	if needsRender {
		model.render(receivedAt)
	}
	if len(commands) == 0 {
		return model, nil
	}
	return model, tea.Batch(commands...)
}

func (model *model) View() string { return model.rendered }

func (model *model) eventContext() EventContext {
	return EventContext{
		start: func(key RequestKey, work Work) (Effect, error) {
			if key.id.IsZero() {
				return Effect{}, errors.New("request key is empty")
			}
			if work == nil {
				return Effect{}, errors.New("request work is nil")
			}
			model.requestGenerations[key]++
			model.correlation++
			correlation, _ := diagnostics.NewID(fmt.Sprintf("c-%d", model.correlation))
			effect := Effect{
				kind: effectWork, requestKey: key, generation: model.requestGenerations[key],
				correlation: correlation, previousCorrelation: model.requestCorrelations[key], work: work,
			}
			model.requestCorrelations[key] = correlation
			return effect, nil
		},
		after: func(delay time.Duration, code EventCode) (Effect, error) {
			if delay <= 0 {
				return Effect{}, errors.New("timer delay must be positive")
			}
			if code.id.IsZero() {
				return Effect{}, errors.New("event code is empty")
			}
			model.timerGenerations[code]++
			return Effect{kind: effectTimer, eventCode: code, generation: model.timerGenerations[code], delay: delay}, nil
		},
	}
}

func (model *model) commands(effects []Effect) []tea.Cmd {
	commands := make([]tea.Cmd, 0, len(effects))
	for _, effect := range effects {
		state := model.surface.DiagnosticState()
		switch effect.kind {
		case effectWork:
			if cancel := model.requestCancels[effect.requestKey]; cancel != nil {
				cancel()
				model.record("request.cancelled", diagnostics.OutcomeCancelled, state, state, ResultEvent{
					Key: effect.requestKey, Generation: effect.generation - 1, Correlation: effect.previousCorrelation,
				}, 0)
			}
			workContext, cancel := context.WithCancel(context.Background())
			model.requestCancels[effect.requestKey] = cancel
			model.record("request.started", diagnostics.OutcomeApplied, state, state, ResultEvent{
				Key: effect.requestKey, Generation: effect.generation, Correlation: effect.correlation,
			}, 0)
			current := effect
			commands = append(commands, func() tea.Msg {
				result := current.work(workContext)
				return resultMessage{ResultEvent{Key: current.requestKey, Generation: current.generation, Correlation: current.correlation, Result: result}}
			})
		case effectTimer:
			model.record("timer.scheduled", diagnostics.OutcomeApplied, state, state, TimerEvent{
				Code: effect.eventCode, Generation: effect.generation,
			}, effect.delay)
			current := effect
			commands = append(commands, tea.Tick(current.delay, func(time.Time) tea.Msg {
				return timerMessage{TimerEvent{Code: current.eventCode, Generation: current.generation}}
			}))
		case effectQuit:
			for _, cancel := range model.requestCancels {
				cancel()
			}
			model.record("program.quit", diagnostics.OutcomeApplied, state, state, nil, 0)
			commands = append(commands, tea.Quit)
		}
	}
	return commands
}

func (model *model) render(receivedAt time.Time) {
	frame, err := model.surface.Render(RenderContext{
		Layout: model.layout, Theme: model.options.Theme, Focused: model.focused,
		ResizeGeneration: model.resizeGeneration, Settled: model.settled,
	})
	contractFailed := err != nil || frame == nil
	if frame != nil && (frame.Width() != model.layout.Render.Columns || frame.Height() != model.layout.Render.Rows) {
		contractFailed = true
	}
	if model.layout.Class == responsive.Recovery || contractFailed {
		frame = model.recoveryFrame(err)
	}
	model.rendered = view.ANSI(frame)
	if model.layout.Projected {
		model.rendered = "\x1b[2J\x1b[H" + model.rendered
	}
	state := model.surface.DiagnosticState()
	outcome := diagnostics.OutcomeApplied
	if contractFailed {
		outcome = diagnostics.OutcomeFailed
	}
	model.record("render.completed", outcome, state, state, nil, time.Since(receivedAt))
}

func (model *model) recoveryFrame(renderErr error) *view.Frame {
	width, height := model.layout.Render.Columns, model.layout.Render.Rows
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	frame, _ := view.NewFrame(width, height)
	style := view.Style{Foreground: model.options.Theme.Text, Background: model.options.Theme.Background}
	frame.Fill(0, 0, width, height, style)
	message := fmt.Sprintf("Window %dx%d", model.layout.Reported.Columns, model.layout.Reported.Rows)
	if renderErr != nil {
		message = "UI unavailable"
	} else if model.layout.Class == responsive.Recovery {
		message += " · expand to at least 40x10"
	}
	frame.PutText(0, 0, message, style)
	if height > 1 {
		frame.PutText(0, height-1, "q quit · d debug", style)
	}
	return frame
}

func (model *model) record(code string, outcome diagnostics.OutcomeCode, before, after diagnostics.VisualState, event Event, duration time.Duration) {
	if model.diagnosticFailed {
		return
	}
	eventCode, err := diagnostics.NewID(code)
	if err != nil {
		model.diagnosticFailed = true
		return
	}
	semantic := diagnostics.SemanticEvent{
		Level: diagnostics.LevelInfo, Kind: diagnostics.KindInteraction,
		Plugin: model.options.PluginID, Code: eventCode, Outcome: outcome,
		Before: before.State, After: after.State, Generation: model.resizeGeneration,
		Duration: duration, Geometry: diagnostics.Geometry{
			ReportedColumns: model.layout.Reported.Columns, ReportedRows: model.layout.Reported.Rows,
			RenderColumns: model.layout.Render.Columns, RenderRows: model.layout.Render.Rows,
		},
		Visual: &after,
	}
	if text, ok := event.(TextEvent); ok {
		semantic.Bytes = len(text.Text)
		semantic.Graphemes = uniseg.GraphemeClusterCount(text.Text)
		semantic.Alt = text.Alt
		semantic.Paste = text.Paste
	}
	if key, ok := event.(KeyEvent); ok {
		semantic.Action, _ = diagnostics.NewID(string(key.Code))
		semantic.Alt = key.Alt
	}
	if result, ok := event.(ResultEvent); ok {
		semantic.Action, _ = diagnostics.NewID(result.Key.String())
		semantic.Correlation = result.Correlation
		semantic.RelatedGeneration = result.Generation
	}
	if timer, ok := event.(TimerEvent); ok {
		semantic.Action, _ = diagnostics.NewID(timer.Code.String())
		semantic.RelatedGeneration = timer.Generation
	}
	if err := model.options.Events.RecordSemantic(semantic); err != nil {
		model.diagnosticFailed = true
	}
}

func translateKey(message tea.KeyMsg) (KeyCode, bool) {
	switch message.Type {
	case tea.KeyEnter:
		return KeyEnter, true
	case tea.KeyBackspace:
		return KeyBackspace, true
	case tea.KeyTab:
		return KeyTab, true
	case tea.KeyEsc:
		return KeyEscape, true
	case tea.KeyUp:
		return KeyUp, true
	case tea.KeyDown:
		return KeyDown, true
	case tea.KeyLeft:
		return KeyLeft, true
	case tea.KeyRight:
		return KeyRight, true
	case tea.KeyHome:
		return KeyHome, true
	case tea.KeyEnd:
		return KeyEnd, true
	case tea.KeyPgUp:
		return KeyPageUp, true
	case tea.KeyPgDown:
		return KeyPageDown, true
	case tea.KeyDelete:
		return KeyDelete, true
	case tea.KeySpace:
		return KeySpace, true
	case tea.KeyCtrlC:
		return KeyCtrlC, true
	default:
		return "", false
	}
}
