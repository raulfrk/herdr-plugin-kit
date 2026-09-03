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

func join(parts []string) string {
	result := ""
	for _, part := range parts {
		result += part
	}
	return result
}
