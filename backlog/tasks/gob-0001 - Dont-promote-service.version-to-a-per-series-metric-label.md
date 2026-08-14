---
id: GOB-0001
title: Don't promote service.version to a per-series metric label
status: To Do
assignee: []
created_date: '2026-08-14 16:10'
updated_date: '2026-08-14 16:51'
labels:
  - telemetry
  - cardinality
  - from-gh-165
dependencies: []
references:
  - 'https://github.com/rknightion/graph2otel/issues/104'
documentation:
  - archive/github-issues-2026-08-14.json
priority: high
ordinal: 1000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Migrated from GitHub issue **#165** (open at the 2026-08-14 tracker migration; full original body in `archive/github-issues-2026-08-14.json`).

`service.version` is set on the OTLP metrics resource (the MeterProvider's `Resource`), so it is promoted to a `service_version` label on **every emitted metric series**. Per the OpenTelemetry → Prometheus compatibility spec that is a deviation: only `service.name` (+ `service.namespace`) → `job` and `service.instance.id` → `instance` are meant to become labels. Every other resource attribute — `service.version` included — belongs on an info metric (`target_info`, or a `*_build_info` gauge), per the OpenMetrics 1.0 convention. Promoting a resource attribute to per-series labels is a documented, non-default opt-in.

**Impact:** each new build mints a whole new series set. After a redeploy the old- and new-version series coexist for the query-lookback window, so any `sum`-style panel adds both (a transient multiplier), and active-series cardinality grows with the number of versions ever seen. Repos on stable release tags hit the cardinality growth but rarely see the doubling.

**Shape of the fix:** keep `service.version` on the OTel resource (semconv-correct, flows to `target_info`); stop it becoming a per-series label; rely on the existing `*_build_info{version=...}` gauge joined via `group_left` where a panel needs it.

Cross-repo consistency pass — the detailed spec citations are in `rknightion/graph2otel#104`. Sibling issues: `tailscale2otel#187`, `opnsense-exporter#270`.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 service_version no longer appears as a label on ordinary metric series (only on target_info / *_build_info)
- [ ] #2 Version stays queryable via the build-info gauge, with a documented group_left example
- [ ] #3 Dashboards and alerts under deploy/grafana/ audited; any query aggregating over or filtering on service_version is repointed at the info metric
- [ ] #4 The OTLP-to-Prometheus resource-attribute convention is noted in the telemetry setup docs
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 make gate
- [ ] #2 go test -tags acceptance ./internal/app/ (only if a §9 acceptance seam changed)
<!-- DOD:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
GitHub issue **#165 was deleted on 2026-08-14** once this task took over the work — `gh issue view 165` now 404s. Do not go looking for it. The full original body (2,267 bytes, no comments) is in the archive:

```sh
jq ".[] | select(.number == 165)" archive/github-issues-2026-08-14.json
```

It is not in the closed-issues index doc either, because that table indexes the *closed* set and this one was open. The archive plus this task are the whole record.
<!-- SECTION:NOTES:END -->
