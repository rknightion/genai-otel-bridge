# genai-otel-bridge — genai-otel-bridge

Vendor-neutral Go service that polls AI-platform APIs (LLM gateways like Portkey, eval platforms
like LangSmith) and emits **operational** telemetry to Grafana Cloud as OTLP metrics/logs. Sits on
the production observability critical path: leader-elected, single-emit, self-observing, resilient
to downstream slowness.

> **Status:** the integrator is feature-complete across **both vendors × both planes** and green.
> Portkey: `analytics` + `groups` → OTLP metrics, `logs_export` → OTLP logs. LangSmith: `sessions`/eval
> → OTLP metrics, `runs` → OTLP logs. Plus composition root, binary, HA, checkpointing, acceptance
> tests, hardened Helm chart, configurable content governance, and durability tuning (metrics
> `max_backfill` 90m ≤ Mimir 2h OOO; logs 24h ≤ Loki 7d; too-old honesty path built+tested both planes).
> Every Portkey + LangSmith settings knob is surfaced in `values.yaml` at its default with a comment.
> **ECS deployment target (branch `feat/ecs-dynamodb-support`, PR):** runs production-grade on AWS ECS
> as well as Kubernetes — **DynamoDB-for-both** HA behind the frozen seams (`ha.coordinator=dynamodb` +
> `ha.checkpoint=dynamodb` + the `ha.dynamodb.*` block; one table backs the CAS lock + the checkpoint),
> a reusable Terraform module in `deploy/ecs/terraform/` (Fargate default, EC2 via `launch_type`), a
> `-healthcheck` binary mode, and a `dynamodb-local` CI gate (`tf-validate` + `dynamodb-backends` job).
> See `ARCHITECTURE.md` decision ledger #17. (This work uses a branch+PR by exception to the usual
> direct-to-main convention — it's a big OSS seam change.)
> **In flight (3 lanes):** in-cluster cleartext-emit opt-out (`emit.*.otlp.allow_insecure`, CP-M7);
> Portkey `groups` cost/metadata flags (live-probe confirmed cost=**cents**, metadata dim field=
> `metadata_value`); config-driven indexed/stream-label opt-in (`governance.allow_label_keys`).
> **Grafana-staff actions (not code blockers, the maintainer files them):** GS1 = Loki stream-label promotion of the
> logs loops' indexed attrs (until done they land as structured metadata, not queryable as `{label=…}`);
> GS2 = widen the backend accept window for long outages. See `docs/DESIGN.md` §7/RP2, `followup.md`.

## Build / test / lint gate

```bash
make gate     # vet + test + lint + forbidden-words + spdx-check + build ./...  — the green bar before any commit
make build    # -> bin/genai-otel-bridge, version stamped via git describe ldflags
make test     # go test ./...
make lint     # golangci-lint run  (config is .golangci.yml, v2 schema)
go test -tags acceptance ./internal/app/   # §9 acceptance gates (failover, outage, soak)
```

Go 1.26. Module path: `github.com/rknightion/genai-otel-bridge`. GitHub (`rknightion/genai-otel-bridge`)
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
  `make gate` green before *every* commit (evidence, not assertion). Stage
  explicit paths (`git add <path>`), never `-A`/`.` — concurrent agents may share the working tree;
  never stage, commit, or revert work that isn't yours. *Exception:* Renovate dependency bumps open
  PRs and self-automerge (non-major only) once the full CI suite is green — see `renovate.json`.
- **CI fans out** (`.github/workflows/ci.yml`): `make ci` is split into a parallel `gate` matrix
  (build-vet / lint / test / race / acceptance / envtest / hygiene) plus `e2e` and `secret-scan`;
  the `ci-success` aggregator job is the single check that gates Renovate automerge and `publish`.
- **Conventional Commits** (`feat:`/`fix:`/`chore:`/`docs:`/`refactor:`/…) — subjects drive the
  release-please-generated `CHANGELOG.md`; only `feat`/`fix`/breaking bump the version, `chore`/`style`/
  `test` are hidden from the changelog. See the Release section below.
- **Gate extras:** `make gate` runs `forbidden-words` (a content/decoupling guard — self-skips where its
  script isn't present) and `spdx-check` (every `.go` carries the AGPL-3.0-only SPDX header).
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
multi-arch image + Helm chart to GHCR. There is no manual `make changelog` / `git tag` step.

- **Version is single-source:** image tag = chart `version` = `appVersion` = release version (also
  enforced by `scripts/publish.sh`, which derives all three from the tag at publish time).
- **License notices + SBOMs are release artifacts (not committed/gated).** `publish.yml` also runs
  `make notices` + `make sbom` and attaches `THIRD_PARTY_NOTICES.md` + SPDX-2.3/CycloneDX-1.6 SBOMs
  to the GitHub Release; the image bakes notices into `/licenses/`. Generated from the real import
  graph (`go-licenses`/`syft`), they churn on every dep bump, so they are deliberately kept out of
  `make gate` to preserve Renovate automerge. See `LICENSING.md`.
- **Merging the release PR:** release-please opens it with `GITHUB_TOKEN`, so CI does not auto-run on it
  (GitHub's recursion guard) — merge it as a repo admin (branch protection `enforce_admins=false` lets
  admins bypass); its content was already validated when the underlying commits landed. To make release
  PRs run CI like any other (and drop the bypass), give the action a fine-grained PAT via a `token:` input.
- `config-file` = `release-please-config.json`, `manifest-file` = `.release-please-manifest.json`
  (tracks the last released version). `publish.yml` is also `workflow_dispatch`-able for a manual re-publish.
- A `forbidden-words` gate (`scripts/forbidden-words.sh`) guards against deployment-specific identifiers
  leaking into the tracked tree. See `CONTRIBUTING.md` for the contributor workflow.
