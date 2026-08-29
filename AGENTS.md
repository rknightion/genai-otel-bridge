# genai-otel-bridge

Vendor-neutral Go service that polls AI-platform APIs (LLM gateways like Portkey, eval platforms
like LangSmith) and emits **operational** telemetry to Grafana Cloud as OTLP metrics/logs. Sits on
the production observability critical path: leader-elected, single-emit, self-observing, resilient
to downstream slowness.

> **Status:** the integrator is feature-complete across **both vendors × both planes** and green.
> Portkey: `analytics` + `groups` → OTLP metrics, `logs_export` → OTLP logs. LangSmith: `sessions`/eval
> → OTLP metrics, `usage` (platform cost-driver metrics) → OTLP metrics, `runs` → OTLP logs. Plus
> composition root, binary, HA, checkpointing, acceptance tests, hardened Helm chart, configurable
> content governance, and durability tuning (metrics `max_backfill` 90m ≤ Mimir 2h OOO; logs 24h ≤
> Loki 7d; too-old honesty path built+tested both planes). Every Portkey + LangSmith settings knob is
> surfaced in `values.yaml` at its default with a comment.
> **ECS deployment target:** runs production-grade on AWS ECS as well as Kubernetes — **DynamoDB-for-both**
> HA behind the frozen seams (`ha.coordinator=dynamodb` + `ha.checkpoint=dynamodb` + the `ha.dynamodb.*`
> block; one table backs the CAS lock + the checkpoint), a reusable Terraform module in
> `deploy/ecs/terraform/` (Fargate default, EC2 via `launch_type`), a `-healthcheck` binary mode, and a
> `dynamodb-local` CI gate (`tf-validate` + `dynamodb-backends` job). Merged to `main` via PR #13
> (2026-06-28; squash-merged as commit f6f0d61) plus follow-up commits — this was built on a branch+PR
> by exception to the usual direct-to-main convention (a big OSS seam change), but the feature itself is
> shipped, not in flight. See `ARCHITECTURE.md` decision ledger #17.
> **CORRECTION 2026-08-14:** this block used to say "In flight (3 lanes)" and name in-cluster
> cleartext-emit opt-out (`emit.*.otlp.allow_insecure`), the Portkey `groups` cost/metadata flags, and
> config-driven indexed/stream-label opt-in (`governance.allow_label_keys`). **All three shipped** —
> verified in `internal/config/config.go`, `deploy/helm/values.yaml` and `internal/source/portkey/
> signals.go`. Nothing is in flight; **what is left is the board** (`backlog task list --plain`).
> **Grafana-staff actions (not code blockers, the maintainer files them, deliberately NOT tracker
> tasks):** GS1 = Loki stream-label promotion of the logs loops' indexed attrs (until done they land as
> structured metadata, not queryable as `{label=…}`); GS2 = widen the backend accept window for long
> outages. See `docs/DESIGN.md` §7/RP2, `followup.md`.

## Task interface

This repo's task surface is a `justfile`. Discover it, don't guess it:

    just --list                        # human-readable
    just --dump --dump-format json     # machine-readable
    just --show <recipe>               # what a recipe actually runs

- `just check` is the full bare-toolchain gate and must pass before you commit.
- `just ci` additionally runs the Docker/service-container legs (`e2e` and
  `test-dynamodb`) that CI runs in separate jobs.
- Prefer `just <recipe>` over the underlying tool. If you are typing `go test`, you want `just test`.
- Run `just` with stdin from /dev/null. Recipes marked `[confirm]` are destructive — stop and ask
  before running one; never pass `--yes` or `JUST_YES=1`.
- If a task you need does not exist, add a recipe with a `#` doc comment and a `[group(...)]`
  rather than running a bare command.

