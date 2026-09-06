# internal/model

The vendor-neutral contract between sources and the emitter. The root file's FROZEN rule applies to
every type here; what follows is the v1 semantics a reader cannot get from the struct definitions.

- `Sample.Kind` is `Gauge` in v1. `Sum` exists for the future and nothing emits it.
- `Sample.Unit` is empty in v1: units are baked into `Name` (`portkey_api_latency_seconds`). The
  emitter still groups by (name, unit), so setting it later splits series.
- `Sample.Timestamp` is always UTC and always the bucket-END time.
- `LogRecord.Body` is empty in v1 and stays empty: FR10 forbids requesting content from any source.
- `LogRecord.IndexedAttributes` map to OTLP resource attributes and are the cardinality-dangerous
  tier, policed by the guard exactly as `Sample.Labels` are. `RecordAttributes` map to per-record log
  attributes and are not indexed.
- `LogRecord.TraceID` is 16 bytes, empty meaning unset. It carries a source-provided correlation id (a Portkey metadata `correlation_id`,
  say) through to the OTLP `trace_id` for logs-to-traces linking. It is correlation passthrough, not
  span synthesis: never invent a span from the gateway hop.
- `CheckpointKey.String()` is the stable durable store key and its format is fixed:
  `instance/loop/fingerprint`.
- `Watermark.Time` is monotonic forward-only; `Epoch` is the leader lease epoch used for write
  fencing; `Cursor` is an optional source resume token the fence does not interpret.
- `Batch` watermark advances only when the batch emits or is skipped with a counted gap. An empty
  batch with an unchanged watermark is valid: nothing has settled yet.
- `Temporality` is carried only so the emitter can reject `Delta`. It is ignored for Gauge.
- `Fingerprint(seriesNames, namingConfig)` is order-insensitive (it sorts internally) and returns a
  16-hex prefix of a SHA256. Adding or removing a series changes the fingerprint, and since the fingerprint is part of `CheckpointKey` the new series set bootstraps its
  own checkpoint history rather than inheriting a stale watermark.
