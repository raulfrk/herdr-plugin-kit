package keymapui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	name                                            string
	editing                                         bool
	revision, invocations, resizes, results, timers int
	text                                            string
	suppress                                        bool
}

func (p *probe) KeymapCatalog() []keymap.Action {
	return []keymap.Action{
		{ID: "probe.refresh", Label: "Refresh example", Context: p.name + ".results", Quick: true, Defaults: []keymap.Sequence{{"g", "r"}, {"f5"}}},
		{ID: "probe.edit", Label: "Edit query", Context: p.name + ".results", Quick: true, Defaults: []keymap.Sequence{{"/"}, {"tab"}}},
		{ID: "probe.finish", Label: "Finish query", Context: p.name + ".editing", Editing: true, Defaults: []keymap.Sequence{{"enter"}, {"escape"}, {"tab"}}},
	}
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
		frame.PutText(0, 3, "example content", view.Style{})
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
	d.key(tea.KeyTab)
	d.wait(t, func(o observation) bool { return o.editing })
	d.key(tea.KeyTab)
	d.wait(t, func(o observation) bool { return strings.Contains(o.frame, "[Actions]") })
	d.key(tea.KeyEnter)
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

func TestTabUsesDeclaredActionsAndHelpRetainsDefaultKeys(t *testing.T) {
	s, p := uiForTest(t, t.TempDir(), false)
	d := runUIForTest(t, s, p)
	d.key(tea.KeyTab)
	d.wait(t, func(o observation) bool { return o.editing })
	d.typeText("preserved")
	d.wait(t, func(o observation) bool { return o.text == "preserved" })
	d.key(tea.KeyTab)
	d.wait(t, func(o observation) bool { return !o.editing && strings.Contains(o.frame, "[Actions]") })
	d.key(tea.KeyTab)
	d.wait(t, func(o observation) bool { return !o.editing && !strings.Contains(o.frame, "[Actions]") })
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
