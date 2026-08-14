---
id: GOB-0011
title: Plugin runtime / dynamic SPI for out-of-tree sources
status: To Do
assignee: []
created_date: '2026-08-14 16:12'
labels:
  - followup-v3
  - architecture
dependencies: []
references:
  - followup.md
priority: low
ordinal: 11000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Migrated from `followup.md` §5 (v3 stream) at the 2026-08-14 tracker migration.

Out-of-tree or dynamically loaded sources, so third parties can add a source without touching core. Very high complexity, high risk.

**Only on concrete demand.** The in-tree `source/<vendor>` path already satisfies the decoupling rule, so this buys extensibility for third parties, not for us — and it would put the FROZEN seams under an external compatibility obligation, which is a much stronger commitment than they carry today.

Recorded so the option is not re-derived; not a roadmap item.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A real third-party extension need exists and is named before any design work starts
- [ ] #2 If pursued: the compatibility obligation the FROZEN seams would take on is stated explicitly and accepted in an ARCHITECTURE.md ledger entry first
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 make gate
- [ ] #2 go test -tags acceptance ./internal/app/ (only if a §9 acceptance seam changed)
<!-- DOD:END -->
