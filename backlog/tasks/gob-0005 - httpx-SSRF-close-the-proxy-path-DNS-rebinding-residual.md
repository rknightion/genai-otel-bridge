---
id: GOB-0005
title: 'httpx SSRF: close the proxy-path DNS-rebinding residual'
status: Done
assignee:
  - '@codex'
created_date: '2026-08-14 16:11'
updated_date: '2026-09-22 10:12'
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
- [x] #1 Either the proxy-path rebinding race is closed (pinned resolution or proxy-side egress policy), or it is documented as accepted with the precise conditions under which it is reachable
- [x] #2 The direct dial path is left authoritative and unchanged
- [x] #3 Any egress-policy guidance change is checked against the logs_export signed-URL S3 download (#128)
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [x] #1 just check
- [x] #2 just test-acceptance (only if a §9 acceptance seam changed)
<!-- DOD:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Wave 1 L7: assess the proxy-only DNS-rebinding residual and either close it or document exact accepted-risk reachability without changing the authoritative direct dial path.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
The acceptance seam was unchanged, so the conditional acceptance-test update item is not applicable.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Documented the accepted proxy-path DNS rebinding residual and exact network-policy reachability requirements in db23fbcbd8bcca74dc7276cf9cfe210c0a055e5a, with the deployment wiring examples corrected in a57fcca06766c6a7c572a0da1b4353dd498ac70c. Direct HTTP transport behavior was unchanged. Targeted HTTPX tests and integrated just check passed.
<!-- SECTION:FINAL_SUMMARY:END -->
