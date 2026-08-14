---
id: doc-0003
title: Wave operating model
type: guide
created_date: '2026-08-14 16:08'
updated_date: '2026-08-14 16:10'
---
**This document restates nothing from the fan-out protocol doc.** Read that one for the campaign
model — run contract, routing, authority, lane briefs, goal-file template, pre-flight. This one
carries only what is true of `genai-otel-bridge` and would be wrong pasted into another repo.

## Rules this project added

Each exists because of a specific failure, recorded with it. None of these are in the protocol.

### The config surface is FOUR files and they move as one

A config knob is not added until it exists in all four:

| File | Role |
|---|---|
| `internal/config/config.go` | the schema, its `helm:` tags, and `Load`-time defaults |
| `deploy/helm/values.yaml` | the Kubernetes surface, knob at its default with a comment |
| `deploy/ecs/terraform/config.example.yaml` | the ECS surface |
| `test/eks/values-eks.yaml` | the live-cluster exercise config |

**The failure:** this drifted at least six separate times, in every direction. `#42` — the whole
`ha.dynamodb.*` block and the `dynamodb` enum value shipped with no public documentation at all.
`#48` — the ECS README taught operators a knob (`emit.retry_max`) that does not exist, and strict
decode means following the advice makes the binary *fail config load*. `#106` and `#105` — the
values comment and `docs/portkey.md` each described behaviour opposite to the code. `#113` —
`emit.telemetry.otlp metric_interval` was accepted by the schema and silently ignored. `#114` —
`queue.max_batches` / `max_batch_bytes` had no `Load`-time defaults, so any non-Helm config silently
ran at depth 1 with no proactive split.

**Two of these are one lane's job, never split across lanes**, because a lane that owns three of
four files ships the fourth as someone else's problem and nobody notices.

### A knob that is accepted must be validated, and a validated knob must fail loudly

The recurring shape is *config accepted, behaviour silently wrong* — the worst failure mode this
codebase has, because the operator gets a clean startup. `#40` typo'd loop names silently ignored.
`#102` `session_label_value` typo silently falls back to `id`. `#39` omitted `rate_limit` passes
validation and makes every upstream request fail instantly on burst 0. `#38` negative durations
pass, and a negative `bucket_settle` emits unsettled buckets, breaking emit-once-after-settle.
`#52` `bucket_settle` had no code default and `0` was accepted. `#57` the analytics loop silently
no-ops *forever* when `bootstrap_lookback <= bucket_settle`. `#41` an `emit.self` block with an
empty endpoint validates and breaks the whole self-telemetry plane at runtime.

**A lane adding or touching a knob owes a `Validate` case and a test that asserts the rejection**,
not just the happy path. "It's covered by the schema" is how every one of the above shipped.

### Doc-vs-code drift is a defect class here, not an untidiness

The single largest category in the closed set — roughly a quarter of it. `#135`, `#131`, `#118`,
`#115`, `#100`, `#97`, `#89`, `#85`, `#79`, `#78`, `#77`, `#76`, `#67`, `#58`, `#55`, `#44`, `#43`,
`#36`. The docs did not lag; they asserted things that were *never* true — `#131`, docs describing a
UUID-shape heuristic inside the guard that exists nowhere in `internal/source`; `#136`, `DESIGN` §8
claiming a golden-file OTLP determinism test that does not exist; `#76`, `ARCHITECTURE.md` §11
listing self-observability metric names absent from the code while omitting the real ones.

**So: a lane's diff is not done when the code is green.** Every behaviour change sweeps the surfaces
that describe it — the package `CLAUDE.md`, `ARCHITECTURE.md`, `docs/DESIGN.md`, the `docs/*.md`
page, the `values.yaml` comment, and any alert or dashboard referencing the metric name. `#44` and
`#79` are the cheap end of this (an alert doc citing an untranslated metric name; a PromQL selector
in the troubleshooting guide that matches nothing) and they still reached a user-facing page.

### Twelve package `CLAUDE.md` files — and a mixed-harness trap

