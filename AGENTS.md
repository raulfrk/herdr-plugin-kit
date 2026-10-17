# Efficient development and verification

Apply these repository-specific rules alongside the general engineering
instructions. Preserve the acceptance criteria and thresholds in
`docs/test-strategy.md`; efficiency comes from sequencing and evidence reuse.

Pushing: after local checks pass, commit and push without asking.

## Work in small observable increments

- Separate fixture readiness, narrow assumption proofs, and complete public
  acceptance; use the technical-investigation and review-gate workflows without
  turning each setup detail into another full test or review cycle.
- After a shared-tooling detour, return to the consumer's original requirements
  and reconcile its outstanding journeys and release gates in the existing
  Beads/traceability. Producer success alone is not whole-product acceptance.
- Inspect the affected implementation and existing evidence before proposing
  work. State the next observable result and its smallest sufficient check.
- Prove uncertain supported-version behavior with a disposable live experiment
  before implementing it. Complete affected live Herdr/harness validation
  before starting expensive final release verification.
- Keep corrections scoped. Do not add infrastructure, abstractions, test
  frameworks, or new tracking records solely to organize a small change.
- For instruction-only or documentation-only changes, review the text and diff.
  Run executable checks only when the change affects an executable example,
  generated output, or another actual acceptance contract.

## Sequence verification by cost and risk

1. Implement the behavior and run focused deterministic regression tests.
2. Run affected supported-version live checks and resolve fixture/setup failures
   separately from production defects.
3. Complete required independent review and corrections. Reuse the reviewer
   thread; supply the delta and current evidence. Re-review only affected
   findings and risks until no actionable findings remain.
4. Once the candidate is stable, run the required broad tests, race,
   property/model, timing, vet, build, tidy, and generated-consumer gates.
5. Run changed-code mutation against the last verified baseline. Schedule the
   required full mutation run once the implementation, live evidence, and
   review corrections have settled. Finish with acceptance of the complete
   evidence; do not introduce new scope at this boundary.

At each transition between focused, live, review, broad, and mutation phases,
reconcile the next action with the active leaf Bead's acceptance criteria and
latest decision-relevant notes. Record new evidence on that owning leaf. Update
parent or related Beads only when the evidence changes their own status,
dependency, acceptance criteria, or release decision; do not duplicate the
same checkpoint across the graph.

Do not repeat a successful check merely because a commit was created, context
was compacted, a reviewer was dispatched, or an artifact was renamed. Before
rerunning it, identify the changed input, failure, or unresolved concern that
could invalidate its result. Include transitive dependencies and test/tool
configuration when deciding whether evidence is reusable.

Record the tested SHA, relevant command, outcome, and artifact location in
existing task notes. Distinguish retained evidence from checks freshly run on
the current candidate. Never relabel older evidence with a newer SHA. Explicit
requirements for verification against an exact candidate still apply; do not
waive them or lower mutation thresholds to save time. If a late correction
invalidates a full run, finish all corrections before scheduling its replacement.

## Handle long-running checks deliberately

- Retain the process handle and collect its actual exit status. Never restart a
  healthy run after an interrupted wait or context compaction.
- Use known runtime and meaningful milestones to reassess progress. Inspect a
  process only when a missed milestone, silence beyond expectations, or an
  error makes it useful. Follow runtime limits on individual waits.
- Keep progress output compact: summarize the phase and decision-relevant
  findings. Avoid per-file mutation narration and repeatedly fetching large
  logs; use the final structured report for acceptance.
- Do useful independent work while waiting when it cannot interfere with the
  check. Respect the existing exclusive lock, worker limit, and isolated
  mutation worktree; avoid competing timing or live-device workloads.
- Finish once the requested result and required gates pass. Report adjacent
  improvements separately instead of extending the implementation.
