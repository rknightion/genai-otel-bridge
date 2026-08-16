---
id: GOB-0014
title: Add Codex Cloud manual environment setup
status: Done
assignee:
  - '@codex'
created_date: '2026-08-16 10:27'
updated_date: '2026-08-16 11:28'
labels: []
dependencies: []
references:
  - 'https://learn.chatgpt.com/docs/environments/cloud-environment#manual-setup'
  - 'https://code.claude.com/docs/en/cloud-environments#setup-scripts'
type: chore
ordinal: 14000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Provide a repository-owned, idempotent setup script for Codex Cloud tasks so manual environment setup installs the task tracker and the complete validation toolchain required by this repository.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 The setup installs the pinned Backlog.md CLI and makes the backlog command available to cloud task agents
- [x] #2 The setup prepares Go dependencies and every tool needed by the documented make gate without relying on automatic setup
- [x] #3 The setup is safe to rerun in a cached Codex Cloud container and documents how to configure it
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [x] #1 make gate
- [x] #2 go test -tags acceptance ./internal/app/ (only if a §9 acceptance seam changed)
<!-- DOD:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Add an idempotent Codex Cloud setup script that persists PATH changes, installs the pinned Backlog.md CLI and IaC validators, and prefetches repository Go and Makefile-managed tooling.

2. Document the one-line manual setup configuration and validate reruns plus the full repository gate.

3. Rename the entry point to scripts/cloud-environment-setup.sh, add the required local-agent warning at the top, and document the same script for Codex Cloud and Claude Code cloud environments.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Validated the setup from a populated cached-container state: it completed successfully, reported Backlog.md 1.50.1, OpenTofu 1.10.6, TFLint 0.59.1, and Checkov 3.2.495, and persisted PATH entries in ~/.bashrc. The subsequent make gate passed.

Follow-up validation passed: bash -n scripts/cloud-environment-setup.sh, git diff --check, and make gate. The setup entry point was intentionally not executed because its new header instructs non-cloud agents not to run it.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Added and documented the idempotent Codex Cloud manual setup. It installs Backlog.md and the full gate toolchain, prefetches dependencies/assets for restricted-network agent execution, and was verified by rerunning the setup plus make gate.

Follow-up renamed the entry point to scripts/cloud-environment-setup.sh, added the required local-agent warning, made apt installation work both as Claude cloud root and as a sudo-capable Codex user, and documented configuration for both platforms.
<!-- SECTION:FINAL_SUMMARY:END -->
