# Changelog

All notable changes to decant. Generated from Conventional Commits.
## [2.1.1] - 2026-06-25

### Bug Fixes
- Empty-array-safe PRIVATE_NAMES expansion (bash 3.2 + set -u)
- Example image -> ghcr.io/rknightion/decant:latest (GHCR has no :main)
- Log in to the registry host, not host/namespace (GHCR support)
## [2.1.0] - 2026-06-25

### Build & CI
- Disable tag-triggered auto-promotion (freeze customer promotion during rename)
- Narrow .github exclude to workflows/ so issue+PR templates promote
- Publish image + chart to GHCR from the public repo on v* tags

### Documentation
- Add OSS governance files + README quickstart
- Sanitize root CLAUDE.md + put CLAUDE.md on the public surface

### Features
- Add 'decant -validate-config' for secret-free config/overlay validation

### Refactor
- Rename make-aip-oi-secret.sh -> make-decant-secret.sh
- Genericize EKS example, move onto the public surface
- Extract customer delivery artifacts out of the product repo
## [2.0.0] - 2026-06-25

### Dependencies
- Optimise renovate config for vendored go + conventional commits

### Refactor
- Rename project to genai-otel-bridge, artifacts to decant
## [1.5.0] - 2026-06-24

### Bug Fixes
- Correct alert state enum OK -> Ok

### Documentation
- Correct GS1 promote-lists to live indexed sets
- Document v2 dashboard, dynamic thresholds, 11-alert set
- Note langsmith product rules + per-bucket-gauge staleness gotcha
- Record late-data investigation (§11) + revision-age instrumentation

### Features
- Add self-relative staleness recording rules
- Self-relative PollerStale + 7 new self-obs alerts
- Rebuild self-obs dashboard as v2 tabs + dynamic layout
- Add LangSmith product recording rules
- Instrument bucket-revision lateness (age histogram)
- Add bucket-revision lateness panel (age p50/p95)
- Map native top-level trace_id field to OTLP trace_id
## [1.4.0] - 2026-06-24

### Bug Fixes
- Gofmt config.go alignment + test stamper functions to satisfy unused lint

### Documentation
- Document portkey api_key_use_cases per-key labelling
- Tighten api_key_use_case slug-rule wording (run-collapse)

### Features
- Lift metadata correlation_id into logs (record attr + OTLP trace_id)
- Add api_key_use_cases to Portkey SourceConfig
- SlugifyUseCase label normaliser
- ResolveUseCases validation + use-case stampers
- Allow-list api_key_use_case (+ app golden union)
- Analytics N internal filtered passes + api_key_use_case stamp
- Groups N internal filtered passes + api_key_use_case stamp
- Logs_export per-use-case fan-out + record-tier api_key_use_case
- Surface portkey api_key_use_cases example

### Testing
- Analytics use-case stamp + api_key_ids filter e2e
- Logs export api_key_ids filter body + record stamp
- ValidateOwnership passes with api_key_use_cases (M7 regression guard)
# Changelog

All notable changes to decant. Subsequent releases are generated from Conventional Commits.

## [1.3.1] - 2026-06-23

### Bug Fixes

- Track released version in appVersion + auto-stamp on changelog

## [1.3.0] - 2026-06-23

### Features

- Notebook-parity analytics — token_type split, latency avg, api_key_ids filter

## [1.2.3] - 2026-06-23

### Bug Fixes

- Rebuild wrong-arch cached tools-e2e binaries (helm/k3d/kubectl)

## [1.2.2] - 2026-06-23

### Bug Fixes

- Pin gate jobs to arm64 + rebuild wrong-arch cached tools

## [1.2.1] - 2026-06-23

### Bug Fixes

- Record last_success on the logs emit path (health coverage for logs loops)
- Pull golang builder base via mirror.gcr.io to dodge Docker Hub rate limits

## [1.2.0] - 2026-06-23

### Bug Fixes

- Allow product OTLP egress to in-cluster Alloy on 4318
- Make self-obs health stats self-contained (not recording-rule dependent)
- Correct scrape_healthy bool + add poller-down/auth self-obs alerts

### Documentation

- Document the self-obs poller-health alerts

### Features

- Add decant self-observability dashboard (v2, self-obs role)
- Stamp a `source` record attribute on product logs (portkey/langsmith)

## [1.1.1] - 2026-06-23

### Bug Fixes

- Recover logs_export from a lost-ack /start instead of wedging on AB01
- Deploy poller into the grafana-poller namespace (ESO SA alignment)

### Documentation

- Align dev overlay with renamed langsmith-poller secret
- Consume existing shared portkey/langsmith secrets

### Features

- Route product telemetry through in-cluster Alloy; enable self traces + profiles

## [1.1.0] - 2026-06-23

### Features

- Add gated ESO SecretStore/ExternalSecret + runtimeEnv to deploy/helm

## [1.0.3] - 2026-06-23

### Build & CI

- **Scope the deploy jobs to the `dev` environment** so the environment-scoped registry/OIDC
  configuration resolves at build time (the image build/push + GitOps sync jobs now declare
  `environment: dev`, matching the rest of the estate).

## [1.0.2] - 2026-06-23

### Build & CI

- **Parallelised the CI gate** — its steps (build, vet, lint, unit test, race, acceptance, envtest,
  helm-lint) now run as concurrent jobs rather than one sequential run, cutting wall-clock to roughly the
  slowest single step. All steps still block the image publish.

## [1.0.1] - 2026-06-23

### Build & CI

- **Delivery pipeline** — the CI now builds the container image and pushes it to the configured
  registry, then syncs the Helm chart into the GitOps deployment repo and pins the released image tag.
- **Kaniko build path** (`Dockerfile.kaniko`) — a BuildKit-free Dockerfile for daemonless CI builders
  (native single-arch build).
- The image is tagged with both the deployment-tracking tag and the `vX.Y.Z` release tag.

## [1.0.0] - 2026-06-23

Initial release.

decant is a vendor-neutral integrator that polls AI-platform APIs (LLM gateways such as Portkey,
evaluation platforms such as LangSmith) and emits operational telemetry to Grafana Cloud as OTLP
metrics and logs.

### Features

- **Portkey source** — `analytics` + `groups` → OTLP metrics; `logs_export` → OTLP logs.
- **LangSmith source** — `sessions`/eval → OTLP metrics; `runs` → a content-free OTLP log index.
- **Content-free by design** — never requests prompt/response bodies; an outbound field allow/deny-list
  governs every emitted field, enforced as a release gate.
- **Highly available** — leader-elected single-emit; monotonic, lease-epoch-fenced checkpointing;
  conditional gap-free emit within source retention plus the sink accept window.
- **Deterministic OTLP** — hand-encoded, byte-identical re-emit (the precondition for sink idempotency).
- **Operationally honest** — every polling/emit gap or skipped sample is an alertable signal, never silent.
- **Configurable governance** — content allow/deny-list and per-metric cardinality guard; durability
  tuning sized to the Grafana Cloud out-of-order / too-old accept windows.
- **Self-observing** — own metrics + logs (distinct resource identity), optional self-profiling and
  self-tracing.
- **Hardened Helm chart** — non-root, default-deny network policy, PDB, least-privilege RBAC.

Licensed under AGPL-3.0-only.
