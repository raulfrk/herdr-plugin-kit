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

## Configurable keyboard shortcuts

The generated `plugin ui` and `plugin debug` commands use the shared keyboard
shortcut UI. Pass `--default-keymap` to either command to ignore live overrides
for that run and repair the saved file from the editor. Outside shortcut
recording and the key checker, `Alt+D` switches views and `Ctrl+K` opens Actions.
Outside text entry, `d` switches views and `:` also opens Actions. `Tab`
traverses the content and the focusable Actions control; press `Enter` on that
control to open it. `Ctrl+C` always quits and cannot be rebound.

Actions contains commands that are available for the current view and selected
target. The full editor lists every command. It can add, replace, remove, or
clear aliases; reset one command or all commands; and save or discard the
draft. While recording a shortcut, type its strokes (for example `g`, `r`) and
press `Enter` to finish or `Escape` to cancel. The key checker displays the
normalized delivered key and modifiers plus the latest eight strokes. Press
`Escape` twice within one second to leave it.

Bindings are private to the plugin configuration directory at
`HERDR_PLUGIN_CONFIG_DIR/keymap.toml`:

```toml
version = 1

[[bindings]]
context = "interaction.picker.results"
action = "picker.next-page"
sequences = [["g", "r"], ["f5"]]
```

A missing binding uses its declared defaults. `sequences = []` leaves the
action unbound, while reset removes the override. Aliases are contextual. They
cannot duplicate or prefix another alias in the same context, even for the same
action, and an editing context cannot start with a printable key. Ordinary IME
input and paste remain text. Prefixes wait for one second; after a timeout or
mismatch, the unmatched stroke is handled once as fresh input.

The file is validated before activation. Invalid startup content uses defaults
and shows a warning; a later invalid edit keeps the last valid map active. The
UI polls for changes every 500 ms, so instances of the same plugin that share a
configuration directory activate updates after the next completed poll. Draft
recording, capture, and the checker defer activation. Saves check the file
revision and offer Reload or Discard after a conflict. Shortcut capture is
saved as a binding only when the draft is saved. Key-checker input is never
saved; paste contents and captured values are not logged.

Wrap a mapped main surface and optional Debug surface, keep the wrapper alive,
and give its runtime to the shell:

```go
surface, err := keymapui.New(keymapui.Options{
    Main: mainSurface, Debug: debugSurface,
    ConfigDirectory: configDirectory,
    StartDebug: startDebug, DefaultKeymap: defaultKeymap,
})
if err != nil { return err }
defer surface.Close()

program, err := shell.NewProgram(shell.ProgramOptions{
    PluginID: pluginID, Theme: palette, Events: recorder,
    Keymap: surface.Runtime(),
}, surface)
```

`Main` and `Debug` implement both `shell.Surface` and `shell.ActionSurface`;
the standard Picker, Form, and Debug UI already do. A custom surface declares
stable actions and reports the current context and availability:

```go
func (s *surface) KeymapCatalog() []keymap.Action {
    return []keymap.Action{{
        ID: "results.refresh", Label: "Refresh", Context: "results.list",
        Quick: true, Defaults: []keymap.Sequence{{"r"}, {"g", "r"}},
    }}
}
func (s *surface) KeymapContext() keymap.Context {
    return keymap.Context{ID: "results.list", Target: s.selectedID}
}
func (s *surface) ActionState(id string) keymap.State {
    return keymap.State{Enabled: id == "results.refresh" && !s.loading,
        Reason: "refresh unavailable"}
}
func (s *surface) Update(events shell.EventContext, event shell.Event) []shell.Effect {
    switch event := event.(type) {
    case shell.ActionEvent:
        if event.ID == "results.refresh" && s.ActionState(event.ID).Enabled {
            return s.refresh(events) // shared by the hotkey and Actions menu
        }
    }
    return nil
}
```

Set `Editing: true` on both the action and its `keymap.Context` for text-entry
contexts. `Target` is an opaque stable identity or revision; change it when the
selected object is replaced. The Actions menu rechecks both target and state
before invoking a command. Custom hosts can instead call `keymap.New(catalog)`
and pass that runtime through `shell.ProgramOptions.Keymap`. Omitting `Keymap`
preserves the shell's legacy input behavior.

Each generated UI still owns diagnostics under its
`HERDR_PLUGIN_STATE_DIR`. Use distinct state directories when testing several
instances against one shared configuration directory; shared shortcut updates
do not add concurrent diagnostic-writer support. Configuration is per plugin;
there is no global or cross-plugin configurator API.

