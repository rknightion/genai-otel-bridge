---
id: GOB-0003
title: 'Instrument the emit POST: loop.emit span and latency histogram'
status: Done
assignee:
  - '@codex'
created_date: '2026-08-14 16:11'
updated_date: '2026-09-22 10:12'
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
- [x] #1 Emit POST latency lands in a histogram, per attempt, so retry timing is visible and not just the terminal outcome
- [x] #2 A loop.emit span exists as a child of the tick span, with trace context carried through the bounded queue into the worker goroutine
- [x] #3 Sibling plane checked: metrics and logs emit paths both covered, or the gap stated with its reason
- [x] #4 Metric and span names added to ARCHITECTURE.md §11 / docs/DESIGN.md — #76 closed on exactly this list being wrong
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [x] #1 just check
- [x] #2 just test-acceptance (only if a §9 acceptance seam changed)
<!-- DOD:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Wave 1 L5 after L4: test-first schedule-local SpanContext envelope, loop.emit child span, and per-attempt emit latency for both planes without changing model.Batch.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
FROZEN DECISION, taken 2026-09-22 during wave-1 goal authoring. Do not reopen inside a lane.

**The bounded queue is `chan model.Batch` (`internal/schedule/runner.go:34,66`), and `model.Batch` is FROZEN.** The task description says the hard part is 'carrying the trace context through the bounded queue into the worker', and the obvious implementation — a context or SpanContext field on `model.Batch` — is a FROZEN-seam change. Per the wave operating model that is a design decision requiring an ARCHITECTURE.md ledger entry, decided before fan-out and never inside a lane. It is therefore decided here, and the answer is **do not touch `model.Batch`.**

**Use a schedule-package-local envelope instead.** Change the channel's element type to an unexported
struct private to `internal/schedule` that carries the batch alongside the propagation value, e.g.
`chan queued` where `queued` wraps a `model.Batch` and a `trace.SpanContext`. `Enqueue`'s exported
signature keeps taking `model.Batch` and lifts the span context off the passed `ctx` itself, so no
caller changes and no seam moves. A `trace.SpanContext` is a value type and safe to copy across the
queue; do not put a live `context.Context` in the envelope, because the tick context is cancelled
when the tick returns and the worker would then hold a dead context.

Rationale for freezing it rather than parking: the envelope is strictly inside one package with no
exported surface, it satisfies AC#2 exactly as written, and the alternative is a FROZEN-seam change
with implementation exposure across emit, source and app for no benefit. If the envelope turns out
not to work, that is a stop condition — return it to the root, do not amend `model.Batch`.

The acceptance seam was unchanged, so the conditional acceptance-test update item is not applicable.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Implemented in dbf5e4f2c1b9ddea6b5a5153d2de0c8ba6f7b4d2, with design-record reconciliation in a57fcca06766c6a7c572a0da1b4353dd498ac70c. The emit path now records one latency sample for every final POST attempt on both success and failure, and creates loop.emit as a child of the live loop tick while preserving cancellation and lease-epoch fencing. Targeted, race, lint, and integrated just check gates passed.
<!-- SECTION:FINAL_SUMMARY:END -->