Go 1.27. Module path: `github.com/rknightion/genai-otel-bridge`. GitHub (`rknightion/genai-otel-bridge`)
is the canonical remote — commit to `main`; releases are cut by merging release-please's PR (see Release).
The repo is public; a self-hosted Forgejo mirror is kept as a cold archive only. `main` is branch-
protected to require the `ci-success` check (with `enforce_admins=false`, so admin direct-to-main
pushes bypass it — the gate exists to hold Renovate's automerge until CI is green).

## Architecture (the seams)

Data flows: **Source.Loop.Collect** (pull bounded window forward from watermark) → `model.Batch`
→ `source.Guard.Sanitize` (cardinality/content governance) → `schedule.LoopRunner` (single-flight,
bounded queue, epoch-fenced checkpoint) → `emit.Emitter` (deterministic OTLP encode + retry).

`internal/` packages, with their own CLAUDE.md where the detail matters:
- `model/` — **FROZEN** vendor-neutral types (Sample, LogRecord, Batch, Watermark, CheckpointKey).
- `source/` (+ `portkey/`) — Source/Loop interface + registry + cardinality Guard.
- `emit/` (+ `otlp/`) — Emitter seam, reject taxonomy, hand-rolled deterministic OTLP protobuf.
- `schedule/` — per-loop tick→collect→enqueue→emit driver; the watermark-advance state machine.
- `checkpoint/` (+ `configmap/`, `file/`, `dynamodb/`) — durable watermark store + monotonic/epoch write fence (`dynamodb/` = the ECS backend; RMW + `CheckMonotonic`, RFC3339Nano time).
- `coordinate/` (+ `lease/`, `dynamodb/`) — leader election; single-active-replica (`dynamodb/` = the ECS backend; CAS lock + monotonic `fence` epoch).
- `httpx/` — hardened outbound client (SSRF egress guard, cross-host redirect block).
- `config/` — YAML config model, secret substitution, validation.
- `selfobs/` — the integrator's own metrics + health endpoints (distinct resource identity).
- `app/` — composition root (wiring only); `cmd/genai-otel-bridge/` — the binary.

Full design: `ARCHITECTURE.md` (durable seams, decision ledger §16), `docs/DESIGN.md` (build spec,
F1–F47 failure handling, review dispositions). Read these before changing a seam.

## Hard rules (from §2 + two adversarial reviews — do not violate)

- **Decoupled.** No vendor/customer/domain knowledge in core code or defaults. Metric names, label
  keys, endpoints, cadences, windows, env identifiers are all config. Vendor code lives only in its
  `source/<vendor>` package behind the common interface.
- **Content minimisation is a release gate, not a nicety.** Never request message bodies/prompts/
  completions. An outbound field allow/deny-list governs *every* emitted field (labels, log body,
  metadata) — content cannot leak via non-label fields. `internal/app` wires the denylist; `source.Guard`
  enforces label allow-listing (default-deny: empty allow-list denies all labels).
- **Conditional gap-free is engineered, not assumed.** It emerges from emit-once-after-settle +
  deterministic byte-identical encoding + monotonic, lease-epoch-fenced checkpoint writes. Don't
  weaken any of these expecting the rest to compensate.
- **Operationally honest.** Every polling/emit gap or skipped sample is alertable (`window_lag`,
  `samples_skipped_total`, etc.) — never silent. A non-retryable reject *advances past* the bad bucket
  with a counted gap (the loop always progresses); it never silently stalls or silently drops.
- **FROZEN seams.** `model.*` types and `source.Source`/`source.Loop` are marked FROZEN — adding/
  renaming fields or methods is a design change requiring an ARCHITECTURE.md update, not a casual edit.

## Conventions

- **Git workflow: direct to `main`.** Commit straight to `main` — no feature branches, no PRs.
  `just check` green before *every* commit (evidence, not assertion). Stage
  explicit paths (`git add <path>`), never `-A`/`.` — concurrent agents may share the working tree;
  never stage, commit, or revert work that isn't yours. *Exception:* Renovate dependency bumps open
  PRs and self-automerge (including majors) once the full CI suite is green — see `renovate.json`.
- **CI fans out** (`.github/workflows/ci.yml`): `just check` is split into a parallel `gate` matrix
  (build-vet / lint / test / race / acceptance / envtest / hygiene) plus `e2e` and `secret-scan`;
  the `ci-success` aggregator job is the single check that gates Renovate automerge and `publish`.
- **Conventional Commits** (`feat:`/`fix:`/`chore:`/`docs:`/`refactor:`/…) — subjects drive the
  release-please-generated `CHANGELOG.md`; only `feat`/`fix`/breaking bump the version, `chore`/`style`/
  `test` are hidden from the changelog. See the Release section below.
- **Gate extras:** `just check` runs `forbidden-words` (a content/decoupling guard — self-skips where its
  script isn't present), `spdx-check` (every `.go` carries the AGPL-3.0-only SPDX header), `tf-validate`
  (ECS Terraform fmt/validate/tflint/checkov — self-skips absent tools), and `helm-lint` (self-installs
  `helm` via `tools-e2e`) — matching every leg of ci.yml's hygiene job, so a local green gate implies a
  green hygiene leg too.
- **Strict TDD.** Failing test → minimal code → green. Table-driven where it fits; `httptest.Server`
  fakes for HTTP; injectable clocks (`SetLoopClockForTest`) for determinism. No live network in tests.
- **`*_review_test.go`** files encode specific adversarial-review findings (tagged like `[ext-review-14]`,
  `CP-R3b`, `Cdx-C14`). Keep them; they are regression guards for known attack/race scenarios.
- Scratch specs/plans live in **`docs/superpowers/` (gitignored)**. The durable spec is `docs/DESIGN.md`
  (tracked). Move anything build-affecting out of scratch into tracked docs.
- Secrets never go in git: `.env`, `*.local.yaml`, `*.secret.*` are gitignored. Config resolves
  `${ENV}` / `file:/path` refs at load time.

## Release

Releases are automated by **release-please** (`.github/workflows/release-please.yml`). On every push to
`main` it maintains a "release PR" that, from the Conventional Commits since the last release, computes
the next semver and updates `CHANGELOG.md` + `deploy/helm/Chart.yaml` (`version` + `appVersion`, the two
`# x-release-please-version`-annotated lines). **Merging that release PR** tags `vX.Y.Z`, creates the
GitHub Release (notes = that version's changelog section), and triggers `publish.yml` to push the
multi-arch image + Helm chart to GHCR. There is no manual changelog or tag step.

- **Version is single-source:** chart `version` = `appVersion` = release version. release-please keeps
  the two annotated `Chart.yaml` lines in step (extra-files), and `publish.yml`'s shared
  `container-publish.yml` reusable derives the published image tags from the same git tag — so the
  registry and source never drift. (`scripts/publish.sh` is a local-only manual-publish fallback, NOT
  the CI path — see its header.)
- **Tag scheme (no `v` prefix on published artifacts):** the git tag / GitHub Release is `vX.Y.Z`, but
  `publish.yml`'s shared `container-publish.yml` reusable tags the image via `docker/metadata-action`
  with `{{version}}`, which strips the leading `v` — published image tags are `ghcr.io/rknightion/
  genai-otel-bridge:X.Y.Z` (+ `:X.Y`, `:X`, `:latest`), matching the already-unprefixed chart
  `version`/`appVersion`. Use the unprefixed form (e.g. `:3.0.1`) in `--set image=...` / registry
  references — not the `vX.Y.Z` git-tag form.
- **License notices + SBOMs are release artifacts (not committed/gated).** `publish.yml`'s notices job
  runs `just notices` and attaches `THIRD_PARTY_NOTICES.md` (the image also bakes notices into
  `/licenses/`); the SBOMs come from `syft` **inside** the `container-publish.yml` reusable, which scans
  the built image (not the local binary) and attaches SPDX + CycloneDX to the GitHub Release. Notices are
  generated from the real import graph (`go-licenses`) and churn on every dep bump, so they are
  deliberately kept out of `just check` to preserve Renovate automerge. See `LICENSING.md`.
- **Merging the release PR:** the workflow passes a fine-grained PAT (`token:
  ${{ secrets.RELEASE_PLEASE_TOKEN }}`) to `googleapis/release-please-action`, so the release PR is
  PAT-authored, not `GITHUB_TOKEN`-authored — GitHub's recursion guard does not apply, and CI runs on
  it automatically like any other PR. Merge policy is wait-for-green (the `ci-success` check), same as
  any other PR; there is no admin-bypass justification here anymore.
- `config-file` = `release-please-config.json`, `manifest-file` = `.release-please-manifest.json`
  (tracks the last released version). `publish.yml` is also `workflow_dispatch`-able for a manual re-publish.
- A `forbidden-words` gate (`scripts/forbidden-words.sh`) guards against deployment-specific identifiers
  leaking into the tracked tree. See `CONTRIBUTING.md` for the contributor workflow.

## Task tracking

Open work lives in **Backlog.md**, in `backlog/`, committed to git. It is a query, not a file you
infer: `backlog task list --plain` for the queue, `backlog doc list --plain` for the durable docs.
This repo moved off GitHub Issues on **2026-08-14**; work closed before that is indexed in the
"Closed GitHub issues" doc, and cited by its original `#NNN`, not by a task ID.

- Read the **fan-out protocol** doc before designing a wave, and the **wave operating model** doc for
  this project's own rules. `backlog doc list --plain` shows both.
- **Never use `--notes` or `--plan` bare.** They *silently replace* the whole section — another
  session's writes vanish with no warning and exit 0. Use `--append-notes` / `--append-plan`. This is
  an open upstream bug, not a misunderstanding, and a global `PreToolUse` hook in the agent config denies the bare
  form rather than trusting anyone to remember.
- **Never hand-edit task, draft, doc, decision or milestone markdown.** Section boundaries are
  HTML-comment markers; break one and the section is *silently dropped* at exit 0 — still in the file,
  invisible to the CLI, until the next write destroys it for real. There is no repair command
  (`backlog doctor` only fixes duplicate task IDs). The same hook denies these writes.
  **`backlog/config.yml` is the one exception** and is edited by hand: list-valued keys cannot be set
  through `backlog config set`, and the tool itself directs you to the file.
- **Finalize in one call**, so an interrupted agent cannot leave finished work looking unfinished:
  `backlog task edit gob-0007 --check-ac 1 --check-ac 2 -s Done`.
- **Never let two agents edit the same task.** v1.50.x fixed the edit funnel's lost-write race but not
  reorder, draft saves, the TUI path, `doc update` or decision updates.
- **`backlog/` is committed and this repo is public, so tasks and docs must never carry real account
  identifiers, endpoints, workspace or tenant IDs, keys, host names or customer names** — the same bar
  `scripts/forbidden-words.sh` enforces on the rest of the tree, and `backlog/` is inside its scope.
  Write the shape, not the instance: "the tenant's second workspace", `<vendor>/<workspace>/<loop>`.
  Aggregate counts, timings and structural findings are fine.
- Do not build a workflow on **decisions** — half-built upstream (no `edit`/`view`/`update`, no
  supersede, no MCP surface). Durable reference goes in **docs**; tasks are the unit. The durable
  design record stays `ARCHITECTURE.md` + `docs/DESIGN.md`, which the tracker does not replace.

<!-- BACKLOG.MD GUIDELINES START -->
<!-- backlog.md-instructions-version: 1.50.1 -->
<CRITICAL_INSTRUCTION>

## Backlog.md Workflow

This project uses Backlog.md for task and project management.

**For every user request in this project, run `backlog instructions overview` before answering or taking action.**

Use the overview to decide whether to search, read, create, or update Backlog tasks.

Before task lifecycle actions, read the matching detailed guide:
- `backlog instructions task-creation` before creating or splitting tasks
- `backlog instructions task-execution` before planning, changing status or assignee, adding a plan or implementation notes, or implementing task work
- `backlog instructions task-finalization` before checking acceptance criteria, writing final summaries, or moving tasks to terminal statuses

Use `backlog <command> --help` before running unfamiliar commands. Help shows options, fields, and examples.

Do not edit Backlog task, draft, document, decision, or milestone markdown files directly. Use the `backlog` CLI so metadata, relationships, and history stay consistent.

</CRITICAL_INSTRUCTION>
<!-- BACKLOG.MD GUIDELINES END -->
