package theme_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
)

func TestCatalogueHasDocumentedStableIDs(t *testing.T) {
	want := []string{"catppuccin", "catppuccin-latte", "dracula", "gruvbox", "kanagawa", "nord", "one-dark", "rose-pine", "solarized", "terminal", "tokyo-night", "vesper"}
	if got := theme.IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs = %v, want %v", got, want)
	}
	for _, id := range want {
		palette, err := theme.Builtin(id)
		if err != nil {
			t.Fatal(err)
		}
		if palette.ID != id {
			t.Fatalf("Builtin(%q).ID = %q", id, palette.ID)
		}
		for name, value := range map[string]theme.Color{
			"background": palette.Background, "panel": palette.PanelBackground, "sidebar": palette.SidebarBackground,
			"active_row": palette.ActiveRowBackground, "selection": palette.SelectionBackground, "surface": palette.Surface,
			"overlay": palette.Overlay, "border": palette.Border, "text": palette.Text, "muted": palette.Muted,
			"accent": palette.Accent, "red": palette.Red, "green": palette.Green, "yellow": palette.Yellow,
			"blue": palette.Blue, "magenta": palette.Magenta, "cyan": palette.Cyan,
		} {
			if value == theme.Reset {
				continue
			}
			if _, ok := value.RGBA(); !ok {
				t.Errorf("%s %s token %s is not concrete", id, name, value)
			}
		}
	}
}

func TestSettingsResolveAndValidate(t *testing.T) {
	settings := theme.DefaultSettings()
	settings.AutoSwitch = true
	settings.Custom = map[string]string{"accent": "rgb(1, 2, 3)", "panel_bg": "reset", "sidebar_bg": "#010101", "active_row_bg": "#020202", "selection_bg": "#030303"}
	palette, err := settings.Resolve(true)
	if err != nil {
		t.Fatal(err)
	}
	if palette.ID != "catppuccin-latte" || palette.Accent != "#010203" || palette.PanelBackground != theme.Reset || palette.SidebarBackground != "#010101" || palette.ActiveRowBackground != "#020202" || palette.SelectionBackground != "#030303" {
		t.Fatalf("resolved palette = %+v", palette)
	}

	for name, test := range map[string]struct {
		mutate   func(*theme.Settings)
		fragment string
	}{
		"unknown theme": {func(s *theme.Settings) { s.Name = "missing" }, "theme.name"},
		"unknown token": {func(s *theme.Settings) { s.Custom = map[string]string{"future": "#fff"} }, "future"},
		"invalid color": {func(s *theme.Settings) { s.Custom = map[string]string{"accent": "#xyz"} }, "theme.custom.accent"},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := theme.DefaultSettings()
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil || !strings.Contains(err.Error(), test.fragment) {
				t.Fatalf("Validate error = %v", err)
			}
		})
	}
}

func TestEveryCustomTokenMapsToItsSemanticField(t *testing.T) {
	getters := map[string]func(theme.Palette) theme.Color{
		"background":    func(p theme.Palette) theme.Color { return p.Background },
		"panel_bg":      func(p theme.Palette) theme.Color { return p.PanelBackground },
		"sidebar_bg":    func(p theme.Palette) theme.Color { return p.SidebarBackground },
		"active_row_bg": func(p theme.Palette) theme.Color { return p.ActiveRowBackground },
		"selection_bg":  func(p theme.Palette) theme.Color { return p.SelectionBackground },
		"surface":       func(p theme.Palette) theme.Color { return p.Surface },
		"overlay":       func(p theme.Palette) theme.Color { return p.Overlay },
		"border":        func(p theme.Palette) theme.Color { return p.Border },
		"text":          func(p theme.Palette) theme.Color { return p.Text },
		"muted":         func(p theme.Palette) theme.Color { return p.Muted },
		"accent":        func(p theme.Palette) theme.Color { return p.Accent },
		"red":           func(p theme.Palette) theme.Color { return p.Red },
		"green":         func(p theme.Palette) theme.Color { return p.Green },
		"yellow":        func(p theme.Palette) theme.Color { return p.Yellow },
		"blue":          func(p theme.Palette) theme.Color { return p.Blue },
		"magenta":       func(p theme.Palette) theme.Color { return p.Magenta },
		"cyan":          func(p theme.Palette) theme.Color { return p.Cyan },
	}
	for token, get := range getters {
		settings := theme.DefaultSettings()
		settings.Custom = map[string]string{token: "#010203"}
		palette, err := settings.Resolve(false)
		if err != nil {
			t.Fatal(err)
		}
		if get(palette) != "#010203" {
			t.Errorf("custom token %q mapped to %q", token, get(palette))
		}
	}
}
