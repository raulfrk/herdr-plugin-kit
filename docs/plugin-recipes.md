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

Generated Help remains scroll-only with its original Enter-to-close and `j/k`
defaults unless a consumer opts into `InteractiveHelp`. Interactive Help makes
the current context's actions selectable and opens the existing shortcut entry
editor for the selected action on Enter. A consumer may also declare bounded,
single-stroke Help commands; the wrapper closes Help and dispatches only the
declared main-surface action:

```go
surface, err := keymapui.New(keymapui.Options{
    Main: mainSurface, ConfigDirectory: configDirectory,
    InteractiveHelp: true,
    HelpOpenedAction: "plugin.help.opened",
    HelpCommands: []keymapui.HelpCommand{
        {ID: "check", Label: "check", Action: "plugin.bindings.check",
            Defaults: []keymap.Sequence{{"k"}}},
        {ID: "reset", Label: "reset", Action: "plugin.bindings.reset",
            Defaults: []keymap.Sequence{{"r"}}},
    },
})
```

Declared commands always return to `Main` before dispatch, including when Help
was opened from the shortcut editor or Debug UI, so consumer dialogs cannot be
hidden behind a wrapper screen. Command keys take precedence over legacy Help
navigation defaults; for example, a declared `k` leaves Up as the previous-row
key. Runtime conflict
validation still applies. `HelpOpenedAction` optionally notifies the main
surface before Help takes focus, allowing a consumer to consume an armed
one-shot navigation state without making Help product-specific. The consumer
remains responsible for action availability and product policy.

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
Herdr agent must expose its Codex session ID. For example, with Codex 0.154.0,
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
// The launcher supplies exact pane mappings without rebuilding Agent identity.
sources := agenthost.NativeSources{
    OMPSockets: map[string]string{
        "workspace:pane": "/absolute/private-runtime/omp-123.sock",
    },
    OpenCodeServers: map[string]agenthost.OpenCodeServer{
        "workspace:other-pane": {
            URL: "http://127.0.0.1:4096", Directory: "/absolute/project",
        },
    },
}
probe, err := agenthost.NewProbeWithNative(host, sources)
assessment, err := probe.Assess(ctx, agents[0])
```

### Literal launch and native fork

`sessionhost.Host` represents the default Herdr session with `Session: ""` and
a named session with its exact name. A target from one host must never be
passed to another. New tabs use Herdr 0.8.2's `layout.apply` command array, so
the configured argv is delivered directly without a shell or user-supplied
environment map:

```go
topology := sessionhost.Host{Runner: runner, Herdr: "/opt/herdr", Session: "work"}
created, err := topology.Launch(ctx, sessionhost.LaunchRequest{
    Origin: sessionhost.Origin{
        SessionName: "work", WorkspaceID: "workspace-id",
        TabID: "tab-id", PaneID: "pane-id",
    },
    Argv: []string{"my-agent", "--model", "provider/model"},
    Directory: "/absolute/project", Placement: sessionhost.NewTab,
})
```

Herdr 0.8.2 cannot assign arbitrary argv to an already-created pane. For
`sessionhost.NewPane`, the kit therefore splits the exact origin pane, requires
Herdr's successful `pane_info`, and waits until `pane process-info` reports the
shell PID as the sole foreground process in the requested CWD. Only `sh`,
`bash`, `dash`, `zsh`, `ksh`, and `mksh` are supported for this mediated path.
It then sends one kit-generated `exec` command.
Every argv element is single-quoted as inert data; profiles never contain raw
shell. An empty or option-like executable, NUL, invalid UTF-8, unsafe terminal controls, oversized input, and an
unsupported shell/readiness state fail before command delivery. Newline is the
only accepted terminal control and remains inside a quoted argument. Do not
recreate this path with `layout.apply`: on 0.8.2 that operation replaces the
existing layout instead of assigning a command to the split pane.

Both placements start in the background. `Focus: true` first rechecks the
exact session/workspace/tab/pane and accepts only a focused response with the
same identities. That receipt correlates the observed focus with the issued
intent; 0.8.2 cannot prove causation against an independently identical focus
operation at the same instant. `Cleanup` verifies the recorded identity and
closes only the task-created pane. Herdr removes an empty task-created tab;
closing the pane instead of the whole tab preserves any unrelated pane added
after creation.

Use `agenthost.NewForker(host).Fork` for native conversation forks. The source
must be the complete `Agent` returned by `Host.List`, including its immutable
native session reference and clean absolute CWD. The adapter owns the native
selector, source identity, and CWD:

| Harness | Adapter-owned suffix/placement |
|---|---|
| Codex 0.154.0 | allowed options, `-C CWD fork THREAD_ID` |
| OMP 18.1.18 | allowed options, `--cwd CWD --fork ABSOLUTE_JSONL` |
| OpenCode 1.18.30 | `CWD`, allowed options, native `session.fork` then `--session CHILD_ID` |

Profiles may contain only the fixed option matrix enforced by
`ForkProfile.Options`. Model, reasoning, sandbox/approval, and documented
display/tool options are accepted where the installed harness supports them.
Subcommands, positional messages, CWD/worktree/add-directory flags, remote
endpoints, profile/session directories, resume/continue/fork selectors, source
IDs, and prompt flags are rejected before creation. Codex `--config` accepts
only `model`, `model_reasoning_effort`, and `service_tier`. Aliases share one
duplicate key, so `-m one --model two` is rejected.

For OpenCode Fork, provide `ForkRequest.OpenCodeServer` for the exact
source-owned loopback listener and source CWD, including an unprompted fork.
The Kit makes one bounded native fork request, validates the distinct returned
child ID, then attaches the TUI to that ID and verifies Herdr reports it.
Do not retry an uncertain native response or infer that a failed TUI launch
removed the native child. `ErrForkOutcomeUncertain` means the caller must retain
the operation for inspection instead of reporting that nothing was created.

For a prompted ordinary Codex fork, set `ObserveCodexPolicy: true`, leave
`CodexPolicy` zero, and provide the prepared prompt. The Kit obtains the
effective model, reasoning effort, approval policy, and sandbox mode from the
guarded child status flow before prompt delivery. Observation with an explicit
policy, without a prompt, or for another harness is rejected before reading the
source or creating topology.

Use `agenthost.NewNativeStarter(host).Start` for an unprompted fresh OMP or
OpenCode conversation whose exact identity is required:

```go
started, err := agenthost.NewNativeStarter(host).Start(ctx, agenthost.NativeStartRequest{
    Origin: origin, Argv: []string{"omp", "--model", "openai/gpt"},
    Directory: "/absolute/project", Placement: sessionhost.NewPane,
    Direction: "right",
})
```

The starter snapshots existing same-harness identities, launches literal argv
in the exact default or named session, and requires two reads of the same new
immutable identity in the created background pane. Normal status or revision
changes between those reads do not invalidate an otherwise stable identity.
Lifecycle selectors such as OMP `--resume`/`--fork` and OpenCode
`--session`/`--fork` are adapter-owned and rejected before observation or
topology. Arbitrary commands remain `sessionhost.Host.Launch` calls and do not
claim an agent identity.

Plain OpenCode does not select or report a conversation at zero turns. For an
unprompted OpenCode New, set `StartOpenCodeServer: true` (or provide a validated
task-owned `OpenCodeServer`). The Kit creates one fresh root session, adds the
exact adapter-owned `--session` selector, binds that ID in Herdr, and stops the
short-lived server while leaving the TUI conversation intact. A failure before
success deletes that created session and cleans only the recorded target.

Herdr/Codex 0.154.0 exposes only a clipped status identity before the first
turn, and the shared App Server cannot authoritatively resolve it at that
point. `NativeStarter` therefore rejects ordinary zero-turn Codex New before
topology. Use `PromptStarter` for ordinary Codex New with a prompt or
`ManagedCodexLauncher` for a managed new conversation; neither path invents or
replaces an identity.

OMP forks require a unique absolute `OMPStatusSocket`. It becomes the sole
fixed process environment delta,
`HERDR_PLUGIN_KIT_OMP_STATUS_SOCKET=<path>`; there is no general environment
map. The consuming plugin still owns the listener and reviewed extension.

Fork completion means Herdr has reported a distinct same-harness session in
the task-created pane, with the requested topology and exact source CWD, and a
second source lookup proves the original identity remains. A temporary nil or
source-valued child identity is pending, not success. Cross-harness, stale,
ambiguous, timed-out, or cancelled states fail and clean only the recorded
created pane. Requested focus occurs only after the distinct identity exists.

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

OMP and OpenCode assessments use only their native structured interfaces.
Herdr locates the exact pane and carries its immutable `agent_session`
reference; Herdr status, readiness, revision, titles, CWD, pane text, and
screen detection are not native status evidence. Generic agent kinds retain
the settled Herdr path, including its bounded detection read. Calling
`NewProbe(host)`, or omitting an exact pane mapping from `NativeSources`, makes
OMP and OpenCode return unstable `Unknown`, `NativeNotManaged`; they are never
guessed from Herdr or terminal content.

| Native evidence for the exact session | Assessment |
|---|---|
| OMP `working` | `Working`, `OMPStatus` |
| OMP `blocked` | `Blocked`, `OMPStatus` |
| OMP `idle` | `Idle`, `OMPStatus` |
| OpenCode pending question and permission | `Blocked`, `OpenCodeQuestionAndPermission` |
| OpenCode pending question only | `Blocked`, `OpenCodeQuestion` |
| OpenCode pending permission only | `Blocked`, `OpenCodePermission` |
| OpenCode `busy` or `retry`, with no pending input | `Working`, `OpenCodeStatus` |
| OpenCode `idle` or absent from the status map, with no pending input | `Idle`, `OpenCodeStatus` |
| Unknown future native status | unstable `Unknown`, `NativeUnsupported` |
| Missing mapping or initial missing native session | unstable `Unknown`, `NativeNotManaged` |
| Native transport or protocol failure | unstable `Unknown`, `NativeUnavailable` |
| Native or Herdr identity/semantic drift | unstable `Unknown`, `Unsettled`, `ErrStaleReport` |

Native OMP and OpenCode probes take two complete observations under one
two-second deadline. They never emit `Done`; the consuming Attention plugin
derives unseen completion from its own prior observation state.

The Attention plugin owns installation of the exact bytes returned by
`OMPStatusExtension()` into its private state directory. Its launcher allocates
a unique Unix socket for every OMP process, injects
`HERDR_PLUGIN_KIT_OMP_STATUS_SOCKET`, and never reuses a stale socket path. It
also owns the launcher-known OpenCode loopback URL and absolute directory
mapping. Plugin Kit does not mutate global OMP/OpenCode configuration, discover
server ports, parse transcripts, or run a status daemon.

The live status contract was verified with Herdr 0.8.2, OMP 18.1.18, and
OpenCode 1.18.30. OMP 18.1.18 `/branch` rewinds were observed to keep both
`sessionManager.getSessionFile()` and Herdr's immutable session path unchanged.
The extension sequence therefore rejects a branch that crosses the two-sample
window, but a fresh stateless assessment after that rewind settles cannot
distinguish the former target. Completed same-path branch invalidation requires
a revised identity contract; it is not inferred from volatile Herdr fields.
OpenCode endpoint shapes must be rechecked against the installed version;
1.18.21 and 1.18.30 were observed during feasibility.

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
