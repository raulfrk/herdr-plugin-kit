# Diagnostics storage contract

The kit records structured events through one process-owned `Recorder`. Plugins
sharing that recorder receive serialized sequence assignment, quota reservation,
retention, and report export. Independently opened recorders and cross-process
writes to the same directory are intentionally outside this contract.

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
that policy for caller-provided metadata.
