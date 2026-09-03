// Package interaction provides reusable terminal interaction state and surfaces.
package interaction

import (
	"strings"

	"github.com/rivo/uniseg"
)

// Buffer is a grapheme-aware single-line editing buffer. Insert treats each
// call atomically, which lets shell text and paste/IME commits share one path.
type Buffer struct {
	graphemes []string
	cursor    int
}

func NewBuffer(text string) Buffer {
	var buffer Buffer
	buffer.Set(text)
	return buffer
}

func (buffer *Buffer) Set(text string) {
	buffer.graphemes = splitGraphemes(text)
	buffer.cursor = len(buffer.graphemes)
}

func (buffer Buffer) Text() string       { return strings.Join(buffer.graphemes, "") }
func (buffer Buffer) Cursor() int        { return buffer.cursor }
func (buffer Buffer) GraphemeCount() int { return len(buffer.graphemes) }

func (buffer *Buffer) SetCursor(cursor int) {
	buffer.cursor = max(0, min(cursor, len(buffer.graphemes)))
}

func (buffer *Buffer) Insert(text string) {
	if text == "" {
		return
	}
	prefix := strings.Join(buffer.graphemes[:buffer.cursor], "") + text
	buffer.graphemes = splitGraphemes(prefix + strings.Join(buffer.graphemes[buffer.cursor:], ""))
	buffer.cursor = len(splitGraphemes(prefix))
}

func (buffer *Buffer) Backspace() bool {
	if buffer.cursor == 0 {
		return false
	}
	prefix := strings.Join(buffer.graphemes[:buffer.cursor-1], "")
	buffer.graphemes = splitGraphemes(prefix + strings.Join(buffer.graphemes[buffer.cursor:], ""))
	buffer.cursor = len(splitGraphemes(prefix))
	return true
}

func (buffer *Buffer) Delete() bool {
	if buffer.cursor == len(buffer.graphemes) {
		return false
	}
	prefix := strings.Join(buffer.graphemes[:buffer.cursor], "")
	buffer.graphemes = splitGraphemes(prefix + strings.Join(buffer.graphemes[buffer.cursor+1:], ""))
	buffer.cursor = len(splitGraphemes(prefix))
	return true
}

func (buffer *Buffer) MoveLeft()  { buffer.SetCursor(buffer.cursor - 1) }
func (buffer *Buffer) MoveRight() { buffer.SetCursor(buffer.cursor + 1) }
func (buffer *Buffer) MoveHome()  { buffer.cursor = 0 }
func (buffer *Buffer) MoveEnd()   { buffer.cursor = len(buffer.graphemes) }

func splitGraphemes(text string) []string {
	graphemes := uniseg.NewGraphemes(text)
	var result []string
	for graphemes.Next() {
		result = append(result, graphemes.Str())
	}
	return result
}