## Recall and Codex Recall

Adapt the generated page loader to the existing recall search semantics. Rank
the complete matching set before slicing an opaque page, preserve equal-score
source order, and let the picker discard stale request generations. Codex
Recall can additionally expose static no-input actions through
`interop.ActionClient`; a successful call is identified by its exact Herdr log
receipt and strictly typed `ActionResponse`.

## Attention Switcher

Use `agenthost.Host.List`, then `agenthost.Probe.Assess` before presenting a
status. Activate with `Host.Focus` using the complete returned identity; do not
reconstruct a target from labels.

For Codex, set `Host.CodexSocket` to an absolute local Unix App Server socket
path. The normal Codex TUI and the probe must connect to the same server; the
Herdr agent must expose its Codex session ID. For example, with Codex 0.153.4,
start a server manually in one terminal:

```sh
cd /absolute/project
codex app-server --listen unix:///absolute/runtime/codex.sock \
  -c 'model="gpt-6-astra"' -c 'model_reasoning_effort="low"' \
  -c 'sandbox_mode="read-only"' -c 'approval_policy="on-request"'
```

Then start the normal TUI in a Herdr pane:

```sh
codex --remote unix:///absolute/runtime/codex.sock -C /absolute/project \
  -m gpt-6-astra -c 'model_reasoning_effort="low"' \
  -s read-only -a on-request
```

Use existing writable directories for the socket and an explicit project CWD.
Handle trust and approvals in the TUI yourself. The permissions above are
initial choices; later user changes in the TUI remain authoritative.

```go
host := agenthost.Host{
    Runner: runner, Herdr: "/opt/herdr", Session: "work",
    CodexSocket: "/absolute/runtime/codex.sock",
}
agents, err := host.List(ctx)
// Handle err and choose an Agent from agents without rebuilding its identity.
assessment, err := agenthost.NewProbe(host).Assess(ctx, agents[0])
```

Each Codex assessment creates and closes a fresh Unix WebSocket connection.
It only sends `initialize`, `initialized`, and
`thread/read` with `includeTurns: false`; it does not load/resume a thread,
subscribe, read pane text, answer approvals, or change the TUI or server
lifecycle. See the [App Server protocol](https://learn.chatgpt.com/docs/app-server).
The caller owns server setup and any reconnect/resume decisions. There is no
daemon, session registry, cached status, or automatic infrastructure.

Codex runtime states map as follows:

| App Server state | Assessment |
|---|---|
| `idle` | `Idle`, `CodexStatus` |
| `active`, no flags | `Working`, `CodexStatus` |
| `waitingOnApproval` | `Blocked`, `Approval` |
| `waitingOnUserInput` | `Blocked`, `UserInput` |
| Both waiting flags | `Blocked`, `ApprovalAndInput` |
| `notLoaded` | `Unknown`, `NotManaged` |
| `systemError` | `Unknown`, `AgentSystemError` |
| Unknown type or flag | `Unknown`, `UnsupportedStatus` |

`Stable` for Codex means a recognized definitive snapshot with matching
immutable Herdr identity before and after the read. It is not a quiet-period
guarantee or proof that no queued work exists. Volatile Herdr status, revision,
and sequence changes do not invalidate it. Every Codex `Unknown` is unstable.
An unmanaged or unloaded session is never inferred to be idle.

Missing socket configuration or a missing usable session ID returns
`Unknown` with `MissingCodexSocket` or `MissingCodexSession` and no error.
Unloaded, system-error and unsupported runtime states also return no error.
Invalid input, transport/protocol failure, cancellation or identity changes
return an unstable `Unknown` and a sanitized error; identity drift uses
`Unsettled` / `ErrStaleReport`. Response IDs and the returned thread ID must
match. Only fields needed for initialization and status are consumed;
additional metadata is tolerated and never exposed in an assessment.

The entire Codex assessment, including both Herdr reads and connection
cleanup, has a two-second budget; an earlier caller deadline wins.
Responses are limited to 1 MiB per message, 64 messages and 4 MiB total per
connection. Notifications count toward these limits and are discarded.
No raw server errors or transcript previews are returned. Other agent kinds
retain their existing Herdr-based assessment behavior. Legacy terminal-text
reason constants remain exported for source compatibility but are no longer
emitted by Codex assessments.

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
