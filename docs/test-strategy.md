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

Critical code comprises manifests, subprocess lifecycle, action, session, and
agent hosts (including the Codex-aware status assessment), interoperability,
diagnostics/redaction/reservations, document storage, scaffold atomicity and
generated-project validation. Critical code requires
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
available logical CPUs. The mutation-process timeout coefficient is 3, which
leaves normal tests several complete baseline durations while bounding an
unexpected infinite mutant. Mutation and live Herdr/device tests share
the same exclusive lock and must never overlap. Separate mutation jobs may run in
parallel only for disjoint packages. Because Gremlins diff mode skips files
introduced directly after an empty root, that bootstrap comparison is rejected
with an instruction to run the full suite.

On 2026-09-03 a disposable calibration with a deliberately infinite loop
reported the mutant as `TIMED OUT` and completed in 6.3 seconds at coefficient
3, while all finite mutants completed. With DocumentStore's observed 31-second
baseline this bounds a stuck worker at roughly 93 seconds. `TIMED OUT` remains
a hard gate failure at every tier.

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

Wall-clock SLO tests run through `make test-timing` in an isolated package
process. `make test` runs that lane after the functional suite. Coverage and
race instrumentation skip only the timing assertions while continuing to run
all shell correctness and concurrency tests.

## Scaffold hypotheses

## HYP-SCAFFOLD-01 — Generation publishes one complete tree without overwrite

- Claim: concurrent generators targeting the same absent path publish exactly
  one fully validated plugin, never replace an existing path, and leave no
  sibling staging directories.
- Fault model: check-then-rename races, partial publication, failed cleanup,
  destination replacement, or a generated tree that differs from validation.
- Setup or generator: eight concurrent publishers, pre-existing destination
  fixtures, invalid options, and the exact generated source-and-test tree.
- Independent oracle: Linux `RENAME_NOREPLACE`, a pre-existing marker byte
  comparison, exact relative-file enumeration, and a fresh `Validate` pass.
- Falsified when: zero or multiple publishers succeed, marker bytes change,
  staging remains, or the winning tree fails validation.
- Diagnostics: contender errors, resulting relative paths, validation stage,
  and the temporary test root only; generated config values are not logged.

## HYP-SCAFFOLD-02 — Both manifests describe one runnable private plugin

- Claim: generated Herdr TOML and `plugin-kit.json` retain identical identity
  and static actions, use argv-only build/action/pane commands, pin kit v0.1.0
  without `replace`, expose responsive main and Debug UI surfaces, and keep
  config/state under Herdr-provided directories.
- Fault model: schema drift, shell command insertion, non-overlay panes,
  missing runtime environment use, malformed Go, wrong license, symlinked or
  oversized contract files, or catalogue assets leaking into output.
- Setup or generator: structural mutations of each contract field, escaped
  metadata, required-file substitutions, and a generated-module build in a
  test-only Go workspace.
- Independent oracle: strict TOML decoding, `manifest.Parse`, exact action and
  pane tables, Go parser/compiler, canonical Apache-2.0 bytes, and exact file
  enumeration.
- Falsified when: any drift validates, the pristine generated `go.mod` changes,
  compilation fails, a required environment/subcommand is absent, or any
  catalogue content is generated.
- Diagnostics: contextual filename/contract errors and compiler output; no
  runtime configuration, state contents, query text, or terminal content.

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

## HYP-AGENT-STATUS-01 — Agent status requires a coherent Codex-aware sample

- Claim: agent enumeration and focus preserve exact current/named-session
  identity, while status is reported only after two coherent observations 400
  ms apart; Codex terminal semantics take precedence over Herdr's host status.
- Fault model: pane-ID reuse, cross-session focus, stale host state, terminal
  queue/question/composer misclassification, or raw terminal text escaping.
- Setup or generator: strict Herdr 0.8.2 fixtures, current and named sessions,
  two-sample identity/state transitions, and bounded 60-line terminal tails.
- Independent oracle: exact identity tuples, explicit semantic status fixtures,
  command call order, and exported-value inspection for terminal sentinels.
