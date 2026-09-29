package shell

import (
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
)

type observerSurface struct {
	*mappedSurface
	generation *uint64
	captured   uint64
	delivered  uint64
}

func TestInputActivityEarlyExitAndSuffix(t *testing.T) {
	for _, value := range []string{"qzz", "q\x00z", "\x00/abc", "/abc"} {
		t.Run(value, func(t *testing.T) {
			m, s, _ := mappedModel(t)
			var generation uint64
			m.options.ObserveInputActivity = func() { generation++ }
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)})
			if generation != uint64(len([]rune(value))) {
				t.Fatalf("lost discarded units: %d", generation)
			}
			if strings.Contains(value, "/") {
				// Preserve the existing normalized-index suffix, including the
				// existing invalid-leading-rune behavior; this is not a repair.
				want := "abc"
				if value[0] == 0 {
					want = "/abc"
				}
				if len(s.texts) != 1 || s.texts[0].Text != want {
					t.Fatalf("changed legacy suffix: %#v", s.texts)
				}
			}
		})
	}
}

type inputActivityCaptureSurface struct {
	*mappedSurface
	endAfter int
}

func (s *inputActivityCaptureSurface) CaptureKey(c EventContext, k CapturedKey) []Effect {
	result := s.mappedSurface.CaptureKey(c, k)
	if len(s.captured) >= s.endAfter {
		s.capture = false
	}
	return result
}
func (s *inputActivityCaptureSurface) Update(c EventContext, e Event) []Effect {
	if a, ok := e.(ActionEvent); ok && a.ID == "edit" {
		s.capture = true
		return nil
	}
	return s.mappedSurface.Update(c, e)
}
func TestInputActivityCaptureEndsMidBatch(t *testing.T) {
	for _, initial := range []bool{true, false} {
		m, base, _ := mappedModel(t)
		s := &inputActivityCaptureSurface{mappedSurface: base, endAfter: 1}
		s.capture = initial
		m.surface = s
		var generation uint64
		m.options.ObserveInputActivity = func() { generation++ }
		text := "zzz"
		if !initial {
			text = "/zzz"
		}
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
		if generation != uint64(len(text)) || len(s.captured) != 1 {
			t.Fatalf("capture remainder lost: %d %#v", generation, s.captured)
		}
	}
}

type inputActivityFilteredSurface struct{ *mappedSurface }

func (*inputActivityFilteredSurface) SuppressEventDiagnostics(Event) bool   { return true }
func (*inputActivityFilteredSurface) SuppressEffectDiagnostics(Effect) bool { return true }
func TestInputActivityIndependentOfPrivacy(t *testing.T) {
	m, s, sink := mappedModel(t)
	m.surface = &inputActivityFilteredSurface{mappedSurface: s}
	initial := len(sink.snapshot())
	var generation uint64
	m.options.ObserveInputActivity = func() { generation++ }
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("secret")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("private paste"), Paste: true})
	if generation != 7 || len(sink.snapshot()) != initial {
		t.Fatalf("observer depends on diagnostic recording: %d/%d", generation, len(sink.snapshot())-initial)
	}
}

// Same names from a different package must not be classified as terminal input.
type unknownInputByteMsg byte
type unknownCSISequenceMsg []byte

func TestInputActivityUnknownMessageIdentity(t *testing.T) {
	m, _, _ := mappedModel(t)
	var generation uint64
	m.options.ObserveInputActivity = func() { generation++ }
	for _, msg := range []tea.Msg{nil, unknownInputByteMsg(0x80), unknownCSISequenceMsg("private"), struct{}{}} {
		m.Update(msg)
	}
	if generation != 0 {
		t.Fatal("background or foreign message counted as input")
	}
}

func TestInputActivityActualUnknownDecoder(t *testing.T) {
	for _, test := range []struct {
		name, input string
		later       uint64
	}{
		{"CSI", "m\x1b[----Xq", 2},
		{"invalid UTF8", "m\x80q", 2},
		{"both", "m\x1b[----X\x80q", 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, base, sink := mappedModel(t)
			var generation uint64
			s := &observerSurface{mappedSurface: base, generation: &generation}
			runtime, e := keymap.New(s.KeymapCatalog())
			if e != nil {
				t.Fatal(e)
			}
			palette, _ := theme.Builtin("terminal")
			program, e := NewProgram(ProgramOptions{PluginID: testID(t, "decoder-observer"), Theme: palette, Events: sink, Keymap: runtime, Input: strings.NewReader(test.input), Output: io.Discard, ObserveInputActivity: func() { generation++ }}, s)
			if e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() { _, e := program.Run(); done <- e }()
			select {
			case e := <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(2 * time.Second):
				program.Kill()
				<-done
				t.Fatal("decoder proof did not exit")
			}
			if s.captured == 0 || generation != s.captured+test.later {
				t.Fatalf("native rejected input invisible: captured=%d final=%d", s.captured, generation)
			}
		})
	}
}
func (s *observerSurface) KeymapCatalog() []keymap.Action {
	return append(s.mappedSurface.KeymapCatalog(), keymap.Action{ID: "submit", Label: "Submit", Context: "results", Defaults: []keymap.Sequence{{"m"}, {"enter"}}})
}
func (s *observerSurface) Update(c EventContext, e Event) []Effect {
	if a, ok := e.(ActionEvent); ok && a.ID == "submit" {
		s.captured = *s.generation
	}
	if _, ok := e.(TextEvent); ok {
		s.delivered = *s.generation
	}
	return s.mappedSurface.Update(c, e)
}

