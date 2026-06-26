# genai-otel-bridge (`genai-otel-bridge`)

A generic, vendor-neutral integrator that pulls **operational telemetry** from AI-platform
APIs and emits it to Grafana Cloud (or any OTLP endpoint) as **metrics and logs**.

It targets two categories of source:

- **LLM gateways** (e.g. Portkey) — request/cost/token/latency/error analytics and request logs.
- **LLM evaluation / observability platforms** (e.g. LangSmith) — project stats, eval-score
  facets, and a content-free run index.

`genai-otel-bridge` polls each source's API on a cadence, derives a vendor-neutral set of metrics and
log records, and pushes them via OTLP/HTTP. It is designed to sit on the critical
observability path for production workloads: it is highly-available (leader-elected,
single-emit), self-observing, and resilient to downstream slowness.

**Content-free by design:** `genai-otel-bridge` never requests prompt/response bodies. An outbound field
allow/deny-list governs every emitted field, enforced as a release gate — operational telemetry
(latency/tokens/cost/errors) only, never inference content.

## Quickstart

Write a config file (`config.yaml`). Secrets are resolved from `${ENV}` / `file:/path` refs at load
time, so no credentials live in the file:

```yaml
emit:
  telemetry:
    otlp:
      endpoint: ${GC_OTLP_ENDPOINT}      # an OTLP/HTTP endpoint (Grafana Cloud or a local collector)
      instance_id: ${GC_INSTANCE_ID}
      token: ${GC_OTLP_TOKEN}
identity:
  service_namespace: genai-otel-bridge
  deployment_environment: ${ENV}
sources:
  - type: portkey
    enabled: true
    base_url: https://api.portkey.ai/v1
    source_instance: portkey-${ENV}
    auth: { header: x-portkey-api-key, value: ${PORTKEY_API_KEY} }
    loops:
      analytics:
        enabled: true
        cadence: 60s
        window: 50m
        bucket_settle: 3m
        max_backfill: 55m
        metric_prefix: portkey_api
        graphs: [requests, cost, tokens, latency, errors]
```

Build and run:

```bash
make build
GC_OTLP_ENDPOINT=... GC_INSTANCE_ID=... GC_OTLP_TOKEN=... ENV=dev PORTKEY_API_KEY=... \
  bin/genai-otel-bridge --config config.yaml
```

Or deploy to Kubernetes with the bundled chart (leader-elected HA, ConfigMap checkpoint store):

```bash
helm install genai-otel-bridge ./deploy/helm -f my-values.yaml
```

See [`deploy/helm/values.yaml`](./deploy/helm/values.yaml) — every source/loop knob is surfaced there
at its default with a comment.

## Status

Feature-complete across both source vendors (Portkey, LangSmith) and both telemetry planes
(metrics + logs), and green. Includes: the composition root + binary, leader-elected single-emit
HA, monotonic/epoch-fenced checkpointing, the deterministic OTLP exporter, self-observability,
acceptance tests (failover / outage / soak gates), a hardened Helm chart, configurable content
governance, and durability tuning for the Grafana Cloud accept windows. The design was settled
through independent adversarial reviews before code.

See [`followup.md`](./followup.md) for deferred/future work.

## Build & test

```bash
make gate     # vet + test + lint + spdx-check + build  — the green bar before any commit
make build    # -> bin/genai-otel-bridge (version stamped via git describe)
make test     # go test ./...
make lint     # golangci-lint run
```

Requires Go 1.26+. Acceptance gates: `go test -tags acceptance ./internal/app/`.

## Documentation

- **[User documentation](https://m7kni.io/genai-otel-bridge/)** — install, configure, the telemetry
  catalogue, operations + runbooks. The pages below are the maintainer-facing design docs.
- **[ARCHITECTURE.md](./ARCHITECTURE.md)** — the durable design: components, interfaces,
  data flow, HA, emit model, configuration, and the decision ledger.
- **[docs/DESIGN.md](./docs/DESIGN.md)** — the detailed, build-facing design spec: scope, concrete
  contracts/schemas, failure handling (F1–F47), test plan, and review dispositions.
- **[followup.md](./followup.md)** — deferred/future topics + the **out-of-scope-until-shape-known**
  register (data-sensitivity governance, emit backends, vendor SDKs, deployment modes, future sources).

## Principles (one line each)

- **Decoupled** — no customer-, vendor-deployment-, or domain-specific knowledge in core; everything is config.
- **OTLP-native** — one transport for metrics and logs; direct to Grafana Cloud or via a local collector.
- **Modular** — sources are self-contained packages behind a common interface; adding one touches only its package.
- **Operationally honest** — a polling gap is always an alertable signal, never silent.

## License

`genai-otel-bridge` is licensed under the GNU Affero General Public License v3.0 only (`AGPL-3.0-only`).
See [LICENSE](./LICENSE) and [LICENSING.md](./LICENSING.md). Every Go source file carries an
`SPDX-License-Identifier: AGPL-3.0-only` header.
