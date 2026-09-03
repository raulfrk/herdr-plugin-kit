# Approved equivalent mutants

An entry excludes exactly one LIVED mutant from the actionable denominator.
The source SHA-256 is mandatory and invalidates the entry as soon as its file
changes. Approval must come from an independent reviewer; broad patterns and
wildcards are not accepted.

| file | line | column | symbol | mutant | original | replacement | source_sha256 | hypothesis | proof | reviewer |
| --- | ---: | ---: | --- | --- | --- | --- | --- | --- | --- | --- |
| runtime/agenthost/agenthost.go | 263 | 16 | boundedTail | CONDITIONALS_BOUNDARY | `len(lines) > DetectionLines` | `len(lines) >= DetectionLines` | c9db06d86549afa7c7600c25608b6c1b58076379695fa43dcb2354425774a8bc | HYP-AGENT-STATUS-01 | For every line count n and k=60, both expressions choose the same branch unless n=k; at n=k the replacement slices `lines[0:]`, preserving every element and the exact joined result. The 60/61 boundary regression test passes and arithmetic slice mutants are killed. | Codex thread `01a067a9-19dd-7ce1-895d-688a45092827` |
| runtime/agenthost/agenthost.go | 270 | 16 | normalize | CONDITIONALS_BOUNDARY | `len(lines) > classifierLines` | `len(lines) >= classifierLines` | c9db06d86549afa7c7600c25608b6c1b58076379695fa43dcb2354425774a8bc | HYP-AGENT-STATUS-01 | The same all-input proof holds for k=12: only n=k differs in branch execution, and `lines[0:]` yields identical normalization and classification. The 12/13 boundary regression test passes and arithmetic slice mutants are killed. | Codex thread `01a067a9-19dd-7ce1-895d-688a45092827` |
