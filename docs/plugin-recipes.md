# Plugin recipes

Generate the first implementation with `herdr-plugin-kit new`. The emitted
searchable starter is the executable reference: keep its single shell,
responsive picker, debug router, semantic preview sink, bounded export, and
revision-checked receipts, then replace only the provider and activation logic.
The design catalogue is development tooling and is never copied into a plugin.

## Shared rules

- Providers own ranking and return final stable order. Use `interaction.Rank`
  when the plugin itself performs in-memory fuzzy search.
- Treat `interaction.Cursor` as opaque outside the provider. Bind every cursor
  to its query and reject a cursor from an older query.
- Keep one `shell.Program`; route the normal and standard Debug UI surfaces in
  that process so they share the recorder and registered previews.
- Put configuration and state only beneath Herdr's absolute plugin directories.
  Use `documentstore.Store` for checked writes and observable receipts.
- Record semantic identifiers, outcomes, generations, counts, and geometry.
  Never record query text, result content, paths, configuration values, or raw
  terminal output.

## Recall and Codex Recall

Adapt the generated page loader to the existing recall search semantics. Rank
the complete matching set before slicing an opaque page, preserve equal-score
source order, and let the picker discard stale request generations. Codex
Recall can additionally expose static no-input actions through
`interop.ActionClient`; a successful call is identified by its exact Herdr log
receipt and strictly typed `ActionResponse`.

## Attention Switcher

Use `agenthost.Host.List`, then `agenthost.Probe.Assess` before presenting a
status. The Codex-aware parser's `Unsettled` and `Unrecognized` outcomes are
real states, not aliases for idle or finished. Activate with `Host.Focus` using
the complete returned identity; do not reconstruct a target from labels.

## Session Switcher

Use `sessionhost.Host.List` as the provider. Opening in an agent-selected
directory is the explicit `OpenAtDirectory` composition; display its returned
workspace identity as the activation receipt. Agent status shown beside a
session still comes from `agenthost.Probe`, not Herdr's host status alone.

## Plugin Configurator

Decode TOML with `config.Loader`, retain the last valid value while watching,
and write through `documentstore.Store.Write` with the revision returned by
`Read`. A conflict means the UI must reload or ask the user—it must never
silently overwrite. Invalid edits stay visible as drafts but do not replace the
active configuration.

## Action Finder

Build the provider from `actionhost.Host.List` results and use
`interop.ActionClient.Invoke` for statically declared no-input interfaces. Keep
the exact action identity and returned log ID through discovery, invocation,
polling, and the final typed response. Herdr 0.8.2 does not support dynamic
request payloads, so unsupported actions must be disabled with a reason.

## Readiness proof

`go test ./consumerreadiness` exercises these six recipes only through exported
kit APIs. A generated plugin must additionally pass its emitted tests,
`herdr-plugin-kit validate`, the repository race and mutation gates, and the
labelled live-device acceptance sequence before release.
