---
id: GOB-0002
title: Tune bucket_settle from the revised-after-settle age histogram
status: To Do
assignee: []
created_date: '2026-08-14 16:11'
labels:
  - followup-v1
  - durability
  - config-only
dependencies: []
references:
  - followup.md
priority: high
ordinal: 2000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Migrated from `followup.md` §11 (v1 stream — "before/at first real deployment") at the 2026-08-14 tracker migration.

`OnBucketRevised(loop, age)` ships `genai_otel_bridge_bucket_revised_after_settle_age_seconds` (age = now − bucketEnd) alongside the count, specifically so `bucket_settle` can be tuned to p95-of-age rather than guessed. That instrumentation landed in `7e2f54b`/`16f136e`; the tuning did not, because it needs a deployed binary plus roughly days of data.

**Why it matters:** metrics cannot be backfilled — Mimir rejects a changed value at an already-settled `(series, ts)` — so `bucket_settle` is the only lever against late-arriving revisions. Too low and revisions are lost; too high and every series is delayed.

**Known prior evidence, do not skip it:** a clean fixed-window probe on 2026-06-24 suggested genuine settling around ~3m, i.e. the 10m default is likely already generous. But the in-product revised *count* is bursty, so measure before changing. Note `#105` closed on exactly this confusion — `docs/portkey.md` claimed a 3m default when the real default is 10m and 3m had been live-measured insufficient. Do not reintroduce that.

Config-only change; no code.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 p95 of bucket_revised_after_settle_age_seconds read from at least several days of data from a deployed binary, and the value recorded in the task notes
- [ ] #2 bucket_settle either raised to that p95 (if it materially exceeds 10m) or 10m explicitly confirmed as adequate, with the measurement as the evidence
- [ ] #3 If the default changes, all four config-surface files and docs/portkey.md move together
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 make gate
- [ ] #2 go test -tags acceptance ./internal/app/ (only if a §9 acceptance seam changed)
<!-- DOD:END -->
