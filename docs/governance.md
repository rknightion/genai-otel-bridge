---
title: Content Governance
description: How genai-otel-bridge enforces content-free telemetry — field allow/deny lists, label governance, and cardinality controls.
---

# Content Governance

Content governance is a **release gate**, not an optional nicety. genai-otel-bridge is designed
to emit only operational telemetry — never prompt text, completion text, message bodies, or
injected PII. This page explains the three-layer model that enforces that guarantee.

---

## The three layers

```
Source loop
    │  produces model.Batch (Samples + LogRecords)
    │
    ▼
[1] Source strip (default-deny allow-list, per-source)
    │  drops all fields not on the explicit allow-list
    │  hard-denied content fields (inputs/outputs/messages/metadata/…) cannot be opted in
    │
    ▼
[2] source.Guard (composition-root denylist + label allow-list)
    │  defence-in-depth backstop — denies content fields regardless of the strip
    │  enforces the per-series label cardinality budget
    │
    ▼
model.Batch emitted via OTLP
```

### Layer 1 — source strip (per-source, default-deny)

Each source package (`portkey/logs_strip.go`, `langsmith/runs_strip.go`) implements a
**default-deny allow-list**: only the fields on the list are passed through; everything
else is silently dropped. This is the primary content control.

The allow-list has two tiers:

- **Indexed attributes** — low-cardinality identity/routing fields (e.g. `run_type`,
  `status`, `ai_model`). These map to `model.LogRecord.IndexedAttributes` and, when
  [GS1 is complete](#grafana-staff-action-gs1), become Loki stream labels.
- **Record attributes** — per-record operational context (IDs, timestamps, token counts,
  cost). These map to `model.LogRecord.RecordAttributes` and land as Loki structured
  metadata.

The `Body` field is **never set** by any source. Nested objects and arrays not explicitly
handled are dropped, never stringified.

### Layer 2 — source.Guard (shared, default-deny)

The composition root wires a single `*source.Guard` instance across all loop runners.
The Guard enforces two things:

**Label allow-list (default-deny for metrics).** An empty `AllowLabelKeys` list denies
**all** metric labels — no label can appear on a metric sample unless it is explicitly
listed. The composition root allow-lists the labels each source loop declares. This
prevents any new label from accidentally reaching Grafana Cloud even if a source bug
or schema change introduces one.

Configure additional label keys via:

```yaml
governance:
  allow_label_keys:
    - my_custom_key
```

**Content denylist (defence-in-depth).** The Guard holds the `AbsoluteNeverDenyKeys`
floor — a fixed set of message-body and PII fields that are denied regardless of
any opt-in:

```
gen_ai.*  input.value  output.value  request  response
inputs    outputs      messages      metadata  portkeyHeaders
```

These fields **cannot** be released by `extra_record_fields` opt-ins. Attempting to
opt in a floor field is rejected at config load time with an error.

Beyond the floor, a configurable gray tier is on the denylist by default but can be
released per deployment via `settings.extra_record_fields` for specific loops.

### Layer 3 — content-leak conformance gate

`TestLogsExportContentLeakConformanceGate` (Portkey) and
`TestLangsmithRunsContentLeakConformanceGate` (LangSmith) are **release gate tests**
in the `internal/app` package. They assert that the full wired pipeline — strip, guard,
and emitter — produces zero content fields in the outbound OTLP payload. These tests
must stay green before shipping any logs-plane change.

---

## Cardinality governance

The `governance.per_metric_cardinality_budget` config key sets the **per-metric** cap on
distinct label-value combinations (default: 10,000). This is a per-metric cap, not a global
one — total cardinality across all metrics is the sum.

```yaml
governance:
  per_metric_cardinality_budget: 10000
```

When a series is over budget, it is **dropped and counted** via
`genai_otel_bridge_guard_dropped_total`. The `GenaiOtelBridgeCardinalitySpike` alert fires when
new label-value combinations are being created at a sustained rate — see
[Alerts & Runbooks](./alerts.md).

!!! note "The real cardinality ceiling is downstream"
    The `per_metric_cardinality_budget` is the in-process guard. The real ceiling is the
    Mimir / Adaptive Metrics limit on the target stack (Grafana-staff action GS3 — exempt
    `genai_otel_bridge_*` from Adaptive Metrics aggregation to preserve health signals).

---

## Opt-in fields

Some operational fields are not emitted by default but are safe to opt in. Use
`settings.extra_record_fields` on the relevant loop to add them to the strip's record
allow-list.

**Portkey examples (safe to opt in):** `request_id`, `trace_id` (top-level field),
`response_latency`.

**LangSmith examples (safe to opt in):** `app_path`, `tags` (rendered as CSV),
`reference_example_id`, `in_dataset`.

Opting in a hard-denied content field (`inputs`, `outputs`, `messages`, `metadata`,
`portkeyHeaders`, `gen_ai.*`) is rejected at config load time with a clear error message.

---

## Grafana-staff action GS1

The indexed attributes on log records land as Loki **structured metadata** until the target
Loki stack promotes them to stream labels. Until GS1 is applied on the target stack, these
attributes are filterable via `| key="value"` syntax but are **not** queryable as
`{key="value"}` (stream-label syntax).

This is a stack-side configuration action; it is not something genai-otel-bridge can configure
itself.

---

## See also

- [Portkey](./portkey.md) — Portkey-specific content controls
- [LangSmith](./langsmith.md) — LangSmith-specific content controls
- [Security](./security.md) — SSRF guard, secret handling, and the AGPL-3.0-only license
- [Telemetry reference](./telemetry.md) — the full signal catalogue
