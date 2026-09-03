// Package catalogue runs maintainer-only live and static design studies for
// future UI consumers. It is internal tooling, not runtime plugin or
// generated-project logic.
//
// HYP-CAT-001: the selected Bento Command / Structured layout remains complete
// and inspectable across the complete theme/viewport/scenario matrix.
//
// HYP-CAT-002: fixed-cell composition with explicit responsive branches keeps
// every study inspectable when a phone keyboard reduces the available height.
//
// HYP-CAT-003: deterministic PNGs and machine-readable indexes make visual
// review reproducible without shipping catalogue assets to plugin consumers.
//
// HYP-CAT-004: reviewing the same surface model through the production shell at
// its delivered terminal size exposes mobile input and resize failures that
// scaled screenshots cannot.
package catalogue
