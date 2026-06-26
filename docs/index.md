---
title: genai-otel-bridge
description: Vendor-neutral Go service that polls AI-platform APIs and emits operational telemetry to Grafana Cloud as OTLP metrics and logs.
---

# genai-otel-bridge

A vendor-neutral Go service that pulls **operational telemetry** from AI-platform APIs and emits it to Grafana Cloud — or any OTLP endpoint — as **metrics and logs**.

It targets two categories of source:

- **LLM gateways** (e.g. Portkey) — request, cost, token, latency, and error analytics, plus per-request content-free log records.
- **LLM evaluation and observability platforms** (e.g. LangSmith) — session and evaluation statistics, plus a content-free run index.

**Content-free by design:** `genai-otel-bridge` never requests prompt or response bodies. An outbound field allow/deny-list governs every emitted field — operational telemetry (latency, tokens, cost, errors) only. This guarantee is enforced as a release gate.

`genai-otel-bridge` is designed to sit on the production observability critical path. It is leader-elected (single-emit, no duplicates), self-observing, and resilient to downstream slowness.

---

## Quick navigation

<div class="grid cards" markdown>

-   :material-rocket-launch-outline: **Getting started**

    ---

    Install the binary or deploy with Helm in minutes.

    [:octicons-arrow-right-24: Getting started](./getting-started.md)

-   :material-cog-outline: **Configuration**

    ---

    Walk through every config key — emit endpoints, sources, loops, governance.

    [:octicons-arrow-right-24: Configuration guide](./configuration.md)

-   :material-chart-bar: **Telemetry reference**

    ---

    Every metric, log, and trace the bridge can emit, generated from the code.

    [:octicons-arrow-right-24: Telemetry reference](./telemetry.md)

-   :material-wrench-outline: **Operations**

    ---

    Dashboards, alert runbooks, and troubleshooting guides.

    [:octicons-arrow-right-24: Dashboards](./dashboards.md)

</div>

---

## Principles

- **Decoupled** — no customer-, vendor-deployment-, or domain-specific knowledge in core code or defaults. Metric names, label keys, endpoints, cadences, and environment identifiers are all config.
- **OTLP-native** — one transport for metrics and logs, direct to Grafana Cloud or via a local collector (e.g. Grafana Alloy).
- **Modular** — sources are self-contained packages behind a common interface; adding a source touches only its package.
- **Operationally honest** — a polling gap is always an alertable, counted signal, never silent.

---

## License

`genai-otel-bridge` is licensed under the [GNU Affero General Public License v3.0 only (AGPL-3.0-only)](https://github.com/rknightion/genai-otel-bridge/blob/main/LICENSE). Every Go source file carries an `SPDX-License-Identifier: AGPL-3.0-only` header.
