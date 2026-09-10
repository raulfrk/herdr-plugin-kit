package shell

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
)

type mappedSurface struct {
	*testSurface
	editing, capture bool
	invoked          []string
	captured         []CapturedKey
}

func (s *mappedSurface) KeymapCatalog() []keymap.Action {
	return []keymap.Action{
		{ID: "refresh", Label: "Refresh", Context: "results", Defaults: []keymap.Sequence{{"g", "r"}, {"f5"}, {"alt+d"}, {"alt+left"}}},
		{ID: "edit", Label: "Edit", Context: "results", Defaults: []keymap.Sequence{{"/"}}},
		{ID: "quit", Label: "Quit", Context: "results", Defaults: []keymap.Sequence{{"q"}}},
		{ID: "finish", Label: "Finish", Context: "editing", Editing: true, Defaults: []keymap.Sequence{{"enter"}}},
		{ID: "editing-command", Label: "Editing command", Context: "editing", Editing: true, Defaults: []keymap.Sequence{{"ctrl+k", "r"}}},
	}
}
func (s *mappedSurface) KeymapContext() keymap.Context {
	if s.editing {
		return keymap.Context{ID: "editing", Editing: true}
	}
	return keymap.Context{ID: "results"}
}
func (s *mappedSurface) ActionState(string) keymap.State { return keymap.State{Enabled: true} }
func (s *mappedSurface) Update(c EventContext, e Event) []Effect {
	if a, ok := e.(ActionEvent); ok {
		s.invoked = append(s.invoked, a.ID)
		if a.ID == "edit" {
			s.editing = true
		}
		if a.ID == "finish" {
			s.editing = false
		}
		if a.ID == "quit" {
			return []Effect{Quit()}
		}
		return nil
	}
	return s.testSurface.Update(c, e)
}

func TestEditingSequencesKeepControlNamesOutOfText(t *testing.T) {
	m, s, _ := mappedModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if strings.Join(s.invoked, ",") != "edit,editing-command" || len(s.texts) != 0 {
		t.Fatal("editing sequence was inserted as text", s.invoked, s.texts)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlF})
	if len(s.texts) != 1 || s.texts[0].Text != " " {
		t.Fatal("mismatching input leaked a key name or lost a space", s.texts)
	}
}

func TestMappedModifiersQuitAndPrefixTimeout(t *testing.T) {
	m, s, _ := mappedModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d"), Alt: true})
	m.Update(tea.KeyMsg{Type: tea.KeyLeft, Alt: true})
	if strings.Join(s.invoked, ",") != "refresh,refresh" {
		t.Fatal("modified keys did not retain Alt", s.invoked)
	}
	now := time.Unix(100, 0)
	effects := m.dispatchKeymap(m.eventContext(), TextEvent{Text: "g"}, now)
	if len(effects) != 1 || effects[0].EventCode() != prefixTimer {
		t.Fatal("prefix did not request its expiry timer", effects)
	}
	m.dispatchKeymap(m.eventContext(), TimerEvent{Code: prefixTimer}, now.Add(keymap.Timeout))
	if len(m.options.Keymap.Pending()) != 0 || len(s.invoked) != 2 {
		t.Fatal("expiry invoked or retained an incomplete command")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("qgr")})
	if strings.Join(s.invoked, ",") != "refresh,refresh,quit" {
		t.Fatal("batch continued after quit", s.invoked)
	}
}
func (s *mappedSurface) CapturingKeys() bool { return s.capture }
func (s *mappedSurface) CaptureKey(_ EventContext, key CapturedKey) []Effect {
	s.captured = append(s.captured, key)
	return nil
}
func mappedModel(t *testing.T) (*model, *mappedSurface, *memorySink) {
	t.Helper()
	s := &mappedSurface{testSurface: newTestSurface(t)}
	r, err := keymap.New(s.KeymapCatalog())
	if err != nil {
		t.Fatal(err)
	}
	palette, _ := theme.Builtin("terminal")
	sink := &memorySink{}
	m := newModel(ProgramOptions{PluginID: testID(t, "mapped"), Theme: palette, Events: sink, Keymap: r}, s)
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	return m, s, sink
}
func TestKeymapBatchesTypingPasteAndLegacy(t *testing.T) {
	m, s, _ := mappedModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("grgr")})
	m.Update(tea.KeyMsg{Type: tea.KeyF5})
	if strings.Join(s.invoked, ",") != "refresh,refresh,refresh" {
		t.Fatal(s.invoked)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/日本語")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e\u0301")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("gr"), Paste: true, Alt: true})
	if len(s.texts) != 3 || s.texts[0].Text != "日本語" || s.texts[1].Text != "e\u0301" || !s.texts[2].Paste || s.texts[2].Alt {
		t.Fatal(s.texts)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("gr"), Paste: true})
	if len(s.invoked) != 5 {
		t.Fatal("paste invoked command", s.invoked)
	}
	legacy, plain, _ := testModel(t)
	legacy.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("gr")})
	if len(plain.texts) != 1 || plain.texts[0].Text != "gr" {
		t.Fatal("legacy changed")
	}
}
func TestKeyCaptureBypassesActionsAndSuppressesPaste(t *testing.T) {
	m, s, sink := mappedModel(t)
	s.capture = true
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("gr")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("private-paste"), Paste: true})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	if len(s.invoked) != 0 || len(s.texts) != 0 || len(s.captured) != 4 || !s.captured[2].Paste || s.captured[2].Stroke != "" || s.captured[3].Stroke != "ctrl+k" {
		t.Fatal(s.captured, s.invoked)
	}
	for _, event := range sink.snapshot() {
		if strings.HasPrefix(event.Code.String(), "input.") && (!event.Action.IsZero() || event.Bytes != 0) {
			t.Fatal("capture input leaked", event)
		}
	}
	_, command := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if command == nil {
		t.Fatal("fixed quit missing")
	}
	if len(s.captured) != 4 {
		t.Fatal("quit captured")
	}
}
