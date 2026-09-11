---
name: herdr-plugin-development
description: Create, modify, validate, test, or review Go plugins built with Herdr Plugin Kit. Use for Herdr plugin repositories and requests involving herdr-plugin.toml or plugin-kit.json. Do not use for extensions to Codex, OpenCode, Pi, or the plugin-kit library itself.
---

# Herdr Plugin Development

Build the smallest complete Herdr plugin requested by the user. Use the released
kit and its generated starter as the executable contract instead of recreating
the shell, diagnostics, keymap, or manifest infrastructure.

## Establish the target

Inspect the repository and its instructions before changing files. Determine
whether the request creates a plugin or modifies an existing one.

For a new plugin, establish these inputs:

- A stable lowercase plugin ID using dots between namespaces, such as
  `example.search`.
- A user-facing name and concise description.
- A destination directory that does not already exist.
- The provider behavior and the activation result visible to the user.

Ask for missing input only when it changes plugin identity, destination, data
access, or user-visible behavior. Resolve ordinary implementation details from
the generated project and the kit documentation.

For an existing plugin, read `plugin-kit.json`, `herdr-plugin.toml`, `go.mod`,
the command under `cmd/`, and its tests. Run validation before editing so an
existing contract failure is distinguished from the requested change. Do not
regenerate over an existing directory.

## Generate a new plugin

Use the exact released generator unless the repository already pins another
supported kit version:

```sh
go run github.com/raulfrk/herdr-plugin-kit/cmd/herdr-plugin-kit@v0.2.0 new \
  --id <plugin-id> \
  --name <display-name> \
  --description <description> \
  --output <new-directory>
```

Enter the generated directory and resolve its standalone module metadata before
running its tests:

```sh
cd <new-directory>
go mod tidy
```

Read the emitted `README.md`, manifests, source, and tests before editing. Keep
the generated single shell, responsive picker, Debug UI routing, semantic
diagnostics, shared keymap UI, bounded exports, and revision-checked receipts.
Replace the example provider, data, and activation logic needed for the current
plugin.

Read [the implementation contracts](references/contracts.md) before changing
the generated runtime composition. Use the detailed versioned recipes linked
there only for the capabilities the plugin actually needs.

## Modify an existing plugin

Preserve the versions and identity declared by the existing project unless the
user explicitly requests an upgrade. Keep `plugin-kit.json` and
`herdr-plugin.toml` consistent. Do not silently change action IDs, interface
versions, command vectors, pane placement, or the executable path.

Extend established provider, surface, and activation patterns. Add only the
capability required for the requested observable behavior. Keep configuration
and state inside the absolute directories supplied by Herdr.

## Validate the result

Follow [the validation workflow](references/validation.md). At minimum, resolve
module metadata for a newly generated plugin, run the kit validator, the plugin
tests, the race detector, and the generated build command. Also run
repository-specific checks required by local instructions.

Inspect the final diff for generated infrastructure that was accidentally
removed, sensitive values added to diagnostics, manifest drift, speculative
options, and unrelated cleanup. Report the exact commands and results. Treat a
live Herdr check as unverified unless it was actually performed on the target
host and viewport.
