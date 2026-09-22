---
id: GOB-0019
title: Relicense the project from AGPL-3.0-only to Apache-2.0
status: Done
assignee:
  - '@codex'
created_date: '2026-09-22 09:08'
updated_date: '2026-09-22 10:12'
labels:
  - licensing
  - release
dependencies:
  - GOB-0018
priority: high
type: chore
ordinal: 19000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Rob's decision, 2026-09-22: genai-otel-bridge becomes Apache-2.0. The AGPL clean-room fork at github.com/grafana-ps/aip-oi is being deprecated with users pointed at this project instead, so it needs nothing and must not be touched.

**The relicence is legally clean and this is the evidence, so nobody has to re-derive it.** `git log` carries exactly one human author (Rob Knight, 206 commits across three spellings of the same noreply address); everything else is rknightion-renovate[bot] and github-actions[bot] doing dependency bumps and changelog commits. No third-party contributions, no CLA to chase. There are zero `Provenance-includes-license` headers in the tree, so no file is derived from upstream code under other terms, and a scan of vendor/ found no GPL, LGPL, AGPL or MPL dependency.

**Surface, measured 2026-09-22.** 194 tracked `.go` files carry `// SPDX-License-Identifier: AGPL-3.0-only` on line 1. Outside those: LICENSE, LICENSING.md, README.md, CONTRIBUTING.md, docs/index.md, docs/security.md, docs/governance.md, .github/PULL_REQUEST_TEMPLATE.md, Dockerfile, Dockerfile.kaniko, deploy/helm/Chart.yaml, justfile, scripts/spdx-check.sh, scripts/notices.sh. `site/` is gitignored zensical build output with zero tracked files, so its AGPL strings are not in scope and regenerate themselves.

Re-derive the file count before trusting it; the number moves with every commit.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 LICENSE holds the verbatim Apache License 2.0 text and no AGPL text survives in it
- [x] #2 Every tracked .go file carries // SPDX-License-Identifier: Apache-2.0 on line 1, with the count re-derived at the time of the change rather than taken from the description
- [x] #3 just spdx-check enforces Apache-2.0 and passes: the header constant in scripts/spdx-check.sh and the recipe doc comment in the justfile both name Apache-2.0, so a stray AGPL header would fail the gate
- [x] #4 LICENSING.md is rewritten: Apache-2.0 as the repository licence, the combined-binary paragraph corrected (it currently says the binary is distributed under AGPL-3.0-only), and the provenance-header example's SPDX line updated
- [x] #5 Every prose surface names Apache-2.0: README.md, CONTRIBUTING.md, docs/index.md, docs/security.md, docs/governance.md, .github/PULL_REQUEST_TEMPLATE.md
- [x] #6 Distribution metadata updated: org.opencontainers.image.licenses in both Dockerfile and Dockerfile.kaniko, and artifacthub.io/license in deploy/helm/Chart.yaml
- [x] #7 FROZEN (Rob, 2026-09-22): no NOTICE file is created and no per-file copyright line is added. SPDX identifiers only, matching the tree's existing convention
- [x] #8 CHANGELOG.md's existing AGPL references are left untouched — they record what actually shipped under that licence and rewriting them would falsify the history
- [x] #9 just notices runs green and confirms no linked dependency is under GPL, LGPL, AGPL or MPL terms, with its output quoted as the evidence (depends on gob-0018)
- [x] #10 FROZEN (Rob, 2026-09-22): committed as feat: relicense from AGPL-3.0-only to Apache-2.0 so release-please cuts a minor and the change appears in the CHANGELOG
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [x] #1 just check
- [x] #2 just test-acceptance (only if a §9 acceptance seam changed)
- [x] #3 just check
- [x] #4 just test-acceptance (only if a §9 acceptance seam changed)
<!-- DOD:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Wave 1 L2+L3 atomic pair: update licence prose and metadata, then sweep every tracked non-vendor Go header and flip the SPDX gate before either commit is pushed.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
No acceptance seam changed, so the conditional acceptance-test items are not applicable.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Relicensed project metadata and prose in bfd95f4237392a2fb75f67e7d2cb6af0bb8186dc and changed all 191 tracked non-vendor Go source headers plus the SPDX gate in 63b432542bdeaaae697d664cc590fcd2324587a2. Notice generation was repaired in 3451473571206df2bb4a51da088973f9bef7f805. just spdx-check confirmed 191 Apache-2.0 headers; just notices generated 107 modules with only Apache-2.0, BSD-3-Clause, MIT, and ISC licenses; integrated just check passed.
<!-- SECTION:FINAL_SUMMARY:END -->
