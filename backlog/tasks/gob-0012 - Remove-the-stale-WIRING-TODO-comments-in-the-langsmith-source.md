---
id: GOB-0012
title: Remove the stale WIRING TODO comments in the langsmith source
status: To Do
assignee: []
created_date: '2026-08-14 16:12'
updated_date: '2026-09-22 09:07'
labels:
  - docs-drift
  - cleanup
  - good-first-task
dependencies: []
priority: medium
ordinal: 12000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Found during the 2026-08-14 tracker migration audit.

`internal/source/langsmith/langsmith.go:51` and `:74` both carry a `WIRING TODO`, claiming the LangSmith-specific knobs are defaulted in-package and not yet exposed via root config:

- line 51: `// LangSmith-specific (defaults applied below; coordinator exposes these via root config — WIRING TODO).`
- line 74: `// ... LangSmith-specific knobs are defaulted here (WIRING TODO: docs/superpowers/specs/langsmith-poc.md).`

**They are wired.** `stats_window`, `session_filter`, `max_sessions` and the rest all resolve from each loop`s `settings` block — verified in `deploy/helm/values.yaml`, `deploy/ecs/terraform/config.example.yaml` and `test/eks/values-eks.yaml`. Line 74 also points at `docs/superpowers/specs/langsmith-poc.md`, which is **gitignored scratch** — a public source file citing a path that does not exist in any clone.

Small, but it is the same doc-vs-code drift class that dominates the closed issue set (`#135`, `#131`, `#118`, `#100`), and a stale TODO invites someone to do work that is already done.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Both WIRING TODO comments at internal/source/langsmith/langsmith.go:51 and :74 are corrected to describe what the code actually does
- [ ] #2 No tracked Go source references a path under gitignored docs/superpowers/ — the live set is internal/source/langsmith/langsmith.go, internal/source/langsmith/derive.go and internal/logging/logging.go
- [ ] #3 Any docs/superpowers/ citation in ARCHITECTURE.md, docs/DESIGN.md or followup.md is reported to the root for the wiring pass rather than edited by this lane
- [ ] #4 just check green
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 just check
- [ ] #2 just test-acceptance (only if a §9 acceptance seam changed)
<!-- DOD:END -->
