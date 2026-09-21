# Shared Codex App Server service

Kit-based plugins share one Codex 0.154.0 App Server managed by the current
Linux user's systemd manager. The service is independent of plugin and Herdr
lifetimes. It uses the configured Codex home for credentials, settings, and
conversation storage; it does not create one server per conversation.

## Managed versus ordinary

Typing `codex` starts an ordinary, unmanaged TUI, but its App Server transport
is the same systemd-owned canonical Unix socket used by managed operations.
Managed New and Fork explicitly use that socket. Convert stops the exact
guarded ordinary TUI and resumes its existing conversation through that socket,
preserving its identity, history, directory, and effective settings. It does
not replace the conversation or move it to a different pane.

Every Kit-managed New, Fork, and Convert/Resume TUI launch uses the
descriptor-owned absolute Codex executable and passes the literal per-process
override `-c check_for_update_on_startup=false`. The Kit pins and validates the
supported Codex version, so `PATH` drift or a newer-version chooser cannot block
managed readiness or receive automation input. This does not edit the user's
Codex configuration or change ordinary sessions started manually with `codex`.

The service launches pinned Codex with literal `app-server --listen unix://`,
which resolves to `CODEX_HOME/app-server-control/app-server-control.sock`.
Consequently plain and explicitly remote Codex clients share one App Server
process while remaining distinct conversations. A native daemon or legacy Kit
service that predates installation must be retired through the explicit
maintenance procedure below; installation never creates a second canonical
listener or silently disconnects clients.

For prompt delivery, an ordinary target has a zero `CodexService` snapshot.
Its exact pane/process, empty composer, and full Herdr hook identity authorize
one submission without `/status`. When a new zero-turn Codex has no full ID,
the Kit may capture a guarded UUID prefix containing at least 88 bits from the
same pane, submit the requested prompt exactly once, and then query this shared
App Server for CLI conversations. It registers an identity only when exactly
one full UUID matches that prefix, CWD, model, reasoning effort, and expected
parent. Zero or multiple matches fail without retrying the prompt or replacing
the conversation. The temporary prefix is internal and cannot be supplied by a
consumer.

A remote target requires a validated service snapshot matching its explicit
socket as well as those pane guards. An ordinary zero-turn conversation need
not already exist in shared-server storage before its first prompt; cleanup
closes only the created topology, never deletes the conversation through an
unrelated server. Native Fork uses the exact source ID in literal argv and
requires the persisted child to have that exact parent: before prompt delivery
when the full child ID is available, or after the one requested prompt when
only a clipped child prefix is available. The source pane/process must remain
unchanged before delivery and after binding.

## Installation and health

```text
herdr-plugin-kit codex-service install
herdr-plugin-kit codex-service status
herdr-plugin-kit codex-service start
```

Installation accepts `--codex` and `--codex-home` absolute paths, defaulting to
the supported installed executable and existing Codex home. It installs only
the Kit-owned `herdr-plugin-kit-codex.service` unit and installation descriptor.
An unchanged installation is idempotent and does not restart clients.
`install` writes the configuration without starting it; `start` enables boot
startup and starts the service. Complete both steps during initial setup.

If an active native daemon already owns the canonical socket, `install` fails
before changing files. After settling and saving all Codex work, migrate using
the supported Codex stop boundary, then stage and restart the exact Kit-owned
unit:

```text
codex app-server daemon stop
herdr-plugin-kit codex-service install
herdr-plugin-kit codex-service restart --confirm-disconnect
```

For a non-default home, run the first command with that same `CODEX_HOME`.
The staged descriptor retains the verified legacy launch identity until the
confirmed restart succeeds, so a failed migration is diagnosable and retryable.

Installing a changed executable or home stages that configuration without
restarting existing clients. Complete the change with an explicitly confirmed
maintenance restart. Until then, the old running instance is not reported as
ready for the new configuration. Changing the configured home does not copy
or migrate conversations between homes.

The user service manager and lingering are prerequisites for starting at boot
and surviving logout. Installation must not silently change system-wide login
policy or repair unrelated failed units. There is no ad hoc process fallback,
and the Kit never starts a second socket.

`status` is read-only. The next explicit managed operation starts an installed
but stopped service and checks readiness. Plugin monitoring uses this same
inspection path; closing a plugin never stops the service. A requested Codex
home that differs from the installation is rejected, not given another server.

The executable, version, home, unit, socket ownership, and service process
identity are checked. A restart invalidates in-flight Kit observations. systemd
restarts a crashed server; supported Codex clients use native reconnection.
The Kit never replays a prompt, automatically relaunches a terminal, or creates
a replacement conversation after a connection failure.

## Maintenance restart and uninstall

Finish active work and save or clear drafts before either operation. These are
explicit maintenance actions: **managed clients will disconnect**. The service
cannot guarantee that an idle client has no unsaved draft.

```text
herdr-plugin-kit codex-service restart --confirm-disconnect
herdr-plugin-kit codex-service uninstall --confirm-disconnect
```

Uninstall validates installation ownership, disables and gracefully stops the
unit, verifies it stopped, removes the Kit-owned unit, reloads the user manager,
and removes its descriptor last. Conflicting or edited installation files are
not overwritten or removed. Partial failures identify the completed steps and
remaining work so a retry is truthful and safe.

It preserves the Codex executable, credentials, configuration, conversations,
history, lingering, unrelated services and panes, and external SSH settings.
systemd cleans the dedicated runtime directory; the Kit does not recursively
delete a shared directory or Codex home. The private lifecycle lock file remains
so separate Kit processes continue to coordinate safely across uninstall and
reinstallation. An already absent installation is a successful no-op. Managed
actions then report that installation is required; they do not reinstall the
service automatically. A later plain `codex` may start Codex's native daemon
again, which is intentionally ordinary and unmanaged until the service is
reinstalled.

## Optional remote access

Keep the socket private. Use SSH Unix-socket forwarding to the owning user's
canonical socket and explicitly attach the remote Codex TUI to the forwarded
local socket. Do not expose an unauthenticated TCP/WebSocket listener. SSH
credentials, tunnel lifecycle, and local socket permissions remain the user's
responsibility; service uninstall does not alter independently owned tunnels.
