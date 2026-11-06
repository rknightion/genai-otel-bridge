---
id: GOB-0022
title: Flatten the e2e utility image for containerd-backed Docker
status: In Progress
assignee: []
created_date: '2026-11-06T20:22:00Z'
updated_date: '2026-11-06T20:24:00Z'
labels: []
dependencies: []
references:
  - https://github.com/containerd/containerd/issues/11344
priority: medium
type: bug
ordinal: 22000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The hosted e2e job imports app, helper and utility images together. A plain multi-platform utility-image pull under the containerd-backed Docker store saves an OCI index that references unavailable platform blobs; every node import fails and Helm waits for unavailable pods. The existing flattening workaround only runs in Docker-out-of-Docker mode, although hosted Docker now uses the same image store. Preserve the full failover and cleanup suite and its deadlines.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The utility image import works with a containerd-backed Docker store without requiring Docker-out-of-Docker mode.
- [ ] #2 The correction retains existing image versions, failover checks, cleanup checks and test deadlines.
- [ ] #3 A red/green transport proof and exact-candidate hosted e2e result establish the correction.
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 just check
- [ ] #2 just test-acceptance (only if a §9 acceptance seam changed)
<!-- DOD:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Reproduce the incomplete multi-platform utility image archive using a private local tag. Compare it with an archive produced by the existing flattening build. Make flattening independent of Docker placement, preserve image versions and full tests/deadlines, then obtain exact-candidate review and hosted CI proof before main publication.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
The same pinned utility-image source produced an incomplete OCI descriptor graph after plain pull/save: fifteen referenced platform descriptors were absent. The existing FROM flattening build produced an archive with zero absent descriptors. The hosted failure reported the matching containerd missing-content error on every node, then an unavailable deployment. Apply that existing flattening operation on both placement paths, without changing image versions, app builds, test semantics, failover coverage, cleanup checks or deadlines. Local source syntax and formatting are checked; exact-candidate combined review and hosted CI remain pending.
<!-- SECTION:NOTES:END -->