- Falsified when: an incoherent sample is stable, the wrong pane/session is
  focused, a semantic state is misclassified, bounds are exceeded, or raw text
  reaches a public result or diagnostic.
- Diagnostics: opaque identity, status/reason, sample outcome, and command
  stage only; terminal contents and paths are never recorded.

## HYP-RESP-01 — Every terminal size resolves to one bounded layout

- Claim: every reported size resolves deterministically to recovery, compact,
  standard, or wide; render geometry is positive and never exceeds 500x200.
- Fault model: a threshold gap/overlap, zero-sized frame, unbounded allocation,
  or nondeterministic class selection.
- Setup or generator: exhaustive sizes from 1x1 through 500x200 plus negative,
  zero, and oversized dimensions.
- Independent oracle: literal class thresholds and component-wise clamp computed
  in the test rather than by the resolver.
- Falsified when: class or geometry differs from the oracle, or repeated calls
  differ.
- Diagnostics: reported size, expected/actual class, render size, and projection
  flag.

## HYP-RESP-02 — Resize bursts converge quickly to the latest stable size

- Claim: every resize is applied immediately, stale settle timers are ignored,
  and the latest generation settles once within 100–250 ms without later stale
  rendering.
- Fault model: debounce hides an intermediate resize, an old timer wins, event
  backlog delays recovery, or the final viewport is misidentified.
- Setup or generator: deterministic generation traces, 200 seeded PTY bursts,
  short phone-like rows, and a final 500x200 resize.
- Independent oracle: monotonically assigned generations, the final emitted
  terminal size, diagnostic outcomes, and elapsed monotonic time.
- Falsified when: a received size is skipped, a stale generation is applied,
  final settling leaves the 100–250 ms window, or the final frame differs from
  the final reported geometry.
- Diagnostics: seed, generation trace, reported/render geometry, outcomes,
  p95 update latency, final-settle latency, and bounded terminal bytes.

## HYP-INPUT-01 — Text reaches the surface but never semantic diagnostics

- Claim: Unicode keyboard and paste text is delivered unchanged to the surface,
  while diagnostics retain only byte and grapheme counts.
- Fault model: input is normalized/truncated before use, or query/path/secret
  content leaks into an event, visual state, or report.
- Setup or generator: fixed multilingual canaries and generated valid UTF-8 text
  delivered as key-run and paste messages.
- Independent oracle: byte equality at the surface, independent grapheme counts,
  and raw persisted-byte absence of every canary.
- Falsified when: surface text differs, counts differ, or any canary appears in
  persisted/exported diagnostics.
- Diagnostics: input length and grapheme count only; failing content is never
  printed or persisted.

## HYP-ASYNC-01 — Latest request generation exclusively owns visible state

- Claim: starting work for an existing request key cancels the prior context,
  and only the latest generation can update the surface.
- Fault model: an old result overwrites new state, cancellation targets the new
  request, or correlations cross between generations.
- Setup or generator: controlled work functions complete in reverse order with
  distinct opaque correlations and cancellation observation channels.
- Independent oracle: generated order, latest generation number, cancellation
  channel, and the surface's accepted result history.
- Falsified when: the old context remains live, a stale result reaches Update,
  or the accepted result/correlation is not the latest.
- Diagnostics: request key, generations, opaque correlations, outcomes, and
  accepted-result count; result values are omitted.

## HYP-DIAG-SEMANTIC-01 — Plugin diagnostics accept only semantic state

- Claim: the public recording boundary accepts only validated static IDs,
  allowlisted outcomes, nonnegative measurements, geometry, and a text-free
  visual projection.
- Fault model: a free-form field becomes recordable, malformed IDs or negative
  values persist, or caller-owned state aliases retained recorder data.
- Setup or generator: compile-time `SemanticSink` conformance, malformed ID and
  measurement tables, a complete visual projection, and raw log inspection.
- Independent oracle: the declared public field set, identifier grammar,
  literal semantic JSON keys, and absence of user-content canaries.
- Falsified when: invalid semantic input is accepted, unexpected keys/content
  persist, or caller mutation changes a retained event.
