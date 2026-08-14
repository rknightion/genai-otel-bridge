---
id: GOB-0007
title: LangSmith bulk-export to S3 (Parquet)
status: To Do
assignee: []
created_date: '2026-08-14 16:12'
labels:
  - followup-v2
  - langsmith
  - new-surface
dependencies: []
references:
  - followup.md
priority: low
ordinal: 7000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Migrated from `followup.md` §4 (v2 stream) at the 2026-08-14 tracker migration.

A batch pipeline for full/regulated records and long-range history into customer-owned S3 — **the regulated-records path the content-free poller deliberately cannot serve.** This is the sanctioned answer to "we need the actual records", precisely so the OTLP path never becomes it.

High complexity, low risk. Needs a more-privileged LangSmith key than the polling path uses.

**Content-governance boundary:** this pipeline handles full records by design, and that is exactly why it must be a separate egress with a separate credential — nothing here may relax the guard, the denylist, or the "no LLM content to Grafana Cloud — ever" posture on the OTLP path. `#95` closed on content previews leaking into emitted logs through `extra_record_fields`; do not create a second route.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Records land in customer-owned S3 as Parquet, over a credential distinct from the polling key
- [ ] #2 No change to the OTLP path guard, denylist, or content posture — asserted by the existing *_content_gate_test.go guards still passing unmodified
- [ ] #3 The privilege difference between the two keys is documented on the security docs page
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 make gate
- [ ] #2 go test -tags acceptance ./internal/app/ (only if a §9 acceptance seam changed)
<!-- DOD:END -->
