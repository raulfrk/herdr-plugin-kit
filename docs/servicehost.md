# Conditional service startup

`runtime/servicehost.Lifecycle.Start(ctx)` is an explicit start command. It can
start an owned disabled unit without enabling its future automatic startup.

Use `StartEnabled(ctx, expectedArguments)` for an explicit-open ensure path that
must respect Disable and the intended service binding. It checks the installed
argument vector for exact equality and reads the owned manager's enabled state
while holding the same lifecycle lock used by Disable and installation changes.
It returns `ErrLifecycleOwnership` or `ErrLifecycleDisabled` before issuing a
start command when either precondition fails. Argument order matters; nil and
empty vectors mean exactly zero arguments, not an unrestricted match.

Successful startup still requires matching service identity, ready and accepting
health, and a positive active process. The method never installs, enables,
upgrades, or repairs a service. Status and health remain read-only.

The lock serializes cooperating SDK lifecycle operations. External systemd or
filesystem mutations that bypass it are not made atomic by this API. As with
ordinary Start, a failure after dispatch can leave a service running; inspect
its actual state rather than automatically retrying or claiming no effect.
