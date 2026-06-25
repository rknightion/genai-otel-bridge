# internal/model — the FROZEN vendor-neutral seam

The contract between sources and the emitter. **Do not add/rename/reorder fields** without a design
change (package doc says FROZEN; see ARCHITECTURE.md §4).

## Types (`model.go`)

- `Sample` — one derived metric point. v1 emits `Gauge` **only** (`Sum` exists for the future).
  `Unit` is **empty in v1** — units are baked into `Name` (e.g. `portkey_api_latency_seconds`).
  `Timestamp` is always **UTC, bucket-END** time. `Labels` are low-cardinality, guard-policed.
- `LogRecord` — `Body` is **empty in v1** (FR10: no content). `IndexedAttributes` → OTLP resource
  attrs (low-card routing); `RecordAttributes` → per-record OTLP log attrs. `TraceID` (16 bytes, empty =
  unset) → OTLP log `trace_id`: a source-provided correlation id (e.g. Portkey metadata `correlation_id`)
  passed through for logs↔traces linking — correlation passthrough, **not** span synthesis (ledger #4/#15).
- `Watermark{Time, Cursor, Epoch}` — loop position. `Time` is monotonic forward-only; `Epoch` is the
  leader lease epoch for write fencing (Cdx-C14); `Cursor` is an optional source resume token.
- `CheckpointKey{SourceInstance, Loop, OutputFingerprint}` — `String()` is deterministic
  (`instance/loop/fingerprint`), used as a stable store key.
- `Batch{Key, Samples, Logs, Watermark}` — one Collect/Emit unit. Watermark advances **iff** the batch
  emits or is skipped-with-gap; an empty batch with unchanged watermark is valid (nothing settled yet).

## Gotchas

- `Temporality` is carried only so the emitter can reject `Delta` (GC gateway is Cumulative-only); it
  is ignored for Gauge.
- `Fingerprint(seriesNames, namingConfig)` is **order-insensitive** (sorts internally) → SHA256 →
  16-hex prefix. Adding/removing a series changes the fingerprint, so a new series set bootstraps its
  **own** checkpoint history (F37) rather than inheriting a stale watermark.

Tests: pure assertions on small structs — `String()` stability, fingerprint order-insensitivity. No
fixtures, no golden files.
