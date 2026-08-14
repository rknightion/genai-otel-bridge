---
id: GOB-0004
title: >-
  Self-APM: remaining span coverage (loop.commit, logs_export steps, httpx,
  election)
status: To Do
assignee: []
created_date: '2026-08-14 16:11'
labels:
  - followup-v2
  - self-obs
  - tracing
dependencies:
  - GOB-0003
references:
  - followup.md
priority: medium
ordinal: 4000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Migrated from `followup.md` §10 (v2 stream) at the 2026-08-14 tracker migration. Enrichment over the existing logs + counters; depends on the `loop.emit` span landing first, since propagation is the shared hard part.

Four candidates, in `followup.md`'s own priority order:

- **`loop.commit`** — the epoch-fenced `Save` (ConfigMap RMW) has only a `checkpoint_fenced` counter, so a slow or contended write and a fence trip have no timing. Lives in `schedule/runner.go`; child of the emit span or the tick depending on propagation.
- **logs_export job lifecycle steps** — the Portkey export is a multi-tick state machine (create → poll → download → page). Logs show coarse phase transitions, but where a slow export spends time (queued at Portkey vs download streaming) is not timed. Cross-tick correlation is the hard part: each step is a separate `Collect`, so the likely shape is a stored trace id in `exportCursor` plus span links.
- **httpx request spans (`upstream.request`) nested under the tick** — the upstream histogram gives per-target latency but not the causal parent, nor in-request detail (DNS, cross-host-redirect block, SSRF-guard reject). Wire it via the existing `httpx.Observer` seam or an `otelhttp`-style RoundTripper so `httpx` stays decoupled; that covers **all** source calls.
- **`coordinate.elect`** — lowest priority. Failover handoff duration is inferable from Lease `leaseTransitions` plus logs, and the lease-transition metric plus e2e already characterise it.

Treat the four as separately landable; a partial result is fine if the notes say which are done.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 loop.commit span times the fenced Save and distinguishes fenced from clean
- [ ] #2 httpx request spans nest under loop.tick via the existing Observer seam, leaving httpx decoupled
- [ ] #3 logs_export per-step timing exists, or the cross-tick correlation approach is recorded as rejected with its reason
- [ ] #4 Any span or metric name added is reflected in ARCHITECTURE.md §11 and docs/DESIGN.md in the same change
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 make gate
- [ ] #2 go test -tags acceptance ./internal/app/ (only if a §9 acceptance seam changed)
<!-- DOD:END -->
