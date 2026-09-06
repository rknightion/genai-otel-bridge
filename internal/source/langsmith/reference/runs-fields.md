# runs loop: which fields may be opted in

Read before adding a key to `settings.extra_record_fields` or `settings.extra_indexed_fields`, or
before widening the default `select` projection.

Every candidate must also appear in `validLangsmithSelectEnum` (`runs_strip.go`), or the whole
`runs/query` 422s. `validateRunsSettings` rejects an out-of-enum key at config load.

## A - default-emitted, content-free

Indexed: `run_type`, `status`. `trace_tier` was deliberately dropped from the indexed tier - it is
NULL at scale, so it burned a Loki stream-label slot for nothing; re-add it via
`extra_indexed_fields` if a deployment actually populates it.

Record: `id`, `trace_id`, `session_id`, `parent_run_id`, `thread_id`, `start_time`, `end_time`,
`first_token_time`, `total|prompt|completion_tokens`, `total|prompt|completion_cost`, `dotted_order`,
plus the on-by-default operational extras `tags`, `child_run_ids`, `app_path`.

`execution_order` is NOT a valid `select` value on 0.13.5 and 422s the whole query. Do not re-add it.

## B - operational, safe to opt in

Scalars: `reference_example_id`, `reference_dataset_id`, `in_dataset`, `price_model_id`,
`ttl_seconds`, `trace_upgrade`, `last_queued_at`, `trace_first_received_at`,
`trace_min_max_start_time`.

Arrays (rendered as csv): `parent_run_ids`.

Objects, still dropped by the scalar-only strip whether or not they are opted in:
`*_token_details`, `*_cost_details`, `feedback_stats`.

**`share_token` is an access token. Never opt it in**, despite it being a valid `select` value and a
plain scalar.

## C - content or free text

Opting one of these in is an explicit content decision, not a configuration tweak:
`events`, `extra`, `serialized`, `manifest`, `error`, `name`.

Hard-denied and NOT opt-in-able at all: `inputs`, `outputs`, `messages`, `inputs_preview`,
`outputs_preview` (previews are truncated renderings of the real prompt and response, so they sit on
the never-subtractable floor `source.AbsoluteNeverDenyKeys`, #95), and `inputs_s3_urls` /
`outputs_s3_urls` (signed URLs = a body plus a credential, via `hardDeniedRunsFields`). All are
rejected fail-fast at config load.

## Indexed-tier promotions

`extra_indexed_fields` routes a content-free field into the strip's INDEXED tier and the composition
root auto-allow-lists it in the guard, so a promotion cannot be silently dropped. A startup WARN
flags the cardinality blast radius; the per-loop budget is the runtime backstop. Indexed attrs are
GS1-gated: until the stack promotes them to Loki stream labels they land as structured metadata and
are not queryable as `{label=...}`.
