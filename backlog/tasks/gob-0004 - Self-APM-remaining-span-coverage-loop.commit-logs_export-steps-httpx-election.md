---
id: GOB-0004
title: >-
  Self-APM: remaining span coverage (loop.commit, logs_export steps, httpx,
  election)
status: Done
assignee:
  - '@codex'
created_date: '2026-08-14 16:11'
updated_date: '2026-09-22 12:11'
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
- [x] #1 loop.commit span times the fenced Save and distinguishes fenced from clean
- [x] #2 Upstream HTTP request spans nest under loop.tick through the shared httpx client transport, leaving httpx decoupled
- [x] #3 logs_export per-step timing exists, or the cross-tick correlation approach is recorded as rejected with its reason
- [x] #4 Any span or metric name added is reflected in ARCHITECTURE.md §11 and docs/DESIGN.md in the same change
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [x] #1 just check
- [x] #2 just test-acceptance (only if a §9 acceptance seam changed)
<!-- DOD:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Wave 2 (2026-09-22):
1. Run L1-L3 concurrently on disjoint owned files: loop.commit span, end-to-end upstream span nesting proof, and logs_export step-timing accept/reject disposition.
2. Root accepts each return against the frozen goal and performs the L4 design/catalogue/generated-doc wiring pass.
3. Run targeted review plus `just check < /dev/null`, commit explicit paths to main, push, and verify CI plus ci-success at the exact SHA.
4. Admit gob-0006 reserve R1 only if its post-L4 conditions and cutoff still hold.
5. Reconcile gob-0004 through the Backlog CLI, then write the terminal wave report and notify once.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Wave 2 preflight at 2026-09-22T11:38Z: fetched origin/main; local HEAD and origin/main both 220eca611ac5c1ad8c6eb0f1a5203acd5f7d6323; clean single worktree; exact-base CI run 35722047761 still in progress. Release PR #3 remains explicitly out of scope.

Wave 2 completed the remaining selected self-APM coverage. loop.commit wraps Checkpointer.Save and records committed, fenced, stale and error outcomes; the stale follow-up Load failure is classified as error. The upstream nesting test proves the existing otelhttp transport creates a CLIENT ancestor reaching loop.tick, so the AC wording was corrected from the unused Observer implementation detail to the shared httpx transport actually shipped. logs_export now emits create/start/poll/download/page/blocked step spans and preserves cross-tick correlation through durable trace/span context links; legacy cursor JSON remains byte-identical. SetLoopClockForTest was widened to support logsExportLoop so the named deterministic clock seam is exercised. ARCHITECTURE.md, docs/DESIGN.md and touched package AGENTS docs record the new span contract. just gen produced no generated-output diff. CodeRabbit pass 1 produced two invalid AGPL-header findings rejected against the Apache-2.0 SPDX gate and one valid stale-load outcome finding that was fixed; pass 2 returned zero findings.

Verification: failing-first targeted tests were recorded for loop.commit, the upstream CLIENT-span negative control, logs_export create coverage and the logs-export clock helper. Targeted tests then passed; full just test and just test-race passed; just check < /dev/null passed. Local Checkov was absent and self-skipped, while exact-SHA CI run 35724808196 at 25dce388af4aadaa4927053780aef85def5b1bd0 installed Checkov and completed all 12 primary jobs plus ci-success successfully.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Closed self-APM span coverage in four commits: 6d712435 adds loop.commit outcome timing; 1f761c1 proves existing otelhttp upstream CLIENT spans nest beneath loop.tick; c6e822f adds durable cross-tick Portkey logs_export lifecycle step spans; 25dce38 records the durable span contract. Verified by failing-first targeted tests, full test and race suites, local just check, two CodeRabbit passes ending with zero findings, and exact-SHA CI run 35724808196 with ci-success green.
<!-- SECTION:FINAL_SUMMARY:END -->
