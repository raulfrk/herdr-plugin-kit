# Test strategy

## Required test layers

Every production change starts with an observable example or acceptance test.
The complete suite combines deterministic unit and integration tests, race
tests, Rapid properties and state-machine models, and Gremlins mutation tests.
A minimized property counterexample becomes a permanent regression fixture.

Use stable hypothesis IDs in this form:

## HYP-SUBSYSTEM-01 — Short falsifiable claim

- Claim: one externally observable invariant.
- Fault model: the defect or perturbation that could violate it.
- Setup or generator: fixtures, generated values, and bounded state space.
- Independent oracle: a result not computed by the implementation under test.
- Falsified when: the exact failing observation.
- Diagnostics: seed, minimized trace, screenshots or snapshots, logs, artifact
  paths, and the redaction rules applied.

Stateful Rapid tests must name their abstract model, model state, generated
transitions, and trace oracle. The initial model families are picker/forms,
receipt lifecycle, and diagnostics reservation concurrency.

## Mutation policy

Gremlins v0.6.0 runs with arithmetic-base, conditionals-boundary,
conditionals-negation, increment-decrement, invert-negatives, and
invert-logical enabled. Assignment, bitwise, loop-control, and self-assignment
mutators remain disabled initially. Only generated files and vendor content may
be excluded; handwritten code may not be excluded.

Critical code comprises manifests, subprocess lifecycle, action and session
hosts, interoperability, diagnostics/redaction/reservations, document storage,
scaffold atomicity and generated-project validation. Critical code requires
100% adjusted efficacy and 100% mutant coverage,
with no actionable LIVED, TIMED OUT, or NOT COVERED mutants. Other handwritten
code requires at least 85% adjusted efficacy and 90% mutant coverage, with no
TIMED OUT mutants.

Raw efficacy is KILLED / (KILLED + LIVED). Coverage is
(KILLED + LIVED) / (KILLED + LIVED + NOT COVERED). If KILLED + LIVED is zero,
raw efficacy is N/A. If NOT COVERED is then nonzero, coverage is zero and
remains enforceable. Only when all three counts are zero is coverage N/A and
the gate vacuously satisfied because there is no viable configured mutant.
Adjusted efficacy is KILLED / (KILLED + actionable LIVED) and is 100% when its
denominator is zero. Raw counts are always retained.

Equivalent mutants require an exact independently approved row in
docs/mutation-baseline.md: file, line, column, symbol, mutator, original and
replacement expressions, source SHA-256, linked hypothesis, proof, and
reviewer. A source hash mismatch invalidates the approval. Equivalents change
only the actionable denominator and never rewrite raw Gremlins counts.

## Reproducibility and isolation

Mutation tests run in a clean dedicated Git worktree at a committed candidate.
The initial baseline is the repository's empty root commit, whose tree must be
4b825dc642cb6eb9a060e54bf8d69288fbee4904. Gremlins cannot accept that tree
object directly because its diff mode requires a commit; the root commit is
the supported, semantically equivalent baseline.

Do not pass Gremlins v0.6.0 --test-cpu: it emits a malformed Go argument.
Do not rely on its threshold exit status; mutationgate independently parses
the JSON and applies this policy. The runner verifies the executable's embedded
module version is exactly v0.6.0. Mutation workers are capped at half the
available logical CPUs. Mutation and live Herdr/device tests share the same
exclusive lock and must never overlap. Separate mutation jobs may run in
parallel only for disjoint packages. Because Gremlins diff mode skips files
introduced directly after an empty root, that bootstrap comparison is rejected
with an instruction to run the full suite.

After each run, before/after SHA-256 manifests for every tracked file must
match and the worktree must contain no tracked or untracked drift. The Gremlins
report, gate report, gate stderr, source manifests, and their SHA-256 manifest
are copied to the ignored .artifacts/mutation/<candidate-commit>/ directory.
If the gate fails before producing its normal report, the runner writes a
machine-readable failed result pointing to the preserved stderr. Normal gate
reports record tool versions, candidate, scope/base, configuration hashes,
thresholds, survivors, and approved-equivalent dispositions.

Run targeted mutation tests for every bead with
make mutation-changed BASE=<commit>, then downstream tests after merge. Run
make mutation-full before the final acceptance bead and again after its final
code commit, before live proof and release. Any later production change
requires another full run.

## Toolchain feasibility record

On 2026-09-02 the disposable gate ran with Go 1.27.0, Rapid 1.3.0, and
Gremlins 0.6.0. Rapid minimized n >= 10 to n=10, wrote a fail file, and replayed
it with zero generated cases. Gremlins produced parseable JSON for killed and
deliberately surviving mutants and restored source bytes exactly. The run also
reproduced the --test-cpu and native-threshold defects above, and proved the
empty-tree ancestor-commit workaround.
