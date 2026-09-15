# Action-route v1

Action-route v1 is the reusable provider protocol for a Universal Action Router.
It lets a router discover another plugin's current actions and invoke one exact
action without opening either plugin UI.

The transport is intentionally narrow. `RouteProcessCaller` asks Herdr 0.8.2
for the installed plugin list, requires an enabled provider with a clean absolute
root and `herdr-plugin.toml` beneath that root, parses `plugin-kit.json`, and
cross-validates the fixed global `route` action byte-for-byte. It then executes
only the discovered argv, without a shell or environment override, with cwd set
to the provider root. The request is the sole stdin value; the response is the
sole stdout value. Both streams are bounded to 64 KiB by `command.ExecRunner`.

Providers add the fixed declaration to both manifests. The helper returns the
two `plugin-kit.json` values; the same action metadata and argv go in
`herdr-plugin.toml`:

```go
action, contract, err := interop.ActionRouteDeclaration(
    []string{"./plugin", "action", "route"},
)
kit.Actions = append(kit.Actions, action)
kit.Interfaces = append(kit.Interfaces, contract)
```

The provider's route subcommand serves one request:

```go
err := interop.ServeRoute(
    ctx, os.Stdin, os.Stdout, time.Now(), provider,
)
```

`RouteProvider.Catalog` receives the immutable Herdr origin and returns a
revision plus at most 256 actions. Each action has a stable ID, label,
availability, optional reason, and an advisory participant-relative path of
lowercase letters or digits. A full invocation contains at most eight keys,
including the router's participant key.

Before calling `RouteProvider.Invoke`, the kit refreshes the catalogue for the
same origin and rejects a stale revision, missing action, or unavailable action.
The provider must still recheck any state that can change between catalogue and
effect. It returns exactly one outcome (`completed`, `rejected`, or `failed`)
and whether focus was `preserved` or `changed`.

Routers can let users edit action paths in their UI. Apply the edits to a copied
`RouteCatalog` and call `interop.ValidateRouteCatalog` before saving. This uses
the same key, path, sibling, identity, and capacity rules enforced on provider
responses. `ValidateRouteOrigin` and `ValidateRouteInvocation` are available
for stored origins and full sequences. Suggested paths are advisory: a router
may replace them, but must retain the provider action ID and the catalogue
revision used for invocation.

```go
catalog, err := client.Catalog(ctx, interop.RouteCatalogCall{
    TargetPlugin: "example.provider",
    Origin: origin,
    Deadline: time.Now().Add(2 * time.Second),
})

edited := interop.RouteCatalog{
    RouteRevision: catalog.RouteRevision,
    Actions: append([]interop.RouteAction(nil), catalog.Actions...),
}
edited.Actions[0].SuggestedPath = []string{"n", "a"}
if err := interop.ValidateRouteCatalog(edited); err != nil {
    // Keep the previous saved mapping and show the validation error.
}
```

Calls carry interop envelope version 1, interface `action-route`, interface
version 1, a caller correlation, target plugin, deadline, method, and typed
payload. Catalogue responses repeat the target and add a route revision.
Invoke responses repeat target, revision, and action. Unknown, duplicate,
case-variant, trailing, oversized, or mismatched fields fail closed. Deadline
and cancellation terminate and reap the literal provider process group.

Backspace and Escape are router navigation/cancellation keys and must never be
sent as provider invocations. No request is carried through `selected_text`, a
shared file, environment data, a socket, or a daemon.
