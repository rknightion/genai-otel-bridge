---
id: GOB-0006
title: 'source_capability{state} — 6-state attribution metric'
status: Done
assignee:
  - '@codex'
created_date: '2026-08-14 16:11'
updated_date: '2026-09-22 14:14'
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
- [x] #1 The state set is closed and enumerated in code, with no path by which a vendor string reaches the label
- [x] #2 The existing 404/auth counters are either subsumed with a documented migration or kept alongside with the overlap stated
- [x] #3 Alert or dashboard consumers under deploy/grafana/ updated in the same change
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [x] #1 just check
- [x] #2 just test-acceptance (only if a §9 acceptance seam changed)
<!-- DOD:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Wave 3 complete migration: L1 replaces the seam and instruments with closed typed vocabularies; L2 maps all fourteen event classes; L3 migrates saved Grafana consumers and operator docs; root reconciles identifiers, wires app.go, updates durable design and package docs, regenerates telemetry, runs CodeRabbit and just check, commits feat! with the breaking-change footer, pushes, verifies exact-SHA CI, then finalizes the tracker.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Wave 2 reserve lane admitted after the primary self-APM work landed, then stopped before edits. Pre-state inspection proved source_graph_unavailable_total carries non-capability events including window_truncated, sessions_truncated, backfill_skipped, span_stats, record-quality drops, export lifecycle failures and scope mismatch. Existing alert and recording-rule consumers depend on graph=window_truncated as a data-loss signal. None of the frozen six capability states truthfully represents truncation or partial-quality events, and the required whole-tree zero legacy-symbol grep also conflicts with immutable history and root-owned durable docs. No implementation or tests were run after this semantic conflict was found.

Wave 3 implemented Rob's approved complete breaking migration. The fourteen producers now map to closed `CapabilityState` or `IncompleteReason` values; the app composition root adapts typed source hooks to string-only schedule metrics. Saved Grafana rules, dashboard generator/artifact, operator docs, durable design and package docs migrated in the same change. Historical retired-symbol mentions remain only in Backlog/campaign material and the immutable archived GitHub-issue export; current code, generated telemetry, dashboards, alert rules and operator docs have zero hits. Validation: focused source/selfobs normal and race tests; Portkey/LangSmith normal and race tests; generated dashboard 37 panels/7 tabs; five Grafana YAML manifests parsed; CodeRabbit review with all valid findings repaired and a fresh zero-finding scoped pass; final `just check < /dev/null` green. The gate explicitly skipped Checkov because it is not installed. Code commit: `90f96ef`.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Parked without code changes. Resume only after Rob chooses the migration contract: preserve a separate bounded event metric for non-capability events, retain the legacy counter for those events while subsuming only capability/auth cases, expand the closed state vocabulary, or approve a complete mapping plus a scoped grep requirement. The recommended choices are a separate bounded event metric or retaining the legacy counter for non-capability events; then transfer source callback, app wiring, Grafana consumer and durable-document ownership before restarting.

Wave 3 completed the approved breaking migration in `90f96ef`: retired `genai_otel_bridge_source_graph_unavailable_total`, replacing it with closed-vocabulary `genai_otel_bridge_source_capability_total{loop,graph,state}` and `genai_otel_bridge_source_data_incomplete_total{loop,reason}` across all fourteen producers and every saved Grafana/operator consumer. Verified with discriminating focused tests, full normal/race/acceptance/integration coverage through `just check`, generated-artifact checks, and CodeRabbit review. Checkov was unavailable and is recorded as skipped; no live vendor, Grafana or deployment action was required.
<!-- SECTION:FINAL_SUMMARY:END -->
