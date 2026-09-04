package interaction

import (
	"testing"

	"github.com/rivo/uniseg"
	"pgregory.net/rapid"
)

func TestBufferEditsPasteAndIMECommitsByGrapheme(t *testing.T) {
	buffer := NewBuffer("Ae\u0301🙂")
	if buffer.GraphemeCount() != 3 || buffer.Cursor() != 3 {
		t.Fatalf("initial buffer = %q cursor=%d count=%d", buffer.Text(), buffer.Cursor(), buffer.GraphemeCount())
	}
	buffer.MoveLeft()
	buffer.Insert("東京👨‍👩‍👧‍👦")
	if got := buffer.Text(); got != "Ae\u0301東京👨‍👩‍👧‍👦🙂" {
		t.Fatalf("inserted buffer = %q", got)
	}
	if !buffer.Backspace() || !buffer.Delete() || buffer.Text() != "Ae\u0301東京" {
		t.Fatalf("edited buffer = %q", buffer.Text())
	}
	buffer.MoveHome()
	if buffer.Backspace() {
		t.Fatal("backspace before start changed buffer")
	}
	buffer.MoveEnd()
	if buffer.Delete() {
		t.Fatal("delete after end changed buffer")
	}
}

func TestModelBufferMatchesGraphemeSlice(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		initial := rapid.String().Draw(t, "initial")
		inserted := rapid.String().Draw(t, "inserted")
		buffer := NewBuffer(initial)
		want := splitGraphemes(initial)
		cursor := rapid.IntRange(-3, len(want)+3).Draw(t, "cursor")
		cursor = max(0, min(cursor, len(want)))
		buffer.SetCursor(cursor)
		prefix := join(want[:cursor]) + inserted
		model := splitGraphemes(prefix + join(want[cursor:]))
		buffer.Insert(inserted)
		wantCursor := len(splitGraphemes(prefix))
		if buffer.Text() != join(model) || buffer.Cursor() != wantCursor || uniseg.GraphemeClusterCount(buffer.Text()) != len(model) {
			t.Fatalf("buffer=%q/%d model=%q/%d", buffer.Text(), buffer.Cursor(), join(model), wantCursor)
		}
	})
}

func TestBufferCursorMovementAndEditsMatchStateModel(t *testing.T) {
	buffer := NewBuffer("A界e\u0301🙂")
	steps := []struct {
		name       string
		apply      func(*Buffer)
		wantText   string
		wantCursor int
	}{
		{name: "left", apply: func(buffer *Buffer) { buffer.MoveLeft() }, wantText: "A界e\u0301🙂", wantCursor: 3},
		{name: "left again", apply: func(buffer *Buffer) { buffer.MoveLeft() }, wantText: "A界e\u0301🙂", wantCursor: 2},
		{name: "right", apply: func(buffer *Buffer) { buffer.MoveRight() }, wantText: "A界e\u0301🙂", wantCursor: 3},
		{name: "insert", apply: func(buffer *Buffer) { buffer.Insert("👨‍👩‍👧‍👦") }, wantText: "A界e\u0301👨‍👩‍👧‍👦🙂", wantCursor: 4},
		{name: "backspace", apply: func(buffer *Buffer) { buffer.Backspace() }, wantText: "A界e\u0301🙂", wantCursor: 3},
		{name: "delete", apply: func(buffer *Buffer) { buffer.Delete() }, wantText: "A界e\u0301", wantCursor: 3},
	}
	for _, step := range steps {
		step.apply(&buffer)
		if buffer.Text() != step.wantText || buffer.Cursor() != step.wantCursor || buffer.GraphemeCount() != uniseg.GraphemeClusterCount(step.wantText) {
			t.Fatalf("%s: buffer=%q cursor=%d count=%d, want %q cursor=%d", step.name, buffer.Text(), buffer.Cursor(), buffer.GraphemeCount(), step.wantText, step.wantCursor)
		}
	}

	buffer.SetCursor(-10)
	buffer.MoveLeft()
	if buffer.Cursor() != 0 || buffer.Text() != "A界e\u0301" {
		t.Fatalf("left boundary changed state: %q/%d", buffer.Text(), buffer.Cursor())
	}
	buffer.SetCursor(100)
	buffer.MoveRight()
	if buffer.Cursor() != buffer.GraphemeCount() || buffer.Text() != "A界e\u0301" {
		t.Fatalf("right boundary changed state: %q/%d", buffer.Text(), buffer.Cursor())
	}
}

func join(parts []string) string {
	result := ""
	for _, part := range parts {
		result += part
	}
	return result
}
