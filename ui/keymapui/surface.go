package keymapui

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/documentstore"
	"github.com/raulfrk/herdr-plugin-kit/ui/interaction"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

type MappedSurface interface {
	shell.Surface
	shell.ActionSurface
}
type Options struct {
	Main            MappedSurface
	Debug           MappedSurface
	ConfigDirectory string
	DefaultKeymap   bool
	StartDebug      bool
}

type screen uint8

const (
	baseScreen screen = iota
	actionsScreen
	editorScreen
	entryScreen
	textScreen
	captureScreen
	checkerScreen
	helpScreen
)

type Surface struct {
	options                                   Options
	runtime                                   *keymap.Runtime
	configuration                             *configuration
	menu, editor, entry                       *list
	view                                      screen
	showingDebug, actionsFocused              bool
	lastResize                                shell.ResizeEvent
	menuOrigin                                screen
	menuContext                               keymap.Context
	menuFocused                               bool
	editorOrigin                              screen
	checkerOrigin, helpOrigin                 screen
	helpContext                               keymap.Context
	helpOffset                                int
	draftOpen                                 bool
	draft                                     keymap.Document
	draftRevision                             documentstore.Revision
	selected                                  keymap.Action
	alias                                     int
	text                                      interaction.Buffer
	captured                                  keymap.Sequence
	checkerKeys                               []string
	checkerPaste                              bool
	lastEscape                                time.Time
	now                                       func() time.Time
	disk                                      diskState
	latestValid                               keymap.Document
	warning, status                           string
	started, saving, loading, reloadRequested bool
}

var ioKey, _ = shell.NewRequestKey("keymap.io")
var pollCode, _ = shell.NewEventCode("keymap.poll")

const pollInterval = 500 * time.Millisecond

func New(options Options) (*Surface, error) {
	if options.Main == nil {
		return nil, errors.New("keymap main surface is nil")
	}
	if options.StartDebug && options.Debug == nil {
		return nil, errors.New("keymap debug surface is nil")
	}
	s := &Surface{options: options, showingDebug: options.StartDebug, now: time.Now}
	var err error
	if s.menu, err = newList("ACTIONS", "keymap.actions"); err != nil {
		return nil, err
	}
	if s.editor, err = newList("KEYBOARD SHORTCUTS", "keymap.editor"); err != nil {
		return nil, err
	}
	if s.entry, err = newList("EDIT SHORTCUT", "keymap.entry"); err != nil {
		return nil, err
	}
	if s.runtime, err = keymap.New(s.catalog()); err != nil {
		return nil, err
	}
	if s.configuration, err = openConfiguration(options.ConfigDirectory, s.runtime); err != nil {
		return nil, err
	}
	s.disk = s.configuration.read(context.Background())
	s.warning = s.disk.warning
	s.latestValid = s.runtime.Document()
	if s.disk.valid {
		s.latestValid = keymap.Clone(s.disk.document)
	}
	if s.disk.valid && !options.DefaultKeymap {
		_ = s.runtime.Apply(s.disk.document)
	}
	return s, nil
}

func (s *Surface) Close() error {
	s.checkerKeys = nil
	s.captured = nil
	s.text.Set("")
	return s.configuration.store.Close()
}
func (s *Surface) Runtime() *keymap.Runtime       { return s.runtime }
func (s *Surface) KeymapCatalog() []keymap.Action { return s.runtime.Catalog() }

