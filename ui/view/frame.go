// Package view provides a renderer-neutral terminal-cell frame.
package view

import (
	"fmt"
	"strings"
	"unsafe"

	"github.com/raulfrk/herdr-plugin-kit/diagnostics"
	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/rivo/uniseg"
)

// Style is deliberately small and comparable so renderers can coalesce runs.
type Style struct {
	Foreground theme.Color
	Background theme.Color
	Bold       bool
	Dim        bool
	Underline  bool
}

// Cell contains one grapheme at its leading cell. Continuation cells reserve
// the remaining columns of a wide grapheme.
type Cell struct {
	Text         string
	Style        Style
	Width        int
	Continuation bool
}

// Frame is a fixed-size grid measured in terminal cells.
type Frame struct {
	width   int
	height  int
	cells   []Cell // nil represents untouched zero cells; readers use CellAt.
	textFit diagnostics.TextFitReport
}

type TextMode string

const (
	TextClip     TextMode = "clip"
	TextTruncate TextMode = "truncate"
	TextWrap     TextMode = "wrap"
)

type TextBoxOptions struct {
	Element                       diagnostics.ID
	Instance, X, Y, Width, Height int
	Mode                          TextMode
	AllowTruncation               bool
}

// TextFit returns an independent copy of the frame's bounded text-fit report.
func (f *Frame) TextFit() diagnostics.TextFitReport {
	cloned := diagnostics.CloneTextFit(&f.textFit)
	return *cloned
}

// PutTextBox lays out hard lines inside a bounded cell rectangle and records
// only static geometry and fit outcomes. Ordinary clipping is not an error.
func (f *Frame) PutTextBox(options TextBoxOptions, lines []string, style Style) error {
	if options.Element.IsZero() || options.Instance < 0 || options.Width < 0 || options.Height < 0 {
		return fmt.Errorf("invalid text box")
	}
	if options.Mode != TextClip && options.Mode != TextTruncate && options.Mode != TextWrap {
		return fmt.Errorf("invalid text mode")
	}

	originalWidth := 0
	laidOut := make([]string, 0, len(lines))
	wrapped, truncated, clipped := false, false, false
	for _, line := range lines {
		var lineWidth int
		switch options.Mode {
		case TextWrap:
			lineWidth = max(0, uniseg.StringWidth(line))
			rows, lost := wrapText(line, options.Width)
			wrapped = wrapped || len(rows) > 1
			clipped = clipped || lost
			laidOut = append(laidOut, rows...)
		case TextTruncate:
			var rendered string
			rendered, lineWidth = truncateMeasured(line, options.Width, "…")
			truncated = truncated || lineWidth > options.Width || options.Width == 0 && line != ""
			clipped = clipped || options.Width == 0 && line != ""
			laidOut = append(laidOut, rendered)
		case TextClip:
			var rendered string
			rendered, lineWidth = truncateMeasured(line, options.Width, "")
			clipped = clipped || lineWidth > options.Width || options.Width == 0 && line != ""
			laidOut = append(laidOut, rendered)
		}
		originalWidth = max(originalWidth, lineWidth)
	}

	for row, line := range laidOut {
		lineWidth := uniseg.StringWidth(line)
		visible := lineWidth > 0
		if row >= options.Height {
			clipped = clipped || visible
			continue
		}
		y, ok := checkedAdd(options.Y, row)
		if !ok {
			clipped = clipped || visible
			continue
		}
		if visible && (y < 0 || y >= f.height || options.X < 0 || options.X > f.width || lineWidth > f.width-options.X) {
			clipped = true
		}
		f.PutText(options.X, y, line, style)
	}

	observation := diagnostics.TextFitObservation{
		Element: options.Element, Instance: options.Instance, OriginalColumns: originalWidth,
		LayoutRows: len(laidOut), AvailableColumns: options.Width, AvailableRows: options.Height,
		Intent: diagnostics.TextFitIntent(options.Mode), Wrapped: wrapped, Truncated: truncated,
		Clipped: clipped, AllowTruncation: options.AllowTruncation,
	}
	if len(f.textFit.Observations) < diagnostics.MaxTextFitObservations {
		f.textFit.Observations = append(f.textFit.Observations, observation)
	} else {
		f.textFit.Omitted++
	}
	return nil
}

