---
id: GOB-0005
title: 'httpx SSRF: close the proxy-path DNS-rebinding residual'
status: To Do
assignee: []
created_date: '2026-08-14 16:11'
labels:
  - followup-v2
  - security
  - httpx
dependencies: []
references:
  - followup.md
priority: medium
ordinal: 5000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Migrated from `followup.md` §8 (v2 stream) at the 2026-08-14 tracker migration.

**Scope this narrowly — the direct dial path is already fully guarded** (confirmed 2026-06-22, and `#96` separately closed the unspecified-address `0.0.0.0` / `::` hole). `httpx.checkDest` is exact for IP-literal hosts and resolves-then-checks hostnames.

The residual is only reachable **when an HTTP(S)_PROXY is configured**: the *proxy* resolves the hostname, so a DNS-rebinding race between our check and the proxy's resolution cannot be fully closed in-process. Closing it means pinning resolution or pushing egress policy to the proxy itself.

Worth doing only if a deployment actually uses a proxy. Note `#128` for the adjacent trap: the `sourceEgressCIDR` guidance ignores the Portkey `logs_export` signed-URL S3 download, so tightening egress per the values comment stalls the logs loop — any egress-policy change here must not repeat that.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Either the proxy-path rebinding race is closed (pinned resolution or proxy-side egress policy), or it is documented as accepted with the precise conditions under which it is reachable
- [ ] #2 The direct dial path is left authoritative and unchanged
- [ ] #3 Any egress-policy guidance change is checked against the logs_export signed-URL S3 download (#128)
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 make gate
- [ ] #2 go test -tags acceptance ./internal/app/ (only if a §9 acceptance seam changed)
<!-- DOD:END -->
