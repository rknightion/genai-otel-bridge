---
id: GOB-0001
title: Don't promote service.version to a per-series metric label
status: Parked
assignee:
  - '@codex'
created_date: '2026-08-14 16:10'
updated_date: '2026-09-22 10:12'
labels:
  - telemetry
  - cardinality
  - from-gh-165
dependencies: []
references:
  - 'https://github.com/rknightion/graph2otel/issues/104'
documentation:
  - archive/github-issues-2026-08-14.json
priority: medium
ordinal: 1000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Migrated from GitHub issue **#165** (open at the 2026-08-14 tracker migration; full original body in `archive/github-issues-2026-08-14.json`).

**PREMISE CORRECTED 2026-09-22 during wave-1 goal authoring — read this before doing any work.** The issue body was written as a cross-repo consistency pass from `rknightion/graph2otel#104` and two of its claims are WRONG for this repo:

1. "`service.version` is set on the OTLP metrics resource" — WRONG for the **product** plane. `IdentityConfig.ProductIdentity()` (`internal/config/config.go:254`) stamps exactly three keys on every emitted product resource: `service.name`, `service.namespace`, `deployment.environment.name`. There is no `service.version`. So no portkey or langsmith series can carry it.

2. `service.version` exists **only on the two self-observability planes** — `internal/selfobs/provider.go:50` (metrics) and `internal/selfobs/tracing.go:51` (traces), set deliberately by `#91` so an operator can correlate a regression to a build. The blast radius is therefore the `genai_otel_bridge_*` self series only, not the product series.

3. "it is promoted to a `service_version` label on every emitted metric series" — **UNVERIFIED for this ingest path.** Whether a resource attribute becomes a per-series label is decided by the OTLP receiver, not by us: Mimir's default sends resource attributes to `target_info` and promotes only `service.name`/`service.namespace` to `job` and `service.instance.id` to `instance`. A per-series `service_version` label only appears if the tenant's `promote_resource_attributes` includes it. Nobody has looked.

**So the code half of this task is blocked on a live measurement, and the measurement is currently impossible:** as of 2026-09-15 no series on the m7kni stack carry `job="genai-otel-bridge"`, and as of 2026-09-22 the bridge is not emitting anywhere. Confirmed with Rob.

What remains doable with no live data is the documentation half — recording the OTLP-to-Prometheus resource-attribute convention in the telemetry docs so nobody reintroduces the misunderstanding. Everything else waits for a deployment.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 The OTLP-to-Prometheus resource-attribute convention is documented in docs/telemetry.md: only service.name/service.namespace map to job and service.instance.id to instance; every other resource attribute belongs on target_info, and promoting one to per-series labels is a non-default opt-in
- [x] #2 The docs state explicitly that service.version is set on the self-observability planes only (provider.go, tracing.go) and is absent from ProductIdentity(), so no product series can carry it
- [ ] #3 DEFERRED, needs a deployed binary: query the target stack for genai_otel_bridge_* series and record whether service_version is present as a per-series label or only on target_info. Record the answer in these notes either way
- [ ] #4 DEFERRED, conditional on AC#3 finding a real per-series label: stop the promotion and repoint any deploy/grafana query that filters on or aggregates over service_version at the info metric, with a documented group_left example
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [x] #1 just check
- [x] #2 just test-acceptance (only if a §9 acceptance seam changed)
<!-- DOD:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Wave 1 L8 docs half: document OTLP-to-Prometheus resource attribute handling and the self-observability-only scope of service.version; defer live AC#3 and conditional AC#4 to gob-0002 deployment evidence.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
GitHub issue **#165 was deleted on 2026-08-14** once this task took over the work — `gh issue view 165` now 404s. Do not go looking for it. The full original body (2,267 bytes, no comments) is in the archive:

```sh
jq ".[] | select(.number == 165)" archive/github-issues-2026-08-14.json
```

It is not in the closed-issues index doc either, because that table indexes the *closed* set and this one was open. The archive plus this task are the whole record.

The acceptance seam was unchanged, so the conditional acceptance-test update item is not applicable. AC3 and AC4 require deployed, emitting telemetry and remain deliberately unchecked.
<!-- SECTION:NOTES:END -->

## Comments

<!-- COMMENTS:BEGIN -->
author: @claude
created: 2026-09-22 09:07
---
Premise corrected during wave-1 goal authoring (2026-09-22). Probed the code directly: ProductIdentity() carries no service.version, so the product plane was never affected; service.version is self-obs-only (#91, deliberate). Whether it becomes a per-series label is an ingest-side question nobody has checked, and it cannot be checked because nothing is emitting. Dropped High to Medium: the cardinality harm the original issue asserted is unproven for this repo, and the only unblocked work is a docs note. Code half parked behind the probe below.
---
<!-- COMMENTS:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Documentation changes landed in a57fcca06766c6a7c572a0da1b4353dd498ac70c. Parked because no deployment or emitted telemetry exists in this wave. Resume after gob-0002 deploys: query the target Grafana stack for genai_otel_bridge_* and target_info; confirm service.version is absent from per-series labels and present on self-observability resource metadata. Stop promotion if either contract fails.
<!-- SECTION:FINAL_SUMMARY:END -->
