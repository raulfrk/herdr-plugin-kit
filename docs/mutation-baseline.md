# Approved equivalent mutants

An entry excludes exactly one LIVED mutant from the actionable denominator.
The source SHA-256 is mandatory and invalidates the entry as soon as its file
changes. Approval must come from an independent reviewer; broad patterns and
wildcards are not accepted.

| file | line | column | symbol | mutant | original | replacement | source_sha256 | hypothesis | proof | reviewer |
| --- | ---: | ---: | --- | --- | --- | --- | --- | --- | --- | --- |
| diagnostics/debug_snapshot.go | 118 | 76 | debugSessionsFromRefs | CONDITIONALS_BOUNDARY | `strings.Compare(sessions[i].ID.String(), sessions[j].ID.String()) < 0` | `strings.Compare(sessions[i].ID.String(), sessions[j].ID.String()) <= 0` | 711567520ddac0b73701b8e88dd62deac95fd2abaa5e060b556909baea234403 | HYP-DEBUG-INSPECTION-01 | Session IDs are unique map keys and `ID.String` returns the sole ID field, so distinct positions cannot compare equal. In Go 1.27.0 every reachable `sort.Slice` comparison uses distinct indices; both predicates therefore agree. Reassess this proof when the Go toolchain or sorting implementation changes because self-comparison would distinguish them. | review_correctness /root/hotkey_plan_correctness; EQ-3FA85D9; 2026-09-11 |
| diagnostics/debug_snapshot.go | 224 | 11 | (*DebugView).Window | CONDITIONALS_BOUNDARY | `start < 0` | `start <= 0` | 711567520ddac0b73701b8e88dd62deac95fd2abaa5e060b556909baea234403 | HYP-DEBUG-INSPECTION-01 | The predicates differ only at `start == 0`; the replacement executes `start = 0`, preserving the value and every output. | review_correctness /root/hotkey_plan_correctness; EQ-3FA85D9; 2026-09-11 |
| diagnostics/debug_snapshot.go | 227 | 11 | (*DebugView).Window | CONDITIONALS_BOUNDARY | `start > len(v.events)` | `start >= len(v.events)` | 711567520ddac0b73701b8e88dd62deac95fd2abaa5e060b556909baea234403 | HYP-DEBUG-INSPECTION-01 | The predicates differ only at `start == len(v.events)`; the replacement assigns that same length, preserving the empty suffix and navigation metadata, including for an empty view. | review_correctness /root/hotkey_plan_correctness; EQ-3FA85D9; 2026-09-11 |
