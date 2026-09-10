// Package keymapui provides the shared Actions, shortcut editor and key checker
// surfaces, with revision-checked configuration owned by the shell event loop.
package keymapui

import (
	"context"
	"errors"
	"os"

	"github.com/pelletier/go-toml/v2"
	"github.com/raulfrk/herdr-plugin-kit/config"
	"github.com/raulfrk/herdr-plugin-kit/documentstore"
	"github.com/raulfrk/herdr-plugin-kit/ui/keymap"
)

const configName = "keymap.toml"

type diskState struct {
	document keymap.Document
	revision documentstore.Revision
	valid    bool
	warning  string
}

type configuration struct {
	store  *documentstore.Store
	loader config.Loader[keymap.Document]
}

func openConfiguration(directory string, runtime *keymap.Runtime) (*configuration, error) {
	store, err := documentstore.Open(directory)
	if err != nil {
		return nil, err
	}
	return &configuration{store: store, loader: config.Loader[keymap.Document]{Validate: runtime.Validate}}, nil
}

func (c *configuration) read(ctx context.Context) diskState {
	document, err := c.store.Read(ctx, configName)
	if errors.Is(err, os.ErrNotExist) {
		return diskState{document: keymap.Document{Version: 1}, valid: true}
	}
	if err != nil {
		return diskState{warning: "Cannot read keyboard shortcuts; keeping the current map."}
	}
	value, err := c.loader.Decode(document.Bytes)
	if err != nil {
		return diskState{revision: document.Revision, warning: "Invalid keyboard shortcuts; keeping the current map. Open the editor to repair."}
	}
	return diskState{document: value, revision: document.Revision, valid: true}
}

func (c *configuration) save(ctx context.Context, value keymap.Document, expected documentstore.Revision) (diskState, error) {
	if err := c.loader.Validate(value); err != nil {
		return diskState{}, err
	}
	data, err := toml.Marshal(value)
	if err != nil {
		return diskState{}, errors.New("cannot encode keyboard shortcuts")
	}
	revision, err := c.store.WritePrivate(ctx, configName, data, expected)
	if err != nil {
		return diskState{}, err
	}
	return diskState{document: keymap.Clone(value), revision: revision, valid: true}, nil
}
