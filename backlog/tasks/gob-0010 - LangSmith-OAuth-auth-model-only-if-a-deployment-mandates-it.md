---
id: GOB-0010
title: LangSmith OAuth auth model (only if a deployment mandates it)
status: To Do
assignee: []
created_date: '2026-08-14 16:12'
labels:
  - followup-v3
  - langsmith
  - auth
dependencies: []
references:
  - followup.md
priority: low
ordinal: 10000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Migrated from `followup.md` §9 and §3 (v3 stream) at the 2026-08-14 tracker migration.

**N/A for the built path, and that is the point of tracking it.** genai-otel-bridge uses static LangSmith service API keys (`lsv2_sk_` plus `X-Tenant-Id`), not OAuth sessions, so there is no session-refresh concern today. The original design analysis flagged an OAuth session-TTL conflict (86400 vs 7200) that would need designing around for token refresh.

Captured so the gap is not re-discovered from scratch: **if a deployment ever mandates OAuth sessions rather than service keys**, token-refresh handling has to be built into the langsmith source, on the critical-path poller. Medium complexity, medium risk — refresh on a poller that must not stall is the risky part.

Do not build speculatively.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A deployment actually mandating OAuth exists before build starts — otherwise this stays To Do as a captured boundary
- [ ] #2 If built: token refresh never stalls or silently degrades a loop, and a refresh failure is alertable rather than silent
- [ ] #3 The static-service-key path remains supported and default
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 make gate
- [ ] #2 go test -tags acceptance ./internal/app/ (only if a §9 acceptance seam changed)
<!-- DOD:END -->
