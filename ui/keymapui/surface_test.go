package keymapui

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/documentstore"
	"github.com/raulfrk/herdr-plugin-kit/ui/interaction"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

type probe struct {
	changeAfterRefresh                              bool
	bold                                            bool
	disableRefresh                                  bool
	extraActions                                    int
	name                                            string
	ownDebugKey                                     bool
	ownHelp                                         bool
	editing                                         bool
	revision, invocations, resizes, results, timers int
	text                                            string
	suppress                                        bool
	ownTab                                          bool
}

func (p *probe) KeymapCatalog() []keymap.Action {
	editDefaults := []keymap.Sequence{{"/"}}
	if p.ownTab {
		editDefaults = append(editDefaults, keymap.Sequence{"tab"})
	}
	actions := []keymap.Action{
		{ID: "probe.refresh", Label: "Refresh example", Context: p.name + ".results", Quick: true, Defaults: []keymap.Sequence{{"g", "r"}, {"f5"}}},
		{ID: "probe.edit", Label: "Edit query", Context: p.name + ".results", Quick: true, Defaults: editDefaults},
		{ID: "probe.finish", Label: "Finish query", Context: p.name + ".editing", Editing: true, Defaults: []keymap.Sequence{{"enter"}, {"escape"}}},
	}
	if p.ownDebugKey {
		actions = append(actions, keymap.Action{ID: "probe.d", Label: "Consumer D", Context: p.name + ".results", Defaults: []keymap.Sequence{{"d"}}})
	}
	if p.ownHelp {
		actions = append(actions, keymap.Action{ID: "picker.help", Label: "Help", Context: p.name + ".results", Defaults: []keymap.Sequence{{"?"}}})
	}
	for index := range p.extraActions {
		actions = append(actions, keymap.Action{
			ID:      fmt.Sprintf("probe.extra.%02d", index),
			Label:   fmt.Sprintf("Extra action %02d", index),
			Context: p.name + ".results",
		})
	}
	return actions
}
func (p *probe) KeymapContext() keymap.Context {
	context := p.name + ".results"
	if p.editing {
		context = p.name + ".editing"
	}
	return keymap.Context{ID: context, Editing: p.editing, Target: fmt.Sprintf("revision-%d", p.revision)}
}
func (p *probe) ActionState(id string) keymap.State {
	for _, a := range p.KeymapCatalog() {
		if a.Context == p.KeymapContext().ID && a.ID == id {
			if p.disableRefresh && id == "probe.refresh" {
				return keymap.State{Reason: "current selection is unavailable"}
			}
			return keymap.State{Enabled: true}
		}
	}
	return keymap.State{}
}
func (p *probe) Update(events shell.EventContext, event shell.Event) []shell.Effect {
	switch e := event.(type) {
	case shell.ActionEvent:
		switch e.ID {
		case "probe.refresh":
			p.invocations++
			if p.changeAfterRefresh {
				code, _ := shell.NewEventCode("probe.replace")
				effect, err := events.After(300*time.Millisecond, code)
				if err == nil {
					return []shell.Effect{effect}
				}
			}
		case "probe.edit":
			p.editing = true
		case "probe.finish":
			p.editing = false
		}
	case shell.TextEvent:
		p.text += e.Text
	case shell.ResizeEvent:
		p.resizes++
	case shell.ResultEvent:
		p.results++
	case shell.TimerEvent:
		p.timers++
		if e.Code.String() == "probe.replace" {
			p.revision++
		}
	}
	return nil
}
func (p *probe) Render(context shell.RenderContext) (*view.Frame, error) {
	frame, err := view.NewFrame(context.Layout.Render.Columns, context.Layout.Render.Rows)
	if frame != nil {
		frame.PutText(0, 3, "example content", view.Style{Bold: p.bold})
	}
	return frame, err
}
func (p *probe) DiagnosticState() diagnostics.VisualState {
	id, _ := diagnostics.NewID(p.name)
	state, _ := diagnostics.NewID("ready")
	return diagnostics.VisualState{Screen: id, State: state}
}
func (p *probe) SuppressEventDiagnostics(shell.Event) bool   { return p.suppress }
func (p *probe) SuppressEffectDiagnostics(shell.Effect) bool { return p.suppress }

