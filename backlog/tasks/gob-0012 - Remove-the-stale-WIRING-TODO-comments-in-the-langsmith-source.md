---
id: GOB-0012
title: Remove the stale WIRING TODO comments in the langsmith source
status: Done
assignee:
  - '@codex'
created_date: '2026-08-14 16:12'
updated_date: '2026-09-22 10:12'
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
- [x] #1 Both WIRING TODO comments at internal/source/langsmith/langsmith.go:51 and :74 are corrected to describe what the code actually does
- [x] #2 No tracked Go source references a path under gitignored docs/superpowers/ — the live set is internal/source/langsmith/langsmith.go, internal/source/langsmith/derive.go and internal/logging/logging.go
- [x] #3 Any docs/superpowers/ citation in ARCHITECTURE.md, docs/DESIGN.md or followup.md is reported to the root for the wiring pass rather than edited by this lane
- [x] #4 just check green
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [x] #1 just check
- [x] #2 just test-acceptance (only if a §9 acceptance seam changed)
<!-- DOD:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Wave 1 L6: correct stale Go comments and report root-owned documentation citations for the wiring pass.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
The acceptance seam was unchanged, so the conditional acceptance-test update item is not applicable.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Removed the stale LangSmith WIRING TODOs in 200bc9737e20bfe5967c4e4a8bf979d889ef1e05 and reconciled the remaining scratch references in a57fcca06766c6a7c572a0da1b4353dd498ac70c. Integrated just check passed.
<!-- SECTION:FINAL_SUMMARY:END -->
