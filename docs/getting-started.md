---
title: Getting started
description: Understand the mental model and core vocabulary of genai-otel-bridge before installing and configuring it.
---

# Getting started

This page explains the mental model behind `genai-otel-bridge`. Reading it first will make the configuration guide and telemetry reference easier to navigate.

For installation steps, see [Installation](./installation.md).

---

## What the bridge does

`genai-otel-bridge` sits between AI-platform APIs (Portkey, LangSmith) and an OTLP backend (Grafana Cloud Mimir for metrics, Loki for logs). It polls those APIs on a cadence, derives a vendor-neutral set of metrics and log records, and pushes them via OTLP/HTTP.

Because the APIs are pull-only and rate-limited, `genai-otel-bridge` does the pulling well, once, and converts the result into clean Grafana-native telemetry — with the content-free guarantee enforced at every step.

---

## Data flow

```text
Source.Loop.Collect(watermark)
    │  pull bounded time window forward
    ▼
model.Batch
    │  vendor-neutral samples + log records
    ▼
source.Guard.Sanitize
    │  label allow-listing, cardinality budget, outbound field deny-list
    ▼
schedule.LoopRunner
    │  single-flight, bounded queue, epoch-fenced checkpoint
    ▼
emit.Emitter
    │  deterministic OTLP/HTTP encode + retry
    ▼
Grafana Cloud (Mimir → metrics, Loki → logs)
```

Each stage is a hard seam: a source package knows nothing about OTLP encoding; the emitter knows nothing about the source domain. The governance guard is the single enforcement point between them.

---

## Core vocabulary

These terms appear throughout the documentation. They map directly to the code.

**Source**
: A vendor integration (e.g. Portkey, LangSmith). Each source exposes one or more independent loops. Registered by type name in the config (`type: portkey`).

**Loop**
: One independent pull–derive cycle within a source. For example, Portkey has an `analytics` loop (time-bucketed metrics) and a `logs_export` loop (per-request log records). Each loop has its own `cadence`, `window`, and watermark.

**Window**
: The time range queried by a single `Collect` call. Kept short (≤ 55 minutes for Portkey analytics) to stay within the source API's fine-granularity bucket regime.

**Watermark**
: The loop's forward-only position: the last fully-emitted (or explicitly skipped) observation time. Persisted durably in a Kubernetes ConfigMap (or a local file for dev). A new leader loads the watermark and resumes without a replay log — the source API is the replayable buffer.

**Bucket settle** (`bucket_settle`)
: The age at which a source bucket stops changing after first observation. The bridge emits a bucket only once, after it has settled, to avoid emitting a value that will change. The default of 10 minutes is the live-measured late-arrival lag for Portkey analytics.

**Emit (once after settle)**
: A bucket is emitted exactly once. If a later poll shows a settled bucket changed (late arrival beyond settle), the bridge does not re-emit (Mimir rejects a changed value at the same series and timestamp); instead, it counts the drift as `bucket_revised_after_settle_total` so `bucket_settle` can be tuned.

**Guard**
: The governance layer between derive and emit. Enforces the label-key allow-list (default-deny: an empty allow-list denies all labels), a per-metric cardinality budget, and the outbound field deny-list that prevents any content from leaving via non-label fields. See [Content governance](./governance.md).

**CheckpointKey**
: A stable key that namespaces a loop's watermark — `{source_instance}/{loop}/{output-fingerprint}`. Adding a new loop or changing the metric prefix creates a new key and bootstraps its own history, rather than being skipped by an already-current loop watermark.

---

## High availability at a glance

`genai-otel-bridge` runs as a multi-replica Kubernetes Deployment. A single active replica holds a Kubernetes `coordination.k8s.io/v1` Lease and runs the scheduler; the others idle hot and take over within the lease duration on failure. The correctness guarantee against double-emit is the **monotonic, lease-epoch-fenced checkpoint write** — a demoted or overlapping leader cannot move the watermark backward.

For a deeper treatment, see [High availability](./high-availability.md).

---

## What to read next

- [Installation](./installation.md) — binary quickstart and Helm deployment.
- [Configuration](./configuration.md) — narrative walk-through of every config key.
- [Portkey guide](./portkey.md) — the Portkey analytics, groups, and logs_export loops.
- [LangSmith guide](./langsmith.md) — the LangSmith sessions and runs loops.
- [Telemetry reference](./telemetry.md) — every metric and log the bridge can emit.
