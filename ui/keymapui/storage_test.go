package keymapui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/documentstore"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
)

func configForTest(t *testing.T, directory string) *configuration {
	t.Helper()
	runtime, err := keymap.New([]keymap.Action{{ID: "refresh", Label: "Refresh", Context: "results", Defaults: []keymap.Sequence{{"f5"}}}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := openConfiguration(directory, runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.store.Close() })
	return c
}
func TestConfigurationCheckedPrivateWritesAndStrictReads(t *testing.T) {
	directory := t.TempDir()
	left, right := configForTest(t, directory), configForTest(t, directory)
	ctx := context.Background()
	a, b := left.read(ctx), right.read(ctx)
	if !a.valid || !b.valid || a.document.Version != 1 {
		t.Fatal("missing defaults", a, b)
	}
	draft := keymap.Document{Version: 1, Bindings: []keymap.Binding{{Context: "results", Action: "refresh", Sequences: []keymap.Sequence{{"g", "r"}}}}}
	saved, err := left.save(ctx, draft, a.revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := right.save(ctx, draft, b.revision); !errors.Is(err, documentstore.ErrConflict) {
		t.Fatal("concurrent save", err)
	}
	read := right.read(ctx)
	if !read.valid || read.revision != saved.revision {
		t.Fatal("other instance did not observe save", read)
	}
	info, err := os.Stat(filepath.Join(directory, configName))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private file", info, err)
	}
	if err := os.WriteFile(filepath.Join(directory, configName), []byte("version = 1\nunknown = 'private-marker'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	invalid := left.read(ctx)
	if invalid.valid || invalid.warning == "" || invalid.revision == saved.revision {
		t.Fatal("invalid file accepted", invalid)
	}
	if _, err := left.save(ctx, draft, saved.revision); !errors.Is(err, documentstore.ErrConflict) {
		t.Fatal("invalid external change overwritten", err)
	}
	if _, err := left.save(ctx, keymap.Document{Version: 1}, invalid.revision); err != nil {
		t.Fatal("explicit repair", err)
	}
}
