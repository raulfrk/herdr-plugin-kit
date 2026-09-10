package keymapui

import (
	"context"
	"errors"
	"strconv"
	"sync"

	"github.com/raulfrk/herdr-plugin-kit/ui/interaction"
	"github.com/raulfrk/herdr-plugin-kit/ui/shell"
)

// A list owns one picker for its entire lifetime. Its provider reads immutable
// row snapshots so asynchronous searches never read event-loop state.
type list struct {
	picker *interaction.Picker
	mu     sync.RWMutex
	items  []interaction.Item
}

func newList(title, namespace string) (*list, error) {
	l := &list{}
	picker, err := interaction.NewPicker(interaction.PickerOptions{Title: title, Namespace: namespace, Load: l.load})
	if err != nil {
		return nil, err
	}
	l.picker = picker
	return l, nil
}

func (l *list) replace(events shell.EventContext, items []interaction.Item) []shell.Effect {
	l.mu.Lock()
	l.items = append([]interaction.Item(nil), items...)
	l.mu.Unlock()
	return l.picker.Refresh(events)
}

func (l *list) load(_ context.Context, query string, cursor interaction.Cursor) (interaction.Page, error) {
	l.mu.RLock()
	items := append([]interaction.Item(nil), l.items...)
	l.mu.RUnlock()
	candidates := make([]interaction.Candidate, len(items))
	byKey := make(map[string]interaction.Item, len(items))
	for i, item := range items {
		candidates[i] = interaction.Candidate{Key: item.Key, Text: item.Label + " " + item.Description}
		byKey[item.Key] = item
	}
	matches := interaction.Rank(query, candidates)
	offset := 0
	if !cursor.Empty() {
		var err error
		offset, err = strconv.Atoi(cursor.Token())
		if err != nil || offset < 0 || offset > len(matches) {
			return interaction.Page{}, errors.New("invalid list cursor")
		}
	}
	end := min(offset+50, len(matches))
	page := interaction.Page{}
	for _, match := range matches[offset:end] {
		page.Items = append(page.Items, byKey[match.Candidate.Key])
	}
	if end < len(matches) {
		page.Next = interaction.NewCursor(strconv.Itoa(end))
	}
	return page, nil
}
