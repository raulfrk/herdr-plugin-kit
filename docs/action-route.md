# Action-route v1 and v2

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

Providers that need only synchronous v1 results use the existing fixed
declaration. Providers that support asynchronous v2 results advertise both
versions through one fixed Herdr action:

```go
action, contracts, err := interop.ActionRouteDeclarations(
    []string{"./plugin", "action", "route"},
)
kit.Actions = append(kit.Actions, action)
kit.Interfaces = append(kit.Interfaces, contracts...)
```

`ActionRouteDeclaration` remains the byte-compatible v1-only helper.

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

Before calling `RouteProvider.Invoke`, the kit refreshes the v1 catalogue for the
same origin and rejects a stale revision, missing action, or unavailable action.
The provider must still recheck any state that can change between catalogue and
effect. It returns exactly one outcome (`completed`, `rejected`, or `failed`)
and whether focus was `preserved` or `changed`.

V2 is explicit opt-in: set `RouteClient.InterfaceVersion` to
`ActionRouteInterfaceVersionV2` and implement `RouteProviderV2`. Dispatch selects
`CatalogV2` and `InvokeV2` before calling provider code. A v2-only asynchronous
action therefore stays absent or unavailable in the v1 catalogue, and a v1
request is rejected before it can enqueue work. Never retry or fall back across
interface versions after an invoke call.

V2 adds two outcomes. `accepted` means durable ownership has transferred to the
provider backend, requires a non-empty printable `operation_id`, and does not
mean the operation completed. Completion is observed later through the
provider's history or notices. `ui_handoff` means the provider opened the exact
workflow UI for required text, directory, confirmation, or setup; it carries no
operation ID. `completed`, `rejected`, and `failed` remain available and also
carry no operation ID. Every outcome reports whether focus was preserved or
changed. The route exchange reports that focus disposition once; it never
restores an older focus after an accepted operation completes.

The short-lived route adapter may connect to a product-owned gRPC API through
the generic service host. Register the product service before `Serve`, then
construct its generated client from the authenticated connection:

```go
productv1.RegisterOperationsServer(server.Registrar(), operations)

service, err := servicehost.Dial(ctx, servicehost.ClientConfig{/* ... */})
backend := productv1.NewOperationsClient(service.Conn())
provider := newRouteProvider(backend)
```

Closing the adapter's client connection does not stop the service or cancel an
operation that was durably accepted. Product RPCs remain responsible for their
own versioned request identity, deduplication, persistence, and result lookup.

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

Calls carry interop envelope version 1, interface `action-route`, an explicit
interface version, a caller correlation, target plugin, deadline, method, and
typed payload. A zero-value `RouteClient` remains on interface version 1.
Catalogue responses repeat the target and add a route revision. Invoke responses
repeat target, revision, and action; only v2 accepts `operation_id`. Unknown, duplicate,
case-variant, trailing, oversized, or mismatched fields fail closed. Deadline
and cancellation terminate and reap the literal provider process group.

Backspace and Escape are router navigation/cancellation keys and must never be
sent as provider invocations. No request is carried through `selected_text`, a
shared file, environment data, a socket, or a daemon.
