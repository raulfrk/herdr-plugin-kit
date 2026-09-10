# Approved equivalent mutants

An entry excludes exactly one LIVED mutant from the actionable denominator.
The source SHA-256 is mandatory and invalidates the entry as soon as its file
changes. Approval must come from an independent reviewer; broad patterns and
wildcards are not accepted.

| file | line | column | symbol | mutant | original | replacement | source_sha256 | hypothesis | proof | reviewer |
| --- | ---: | ---: | --- | --- | --- | --- | --- | --- | --- | --- |
| diagnostics/debug_snapshot.go | 118 | 76 | debugSessionsFromRefs | CONDITIONALS_BOUNDARY | `strings.Compare(sessions[i].ID.String(), sessions[j].ID.String()) < 0` | `strings.Compare(sessions[i].ID.String(), sessions[j].ID.String()) <= 0` | f56aea3a3661ffa39ce4022ebef194eb0bdb618f9b4cb3389bdde8ae5013cb9b | HYP-DEBUG-INSPECTION-01 | Session IDs are unique map keys and `ID.String` returns the sole ID field, so distinct positions cannot compare equal. In Go 1.27.0 every reachable `sort.Slice` comparison uses distinct indices; both predicates therefore agree. Reassess this proof when the Go toolchain or sorting implementation changes because self-comparison would distinguish them. | review_correctness `01a07e2f-1d29-7190-9d2f-d74b2bc9ccc8`; FRESH-TEXTFIT-CORRECTION-EQUIVALENCE-529AD5D, 2026-09-09 14:25 UTC |
| diagnostics/debug_snapshot.go | 231 | 11 | (*DebugView).Window | CONDITIONALS_BOUNDARY | `start < 0` | `start <= 0` | f56aea3a3661ffa39ce4022ebef194eb0bdb618f9b4cb3389bdde8ae5013cb9b | HYP-DEBUG-INSPECTION-01 | The predicates differ only at `start == 0`; the replacement executes `start = 0`, preserving the value and every output. | review_correctness `01a07e2f-1d29-7190-9d2f-d74b2bc9ccc8`; FRESH-TEXTFIT-CORRECTION-EQUIVALENCE-529AD5D, 2026-09-09 14:25 UTC |
| diagnostics/debug_snapshot.go | 234 | 11 | (*DebugView).Window | CONDITIONALS_BOUNDARY | `start > len(v.events)` | `start >= len(v.events)` | f56aea3a3661ffa39ce4022ebef194eb0bdb618f9b4cb3389bdde8ae5013cb9b | HYP-DEBUG-INSPECTION-01 | The predicates differ only at `start == len(v.events)`; the replacement assigns that same length, preserving the empty suffix and navigation metadata, including for an empty view. | review_correctness `01a07e2f-1d29-7190-9d2f-d74b2bc9ccc8`; FRESH-TEXTFIT-CORRECTION-EQUIVALENCE-529AD5D, 2026-09-09 14:25 UTC |
