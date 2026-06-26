# internal/config — YAML config model, secret substitution, validation

`config.go` holds the config tree, `Load(path)`, and `Validate(known)`. Fixtures in `testdata/`.

## Helm default-config generation (`helm:` tags + `make generate`)

The chart's default `config:` block (`deploy/helm/values.yaml`) is **generated from the structs in
`config.go`** — never hand-edit it. Each field carries a `helm:"..."` render tag alongside its
`yaml:"..."` name: `env=NAME` → `${NAME}`; `default=VALUE` → literal (a `[]string` splits VALUE on
`,`; `Duration` defaults render as quoted strings like `60s`/`10m`); `omit` → excluded;
`key=NAME` → a `map[string]T` field emits one entry under NAME; `instance` → a `[]T` field emits one
example element. **Every field must be covered** — an untagged scalar leaf is a hard generate error
(that's the forcing function). Struct/nested fields with no tag recurse as mappings. Field
**Go doc-comments** become the chart's inline `#` comments (parsed via `go/ast`).

Generator: `internal/config/gen` (binary) → `internal/config/gen/helmgen` (render lib; takes a
`reflect.Type` + the source path, so it does NOT import `config` — no cycle with the in-package gate
test). Run `make generate` (or `go generate ./internal/config/`) after any field/tag/default/
doc-comment change. `TestHelmGeneratedConfigUpToDate` re-renders in-memory and byte-compares the
committed `values.yaml` region; it fails with "run `make generate`" on drift. This **superseded**
the old reflection-parity `helm_alignment_test.go` (a generated file can't drift from its generator).
Note `LoopConfig.BucketSettle` default is **10m** (live-measured; 3m was insufficient).

`LoopConfig.MaxBackfill` default is **90m** (was 55m) — sized to the Grafana Cloud Mimir
`out_of_order_time_window` of 2h, leaving ~30m margin for clock skew + catch-up walk. NOTE: this is
unrelated to the ≤55m Portkey granularity clamp (`maxWindow=55m`, enforced separately in `config.go`).

The Helm example block (`RenderExampleBlock([]helmgen.Example{...})` in `internal/config/gen/main.go`)
renders each source's per-loop `settings` at its package default with vendor-owned per-key head-comments
supplied via each package's `ExampleSettingsComments()`. `TestHelmGeneratedExamplesUpToDate` guards
this block for drift.

## Secret substitution (§4.1 — subtle, don't simplify)

`${VAR}` / `file:/path` refs are replaced with **YAML-safe placeholders** (`genai-otel-bridgeXsecretX<N>Xgenai-otel-bridge`)
*before* parsing, then the real secret is written into the `yaml.Node.Value` post-parse — so it is
**never re-interpreted as YAML**. This prevents a secret containing YAML-special chars (e.g.
`tok # x`) from being parsed as a comment, and unresolved refs from becoming invalid flow-map syntax.
Unknown YAML keys are rejected (`KnownFields(true)`). An unset `${VAR}` is fatal.

**Gotcha — `${VAR}` in a YAML *comment* is still resolved.** `injectEnvPlaceholders` runs the regex
over the **raw text** before parsing, so a `${VAR}` inside a `#` comment matches too — an unset var
there is a fatal startup error. Don't put live env-ref syntax in commented-out example blocks
(testdata fixtures, the helm configmap baked default); use `<env ref>` placeholder prose instead.

## Validation rules (`Validate`, enforced before wiring)

- `emit.telemetry` and optional `emit.self` endpoints must be **https://** (or loopback for dev). The
  loopback check **parses the URL and matches the hostname exactly** — not a prefix — to block
  `localhost.evil.example`. **[CP-M7] `allow_insecure` opt-out** (`validateEmitEndpoint`, mirrors the
  source-side `http.allow_private`): with `emit.{telemetry,self}.otlp.allow_insecure: true` an http
  NON-loopback endpoint is permitted ONLY token-less (a non-empty `instance_id`/`token` on a cleartext
  endpoint is rejected — nothing credential-shaped rides the link; the in-cluster collector holds the GC
  creds) and ONLY private (an IP literal must be RFC-1918/loopback/link-local — a public IP is rejected;
  a DNS host like a k8s Service is permitted, unresolvable at load time). The emitter + self-obs exporter
  both omit `Authorization` when creds are empty. For the EKS in-cluster Alloy receiver topology.
- `emit.self.metric_interval` (Duration, optional) — self-obs PeriodicReader export period. Unset ⇒ 60s
  (provider default). **Must be ≥ 60s** (1DPM emission constraint); sub-minute is rejected. Honoured
  only for `emit.self` — the product plane's rate is gated by the per-loop bucket cadence.
- A source `base_url` must be https:// unless `http.allow_private=true` (in-VPC exception).
- **`ha.checkpoint=file` + `ha.coordinator=lease` is forbidden** (file checkpoint is per-pod, not
  shared — would lose the watermark, CP-H11).
- `source_instance` may not contain `/` (it's the CheckpointKey delimiter).
- **`queue.emit_workers` must be 1** (per-loop single-flight emit, C3).
- Window/cadence math (AR-M-win): `cadence ≥ 10s`, `window ≤ 55m`, and `window` must cover
  `cadence·(1+2·jitterFrac) + bucket_settle` and be `≤ max_backfill`. Constants: `maxWindow=55m`,
  `minCadence=10s`, `jitterFrac=0.10`.

Source/loop config keys: `SourceConfig{Type,Enabled,BaseURL,SourceInstance,Auth,RateLimit,HTTP,Loops}`;
`LoopConfig{Enabled,Cadence,Window,BucketSettle,BootstrapLookback,MaxBackfill,MetricPrefix,Graphs}`.

Top-level governance/logging keys:
- `governance.max_dpm` (int, default 1) — hard DPM cap: drives the product-plane `emit.CoalesceDPM`
  coalesce stage AND clamps the self-obs PeriodicReader interval to `60s/max_dpm`. Both planes enforce it.
- `governance.per_metric_cardinality_budget` (int) — PER-METRIC cardinality cap (distinct label-sets
  per metric name, not global) → `source.GuardConfig.PerSeriesBudget`. Unset/0 ⇒ default 10000 applied
  in `Load` (0 would mean unlimited in the guard); negative is rejected by `Validate`.
- `governance.max_stream_label_keys` (int, default 15) — per-LOGS-loop Loki stream-label budget. The GC
  Loki `max_label_names_per_series` default (15; per-tenant overridable by Grafana staff, so raise the knob
  to match). `internal/app` fails fast at Build if a logs loop's product identity resource attrs (3) +
  `IndexedKeys()` (base ∪ `extra_indexed_fields`) would exceed it — Loki silently drops a stream over the
  limit. Unset/0 ⇒ `DefaultMaxStreamLabelKeys` (15) in `Load` AND defensively at point-of-use in Build
  (struct-built test configs bypass `Load`); negative rejected by `Validate`. Metrics plane is N/A (3
  resource attrs « Mimir 40). `IdentityConfig.ProductIdentity()` is the single source of truth for the
  identity keys (cmd builds the emitter map from it; app counts it). In-cluster-Alloy `k8s.*`/`cloud.*`
  enrichment shares this budget (documented; not enforceable from genai-otel-bridge).
- `governance.allow_label_keys` ([]string, default empty) — EXTRA content-free indexed/label keys the
  operator opts into the guard allow-list, ON TOP OF each enabled source's declared keys
  (`portkey.AllowedLabelKeys()`/`langsmith.AllowedLabelKeys()` — vendor packages own the strings, not
  `app.go`). `app.go` unions vendor + opt-in (deduped) and REJECTS a content-floor key here fail-fast.
  **GS1 limitation** (documented on the field doc-comment → `values.yaml`): a key is allowed past the
  guard but only becomes a queryable Loki **stream label** if it is in the Grafana OTel gateway's default
  label config; any other label needs a Grafana support ticket — until then it lands as structured metadata.
- `log.format` (`logfmt` | `json`; empty ⇒ logfmt) — handler for the integrator's own STDOUT logs
  (scraped by k8s-monitoring → Loki, never OTLP). Built in `internal/logging`; invalid value rejected
  by `Validate`.
- `selfobs.profiling` — opt-in/default-off self-profiling. `enabled` (bool), `mode` (`pull`|`push`,
  empty ⇒ pull), `pull.addr` (default `:6060` when enabled+pull), `push.{endpoint,instance_id,token}`.
  Validated only when `enabled`. push endpoint must be https (or loopback); push requires
  instance_id+token. Cross-checks reject a config that lies about intent: `push.*` set with `mode:
  pull`, or `pull.addr` set with `mode: push`. Maps into `selfobs.ProfilingConfig` in `main.go`.

Tests set env vars before `Load`; `testdata/valid.yaml` is the canonical good config. `secret_test.go`
asserts YAML-special chars survive; `loopback_test.go` asserts URL-parse (not prefix) matching.
