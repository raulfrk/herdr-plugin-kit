package view

import (
	"fmt"
	"strings"

	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
)

// ANSI returns deterministic text containing only SGR style sequences and
// newlines. It performs no I/O and resets SGR state before every newline/end.
func ANSI(frame *Frame) string {
	if frame.width == 0 || frame.height == 0 {
		return ""
	}
	var output strings.Builder
	for y := 0; y < frame.height; y++ {
		active := Style{}
		styled := false
		for x := 0; x < frame.width; x++ {
			cell := frame.cells[frame.index(x, y)]
			if cell.Continuation {
				continue
			}
			if cell.Style != active {
				if styled {
					output.WriteString("\x1b[0m")
				}
				sequence := ansiStyle(cell.Style)
				if sequence != "" {
					output.WriteString(sequence)
					styled = true
				} else {
					styled = false
				}
				active = cell.Style
			}
			if cell.Text == "" {
				output.WriteByte(' ')
			} else {
				output.WriteString(cell.Text)
			}
		}
		if styled {
			output.WriteString("\x1b[0m")
		}
		if y+1 < frame.height {
			output.WriteByte('\n')
		}
	}
	if !strings.HasSuffix(output.String(), "\x1b[0m") {
		output.WriteString("\x1b[0m")
	}
	return output.String()
}

func ansiStyle(style Style) string {
	codes := []string{"0"}
	if style.Bold {
		codes = append(codes, "1")
	}
	if style.Dim {
		codes = append(codes, "2")
	}
	if style.Underline {
		codes = append(codes, "4")
	}
	if rgb, ok := style.Foreground.RGBA(); ok {
		codes = append(codes, fmt.Sprintf("38;2;%d;%d;%d", rgb.R, rgb.G, rgb.B))
	} else if style.Foreground == theme.Reset {
		codes = append(codes, "39")
	}
	if rgb, ok := style.Background.RGBA(); ok {
		codes = append(codes, fmt.Sprintf("48;2;%d;%d;%d", rgb.R, rgb.G, rgb.B))
	} else if style.Background == theme.Reset {
		codes = append(codes, "49")
	}
	if len(codes) == 1 {
		return ""
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}
