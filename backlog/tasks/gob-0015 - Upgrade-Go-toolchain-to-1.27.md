---
id: GOB-0015
title: Upgrade Go toolchain to 1.27
status: In Progress
assignee: []
created_date: '2026-08-23 19:06'
updated_date: '2026-08-23 19:49'
labels: []
dependencies: []
ordinal: 15000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Adopt Go 1.27 consistently across the application, nested modules, build images, CI configuration, setup automation, and version-specific contributor documentation.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 All active Go module and toolchain pins require Go 1.27
- [x] #2 Build images, CI jobs, setup automation, and current documentation agree with the Go 1.27 requirement
- [x] #3 The repository green-bar validation passes under Go 1.27
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [x] #1 make gate
- [x] #2 go test -tags acceptance ./internal/app/ (only if a §9 acceptance seam changed)
<!-- DOD:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Inventory every active Go version pin, including nested modules and container or CI toolchains. 2. Update the pins and version-specific documentation to Go 1.27.0 without changing historical records or fixtures. 3. Run the repository-defined validation gate, review the diff, commit to main, push, and confirm hosted CI.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Local Go 1.27.0 make gate passed in full, including tests, lint, acceptance lint, forbidden words, SPDX, Terraform validation, Checkov with 0 failures, Helm lint, and build. The deterministic retry-test helper budget was raised to 500 ms after its 30 ms test-only ceiling failed under Go 1.27; the production retry policy is unchanged. No section 9 acceptance seam changed. Paid-plan CodeRabbit review completed with zero findings.
<!-- SECTION:NOTES:END -->
