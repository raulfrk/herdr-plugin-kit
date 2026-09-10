package keymapui

import (
	"context"
	"fmt"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/ui/interaction"
)

func TestShortcutListSearchAndPagination(t *testing.T) {
	for _, count := range []int{0, 49, 50, 51, 100, 101} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			l := &list{}
			for index := range count {
				l.items = append(l.items, interaction.Item{Key: fmt.Sprint(index), Label: fmt.Sprintf("Action %03d", index), Description: fmt.Sprintf("context-%03d", index)})
			}
			seen := map[string]bool{}
			cursor := interaction.Cursor{}
			for pages := 0; ; pages++ {
				if pages > count/50+1 {
					t.Fatal("pagination did not terminate")
				}
				page, err := l.load(context.Background(), "", cursor)
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Items) > 50 || (pages > 0 && len(page.Items) == 0) {
					t.Fatal("invalid page size", len(page.Items))
				}
				for _, item := range page.Items {
					if seen[item.Key] {
						t.Fatal("duplicate result", item.Key)
					}
					seen[item.Key] = true
				}
				if page.Next.Empty() {
					break
				}
				cursor = page.Next
			}
			if len(seen) != count {
				t.Fatalf("received %d of %d actions", len(seen), count)
			}
			if count > 0 {
				page, err := l.load(context.Background(), "context-000", interaction.Cursor{})
				if err != nil || len(page.Items) != 1 || page.Items[0].Key != "0" {
					t.Fatal("description search did not find its action", page, err)
				}
			}
			for _, token := range []string{"invalid", "-1", fmt.Sprint(count + 1)} {
				if _, err := l.load(context.Background(), "", interaction.NewCursor(token)); err == nil {
					t.Fatal("invalid or obsolete cursor accepted", token)
				}
			}
		})
	}
}