- Diagnostics: rejected field category, semantic IDs, measurements, and raw-key
  differences; no user content is emitted.

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

## HYP-INTEROP-02 — Herdr actions provide no-input, receipt-correlated calls

- Claim: on Herdr 0.8.2 an exact static action can be discovered, invoked, and
  correlated by its opaque receipt ID; successful stdout carries one strict,
  bounded response envelope whose interface, version, and method match the
  caller's binding. Caller-selected request envelopes and correlation IDs are
  explicitly unsupported.
- Fault model: an unrelated or duplicate action is selected, receipt identities
  cross under concurrency, malformed/trailing/oversized stdout is accepted, a
  response changes the bound interface identity, cancellation is ignored, or a
  mailbox/socket/environment channel is silently introduced.
- Setup or generator: injected ActionHost fixtures plus a disposable live Herdr
  plugin with one static action. The negative live fixture invokes concurrently
  with distinct stdin values; the positive fixture emits fixed versioned JSON.
- Independent oracle: exact action tuple, distinct Herdr log IDs, byte-exact
  captured stdin/stdout, strict standard-library JSON decoding, and the
  published capability booleans.
- Falsified when: caller bytes reach the action, receipts are confused, response
  identity or bounds are not enforced, or cancellation fails to end waiting.
- Diagnostics: plugin/action/interface/method IDs, opaque receipt ID, response
  length, and typed category only; payload content is omitted.

## HYP-DOCUMENTSTORE-01 — Checked replacement is confined and durable

- Claim: a write beneath an opened root publishes only when the current
  SHA-256 revision equals the caller's expectation, never follows a symlink or
  accepts a special file, preserves replacement metadata, and completes file
  fsync, rename, then directory fsync while holding the cooperative lock.
- Fault model: path traversal or symlink escape, lost-update overwrite,
  partial publication, metadata drift, unlocked rename, or reordered/omitted
  durability operations.
- Setup or generator: canonical and hostile root-relative paths, stale and
  zero revisions, regular/symlink/FIFO targets, injected syscall failures, and
  a Rapid state model of 1–30 checked writes.
- Independent oracle: paths and bytes read directly through the filesystem,
  independently computed SHA-256 revisions, pre-write stat metadata, a literal
  fsync/rename trace, and a competing nonblocking lock acquisition at rename.
- Falsified when: bytes escape the root, an invalid target is accepted, a stale
  write publishes, returned bytes/revision/metadata differ, temporary files
  survive failure, or durability and lock ordering differs from the contract.
- Diagnostics: minimized operation trace, path category, revision/result,
  syscall stage, metadata tuple, and remaining temporary filenames; document
  contents are omitted.

## HYP-DOCUMENTSTORE-02 — Cooperative access is coherent and cancelable

- Claim: participating readers observe one complete revision, competing
  writers serialize and reject a stale revision, cancellation bounds lock
  waiting, and `Close` excludes new work while waiting for active work.
- Fault model: split reads across a rename, writers bypass the sidecar lock,
  lock polling ignores context, or the root descriptor closes during an active
  operation.
- Setup or generator: two stores on one root, concurrent fixed-size alternating
  writes and reads under the race detector, concurrent first-lock publication
  under a restrictive umask, a deliberately held lock, and a write paused
  immediately before rename while `Close` races it.
- Independent oracle: membership in the two complete byte fixtures, revision
  recomputation, channel-observed operation ordering, bounded context deadline,
  and `ErrClosed` after close.
- Falsified when: a mixed document is observed, both stale writers publish,
  cancellation exceeds its bound, close returns before active publication, or
  post-close work reaches the filesystem.
- Diagnostics: operation ordering, elapsed cancellation time, revision tuple,
  and race report; document contents are represented only by fixture identity.

## Visual/config hypothesis register

### Theme configuration reference

