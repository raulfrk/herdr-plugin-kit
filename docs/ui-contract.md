# Plugin UI contract

This document is the binding visual and responsive contract for Plugin Kit
v0.1. The approved direction is **Bento Command** with **Structured** selection.
The catalogue is review tooling; generated plugins implement this contract
without including catalogue code or assets.

## Visual language

- Use semantic palette tokens only. Catppuccin is the default and Catppuccin
  Latte is the documented light choice; every built-in Herdr theme and custom
  semantic overrides remain supported.
- Keep header and status regions quiet. Search is borderless with an accent
  rail. The selected row has a filled, bold treatment and a visible `▌` so
  focus never depends on colour alone.
- Do not draw ASCII boxes. Loading, empty, disabled, error, focus, progress,
  and success states need a text or glyph cue in addition to colour.
- Use at most four chrome rows and preserve at least six task rows at `40x10`.
- Compact views use a single-screen drill-in. Standard and Wide views show
  results and context together, using a 2:1 split where space permits.
- Automatic light/dark switching is not supported until Herdr exposes a
  stable appearance signal.

## Geometry and lifecycle

| Class | Inclusive rule | Presentation |
| --- | --- | --- |
| Recovery | columns below 40 or rows below 10 | Explain the minimum size while preserving interaction state. |
| Compact | supported size with columns below 80 or rows below 18 | Single-screen task flow and visible basic-key controls. |
| Standard | at least `80x18`, below `110x24` | Results with bounded context when it fits. |
| Wide | at least `110x24` | Results and context together. |

Exactly 18 rows deliberately retain the compact single-screen presentation,
even though the shell class is Standard. Reported dimensions remain visible;
render dimensions clamp independently to `500x200`. Oversized projection must
not allocate or draw beyond that bound.

Recovery and resize bursts preserve the query and editing state, selected item
and page, form draft and validation, pending request generations, and current
drill-in screen. Only the newest request and resize generation may commit. The
shell renders the newest size immediately and marks it settled only after its
100 ms settle event survives later resize generations.

## Controls

| Key | Contract |
| --- | --- |
| `/` | Enter or focus search. |
| Arrows | Move selection or focus. |
| Home / End | Move to the first or last available item. |
| PageUp / PageDown | Move through result pages. |
| Enter | Open, activate, apply, or commit the focused operation. |
| Tab | Move between regions or form fields. |
| Backspace | Edit text; when not editing, navigate back. |
| Escape | Navigate back; from the root, close. |
| `?` | Open complete help. |
| Ctrl-C | Quit. |

Any operation normally reached with a special key must also expose a visible
basic-key alternative on mobile. Footer hints may abbreviate, but `?` must
always open the complete unclipped guide.

## Required viewport evidence

Deterministic evidence covers every built-in theme at `40x10`, `48x18`,
`48x30`, `78x10`, `78x20`, `80x18`, `110x24`, and `500x200`. Recovery and
oversized lifecycle behavior are shell tests rather than static screenshots.

The final live device review records the geometry HUD after at least 250 ms of
quiet and completes help, search, selection, Enter, and Backspace tasks in each
explicitly labelled state:

| Device state | Reported geometry | Rendered geometry | Generation settled | Result |
| --- | --- | --- | --- | --- |
| iPhone, keyboard open | `70x37` | `70x37` | generation 5, event 784 | Approved; returned cleanly after the close/open burst. |
| iPhone, keyboard closed | `70x67` | `70x67` | generation 4, event 780 | Approved after the intermediate `70x63` resize settled. |
| iPhone, landscape | `148x39` | `148x39` | generation 1, event 766 | Approved at native terminal scale. |

These labels come from the explicit landscape → portrait keyboard-open →
keyboard-close → keyboard-open review sequence, not from a size heuristic.
The diagnostics show each final generation settling after the resize burst;
the user completed the live catalogue review and selected Bento Command.

## Agent status presentation

Session and attention surfaces treat agent status as an assessment, not a
trusted Herdr field. They display the kit's settled Codex-aware result together
with a non-colour cue. `Unsettled` and `Unrecognized` are first-class outcomes:
they never silently become idle or done, and their details link to a sanitized
diagnostic event. Raw terminal text must not enter UI state, diagnostics, or
exports.

## Privacy and diagnostics

Semantic diagnostics may contain bounded identifiers, outcomes, counters,
durations, generations, and geometry. Queries, form values, secrets, paths,
terminal text, and rendered user content are forbidden. Debug previews render
semantic placeholders and must pass PreviewStore provenance checks before they
can appear in a gallery or exported report.
