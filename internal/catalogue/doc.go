// Package catalogue renders maintainer-only static design studies for future UI
// consumers. It is internal tooling, not runtime plugin or generated-project logic.
//
// HYP-CAT-001: comparing structurally different layouts across the complete
// theme/viewport/scenario matrix reveals useful design trade-offs that isolated
// colour studies miss.
//
// HYP-CAT-002: fixed-cell composition with explicit responsive branches keeps
// every study inspectable when a phone keyboard reduces the available height.
//
// HYP-CAT-003: deterministic PNGs and machine-readable indexes make visual
// review reproducible without shipping catalogue assets to plugin consumers.
package catalogue
