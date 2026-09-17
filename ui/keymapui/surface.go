package keymapui

import (
	"context"
	"errors"
	"fmt"
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

func (s *Surface) childOwnsTab(context keymap.Context) bool {
	for _, child := range []MappedSurface{s.options.Main, s.options.Debug} {
		if child == nil {
			continue
		}
		for _, action := range child.KeymapCatalog() {
			if action.Context != context.ID || action.Editing != context.Editing {
				continue
			}
			for _, sequence := range action.Defaults {
				if len(sequence) != 1 {
					continue
				}
				key, _ := keymap.Normalize(sequence[0])
				if key == "tab" {
					return true
				}
			}
		}
	}
	return false
}

type Options struct {
	Main             MappedSurface
	Debug            MappedSurface
	ConfigDirectory  string
	DefaultKeymap    bool
	StartDebug       bool
	InteractiveHelp  bool
	HelpOpenedAction string
	HelpCommands     []HelpCommand
	HelpOverview     *HelpOverview
}

// HelpCommand adds one consumer-owned action to interactive generated Help.
// Action is dispatched to Main after Help closes; the Kit retains ownership of
// shortcut validation and the Help lifecycle.
type HelpCommand struct {
	ID       string
	Label    string
	Action   string
	Defaults []keymap.Sequence
}

// HelpOverview replaces the generic interactive Help list with a consumer-
// described overview while the Kit retains binding resolution and editing.
// Status must be read-only and non-blocking because it runs during rendering.
type HelpOverview struct {
	Title, Current, ViewMenu string
	Description, Footer      string
	CompactFooter            string
	InitialRow               int
	Status                   func(columns int) HelpOverviewStatus
	Views                    []HelpOverviewView
	Rows                     []HelpOverviewRow
	Aside                    *HelpOverviewAside
}

type HelpOverviewStatus struct {
	Text string
	Tone HelpOverviewStatusTone
}

type HelpOverviewStatusTone uint8

const (
	HelpStatusNeutral HelpOverviewStatusTone = iota
	HelpStatusSuccess
	HelpStatusWarning
	HelpStatusError
)

// HelpOverviewRow associates consumer copy with one real configurable action.
// Context and Action must exactly match the consumer's keymap catalog.
type HelpOverviewRow struct {
	Label, Context, Action, Summary string
}

// HelpOverviewView is one consumer view in the wide navigation rail. Exactly
// one view is Current; every other view references a configurable action.
type HelpOverviewView struct {
	Label, Context, Action string
	Current                bool
}

// HelpOverviewAside supplies optional wide-only workflow guidance.
type HelpOverviewAside struct {
	Title   string
	Steps   []HelpOverviewAsideLine
	Details []HelpOverviewAsideLine
}

type HelpOverviewAsideLine struct {
	Label, Text string
}

func (overview *HelpOverview) composed() bool {
	return overview != nil && (overview.Description != "" || overview.Footer != "" || len(overview.Views) != 0 || overview.Aside != nil)
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
	helpSelected                              int
	helpCommands                              map[string]HelpCommand
	helpOverviewActions                       []keymap.Action
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
	helpCommands, overview, overviewActions, err := validateHelpOptions(options)
	if err != nil {
		return nil, err
	}
	options.HelpOverview = overview
	s := &Surface{options: options, showingDebug: options.StartDebug, now: time.Now, helpCommands: helpCommands, helpOverviewActions: overviewActions}
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

func validateHelpOptions(options Options) (map[string]HelpCommand, *HelpOverview, []keymap.Action, error) {
	if !options.InteractiveHelp && (options.HelpOpenedAction != "" || len(options.HelpCommands) != 0 || options.HelpOverview != nil) {
		return nil, nil, nil, errors.New("Help extensions require interactive Help")
	}
	targets := make(map[string]bool)
	catalog := options.Main.KeymapCatalog()
	for _, action := range catalog {
		targets[action.ID] = true
	}
	if options.HelpOpenedAction != "" && !targets[options.HelpOpenedAction] {
		return nil, nil, nil, errors.New("Help-opened action must target a main action")
	}
	commands := make(map[string]HelpCommand, len(options.HelpCommands))
	for _, command := range options.HelpCommands {
		id := helpCommandID(command)
		if _, err := diagnostics.NewID(id); err != nil {
			return nil, nil, nil, fmt.Errorf("help command ID: %w", err)
		}
		if command.Label == "" || command.Action == "" || !targets[command.Action] {
			return nil, nil, nil, fmt.Errorf("help command %q must have a label and target a main action", command.ID)
		}
		if _, duplicate := commands[id]; duplicate {
			return nil, nil, nil, fmt.Errorf("duplicate help command %q", command.ID)
		}
		for _, sequence := range command.Defaults {
			if len(sequence) != 1 {
				return nil, nil, nil, fmt.Errorf("help command %q defaults must be single-stroke", command.ID)
			}
			if _, err := keymap.Normalize(sequence[0]); err != nil {
				return nil, nil, nil, fmt.Errorf("help command %q default: %w", command.ID, err)
			}
		}
		commands[id] = command
	}
	overview, actions, err := validateHelpOverview(options.HelpOverview, catalog)
	if err != nil {
		return nil, nil, nil, err
	}
	return commands, overview, actions, nil
}

func validateHelpOverview(overview *HelpOverview, catalog []keymap.Action) (*HelpOverview, []keymap.Action, error) {
	if overview == nil {
		return nil, nil, nil
	}
	if err := validateHelpOverviewText("title", overview.Title, 80, true); err != nil {
		return nil, nil, err
	}
	if err := validateHelpOverviewText("current", overview.Current, 64, false); err != nil {
		return nil, nil, err
	}
	if err := validateHelpOverviewText("view menu", overview.ViewMenu, 32, false); err != nil {
		return nil, nil, err
	}
	if len(overview.Rows) == 0 || len(overview.Rows) > 64 {
		return nil, nil, errors.New("Help overview must contain between 1 and 64 rows")
	}
	byKey := make(map[string]keymap.Action, len(catalog))
	for _, action := range catalog {
		byKey[action.Context+"\x00"+action.ID] = action
	}
	clone := *overview
	clone.Rows = append([]HelpOverviewRow(nil), overview.Rows...)
	clone.Views = append([]HelpOverviewView(nil), overview.Views...)
	if overview.Aside != nil {
		aside := *overview.Aside
		aside.Steps = append([]HelpOverviewAsideLine(nil), aside.Steps...)
		aside.Details = append([]HelpOverviewAsideLine(nil), aside.Details...)
		clone.Aside = &aside
	}
	for _, field := range []struct {
		name, value string
		limit       int
	}{
		{name: "description", value: clone.Description, limit: 240},
		{name: "footer", value: clone.Footer, limit: 240},
		{name: "compact footer", value: clone.CompactFooter, limit: 120},
	} {
		if err := validateHelpOverviewText(field.name, field.value, field.limit, false); err != nil {
			return nil, nil, err
		}
	}
	if clone.InitialRow < 0 || clone.InitialRow >= len(clone.Rows) {
		return nil, nil, errors.New("Help overview initial row is outside its rows")
	}
	actions := make([]keymap.Action, 0, len(clone.Rows))
	seen := make(map[string]bool, len(clone.Rows))
	for index, row := range clone.Rows {
		if err := validateHelpOverviewText(fmt.Sprintf("row %d label", index+1), row.Label, 64, true); err != nil {
			return nil, nil, err
		}
		if err := validateHelpOverviewText(fmt.Sprintf("row %d summary", index+1), row.Summary, 160, false); err != nil {
			return nil, nil, err
		}
		key := row.Context + "\x00" + row.Action
		action, ok := byKey[key]
		if !ok {
			return nil, nil, fmt.Errorf("Help overview row %d must reference a catalog action", index+1)
		}
		if seen[key] {
			return nil, nil, fmt.Errorf("duplicate Help overview action reference %q/%q", row.Context, row.Action)
		}
		seen[key] = true
		actions = append(actions, action)
	}
	if len(clone.Views) != 0 {
		current := 0
		for index, view := range clone.Views {
			if err := validateHelpOverviewText(fmt.Sprintf("view %d label", index+1), view.Label, 32, true); err != nil {
				return nil, nil, err
			}
			if view.Current {
				current++
				if view.Context != "" || view.Action != "" {
					return nil, nil, fmt.Errorf("Help overview current view %d must not reference an action", index+1)
				}
				continue
			}
			if _, ok := byKey[view.Context+"\x00"+view.Action]; !ok {
				return nil, nil, fmt.Errorf("Help overview view %d must reference a catalog action", index+1)
			}
		}
		if current != 1 {
			return nil, nil, errors.New("Help overview views must contain exactly one current view")
		}
	}
	if clone.Aside != nil {
		if err := validateHelpOverviewText("aside title", clone.Aside.Title, 64, true); err != nil {
			return nil, nil, err
		}
		if len(clone.Aside.Steps)+len(clone.Aside.Details) > 16 {
			return nil, nil, errors.New("Help overview aside exceeds 16 lines")
		}
		for _, section := range []struct {
			name  string
			lines []HelpOverviewAsideLine
		}{{name: "step", lines: clone.Aside.Steps}, {name: "detail", lines: clone.Aside.Details}} {
			for index, line := range section.lines {
				if err := validateHelpOverviewText(fmt.Sprintf("aside %s %d label", section.name, index+1), line.Label, 32, true); err != nil {
					return nil, nil, err
				}
				if err := validateHelpOverviewText(fmt.Sprintf("aside %s %d text", section.name, index+1), line.Text, 160, true); err != nil {
					return nil, nil, err
				}
			}
		}
	}
	return &clone, actions, nil
}

func validateHelpOverviewText(field, value string, limit int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("Help overview %s is required", field)
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("Help overview %s must be single-line", field)
	}
	if len([]rune(value)) > limit {
		return fmt.Errorf("Help overview %s exceeds %d characters", field, limit)
	}
	return nil
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
	mainCatalog := s.options.Main.KeymapCatalog()
	catalog := append([]keymap.Action(nil), mainCatalog...)
	if s.options.Debug != nil {
		catalog = append(catalog, s.options.Debug.KeymapCatalog()...)
	}
	for _, l := range []*list{s.menu, s.editor, s.entry} {
		catalog = append(catalog, l.picker.KeymapCatalog()...)
	}
	tabOwned := make(map[keymap.Context]bool)
	mainOwnsDebugKey := make(map[keymap.Context]bool)
	for _, action := range mainCatalog {
		context := keymap.Context{ID: action.Context, Editing: action.Editing}
		for _, sequence := range action.Defaults {
			if len(sequence) != 1 {
				continue
			}
			key, err := keymap.Normalize(sequence[0])
			if err == nil && key == "d" {
				mainOwnsDebugKey[context] = true
			}
		}
	}
	for _, child := range []MappedSurface{s.options.Main, s.options.Debug} {
		if child == nil {
			continue
		}
		for _, action := range child.KeymapCatalog() {
			context := keymap.Context{ID: action.Context, Editing: action.Editing}
			if s.childOwnsTab(context) {
				tabOwned[context] = true
			}
		}
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
	claimedHelpKeys := make(map[string]bool)
	for _, command := range s.options.HelpCommands {
		for _, sequence := range command.Defaults {
			key, _ := keymap.Normalize(sequence[0])
			claimedHelpKeys[key] = true
		}
	}
	for _, a := range []struct {
		id, label string
		keys      []string
	}{{"back", "Close help", []string{"?", "escape", "backspace", "enter"}}, {"quit", "Quit", []string{"q"}}, {"next", "Next shortcut", []string{"down", "j"}}, {"previous", "Previous shortcut", []string{"up", "k"}}, {"page-next", "Next page", []string{"page-down"}}, {"page-previous", "Previous page", []string{"page-up"}}, {"home", "First shortcut", []string{"home"}}, {"end", "Last shortcut", []string{"end"}}} {
		keys := a.keys
		if s.options.InteractiveHelp {
			filtered := keys[:0]
			for _, key := range keys {
				normalized, _ := keymap.Normalize(key)
				if (a.id == "back" && normalized == "enter") || claimedHelpKeys[normalized] {
					continue
				}
				filtered = append(filtered, key)
			}
			keys = filtered
		}
		add("keymap.help."+a.id, a.label, "keymap.help", false, false, keys...)
	}
	if s.options.InteractiveHelp {
		add("keymap.help.edit", "Edit selected shortcut", "keymap.help", false, false, "enter")
		for _, command := range s.options.HelpCommands {
			action := keymap.Action{ID: helpCommandID(command), Label: command.Label, Context: "keymap.help"}
			action.Defaults = append([]keymap.Sequence(nil), command.Defaults...)
			catalog = append(catalog, action)
		}
	}
	// Tab traverses the existing controls and the appended Actions control unless
	// an explicit child context owns Tab for its internal form navigation.
	// Either behavior is an ordinary remappable action, never a hidden fallback.
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
				if key == "tab" && !tabOwned[keymap.Context{ID: a.Context, Editing: a.Editing}] {
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
		var focusKeys []string
		if !tabOwned[keymap.Context{ID: context, Editing: editing}] {
			focusKeys = []string{"tab"}
		}
		add("keymap.focus-next", "Next control", context, editing, false, focusKeys...)
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
			if !editing && !mainOwnsDebugKey[keymap.Context{ID: context, Editing: editing}] {
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

func helpCommandID(command HelpCommand) string { return "keymap.help.command." + command.ID }

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
	if _, ok := s.helpCommands[id]; ok {
		return keymap.State{Enabled: true}
	}
	switch id {
	case "keymap.actions", "keymap.focus-next", "keymap.editor", "keymap.checker", "keymap.help", "keymap.debug", "keymap.save", "keymap.discard", "keymap.reload", "keymap.reset-all",
		"keymap.text.finish", "keymap.text.cancel", "keymap.text.backspace", "keymap.text.delete", "keymap.text.left", "keymap.text.right", "keymap.text.home", "keymap.text.end",
		"keymap.help.back", "keymap.help.quit", "keymap.help.next", "keymap.help.previous", "keymap.help.page-next", "keymap.help.page-previous", "keymap.help.home", "keymap.help.end", "keymap.help.edit":
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
		case "keymap.help.edit":
			if len(s.helpActions()) == 0 {
				return keymap.State{Reason: "No configurable shortcut"}
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
		var effects []shell.Effect
		if s.options.HelpOpenedAction != "" {
			effects = s.options.Main.Update(events, shell.ActionEvent{ID: s.options.HelpOpenedAction})
		}
		s.helpOffset = 0
		s.helpSelected = 0
		if s.options.HelpOverview != nil {
			s.helpSelected = s.options.HelpOverview.InitialRow
		}
		s.view = helpScreen
		s.actionsFocused = false
		return effects
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
	case "keymap.help.edit":
		return s.openHelpSelection(events)
	}
	if command, ok := s.helpCommands[id]; ok {
		s.view = baseScreen
		s.showingDebug = false
		s.actionsFocused = false
		s.status = ""
		effects := s.options.Main.Update(events, shell.ActionEvent{ID: command.Action})
		return append(effects, s.resizeActive(events)...)
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
	if s.options.InteractiveHelp {
		actions := s.helpActions()
		last := max(0, len(actions)-1)
		page := max(1, s.lastResize.Layout.Render.Rows-4)
		switch id {
		case "keymap.help.quit":
			return []shell.Effect{shell.Quit()}
		case "keymap.help.back":
			s.view = s.helpOrigin
			return s.resizeActive(events)
		case "keymap.help.next":
			s.helpSelected = min(last, s.helpSelected+1)
		case "keymap.help.previous":
			s.helpSelected = max(0, s.helpSelected-1)
		case "keymap.help.page-next":
			s.helpSelected = min(last, s.helpSelected+page)
		case "keymap.help.page-previous":
			s.helpSelected = max(0, s.helpSelected-page)
		case "keymap.help.home":
			s.helpSelected = 0
		case "keymap.help.end":
			s.helpSelected = last
		}
		return nil
	}
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
