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
is the canonical remote — commit to `main`, tag `vX.Y.Z` to release (the tag drives the GHCR publish job).
The repo is currently private; a self-hosted Forgejo mirror is kept as a cold archive only.

## Architecture (the seams)

Data flows: **Source.Loop.Collect** (pull bounded window forward from watermark) → `model.Batch`
→ `source.Guard.Sanitize` (cardinality/content governance) → `schedule.LoopRunner` (single-flight,
bounded queue, epoch-fenced checkpoint) → `emit.Emitter` (deterministic OTLP encode + retry).

`internal/` packages, with their own CLAUDE.md where the detail matters:
- `model/` — **FROZEN** vendor-neutral types (Sample, LogRecord, Batch, Watermark, CheckpointKey).
- `source/` (+ `portkey/`) — Source/Loop interface + registry + cardinality Guard.
- `emit/` (+ `otlp/`) — Emitter seam, reject taxonomy, hand-rolled deterministic OTLP protobuf.
- `schedule/` — per-loop tick→collect→enqueue→emit driver; the watermark-advance state machine.
- `checkpoint/` (+ `configmap/`, `file/`) — durable watermark store + monotonic/epoch write fence.
- `coordinate/` (+ `lease/`) — leader election; single-active-replica.
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
  generated `CHANGELOG.md` (`make changelog`); `chore`/`style`/release commits are excluded from it.
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

Conventional Commits drive the generated `CHANGELOG.md` (`make changelog VERSION=vX.Y.Z`). Tagging a
`vX.Y.Z` triggers CI to build and publish the multi-arch container image + Helm chart to the registry.
A `forbidden-words` gate (`scripts/forbidden-words.sh`, self-skips when absent) guards against
deployment-specific identifiers leaking into the tracked tree. See `CONTRIBUTING.md` for the workflow.