func (s *Surface) base() MappedSurface {
	if s.showingDebug && s.options.Debug != nil {
		return s.options.Debug
	}
	return s.options.Main
}
func (s *Surface) activePicker() *interaction.Picker {
	switch s.view {
	case actionsScreen:
		return s.menu.picker
	case editorScreen:
		return s.editor.picker
	case entryScreen:
		return s.entry.picker
	}
	return nil
}
func (s *Surface) contentContext() keymap.Context {
	if picker := s.activePicker(); picker != nil {
		return picker.KeymapContext()
	}
	switch s.view {
	case textScreen:
		return keymap.Context{ID: "keymap.text", Editing: true, Target: actionKey(s.selected)}
	case captureScreen:
		return keymap.Context{ID: "keymap.capture", Target: actionKey(s.selected)}
	case checkerScreen:
		return keymap.Context{ID: "keymap.checker"}
	case helpScreen:
		return keymap.Context{ID: "keymap.help", Target: s.helpContext.ID + s.helpContext.Target}
	default:
		return s.base().KeymapContext()
	}
}
func (s *Surface) KeymapContext() keymap.Context {
	if s.actionsFocused {
		return keymap.Context{ID: "keymap.actions-control", Target: s.contentContext().Target}
	}
	return s.contentContext()
}

func (s *Surface) catalog() []keymap.Action {
	catalog := append([]keymap.Action(nil), s.options.Main.KeymapCatalog()...)
	if s.options.Debug != nil {
		catalog = append(catalog, s.options.Debug.KeymapCatalog()...)
	}
	for _, l := range []*list{s.menu, s.editor, s.entry} {
		catalog = append(catalog, l.picker.KeymapCatalog()...)
	}
	helpDefaults := map[string][]keymap.Sequence{}
	filtered := catalog[:0]
	for _, action := range catalog {
		if action.ID == "picker.help" || action.ID == "debug.help" {
			helpDefaults[action.Context] = action.Defaults
			continue
		}
		filtered = append(filtered, action)
	}
	catalog = filtered
	add := func(id, label, context string, editing, quick bool, keys ...string) {
		a := keymap.Action{ID: id, Label: label, Context: context, Editing: editing, Quick: quick}
		for _, key := range keys {
			a.Defaults = append(a.Defaults, keymap.Sequence{key})
		}
		catalog = append(catalog, a)
	}
	for _, a := range []struct{ id, label, key string }{{"finish", "Use sequence", "enter"}, {"cancel", "Cancel", "escape"}, {"backspace", "Delete before cursor", "backspace"}, {"delete", "Delete at cursor", "delete"}, {"left", "Move cursor left", "left"}, {"right", "Move cursor right", "right"}, {"home", "Move to start", "home"}, {"end", "Move to end", "end"}} {
		add("keymap.text."+a.id, a.label, "keymap.text", true, false, a.key)
	}
	for _, a := range []struct {
		id, label string
		keys      []string
	}{{"back", "Close help", []string{"?", "escape", "backspace", "enter"}}, {"quit", "Quit", []string{"q"}}, {"next", "Scroll down", []string{"down", "j"}}, {"previous", "Scroll up", []string{"up", "k"}}, {"page-next", "Next page", []string{"page-down"}}, {"page-previous", "Previous page", []string{"page-up"}}, {"home", "First shortcut", []string{"home"}}, {"end", "Last shortcut", []string{"end"}}} {
		add("keymap.help."+a.id, a.label, "keymap.help", false, false, a.keys...)
	}
	// Tab traverses the existing controls and the appended Actions control.
	// Its behavior is one ordinary remappable action, never a hidden raw fallback.
	contexts := map[string]bool{"keymap.actions-control": false}
	var order []string
	for i, a := range catalog {
		if _, ok := contexts[a.Context]; !ok {
			order = append(order, a.Context)
		}
		contexts[a.Context] = a.Editing
		var defaults []keymap.Sequence
		for _, sequence := range a.Defaults {
			if len(sequence) == 1 {
				key, _ := keymap.Normalize(sequence[0])
				if key == "tab" {
					continue
				}
			}
			defaults = append(defaults, sequence)
		}
		catalog[i].Defaults = defaults
	}
	order = append(order, "keymap.actions-control")
	for _, context := range order {
		editing := contexts[context]
		keys := []string{"ctrl+k"}
		if !editing {
			keys = append(keys, ":")
		}
		if context == "keymap.actions-control" {
			keys = append(keys, "enter")
		}
		add("keymap.actions", "Actions", context, editing, false, keys...)
		add("keymap.focus-next", "Next control", context, editing, false, "tab")
		add("keymap.editor", "Keyboard shortcuts", context, editing, true)
		add("keymap.checker", "Key checker", context, editing, true)
		var helpKeys []string
		for _, sequence := range helpDefaults[context] {
			if len(sequence) == 1 {
				helpKeys = append(helpKeys, sequence[0])
			}
		}
		add("keymap.help", "Help and effective shortcuts", context, editing, true, helpKeys...)
		if s.options.Debug != nil {
			keys = []string{"alt+d"}
			if !editing {
				keys = append(keys, "d")
			}
			add("keymap.debug", "Debug UI", context, editing, true, keys...)
		}
		if strings.HasPrefix(context, "keymap.editor.") || strings.HasPrefix(context, "keymap.entry.") || context == "keymap.text" {
			add("keymap.save", "Save shortcut changes", context, editing, true, "ctrl+s")
			add("keymap.discard", "Discard shortcut changes", context, editing, true)
			add("keymap.reload", "Reload shortcuts", context, editing, true)
			add("keymap.reset-all", "Reset all shortcuts", context, editing, true)
		}
	}
	return catalog
}

