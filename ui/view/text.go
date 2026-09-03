package view

import (
	"strings"

	"github.com/rivo/uniseg"
)

// Truncate fits text to a terminal-cell width without splitting graphemes.
// Marker is itself clipped when the available width is smaller than it.
func Truncate(text string, width int, marker string) string {
	if width <= 0 {
		return ""
	}
	if uniseg.StringWidth(text) <= width {
		return text
	}
	marker = truncateWithoutMarker(marker, width)
	return truncateWithoutMarker(text, width-uniseg.StringWidth(marker)) + marker
}

func truncateWithoutMarker(text string, width int) string {
	if width <= 0 {
		return ""
	}
	var result strings.Builder
	used := 0
	graphemes := uniseg.NewGraphemes(text)
	for graphemes.Next() {
		part := graphemes.Str()
		partWidth := uniseg.StringWidth(part)
		if used+partWidth > width {
			break
		}
		result.WriteString(part)
		used += partWidth
	}
	return result.String()
}
