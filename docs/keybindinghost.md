# Live Herdr keybindings

`runtime/keybindinghost` manages only Plugin Kit-owned Herdr command
keybindings. It does not register runtime actions: every binding must target a
fixed action already declared by the same plugin owner.

Use `Host.Read` to obtain the current owner-specific bindings and exact
SHA-256 content revision, then submit that revision to `Host.Apply`:

```go
host := keybindinghost.Host{
    Runner: command.ExecRunner{},
    Session: "work", // empty means the default Herdr session
}
snapshot, err := host.Read(ctx, "attention.plugin")
if err != nil {
    return err
}
result, err := host.Apply(ctx, keybindinghost.Request{
    Owner:    "attention.plugin",
    Expected: snapshot.Revision,
    Bindings: []keybindinghost.Binding{{
        Key:         "prefix+a",
        Action:      "attention.plugin.open",
        Description: "Open Attention",
    }},
})
```

`Apply` reports `Result.Activated` only after the exact addressed Herdr session
accepts the reload. If the requested owner bindings are already durable and a
fresh activation receipt is required, call `Activate` with the same snapshot
revision and binding set:

```go
result, err = host.Activate(ctx, keybindinghost.Request{
    Owner: "attention.plugin", Expected: snapshot.Revision,
    Bindings: snapshot.Bindings,
})
```

`Activate` revalidates the exact revision, ownership, requested bindings, and
unmanaged collisions; checks the unchanged config; and reloads only the
addressed default or named session. It never writes the config. A binding
mismatch is `ErrDrift`, and only an accepted reload sets `Result.Activated`.

The host resolves `HERDR_CONFIG_PATH` when present, otherwise
`$XDG_CONFIG_HOME/herdr/config.toml` (or the platform user-config default).
An explicit `ConfigPath` is useful for isolated consumers and tests. Paths
must be clean and absolute. The config file must already exist as a regular
file.

## Transaction contract

- A revision identifies every byte in the config. Drift fails before
  replacement with `ErrDrift`.
- The caller replaces its complete binding set. An empty set removes it.
- Owner and action IDs are stable IDs, and each action must start with the
  exact owner plus `.`.
- Requested keys cannot collide with unmanaged commands or another Kit owner.
  Existing unmanaged bytes and other owner groups are retained byte-for-byte.
- A private, bounded same-directory candidate is checked by the pinned Herdr
  CLI before the real file changes. Any non-clean response is `ErrCheck`.
- Publication uses the existing `documentstore` atomic checked write, which
  preserves existing mode and ownership and uses its documented cooperative
  sidecar lock.
- A private owner-controlled `.herdr-plugin-kit-keybindings.lock` in the config
  directory serializes Kit keybinding transactions across processes. The lock
  remains held from the revision read through candidate validation,
  publication, addressed-session reload, and any rollback/recovery reload, so
  no later Kit transaction can make an earlier reload effective for the wrong
  revision. Waiting is cancellation-aware.
- Reload clears inherited Herdr routing variables and addresses exactly the
  configured default or named session. It accepts only the Herdr 0.8.2
  `config_reload/applied` receipt.
- A reload or post-rename write failure triggers a five-second
  cancellation-independent attempt to restore the exact prior bytes and reload
  the prior keymap. `Result.RolledBack` and
  `Result.EffectiveRestored` expose what succeeded; `ErrRollback` means
  recovery was incomplete.
- Plugin-local page and action keys never use this API and require no Herdr
  reload.

`Error.Kind` and `errors.Is` distinguish invalid input, ownership damage,
collision, drift, check, write, reload, rollback, and caller cancellation.
The API adds no Attention-specific policy or product orchestration.