func (s *Surface) ActionState(id string) keymap.State {
	known := false
	context := s.KeymapContext().ID
	for _, a := range s.runtime.Catalog() {
		if a.Context == context && a.ID == id {
			known = true
			break
		}
	}
	if !known {
		return keymap.State{Reason: "Unavailable in this view"}
	}
	switch id {
	case "keymap.actions", "keymap.focus-next", "keymap.editor", "keymap.checker", "keymap.help", "keymap.debug", "keymap.save", "keymap.discard", "keymap.reload", "keymap.reset-all",
		"keymap.text.finish", "keymap.text.cancel", "keymap.text.backspace", "keymap.text.delete", "keymap.text.left", "keymap.text.right", "keymap.text.home", "keymap.text.end",
		"keymap.help.back", "keymap.help.quit", "keymap.help.next", "keymap.help.previous", "keymap.help.page-next", "keymap.help.page-previous", "keymap.help.home", "keymap.help.end":
		switch id {
		case "keymap.checker":
			if s.saving {
				return keymap.State{Reason: "Saving changes; please wait"}
			}
		case "keymap.save":
			if !s.draftOpen || s.saving {
				return keymap.State{Reason: "No editable draft"}
			}
			if s.view == textScreen {
				return keymap.State{Reason: "Finish or cancel sequence entry first"}
			}
			if s.draftRevision != s.disk.revision {
				return keymap.State{Reason: "File changed; reload or discard"}
			}
			if err := s.runtime.Validate(s.draft); err != nil {
				return keymap.State{Reason: err.Error()}
			}
		case "keymap.discard", "keymap.reload", "keymap.reset-all":
			if !s.draftOpen || s.saving {
				return keymap.State{Reason: "No editable draft"}
			}
		}
		return keymap.State{Enabled: true}
	}
	if picker := s.activePicker(); picker != nil {
		if s.saving && s.draftOpen {
			return keymap.State{Reason: "Saving changes; please wait"}
		}
		if id == interaction.ActionOpen || id == interaction.ActionActivate {
			_, ok := picker.Selected()
			return keymap.State{Enabled: ok && !picker.DiagnosticState().Pending, Reason: "No available selection"}
		}
		return picker.ActionState(id)
	}
	return s.base().ActionState(id)
}

