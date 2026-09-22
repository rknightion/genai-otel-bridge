---
id: GOB-0018
title: >-
  Fix the go-licenses module path so just notices and the publish notices job
  stop failing
status: To Do
assignee: []
created_date: '2026-09-22 09:08'
labels:
  - ci
  - release
  - licensing
dependencies: []
priority: high
type: bug
ordinal: 18000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
`justfile:337` installs `github.com/google/go-licenses@{{ go_licenses_version }}` while `justfile:16` pins `go_licenses_version := env('GO_LICENSES_VERSION', 'v2.0.1')`. Go refuses that combination because a post-v1 module must be imported at its majored path:

    go: github.com/google/go-licenses@v2.0.1: invalid version: go.mod has post-v2 module path "github.com/google/go-licenses/v2" at revision v2.0.1

So `_tools-licensing` exits 1, `just notices` cannot run at all, and the `publish / notices` job fails. Reproduced locally 2026-09-22 and observed in run 35631028751 (workflow auto-rc, tag v3.1.0-rc.78, HEAD 8037495), whose only failing step was 'Generate + attach third-party notices'.

The cause is a Renovate bump across a major boundary: the version variable is annotated `# renovate: datasource=github-releases depName=google/go-licenses`, so Renovate raised v1 to v2.0.1 without knowing the install path encodes the major. Nothing in the repo couples the two, so the same break recurs at v3.

This blocks more than releases: LICENSING.md makes `just notices` the mechanism that proves what the shipped binary actually links, so no licensing claim about the dependency graph can be verified while it is red.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 just notices completes and writes THIRD_PARTY_NOTICES.md
- [ ] #2 The install path matches the pinned major (github.com/google/go-licenses/v2 for a v2.x pin)
- [ ] #3 scripts/notices.sh is verified against go-licenses v2's actual CLI surface rather than assumed unchanged; v2 is a major release and its flags may have moved
- [ ] #4 A future Renovate major bump cannot silently break this again: either the path is derived from the pinned version, or the coupling is stated where the pin lives so a bump that needs a path change fails loudly instead of at release time
- [ ] #5 The previously failing publish / notices job passes at the tested SHA, cited by run ID
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 just check
- [ ] #2 just test-acceptance (only if a §9 acceptance seam changed)
- [ ] #3 just check
- [ ] #4 just notices exits 0 and its output is quoted as evidence
<!-- DOD:END -->
