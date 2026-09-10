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

The diagnostics root and its private `events-v1/` directory use `0700`; stored
files use `0600`. Ordered segments are named `events-<20-digit ordinal>.jsonl`.
The highest ordinal is active. Segments target 256 KiB, capped by `MaxBytes`;
a larger valid event occupies its own segment and is never split. Retention
replaces only a surviving boundary suffix and removes wholly expired segments.
Unchanged segments retain their names and bytes. Pre-existing symlink,
special-file and invalid owned-name targets are rejected. Values
are recursively validated and redacted before persistence, and reports repeat
that policy for caller-provided metadata. Redaction remains a defense in depth
for legacy records and report metadata; it is not the primary privacy boundary
for plugin events.

Accepted events are appended synchronously, so they are immediately available
to the Debug UI and file readers. Appends rely on the operating system's normal
writeback while the recorder is active, without per-event fsync. Rotation
synchronizes and closes the old segment, writes and synchronizes the new one,
and synchronizes its directory before acknowledging the append. Retention first
synchronizes pending data and its namespace, then publishes and synchronizes a
surviving suffix before removing expired files and synchronizing the directory.
`Close` synchronizes the active file and is idempotent.
This keeps per-event synchronization latency out of the interactive event loop
without buffering events in a second in-process queue.

`EventLogName` identifies the legacy `events.jsonl` import file, not the current
full-history location. Use `Export` for full retained history, or read canonical
segments in ordinal order with a sequence cursor and complete JSONL records.
On first open, legacy history is validated and imported through a private
staging directory. Segment data and the staged namespace are synchronized before
atomic publication; only then is the legacy source unlinked and its parent
directory synchronized. An interrupted publication is reconciled only when the
live history is a validated retained suffix of the legacy history. Foreign root
files are not deleted. Older binaries are incompatible with the segmented layout.

Open validates sequence/time order, recovers corrupt or truncated records under
the existing recovery rules, and applies current retention. Historical segmented
records are not declared corrupt merely because `MaxBytes` was lowered. Before
destructive recovery, surviving data and its namespace are synchronized. Opening
unchanged segmented history does not rewrite its files.

A storage failure marks the recorder unwritable and leaves its previously
published records and cached snapshots unchanged. This is not disk rollback:
a complete valid append from a failed call can be admitted on reopen. Callers
must not assume automatic retry or exactly-once persistence. Open refuses
admission if required recovery or post-unlink directory synchronization fails.

The generated interactive shell is the deliberate exception for the Debug UI's
own six visual identities (`debug.health`, `debug.timeline`, `debug.gallery`,
`debug.hud`, `debug.detail`, and `debug.help`). `debugui.Recording` copies those
semantic values at admission and sends them through one fixed 128-event FIFO and
one storage worker. Admission never waits for disk or queue space; a full queue
rejects the newest debugger event and exposes the rejection in bounded status
counters. Other plugin and shell events keep the synchronous `SemanticSink`
behavior above. `Recording.Close` stops admission, drains accepted events, and
reports any rejection or persistence failure. A process crash can lose pending
debugger events, so normal and error exits must close the recording adapter
before closing preview or recorder storage.

`Recorder.RecordSemanticWithSequence` has the same validation and persistence
contract as `RecordSemantic` and additionally returns the exact committed
sequence. Preview producers use that sequence for correlation instead of
querying for whichever event is newest.

Safe debugger projections are normalized and cached once with each retained
event. `DebugSnapshot` captures immutable projection references and health under
the recorder lock, then prepares indexes and preview eligibility outside it.
`DebugSnapshot.Select` creates a filtered view; `DebugView.Window` and `Locate`
provide bounded sequence-anchored navigation; `ExportWindow` exports the
captured window and revalidates preview provenance. Returned sessions, events,
and nested visual values are copies and do not alias recorder state. Existing
`Debug` and `ExportDebug` defaults and page-size behavior remain unchanged.

## Text-fit measurements

`Frame.PutTextBox` records layout measurements while drawing a bounded text box.
All integer report metadata must be in 0–9,007,199,254,740,991 (2^53−1),
and fit the platform's `int`, so JSON normalization preserves it exactly.
Use a static element ID and instance index, the available cell rectangle, and
`TextClip`, `TextTruncate`, or `TextWrap`. Pass complete hard lines before
applying height limits. `AllowTruncation` defaults to false; enable it only for
intentional abbreviation, such as a list preview. Virtualized rows that remain
reachable through scrolling are not lost content.

`Frame.TextFit` returns a copy of at most 256 observations plus an omitted count.
Each observation contains original width, layout row count, available dimensions,
intent and outcome flags. It contains no text, snippets, hashes or data-derived
identifiers. Unexpected loss means `Clipped || (Truncated && !AllowTruncation)`.
The shell attaches the returned frame's report to its existing `render.completed`
event. Queued debugger admission, projections and exports copy the report.

Debug event details show measured and omitted counts, dimensions, and intentional
or unexpected loss. Legacy events without a report are unmeasured; coverage never
certifies unchecked content. In event detail, Up/Down, Home/End and Page Up/Down
scroll the pinned event, including wrapped measurement rows. Escape returns to
the list position; opening a detail starts at the top.
