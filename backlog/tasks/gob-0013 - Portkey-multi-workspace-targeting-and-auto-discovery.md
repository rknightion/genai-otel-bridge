---
id: GOB-0013
title: Portkey multi-workspace targeting and auto-discovery
status: Parked
assignee: []
created_date: '2026-08-14 16:13'
updated_date: '2026-08-14 16:13'
labels:
  - followup-vx
  - portkey
  - blocked-on-vendor
dependencies: []
references:
  - followup.md
priority: low
ordinal: 13000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Migrated from `followup.md` §4 (vX stream — blocked on an external API surface) at the 2026-08-14 tracker migration. **Parked, not To Do:** this was attempted and is blocked by the vendor, not by us.

**Goal:** emit all workspaces under one admin key, with TTL-refresh discovery, instead of one deployment per workspace key.

**What is already BUILT:** the safety guardrail `expected_workspace`.

**Why it is blocked:** Portkey **ignores per-request workspace targeting on `/analytics/groups/*`** — the scope is key-bound. Re-confirmed on a live instance 2026-06-22. So multi-workspace stays key-per-workspace, and no amount of client-side work changes that.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 One admin key emits all workspaces, with TTL-refresh discovery
- [ ] #2 expected_workspace remains enforced as the safety guardrail, per source instance
- [ ] #3 Cardinality impact of a workspace dimension across N workspaces is stated before build
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [ ] #1 make gate
- [ ] #2 go test -tags acceptance ./internal/app/ (only if a §9 acceptance seam changed)
<!-- DOD:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
RESUME BOUNDARY (set at the 2026-08-14 tracker migration; the block is the vendor, not this repo).

**The exact probe that unparks this — an agent can run it in minutes.** Against a live Portkey instance, call `/analytics/groups/*` twice with identical parameters, once with the `x-portkey-workspace` header (or a `workspace_id` query param) set to a *different* workspace than the key is bound to, and once without.

**The question is not whether it returns 200 — it does. It is whether the returned ROW SET CHANGES.**

- Row set unchanged  ⇒ still blocked. Update the re-confirmation date here and leave Parked.
- Row set changes    ⇒ the fan-out becomes buildable. Move to To Do and size it (Med-High complexity, Med risk).

Last probed: **2026-06-22 — row set unchanged, targeting ignored, scope key-bound.**

Do not attempt a client-side workaround. Iterating keys per workspace is the current supported shape and is already what deployments do.
<!-- SECTION:NOTES:END -->
