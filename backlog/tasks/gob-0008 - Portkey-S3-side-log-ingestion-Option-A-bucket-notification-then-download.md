---
id: GOB-0008
title: 'Portkey S3-side log ingestion (Option A: bucket notification then download)'
status: To Do
assignee: []
created_date: '2026-08-14 16:12'
labels:
  - followup-v2
  - portkey
  - new-surface
dependencies: []
references:
  - followup.md
priority: low
ordinal: 8000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Migrated from `followup.md` §9 (v2 stream) at the 2026-08-14 tracker migration.

A **new logs source mode** for operator-owned, WORM-capable S3: bucket notification → poller download, as an alternative or augment to the export-job lifecycle loop. v1 `logs_export` uses the Portkey export-job lifecycle only.

**PoC-gated and conditional — build it only if an operator deployment actually uses Option A.** It sits behind the existing frozen source interface as a new mode, alongside the §4 future source categories; it is not a change to the seam.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The mode sits behind the existing frozen source/Loop interface with no seam change
- [ ] #2 A concrete operator deployment needing Option A exists before build starts — otherwise this stays To Do
- [ ] #3 Signed-URL and egress handling matches the hardened logs_export path (scheme validation per #139, credential redaction per #34)
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 make gate
- [ ] #2 go test -tags acceptance ./internal/app/ (only if a §9 acceptance seam changed)
<!-- DOD:END -->
