---
id: GOB-0017
title: Default to native (exponential) histograms for all histogram instruments
status: Parked
assignee:
  - '@codex'
created_date: '2026-09-15 13:00'
updated_date: '2026-09-22 10:12'
labels:
  - observability
  - cardinality
dependencies: []
priority: medium
type: chore
ordinal: 17000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Standing default across Rob's OTLP exporters: every histogram instrument emits base2 exponential (native) histograms, never classical buckets. Logging it here so this repo does not reintroduce the problem the sibling exporters are being fixed for.

This repo is NOT currently a contributor. As of 2026-09-15 no series on the m7kni stack (stack 1217581, Mimir tenant 2359401) carry job="genai-otel-bridge", and a grep for ExponentialHistogram / base2_exponential / NativeHistogram in this tree returns nothing outside vendor. So this is preventive, not remedial: no measured saving to claim.

For scale, the sibling repos measured on the same day:
  codexlb2otel   4,768 classical bucket series across 23 families
  opnsense2otel  2,621 classical bucket series across 4 families

A classical histogram costs one series per bucket plus _sum plus _count. A native histogram is one series for the whole distribution at better resolution. The gen_ai_* semconv families are the expensive ones in codexlb2otel (gen_ai_client_token_usage_bucket alone is 2,010 series), and this bridge emits the same semantic convention, so it would land in the same place at the same scale once it carries traffic.

Cheapest route, no code change:
  OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION=base2_exponential_bucket_histogram

Explicit route is a metric.View with metric.AggregationBase2ExponentialHistogram{MaxSize: 160, MaxScale: 20}.

Do this before the bridge carries real traffic. Retrofitting means reworking every dashboard query that used a le label.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 Histogram instruments emit base2 exponential histograms by default
- [x] #2 The aggregation choice is documented in the repo config reference, with the env var named
- [ ] #3 No classical _bucket family reaches the stack once the bridge carries traffic, verified with gcx metrics query for job=genai-otel-bridge
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [x] #1 just check
- [x] #2 just test-acceptance (only if a §9 acceptance seam changed)
<!-- DOD:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Wave 1 L4: add an explicit base2 exponential histogram view, pin it with a failing-first test, regenerate dashboard consumers, and defer live-stack AC#3 to gob-0002.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
SCOPE, measured 2026-09-22 during wave-1 goal authoring.

**The instruments.** Three histograms, all in one place: `internal/selfobs/metrics.go:42` builds them
through a local `mh()` helper that passes `metric.WithExplicitBucketBoundaries(bounds...)` — so
`upstreamDur`, `revisedAge` and `emitDur` (`metrics.go:20`) are the whole population. There are no
`metric.View`s anywhere in the tree and no reference to exponential aggregation outside vendor/,
confirming the task's 'preventive, not remedial' framing.

**The consumers are the real work, and the task text does not mention them.** `deploy/grafana/self-obs/`
carries 7 `histogram_quantile` queries in `dashboard-self-obs.yaml` — and that YAML has a generator,
`deploy/grafana/self-obs/gen_dashboard.py`, which carries the same 7. **Edit the generator and
regenerate; do not hand-edit the YAML**, or the next regeneration silently reverts the change.
`alertrule-bucket-revised-after-settle.yaml` and `recordingrule-scrape-healthy.yaml` also reference
these families and must be checked. A native histogram changes the query shape: the classical
`histogram_quantile(q, sum(rate(x_bucket[w])) by (le))` loses its `by (le)` and reads the native
series directly, so every one of these is a real edit rather than a rename.

**docs/telemetry.md is generated** (`internal/docs/gen`, gated by `just gen-check` inside `just check`).
Run `just gen` and commit the output; hand-editing it fails the gate.

**AC#3 cannot be satisfied this wave.** Confirmed with Rob 2026-09-22: the bridge is not emitting
anywhere, so there is no stack on which to prove no classical `_bucket` family arrives. Treat AC#3 as
deferred with gob-0002's deployment as its successor condition, and say so explicitly rather than
marking it done on the strength of the code change.

**Prefer the explicit `metric.View` over the env var.** The task offers
`OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION=base2_exponential_bucket_histogram` as the
cheapest route, but this repo owns its MeterProvider construction in `internal/selfobs/provider.go`,
and an env var set outside the chart is exactly the 'config accepted, behaviour silently wrong' shape
the wave operating model names as this codebase's worst failure mode. A View in code cannot be
un-set by a deployment that forgets an env var.

The acceptance seam was unchanged, so the conditional acceptance-test update item is not applicable. AC3 requires a deployed, emitting instance and remains deliberately unchecked.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Native base2 exponential histogram views landed in 35b2dfba437053b85c981c0e22f0ea8965e6c0e8, and the authoritative aggregation settings were documented in ace1329d93c8c3915dc7e1f7422b3454b2bb394b. Parked because no deployed telemetry exists in this wave. Resume after gob-0002 deploys: query the job's self-observability series and prove native histograms exist while classical _bucket series do not.
<!-- SECTION:FINAL_SUMMARY:END -->