func TestInputActivityPrecedesEditingSuffixDelivery(t *testing.T) {
	for _, text := range []string{"/abc", "/日本語", "\x00/abc"} {
		t.Run(text, func(t *testing.T) {
			m, base, _ := mappedModel(t)
			var generation uint64
			s := &observerSurface{mappedSurface: base, generation: &generation}
			m.surface = s
			m.options.ObserveInputActivity = func() { generation++ }
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
			if s.delivered != uint64(len([]rune(text))) {
				t.Fatalf("input observed after editor delivery: delivered=%d final=%d", s.delivered, generation)
			}
		})
	}
}

func TestInputActivityCoalesced(t *testing.T) {
	for _, msg := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("mz")}, {Type: tea.KeyRunes, Runes: []rune{'m', '\x00'}}} {
		var generation uint64
		_, s, sink := mappedModel(t)
		wrapped := &observerSurface{mappedSurface: s, generation: &generation}
		runtime, err := keymap.New(wrapped.KeymapCatalog())
		if err != nil {
			t.Fatal(err)
		}
		palette, _ := theme.Builtin("terminal")
		m := newModel(ProgramOptions{PluginID: testID(t, "input-probe"), Theme: palette, Events: sink, Keymap: runtime, ObserveInputActivity: func() { generation++ }}, wrapped)
		m.Update(msg)
		if wrapped.captured == 0 || generation <= wrapped.captured {
			t.Fatalf("later dropped unit invisible: captured=%d generation=%d", wrapped.captured, generation)
		}
	}
}

func TestInputActivityDroppedAfterEnter(t *testing.T) {
	var generation uint64
	_, s, sink := mappedModel(t)
	wrapped := &observerSurface{mappedSurface: s, generation: &generation}
	runtime, _ := keymap.New(wrapped.KeymapCatalog())
	palette, _ := theme.Builtin("terminal")
	m := newModel(ProgramOptions{PluginID: testID(t, "input-probe"), Theme: palette, Events: sink, Keymap: runtime, ObserveInputActivity: func() { generation++ }}, wrapped)
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	captured := wrapped.captured
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	m.Update(tea.KeyMsg{Type: tea.KeyF20})
	if captured == 0 || generation != captured+2 {
		t.Fatalf("dropped inputs invisible: captured=%d generation=%d", captured, generation)
	}
}

func TestInputActivityCapturePasteAndActivity(t *testing.T) {
	for _, test := range []struct {
		name             string
		msg              tea.Msg
		capture, editing bool
		want             uint64
	}{
		{"unmapped rune", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")}, false, false, 1},
		{"unsupported key", tea.KeyMsg{Type: tea.KeyF20}, false, false, 1},
		{"capture repeated runes", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("zzz")}, true, false, 3},
		{"paste capture", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("private-paste"), Paste: true}, true, false, 1},
		{"paste editing", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("private-paste"), Paste: true}, false, true, 1},
		{"paste discarded", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("private-paste"), Paste: true}, false, false, 1},
		{"modified key", tea.KeyMsg{Type: tea.KeyLeft, Alt: true}, false, false, 1},
		{"quit", tea.KeyMsg{Type: tea.KeyCtrlC}, false, false, 1},
		{"resize", tea.WindowSizeMsg{Width: 80, Height: 18}, false, false, 1},
		{"blur", tea.BlurMsg{}, false, false, 1},
		{"focus", tea.FocusMsg{}, false, false, 1},
		{"mouse", tea.MouseMsg{}, false, false, 1},
		{"background message", struct{}{}, false, false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, s, _ := mappedModel(t)
			s.capture = test.capture
			s.editing = test.editing
			var generation uint64
			m.options.ObserveInputActivity = func() { generation++ }
			m.Update(test.msg)
			if generation != test.want {
				t.Fatalf("observed=%d want=%d", generation, test.want)
			}
		})
	}
}
