---
id: GOB-0021
title: Retry Kubernetes 1.37.1 envtest update when upstream artifacts exist
status: Parked
assignee: []
created_date: '2026-10-21T15:14:00Z'
labels: []
dependencies: []
references:
  - 'https://github.com/rknightion/genai-otel-bridge/pull/246'
priority: medium
ordinal: 21000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Renovate PR #246 cannot pass envtest: CI run 39575235449 reports unable to find archive for 1.37.1 (linux,amd64). Canonical https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/main/envtest-releases.yaml lists 1.37.0 but not 1.37.1 as observed 2026-10-21. Keep existing test version and leave PR open until upstream publishes artifacts.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Canonical upstream envtest index contains Linux amd64 and arm64 artifacts for the proposed version
- [ ] #2 Refresh PR against current main and pass exact-head CI before merging
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 just check
- [ ] #2 just test-acceptance (only if a §9 acceptance seam changed)
<!-- DOD:END -->