Stable built-in IDs are `catppuccin`, `terminal`, `tokyo-night`, `dracula`,
`nord`, `gruvbox`, `one-dark`, `solarized`, `kanagawa`, `rose-pine`, `vesper`,
and `catppuccin-latte`. The Latte theme is the default light counterpart when
auto-switching. Custom tokens are `background`, `panel_bg`, `sidebar_bg`,
`active_row_bg`, `selection_bg`, `surface`, `overlay`, `border`, `text`,
`muted`, `accent`, `red`, `green`, `yellow`, `blue`, `magenta`, and `cyan`.
Colours accept `#rgb`, `#rrggbb`, ANSI names, `rgb(r,g,b)`, or `reset`.

```toml
[theme]
auto_switch = true
dark_name = "catppuccin"
light_name = "catppuccin-latte"

[theme.custom]
accent = "#89b4fa"
panel_bg = "reset"
```

## HYP-UI-01 — Frame placement preserves terminal-cell geometry

- Claim: every placement leaves a fixed-size frame whose wide graphemes have
  exactly one leading cell and no orphan continuation cell; combining clusters
  occupy one cell and edge clipping never emits half a wide cluster.
- Fault model: byte/rune indexing, incorrect Unicode width, partial clipping,
  or overwriting only one half of an existing wide grapheme.
- Setup or generator: deterministic CJK/combining/emoji edge fixtures plus
  Rapid-generated frame widths, positions, text choices, and placement traces.
- Independent oracle: `uniseg.StringWidth` for rendered row width and a direct
  scan from each continuation cell to a leading cell whose declared span
  contains it.
- Falsified when: a row width differs from `Frame.Width`, a clipped half appears,
  or any continuation lacks a covering lead.
- Diagnostics: Rapid seed/minimized operation trace and the frame cells; frame
  text is passed through the HYP-SNAPSHOT-01 policy if persisted.

## HYP-UI-02 — ANSI and PNG are deterministic views of one frame

- Claim: repeated renders are byte-identical; ANSI rows retain frame cell
  dimensions and end with reset style state, while PNG dimensions are exactly
  `width*8` by `height*16` and cell backgrounds match the same styles. A frame
  with either dimension zero emits no ANSI or PNG bytes.
- Fault model: map/order nondeterminism, renderer-specific layout, split wide
  graphemes, omitted style reset, or hidden terminal cursor/screen mutation.
- Setup or generator: styled fixed fixtures and Rapid-generated bounded frame
  dimensions, positions, and Unicode graphemes.
- Independent oracle: ANSI SGR stripping plus `uniseg.StringWidth`, standard
  library PNG decoding, direct pixel sampling, and byte equality on rerender.
- Falsified when: bytes differ, dimensions differ, sampled style differs, a row
  width differs, a zero-geometry frame emits bytes, or positive-geometry ANSI
  contains non-SGR controls or lacks its final SGR reset.
- Diagnostics: generated dimensions/text trace, escaped ANSI, PNG bytes or
  decoded bounds; diagnostics use HYP-SNAPSHOT-01 redaction.

## HYP-CONFIG-01 — Strict loading preserves defaults and rejects invalid input

- Claim: omitted TOML values retain caller defaults, while unknown fields,
  unknown theme IDs/tokens, and malformed colours return contextual errors.
- Fault model: zeroing before decode, permissive schema drift, or validation
  bypass after decoding/customisation.
- Setup or generator: minimal valid documents and one-fault fixtures for an
  unknown field, theme, token, and colour.
- Independent oracle: explicit expected defaults and the catalogue copied from
  the current `herdr --default-config` built-in list.
- Falsified when: a default is lost, invalid input succeeds, or an error omits
  the invalid field/theme context.
- Diagnostics: input fixture and error with values redacted by HYP-SNAPSHOT-01.

## HYP-CONFIG-02 — Live reload detects replacement and stops cleanly

- Claim: a content-changing atomic replacement with identical byte size and
  preserved timestamps is detected in under one second and reaches one
  callback in under two seconds; callback-initiated and concurrent `Stop` calls
  complete. Callbacks may overlap and already-started callbacks may outlive
  `Done`; once `Done` closes, no new callback is dispatched.
- Fault model: metadata-only watching, missed rename, debounce starvation,
  unsynchronised callback/cancellation, or duplicate delivery.
