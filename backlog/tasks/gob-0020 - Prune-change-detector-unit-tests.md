---
id: GOB-0020
title: Prune change-detector unit tests
status: To Do
assignee: []
created_date: '2026-09-25 08:05'
labels:
  - testing
dependencies: []
ordinal: 20000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
From the 2026-09-25 fleet test-signal audit (sampled read-only). Delete or consolidate tautological tests (restating the implementation) and change-detector tests (pinning incidental text, markup, counts or internals). Keep parsing, state-machine, retry, security/PII, wire-contract and incident regression tests. Re-verify each candidate before deleting it; the list below comes from a sample and is not exhaustive. Candidates: internal/version/version_test.go:7 (only checks the string is non-empty); internal/app/labels_test.go:28 (hardcoded label-key union list; keep only if it is a documented contract). Keep the generated-doc drift gates (helm_generated_test.go, ecs_config_generated_test.go); they are CI drift checks, not logic tests. Suite was about 82% valuable, so the task is small.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Each listed candidate is deleted, consolidated or kept with a one-line reason in the notes
- [ ] #2 Other tests in the same pattern found during the work are handled the same way
- [ ] #3 The repo's check recipe passes
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 just check
- [ ] #2 just test-acceptance (only if a §9 acceptance seam changed)
<!-- DOD:END -->
