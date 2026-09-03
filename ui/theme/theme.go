// Package theme provides Herdr-compatible semantic colour themes.
package theme

import (
	"fmt"
	"image/color"
	"sort"
	"strconv"
	"strings"
)

// Color is a canonical #rrggbb colour or Reset.
type Color string

const Reset Color = "reset"

var namedColors = map[string]string{
	"black": "#000000", "red": "#800000", "green": "#008000", "yellow": "#808000",
	"blue": "#000080", "magenta": "#800080", "cyan": "#008080", "white": "#c0c0c0",
	"bright-black": "#808080", "bright-red": "#ff0000", "bright-green": "#00ff00", "bright-yellow": "#ffff00",
	"bright-blue": "#0000ff", "bright-magenta": "#ff00ff", "bright-cyan": "#00ffff", "bright-white": "#ffffff",
}

// ParseColor accepts reset, named ANSI colours, #rgb, #rrggbb, and rgb(r,g,b).
func ParseColor(value string) (Color, error) {
	s := strings.ToLower(strings.TrimSpace(value))
	if s == "reset" {
		return Reset, nil
	}
	if hex, ok := namedColors[s]; ok {
		s = hex
	}
	if strings.HasPrefix(s, "rgb(") && strings.HasSuffix(s, ")") {
		parts := strings.Split(s[4:len(s)-1], ",")
		if len(parts) != 3 {
			return "", fmt.Errorf("invalid colour %q", value)
		}
		var rgb [3]int
		for i, part := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || n < 0 || n > 255 {
				return "", fmt.Errorf("invalid colour %q", value)
			}
			rgb[i] = n
		}
		return Color(fmt.Sprintf("#%02x%02x%02x", rgb[0], rgb[1], rgb[2])), nil
	}
	if len(s) == 4 && s[0] == '#' {
		s = fmt.Sprintf("#%c%c%c%c%c%c", s[1], s[1], s[2], s[2], s[3], s[3])
	}
	if len(s) != 7 || s[0] != '#' {
		return "", fmt.Errorf("invalid colour %q", value)
	}
	if _, err := strconv.ParseUint(s[1:], 16, 24); err != nil {
		return "", fmt.Errorf("invalid colour %q", value)
	}
	return Color(s), nil
}

// RGBA resolves a non-reset colour for image rendering.
func (c Color) RGBA() (color.RGBA, bool) {
	if c == Reset || len(c) != 7 || c[0] != '#' {
		return color.RGBA{}, false
	}
	n, err := strconv.ParseUint(string(c[1:]), 16, 24)
	if err != nil {
		return color.RGBA{}, false
	}
	return color.RGBA{R: uint8(n >> 16), G: uint8(n >> 8), B: uint8(n), A: 255}, true
}

// Palette contains semantic tokens shared by plugin views.
type Palette struct {
	ID                  string
	Light               bool
	Background          Color
	PanelBackground     Color
	SidebarBackground   Color
	ActiveRowBackground Color
	SelectionBackground Color
	Surface             Color
	Overlay             Color
	Border              Color
	Text                Color
	Muted               Color
	Accent              Color
	Red                 Color
	Green               Color
	Yellow              Color
	Blue                Color
	Magenta             Color
	Cyan                Color
}

func palette(id string, light bool, values ...string) Palette {
	colors := make([]Color, len(values))
	for i, value := range values {
		colors[i], _ = ParseColor(value)
	}
	return Palette{
		ID: id, Light: light, Background: colors[0], PanelBackground: colors[1],
		SidebarBackground: colors[1], ActiveRowBackground: colors[2], SelectionBackground: colors[3],
		Surface: colors[2], Overlay: colors[3], Border: colors[4], Text: colors[5], Muted: colors[6],
		Accent: colors[7], Red: colors[8], Green: colors[9], Yellow: colors[10], Blue: colors[11],
		Magenta: colors[12], Cyan: colors[13],
	}
}

