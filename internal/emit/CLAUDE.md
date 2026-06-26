# internal/emit — Emitter seam + deterministic OTLP exporter

`emit.go` defines the `Emitter` interface and the reject/retry error taxonomy; `otlp/` is the concrete
OTLP/HTTP exporter with a hand-rolled protobuf encoder.

```go
type Emitter interface { Emit(ctx context.Context, b model.Batch) error }
```

## Reject taxonomy (drives the scheduler's advance/halt decision — F9/CP-C7)

`RejectError.AdvancesPast()`:
- `DuplicateTimestamp`, `TooOld`, `PayloadTooLarge` → **true**: scheduler advances watermark past the
  bucket with a counted gap, loop progresses.
- `BadEncoding` (our bug) → **false**: HALT + alert, no advance (prevents silent loss).
- `Unknown` (unrecognised 4xx, e.g. bad token) → **false**: HALT + degrade + back off — *not* retried
  forever, *not* silently advanced.

`RetryableError` = transient (5xx/429/transport) with the retry budget exhausted. 429/502/503/504 retry
inline; 500/501 do **not** (re-pull next cadence).

## Determinism is a correctness precondition (§4.4 — not just tidiness)

Conditional idempotency requires re-emitted batches to be **byte-identical**. `Encode` therefore sorts
by name → unit → label-key → timestamp, defeating Go map-iteration randomness. Two subtle bugs already
fixed and guarded:
- **Unit is part of the group key** (`ext-review-10`): same-name/different-unit samples must stay
  contiguous in one `Metric`.
- **Label keys are length-prefixed** (`ext-review-11`): `labelKey` emits `5:hello5:world` so
  `{"a":"b;c=d"}` and `{"a":"b","c":"d"}` can't collide. Never revert to a naive `k=v;` join.

## DPM cap (product plane)

`coalesce.go` = `CoalesceDPM`: stateless per-(series,minute) LWW coalesce stage (collision-safe key independent of `otlp.labelKey`); called from `schedule.ProcessBatch` before `splitByBucket`. Suppressions counted by the caller via `genai_otel_bridge_samples_capped_total{loop,reason="dpm"}`.

## Other gotchas

- `Encode` **rejects `Delta` temporality** (GC gateway is Cumulative-only).
- Payload splitting (CP-C11): proactive (encoded > `MaxBytes`) and reactive (on 413), recursive midpoint
  split. A single-sample 413 → `RejectError` (malformed, not oversized).
- `redactSecrets` strips token/instance_id/base64 Basic creds from response bodies before they enter
  error strings (`ext-review-7`) — defence against proxies echoing auth.
- OTLP is hand-encoded with `protowire` (wraps `ResourceMetrics` as request field 1) to avoid
  `collector/*` imports.

Tests: error-taxonomy table tests, `httptest.Server` integration (auth, retry/backoff, 4xx
classification, secret redaction), determinism under map shuffle.
