# internal/emit

The `Emitter` seam plus the reject/retry error taxonomy; `otlp/` is the OTLP/HTTP exporter with a
hand-rolled protobuf encoder.

## Reject taxonomy drives the scheduler's advance-or-halt decision

`RejectError.AdvancesPast()` returns true for `DuplicateTimestamp`, `TooOld` and `PayloadTooLarge`:
the scheduler advances the watermark past the bucket with a counted gap and the loop progresses. It
returns false for `BadEncoding` (our bug) and `Unknown` (an unrecognised 4xx, for example a bad
token): halt, degrade and back off, so nothing is retried forever and nothing is silently advanced
past.

`RetryableError` is a transient failure whose retry budget was exhausted. 429/502/503/504 retry
inline; other 5xx including 500/501 do not, and the window is re-pulled next cadence. A 429
`Retry-After` is honoured, and a value exceeding the remaining budget returns immediately instead of
burning attempts on guaranteed 429s.

Any 2xx is success. Grafana Cloud answers 200 on `/v1/metrics` but 204 on `/v1/logs`; accepting only
200 once misclassified every successful logs POST as retryable and the logs loop never advanced.

## Determinism is a correctness precondition, not tidiness

Conditional idempotency needs re-emitted batches to be byte-identical, so `Encode` sorts by name,
unit, label key, then timestamp to defeat map-iteration randomness. Two fixed bugs are guarded:

- Unit is part of the group key, so same-name different-unit samples stay contiguous in one `Metric`.
- Label keys are length-prefixed (`labelKey` emits `5:hello5:world`) so `{"a":"b;c=d"}` and
  `{"a":"b","c":"d"}` cannot collide. Never revert to a naive `k=v;` join.

## Other traps

- `Encode` rejects `Delta` temporality outright: the GC gateway is cumulative-only.
- Payload splitting is both proactive (encoded over `MaxBytes`) and reactive (on 413), by recursive
  midpoint split. A single-sample 413 becomes `ReasonPayloadTooLarge`, which advances past with a
  counted gap, never a halt.
- `redactSecrets` strips token, instance_id and base64 Basic credentials from response bodies before
  they reach error strings. A transparent proxy can echo the request's `Authorization` header into
  its own error body.
- OTLP is hand-encoded with `protowire`, wrapping `ResourceMetrics` as request field 1, to avoid
  importing `collector/*`.
- A 200 carrying an OTLP `partial_success` body (`rejected_data_points` / `rejected_log_records`) is
  still success and the batch advances, but part of it was dropped past the taxonomy. `post()`
  decodes it from the raw pre-redaction bytes and fires `Config.OnPartialReject(plane, n, msg)`. The
  emitter is loop-agnostic and owns no metrics or logging, so the composition root
  (`cmd/genai-otel-bridge/main.go`) wires that to `metrics.ObserveEmitPartialReject`
  (`genai_otel_bridge_emit_partial_success_rejected_total{plane}`) plus a rate-limited warn. No
  confirmed backend emits this form; the in-cluster Alloy topology could.
- `CoalesceDPM` in `coalesce.go` is the stateless per-(series, minute) last-write-wins cap, called
  from `schedule.ProcessBatch` before `splitByBucket`. Its collision-safe key is independent of
  `otlp.labelKey`. Suppressions are counted by the caller as
  `genai_otel_bridge_samples_capped_total{loop,reason="dpm"}`. Survivor timestamps are not re-aligned
  to the minute boundary; that would invent timestamps and risk cross-batch duplicate-timestamp churn.