func wrapText(text string, width int) ([]string, bool) {
	if text == "" {
		return []string{""}, false
	}
	if width == 0 {
		return []string{""}, true
	}
	rows := make([]string, 0, 1)
	var row strings.Builder
	used := 0
	lost := false
	state := -1
	for text != "" {
		var part string
		var partWidth int
		part, text, partWidth, state = uniseg.FirstGraphemeClusterInString(text, state)
		if partWidth <= 0 {
			continue
		}
		if partWidth > width {
			lost = true
			continue
		}
		if used > 0 && partWidth > width-used {
			rows = append(rows, row.String())
			row.Reset()
			used = 0
		}
		row.WriteString(part)
		used += partWidth
	}
	rows = append(rows, row.String())
	return rows, lost
}

func checkedAdd(left, right int) (int, bool) {
	if right > 0 && left > int(^uint(0)>>1)-right {
		return 0, false
	}
	return left + right, true
}

func NewFrame(width, height int) (*Frame, error) {
	if width < 0 || height < 0 {
		return nil, fmt.Errorf("negative frame size %dx%d", width, height)
	}
	maxInt := int(^uint(0) >> 1)
	cellSize := int(unsafe.Sizeof(Cell{}))
	maxCells := maxInt / cellSize
	if dimensionsOverflow(width, height, maxCells) {
		return nil, fmt.Errorf("frame size overflows int: %dx%d", width, height)
	}
	return &Frame{width: width, height: height, textFit: diagnostics.TextFitReport{Observations: []diagnostics.TextFitObservation{}}}, nil
}

func dimensionsOverflow(width, height, maxCells int) bool {
	return height != 0 && width > maxCells/height
}

func (f *Frame) Width() int  { return f.width }
func (f *Frame) Height() int { return f.height }

func (f *Frame) CellAt(x, y int) (Cell, bool) {
	if !f.inBounds(x, y) {
		return Cell{}, false
	}
	if f.cells == nil {
		return Cell{}, true
	}
	return f.cells[f.index(x, y)], true
}

// Fill replaces a clipped rectangle with styled blank cells.
func (f *Frame) Fill(x, y, width, height int, style Style) {
	startX, endX := clipRange(x, width, f.width)
	startY, endY := clipRange(y, height, f.height)
	if f.cells == nil {
		f.cells = make([]Cell, f.width*f.height)
	}
	for row := startY; row < endY; row++ {
		for col := startX; col < endX; col++ {
			f.clearGlyphAt(col, row)
			f.cells[f.index(col, row)] = Cell{Style: style, Width: 1}
		}
	}
}

// PutText places complete grapheme clusters. A cluster intersecting either
// horizontal clipping edge is omitted rather than split.
func (f *Frame) PutText(x, y int, text string, style Style) {
	if y < 0 || y >= f.height {
		return
	}
	if f.cells == nil {
		f.cells = make([]Cell, f.width*f.height)
	}
	column := x
	state := -1
	for text != "" {
		if column >= f.width {
			return
		}
		var cluster string
		var width int
		cluster, text, width, state = uniseg.FirstGraphemeClusterInString(text, state)
		if width <= 0 {
			continue
		}
		if column >= 0 && column <= f.width-width {
			for i := range width {
				f.clearGlyphAt(column+i, y)
			}
			f.cells[f.index(column, y)] = Cell{Text: cluster, Style: style, Width: width}
			for i := 1; i < width; i++ {
				f.cells[f.index(column+i, y)] = Cell{Style: style, Continuation: true}
			}
		}
		column += width
	}
}

func clipRange(origin, length, limit int) (int, int) {
	if length <= 0 {
		return 0, 0
	}
	if origin < 0 {
		return 0, min(origin+length, limit)
	}
	return origin, origin + min(length, limit-origin)
}

func (f *Frame) inBounds(x, y int) bool { return x >= 0 && x < f.width && y >= 0 && y < f.height }
func (f *Frame) index(x, y int) int     { return y*f.width + x }

func (f *Frame) clearGlyphAt(x, y int) {
	if !f.inBounds(x, y) {
		return
	}
	lead := x
	// A continuation at column zero cannot be produced by Frame methods.
	for f.cells[f.index(lead, y)].Continuation {
		lead--
	}
	cell := f.cells[f.index(lead, y)]
	span := max(cell.Width, 1)
	// Every stored glyph was admitted only when its full span fit in the frame.
	for i := range span {
		f.cells[f.index(lead+i, y)] = Cell{}
	}
}