var catalogue = map[string]Palette{
	"catppuccin":       palette("catppuccin", false, "#1e1e2e", "#181825", "#313244", "#45475a", "#585b70", "#cdd6f4", "#a6adc8", "#f5c2e7", "#f38ba8", "#a6e3a1", "#f9e2af", "#89b4fa", "#cba6f7", "#94e2d5"),
	"catppuccin-latte": palette("catppuccin-latte", true, "#eff1f5", "#e6e9ef", "#dce0e8", "#ccd0da", "#9ca0b0", "#4c4f69", "#6c6f85", "#8839ef", "#d20f39", "#40a02b", "#df8e1d", "#1e66f5", "#ea76cb", "#179299"),
	"terminal":         palette("terminal", false, "reset", "reset", "#202020", "#404040", "#808080", "#ffffff", "#c0c0c0", "#00ffff", "#ff0000", "#00ff00", "#ffff00", "#0000ff", "#ff00ff", "#00ffff"),
	"tokyo-night":      palette("tokyo-night", false, "#1a1b26", "#16161e", "#24283b", "#414868", "#565f89", "#c0caf5", "#9aa5ce", "#7aa2f7", "#f7768e", "#9ece6a", "#e0af68", "#7aa2f7", "#bb9af7", "#7dcfff"),
	"dracula":          palette("dracula", false, "#282a36", "#21222c", "#44475a", "#6272a4", "#6272a4", "#f8f8f2", "#bfbfbf", "#bd93f9", "#ff5555", "#50fa7b", "#f1fa8c", "#8be9fd", "#ff79c6", "#8be9fd"),
	"nord":             palette("nord", false, "#2e3440", "#272c36", "#3b4252", "#434c5e", "#4c566a", "#eceff4", "#d8dee9", "#88c0d0", "#bf616a", "#a3be8c", "#ebcb8b", "#81a1c1", "#b48ead", "#8fbcbb"),
	"gruvbox":          palette("gruvbox", false, "#282828", "#1d2021", "#3c3836", "#504945", "#665c54", "#ebdbb2", "#a89984", "#d79921", "#fb4934", "#b8bb26", "#fabd2f", "#83a598", "#d3869b", "#8ec07c"),
	"one-dark":         palette("one-dark", false, "#282c34", "#21252b", "#2c313c", "#3e4451", "#5c6370", "#abb2bf", "#828997", "#61afef", "#e06c75", "#98c379", "#e5c07b", "#61afef", "#c678dd", "#56b6c2"),
	"solarized":        palette("solarized", false, "#002b36", "#00212b", "#073642", "#0b4654", "#586e75", "#eee8d5", "#93a1a1", "#268bd2", "#dc322f", "#859900", "#b58900", "#268bd2", "#d33682", "#2aa198"),
	"kanagawa":         palette("kanagawa", false, "#1f1f28", "#16161d", "#2a2a37", "#363646", "#54546d", "#dcd7ba", "#a6a69c", "#7e9cd8", "#e82424", "#98bb6c", "#e6c384", "#7e9cd8", "#957fb8", "#7fb4ca"),
	"rose-pine":        palette("rose-pine", false, "#191724", "#1f1d2e", "#26233a", "#403d52", "#6e6a86", "#e0def4", "#908caa", "#c4a7e7", "#eb6f92", "#9ccfd8", "#f6c177", "#31748f", "#c4a7e7", "#9ccfd8"),
	"vesper":           palette("vesper", false, "#101010", "#0a0a0a", "#1c1c1c", "#282828", "#505050", "#ffffff", "#a0a0a0", "#ffc799", "#ff8080", "#99ffe4", "#ffc799", "#a0a0ff", "#d8b4fe", "#99ffe4"),
}

// IDs returns the stable built-in IDs in lexical order.
func IDs() []string {
	ids := make([]string, 0, len(catalogue))
	for id := range catalogue {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Builtin returns an independent copy of a built-in palette.
func Builtin(id string) (Palette, error) {
	p, ok := catalogue[id]
	if !ok {
		return Palette{}, fmt.Errorf("unknown theme %q", id)
	}
	return p, nil
}

// Settings selects a base palette and optional semantic-token overrides.
type Settings struct {
	Name       string            `toml:"name"`
	AutoSwitch bool              `toml:"auto_switch"`
	DarkName   string            `toml:"dark_name"`
	LightName  string            `toml:"light_name"`
	Custom     map[string]string `toml:"custom"`
}

func DefaultSettings() Settings {
	return Settings{Name: "catppuccin", DarkName: "catppuccin", LightName: "catppuccin-latte"}
}

func (s Settings) Validate() error {
	for _, entry := range []struct{ field, id string }{{"name", s.Name}, {"dark_name", s.DarkName}, {"light_name", s.LightName}} {
		if _, ok := catalogue[entry.id]; !ok {
			return fmt.Errorf("theme.%s: unknown theme %q", entry.field, entry.id)
		}
	}
	_, err := applyCustom(Palette{}, s.Custom)
	return err
}

// Resolve returns the configured palette. light is used only with AutoSwitch.
func (s Settings) Resolve(light bool) (Palette, error) {
	if err := s.Validate(); err != nil {
		return Palette{}, err
	}
	id := s.Name
	if s.AutoSwitch {
		if light {
			id = s.LightName
		} else {
			id = s.DarkName
		}
	}
	p := catalogue[id]
	return applyCustom(p, s.Custom)
}

func applyCustom(p Palette, custom map[string]string) (Palette, error) {
	setters := map[string]*Color{
		"background": &p.Background, "panel_bg": &p.PanelBackground, "surface": &p.Surface,
		"sidebar_bg": &p.SidebarBackground, "active_row_bg": &p.ActiveRowBackground, "selection_bg": &p.SelectionBackground,
		"overlay": &p.Overlay, "border": &p.Border, "text": &p.Text, "muted": &p.Muted,
		"accent": &p.Accent, "red": &p.Red, "green": &p.Green, "yellow": &p.Yellow,
		"blue": &p.Blue, "magenta": &p.Magenta, "cyan": &p.Cyan,
	}
	keys := make([]string, 0, len(custom))
	for key := range custom {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		raw := custom[key]
		target, ok := setters[key]
		if !ok {
			return Palette{}, fmt.Errorf("theme.custom: unknown token %q", key)
		}
		value, err := ParseColor(raw)
		if err != nil {
			return Palette{}, fmt.Errorf("theme.custom.%s: %w", key, err)
		}
		*target = value
	}
	return p, nil
}