func (s *Surface) Update(events shell.EventContext, event shell.Event) []shell.Effect {
	var effects []shell.Effect
	if resize, ok := event.(shell.ResizeEvent); ok {
		if resize.Generation > s.lastResize.Generation {
			s.lastResize = resize
		}
		if !s.started {
			s.started = true
			effects = append(effects, s.pollLater(events)...)
		}
	}
	if action, ok := event.(shell.ActionEvent); ok {
		return s.action(events, action.ID)
	}
	if result, ok := event.(shell.ResultEvent); ok && result.Key == ioKey {
		return s.finishIO(events, result)
	}
	if timer, ok := event.(shell.TimerEvent); ok && timer.Code == pollCode {
		if !s.saving && !s.loading {
			return s.load(events)
		}
		return s.pollLater(events)
	}
	if text, ok := event.(shell.TextEvent); ok && s.view == textScreen {
		s.text.Insert(text.Text)
		return nil
	}
	switch event.(type) {
	case shell.ResultEvent, shell.TimerEvent:
		effects = append(effects, s.options.Main.Update(events, event)...)
		if s.options.Debug != nil {
			effects = append(effects, s.options.Debug.Update(events, event)...)
		}
		for _, l := range []*list{s.menu, s.editor, s.entry} {
			effects = append(effects, l.picker.Update(events, event)...)
		}
		return effects
	}
	if picker := s.activePicker(); picker != nil {
		effects = append(effects, picker.Update(events, event)...)
	} else if s.view == baseScreen {
		effects = append(effects, s.base().Update(events, event)...)
	}
	return effects
}

func (s *Surface) resizeActive(events shell.EventContext) []shell.Effect {
	if s.lastResize.Generation == 0 {
		return nil
	}
	if picker := s.activePicker(); picker != nil {
		return picker.Update(events, s.lastResize)
	}
	if s.view == baseScreen {
		return s.base().Update(events, s.lastResize)
	}
	return nil
}

func (s *Surface) action(events shell.EventContext, id string) []shell.Effect {
	if state := s.ActionState(id); !state.Enabled {
		s.status = state.Reason
		return nil
	}
	switch id {
	case "keymap.actions":
		return s.openActions(events)
	case "keymap.focus-next":
		return s.focusNext(events)
	case "keymap.editor":
		return s.openEditor(events)
	case "keymap.checker":
		s.openChecker()
		return nil
	case "keymap.help":
		s.helpOrigin = s.view
		s.helpContext = s.contentContext()
		s.helpOffset = 0
		s.view = helpScreen
		s.actionsFocused = false
		return nil
	case "keymap.debug":
		s.showingDebug = !s.showingDebug
		s.view = baseScreen
		s.actionsFocused = false
		return s.resizeActive(events)
	case "keymap.save":
		return s.save(events)
	case "keymap.discard":
		return s.closeEditor(events)
	case "keymap.reload":
		s.reloadRequested = true
		return s.load(events)
	case "keymap.reset-all":
		s.draft = keymap.Document{Version: 1}
		s.status = "Defaults restored in draft; save to apply."
		return s.refreshEditor(events)
	}
	if s.view == helpScreen {
		return s.helpAction(events, id)
	}
	if s.view == textScreen {
		return s.textAction(events, id)
	}
	if picker := s.activePicker(); picker != nil {
		if id == interaction.ActionOpen || id == interaction.ActionActivate {
			return s.activateRow(events)
		}
		if id == "picker.quit" || id == "picker.detail-back" || id == "picker.help-back" {
			return s.back(events)
		}
		return picker.Update(events, shell.ActionEvent{ID: id})
	}
	return s.base().Update(events, shell.ActionEvent{ID: id})
}

