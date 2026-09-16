# Ordinary-to-managed Codex conversion

`agenthost.CodexConverter` converts one ordinary Codex 0.154.0 TUI in its
existing Herdr 0.8.2 pane. It uses the Kit's one installed user-systemd App Server
service and configured `CODEX_HOME`; it never starts a per-pane server, creates a replacement
conversation, injects environment, or moves the session to another pane.

Plain `codex` remains unmanaged even while the service runs. Managed New/Fork
explicitly connect to its private socket. See [service lifecycle](codex-service.md)
for installation, migration, maintenance restart, and uninstall.

## Identity prerequisite

Conversion never guesses a Codex thread from CWD, time, title, or the global
thread list. An already-used ordinary session normally has an exact
`agent_session` from the installed Herdr `SessionStart` hook. If no exact
identity is available, the source process, draft, focus, and topology remain
untouched and conversion reports identity unavailable.

The guarded zero-turn fallback may issue one local `/status` only after two
unchanged observations prove that the exact pane is unfocused, quiescent, and
has an empty composer. Incomplete output fails safely with widen-and-retry.
Widening is guidance only when status output was actually truncated. If Herdr
cannot submit the command, use the ordinary session normally and retry after
its authoritative hook identity appears; conversion must not try alternate
keys or reconstruct an ID from correlated metadata.

## Automatic conversion

Call `Convert` with the exact `Agent` returned for the source pane. Conversion:

1. validates the default or named Herdr session, pane identity, stable
   `agent_session`, clean absolute CWD, persisted turn history, foreground PID
   and literal argv;
2. reads current Codex constraints, then waits without input for two stable
   unfocused, idle/done observations with an empty composer;
3. gracefully stops only that exact Codex process and requires the same pane's
   authoritative shell;
4. captures the effective policy now that the ordinary active writer has
   released the thread;
5. records a short-lived shell lease containing the exact shell process, pane
   revision, visible ANSI buffer, CWD, and unfocused state, then proves it is
   unchanged immediately before each command submission;
6. runs exactly
   `codex resume --remote unix://SHARED --cd CWD ORIGINAL_ID` in that shell;
7. if Codex startup reapplies a default policy, reconciles the captured model,
   reasoning effort, approval policy, and complete sandbox or permission
   profile once through `thread/settings/update`; and
8. verifies the same thread, history, pane, process, CWD, service instance, and exposed
   policy before returning.

The captured effective policy is authoritative. A CLI sandbox flag can resolve
to a different effective sandbox through an active permission profile; the
automatic path preserves the resolved policy rather than assuming argv alone
describes it. For a workspace-write sandbox, preservation includes network
access, every writable root, and both `/tmp` exclusion controls. Read-only
network access is also preserved. Invalid or inapplicable detailed fields fail
closed instead of being silently discarded.

The remote-resume argv never carries model, reasoning, approval, sandbox,
permission, profile, prompt, or environment overrides. Settings reconciliation
is an App Server operation on the already attached same thread and is never
retried when its outcome is unknown.

## Transient custom policy

Before showing the destructive confirmation, call:

```go
disclosure, err := converter.PrepareCustomization(
    ctx,
    source,
    agenthost.ManagedCodexPolicy{
        Model: "gpt-5.6-sol", ReasoningEffort: "high",
        ApprovalPolicy: "on-request", SandboxMode: "read-only",
    },
    30*time.Second,
)
```

Preparation sends no pane input. The UI must display `KnownReplacements`, every
entry in `UnknownBeforeStop`, the desired policy, thread ID, and CWD. Only after
a dedicated second user confirmation may it pass the returned token:

```go
result, err := converter.Convert(ctx, agenthost.CodexConversionRequest{
    Source: source,
    Customization: &agenthost.CodexConversionCustomization{
        Desired: disclosure.Desired,
        ConfirmationToken: disclosure.ConfirmationToken,
    },
})
```

The token is bound to the source Herdr session/workspace/tab/pane, immutable
thread provenance, and desired policy. Conversion recaptures those values and
revalidates constraints before stopping. Exposed settings are checked after the
managed TUI is active. Codex 0.154.0 does not expose pre-stop personality,
collaboration instructions, or instruction contents; the disclosure states
that limitation instead of claiming they were compared.

## Failure and recovery

Pre-stop rejection sends no pane input. If authoritative capture fails after a
successful stop, conversion performs exactly one minimal ordinary
`codex resume --cd CWD ORIGINAL_ID` after rechecking the unchanged shell,
identity, history, and service instance. The returned result has outcome
`ordinary_fallback`, and the error matches both the original cause and
`ErrCodexConversionFallback` so the UI must expose the fallback.

An eligible managed launch may be retried once with a byte-identical request
after renewed shell, service-instance, identity, and history checks. Each retry and the
ordinary fallback obtain a fresh lease for the original shell process. If the
pane becomes focused, its visible buffer or revision changes, the shell or CWD
changes, or the observations do not stabilize, conversion sends no next
command. Two eligible failures otherwise lead to the same single ordinary
fallback. Cancellation, stale or ambiguous identity, cleanup failure, service
drift, user input, or an outcome-unknown settings update blocks further input.
In the last case, do not claim that the pre-update policy was restored: the last
verified policy is unknown even though the same conversation remains
authoritative.

Herdr 0.8.2 does not expose a shell editor's pending-input buffer. Consequently,
text inserted by a concurrent non-Kit actor after the source stops but already
stable before the first shell observation cannot be distinguished generically
from a custom prompt. The normal plugin workflow keeps the source pane
unfocused, and later focus, revision, buffer, process, or CWD changes still fail
closed. Callers that permit direct concurrent `herdr pane send-text` or
`herdr pane run` operations during this narrow interval accept that documented
provenance limitation.
