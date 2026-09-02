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

## Runtime foundation hypotheses

The Herdr host fixtures below were derived on 2026-09-02 from installed Herdr
0.8.2 command help, generated completion metadata, its bundled API schema, and
read-only `plugin action list`, `plugin log list`, and `session list --json`
output. Tests use injected runners and do not create, invoke, stop, or delete
live Herdr resources.

## HYP-MANIFEST-01 — Valid declarations are stable and unambiguous

- Claim: a manifest accepts only bounded stable plugin/action/capability IDs,
  display metadata, executable arguments, and unique versioned interface
  declarations with an explicit provides/requires direction.
- Fault model: permissive IDs, duplicate declarations, zero interface versions,
  unknown JSON fields, trailing JSON, or unbounded collections are accepted.
- Setup or generator: canonical fixtures plus Rapid-generated identifier parts
  and argument counts around the declared limit.
- Independent oracle: the documented identifier grammar, collection constants,
  and duplicate keys formed independently in the test fixture.
- Falsified when: a valid generated declaration is rejected or any malformed or
  over-limit declaration validates.
- Diagnostics: Rapid seed and minimized ID/count; offending field and index in
  the validation error. No manifest secrets are logged.

## HYP-PROCESS-01 — Subprocess output and exit state are deterministic

- Claim: stdout and stderr never exceed their independent limits, truncation is
  explicit, and a normal nonzero exit retains its exact exit code.
- Fault model: buffers grow without bound, short writes alter the child, output
  truncation is silent, or wait errors erase the process exit status.
- Setup or generator: local `sh` commands emit known byte sequences and exit 7.
- Independent oracle: literal expected byte prefixes, truncation flags, typed
  runner error category, and shell exit code.
- Falsified when: captured bytes exceed/differ from the prefix, truncation is
  unreported, or the result does not contain exit code 7.
- Diagnostics: command fixture, result fields, and captured bounded streams.

## HYP-PROCESS-02 — Cancellation reaps the complete Linux process group

- Claim: cancellation sends TERM, escalates after the configured grace period,
  waits for the leader, and leaves no surviving descendant process.
- Fault model: only the leader is signaled, KILL is not bounded, `Wait` is
  abandoned, or a descendant/goroutine leaks.
- Setup or generator: a shell and child both ignore TERM; a short context and
  grace period force process-group KILL.
- Independent oracle: canceled/killed result flags and Linux signal-0 lookup of
  the emitted child PID after runner return; the race suite checks shared state.
- Falsified when: Run does not return promptly, the child PID remains alive, or
  cancellation is reported as an ordinary exit.
- Diagnostics: bounded child PID, elapsed state, result flags, and race report.

## HYP-ACTION-01 — Action identity and receipts remain caller-stable

- Claim: list/invoke commands preserve `(plugin_id, action_id)`, invocation
  returns Herdr's exact opaque `log_id`, and polling selects that exact receipt.
- Fault model: IDs are normalized or substituted, the newest unrelated log is
  returned, or unknown/oversized JSON is accepted.
- Setup or generator: strict Herdr 0.8.2 response fixtures and scripted command
  results containing stable and mismatched identities.
- Independent oracle: literal requested IDs and opaque receipt ID held by the
  test, independent of host parsing.
- Falsified when: command arguments or returned IDs differ, a mismatched log is
  selected, or strict decoding accepts an unknown field.
- Diagnostics: scripted argv and bounded fixture/result; command output is not
  emitted outside the test failure.

## HYP-ACTION-02 — Receipt polling follows the finite lifecycle

- Claim: the receipt model remains `running` until the first `succeeded` or
  `failed` state, preserves the initial receipt identity, then stops polling.
- Fault model: a terminal state is skipped, polling continues after terminal,
  identity changes, or context cancellation is ignored.
- Setup or generator: Rapid state model with 0–5 running transitions followed
  by a generated succeeded/failed transition; explicit canceled-context case.
- Independent oracle: model transition count, selected terminal state, and the
  initial plugin/action/log tuple.
- Falsified when: returned state/identity differs, runner call count exceeds the
  model trace, or cancellation does not end the wait.
- Diagnostics: Rapid seed, minimized transition trace, argv history, and state.

## HYP-SESSION-01 — Session operations compose only supported commands

- Claim: list/stop/delete map to their documented Herdr 0.8.2 argv, while open
  at a directory executes exactly `--session NAME` followed by a session-routed
  `workspace create --cwd ABSOLUTE_CLEAN_PATH`.
- Fault model: a nonexistent one-command cwd operation is claimed, the second
  command targets the default session, unsafe path/name input reaches the
  runner, or shortcut binding is advertised.
- Setup or generator: injected response fixtures, exact argv snapshots, unsafe
  name/path table, and the exported capability description.
- Independent oracle: installed CLI help/completion evidence and literal
  two-command expected sequence.
- Falsified when: argv/order differs, execution occurs for invalid input, or
  one-command cwd/shortcut support is true.
- Diagnostics: argv history, validated name/path, and bounded JSON error. No
  live session or workspace is created.

## HYP-INTEROP-01 — Versioned envelopes are bounded and cancelable

- Claim: requests explicitly identify target plugin/interface/version/method,
  correlation ID and deadline; request/response JSON payloads are capped at
  1 MiB; caller/handler contracts carry context cancellation and typed errors.
- Fault model: an expired or canceled call proceeds, an oversized/invalid JSON
  payload validates, response payload and error coexist, or an unknown error
  category crosses the boundary.
- Setup or generator: Rapid payload sizes around 1 MiB, fixed expired/canceled
  contexts, invalid JSON, and representative invalid response error shapes.
- Independent oracle: raw payload length, `json.Valid`, context state, and the
  closed category set declared by the contract.
- Falsified when: acceptance differs from the independent boundary predicate,
  cancellation is not visible to a handler, or an invalid response validates.
- Diagnostics: Rapid seed/minimized size, category, correlation ID, and context
  error; payload content is omitted.
