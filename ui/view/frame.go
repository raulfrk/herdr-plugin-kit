// Package view provides a renderer-neutral terminal-cell frame.
package view

import (
	"fmt"
	"unsafe"

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
	width  int
	height int
	cells  []Cell
}

func NewFrame(width, height int) (*Frame, error) {
	if width < 0 || height < 0 {
		return nil, fmt.Errorf("negative frame size %dx%d", width, height)
	}
	maxInt := int(^uint(0) >> 1)
	cellSize := int(unsafe.Sizeof(Cell{}))
	maxCells := maxInt / cellSize
	if height != 0 && width > maxCells/height {
		return nil, fmt.Errorf("frame size overflows int: %dx%d", width, height)
	}
	return &Frame{width: width, height: height, cells: make([]Cell, width*height)}, nil
}

func (f *Frame) Width() int  { return f.width }
func (f *Frame) Height() int { return f.height }

func (f *Frame) CellAt(x, y int) (Cell, bool) {
	if !f.inBounds(x, y) {
		return Cell{}, false
	}
	return f.cells[f.index(x, y)], true
}

// Fill replaces a clipped rectangle with styled blank cells.
func (f *Frame) Fill(x, y, width, height int, style Style) {
	startX, endX := clipRange(x, width, f.width)
	startY, endY := clipRange(y, height, f.height)
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
	column := x
	graphemes := uniseg.NewGraphemes(text)
	for graphemes.Next() {
		if column >= f.width {
			return
		}
		cluster := graphemes.Str()
		width := uniseg.StringWidth(cluster)
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
	for lead > 0 && f.cells[f.index(lead, y)].Continuation {
		lead--
	}
	cell := f.cells[f.index(lead, y)]
	span := max(cell.Width, 1)
	for i := range min(span, f.width-lead) {
		f.cells[f.index(lead+i, y)] = Cell{}
	}
}
