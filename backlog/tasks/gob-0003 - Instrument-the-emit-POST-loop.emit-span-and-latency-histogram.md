---
id: GOB-0003
title: 'Instrument the emit POST: loop.emit span and latency histogram'
status: To Do
assignee: []
created_date: '2026-08-14 16:11'
labels:
  - followup-v2
  - self-obs
  - tracing
dependencies: []
references:
  - followup.md
priority: high
ordinal: 3000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Migrated from `followup.md` §10 (v2 stream) at the 2026-08-14 tracker migration. Also raised as `#60` in the pre-migration issue set.

**The one real self-observability blind spot.** The emit POST to `/v1/metrics` and `/v1/logs` uses a plain `http.Client`, **not** `httpx`, so emit latency is in no histogram at all — the only signal is the `emit_errors_total` counter. A slow or heavily-retried emit is invisible: you can see that it failed, never where the time went or how the per-attempt backoff behaved.

The tick span currently stops at `Enqueue`. The actual encode → OTLP POST → retry/backoff runs asynchronously in the runner's worker goroutine, so the hard part is **carrying the trace context through the bounded queue into the worker** — the batch/queue item has to ferry the context or SpanContext. `followup.md` §10 calls this the single highest-value next span.

Related but separately tracked: the remaining span coverage (`loop.commit`, logs_export lifecycle steps, httpx request spans, election).
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Emit POST latency lands in a histogram, per attempt, so retry timing is visible and not just the terminal outcome
- [ ] #2 A loop.emit span exists as a child of the tick span, with trace context carried through the bounded queue into the worker goroutine
- [ ] #3 Sibling plane checked: metrics and logs emit paths both covered, or the gap stated with its reason
- [ ] #4 Metric and span names added to ARCHITECTURE.md §11 / docs/DESIGN.md — #76 closed on exactly this list being wrong
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 make gate
- [ ] #2 go test -tags acceptance ./internal/app/ (only if a §9 acceptance seam changed)
<!-- DOD:END -->
