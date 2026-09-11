# Herdr plugin contracts

The generated project is the starting contract. Preserve these parts unless a
requested behavior demonstrably requires a compatible extension:

- One `shell.Program` routes the main and standard Debug UI surfaces so they
  share the recorder, previews, and keymap runtime.
- The `keymapui` wrapper remains alive for the program lifetime. User overrides
  stay under `HERDR_PLUGIN_CONFIG_DIR/keymap.toml`; `Ctrl+C` remains reserved.
- Providers return final stable order. Use `interaction.Rank` for local fuzzy
  ranking and treat `interaction.Cursor` as opaque and query-bound.
- Configuration and state use the absolute directories supplied by Herdr.
  Checked document writes use `documentstore.Store` and their returned
  revisions rather than silently overwriting conflicts.
- Semantic diagnostics may record identifiers, outcomes, generations, counts,
  and geometry. They must not record query text, result content, paths,
  configuration values, captured keys, paste contents, or raw terminal output.
- `plugin-kit.json`, `herdr-plugin.toml`, and `go.mod` retain matching plugin
  identity and supported versions. Herdr build, action, and pane commands remain
  argv arrays rather than shell command strings.
- Activation produces an observable receipt or result. Errors remain actionable
  without disclosing sensitive input.

Use the versioned kit recipes for capability-specific composition:

- https://github.com/raulfrk/herdr-plugin-kit/blob/v0.2.0/docs/plugin-recipes.md
- https://github.com/raulfrk/herdr-plugin-kit/blob/v0.2.0/docs/diagnostics.md
- https://github.com/raulfrk/herdr-plugin-kit/blob/v0.2.0/docs/documentstore.md
- https://github.com/raulfrk/herdr-plugin-kit/blob/v0.2.0/docs/interop.md
- https://github.com/raulfrk/herdr-plugin-kit/blob/v0.2.0/docs/ui-contract.md