func (s *Surface) focusNext(events shell.EventContext) []shell.Effect {
	if s.actionsFocused {
		s.actionsFocused = false
		return nil
	}
	if s.view == helpScreen || s.view == textScreen {
		s.actionsFocused = true
		return nil
	}
	var child MappedSurface = s.base()
	if picker := s.activePicker(); picker != nil {
		child = picker
	}
	before, visual := child.KeymapContext(), child.DiagnosticState()
	var effects []shell.Effect
traversal:
	for _, action := range child.KeymapCatalog() {
		if action.Context != before.ID || !child.ActionState(action.ID).Enabled {
			continue
		}
		for _, sequence := range action.Defaults {
			if len(sequence) != 1 {
				continue
			}
			if stroke, err := keymap.Normalize(sequence[0]); err == nil && stroke == "tab" {
				effects = child.Update(events, shell.ActionEvent{ID: action.ID})
				break traversal
			}
		}
	}
	after := child.KeymapContext()
	if before.Editing && !after.Editing || before.Editing && after.Editing && child.DiagnosticState().SelectedIndex <= visual.SelectedIndex || !before.Editing && !after.Editing {
		s.actionsFocused = true
	}
	return effects
}

func (s *Surface) CapturingKeys() bool { return s.view == captureScreen || s.view == checkerScreen }
func (s *Surface) CaptureKey(events shell.EventContext, key shell.CapturedKey) []shell.Effect {
	if s.view == checkerScreen {
		if key.Paste {
			s.checkerPaste = true
			s.lastEscape = time.Time{}
			return nil
		}
		now := s.now()
		if key.Stroke == "escape" && !s.lastEscape.IsZero() && now.Sub(s.lastEscape) <= time.Second {
			return s.closeChecker(events)
		}
		s.lastEscape = time.Time{}
		if key.Stroke == "escape" {
			s.lastEscape = now
		}
		s.checkerPaste = false
		s.checkerKeys = append(s.checkerKeys, key.Stroke)
		if len(s.checkerKeys) > 8 {
			s.checkerKeys = append([]string(nil), s.checkerKeys[len(s.checkerKeys)-8:]...)
		}
		return nil
	}
	if s.view != captureScreen {
		return nil
	}
	if key.Paste {
		s.status = "Paste ignored while recording."
		return nil
	}
	switch key.Stroke {
	case "escape":
		s.captured = nil
		s.view = entryScreen
		return s.resizeActive(events)
	case "enter":
		if len(s.captured) == 0 {
			s.status = "Press a key first."
			return nil
		}
		return s.useSequence(events, s.captured)
	default:
		s.captured = append(s.captured, key.Stroke)
	}
	return nil
}

func (s *Surface) openChecker() {
	s.checkerOrigin = s.view
	s.view = checkerScreen
	s.actionsFocused = false
	s.checkerKeys = nil
	s.checkerPaste = false
	s.lastEscape = time.Time{}
	s.runtime.Cancel()
}
func (s *Surface) closeChecker(events shell.EventContext) []shell.Effect {
	s.checkerKeys = nil
	s.checkerPaste = false
	s.lastEscape = time.Time{}
	s.view = s.checkerOrigin
	if !s.draftOpen {
		s.applyLatest()
	}
	return s.resizeActive(events)
}

func (s *Surface) DiagnosticState() diagnostics.VisualState {
	if s.view == baseScreen && !s.actionsFocused {
		state := s.base().DiagnosticState()
		state.HasError = state.HasError || s.warning != ""
		return state
	}
	screenID, _ := diagnostics.NewID("keymap")
	state, _ := diagnostics.NewID("ready")
	if s.saving {
		state, _ = diagnostics.NewID("saving")
	}
	return diagnostics.VisualState{Screen: screenID, State: state, ResizeGeneration: s.lastResize.Generation, Pending: s.saving || s.loading, HasError: s.warning != "", ItemCount: len(s.runtime.Catalog())}
}

