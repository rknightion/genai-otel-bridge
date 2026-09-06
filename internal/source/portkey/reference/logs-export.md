# logs_export: knobs, download path and failure handling

Read before setting or adding a `logs_export` knob. Defaults live in `defaultLogsSettings()`
(`logs_settings.go`) and are rendered into the Helm example, so this file covers only what the
defaults do not tell you.

## Required, no default

`workspace_id` and `signed_url_allow_hosts` are deliberately absent from the default set so an unset
config fails validation loudly rather than exporting unscoped or fetching from an unvalidated host.

Minimal block:

```yaml
loops.logs_export:
  enabled: true
  cadence: 60s
  settings:
    workspace_id: ws-...
    signed_url_allow_hosts: signed-url-host.example.com
    window: 1h
    settle: 10m
```

## `requested_data` is not an egress filter

Portkey injects `metadata` and `portkeyHeaders` regardless of what is requested, so the strip
allow-list is the authority. Naming a content or PII field in `requested_data` still fails fast: it
cannot widen what is emitted, but asking for it at all is a content-minimisation violation.

## The four opt-in keys

- **`extra_record_fields`** (csv) appends content-free fields to the strip's RECORD allow-list and to
  `requested_data`. It is NOT empty by default: `cache_status` ships on.
- **`extra_indexed_fields`** (csv) routes a field into the INDEXED tier, is merged into
  `requested_data`, and is auto-allow-listed in the guard so a promotion cannot be silently dropped.
  A startup WARN flags the cardinality blast radius. GS1-gated to be queryable as `{label=...}`.
- **`metadata_record_fields`** (csv) lifts named sub-keys OUT of the hard-denied `metadata` blob into
  the RECORD tier. This is the **only** sanctioned path into `metadata`. Only scalar, non-hard-denied
  sub-keys are lifted; the rest of the blob stays dropped. It is deliberately NOT added to
  `requested_data` - the blob is never asked for, it is extracted client-side from what Portkey
  injects.
- **`metadata_trace_id_field`** / **`trace_id_field`** name the one key whose UUID populates
  `model.LogRecord.TraceID`, giving OTLP `trace_id` for logs-to-traces correlation (ledger #15). The
  first names a metadata sub-key and is auto-lifted into the record tier; the second names a
  top-level export field and is auto-unioned into the record allow-list and `requested_data`. **They
  are mutually exclusive** - both feed the single OTLP trace_id, and setting both is rejected
  fail-fast. Each takes a SINGLE key; a comma is rejected (use `metadata_record_fields` for several).

A non-UUID value leaves `TraceID` unset but still ships the attribute, and the record is counted via
`OnGraphSkipped(logs_export, "trace_id_unparsed")` so a systematically broken upstream format is
alertable. Expect a non-trivial unparsed fraction where the native `trace_id` doubles as a free-text
caller label.

## Hard-denied everywhere

`hardDeniedLogFields` is `source.AbsoluteNeverDenyKeys()` plus the Portkey content fields
(`contentRequestKeys`, which includes a bare `prompt`). It gates every one of the keys above, so the
fail-fast mirror cannot drift from the guard backstop.

## Sizing

`page_size` is capped at Portkey's hard ceiling of 50000. `chunk_max_records` bounds memory per
`Collect` step, and `job_poll_timeout` must stay generous because a full 50k page takes on the order
of tens of minutes to generate server-side. Tune `chunk_max_records` and `window` together for
high-traffic windows: an over-size window parks in phase `blocked` rather than retrying.

## Producer identity

`strip` stamps `RecordAttributes["source"] = "portkey"` at RECORD tier, so portkey and langsmith log
data are distinguishable in Loki with no stream-label budget cost. The langsmith runs strip mirrors
it with `"langsmith"`.

## GS1 is a ship prerequisite, not a code blocker

The indexed attrs (`ai_org`, `ai_model`, `response_status_code`) need stack-side Loki stream-label
promotion to be queryable as `{label=...}`. Until then they land as structured metadata.

## Download, strip and failure paths

- **Signed-URL SSRF (§7).** The download URL is a SERVER-controlled input. `validateSignedURLHost`
  exact-matches `settings.signed_url_allow_hosts` **and requires an `https` scheme** before any fetch.
  The dlClient egress guard uses the same host list but checks host and IP, NOT scheme - that is why
  both exist. A non-https signed URL is refused unless `http.allow_private` is set (the in-VPC
  carve-out), because otherwise the object and its embedded `X-Amz-Signature` credential transit
  cleartext.
- **Signed-URL secret redaction.** A transport error on the object download is a `*url.Error` whose
  string embeds the FULL URL, and the signed query is a live bearer credential. Every download
  transport and request-build error goes through `httpx.RedactURLError`, so the signature can never
  reach slog, stdout and Loki. The non-2xx path uses `httpx.ErrSnippet` (bounded body, no URL).
- **The chunker must not use `bufio.Scanner`.** `logs_download.go` uses a bounded `readLine` reader:
  the page file is never buffered whole, `page_offset_done` lines are skipped, and at most
  `chunk_max_records` are taken. `bufio.Scanner` aborts the WHOLE scan with `ErrTooLong` on one
  over-long line and cannot advance past it, which permanently stalled the window.
  `page_offset_done` counts LINES, and both a malformed line and an over-long line (>1 MiB) are
  skipped loudly yet still advance the offset, so the loop can never wedge re-reading the same bytes.
  An over-long line is drained and discarded, never parsed or stringified - it may be content-bearing.
- **HTTP Range resume.** The cursor carries a line-boundary `page_byte_offset` alongside
  `page_offset_done`, and `downloadChunk` sends `Range: bytes=<offset>-` so a `>chunk_max_records`
  page does not re-GET the whole object per chunk. A server or CDN that ignores Range is detected via
  the status and `Content-Range` and falls back to the line-skip, so correctness never depends on
  Range being honoured.
- **Failure honesty.** Failed, stopped and stuck jobs log at error level AND fire
  `Deps.OnGraphSkipped` (`export_failed`/`export_stuck`); skipped lines and unparseable trace-id
  values reuse the same self-metric (`line_oversize`, `line_unparseable`, `trace_id_unparsed`), so a
  systematic upstream format change that drops 100% of records is alertable rather than a warn line
  nobody sees. A download over the cap errors loudly rather than truncating silently.
- **An over-size window PARKS rather than retrying.** Exceeding `max_pages_per_window` moves the
  cursor to phase `blocked`: the draft export is created AT MOST ONCE, each later tick re-raises the
  loud error from cursor state without re-creating, and `window_oversize` fires once on entry.
  Portkey has no delete API and cancel is invalid on a draft, so re-creating every tick would spam
  orphaned drafts and burn rate tokens. It clears automatically once `window` is shrunk.