- Setup or generator: temporary TOML files, content hash polling at 10ms,
  30ms debounce, preserved `mtime`, atomic rename, invalid reload, and eight
  concurrent stop callers plus a callback-initiated stop.
- Independent oracle: callback value/error, monotonic elapsed thresholds,
  timestamped `Update`, and a post-stop callback channel observation.
- Falsified when: detection is at least one second, propagation is at least two
  seconds, the replacement/error is absent or duplicated, a stop hangs, or a
  callback is dispatched after `Done` closes.
- Diagnostics: start/detected/applied times and redacted callback error/value.

## HYP-SNAPSHOT-01 — Sensitive diagnostic values are explicitly redacted

- Claim: values for the published sensitive-key regexes and bearer credentials
  are replaced across assignment, JSON-like, environment, and CLI forms;
  redaction is deterministic/idempotent and leaves non-sensitive assignments.
- Fault model: secret-form blind spots, substring key matching, replacement
  instability, or collateral redaction.
- Setup or generator: representative multi-format fixture and Rapid-generated
  token lengths used to derive bounded values without recording secret text.
- Independent oracle: exact `<redacted>` assignment, absence of fixed secrets,
  preservation of `username=raul`, and equality after a second redaction.
- Falsified when: a sensitive value remains, non-sensitive text changes, or a
  second call changes output.
- Diagnostics: only already-redacted output, sensitive key pattern index, and
  Rapid seed/minimized value shape (never the generated secret itself).

## HYP-CAT-01 — The final review catalogue covers every committed dimension

- Claim: the catalogue matrix is the deterministic Cartesian product of the
  selected Bento Command design, every built-in Herdr theme, eight supported
  boundary/device viewports, and every committed UI scenario. Each planned
  plugin surface and each required state appears in at least one scenario.
- Fault model: a new theme or scenario silently omitted from review, unstable
  ordering, a keyboard-open fixture losing its provenance, or a future consumer
  represented only by a happy-path screen.
- Setup or generator: the checked-in matrix fixture plus the live built-in theme
  catalogue and published viewport, scenario, state, and surface IDs.
- Independent oracle: explicit required surface/state sets, keyboard-open flags,
  fixed matrix endpoints, and the arithmetic product of dimensions.
- Falsified when: a required dimension is absent, order changes unexpectedly,
  viewport provenance changes, or the product differs from the exported entries.
- Diagnostics: the missing or unexpected ID, matrix endpoints, and dimension
  counts; no generated image bytes are needed to diagnose matrix membership.

## HYP-CAT-02 — Responsive studies remain bounded and reproducible

- Claim: every scenario renders at the exact requested cell dimensions from
  `40x10` through `500x200` and retains styled frame edges; every theme,
  viewport, and scenario preserves the reviewed cell text, style, Unicode width,
  continuation, and dimensions in a stable semantic fingerprint.
- Fault model: desktop-only coordinates, content drawn outside a short viewport,
  nondeterministic rendering, or a theme ignored by the renderer.
- Setup or generator: all viewport/scenario combinations under one theme,
  followed by every built-in theme and the checked-in 96-group fingerprint.
- Independent oracle: frame dimensions and corner cells plus a canonical
  serialization of each cell's content, style, width, and continuation state.
- Falsified when: rendering fails, dimensions drift, an edge is lost, repeated
  PNG bytes differ, or a supported theme does not contribute its own palette.
- Diagnostics: the design/theme/viewport/scenario path and observed frame or PNG
  dimensions, suitable for opening the exact failed image in the review index.

## HYP-CAT-03 — Catalogue exports are safe, complete, and inspectable

- Claim: exporting a selected review slice writes each matrix image once, adds
  its expected contact sheets and JSON/HTML indexes, and produces identical file
  bytes in independent output directories without overwriting existing content.
- Fault model: partial or reordered output, path escape, stale-file overwrite,
  invalid PNGs, or an index that cannot identify the generated studies.
- Setup or generator: a filtered multi-scenario selection exported twice, an
  unsafe selector, and a non-empty destination.
