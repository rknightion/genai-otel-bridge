# genai-otel-bridge (`decant`)

A generic, vendor-neutral integrator that pulls **operational telemetry** from AI-platform
APIs and emits it to Grafana Cloud (or any OTLP endpoint) as **metrics and logs**.

It targets two categories of source:

- **LLM gateways** (e.g. Portkey) — request/cost/token/latency/error analytics and request logs.
- **LLM evaluation / observability platforms** (e.g. LangSmith) — project stats, eval-score
  facets, and a content-free run index.

`decant` polls each source's API on a cadence, derives a vendor-neutral set of metrics and
log records, and pushes them via OTLP/HTTP. It is designed to sit on the critical
observability path for production workloads: it is highly-available (leader-elected,
single-emit), self-observing, and resilient to downstream slowness.

**Content-free by design:** `decant` never requests prompt/response bodies. An outbound field
allow/deny-list governs every emitted field, enforced as a release gate — operational telemetry
(latency/tokens/cost/errors) only, never inference content.

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
make gate     # vet + test + lint + decouple-check + spdx-check + build  — the green bar before any commit
make build    # -> bin/decant (version stamped via git describe)
make test     # go test ./...
make lint     # golangci-lint run
```

Requires Go 1.26+. Acceptance gates: `go test -tags acceptance ./internal/app/`.

## Documentation

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

`decant` is licensed under the GNU Affero General Public License v3.0 only (`AGPL-3.0-only`).
See [LICENSE](./LICENSE) and [LICENSING.md](./LICENSING.md). Every Go source file carries an
`SPDX-License-Identifier: AGPL-3.0-only` header.
