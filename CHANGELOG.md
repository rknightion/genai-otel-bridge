# Changelog

All notable changes to genai-otel-bridge. Generated from Conventional Commits.
## [3.1.0](https://github.com/rknightion/genai-otel-bridge/compare/v3.0.1...v3.1.0) (2026-07-02)


### Features

* AWS ECS deployment target (DynamoDB-backed HA) ([#13](https://github.com/rknightion/genai-otel-bridge/issues/13)) ([f6f0d61](https://github.com/rknightion/genai-otel-bridge/commit/f6f0d61df6e3179ae6f7ea88e77fa9cb4538c4fc))
* generate drift-guarded telemetry catalogue into docs/telemetry.md ([cfd4ad4](https://github.com/rknightion/genai-otel-bridge/commit/cfd4ad46ab3f3843a8e94673295bb09c8ea553b5))
* third-party license notices + SBOMs as release artifacts ([f8cf300](https://github.com/rknightion/genai-otel-bridge/commit/f8cf300a3d47e7a1fe862fbb2cf3c7170b37fcb3))


### Bug Fixes

* **deps:** update go modules (non-major) ([#22](https://github.com/rknightion/genai-otel-bridge/issues/22)) ([dd2f4b4](https://github.com/rknightion/genai-otel-bridge/commit/dd2f4b455cf0b315203c9eb47e42fbdc796ff62a))
* **deps:** update kubernetes libraries ([#6](https://github.com/rknightion/genai-otel-bridge/issues/6)) ([71166a3](https://github.com/rknightion/genai-otel-bridge/commit/71166a34d4e8c9cfab655048056a0b19d8020670))
* **ecs:** build task IAM policy as a literal list, not jsondecode of a deferred data source ([60243f2](https://github.com/rknightion/genai-otel-bridge/commit/60243f26f1074ef567c8045de2430d5f70fbea0e))


### Documentation

* fix config-accuracy errors found in final review ([62ac033](https://github.com/rknightion/genai-otel-bridge/commit/62ac03333c0e9ff8860f6b626253b86ff13fe465))
* scaffold zensical site + dispatch workflow for m7kni.io hub ([2736648](https://github.com/rknightion/genai-otel-bridge/commit/2736648ebe67a628cf8b969964c0f307b67b1633))
* write full user-documentation page set ([79c83a9](https://github.com/rknightion/genai-otel-bridge/commit/79c83a9e01d61c6014e752c786dea3d652f76416))


### Build & CI

* add hadolint + trivy Docker security scans ([2c8dc84](https://github.com/rknightion/genai-otel-bridge/commit/2c8dc840536ecf13c64a44b18a4a249498328c74))
* add Snyk -&gt; Snyk Cloud monitor (SCA/SAST/IaC/container) ([5113e36](https://github.com/rknightion/genai-otel-bridge/commit/5113e36029c7ac8c96ad94d0f954ec05d74ed2a7))
* adopt shared rknightion/.github reusable security workflows ([25fbdb3](https://github.com/rknightion/genai-otel-bridge/commit/25fbdb3a9a413707bc35ef164534c384f658927c))
* auto-assign maintainer on new issues (notify by email) ([a919c0f](https://github.com/rknightion/genai-otel-bridge/commit/a919c0f0c2c3828a531974c5b5837b39eb117c61))
* build + publish edge :main image + snapshot chart on push to main ([3cbf760](https://github.com/rknightion/genai-otel-bridge/commit/3cbf7605a19a8dd9cecbdbe9f25640ee2cc3de21))
* bump shared rknightion reusables to v1.3.1 ([ecaab65](https://github.com/rknightion/genai-otel-bridge/commit/ecaab6537b543bc7789f7d3048fcb79bc71fa922))
* **codacy:** add local analysis config + Cloud file exclusions ([2959c88](https://github.com/rknightion/genai-otel-bridge/commit/2959c883a012f7e85b45c6f3a2ddd45544d3c5b0))
* open Renovate PRs by counting internal checks as success ([91c65f2](https://github.com/rknightion/genai-otel-bridge/commit/91c65f20591297eda192535a8efe9f3f4797ce22))
* open the release-please PR under a PAT so CI runs without manual approval ([f85574d](https://github.com/rknightion/genai-otel-bridge/commit/f85574d00933cf442ff2f62e46a5dff606f4646f))
* pin shared rknightion reusables to v1.0.0 ([ba573e7](https://github.com/rknightion/genai-otel-bridge/commit/ba573e748a7b7afae7bec9e159a3d8a9fe278403))
* publish image + Helm chart via shared container-publish reusable ([5e59ce5](https://github.com/rknightion/genai-otel-bridge/commit/5e59ce5098a2981baac7b79d2822846aa7646a3a))
* reference rknightion/.github reusables [@main](https://github.com/main) (unpin from digest) ([302ec29](https://github.com/rknightion/genai-otel-bridge/commit/302ec2923f9a1e811b3025ab74f0b080628e61f4))
* report Go test coverage to Codacy ([a90ede4](https://github.com/rknightion/genai-otel-bridge/commit/a90ede4d506b77b3acba4687b0966cd4fff4489e))
* resolve actionlint/shellcheck + zizmor workflow findings ([437450d](https://github.com/rknightion/genai-otel-bridge/commit/437450d475907481212110fc57a9228bd2f1921a))
* use Codacy account token for coverage upload ([2b02c4d](https://github.com/rknightion/genai-otel-bridge/commit/2b02c4da016f1e39cb2696a1dcce71e30ea12536))

## [3.0.1](https://github.com/rknightion/genai-otel-bridge/compare/v3.0.0...v3.0.1) (2026-06-26)


### Bug Fixes

* force release-please generic updater on Chart.yaml; add workflow_dispatch ([3050d0b](https://github.com/rknightion/genai-otel-bridge/commit/3050d0bda9c1a104600bc85f741ecb8d892d7481))


### Documentation

* repo is public + main requires ci-success (admin bypass) ([f2b88e2](https://github.com/rknightion/genai-otel-bridge/commit/f2b88e24e90df60af064a037629452ad254c12dd))


### Build & CI

* add hybrid issue-triage (no-tools AI analysis + deterministic apply) ([98cc6a0](https://github.com/rknightion/genai-otel-bridge/commit/98cc6a0dcf2f23883eaeee51cb3e74059b5805ca))
* automate releases with release-please (changelog + GitHub Releases + chart bump) ([d5c84f1](https://github.com/rknightion/genai-otel-bridge/commit/d5c84f1d6f563aeb16a6a3bdd2c2c1338f06642c))
* parallelize CI matrix + add ci-success gate; enable Renovate automerge ([fd6a293](https://github.com/rknightion/genai-otel-bridge/commit/fd6a29352e7c3bf39e04dd2c20f7042433977a63))
* wire release-please config, workflows, and chart bump ([98db168](https://github.com/rknightion/genai-otel-bridge/commit/98db1681116894919db0d4530df5d7e05d1b5f8a))

## [3.0.0] - 2026-06-26

### Build & CI
- Harden GitHub Actions workflow (zizmor)
- Enforce the forbidden-words guard in CI via FORBIDDEN_WORDS_PATTERN
- Strengthen leak detection — credential shapes, gitleaks, fix silent grep bug

### Refactor
- Unify project naming as genai-otel-bridge (retire "decant")
## [2.1.1] - 2026-06-25

### Bug Fixes
- Empty-array-safe PRIVATE_NAMES expansion (bash 3.2 + set -u)
- Example image -> ghcr.io/rknightion/genai-otel-bridge:latest (GHCR has no :main)
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
- Add 'genai-otel-bridge -validate-config' for secret-free config/overlay validation

### Refactor
- Rename make-aip-oi-secret.sh -> make-genai-otel-bridge-secret.sh
- Genericize EKS example, move onto the public surface
- Extract customer delivery artifacts out of the product repo
## [2.0.0] - 2026-06-25

### Dependencies
- Optimise renovate config for vendored go + conventional commits

### Refactor
- Rename project to genai-otel-bridge, artifacts to genai-otel-bridge
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

All notable changes to genai-otel-bridge. Subsequent releases are generated from Conventional Commits.

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

- Add genai-otel-bridge self-observability dashboard (v2, self-obs role)
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

genai-otel-bridge is a vendor-neutral integrator that polls AI-platform APIs (LLM gateways such as Portkey,
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
