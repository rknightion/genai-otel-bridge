# genai-otel-bridge

Vendor-neutral Go service that polls AI-platform APIs (LLM gateways, eval platforms) and emits
**operational** telemetry to Grafana Cloud as OTLP metrics and logs. It sits on the production
observability critical path: leader-elected, single-emit, self-observing, resilient to downstream
slowness.

## Hard rules (do not violate)

- **Decoupled.** No vendor, customer or domain knowledge in core code or defaults. Metric names,
  label keys, endpoints, cadences, windows and env identifiers are all config. Vendor code lives
  only in its `internal/source/<vendor>` package behind the common interface.
- **Content minimisation is a release gate.** Never request message bodies, prompts or completions.
  An outbound field allow/deny-list governs *every* emitted field (labels, log body, metadata), so
  content cannot leak via non-label fields. `internal/app` wires the denylist; `source.Guard`
  enforces label allow-listing, default-deny (an empty allow-list denies all labels).
- **Conditional gap-free is engineered, not assumed.** It emerges from emit-once-after-settle plus
  deterministic byte-identical encoding plus monotonic, lease-epoch-fenced checkpoint writes. Do not
  weaken any of the three expecting the others to compensate.
- **Operationally honest.** Every polling or emit gap and every skipped sample is alertable
  (`window_lag`, `samples_skipped_total`), never silent. A non-retryable reject *advances past* the
  bad bucket with a counted gap so the loop always progresses; it never stalls or drops silently.
- **FROZEN seams.** `model.*` types and `source.Source` / `source.Loop` are marked FROZEN. Adding or
  renaming a field or method there is a design change requiring an `ARCHITECTURE.md` update.

## Architecture

`Source.Loop.Collect` (pull a bounded window forward from the watermark) to `model.Batch` to
`source.Guard.Sanitize` (cardinality and content governance) to `schedule.LoopRunner` (single-flight,
bounded queue, epoch-fenced checkpoint) to `emit.Emitter` (deterministic OTLP encode plus retry).

Read `ARCHITECTURE.md` (durable seams, decision ledger) and `docs/DESIGN.md` (build spec, F1-F47
failure handling) before changing a seam.

## Task interface

`just check` is the gate. `just ci` adds the two legs needing Docker or a service container
(`e2e`, `test-dynamodb`). Repo deltas beyond the standard task-surface rule:

- Run `just` with stdin from `/dev/null`. `publish` is `[confirm]` and pushes to a real registry.
  Never pass `--yes` or `JUST_YES=1`.
- `just check` covers every leg of ci.yml's `hygiene` job (`forbidden-words`, `spdx-check`,
  `helm-lint`, `tf-validate`), so a local green gate implies a green hygiene leg.
- `tf-validate` self-skips when the IaC tools are absent, and `forbidden-words` scans only the
  built-in credential shapes when neither `$FORBIDDEN_WORDS_PATTERN` nor
  `scripts/forbidden-words.local` is present. A clean local run is weaker than CI's on both.
- `ci-success` is the only required check on `main`; the individual CI legs are not, so adding or
  renaming a leg never touches branch rules. Renovate PRs self-automerge, majors included, once
  `ci-success` is green.

## Conventions

- **Conventional Commits.** Subjects drive the release-please `CHANGELOG.md`. Only `feat`, `fix` and
  breaking changes bump the version; `chore`, `style` and `test` are hidden from the changelog.
- Stage explicit paths (`git add <path>`), never `-A` or `.`. Concurrent agents may share this
  working tree; never stage, commit or revert work that is not yours.
- No live network in tests. `httptest.Server` fakes for HTTP, injectable clocks
  (`SetLoopClockForTest`) for determinism.
- **`*_review_test.go` files encode specific adversarial-review findings** (tagged `[ext-review-14]`,
  `CP-R3b`, `Cdx-C14`). They are regression guards for known attack and race scenarios; keep them.
- The durable design record is `ARCHITECTURE.md` plus `docs/DESIGN.md` (tracked). Move anything
  build-affecting out of scratch into those.
- Config resolves `${ENV}` and `file:/path` refs at load time; `.env`, `*.local.yaml` and
  `*.secret.*` are gitignored.
- `scripts/forbidden-words.sh` blocks deployment-specific identifiers anywhere in the tracked tree,
  **`backlog/` included**. The repo is public: write the shape, not the instance
  (`<vendor>/<workspace>/<loop>`, "the tenant's second workspace"). Counts, timings and structural
  findings are fine.

## Release

release-please maintains a release PR from the Conventional Commits on `main`; merging it tags
`vX.Y.Z`, creates the GitHub Release and triggers `publish.yml`. There is no manual changelog or tag
step.

- **Version is single-source:** chart `version` = `appVersion` = release version, kept in step by the
  two `# x-release-please-version` lines in `deploy/helm/Chart.yaml`.
- **Published artifacts carry no `v` prefix.** The git tag is `vX.Y.Z`, but `docker/metadata-action`
  inside the shared `container-publish.yml` strips it, so image tags are
  `ghcr.io/rknightion/genai-otel-bridge:X.Y.Z` (plus `:X.Y`, `:X`, `:latest`). Use the unprefixed
  form in `--set image=...` and registry references.
- **Third-party notices and SBOMs are release artifacts, deliberately outside `just check`.**
  `just notices` regenerates from the real import graph and churns on every dependency bump, so
  gating on it would break Renovate automerge. SBOMs come from syft inside `container-publish.yml`,
  scanning the built image rather than the local binary. See `LICENSING.md`.
- `scripts/publish.sh` is a local manual fallback, **not** the CI path.

## Task tracking

Open work lives in Backlog.md under `backlog/`, task prefix `gob`. Work closed before the move off
GitHub Issues is indexed in the "Closed GitHub issues" doc and cited by its original `#NNN`, not by a
task ID. Read the fan-out protocol doc before designing a wave, and the wave operating model doc for
this project's own rules.

- **`backlog/config.yml` is the one file to hand-edit.** List-valued keys cannot be set through
  `backlog config set`, and the tool directs you to the file. Every other task, draft, doc, decision
  and milestone file is CLI-only: section boundaries are HTML-comment markers, and breaking one
  *silently drops* the section at exit 0 with no repair command (`backlog doctor` only fixes
  duplicate task IDs).
- **Finalize in one call** so an interrupted session cannot leave finished work looking unfinished:
  `backlog task edit gob-0007 --check-ac 1 --check-ac 2 -s Done`.
- **Never let two agents edit the same task.** The lost-write race is fixed for the edit funnel only,
  not for reorder, draft saves, the TUI path, `doc update` or decision updates.
- Do not build a workflow on **decisions**: half-built upstream, with no `edit`/`view`/`update`, no
  supersede and no MCP surface. Durable reference goes in docs; tasks are the unit.

## Deeper references

- `deploy/grafana/README.md` - read before querying the emitted metrics or changing an alert. Also
  carries GS1-GS4, the Grafana-staff stack-side prerequisites this repo cannot set.
- `docs/DESIGN.md` - read before changing window, settle, backfill or reject semantics.
- `CONTRIBUTING.md` - contributor workflow.

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
