# Test strategy

Run deterministic focused tests before broader checks. Every implementation
task must pass changed-scope mutation against its verified base commit:

```sh
make mutation-changed BASE=<verified-base-commit>
```

Run full mutation only after the release candidate is otherwise stable:

```sh
make mutation-full
```

Critical code must reach 100% adjusted efficacy and 100% mutant coverage.
Other code must reach at least 85% adjusted efficacy and 90% mutant coverage.
Equivalent-mutant approvals in `docs/mutation-baseline.md` are exact,
source-bound, and independently reviewed.

New plugins have no historical mutation-debt baseline. A repository may add
`mutation-full-baseline.json` only from a trustworthy exact full run after
independent review. Such a baseline rejects new or worsened critical debt and
does not hide failure of the absolute thresholds.

Generation, validation, and ordinary tests do not run mutation. Mutation runs
are explicit, require a clean committed candidate, use an exclusive lock, and
write non-overwriting scope-specific evidence under `.artifacts/mutation/`.
