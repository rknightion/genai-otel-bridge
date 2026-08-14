---
id: GOB-0006
title: 'source_capability{state} — 6-state attribution metric'
status: To Do
assignee: []
created_date: '2026-08-14 16:11'
labels:
  - followup-v2
  - self-obs
dependencies: []
references:
  - followup.md
priority: low
ordinal: 6000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Migrated from `followup.md` §8/§9 (v2 stream) at the 2026-08-14 tracker migration.

Richer attribution for why a source is not producing: **endpoint-absent / plan-unsupported / permission-denied / no-data / transient-404 / schema-changed**. Today only `genai_otel_bridge_source_graph_unavailable_total` (the 404-skip hook) and the auth-error counter exist.

**This is enrichment, not a coverage gap** — error coverage already exists in logs and counters, which is why it sat in v2 rather than v1. `followup.md` §9 notes the §8 row is marked RESOLVED but covers only the 404 counter; that is the clarification, not a contradiction. Build it if capability flapping ever needs finer attribution than the 404 counter gives.

A new metric with a new label dimension is a cardinality decision: the state set must stay closed and enumerated, never free-text from a vendor response.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The state set is closed and enumerated in code, with no path by which a vendor string reaches the label
- [ ] #2 The existing 404/auth counters are either subsumed with a documented migration or kept alongside with the overlap stated
- [ ] #3 Alert or dashboard consumers under deploy/grafana/ updated in the same change
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 make gate
- [ ] #2 go test -tags acceptance ./internal/app/ (only if a §9 acceptance seam changed)
<!-- DOD:END -->
