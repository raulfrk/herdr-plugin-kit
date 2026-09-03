package catalogue

import (
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/ui/responsive"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
)

func TestLiveCatalogueKeepsSurfacesStatesAndFinalPresentation(t *testing.T) {
	if len(PluginSurfaces()) != 6 {
		t.Fatalf("surfaces = %d", len(PluginSurfaces()))
	}
	for _, surface := range PluginSurfaces() {
		if len(surface.States) != 6 {
			t.Fatalf("%s states = %d", surface.ID, len(surface.States))
		}
	}
	live := NewLiveSurface()
	live.Update(shell.EventContext{}, shell.TextEvent{Text: "p"})
	live.Update(shell.EventContext{}, shell.TextEvent{Text: "s"})
	live.Update(shell.EventContext{}, shell.TextEvent{Text: "t"})
	if got := live.DiagnosticState().Selection.String(); got != theme.IDs()[1] {
		t.Fatalf("theme = %s", got)
	}
	context := shell.RenderContext{Layout: responsive.Resolve(responsive.Size{Columns: 48, Rows: 18}), ResizeGeneration: 7}
	frame, err := live.Render(context)
	if err != nil {
		t.Fatal(err)
	}
	plain := view.ANSI(frame)
	if frame.Width() != 48 || frame.Height() != 18 || !strings.Contains(plain, "Bento Command") || !strings.Contains(plain, "▌") {
		t.Fatalf("final compact command frame missing required cues")
	}
}

func TestLiveCatalogueEditingHUDAndHelp(t *testing.T) {
	live := NewLiveSurface()
	live.Update(shell.EventContext{}, shell.TextEvent{Text: "/"})
	live.Update(shell.EventContext{}, shell.TextEvent{Text: "東京e\u0301🧭", Paste: true})
	live.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyBackspace})
	if live.query != "東京e\u0301" {
		t.Fatal(live.query)
	}
	live.Update(shell.EventContext{}, shell.KeyEvent{Code: shell.KeyEnter})
	live.Update(shell.EventContext{}, shell.TextEvent{Text: "h"})
	frame, err := live.Render(shell.RenderContext{Layout: responsive.Resolve(responsive.Size{Columns: 110, Rows: 24}), ResizeGeneration: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.ANSI(frame), "HUD 110x24") {
		t.Fatal("HUD missing")
	}
	live.editing = false
	live.Update(shell.EventContext{}, shell.TextEvent{Text: "?"})
	frame, err = live.Render(shell.RenderContext{Layout: responsive.Resolve(responsive.Size{Columns: 40, Rows: 10})})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.ANSI(frame), "CATALOGUE HELP") {
		t.Fatal("help missing")
	}
}