- Independent oracle: the selected matrix paths, standard-library PNG decoding,
  recursive byte comparison, and explicit index/file existence checks.
- Falsified when: any selected artifact is absent or undecodable, exports differ,
  an unsafe selector succeeds, or a non-empty destination is modified.
- Diagnostics: relative artifact path and byte comparison; output remains in a
  caller-selected directory and is never part of a generated plugin.

## HYP-CAT-04 — Live review reflects the delivered terminal

- Claim: the interactive catalogue runs through the production shell, renders
  the selected Bento Command treatment for each plugin surface, flow state, and
  built-in theme, and remains operable at `40x10`. Its unclipped compact footer
  opens a complete `40x10` navigation guide. Its HUD shows the reported and
  bounded render geometry, resize generation, and final settled state.
- Fault model: a static-only or desktop-only review hides short-height failures;
  a resize HUD reports a guessed keyboard size; a design dimension is present
  in metadata but cannot be reached; or text editing splits a grapheme.
- Setup or generator: direct shell events over every live catalogue axis,
  compact and representative render contexts, Unicode/IME-style committed text,
  checked-in viewport fixtures, and the labelled iPhone landscape → portrait
  keyboard-open → keyboard-closed → keyboard-open review sequence.
- Independent oracle: exact public axis IDs, frame dimensions, six populated
  compact task rows, visible non-colour selection markers, the complete help
  text at minimum size, grapheme-aware deletion, HUD text derived from
  `responsive.Layout`, and settled semantic geometry events from the device.
- Falsified when: a choice or state is unreachable, a compact task row budget is
  lost, the final frame differs from the latest delivered geometry, query text
  corrupts, or the HUD substitutes a fixture for observed terminal size.
- Diagnostics: semantic axis/state IDs plus reported/rendered dimensions and
  resize generation; sample query text is never included.

## HYP-DIAG-01 — Persisted diagnostics are private, valid, and recoverable

- Claim: each accepted event receives a monotonic versioned identity, every
  textual and structured field is redacted before persistence, storage uses
  owner-only modes, and reopening repairs corrupt, truncated, delimiter-less,
  or newly over-budget logs while retaining the newest valid tail.
- Fault model: a secret escapes through a nested key or snapshot reference,
  partial writes poison later appends, recovery preserves stale records instead
  of recent context, or permissive file modes expose diagnostics.
- Setup or generator: nested multi-format secrets, corrupt/truncated JSONL,
  a valid final record without a newline, and a prior log larger than a reduced
  byte budget.
- Independent oracle: raw persisted-byte secret absence, decoded event schema
  and sequences, filesystem modes, newest-tail identity, and successful record
  immediately after recovery.
- Falsified when: secret input is present on disk, valid records stop decoding,
  sequence continuity is lost, stale head records survive instead of the newest
  tail, or directory/log permissions differ from `0700`/`0600`.
- Diagnostics: redacted contextual error, corrupt-record count, storage health,
  and event sequence; rejected cyclic/deep input itself is never persisted.

## HYP-DIAG-02 — Retention remains bounded under concurrent recording

- Claim: participants sharing one recorder receive unique sequences and observe
  settled count, byte, and age limits after every operation without races,
  deadlocks, or an on-disk quota overshoot.
- Fault model: check-then-write quota races, incorrect byte accounting during
  compaction rollback, expired records surviving cleanup, or concurrent close
  and health inspection corrupting state.
- Setup or generator: deterministic count/byte/age pressure, twelve concurrent
  writers, and Rapid-generated event sizes, operation counts, and quota values.
- Independent oracle: decoded unique sequences, `Health` counters, filesystem
  size, age cutoff, and the configured limits after each generated operation.
- Falsified when: any accepted state exceeds a limit, sequences duplicate, an
  expired event remains, a writer hangs/errors unexpectedly, or the race
  detector reports shared-state access.
- Diagnostics: Rapid seed/minimized operation, configured quotas, health, file
  size, and retained sequence list. Cross-process recorders are not participants
  in this contract.

## HYP-DIAG-03 — Debug reports are deterministic, redacted, and truly bounded

