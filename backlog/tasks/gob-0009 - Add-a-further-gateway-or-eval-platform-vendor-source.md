---
id: GOB-0009
title: Add a further gateway or eval-platform vendor source
status: To Do
assignee: []
created_date: '2026-08-14 16:12'
labels:
  - followup-v3
  - new-vendor
dependencies: []
references:
  - followup.md
priority: low
ordinal: 9000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Migrated from `followup.md` §4 (v3 stream) at the 2026-08-14 tracker migration.

Each new vendor is a new `source/<vendor>` package behind the frozen `source.Source` / `source.Loop` interface. Complexity scales per vendor; risk is low precisely because the seam is frozen — **and a new vendor that seems to need a seam change is a design decision, not a lane decision.**

This task is the standing placeholder. When a specific vendor is chosen, split it into its own task rather than growing this one.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The vendor lives entirely in internal/source/<vendor> with no vendor knowledge in core code or defaults
- [ ] #2 No change to the FROZEN model.* types or source interfaces; if one seems needed, it is raised as a design decision with an ARCHITECTURE.md ledger entry first
- [ ] #3 The four config-surface files and the docs page land in the same change
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 make gate
- [ ] #2 go test -tags acceptance ./internal/app/ (only if a §9 acceptance seam changed)
<!-- DOD:END -->
