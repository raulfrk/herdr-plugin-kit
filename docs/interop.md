# Inter-plugin actions on Herdr 0.8.2

Herdr 0.8.2 supports exact plugin-action discovery, invocation, terminal receipt
polling, and stdout capture. It does not provide a caller-controlled argument,
stdin, environment, or correlation channel to the action process. The plugin kit
therefore supports only statically declared, no-input inter-plugin actions over
Herdr's action host.

`interop.ActionTarget` binds a known Herdr action ID to an interface, interface
version, and method. `ActionClient.Invoke` confirms that exact action exists,
invokes it, waits for its exact opaque receipt ID, and strictly decodes one
bounded `ActionResponse` from successful stdout. The receipt log ID is the call
correlation identity. Response metadata must match the binding exactly.

Plugins must not use shared files, sockets, environment variables, selected
text, or dynamically encoded action IDs to smuggle request payloads. Those are
not part of the supported contract. `SupportedActionCapabilities` reports
`RequestEnvelope` and `CallerCorrelation` as false so callers can fail before
presenting unsupported functionality.

The response shape is:

```json
{
  "version": 1,
  "interface": "health",
  "interface_version": 1,
  "method": "check",
  "payload": {"ok": true}
}
```

Herdr 0.8.2 preserves action stdout through exactly 64 KiB and truncates larger
output. The complete action response envelope is therefore capped at 64 KiB.
The separate transport-neutral request/response contract retains its 1 MiB
payload and 16 KiB metadata allowance. Failed actions, missing stdout,
malformed or trailing JSON, identity changes, oversized envelopes, and invalid
typed errors fail closed.
