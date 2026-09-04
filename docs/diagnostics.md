# Diagnostics storage contract

The kit records structured events through one process-owned `Recorder`. Plugins
sharing that recorder receive serialized sequence assignment, quota reservation,
retention, and report export. Independently opened recorders and cross-process
writes to the same directory are intentionally outside this contract.

Plugin code records through `SemanticSink.RecordSemantic`. Its event and visual
state types contain only bounded identifiers, counters, booleans, durations, and
geometry. They deliberately provide no free-form message, path, query, document,
or terminal-output field. Identifiers must be static developer-defined values;
`NewID` validates their shape and size but cannot establish their provenance.

`Event` is the persisted/report wire schema, not a plugin input API. Keeping the
wire type public lets tools decode existing logs and reports without exposing a
mutable, free-form recording path. Tests in the diagnostics package exercise
legacy wire validation directly; generated plugins receive only a
`SemanticSink`.

Default bootstrap values favor useful debugging history while bounding local
storage:

| Setting | Default |
| --- | ---: |
| Retained events | 100,000 |
| Event-log bytes | 128 MiB |
| Event age | 14 days |
| Structured details per event | 1 MiB |
| UI snapshot name and text per event | 2 MiB |
| Exported debug report | 32 MiB |

Every value is configurable. Validation rejects a non-positive count or age, an
event-log budget below 1 KiB or above 1 GiB, non-positive detail/snapshot
budgets, and a report budget below 1 KiB. Retention removes the oldest events
first and records pressure/drop state in `Health`.

The diagnostics directory and JSONL event log use `0700` and `0600`
permissions. Pre-existing symlink or special-file targets are rejected. Values
are recursively validated and redacted before persistence, and reports repeat
that policy for caller-provided metadata. Redaction remains a defense in depth
for legacy records and report metadata; it is not the primary privacy boundary
for plugin events.

Accepted events are appended synchronously, so they are immediately available
to the Debug UI and file readers. Appends rely on the operating system's normal
writeback while the recorder is active; `Close` synchronizes the log before
returning. Retention rewrites synchronize their replacement before publication.
This keeps per-event synchronization latency out of the interactive event loop
without buffering events in a second in-process queue.
