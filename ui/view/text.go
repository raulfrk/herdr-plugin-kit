package view

import (
	"strings"

	"github.com/rivo/uniseg"
)

// Wrap splits text into terminal-cell rows without splitting graphemes. It
// preserves explicit line breaks and uses the same layout as TextWrap boxes.
func Wrap(text string, width int) []string {
	var rows []string
	for _, line := range strings.Split(text, "\n") {
		wrapped, _ := wrapText(line, max(0, width))
		rows = append(rows, wrapped...)
	}
	return rows
}

// Truncate fits text to a terminal-cell width without splitting graphemes.
// Marker is itself clipped when the available width is smaller than it.
func Truncate(text string, width int, marker string) string {
	if width <= 0 {
		return ""
	}
	rendered, _ := truncateMeasured(text, width, marker)
	return rendered
}

func truncateMeasured(text string, width int, marker string) (rendered string, originalColumns int) {
	marker = truncateWithoutMarker(marker, width)
	prefixBudget := width - uniseg.StringWidth(marker)
	capturing := prefixBudget > 0
	prefixEnd := 0
	rest, state := text, -1
	for rest != "" {
		var cluster string
		var clusterWidth int
		cluster, rest, clusterWidth, state = uniseg.FirstGraphemeClusterInString(rest, state)
		originalColumns += clusterWidth
		if capturing {
			if originalColumns > prefixBudget {
				capturing = false
			} else {
				prefixEnd += len(cluster)
			}
		}
	}
	if width <= 0 {
		return "", originalColumns
	}
	if originalColumns <= width {
		return text, originalColumns
	}
	return text[:prefixEnd] + marker, originalColumns
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