func (s *Surface) SuppressEventDiagnostics(event shell.Event) bool {
	if timer, ok := event.(shell.TimerEvent); ok && timer.Code == pollCode {
		return true
	}
	if result, ok := event.(shell.ResultEvent); ok && result.Key == ioKey {
		if value, ok := result.Result.Value.(ioResult); ok && !value.save {
			return value.disk.revision == s.disk.revision && value.disk.warning == s.disk.warning
		}
	}
	for _, child := range []MappedSurface{s.options.Main, s.options.Debug} {
		if filter, ok := child.(shell.DiagnosticFilter); ok && filter.SuppressEventDiagnostics(event) {
			return true
		}
	}
	return false
}
func (s *Surface) SuppressEffectDiagnostics(effect shell.Effect) bool {
	if effect.EventCode() == pollCode || effect.RequestKey() == ioKey {
		return true
	}
	for _, child := range []MappedSurface{s.options.Main, s.options.Debug} {
		if filter, ok := child.(shell.DiagnosticFilter); ok && filter.SuppressEffectDiagnostics(effect) {
			return true
		}
	}
	return false
}

func (s *Surface) helpAction(events shell.EventContext, id string) []shell.Effect {
	switch id {
	case "keymap.help.quit":
		return []shell.Effect{shell.Quit()}
	case "keymap.help.back":
		s.view = s.helpOrigin
		return s.resizeActive(events)
	case "keymap.help.next":
		s.helpOffset++
	case "keymap.help.previous":
		s.helpOffset--
	case "keymap.help.page-next":
		s.helpOffset += max(1, s.lastResize.Layout.Render.Rows-4)
	case "keymap.help.page-previous":
		s.helpOffset -= max(1, s.lastResize.Layout.Render.Rows-4)
	case "keymap.help.home":
		s.helpOffset = 0
	case "keymap.help.end":
		s.helpOffset = len(view.Wrap(strings.Join(s.helpLines(), "\n"), max(0, s.lastResize.Layout.Render.Columns-2)))
	}
	rows := view.Wrap(strings.Join(s.helpLines(), "\n"), max(0, s.lastResize.Layout.Render.Columns-2))
	s.helpOffset = min(max(0, s.helpOffset), max(0, len(rows)-max(1, s.lastResize.Layout.Render.Rows-3)))
	return nil
}

func (s *Surface) textAction(events shell.EventContext, id string) []shell.Effect {
	switch id {
	case "keymap.text.cancel":
		s.text.Set("")
		s.view = entryScreen
		return s.resizeActive(events)
	case "keymap.text.finish":
		var sequence keymap.Sequence
		for _, input := range strings.Fields(s.text.Text()) {
			stroke, err := keymap.Normalize(input)
			if err != nil || stroke == "ctrl+c" {
				s.status = "Unsupported or reserved key. Use Key checker to inspect received input."
				return nil
			}
			sequence = append(sequence, stroke)
		}
		if len(sequence) == 0 {
			s.status = "Enter at least one key."
			return nil
		}
		return s.useSequence(events, sequence)
	case "keymap.text.backspace":
		s.text.Backspace()
	case "keymap.text.delete":
		s.text.Delete()
	case "keymap.text.left":
		s.text.MoveLeft()
	case "keymap.text.right":
		s.text.MoveRight()
	case "keymap.text.home":
		s.text.MoveHome()
	case "keymap.text.end":
		s.text.MoveEnd()
	}
	return nil
}

func (s *Surface) useSequence(events shell.EventContext, sequence keymap.Sequence) []shell.Effect {
	sequences, _ := draftSequences(s.draft, s.selected)
	next := make([]keymap.Sequence, len(sequences))
	copy(next, sequences)
	if s.alias >= 0 && s.alias < len(next) {
		next[s.alias] = append(keymap.Sequence(nil), sequence...)
	} else {
		next = append(next, append(keymap.Sequence(nil), sequence...))
	}
	setBinding(&s.draft, s.selected, next)
	s.captured = nil
	s.text.Set("")
	s.view = entryScreen
	s.status = "Draft updated; save to apply."
	if err := s.runtime.Validate(s.draft); err != nil {
		s.status = err.Error()
	}
	return s.refreshEntry(events)
}

var _ MappedSurface = (*Surface)(nil)
var _ shell.KeyCapture = (*Surface)(nil)