`internal/{app,checkpoint,config,coordinate,emit,httpx,model,schedule,selfobs,source}`, plus
`source/portkey` and `source/langsmith`. These are the nearest thing to a spec for each seam and
they are where drift concentrates (`#135` a stale FROZEN constructor signature, `#118` a fence
description omitting the cursor relaxation and the whole `dynamodb` backend, `#100` a package doc
omitting the `usage` loop and carrying a default of 50 against the code's 100).

**The trap: Claude Code loads nested `CLAUDE.md` recursively, Codex reads `AGENTS.md` only.** The
root file is `AGENTS.md` (with `CLAUDE.md` importing it), so both harnesses get the root contract —
but a Codex lane gets *none* of the twelve package files. Give a Codex lane the package doc's path
in its brief explicitly; do not assume it was loaded.

### Governance is default-deny, and it has been weakened four times through a side path

Never by someone editing the denylist. `#130` — `extra_record_fields` on *any* enabled loop
subtracted gray keys from the one shared guard denylist for *every* loop. `#132`/`#75` — the label
allow-list unioned both vendors' keys regardless of which source was enabled, contradicting the
adjacent comment. `#97` — the `AbsoluteNeverDenyKeys` floor was exact-match while the docs claimed
`gen_ai.*` prefix coverage, so prefix variants bypassed *both* deny layers. `#95` —
`extra_record_fields` could opt LLM-content previews (`inputs_preview`/`outputs_preview`) into
emitted logs, against the settled "no LLM content to Grafana Cloud — ever" posture.

**Any diff touching `internal/source/guard.go`, the denylist wiring in `internal/app`, or an
`extra_*_fields` path is a content-egress change** and gets reviewed as one regardless of how small
it looks. The `*_content_gate_test.go` files in `internal/app` are the regression guards; extend
them, never relax them.

### Parallel implementations must be diffed against each other, not just tested

Two backends exist for both HA seams (`checkpoint/{configmap,dynamodb}`, `coordinate/{lease,dynamodb}`)
and two planes for the pipeline (metrics, logs). They drift silently. `#116` — RMW retry budgets of
6 vs 5 while the `dynamodb` package doc claimed it mirrored the ConfigMap path exactly. `#110` — the
lease coordinator exits the process on leadership loss while the DynamoDB one re-campaigns
in-process, undocumented. `#137` — payload splitting tested on the logs plane only. `#134` —
`race_test.go` covered the samples `Sanitize` path but not `SanitizeLogs`. `#117` — the configmap
backend's corrupt-value refusal had no test while the package doc claimed corruption was covered.

**A lane changing one backend or one plane states in its notes what it checked in the sibling**, even
if the answer is "sibling unaffected, here's why".

### `make gate` is the bar, and it is deliberately equal to CI

`make gate` = vet + test + lint + `forbidden-words` + `spdx-check` + `tf-validate` + `helm-lint` +
`build ./...`. It exists to equal CI's hygiene leg, and `#109` is why that equality is a rule: `make
ci` had been missing `tf-validate` while `make gate` was missing `helm-lint`, so the local "full CI
mirror" targets did not mirror CI. **If you add a CI leg, add it to the matching make target in the
same change.** Acceptance gates are separate and run on demand:
`go test -tags acceptance ./internal/app/`.

`forbidden-words` scans `backlog/` and `archive/` — they are tracked and not in `PRIVATE_PATHS`. On
a fork PR the real identifier list is absent and only credential shapes are scanned (`#108`), so a
green fork PR is *not* evidence the identifier sweep passed.

### Evidence, not assertion — and the tests that claim coverage are the ones to distrust

`#136`, `#137`, `#134`, `#117`, `#150`, `#138`, `#64` are all "a test or doc claims this is covered
and it is not". `#64` is the sharpest: the Portkey analytics HTTP fixtures were encoded *from the
decode struct itself*, so a JSON field-name regression could not fail any test. `#150` — an e2e
fence-evidence assertion was unreachable and red on main. **When a lane's brief says "it's already
tested", the lane verifies that the test would fail if the behaviour broke.**

## Exclusive resources — one lane at a time, named in the goal

| Resource | Why exclusive |
|---|---|
| the k3d cluster (`make k3d-up` / `k3d-e2e` / `k3d-down`) | one named cluster per machine; two lanes racing it produce failures that look like product bugs |
| the EKS test environment (`test/eks/`) | a real cluster and real spend |
| a live Portkey key | workspace scope is **key-bound**; Portkey ignores per-request workspace targeting on `/analytics/groups/*` (re-confirmed 2026-06-22, `followup.md` vX) |
| a live LangSmith key | the rate budget is **tenant-wide**, roughly 10 req/10s, shared across every loop and every lane |
| `.tools/` (`make tools`, `tools-e2e`) | pinned tooling installed into one directory; concurrent installs corrupt it |

**No lane calls a live vendor API without the goal naming it as that lane's exclusive resource.** No
test does it at all — `httptest.Server` fakes and injectable clocks (`SetLoopClockForTest`) are the
only in-test path.

## Ownership

**One file, one owner** is the protocol's rule; these are this repo's specific single-owner files,
which a wave assigns to exactly one lane or defers to a wiring pass:

- `internal/app/app.go` — the composition root. Wiring only; every lane wants to touch it.
- the four config-surface files above — one lane, together.
- `internal/source/*/signals.go` + `labels.go` — `SeriesNames` and `ValidateOwnership` are the only
  cross-loop series-collision gate (`#63`, `#104`). Two lanes editing these independently is how a
  collision ships.
- `Makefile` + `.github/workflows/ci.yml` — the gate matrix, per `#109`.
- `ARCHITECTURE.md`, `docs/DESIGN.md`, `followup.md` — the durable design record, appended by the
  wiring pass, not by every lane.

**FROZEN seams** (`model.*` types, `source.Source` / `source.Loop`) are frozen in the protocol's
sense: changing one is a design change requiring an `ARCHITECTURE.md` ledger entry, decided *before*
fan-out, never inside a lane. A lane that believes it needs a frozen-seam change has hit a stop
condition — see below.

**`*_review_test.go` and review-tagged assertions are regression guards, not scaffolding.** Tags like
`[CP-M7]`, `[CP-C1]`, `[ext-review-14]`, `[Cdx-M5]` encode a specific adversarial-review finding. A
lane may extend them; a lane that needs to *delete* one is changing a settled decision and stops.

### The escape hatch

A lane that hits a boundary **returns the question and keeps going on everything the answer does not
block**. It does not invent an answer, and it does not stop the whole lane on one blocked item.

Concretely, a lane stops and asks when it needs: a FROZEN-seam change; a review-guard assertion
deleted; a new emitted label key or series name (a cardinality decision); anything that could put
vendor content on the wire; a live vendor key it was not given; or a `main` push (only the root
agent commits — see below).

Everything else it decides itself and records the decision in its task notes. **A boundary with no
escape hatch is a stop condition wearing a safety label** — if a lane cannot both ask and continue,
the wave was scoped wrong.

## Run-end against this tracker

- Landed work: `Done`, **with the commit SHA in the final summary**. The index doc's commits column
  exists because past work had to be reconstructed by grepping the log — do not recreate that debt.
- Blocked work: `Parked`, with a **concrete resume boundary** — the file and line, the question, and
  what has already been ruled out. "Blocked on review" is not a resume boundary.
- Untouched work is self-evidently still `To Do`. Do not annotate it.
- Discovered work: a new task labelled `needs-triage`. This repo generates a lot of it — the closed
  set is overwhelmingly review findings that spawned other review findings.
- **Commits go to `main` directly** (no feature branches, no PRs), `make gate` green first, staging
  **explicit paths** — never `git add -A` or `-a`, because concurrent lanes share the working tree.
  Renovate is the only exception: it opens PRs and self-automerges on green `ci-success`.
- Cite closed pre-migration work as `#NNN` (the index doc), new work as `gob-NNNN`. Never renumber.