func uiForTest(t *testing.T, directory string, recovery bool) (*Surface, *probe) {
	t.Helper()
	p := &probe{name: "example"}
	s, err := New(Options{Main: p, ConfigDirectory: directory, DefaultKeymap: recovery})
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func TestCompactNormalWeightIsOptInAcrossComposedScreens(t *testing.T) {
	palette, err := theme.Builtin("catppuccin")
	if err != nil {
		t.Fatal(err)
	}
	render := func(t *testing.T, width, height int, enabled bool, screen screen) *view.Frame {
		t.Helper()
		main := &probe{name: "example", bold: true}
		surface, newErr := New(Options{
			Main: main, ConfigDirectory: t.TempDir(), InteractiveHelp: true,
			CompactNormalWeight: enabled,
		})
		if newErr != nil {
			t.Fatal(newErr)
		}
		defer surface.Close()
		surface.view = screen
		frame, renderErr := surface.Render(shell.RenderContext{
			Layout: responsive.Resolve(responsive.Size{Columns: width, Rows: height}),
			Theme:  palette,
		})
		if renderErr != nil {
			t.Fatal(renderErr)
		}
		return frame
	}
	hasBold := func(frame *view.Frame) bool {
		for y := 0; y < frame.Height(); y++ {
			for x := 0; x < frame.Width(); x++ {
				cell, _ := frame.CellAt(x, y)
				if cell.Text != "" && cell.Style.Bold {
					return true
				}
			}
		}
		return false
	}

	for _, current := range []screen{baseScreen, actionsScreen, editorScreen, entryScreen, textScreen, captureScreen, checkerScreen, helpScreen} {
		t.Run(fmt.Sprintf("compact-screen-%d", current), func(t *testing.T) {
			if frame := render(t, 48, 18, true, current); hasBold(frame) {
				t.Fatalf("screen %d retained visible bold cells", current)
			}
		})
	}
	for _, size := range []responsive.Size{{Columns: 40, Rows: 10}, {Columns: 48, Rows: 18}} {
		if frame := render(t, size.Columns, size.Rows, true, baseScreen); hasBold(frame) {
			t.Fatalf("opted-in compact size %+v retained visible bold cells", size)
		}
	}
	for _, test := range []struct {
		name    string
		width   int
		enabled bool
	}{{name: "default compact", width: 48}, {name: "boundary", width: 64, enabled: true}, {name: "standard", width: 80, enabled: true}} {
		t.Run(test.name, func(t *testing.T) {
			if frame := render(t, test.width, 18, test.enabled, baseScreen); !hasBold(frame) {
				t.Fatalf("width %d enabled=%t lost existing bold hierarchy", test.width, test.enabled)
			}
		})
	}

	defaultFrame := render(t, 48, 18, false, helpScreen)
	compactFrame := render(t, 48, 18, true, helpScreen)
	for y := 0; y < defaultFrame.Height(); y++ {
		for x := 0; x < defaultFrame.Width(); x++ {
			want, _ := defaultFrame.CellAt(x, y)
			got, _ := compactFrame.CellAt(x, y)
			want.Style.Bold = false
			if got != want {
				t.Fatalf("cell %d,%d changed beyond bold: got=%+v want=%+v", x, y, got, want)
			}
		}
	}
	if !reflect.DeepEqual(defaultFrame.TextFit(), compactFrame.TextFit()) {
		t.Fatal("compact weight changed text-fit diagnostics")
	}
}

func TestGlobalFooterFitsSupportedMinimum(t *testing.T) {
	palette, err := theme.Builtin("catppuccin")
	if err != nil {
		t.Fatal(err)
	}
	states := []struct {
		name  string
		setup func(*Surface)
		want  string
	}{
		{name: "base", setup: func(*Surface) {}, want: "Ctrl-C quit"},
		{name: "actions focused", setup: func(surface *Surface) { surface.actionsFocused = true }, want: "[Actions]"},
		{name: "pending", setup: func(surface *Surface) {
			if _, consumed := surface.runtime.Step(surface.contentContext(), "g", time.Now()); !consumed {
				t.Fatal("test prefix was not consumed")
			}
		}, want: "g → r"},
		{name: "checker", setup: func(surface *Surface) { surface.view = checkerScreen }, want: "Esc Esc within 1s return"},
		{name: "capture", setup: func(surface *Surface) { surface.view = captureScreen }, want: "Enter finish"},
	}
	for _, size := range []responsive.Size{{Columns: 110, Rows: 24}, {Columns: 80, Rows: 18}, {Columns: 48, Rows: 18}, {Columns: 40, Rows: 10}} {
		for _, state := range states {
			t.Run(fmt.Sprintf("%s/%dx%d", state.name, size.Columns, size.Rows), func(t *testing.T) {
				surface, _ := uiForTest(t, t.TempDir(), false)
				t.Cleanup(func() { _ = surface.Close() })
				state.setup(surface)
				frame, err := surface.Render(shell.RenderContext{Layout: responsive.Resolve(size), Theme: palette})
				if err != nil {
					t.Fatal(err)
				}
				for _, observation := range frame.TextFit().Observations {
					if observation.Element.String() == "keymap-footer" && (observation.Clipped || observation.Truncated) {
						t.Fatalf("footer did not fit: %+v", observation)
					}
				}
				if text := view.ANSI(frame); !strings.Contains(text, state.want) {
					t.Fatalf("footer lost %q: %q", state.want, text)
				} else if state.name == "base" && size.Columns == 40 && !strings.Contains(text, "Ctrl-C quit · ctrl+k Actions · tab nav") {
					t.Fatalf("minimum footer is not explicit: %q", text)
				}
			})
		}
	}
}

func TestExplicitChildTabOwnershipPreservesFormBinding(t *testing.T) {
	p := &probe{name: "example", ownTab: true}
	s, err := New(Options{Main: p, ConfigDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	first := s
	t.Cleanup(func() { _ = first.Close() })
	action, consumed := s.runtime.Step(p.KeymapContext(), "tab", time.Now())
	if !consumed || action != "probe.edit" {
		t.Fatalf("action=%q consumed=%v", action, consumed)
	}
	if got := s.runtime.Effective("example.results", "keymap.focus-next"); len(got) != 0 {
		t.Fatalf("wrapper Tab still competes in owned context: %v", got)
	}
	palette, _ := theme.Builtin("catppuccin")
	frame, err := s.Render(shell.RenderContext{Layout: responsive.Resolve(responsive.Size{Columns: 80, Rows: 18}), Theme: palette})
	if err != nil {
		t.Fatal(err)
	}
	if text := view.ANSI(frame); strings.Contains(text, "unbound controls") || !strings.Contains(text, "Actions ctrl+k") {
		t.Fatalf("owned Tab footer is misleading: %q", text)
	}
	p.ownTab = false
	s, err = New(Options{Main: p, ConfigDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	second := s
	t.Cleanup(func() { _ = second.Close() })
	if got := s.runtime.Effective("example.results", "keymap.focus-next"); !reflect.DeepEqual(got, []keymap.Sequence{{"tab"}}) {
		t.Fatalf("ordinary wrapper Tab changed: %v", got)
	}
}

func TestEmbeddedPickerReservesGlobalFooterAtMinimumSize(t *testing.T) {
	palette, _ := theme.Builtin("terminal")
	tests := []struct {
		name          string
		wrapperFooter string
		build         func(*testing.T) *Surface
	}{
		{name: "internal editor", wrapperFooter: "Ctrl-C quit · ctrl+k Actions · tab nav", build: func(t *testing.T) *Surface {
			surface, _ := uiForTest(t, t.TempDir(), false)
			surface.view = editorScreen
			return surface
		}},
		{name: "consumer main picker", wrapperFooter: "Ctrl-C quit · Actions ctrl+k", build: func(t *testing.T) *Surface {
			picker, err := interaction.NewPicker(interaction.PickerOptions{Title: "Main", Load: func(context.Context, string, interaction.Cursor) (interaction.Page, error) {
				return interaction.Page{}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			surface, err := New(Options{Main: picker, ConfigDirectory: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			return surface
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			surface := test.build(t)
			t.Cleanup(func() { _ = surface.Close() })
			frame, err := surface.Render(shell.RenderContext{Layout: responsive.Resolve(responsive.Size{Columns: 40, Rows: 10}), Theme: palette})
			if err != nil {
				t.Fatal(err)
			}
			line := func(row int) string {
				var text strings.Builder
				for column := 0; column < frame.Width(); column++ {
					cell, _ := frame.CellAt(column, row)
					if cell.Continuation {
						continue
					}
					if cell.Text == "" {
						text.WriteByte(' ')
					} else {
						text.WriteString(cell.Text)
					}
				}
				return strings.TrimRight(text.String(), " ")
			}
			want := []string{
				"? help · / search · j/k move · o open",
				"n/p page · q quit",
				test.wrapperFooter,
			}
			for index, expected := range want {
				if got := line(frame.Height() - len(want) + index); got != expected {
					t.Fatalf("composed footer row %d=%q, want %q", index, got, expected)
				}
			}
		})
	}
}

func TestConsumerDebugKeyOverridesOnlyBareWrapperFallback(t *testing.T) {
	main := &probe{name: "example", ownDebugKey: true}
	debug := &probe{name: "debug"}
	s, err := New(Options{Main: main, Debug: debug, ConfigDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := s.runtime.Effective("example.results", "keymap.debug"); !reflect.DeepEqual(got, []keymap.Sequence{{"alt+d"}}) {
		t.Fatalf("claimed-context diagnostics bindings=%v", got)
	}
	if action, consumed := s.runtime.Step(main.KeymapContext(), "d", time.Now()); !consumed || action != "probe.d" {
		t.Fatalf("consumer d action=%q consumed=%t", action, consumed)
	}
	if action, consumed := s.runtime.Step(main.KeymapContext(), "alt+d", time.Now()); !consumed || action != "keymap.debug" {
		t.Fatalf("Alt+D diagnostics action=%q consumed=%t", action, consumed)
	}

	ordinary := &probe{name: "ordinary"}
	legacy, err := New(Options{Main: ordinary, Debug: debug, ConfigDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if action, consumed := legacy.runtime.Step(ordinary.KeymapContext(), "d", time.Now()); !consumed || action != "keymap.debug" {
		t.Fatalf("unclaimed d action=%q consumed=%t", action, consumed)
	}
	ordinary.editing = true
	if action, consumed := legacy.runtime.Step(ordinary.KeymapContext(), "d", time.Now()); consumed || action != "" {
		t.Fatalf("editing d action=%q consumed=%t", action, consumed)
	}
}

type observation struct {
	view                                   screen
	debug, editing, pending, draft, paste  bool
	selected, status, warning, text, frame string
	keys                                   []string
	calls, revision                        int
	document                               keymap.Document
	diskRevision                           documentstore.Revision
	draftDocument                          keymap.Document
	saveEnabled                            bool
}
type observer struct {
	*Surface
	main   *probe
	states chan observation
}

func (o *observer) Render(context shell.RenderContext) (*view.Frame, error) {
	frame, err := o.Surface.Render(context)
	state := observation{view: o.view, debug: o.showingDebug, editing: o.contentContext().Editing, pending: o.loading || o.saving, draft: o.draftOpen, paste: o.checkerPaste, status: o.status, warning: o.warning, keys: append([]string(nil), o.checkerKeys...), calls: o.main.invocations, revision: o.main.revision, text: o.main.text, document: o.runtime.Document()}
	state.diskRevision, state.draftDocument = o.disk.revision, keymap.Clone(o.draft)
	state.saveEnabled = o.ActionState("keymap.save").Enabled
	picker := o.activePicker()
	if picker == nil && o.view == baseScreen {
		picker, _ = o.base().(*interaction.Picker)
	}
	if picker != nil {
		item, _ := picker.Selected()
		state.selected = item.Key
		state.pending = state.pending || picker.DiagnosticState().Pending
	}
	if frame != nil {
		state.frame = view.ANSI(frame)
	}
	select {
	case o.states <- state:
	default:
	}
	return frame, err
}

type semanticSink struct {
	mu     sync.Mutex
	events []diagnostics.SemanticEvent
}

func (s *semanticSink) RecordSemantic(event diagnostics.SemanticEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return nil
}
func (s *semanticSink) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(s.events)
	return data
}

type driver struct {
	program *tea.Program
	done    chan error
	input   *io.PipeWriter
	surface *Surface
	states  chan observation
	sink    *semanticSink
	last    observation
	queries map[screen]string
	stopped bool
}

func runUIForTest(t *testing.T, s *Surface, p *probe) *driver {
	t.Helper()
	palette, _ := theme.Builtin("terminal")
	plugin, _ := diagnostics.NewID("keymap-test")
	input, writer := io.Pipe()
	o := &observer{Surface: s, main: p, states: make(chan observation, 128)}
	sink := &semanticSink{}
	program, err := shell.NewProgram(shell.ProgramOptions{PluginID: plugin, Theme: palette, Events: sink, Keymap: s.Runtime(), Input: input, Output: io.Discard}, o)
	if err != nil {
		t.Fatal(err)
	}
	d := &driver{program: program, done: make(chan error, 1), input: writer, surface: s, states: o.states, sink: sink, queries: map[screen]string{}}
	go func() { _, err := program.Run(); d.done <- err }()
	t.Cleanup(func() {
		if !d.stopped {
			program.Kill()
			_ = writer.Close()
			select {
			case <-d.done:
			case <-time.After(2 * time.Second):
				t.Error("keymap program did not stop")
			}
		}
		_ = s.Close()
	})
	program.Send(tea.WindowSizeMsg{Width: 80, Height: 19})
	d.wait(t, func(o observation) bool { return o.view == baseScreen })
	return d
}
func (d *driver) wait(t *testing.T, predicate func(observation) bool) observation {
	t.Helper()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case state := <-d.states:
			d.last = state
			if predicate(state) {
				return state
			}
		case err := <-d.done:
			d.stopped = true
			t.Fatalf("program stopped: %v", err)
		case <-timeout.C:
			d.sink.mu.Lock()
			var actions []string
			for _, event := range d.sink.events {
				if event.Code.String() == "action.invoked" {
					actions = append(actions, event.Action.String())
				}
			}
			d.sink.mu.Unlock()
			if len(actions) > 12 {
				actions = actions[len(actions)-12:]
			}
			t.Fatalf("UI did not reach expected state; last view=%d selected=%q status=%q pending=%t; actions=%v", d.last.view, d.last.selected, d.last.status, d.last.pending, actions)
		}
	}
}
func (d *driver) key(code tea.KeyType) { d.program.Send(tea.KeyMsg{Type: code}) }
func (d *driver) typeText(text string) {
	d.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
}
func (d *driver) choose(t *testing.T, key, query string) {
	t.Helper()
	current := d.last.view
	d.typeText("/")
	d.key(tea.KeyHome)
	for range d.queries[current] {
		d.key(tea.KeyDelete)
	}
	d.typeText(query)
	d.queries[current] = query
	d.wait(t, func(o observation) bool { return o.view == current && !o.pending && o.editing && o.selected == key })
	d.key(tea.KeyEnter)
	d.wait(t, func(o observation) bool { return o.view == current && !o.pending && !o.editing && o.selected == key })
	d.key(tea.KeyEnter)
}

func TestEditorCheckerAndSavedBindingsThroughShell(t *testing.T) {
	directory := t.TempDir()
	s, p := uiForTest(t, directory, false)
	d := runUIForTest(t, s, p)
	d.typeText("gr")
	d.wait(t, func(o observation) bool { return o.calls == 1 })
	d.key(tea.KeyCtrlK)
	d.wait(t, func(o observation) bool { return o.view == actionsScreen && !o.pending })
	d.choose(t, "keymap.editor", "Keyboard shortcuts")
	d.wait(t, func(o observation) bool { return o.view == editorScreen && !o.pending })
	d.choose(t, "example.results/probe.refresh", "Refresh example")
	d.wait(t, func(o observation) bool { return o.view == entryScreen && !o.pending })
	d.choose(t, "clear", "Clear all bindings")
	d.wait(t, func(o observation) bool {
		return o.view == entryScreen && !o.pending && strings.Contains(o.status, "Unbound")
	})
	d.choose(t, "type", "Type a new sequence")
	d.wait(t, func(o observation) bool { return o.view == textScreen })
	d.typeText("x")
	d.key(tea.KeyEnter)
	d.wait(t, func(o observation) bool { return o.view == entryScreen && !o.pending })
	d.key(tea.KeyCtrlS)
	saved := d.wait(t, func(o observation) bool { return o.view == baseScreen && o.status == "Keyboard shortcuts saved." })
	if saved.draft {
		t.Fatal("saved editor retained draft")
	}
	d.typeText("grx")
	d.wait(t, func(o observation) bool { return o.calls == 2 })
	d.key(tea.KeyCtrlK)
	d.wait(t, func(o observation) bool { return o.view == actionsScreen && !o.pending })
	d.choose(t, "keymap.checker", "Key checker")
	d.wait(t, func(o observation) bool { return o.view == checkerScreen })
	d.typeText("1234567890")
	seen := d.wait(t, func(o observation) bool { return len(o.keys) == 8 && o.keys[7] == "0" })
	if strings.Join(seen.keys, "") != "34567890" || seen.calls != 2 {
		t.Fatal("checker invoked or retained unbounded input", seen.keys, seen.calls)
	}
	d.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("private-paste-canary"), Paste: true})
	d.wait(t, func(o observation) bool { return o.paste })
	d.key(tea.KeyEsc)
	d.key(tea.KeyEsc)
	returned := d.wait(t, func(o observation) bool { return o.view == baseScreen })
	if len(returned.keys) != 0 || returned.calls != 2 || returned.text != "" {
		t.Fatal("checker leaked input into origin")
	}
	if strings.Contains(string(d.sink.bytes()), "private-paste-canary") {
		t.Fatal("paste leaked into diagnostics")
	}
	d.key(tea.KeyCtrlC)
	select {
	case err := <-d.done:
		d.stopped = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ctrl-C did not quit")
	}
}

func TestCheckerEscapeTimingAndDraftPreservation(t *testing.T) {
	s, _ := uiForTest(t, t.TempDir(), false)
	defer s.Close()
	s.draftOpen = true
	s.draft = keymap.Document{Version: 1}
	s.view = entryScreen
	s.status = "draft"
	now := time.Unix(10, 0)
	s.now = func() time.Time { return now }
	s.openChecker()
	s.CaptureKey(shell.EventContext{}, shell.CapturedKey{Stroke: "escape"})
	now = now.Add(time.Second + time.Nanosecond)
	s.CaptureKey(shell.EventContext{}, shell.CapturedKey{Stroke: "escape"})
	if s.view != checkerScreen {
		t.Fatal("late Escape exited")
	}
	s.CaptureKey(shell.EventContext{}, shell.CapturedKey{Stroke: "x"})
	s.CaptureKey(shell.EventContext{}, shell.CapturedKey{Stroke: "escape"})
	if s.view != checkerScreen {
		t.Fatal("nonconsecutive Escape exited")
	}
	now = now.Add(time.Millisecond)
	s.CaptureKey(shell.EventContext{}, shell.CapturedKey{Stroke: "escape"})
	if s.view != entryScreen || !s.draftOpen || s.draft.Version != 1 || len(s.checkerKeys) != 0 {
		t.Fatal("checker did not restore draft")
	}
}

func TestSharedRouterBackgroundDeliveryAndPrivacy(t *testing.T) {
	main, debug := &probe{name: "main"}, &probe{name: "debug", suppress: true}
	s, err := New(Options{Main: main, Debug: debug, ConfigDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Update(shell.EventContext{}, shell.ResizeEvent{Generation: 1})
	s.Update(shell.EventContext{}, shell.ActionEvent{ID: "keymap.debug"})
	s.Update(shell.EventContext{}, shell.ResultEvent{})
	s.Update(shell.EventContext{}, shell.TimerEvent{})
	s.Update(shell.EventContext{}, shell.ActionEvent{ID: "keymap.debug"})
	if main.results != 1 || debug.results != 1 || main.timers != 1 || debug.timers != 1 || main.resizes != 2 || debug.resizes != 1 || s.showingDebug {
		t.Fatal("background router delivery changed", main, debug)
	}
	if !s.SuppressEventDiagnostics(shell.TimerEvent{}) || !s.SuppressEffectDiagnostics(shell.Effect{}) {
		t.Fatal("debug suppression missing")
	}
	s.openChecker()
	s.checkerKeys = []string{"private-key-canary"}
	data, _ := json.Marshal(s.DiagnosticState())
	if strings.Contains(string(data), "private-key-canary") {
		t.Fatal("checker input in snapshot")
	}
}

func documentWithKey(key string) keymap.Document {
	return keymap.Document{Version: 1, Bindings: []keymap.Binding{{Context: "example.results", Action: "probe.refresh", Sequences: []keymap.Sequence{{key}}}}}
}
func hasKey(document keymap.Document, key string) bool {
	return len(document.Bindings) == 1 && len(document.Bindings[0].Sequences) == 1 && len(document.Bindings[0].Sequences[0]) == 1 && document.Bindings[0].Sequences[0][0] == key
}

func TestTwoInstancesReloadDraftDeferralInvalidUpdatesAndConflict(t *testing.T) {
	directory := t.TempDir()
	left, lp := uiForTest(t, directory, false)
	right, rp := uiForTest(t, directory, false)
	a, b := runUIForTest(t, left, lp), runUIForTest(t, right, rp)
	written, err := left.configuration.save(context.Background(), documentWithKey("x"), documentstore.Revision{})
	if err != nil {
		t.Fatal(err)
	}
	a.wait(t, func(o observation) bool { return hasKey(o.document, "x") })
	b.wait(t, func(o observation) bool { return hasKey(o.document, "x") })
	a.key(tea.KeyCtrlK)
	a.wait(t, func(o observation) bool { return o.view == actionsScreen && !o.pending })
	a.choose(t, "keymap.editor", "Keyboard shortcuts")
	a.wait(t, func(o observation) bool { return o.view == editorScreen && !o.pending })
	_, err = right.configuration.save(context.Background(), documentWithKey("y"), written.revision)
	if err != nil {
		t.Fatal(err)
	}
	// Save either observes the new revision or loses the CAS race. Both must
	// retain this instance's draft and preserve the other instance's bytes.
	a.key(tea.KeyCtrlS)
	deferred := a.wait(t, func(o observation) bool { return o.draft && strings.Contains(o.status, "File changed") })
	if !hasKey(deferred.document, "x") {
		t.Fatal("external map activated over editor draft")
	}
	b.wait(t, func(o observation) bool { return hasKey(o.document, "y") })
	if current := right.configuration.read(context.Background()); !hasKey(current.document, "y") {
		t.Fatal("stale save overwrote concurrent file")
	}
	if err := os.WriteFile(filepath.Join(directory, configName), []byte("version = 1\nprivate_unknown = 'do-not-log'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.wait(t, func(o observation) bool { return o.warning != "" })
	invalid := b.wait(t, func(o observation) bool { return o.warning != "" })
	if !hasKey(invalid.document, "y") {
		t.Fatal("invalid update replaced last valid map")
	}
	a.choose(t, "keymap.discard", "Discard and return")
	closed := a.wait(t, func(o observation) bool { return o.view == baseScreen && !o.draft })
	if !hasKey(closed.document, "y") {
		t.Fatal("leaving draft lost newest valid map")
	}
	if strings.Contains(string(a.sink.bytes()), "do-not-log") || strings.Contains(string(b.sink.bytes()), "do-not-log") {
		t.Fatal("configuration value leaked into diagnostics")
	}
}

func TestRecoveryEditsSavedMapWithoutActivatingOverrides(t *testing.T) {
	directory := t.TempDir()
	seed, p := uiForTest(t, directory, false)
	if _, err := seed.configuration.save(context.Background(), documentWithKey("x"), seed.disk.revision); err != nil {
		t.Fatal(err)
	}
	_ = seed.Close()
	_ = p
	recovery, rp := uiForTest(t, directory, true)
	normal, np := uiForTest(t, directory, false)
	a, b := runUIForTest(t, recovery, rp), runUIForTest(t, normal, np)
	if len(a.last.document.Bindings) != 0 || !hasKey(b.last.document, "x") || !strings.Contains(a.last.frame, "Recovery") {
		t.Fatal("recovery did not retain defaults and label mode")
	}
	a.key(tea.KeyCtrlK)
	a.wait(t, func(o observation) bool { return o.view == actionsScreen && !o.pending })
	a.choose(t, "keymap.editor", "Keyboard shortcuts")
	a.wait(t, func(o observation) bool { return o.view == editorScreen && !o.pending })
	a.choose(t, "keymap.reset-all", "Reset all overrides")
	a.wait(t, func(o observation) bool { return o.view == editorScreen && !o.pending })
	a.key(tea.KeyCtrlS)
	a.wait(t, func(o observation) bool { return o.view == baseScreen && o.status == "Keyboard shortcuts saved." })
	b.wait(t, func(o observation) bool { return len(o.document.Bindings) == 0 })
	a.typeText("gr")
	a.wait(t, func(o observation) bool { return o.calls == 1 })
	b.typeText("gr")
	b.wait(t, func(o observation) bool { return o.calls == 1 })
}

func TestRecordReplaceRemoveResetDiscardAndMobileActions(t *testing.T) {
	s, p := uiForTest(t, t.TempDir(), false)
	d := runUIForTest(t, s, p)
	d.program.Send(tea.WindowSizeMsg{Width: 40, Height: 10})
	d.typeText("/")
	d.wait(t, func(o observation) bool { return o.editing })
	d.key(tea.KeyEnter)
	d.wait(t, func(o observation) bool { return !o.editing })
	d.key(tea.KeyCtrlK)
	d.wait(t, func(o observation) bool { return o.view == actionsScreen && !o.pending })
	d.choose(t, "keymap.editor", "Keyboard shortcuts")
	d.wait(t, func(o observation) bool { return o.view == editorScreen && !o.pending })
	d.choose(t, "example.results/probe.refresh", "Refresh example")
	d.wait(t, func(o observation) bool { return o.view == entryScreen && !o.pending })
	d.choose(t, "replace-record-0", "Replace by recording: g r")
	d.wait(t, func(o observation) bool { return o.view == captureScreen })
	d.typeText("z")
	d.key(tea.KeyEnter)
	d.wait(t, func(o observation) bool { return o.view == entryScreen && !o.pending })
	d.choose(t, "remove-1", "Remove f5")
	d.wait(t, func(o observation) bool { return o.view == entryScreen && !o.pending })
	d.choose(t, "reset", "Restore defaults")
	d.wait(t, func(o observation) bool {
		return o.view == entryScreen && !o.pending && strings.Contains(o.status, "Default")
	})
	d.key(tea.KeyEsc)
	d.wait(t, func(o observation) bool { return o.view == editorScreen && !o.pending })
	d.choose(t, "keymap.discard", "Discard and return")
	closed := d.wait(t, func(o observation) bool { return o.view == baseScreen && !o.draft })
	if len(closed.document.Bindings) != 0 {
		t.Fatal("discard applied draft")
	}
}

func TestEachAliasOperationPersistsAndApplies(t *testing.T) {
	s, p := uiForTest(t, t.TempDir(), false)
	d := runUIForTest(t, s, p)
	action := p.KeymapCatalog()[0]
	for _, test := range []struct {
		operation, query, typed, prefill string
		want                             []keymap.Sequence
		state                            string
	}{
		{"type", "Type a new sequence", "f6", "", []keymap.Sequence{{"g", "r"}, {"f5"}, {"f6"}}, "Custom"},
		{"replace-type-0", "Replace by typing: g r", "f7", "g r", []keymap.Sequence{{"f7"}, {"f5"}, {"f6"}}, "Custom"},
		{"remove-0", "Remove: f7", "", "", []keymap.Sequence{{"f5"}, {"f6"}}, "Custom"},
		{"remove-1", "Remove: f6", "", "", []keymap.Sequence{{"f5"}}, "Custom"},
		{"clear", "Clear all bindings", "", "", nil, "Unbound"},
		{"reset", "Restore defaults", "", "", action.Defaults, "Default"},
	} {
		d.key(tea.KeyCtrlK)
		d.wait(t, func(o observation) bool { return o.view == actionsScreen && !o.pending })
		d.choose(t, "keymap.editor", "Keyboard shortcuts")
		d.wait(t, func(o observation) bool { return o.view == editorScreen && !o.pending })
		d.choose(t, "example.results/probe.refresh", "Refresh example")
		d.wait(t, func(o observation) bool { return o.view == entryScreen && !o.pending })
		d.choose(t, test.operation, test.query)
		if test.typed != "" {
			d.wait(t, func(o observation) bool { return o.view == textScreen })
			d.key(tea.KeyHome)
			for range test.prefill {
				d.key(tea.KeyDelete)
			}
			d.typeText(test.typed)
			d.key(tea.KeyEnter)
		}
		d.wait(t, func(o observation) bool { return o.view == entryScreen && !o.pending })
		d.key(tea.KeyCtrlS)
		saved := d.wait(t, func(o observation) bool { return o.view == baseScreen && o.status == "Keyboard shortcuts saved." })
		for _, document := range []keymap.Document{saved.document, s.configuration.read(context.Background()).document} {
			sequences, state := draftSequences(document, action)
			if state != test.state || !slices.EqualFunc(sequences, test.want, slices.Equal[keymap.Sequence]) {
				t.Fatalf("%s persisted %v (%s), want %v (%s)", test.operation, sequences, state, test.want, test.state)
			}
		}
	}
}

func TestPollingDefersDuringDraftAndStandaloneChecker(t *testing.T) {
	s, p := uiForTest(t, t.TempDir(), false)
	d := runUIForTest(t, s, p)
	written, err := s.configuration.save(context.Background(), documentWithKey("x"), documentstore.Revision{})
	if err != nil {
		t.Fatal(err)
	}
	d.wait(t, func(o observation) bool { return hasKey(o.document, "x") })
	d.key(tea.KeyCtrlK)
	d.wait(t, func(o observation) bool { return o.view == actionsScreen && !o.pending })
	d.choose(t, "keymap.editor", "Keyboard shortcuts")
	d.wait(t, func(o observation) bool { return o.view == editorScreen && !o.pending })
	written, err = s.configuration.save(context.Background(), documentWithKey("y"), written.revision)
	if err != nil {
		t.Fatal(err)
	}
	deferred := d.wait(t, func(o observation) bool { return o.diskRevision == written.revision && !o.pending })
	if !hasKey(deferred.document, "x") || !hasKey(deferred.draftDocument, "x") || deferred.saveEnabled || !strings.Contains(deferred.status, "File changed") {
		t.Fatal("poll did not freeze the active map and draft or disable stale Save", deferred)
	}
	d.choose(t, "keymap.reload", "Reload saved shortcuts")
	reloaded := d.wait(t, func(o observation) bool { return o.status == "Saved shortcuts reloaded into the draft." && !o.pending })
	if !hasKey(reloaded.document, "y") || !hasKey(reloaded.draftDocument, "y") || !reloaded.saveEnabled {
		t.Fatal("explicit Reload did not replace the map and draft", reloaded)
	}
	d.key(tea.KeyEsc)
	d.wait(t, func(o observation) bool { return o.view == baseScreen && !o.draft })
	d.key(tea.KeyCtrlK)
	d.wait(t, func(o observation) bool { return o.view == actionsScreen && !o.pending })
	d.choose(t, "keymap.checker", "Key checker")
	d.wait(t, func(o observation) bool { return o.view == checkerScreen })
	written, err = s.configuration.save(context.Background(), documentWithKey("z"), written.revision)
	if err != nil {
		t.Fatal(err)
	}
	deferred = d.wait(t, func(o observation) bool { return o.diskRevision == written.revision && !o.pending })
	if !hasKey(deferred.document, "y") {
		t.Fatal("poll replaced the active map inside standalone checker")
	}
	d.key(tea.KeyEsc)
	d.key(tea.KeyEsc)
	d.wait(t, func(o observation) bool { return o.view == baseScreen && hasKey(o.document, "z") })
}

func TestOnlyUnchangedKeymapPollEventsAreQuiet(t *testing.T) {
	s, _ := uiForTest(t, t.TempDir(), false)
	defer s.Close()
	s.disk = diskState{revision: documentstore.Revision{1}, warning: "existing warning"}
	s.warning = s.disk.warning
	otherCode, _ := shell.NewEventCode("other.timer")
	otherKey, _ := shell.NewRequestKey("other.request")
	for _, test := range []struct {
		name  string
		event shell.Event
		quiet bool
	}{
		{"poll", shell.TimerEvent{Code: pollCode}, true},
		{"other timer", shell.TimerEvent{Code: otherCode}, false},
		{"unchanged", shell.ResultEvent{Key: ioKey, Result: shell.WorkResult{Value: ioResult{disk: s.disk}}}, true},
		{"revision changed", shell.ResultEvent{Key: ioKey, Result: shell.WorkResult{Value: ioResult{disk: diskState{revision: documentstore.Revision{2}, warning: s.disk.warning}}}}, false},
		{"warning changed", shell.ResultEvent{Key: ioKey, Result: shell.WorkResult{Value: ioResult{disk: diskState{revision: s.disk.revision, warning: "new warning"}}}}, false},
		{"save", shell.ResultEvent{Key: ioKey, Result: shell.WorkResult{Value: ioResult{disk: s.disk, save: true}}}, false},
		{"unrelated result", shell.ResultEvent{Key: otherKey, Result: shell.WorkResult{Value: ioResult{disk: s.disk}}}, false},
		{"wrong result type", shell.ResultEvent{Key: ioKey}, false},
		{"action", shell.ActionEvent{ID: "probe.refresh"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := s.SuppressEventDiagnostics(test.event); got != test.quiet {
				t.Fatalf("suppressed = %t, want %t", got, test.quiet)
			}
		})
	}
	if s.SuppressEffectDiagnostics(shell.Quit()) {
		t.Fatal("quit effect suppressed")
	}
	for _, busy := range []struct{ saving, loading bool }{{}, {true, false}, {false, true}} {
		s.view, s.saving, s.loading = actionsScreen, busy.saving, busy.loading
		state := s.DiagnosticState()
		if state.Pending != (busy.saving || busy.loading) || !state.HasError || state.Screen.String() != "keymap" {
			t.Fatal("shared diagnostic state lost pending or error status", state)
		}
	}
}

type renderProbe struct {
	*probe
	frame *view.Frame
	err   error
	state diagnostics.VisualState
}

func (p *renderProbe) Render(shell.RenderContext) (*view.Frame, error) { return p.frame, p.err }
func (p *renderProbe) DiagnosticState() diagnostics.VisualState        { return p.state }

func TestWrapperPreservesChildContentErrorsAndDiagnostics(t *testing.T) {
	s, _ := uiForTest(t, t.TempDir(), false)
	defer s.Close()
	palette, _ := theme.Builtin("terminal")
	context := shell.RenderContext{Theme: palette, Layout: responsive.Resolve(responsive.Size{Columns: 80, Rows: 19})}
	frame, err := s.Render(context)
	if err != nil || !strings.Contains(view.ANSI(frame), "example content") {
		t.Fatal("wrapper hid child content", err)
	}
	p := &renderProbe{probe: &probe{name: "example"}}
	s.options.Main = p
	for _, failure := range []struct {
		frame *view.Frame
		err   error
	}{{nil, nil}, {nil, errors.New("render failed")}, {frame, errors.New("partial render")}} {
		p.frame, p.err = failure.frame, failure.err
		got, err := s.Render(context)
		if got != failure.frame || err != failure.err {
			t.Fatal("wrapper changed a child render failure", err)
		}
	}
	for _, hasError := range []bool{false, true} {
		for _, warning := range []string{"", "keyboard map warning"} {
			p.state = p.probe.DiagnosticState()
			p.state.HasError = hasError
			s.warning = warning
			got := s.DiagnosticState()
			if got.Screen != p.state.Screen || got.State != p.state.State || got.HasError != (hasError || warning != "") {
				t.Fatal("wrapper lost child diagnostics or map warning", got)
			}
		}
	}
}

func TestHelpListsOnlyOriginContextAndCheckerReportsPlainKey(t *testing.T) {
	s, _ := uiForTest(t, t.TempDir(), false)
	defer s.Close()
	s.helpContext = keymap.Context{ID: "example.results"}
	lines := strings.Join(s.helpLines(), "\n")
	if !strings.Contains(lines, "Refresh example: g r / f5") || strings.Contains(lines, "Finish query") {
		t.Fatal("help omitted effective bindings or mixed contexts", lines)
	}
	s.openChecker()
	s.CaptureKey(shell.EventContext{}, shell.CapturedKey{Stroke: "x"})
	palette, _ := theme.Builtin("terminal")
	frame, err := s.Render(shell.RenderContext{Theme: palette, Layout: responsive.Resolve(responsive.Size{Columns: 80, Rows: 19})})
	if err != nil || !strings.Contains(view.ANSI(frame), "Modifiers: none reported") {
		t.Fatal("plain-key modifier information missing", err)
	}
}

func TestInteractiveHelpEditsSelectedActionAndRoutesDeclaredCommands(t *testing.T) {
	p := &probe{name: "example"}
	commands := []HelpCommand{
		{ID: "check", Label: "check", Action: "probe.refresh", Defaults: []keymap.Sequence{{"k"}}},
		{ID: "reset", Label: "reset", Action: "probe.edit", Defaults: []keymap.Sequence{{"r"}}},
	}
	s, err := New(Options{Main: p, ConfigDirectory: t.TempDir(), InteractiveHelp: true, HelpOpenedAction: "probe.refresh", HelpCommands: commands})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.runtime.Effective("keymap.help", "keymap.help.previous"); !reflect.DeepEqual(got, []keymap.Sequence{{"up"}}) {
		t.Fatalf("interactive previous bindings=%v", got)
	}
	if got := s.runtime.Effective("keymap.help", "keymap.help.back"); slices.ContainsFunc(got, func(sequence keymap.Sequence) bool { return reflect.DeepEqual(sequence, keymap.Sequence{"enter"}) }) {
		t.Fatalf("interactive back still owns Enter: %v", got)
	}
	if got := s.runtime.Effective("keymap.help", "keymap.help.edit"); !reflect.DeepEqual(got, []keymap.Sequence{{"enter"}}) {
		t.Fatalf("interactive edit bindings=%v", got)
	}
	if got := s.runtime.Effective("keymap.help", "keymap.help.command.check"); !reflect.DeepEqual(got, []keymap.Sequence{{"k"}}) {
		t.Fatalf("check bindings=%v", got)
	}
	s.view, s.helpOrigin, s.helpContext = helpScreen, baseScreen, keymap.Context{ID: "example.results"}
	s.helpAction(shell.EventContext{}, "keymap.help.next")
	if s.helpSelected != 1 {
		t.Fatalf("next selected=%d", s.helpSelected)
	}
	s.helpAction(shell.EventContext{}, "keymap.help.end")
	if s.helpSelected != len(s.helpActions())-1 {
		t.Fatalf("end selected=%d actions=%d", s.helpSelected, len(s.helpActions()))
	}
	s.helpAction(shell.EventContext{}, "keymap.help.home")
	palette, _ := theme.Builtin("terminal")
	for _, size := range []responsive.Size{{Columns: 110, Rows: 24}, {Columns: 80, Rows: 18}, {Columns: 48, Rows: 18}, {Columns: 40, Rows: 10}} {
		frame, renderErr := s.Render(shell.RenderContext{Layout: responsive.Resolve(size), Theme: palette})
		text := view.ANSI(frame)
		if renderErr != nil || !strings.Contains(text, "> Edit query") || !strings.Contains(text, "Enter edit · k check · r reset") {
			t.Fatalf("size=%+v frame=%q error=%v", size, text, renderErr)
		}
	}
	s.view = baseScreen

	// A declared command always returns to Main before dispatch, including when
	// Help was opened from an editor or Debug surface.
	for index, origin := range []screen{baseScreen, editorScreen, entryScreen} {
		s.view, s.helpOrigin, s.helpContext = helpScreen, origin, keymap.Context{ID: "example.results"}
		s.showingDebug = index == 2
		s.status = "editor status"
		s.action(shell.EventContext{}, "keymap.help.command.check")
		if s.view != baseScreen || s.showingDebug || s.status != "" || p.invocations != index+1 {
			t.Fatalf("origin=%v view=%v debug=%t status=%q invocations=%d", origin, s.view, s.showingDebug, s.status, p.invocations)
		}
	}
	p.invocations = 0

	d := runUIForTest(t, s, p)
	d.key(tea.KeyCtrlK)
	d.wait(t, func(o observation) bool { return o.view == actionsScreen && !o.pending })
	d.choose(t, "keymap.help", "Help and effective shortcuts")
	d.wait(t, func(o observation) bool { return o.view == helpScreen })
	if p.invocations != 1 {
		t.Fatalf("Help-opened action invocations=%d", p.invocations)
	}
	d.key(tea.KeyEnter)
	d.wait(t, func(o observation) bool { return o.view == entryScreen && !o.pending })
	if s.selected.ID != "probe.edit" {
		t.Fatalf("Enter opened %q, want selected Help action", s.selected.ID)
	}
}

func TestHelpOverviewValidatesCatalogReferences(t *testing.T) {
	p := &probe{name: "example"}
	valid := HelpOverview{
		Title: "ACTIONS ◈ SHORTCUTS", Current: "? · Keys", ViewMenu: "g views",
		Rows: []HelpOverviewRow{{Label: "Search", Context: "example.results", Action: "probe.edit", Summary: "type query · enter switch"}},
	}
	for _, test := range []struct {
		name     string
		overview HelpOverview
		want     string
	}{
		{name: "interactive required", overview: valid, want: "interactive Help"},
		{name: "missing action", overview: HelpOverview{Title: "Keys", Rows: []HelpOverviewRow{{Label: "Missing", Context: "example.results", Action: "probe.missing"}}}, want: "catalog action"},
		{name: "duplicate reference", overview: HelpOverview{Title: "Keys", Rows: []HelpOverviewRow{{Label: "One", Context: "example.results", Action: "probe.edit"}, {Label: "Two", Context: "example.results", Action: "probe.edit"}}}, want: "duplicate"},
		{name: "multiline label", overview: HelpOverview{Title: "Keys", Rows: []HelpOverviewRow{{Label: "bad\nlabel", Context: "example.results", Action: "probe.edit"}}}, want: "single-line"},
		{name: "initial row outside rows", overview: HelpOverview{Title: "Keys", InitialRow: 1, Rows: valid.Rows}, want: "initial row"},
		{name: "view without current", overview: HelpOverview{Title: "Keys", Views: []HelpOverviewView{{Label: "Search", Context: "example.results", Action: "probe.edit"}}, Rows: valid.Rows}, want: "exactly one current"},
		{name: "current view references action", overview: HelpOverview{Title: "Keys", Views: []HelpOverviewView{{Label: "Keys", Context: "example.results", Action: "probe.edit", Current: true}}, Rows: valid.Rows}, want: "must not reference"},
		{name: "aside without title", overview: HelpOverview{Title: "Keys", Aside: &HelpOverviewAside{Steps: []HelpOverviewAsideLine{{Label: "1", Text: "open"}}}, Rows: valid.Rows}, want: "aside title is required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(Options{Main: p, ConfigDirectory: t.TempDir(), InteractiveHelp: test.name != "interactive required", HelpOverview: &test.overview})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want containing %q", err, test.want)
			}
		})
	}
	s, err := New(Options{Main: p, ConfigDirectory: t.TempDir(), InteractiveHelp: true, HelpOverview: &valid})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	catalog := (&probe{name: "bounds", extraActions: 64}).KeymapCatalog()
	rows := make([]HelpOverviewRow, 64)
	for index := range rows {
		rows[index] = HelpOverviewRow{Label: fmt.Sprintf("Row %d", index+1), Context: "bounds.results", Action: fmt.Sprintf("probe.extra.%02d", index)}
	}
	if _, _, err := validateHelpOverview(&HelpOverview{Title: "Keys", Rows: rows}, catalog); err != nil {
		t.Fatalf("64-row boundary rejected: %v", err)
	}
	for _, test := range []struct {
		name string
		rows []HelpOverviewRow
		want string
	}{
		{name: "zero rows", want: "must contain between 1 and 64 rows"},
		{name: "65 rows", rows: append(rows, HelpOverviewRow{Label: "Too many", Context: "bounds.results", Action: "probe.refresh"}), want: "must contain between 1 and 64 rows"},
		{name: "second label", rows: []HelpOverviewRow{rows[0], {Context: "bounds.results", Action: "probe.extra.01"}}, want: "row 2 label is required"},
		{name: "second summary", rows: []HelpOverviewRow{rows[0], {Label: "Row 2", Context: "bounds.results", Action: "probe.extra.01", Summary: strings.Repeat("x", 161)}}, want: "row 2 summary exceeds 160 characters"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, validationErr := validateHelpOverview(&HelpOverview{Title: "Keys", Rows: test.rows}, catalog)
			if validationErr == nil || validationErr.Error() != "Help overview "+test.want {
				t.Fatalf("error=%v want=%q", validationErr, "Help overview "+test.want)
			}
		})
	}
}

func TestHelpOverviewUsesEffectiveBindingsThemeAndReferencedEditTarget(t *testing.T) {
	p := &probe{name: "example"}
	status := "Codex ● ready"
	overview := HelpOverview{
		Title: "ACTIONS ◈ SHORTCUTS", Current: "? · Keys", ViewMenu: "g views",
		Status: func(int) HelpOverviewStatus { return HelpOverviewStatus{Text: status, Tone: HelpStatusSuccess} },
		Rows: []HelpOverviewRow{
			{Label: "Attention queue", Context: "example.results", Action: "probe.refresh", Summary: "enter switch · x exclude"},
			{Label: "Agent search", Context: "example.results", Action: "probe.edit", Summary: "type query · enter switch"},
			{Label: "Finish query", Context: "example.editing", Action: "probe.finish", Summary: "save exact input"},
		},
	}
	s, err := New(Options{Main: p, ConfigDirectory: t.TempDir(), InteractiveHelp: true, HelpOverview: &overview})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.view, s.helpOrigin, s.helpContext = helpScreen, baseScreen, keymap.Context{ID: "example.results"}

	palettes := []theme.Palette{}
	for _, id := range []string{"terminal", "catppuccin", "catppuccin-latte"} {
		palette, paletteErr := theme.Builtin(id)
		if paletteErr != nil {
			t.Fatal(paletteErr)
		}
		palettes = append(palettes, palette)
	}
	custom := palettes[1]
	custom.ID = "custom-proof"
	custom.Background, custom.PanelBackground = theme.Color("#010101"), theme.Color("#020202")
	custom.ActiveRowBackground, custom.SelectionBackground = theme.Color("#030303"), theme.Color("#040404")
	custom.Surface, custom.Border = theme.Color("#050505"), theme.Color("#060606")
	custom.Text, custom.Muted, custom.Accent = theme.Color("#070707"), theme.Color("#080808"), theme.Color("#090909")
	custom.Red, custom.Green, custom.Yellow = theme.Color("#100001"), theme.Color("#100002"), theme.Color("#100003")
	custom.Blue, custom.Magenta, custom.Cyan = theme.Color("#100004"), theme.Color("#100005"), theme.Color("#100006")
	palettes = append(palettes, custom)

	for _, size := range []responsive.Size{{Columns: 110, Rows: 24}, {Columns: 80, Rows: 18}, {Columns: 48, Rows: 18}, {Columns: 40, Rows: 10}} {
		var geometry string
		for _, palette := range palettes {
			frame, renderErr := s.Render(shell.RenderContext{Layout: responsive.Resolve(size), Theme: palette})
			if renderErr != nil {
				t.Fatalf("theme=%s size=%+v: %v", palette.ID, size, renderErr)
			}
			text := view.ANSI(frame)
			for _, want := range []string{"ACTIONS ◈ SHORTCUTS", "? · Keys", "g views", status, "Attention que", "enter switch"} {
				if !strings.Contains(text, want) {
					t.Fatalf("theme=%s size=%+v missing %q from %q", palette.ID, size, want, text)
				}
			}
			if size.Columns >= 48 && (!strings.Contains(text, "Attention queue") || !strings.Contains(text, "g r / f5")) {
				t.Fatalf("theme=%s size=%+v lost full label or binding from %q", palette.ID, size, text)
			}
			gotGeometry := helpFrameGeometry(frame)
			if geometry == "" {
				geometry = gotGeometry
			} else if gotGeometry != geometry {
				t.Fatalf("theme=%s size=%+v changed geometry", palette.ID, size)
			}
			assertHelpFrameUsesPalette(t, frame, palette)
		}
	}

	s.helpSelected = 1
	s.action(shell.EventContext{}, "keymap.help.edit")
	if s.view != entryScreen || s.selected.Context != "example.results" || s.selected.ID != "probe.edit" {
		t.Fatalf("view=%v selected=%+v", s.view, s.selected)
	}
}

func TestHelpOverviewMeasuresHeaderInTerminalCells(t *testing.T) {
	p := &probe{name: "example"}
	overview := HelpOverview{
		Title: "操作 ◈ キー", Current: "? · 検索", ViewMenu: "表示",
		Status: func(int) HelpOverviewStatus { return HelpOverviewStatus{Text: "準備完了", Tone: HelpStatusSuccess} },
		Rows:   []HelpOverviewRow{{Label: "検索", Context: "example.results", Action: "probe.edit", Summary: "入力"}},
	}
	s, err := New(Options{Main: p, ConfigDirectory: t.TempDir(), InteractiveHelp: true, HelpOverview: &overview})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.view, s.helpContext = helpScreen, keymap.Context{ID: "example.results"}
	palette, _ := theme.Builtin("terminal")
	frame, err := s.Render(shell.RenderContext{Layout: responsive.Resolve(responsive.Size{Columns: 80, Rows: 18}), Theme: palette})
	if err != nil {
		t.Fatal(err)
	}
	text := view.ANSI(frame)
	for _, want := range []string{"操作 ◈ キー", "? · 検索", "表示", "準備完了"} {
		if !strings.Contains(text, want) {
			t.Fatalf("wide header text %q was truncated from %q", want, text)
		}
	}
}

func TestHelpOverviewApprovedGeometry(t *testing.T) {
	p := &probe{name: "example", extraActions: 8}
	rows := []HelpOverviewRow{
		{Label: "Attention queue", Context: "example.results", Action: "probe.refresh", Summary: "enter switch · x exclude"},
		{Label: "Agent search", Context: "example.results", Action: "probe.edit", Summary: "type query · enter switch"},
	}
	for index := range 7 {
		rows = append(rows, HelpOverviewRow{
			Label:   fmt.Sprintf("Workflow %d", index+1),
			Context: "example.results",
			Action:  fmt.Sprintf("probe.extra.%02d", index),
			Summary: "run configured action",
		})
	}
	overview := HelpOverview{
		Title: "ACTIONS ◈ SHORTCUTS", Current: "? · Keys", ViewMenu: "g views",
		Status: func(int) HelpOverviewStatus {
			return HelpOverviewStatus{Text: "Codex ● ready", Tone: HelpStatusSuccess}
		},
		Rows: rows,
	}
	s, err := New(Options{Main: p, ConfigDirectory: t.TempDir(), InteractiveHelp: true, HelpOverview: &overview})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.view, s.helpContext = helpScreen, keymap.Context{ID: "example.results"}
	palette, _ := theme.Builtin("terminal")

	tests := []struct {
		name     string
		size     responsive.Size
		selected int
		want     string
	}{
		{name: "wide first", size: responsive.Size{Columns: 110, Rows: 24}, selected: 0, want: "5b342f04e061a8c7daf06c8ffec546732eacc70396246832ddb13fb726a6c7a8"},
		{name: "wide last", size: responsive.Size{Columns: 110, Rows: 24}, selected: 8, want: "58407a53c0d847605c50a87249d5a6cd87094fd49af5567ec8122cfdd116730a"},
		{name: "standard first", size: responsive.Size{Columns: 80, Rows: 18}, selected: 0, want: "744dfbbfd3f7c555caf831ac0761671d979b6a648964967ba0326d826bcf9e16"},
		{name: "standard last", size: responsive.Size{Columns: 80, Rows: 18}, selected: 8, want: "9efbde2a6dd378e4fb0c9de8ebab1a8b215f2eb85b5bb53f822f96f360687270"},
		{name: "compact first", size: responsive.Size{Columns: 48, Rows: 18}, selected: 0, want: "2d4c7ab44f3cc0ee06238292ce77c852e853d35d5ed35a6c679193d719902936"},
		{name: "compact last", size: responsive.Size{Columns: 48, Rows: 18}, selected: 8, want: "9237462e19f14bc24e0bebab84cc01dd7a82bd5e3fdf8426fa3938e2a45d9525"},
		{name: "minimum first", size: responsive.Size{Columns: 40, Rows: 10}, selected: 0, want: "a9cb1e14c8096f8a1a13574c956b3f70edee4a66a5961a72457f5834b92eabec"},
		{name: "minimum last", size: responsive.Size{Columns: 40, Rows: 10}, selected: 8, want: "2781d0dad1246dfbe98bf8a58ec0edc94377ae55ada8a95d519782ea60845725"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s.helpSelected = test.selected
			frame, renderErr := s.Render(shell.RenderContext{Layout: responsive.Resolve(test.size), Theme: palette})
			if renderErr != nil {
				t.Fatal(renderErr)
			}
			got := fmt.Sprintf("%x", sha256.Sum256([]byte(helpFrameVisualSignature(frame))))
			if got != test.want {
				t.Fatalf("geometry digest=%s want=%s\n%s", got, test.want, view.ANSI(frame))
			}
		})
	}
}

func TestComposedHelpOverviewMatchesResponsiveHierarchyAndThemeGeometry(t *testing.T) {
	p := &probe{name: "example", extraActions: 7, ownHelp: true}
	const description = "Herdr opener arms a page letter; inside the plugin use g + page letter, then a local action."
	rows := []HelpOverviewRow{
		{Label: "Attention queue", Context: "example.results", Action: "probe.refresh", Summary: "enter switch · x exclude"},
		{Label: "Agent search", Context: "example.results", Action: "probe.edit", Summary: "type query · enter switch"},
	}
	for index := range 7 {
		rows = append(rows, HelpOverviewRow{Label: fmt.Sprintf("Workflow %d", index+1), Context: "example.results", Action: fmt.Sprintf("probe.extra.%02d", index), Summary: "run configured action"})
	}
	rows[5].Label = "Fork agent"
	rows[len(rows)-1] = HelpOverviewRow{Label: "Keys & workflow", Context: "example.results", Action: "picker.help", Summary: "this reference"}
	overview := HelpOverview{
		Title: "ACTIONS ◈ SHORTCUTS", Current: "? · Keys", ViewMenu: "g views",
		Description: description,
		Footer:      "? keys · enter edit · ctrl+s save · k key checker · r reset", CompactFooter: "? keys · enter edit · ctrl+s save", InitialRow: 5,
		Status: func(int) HelpOverviewStatus {
			return HelpOverviewStatus{Text: "Codex ● ready", Tone: HelpStatusSuccess}
		},
		Views: []HelpOverviewView{
			{Label: "Queue", Context: "example.results", Action: "probe.refresh"},
			{Label: "Search", Context: "example.results", Action: "probe.edit"},
			{Label: "Keys", Current: true},
		},
		Rows: rows,
		Aside: &HelpOverviewAside{
			Title: "Fast path · Fork with letter",
			Steps: []HelpOverviewAsideLine{{Label: "1", Text: "prefix+a · arm pages"}, {Label: "2", Text: "f · Fork page"}, {Label: "3", Text: "s · Sonnet preset"}},
			Details: []HelpOverviewAsideLine{
				{Label: "one opener", Text: "opens or focuses the plugin and arms one page key"},
				{Label: "open mode", Text: "Custom Herdr key"},
				{Label: "Herdr key", Text: "prefix+a"},
				{Label: "switch page", Text: "g then the page key; ? opens help directly"},
			},
		},
	}
	s, err := New(Options{Main: p, ConfigDirectory: t.TempDir(), InteractiveHelp: true, HelpOverview: &overview})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.action(shell.EventContext{}, "keymap.help")
	if s.view != helpScreen || s.helpSelected != overview.InitialRow {
		t.Fatalf("opened composed Help at view=%v row=%d want row=%d", s.view, s.helpSelected, overview.InitialRow)
	}

	palettes := []theme.Palette{}
	for _, id := range []string{"terminal", "catppuccin", "catppuccin-latte"} {
		palette, paletteErr := theme.Builtin(id)
		if paletteErr != nil {
			t.Fatal(paletteErr)
		}
		palettes = append(palettes, palette)
	}
	custom := palettes[1]
	custom.ID = "custom-composed-proof"
	custom.Background, custom.PanelBackground, custom.ActiveRowBackground = theme.Color("#010101"), theme.Color("#020202"), theme.Color("#030303")
	custom.SelectionBackground, custom.Surface, custom.Border = theme.Color("#040404"), theme.Color("#050505"), theme.Color("#060606")
	custom.Text, custom.Muted, custom.Accent = theme.Color("#070707"), theme.Color("#080808"), theme.Color("#090909")
	custom.Red, custom.Green, custom.Yellow = theme.Color("#100001"), theme.Color("#100002"), theme.Color("#100003")
	palettes = append(palettes, custom)

	tests := []struct {
		name string
		size responsive.Size
		want string
	}{
		{name: "wide", size: responsive.Size{Columns: 110, Rows: 24}, want: "0879209278882e6dbebcee5d54b9755c2aa57461952eba542019c174c1e397ef"},
		{name: "standard", size: responsive.Size{Columns: 80, Rows: 18}, want: "191f6a12f545d9cb8024fbc237bd377553269abfdafe9b31b982982627c18692"},
		{name: "compact", size: responsive.Size{Columns: 48, Rows: 18}, want: "5fbadcfd118d9fb2dfecddd460db63bb08a61e86121b8a29389bf3ba3a628b7e"},
		{name: "minimum", size: responsive.Size{Columns: 40, Rows: 10}, want: "c2028695658d33a0f327e61d443f2995e087e5cdb5cbd37d5ef6d3c6eafc8a1a"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			geometry := ""
			for paletteIndex, palette := range palettes {
				frame, renderErr := s.Render(shell.RenderContext{Layout: responsive.Resolve(test.size), Theme: palette})
				if renderErr != nil {
					t.Fatal(renderErr)
				}
				text := view.ANSI(frame)
				for _, want := range []string{"ACTIONS ◈ SHORTCUTS", "Fork", "? keys", "ctrl+s save"} {
					if !strings.Contains(text, want) {
						t.Fatalf("theme=%s missing %q from %q", palette.ID, want, text)
					}
				}
				if test.size.Columns >= 100 {
					for _, want := range []string{"Queue", "Search", "Fast path", "prefix+a", description, "one page key", "directly"} {
						if !strings.Contains(text, want) {
							t.Fatalf("wide theme=%s missing %q from %q", palette.ID, want, text)
						}
					}
				} else if strings.Contains(text, "Fast path") {
					t.Fatalf("theme=%s size=%+v retained wide aside", palette.ID, test.size)
				}
				gotGeometry := helpFrameGeometry(frame)
				if geometry == "" {
					geometry = gotGeometry
				} else if gotGeometry != geometry {
					t.Fatalf("theme=%s size=%+v changed composed geometry", palette.ID, test.size)
				}
				assertHelpFrameUsesPalette(t, frame, palette)
				if paletteIndex == 0 {
					got := fmt.Sprintf("%x", sha256.Sum256([]byte(helpFrameVisualSignature(frame))))
					if got != test.want {
						t.Fatalf("visual digest=%s want=%s\n%s", got, test.want, text)
					}
				}
			}
		})
	}

	s.helpSelected = len(rows) - 1
	s.action(shell.EventContext{}, "keymap.help.edit")
	if s.view != entryScreen || s.selected.Context != "example.results" || s.selected.ID != "keymap.help" {
		t.Fatalf("Help reference edit target=%+v view=%v", s.selected, s.view)
	}
}

func TestComposedHelpOverviewLayoutValues(t *testing.T) {
	p := &probe{name: "example", extraActions: 7, ownHelp: true}
	rows := []HelpOverviewRow{
		{Label: "Queue", Context: "example.results", Action: "probe.refresh"},
		{Label: "Search", Context: "example.results", Action: "probe.edit"},
	}
	for index := range 7 {
		rows = append(rows, HelpOverviewRow{Label: fmt.Sprintf("Row %d", index+3), Context: "example.results", Action: fmt.Sprintf("probe.extra.%02d", index)})
	}
	overview := HelpOverview{
		Title: "Keys", Description: "Workflow", InitialRow: 5, Rows: rows,
		Views: []HelpOverviewView{
			{Label: "Queue", Context: "example.results", Action: "probe.refresh"},
			{Label: "Search", Context: "example.results", Action: "probe.edit"},
			{Label: "Keys", Current: true},
		},
		Aside: &HelpOverviewAside{Title: "Fast path"},
	}
	s, err := New(Options{Main: p, ConfigDirectory: t.TempDir(), InteractiveHelp: true, HelpOverview: &overview})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.helpContext, s.helpSelected = keymap.Context{ID: "example.results"}, overview.InitialRow

	tests := []struct {
		width, height int
		want          helpCompositionLayout
	}{
		{width: 110, height: 24, want: helpCompositionLayout{mainWidth: 79, asideX: 80, asideWidth: 30, contentTop: 3, visible: 20, selected: 5, start: 0, keyX: 44, keyWidth: 10, showSummary: true}},
		{width: 100, height: 18, want: helpCompositionLayout{mainWidth: 71, asideX: 72, asideWidth: 28, contentTop: 3, visible: 14, selected: 5, start: 0, keyX: 39, keyWidth: 10, showSummary: true}},
		{width: 80, height: 18, want: helpCompositionLayout{mainWidth: 80, contentTop: 3, visible: 14, selected: 5, start: 0, keyX: 44, keyWidth: 10, showSummary: true}},
		{width: 64, height: 18, want: helpCompositionLayout{mainWidth: 64, contentTop: 3, visible: 14, selected: 5, start: 0, keyX: 35, keyWidth: 10, showSummary: true}},
		{width: 48, height: 18, want: helpCompositionLayout{mainWidth: 48, contentTop: 3, visible: 14, selected: 5, start: 0, keyX: 26, keyWidth: 22}},
		{width: 40, height: 10, want: helpCompositionLayout{mainWidth: 40, contentTop: 3, visible: 6, selected: 5, start: 0, keyX: 22, keyWidth: 18}},
	}
	for _, test := range tests {
		if got := s.composedHelpLayout(test.width, test.height); !reflect.DeepEqual(got, test.want) {
			t.Fatalf("layout %dx%d=%+v want=%+v", test.width, test.height, got, test.want)
		}
	}
	s.helpSelected = 8
	wantScrolled := helpCompositionLayout{mainWidth: 40, contentTop: 3, visible: 6, selected: 8, start: 3, keyX: 22, keyWidth: 18}
	if got := s.composedHelpLayout(40, 10); !reflect.DeepEqual(got, wantScrolled) {
		t.Fatalf("scrolled minimum layout=%+v want=%+v", got, wantScrolled)
	}

	wantPlacements := []helpOverviewViewPlacement{
		{x: 1, width: 16, label: " g r / f5 Queue "},
		{x: 17, width: 10, label: " / Search "},
		{x: 27, width: 8, label: " ? Keys ", current: true},
	}
	if got := s.helpOverviewViewPlacements(110); !reflect.DeepEqual(got, wantPlacements) {
		t.Fatalf("view placements=%+v want=%+v", got, wantPlacements)
	}
	if got := s.helpOverviewViewPlacements(20); len(got) != 1 || !reflect.DeepEqual(got[0], wantPlacements[0]) {
		t.Fatalf("bounded view placements=%+v", got)
	}
	if got := s.helpOverviewViewPlacements(17); len(got) != 1 || !reflect.DeepEqual(got[0], wantPlacements[0]) {
		t.Fatalf("exact-boundary view placements=%+v", got)
	}
	if label, textWidth, rows := helpAsideDetailSize(30, 20, "opens or focuses the plugin and arms one page key"); label != 10 || textWidth != 17 || rows != 3 {
		t.Fatalf("aside detail size=(%d,%d,%d)", label, textWidth, rows)
	}
	if label, textWidth, rows := helpAsideDetailSize(28, 20, "opens or focuses the plugin and arms one page key"); label != 9 || textWidth != 16 || rows != 4 {
		t.Fatalf("100-column aside detail size=(%d,%d,%d)", label, textWidth, rows)
	}
	if _, _, rows := helpAsideDetailSize(30, 2, "opens or focuses the plugin and arms one page key"); rows != 2 {
		t.Fatalf("bounded aside detail rows=%d", rows)
	}
	s.options.HelpOverview.Description = ""
	if got := s.composedHelpLayout(80, 18); got.contentTop != 2 || got.visible != 15 {
		t.Fatalf("description-free layout=%+v", got)
	}
}

func TestComposedHelpOverviewReachableAllocationBoundaries(t *testing.T) {
	p := &probe{name: "example", ownHelp: true}
	overview := HelpOverview{
		Title: strings.Repeat("T", 80), Current: strings.Repeat("C", 60), ViewMenu: "g views",
		Status: func(int) HelpOverviewStatus { return HelpOverviewStatus{Text: "ready", Tone: HelpStatusSuccess} },
		Views:  []HelpOverviewView{{Label: "Keys", Current: true}},
		Rows:   []HelpOverviewRow{{Label: strings.Repeat("L", 64), Context: "example.results", Action: "probe.refresh", Summary: strings.Repeat("S", 80)}},
		Aside: &HelpOverviewAside{
			Title:   strings.Repeat("A", 64),
			Steps:   []HelpOverviewAsideLine{{Label: "1", Text: strings.Repeat("P", 64)}},
			Details: []HelpOverviewAsideLine{{Label: "Herdr key", Text: strings.Repeat("D", 64)}},
		},
	}
	s, err := New(Options{Main: p, ConfigDirectory: t.TempDir(), InteractiveHelp: true, HelpOverview: &overview})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.runtime.Apply(keymap.Document{Version: 1, Bindings: []keymap.Binding{{
		Context: "example.results", Action: "probe.refresh",
		Sequences: []keymap.Sequence{{"ctrl+page-up"}, {"ctrl+page-down"}},
	}}}); err != nil {
		t.Fatal(err)
	}
	s.action(shell.EventContext{}, "keymap.help")
	palette, _ := theme.Builtin("terminal")

	render := func(width, height int) *view.Frame {
		t.Helper()
		frame, renderErr := s.Render(shell.RenderContext{Layout: responsive.Resolve(responsive.Size{Columns: width, Rows: height}), Theme: palette})
		if renderErr != nil {
			t.Fatal(renderErr)
		}
		return frame
	}

	wide := render(100, 24)
	for element, want := range map[string]struct{ original, columns int }{
		"keymap-overview-title":              {original: 80, columns: 94},
		"keymap-overview-aside-title":        {original: 64, columns: 26},
		"keymap-overview-aside-step-text":    {original: 64, columns: 23},
		"keymap-overview-aside-detail-label": {original: 9, columns: 9},
		"keymap-overview-aside-detail-text":  {original: 64, columns: 16},
		"keymap-footer":                      {original: 10, columns: 100},
	} {
		got := helpTextFitObservation(t, wide, element)
		if got.OriginalColumns != want.original || got.AvailableColumns != want.columns {
			t.Fatalf("%s fit=(original %d, available %d) want=(%d,%d)", element, got.OriginalColumns, got.AvailableColumns, want.original, want.columns)
		}
	}
	if text := view.ANSI(wide); !strings.Contains(text, "Enter edit") {
		t.Fatalf("generated Help footer missing: %q", text)
	}
	border := helpTextFitObservation(t, wide, "keymap-overview-aside-border")
	if border.LayoutRows != 21 || border.AvailableRows != 21 || border.Clipped {
		t.Fatalf("wide aside border fit=%+v", border)
	}

	standard := render(80, 18)
	if cell, _ := standard.CellAt(71, 1); cell.Text != "›" {
		t.Fatalf("long-current next arrow cell=%+v", cell)
	}
	if got := helpTextFitObservation(t, standard, "keymap-overview-title"); got.OriginalColumns != 80 || got.AvailableColumns != 74 || !got.Truncated {
		t.Fatalf("standard title fit=%+v", got)
	}

	compact := render(40, 10)
	if got := helpTextFitObservation(t, compact, "keymap-overview-label"); got.OriginalColumns != 64 || got.AvailableColumns != 19 || !got.Truncated {
		t.Fatalf("compact label fit=%+v", got)
	}
	binding := helpTextFitObservation(t, compact, "keymap-overview-binding")
	if binding.OriginalColumns <= binding.AvailableColumns || binding.AvailableColumns != 16 || !binding.Truncated {
		t.Fatalf("compact binding fit=%+v", binding)
	}
}

func TestComposedHelpOverviewValidationBoundaries(t *testing.T) {
	base := HelpOverview{Title: "Keys", Rows: []HelpOverviewRow{{Label: "Search", Context: "example.results", Action: "probe.edit"}}}
	for name, mutate := range map[string]func(*HelpOverview){
		"description": func(value *HelpOverview) { value.Description = "workflow" },
		"footer":      func(value *HelpOverview) { value.Footer = "keys" },
		"views":       func(value *HelpOverview) { value.Views = []HelpOverviewView{{Label: "Keys", Current: true}} },
		"aside":       func(value *HelpOverview) { value.Aside = &HelpOverviewAside{Title: "Fast path"} },
	} {
		t.Run(name, func(t *testing.T) {
			value := base
			mutate(&value)
			if !value.composed() {
				t.Fatal("composed layout was not activated")
			}
		})
	}
	if base.composed() || (*HelpOverview)(nil).composed() {
		t.Fatal("empty overview activated composed layout")
	}

	catalog := (&probe{name: "example"}).KeymapCatalog()
	validView := HelpOverviewView{Label: "Search", Context: "example.results", Action: "probe.edit"}
	tests := []struct {
		name     string
		overview HelpOverview
		want     string
	}{
		{name: "second unknown view", overview: HelpOverview{Title: "Keys", Rows: base.Rows, Views: []HelpOverviewView{{Label: "Keys", Current: true}, {Label: "Missing", Context: "example.results", Action: "missing"}}}, want: "Help overview view 2 must reference a catalog action"},
		{name: "second view label", overview: HelpOverview{Title: "Keys", Rows: base.Rows, Views: []HelpOverviewView{{Label: "Keys", Current: true}, {Context: "example.results", Action: "probe.edit"}}}, want: "Help overview view 2 label is required"},
		{name: "current context only", overview: HelpOverview{Title: "Keys", Rows: base.Rows, Views: []HelpOverviewView{{Label: "Keys", Context: "example.results", Current: true}}}, want: "Help overview current view 1 must not reference an action"},
		{name: "current action only", overview: HelpOverview{Title: "Keys", Rows: base.Rows, Views: []HelpOverviewView{{Label: "Keys", Action: "probe.edit", Current: true}}}, want: "Help overview current view 1 must not reference an action"},
		{name: "second current reference", overview: HelpOverview{Title: "Keys", Rows: base.Rows, Views: []HelpOverviewView{validView, {Label: "Keys", Context: "example.results", Current: true}}}, want: "Help overview current view 2 must not reference an action"},
		{name: "17 aside lines", overview: HelpOverview{Title: "Keys", Rows: base.Rows, Aside: &HelpOverviewAside{Title: "Aside", Steps: make([]HelpOverviewAsideLine, 17)}}, want: "Help overview aside exceeds 16 lines"},
		{name: "17 combined aside lines", overview: HelpOverview{Title: "Keys", Rows: base.Rows, Aside: &HelpOverviewAside{Title: "Aside", Steps: make([]HelpOverviewAsideLine, 8), Details: make([]HelpOverviewAsideLine, 9)}}, want: "Help overview aside exceeds 16 lines"},
		{name: "second aside label", overview: HelpOverview{Title: "Keys", Rows: base.Rows, Aside: &HelpOverviewAside{Title: "Aside", Steps: []HelpOverviewAsideLine{{Label: "1", Text: "one"}, {Text: "two"}}}}, want: "Help overview aside step 2 label is required"},
		{name: "second aside text", overview: HelpOverview{Title: "Keys", Rows: base.Rows, Aside: &HelpOverviewAside{Title: "Aside", Details: []HelpOverviewAsideLine{{Label: "one", Text: "first"}, {Label: "two"}}}}, want: "Help overview aside detail 2 text is required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := validateHelpOverview(&test.overview, catalog)
			if err == nil || err.Error() != test.want {
				t.Fatalf("error=%v want=%q", err, test.want)
			}
		})
	}
	lines := make([]HelpOverviewAsideLine, 16)
	for index := range lines {
		lines[index] = HelpOverviewAsideLine{Label: fmt.Sprintf("%d", index+1), Text: "line"}
	}
	valid := base
	valid.Aside = &HelpOverviewAside{Title: "Aside", Steps: lines}
	if _, _, err := validateHelpOverview(&valid, catalog); err != nil {
		t.Fatalf("16-line aside boundary rejected: %v", err)
	}
}

func TestFittingFooterMeasuresTerminalCells(t *testing.T) {
	full := strings.Repeat("界", 30)
	if got := fittingFooter(40, full, "compact fits"); got != "compact fits" {
		t.Fatalf("wide-cell full footer selected over fitting compact footer: %q", got)
	}
	if got := fittingFooter(3, "界a", "compact"); got != "界a" {
		t.Fatalf("exact-width full footer=%q", got)
	}
}

func TestHelpOverviewRenderingBoundaries(t *testing.T) {
	p := &probe{name: "example"}
	overview := HelpOverview{
		Title: strings.Repeat("T", 80), Current: "Current view", ViewMenu: "g views",
		Rows: []HelpOverviewRow{{Label: "Search", Context: "example.results", Action: "probe.edit", Summary: "type query"}},
	}
	statusCalls := 0
	overview.Status = func(int) HelpOverviewStatus {
		statusCalls++
		return HelpOverviewStatus{Text: "Codex ● ready", Tone: HelpStatusSuccess}
	}
	s, err := New(Options{Main: p, ConfigDirectory: t.TempDir(), InteractiveHelp: true, HelpOverview: &overview})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	palette, _ := theme.Builtin("terminal")
	context := shell.RenderContext{Theme: palette}

	for _, test := range []struct {
		name          string
		overview      *HelpOverview
		width, height int
	}{
		{name: "nil overview", overview: nil, width: 1, height: 1},
		{name: "zero width", overview: &overview, width: 0, height: 1},
		{name: "zero height", overview: &overview, width: 1, height: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame, frameErr := view.NewFrame(1, 1)
			if frameErr != nil {
				t.Fatal(frameErr)
			}
			s.options.HelpOverview = test.overview
			s.renderHelpOverview(frame, context, test.width, test.height)
		})
	}
	if statusCalls != 0 {
		t.Fatalf("status callback ran %d times for non-renderable overviews", statusCalls)
	}

	s.options.HelpOverview = &overview
	frame, err := view.NewFrame(40, 6)
	if err != nil {
		t.Fatal(err)
	}
	s.renderHelpOverview(frame, context, 40, 6)
	got := fmt.Sprintf("%x", sha256.Sum256([]byte(helpFrameVisualSignature(frame))))
	const want = "a540dfbc5889f1abf892eba1a0bead1d5093f114b22824509282170b4af605b3"
	if got != want {
		t.Fatalf("boundary visual digest=%s want=%s\n%s", got, want, view.ANSI(frame))
	}
}

func helpFrameVisualSignature(frame *view.Frame) string {
	var result strings.Builder
	for y := 0; y < frame.Height(); y++ {
		for x := 0; x < frame.Width(); x++ {
			cell, _ := frame.CellAt(x, y)
			fmt.Fprintf(&result, "%s/%d/%t/%+v|", cell.Text, cell.Width, cell.Continuation, cell.Style)
		}
	}
	return result.String()
}

func helpFrameGeometry(frame *view.Frame) string {
	var result strings.Builder
	for y := 0; y < frame.Height(); y++ {
		for x := 0; x < frame.Width(); x++ {
			cell, _ := frame.CellAt(x, y)
			fmt.Fprintf(&result, "%s/%d/%t|", cell.Text, cell.Width, cell.Continuation)
		}
	}
	return result.String()
}

func helpTextFitObservation(t *testing.T, frame *view.Frame, element string) diagnostics.TextFitObservation {
	t.Helper()
	for _, observation := range frame.TextFit().Observations {
		if observation.Element.String() == element {
			return observation
		}
	}
	t.Fatalf("missing text-fit observation %q", element)
	return diagnostics.TextFitObservation{}
}

func assertHelpFrameUsesPalette(t *testing.T, frame *view.Frame, palette theme.Palette) {
	t.Helper()
	allowed := map[theme.Color]bool{
		"": true, theme.Reset: true,
		palette.Background: true, palette.PanelBackground: true, palette.SidebarBackground: true,
		palette.ActiveRowBackground: true, palette.SelectionBackground: true, palette.Surface: true,
		palette.Overlay: true, palette.Border: true, palette.Text: true, palette.Muted: true,
		palette.Accent: true, palette.Red: true, palette.Green: true, palette.Yellow: true,
		palette.Blue: true, palette.Magenta: true, palette.Cyan: true,
	}
	for y := 0; y < frame.Height(); y++ {
		for x := 0; x < frame.Width(); x++ {
			cell, _ := frame.CellAt(x, y)
			if !allowed[cell.Style.Foreground] || !allowed[cell.Style.Background] {
				t.Fatalf("theme=%s cell=%d,%d style=%+v", palette.ID, x, y, cell.Style)
			}
		}
	}
}

func TestInteractiveHelpClampsSelectionAndPagesCompactRows(t *testing.T) {
	p := &probe{name: "example", extraActions: 12}
	s, err := New(Options{Main: p, ConfigDirectory: t.TempDir(), InteractiveHelp: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.view, s.helpOrigin, s.helpContext = helpScreen, baseScreen, keymap.Context{ID: "example.results"}

	s.helpSelected = -3
	s.openHelpSelection(shell.EventContext{})
	if s.helpSelected != 0 || s.selected.ID != "probe.refresh" {
		t.Fatalf("negative selection=%d action=%q", s.helpSelected, s.selected.ID)
	}
	s.view = helpScreen
	s.helpSelected = 99
	s.openHelpSelection(shell.EventContext{})
	actions := s.helpActions()
	if last := len(actions) - 1; s.helpSelected != last || s.selected.ID != actions[last].ID {
		t.Fatalf("high selection=%d action=%q last=%d", s.helpSelected, s.selected.ID, last)
	}

	layout := responsive.Resolve(responsive.Size{Columns: 40, Rows: 10})
	s.lastResize.Layout = layout
	s.view = helpScreen
	s.helpSelected = 0
	s.helpAction(shell.EventContext{}, "keymap.help.previous")
	if s.helpSelected != 0 {
		t.Fatalf("previous moved before first row: %d", s.helpSelected)
	}
	page := max(1, layout.Render.Rows-4)
	s.helpAction(shell.EventContext{}, "keymap.help.page-next")
	if s.helpSelected != page {
		t.Fatalf("page next selected=%d want=%d", s.helpSelected, page)
	}
	s.helpAction(shell.EventContext{}, "keymap.help.page-previous")
	if s.helpSelected != 0 {
		t.Fatalf("page previous selected=%d want=0", s.helpSelected)
	}

	s.helpSelected = len(actions) - 1
	palette, _ := theme.Builtin("terminal")
	frame, err := s.Render(shell.RenderContext{Layout: layout, Theme: palette})
	if err != nil {
		t.Fatal(err)
	}
	text := view.ANSI(frame)
	if !strings.Contains(text, "> "+actions[len(actions)-1].Label) || strings.Contains(text, "> "+actions[0].Label) {
		t.Fatalf("compact Help did not keep the final selection visible: %q", text)
	}

	s.helpSelected = 99
	lines := s.helpLines()
	if !strings.HasPrefix(lines[len(lines)-1], "> ") || strings.HasPrefix(lines[0], "> ") {
		t.Fatalf("rendered selection was not clamped to the final row: %q", lines)
	}
}

func TestInteractiveHelpOptionsRejectInvalidCommandsAndLegacyDefaultsStayUnchanged(t *testing.T) {
	p := &probe{name: "example"}
	for name, commands := range map[string][]HelpCommand{
		"empty label":    {{ID: "check", Action: "probe.refresh", Defaults: []keymap.Sequence{{"k"}}}},
		"empty action":   {{ID: "check", Label: "check", Defaults: []keymap.Sequence{{"k"}}}},
		"unknown target": {{ID: "check", Label: "check", Action: "missing", Defaults: []keymap.Sequence{{"k"}}}},
		"multi stroke":   {{ID: "check", Label: "check", Action: "probe.refresh", Defaults: []keymap.Sequence{{"g", "k"}}}},
		"duplicate ID": {
			{ID: "check", Label: "check", Action: "probe.refresh", Defaults: []keymap.Sequence{{"k"}}},
			{ID: "check", Label: "again", Action: "probe.edit", Defaults: []keymap.Sequence{{"r"}}},
		},
		"colliding keys": {
			{ID: "check", Label: "check", Action: "probe.refresh", Defaults: []keymap.Sequence{{"k"}}},
			{ID: "reset", Label: "reset", Action: "probe.edit", Defaults: []keymap.Sequence{{"k"}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if surface, err := New(Options{Main: p, ConfigDirectory: t.TempDir(), InteractiveHelp: true, HelpCommands: commands}); err == nil {
				_ = surface.Close()
				t.Fatal("invalid Help commands were accepted")
			}
		})
	}
	legacy, err := New(Options{Main: p, ConfigDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if got := legacy.runtime.Effective("keymap.help", "keymap.help.back"); !slices.ContainsFunc(got, func(sequence keymap.Sequence) bool { return reflect.DeepEqual(sequence, keymap.Sequence{"enter"}) }) {
		t.Fatalf("legacy Enter-to-close changed: %v", got)
	}
	if got := legacy.runtime.Effective("keymap.help", "keymap.help.previous"); !slices.ContainsFunc(got, func(sequence keymap.Sequence) bool { return reflect.DeepEqual(sequence, keymap.Sequence{"k"}) }) {
		t.Fatalf("legacy k-to-scroll changed: %v", got)
	}
	if surface, err := New(Options{Main: p, ConfigDirectory: t.TempDir(), InteractiveHelp: true, HelpOpenedAction: "missing"}); err == nil {
		_ = surface.Close()
		t.Fatal("unknown Help-opened action was accepted")
	}
	if surface, err := New(Options{Main: p, ConfigDirectory: t.TempDir(), HelpOpenedAction: "probe.refresh"}); err == nil {
		_ = surface.Close()
		t.Fatal("Help extension without interactive Help was accepted")
	}
}

func TestTabUsesDeclaredActionsAndHelpRetainsDefaultKeys(t *testing.T) {
	p := &probe{name: "example", ownTab: true}
	s, err := New(Options{Main: p, ConfigDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	d := runUIForTest(t, s, p)
	d.key(tea.KeyTab)
	d.wait(t, func(o observation) bool { return o.editing })
	d.typeText("preserved")
	d.wait(t, func(o observation) bool { return o.text == "preserved" })
	d.key(tea.KeyEnter)
	d.wait(t, func(o observation) bool { return !o.editing })
	openHelp := func() {
		d.key(tea.KeyCtrlK)
		d.wait(t, func(o observation) bool { return o.view == actionsScreen && !o.pending })
		d.choose(t, "keymap.help", "Help and effective shortcuts")
		d.wait(t, func(o observation) bool { return o.view == helpScreen })
	}
	openHelp()
	d.typeText("?")
	d.wait(t, func(o observation) bool { return o.view == baseScreen && o.text == "preserved" })
	openHelp()
	d.typeText("q")
	select {
	case err := <-d.done:
		d.stopped = true
		_ = d.input.Close()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("q did not quit from shared help")
	}
}

func TestCheckerAndHelpRenderAtSupportedSizesAndThemes(t *testing.T) {
	s, _ := uiForTest(t, t.TempDir(), false)
	defer s.Close()
	s.openChecker()
	s.checkerKeys = []string{"ctrl+alt+k"}
	for _, id := range theme.IDs() {
		palette, _ := theme.Builtin(id)
		for _, size := range []responsive.Size{{Columns: 40, Rows: 10}, {Columns: 80, Rows: 19}, {Columns: 120, Rows: 30}} {
			layout := responsive.Resolve(size)
			frame, err := s.Render(shell.RenderContext{Layout: layout, Theme: palette})
			if err != nil || frame.Width() != size.Columns || frame.Height() != size.Rows {
				t.Fatalf("%s %v: %v", id, size, err)
			}
			text := view.ANSI(frame)
			for _, want := range []string{"KEY CHECKER", "ctrl+alt+k", "Esc Esc within 1s return", "Ctrl-C quit"} {
				if !strings.Contains(text, want) {
					t.Fatalf("%s %v missing %q", id, size, want)
				}
			}
		}
	}
	s.helpContext = keymap.Context{ID: "example.results"}
	s.view = helpScreen
	s.helpOffset = 1000
	palette, _ := theme.Builtin("terminal")
	frame, err := s.Render(shell.RenderContext{Layout: responsive.Resolve(responsive.Size{Columns: 40, Rows: 10}), Theme: palette})
	if err != nil || !strings.Contains(view.ANSI(frame), "Key checker") {
		t.Fatal("help cannot reach final entries", err)
	}
}

func TestDebugShortcutDoesNotStealTypingOrPaste(t *testing.T) {
	main, debug := &probe{name: "main"}, &probe{name: "debug"}
	s, err := New(Options{Main: main, Debug: debug, ConfigDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	d := runUIForTest(t, s, main)
	d.typeText("/d")
	typed := d.wait(t, func(o observation) bool { return o.text == "d" })
	if typed.debug {
		t.Fatal("plain d stole typing")
	}
	d.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d"), Alt: true})
	d.wait(t, func(o observation) bool { return o.debug })
	d.typeText("/")
	d.wait(t, func(o observation) bool { return o.debug && o.editing })
	d.typeText("d")
	d.program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d"), Alt: true, Paste: true})
	// A normal editing key supplies an ordered observation after both text messages.
	d.key(tea.KeyEnter)
	state := d.wait(t, func(o observation) bool { return o.debug && !o.editing })
	if state.text != "d" {
		t.Fatal("main query changed while debug was active")
	}
	d.typeText("d")
	returned := d.wait(t, func(o observation) bool { return !o.debug })
	if !returned.editing || returned.text != "d" {
		t.Fatal("debug toggle discarded main query")
	}
}

func TestRealPickerClearedDefaultAndSequence(t *testing.T) {
	picker, err := interaction.NewPicker(interaction.PickerOptions{Title: "Example", Load: func(context.Context, string, interaction.Cursor) (interaction.Page, error) {
		return interaction.Page{Items: []interaction.Item{{Key: "first", Label: "First"}, {Key: "second", Label: "Second"}}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Main: picker, ConfigDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Runtime().Apply(keymap.Document{Version: 1, Bindings: []keymap.Binding{{Context: "interaction.picker.results", Action: "picker.next", Sequences: []keymap.Sequence{{"g", "r"}}}}}); err != nil {
		t.Fatal(err)
	}
	d := runUIForTest(t, s, &probe{})
	d.wait(t, func(o observation) bool { return o.selected == "first" && !o.pending })
	d.typeText("j/")
	state := d.wait(t, func(o observation) bool { return o.editing })
	if state.selected != "first" {
		t.Fatal("cleared j used the raw fallback")
	}
	d.key(tea.KeyEnter)
	d.wait(t, func(o observation) bool { return !o.editing })
	d.typeText("gr")
	d.wait(t, func(o observation) bool { return o.selected == "second" })
}

func TestActionsRejectChangedOriginTarget(t *testing.T) {
	s, p := uiForTest(t, t.TempDir(), false)
	p.changeAfterRefresh = true
	d := runUIForTest(t, s, p)
	d.key(tea.KeyF5)
	d.key(tea.KeyCtrlK)
	state := d.wait(t, func(o observation) bool { return o.view == actionsScreen && !o.pending })
	if state.revision != 0 {
		t.Fatal("background change arrived before menu opened")
	}
	d.wait(t, func(o observation) bool { return o.revision == 1 })
	d.choose(t, "probe.refresh", "Refresh")
	state = d.wait(t, func(o observation) bool { return strings.Contains(o.status, "Target changed") })
	if state.calls != 1 || state.view != actionsScreen {
		t.Fatal("stale menu invoked action")
	}
}

func TestActionsPaletteShowsDisabledActionReason(t *testing.T) {
	s, p := uiForTest(t, t.TempDir(), false)
	p.disableRefresh = true
	d := runUIForTest(t, s, p)
	d.key(tea.KeyCtrlK)
	state := d.wait(t, func(o observation) bool { return o.view == actionsScreen && !o.pending })
	if !strings.Contains(state.frame, "× Refresh example · current selection is unavailable") {
		t.Fatalf("disabled reason missing from Actions palette: %q", state.frame)
	}
	if state.selected != "probe.edit" || state.calls != 0 {
		t.Fatalf("disabled action remained selectable or was invoked: selected=%q calls=%d", state.selected, state.calls)
	}
}

func TestHelpEndThenUpMovesVisibleContent(t *testing.T) {
	s, _ := uiForTest(t, t.TempDir(), false)
	defer s.Close()
	layout := responsive.Resolve(responsive.Size{Columns: 40, Rows: 10})
	s.lastResize = shell.ResizeEvent{Generation: 1, Layout: layout}
	s.helpContext = keymap.Context{ID: "example.results"}
	s.view = helpScreen
	palette, _ := theme.Builtin("terminal")
	context := shell.RenderContext{Layout: layout, Theme: palette}
	s.Update(shell.EventContext{}, shell.ActionEvent{ID: "keymap.help.end"})
	bottom, err := s.Render(context)
	if err != nil {
		t.Fatal(err)
	}
	s.Update(shell.EventContext{}, shell.ActionEvent{ID: "keymap.help.previous"})
	previous, err := s.Render(context)
	if err != nil {
		t.Fatal(err)
	}
	if view.ANSI(bottom) == view.ANSI(previous) {
		t.Fatal("Up after End did not move help content")
	}
	s.draftOpen = true
	s.draft = keymap.Document{Version: 1, Bindings: []keymap.Binding{{Context: "example.results", Action: "probe.refresh", Sequences: []keymap.Sequence{{"/"}}}}}
	s.Update(shell.EventContext{}, shell.ActionEvent{ID: "keymap.help.home"})
	conflict, err := s.Render(context)
	if err != nil || !strings.Contains(view.ANSI(conflict), "Draft conflict") {
		t.Fatal("conflict details unavailable", err)
	}
}

func TestEditorSearchStaysVisibleBesideStatus(t *testing.T) {
	s, p := uiForTest(t, t.TempDir(), false)
	d := runUIForTest(t, s, p)
	d.key(tea.KeyCtrlK)
	d.wait(t, func(o observation) bool { return o.view == actionsScreen && !o.pending })
	d.choose(t, "keymap.editor", "Keyboard shortcuts")
	d.wait(t, func(o observation) bool { return o.view == editorScreen && !o.pending })
	d.program.Send(tea.WindowSizeMsg{Width: 40, Height: 10})
	d.typeText("/Refresh example")
	state := d.wait(t, func(o observation) bool {
		return o.view == editorScreen && o.editing && !o.pending && o.selected == "example.results/probe.refresh"
	})
	if !strings.Contains(state.frame, "Search: Refresh example") || !strings.Contains(state.frame, "Changes stay in the draft") {
		t.Fatal("search or status hidden at 40x10", state.frame)
	}
}
