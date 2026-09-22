---
id: GOB-0002
title: Tune bucket_settle from the revised-after-settle age histogram
status: Parked
assignee: []
created_date: '2026-08-14 16:11'
updated_date: '2026-09-22 09:07'
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
- [ ] #1 just check
- [ ] #2 just test-acceptance (only if a §9 acceptance seam changed)
<!-- DOD:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
RESUME BOUNDARY (set 2026-09-22 during wave-1 goal authoring).

**Blocked on a deployment, not on this repo.** Confirmed with Rob 2026-09-22: the bridge is not
emitting anywhere. gob-0017 independently established that as of 2026-09-15 no series on the m7kni
stack (stack 1217581, Mimir tenant 2359401) carry `job="genai-otel-bridge"`. AC#1 asks for p95 of
`genai_otel_bridge_bucket_revised_after_settle_age_seconds` over several days of real data, so
there is nothing to measure and no amount of local work substitutes.

**The exact check that unparks this:** a deployed binary has been emitting for at least several
days, and

    histogram_quantile(0.95, sum(rate(genai_otel_bridge_bucket_revised_after_settle_age_seconds_bucket[1d])) by (le))

returns a value. Note that gob-0017 converts these instruments to native exponential histograms, so
after it lands the query loses `by (le)` and reads the native series directly — re-derive the query
from whatever gob-0017 shipped rather than copying the classical form above.

**Already ruled out:** guessing from the 2026-06-24 fixed-window probe. It suggested settling at
roughly 3m, which would imply the 10m default is generous, but the in-product revised count is
bursty and `#105` closed on exactly the confusion of treating 3m as the default. A change made on
that probe alone would reintroduce a closed defect. Do not raise or lower `bucket_settle` without
the deployed measurement.
<!-- SECTION:NOTES:END -->