- Claim: unchanged recorder state exports byte-identical versioned JSON no
  larger than the caller's valid limit; oldest events, snapshots, metadata, and
  finally an oversized last event can be omitted with explicit reasons while
  preserving the newest event whenever it fits.
- Fault model: wall-clock nondeterminism, report-only metadata leaks, silent
  truncation, or a single retained event making the configured bound impossible.
- Setup or generator: fixed time, secret-bearing metadata/snapshots, multiple
  large events, and one event larger than the minimum report limit.
- Independent oracle: repeated-byte equality, JSON decoding, secret absence,
  output length, truncation reasons, and newest retained sequence.
- Falsified when: equivalent exports differ, output exceeds its limit, a secret
  remains, omissions are unnamed, or export fails when an event-free envelope
  fits.
- Diagnostics: report length, reasons, retained sequences, and redacted health;
  report contents are safe to attach to a debugging session.

## HYP-INTERACTION-01 — Picker state converges on the newest visible request

- Claim: opaque provider cursors can page without a kit-imposed total limit,
  keyed selection remains stable across reordered results, and only the newest
  request and resize generations can change visible state.
- Fault model: a stale completion replaces current results, an older resize
  restores obsolete geometry, paging silently stops at a fixed count, or a
  reordered page selects a different action.
- Setup or generator: 4,097 opaque pages, reordered keyed results, out-of-order
  result and resize generations, Unicode committed text, and every responsive
  boundary including Recovery and projected oversize.
- Independent oracle: provider cursor identity, selected item key, current
  request/resize generation, exact frame geometry, and visible selected row.
- Falsified when: any stale generation commits, navigation loses the selected
  key, a valid next cursor is refused, or Recovery changes interaction state.
- Diagnostics: only static screen/state/selection IDs, counts, generations, and
  geometry; query text and provider item content remain private.

## HYP-FORM-01 — Apply and rollback use immutable submitted values

- Claim: validation runs on a cloned value set, apply receives the exact
  submitted snapshot, later draft edits remain visible, stale completions are
  ignored, and every nonempty failure or success token is rolled back through
  the matching operation without entering diagnostics.
- Fault model: caller mutation aliases form state, an apply completion commits
  a newer draft that was not submitted, an error token is abandoned, or an old
  result overwrites the current operation.
- Setup or generator: invalid values, grapheme edits, mutating appliers,
  partial-failure tokens, edits during apply, stale generations, explicit
  rollback, Recovery, and secret fields.
- Independent oracle: applier-observed values and tokens, form draft and state,
  rollback calls, visible non-secret cues, and semantic diagnostics.
- Falsified when: submitted/applied values differ, a stale result changes state,
  a partial failure skips rollback, a newer draft is lost, or a secret appears
  in a frame or diagnostic projection.
- Diagnostics: static field/state IDs, operation generation, geometry, pending,
  and error presence only.

## HYP-DEBUG-UI-01 — Debug views expose only trusted semantic evidence

- Claim: the Debug UI projects only events marked by `RecordSemantic`, exports
  only the allowlisted projection, and displays or exports a PNG only when the
  persistent PreviewStore registration, event/state association, confined
  owner-only regular file, visual digest, and PNG hash all revalidate.
- Fault model: ID-shaped raw content passes as semantic data, a forged snapshot
  reaches the gallery, a path or free-form error enters export, a symlink or
  tampered PNG remains visible, or selection scrolls out of a compact viewport.
- Setup or generator: static-looking raw canaries, forged semantic snapshots,
  registered and unregistered previews, changed state/hash/file type/mode,
  compact navigation, stale resize generations, and bounded export limits.
- Independent oracle: decoded `DebugReport`, raw canary absence, PreviewStore
  registry membership and hashes, filesystem modes, and visible focus rail.
- Falsified when: untrusted data is projected, an invalid preview is accepted,
  output exceeds its limit, selected focus disappears, or an old resize enters
  the flight recorder.
- Diagnostics: omission counts, semantic IDs, health, generations, and relative
  registered preview references; payloads and absolute paths are never exposed.
