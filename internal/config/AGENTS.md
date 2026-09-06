# internal/config - config model, secret substitution, validation

`config.go` holds the config tree, `Load(path)` and `Validate(known)`. Fixtures in `testdata/`;
`testdata/valid.yaml` is the canonical good config. Tests set env vars before `Load`.

## This package generates two committed artifacts. Never hand-edit either.

`just gen` (or `go generate ./internal/config/`) renders, from the structs in `config.go`:

- the `config:` block in `deploy/helm/values.yaml`, plus its commented per-source example region
- `deploy/ecs/terraform/config.example.yaml`

Run it after **any** field, tag, default or doc-comment change. `TestHelmGeneratedConfigUpToDate`,
`TestHelmGeneratedExamplesUpToDate` and `TestECSConfigExampleUpToDate` re-render in memory and
byte-compare the committed files, failing with "run `just gen`".

### Render tags

Each field carries a `helm:"..."` tag beside its `yaml:"..."` name: `env=NAME` renders `${NAME}`;
`default=VALUE` renders a literal (a `[]string` splits VALUE on `,`; a `Duration` renders as a quoted
string like `60s`); `omit` excludes it; `key=NAME` makes a `map[string]T` emit one entry under NAME;
`instance` makes a `[]T` emit one example element. Struct fields with no tag recurse as mappings.
**An untagged scalar leaf is a hard generate error** - that is the forcing function that stops a new
knob from being invisible in the chart. Go doc-comments become the chart's inline `#` comments,
parsed via `go/ast`.

The generator is `internal/config/gen` (binary) over `internal/config/gen/helmgen` (render library).
`helmgen` takes a `reflect.Type` plus the source path and deliberately **does not import `config`**,
so the in-package gate tests do not create an import cycle.

### The ECS profile

`helmgen.RenderECSConfigFile` runs the same schema under a `helmgen.Profile`. `Include` force-renders
specific `helm:"omit"` paths (keyed `"StructName.FieldName"`), `Defaults` overrides a tagged scalar.
`helmgen.ECSProfile()` flips `ha.coordinator` and `ha.checkpoint` to `dynamodb` and force-includes the
`ha.dynamodb` block at its defaults. Output is a bare config document (no `config:` wrapper) that the
ECS module injects verbatim as `GENAI_OTEL_BRIDGE_CONFIG`, followed by a commented all-loops example
block.

- **The example block's `${VAR}` refs are neutralised to `<VAR>` (`neutralizeEnvRefs`), and that is
  load-bearing.** The whole file is parsed by `config.LoadBytes`, which resolves `${VAR}` even inside
  a `#` comment, so a live `${LANGSMITH_API_KEY}` in the commented block would force that env var on
  a Portkey-only deployment.
- The active ECS config stays minimal (one source, one loop) because that is the only shape that
  starts with credentials alone; everything else needs real per-deployment settings and so lives in
  the commented block.
- The zero `Profile` is byte-identical to the un-profiled path, guarded by
  `TestRenderTypeProfile_ZeroProfileMatchesRenderType`, so `values.yaml` is unaffected.
- `TestECSProfileDefaultsMatchLoad` ties the profile's hard-coded dynamodb default strings to the
  `defaultDynamoDB*` consts `Load` actually applies. The consts are the single source of truth;
  `helmgen` cannot import `config`, so that test is what closes the gap.

## Secret substitution (subtle, do not simplify)

`${VAR}` and `file:/path` refs are replaced with **YAML-safe placeholders before parsing**, and the
real secret is written into the `yaml.Node.Value` post-parse, so it is never re-interpreted as YAML.
That is what stops a secret containing YAML-special characters (`tok # x`) from being parsed as a
comment, and an unresolved ref from becoming invalid flow-map syntax. Unknown YAML keys are rejected
(`KnownFields(true)`). An unset `${VAR}` is fatal.

**A `${VAR}` inside a YAML comment is still resolved.** `injectEnvPlaceholders` runs its regex over
the raw text before parsing, so an unset var in a commented-out example block is a fatal startup
error. Use `<env ref>` placeholder prose in example blocks, never live syntax.

## Validation rules worth knowing before you touch them

- `emit.telemetry` and `emit.self` endpoints must be **https**, or loopback for dev. The loopback
  check **parses the URL and matches the hostname exactly**, not by prefix, so `localhost.evil.example`
  is blocked.
- **`allow_insecure` opt-out** (for the in-cluster Alloy receiver topology): a cleartext non-loopback
  endpoint is permitted only **token-less** (a non-empty `instance_id`/`token` on it is rejected -
  nothing credential-shaped rides the link, the in-cluster collector holds the Grafana Cloud
  credentials) and only **private** (an IP literal must be RFC-1918, loopback or link-local; a DNS
  host such as a Service name is permitted since it is unresolvable at load time). The emitter and
  self-obs exporter both omit `Authorization` when credentials are empty. This mirrors the
  source-side `http.allow_private`.
- **`emit.telemetry.metric_interval` is rejected outright.** `MetricInterval` sits on the shared
  `OTLPTarget` so it parses under `KnownFields`, but it is never read and never honoured on the
  product plane. Rejecting it is deliberate: a dead knob is a config that lies about intent.
- `emit.self.metric_interval` must be at least `60s / max_dpm` (unset means 60s, the provider
  default). The product plane's rate is gated by the per-loop bucket cadence instead.
- **`ha.checkpoint=file` with `ha.coordinator=lease` is forbidden** - a file checkpoint is per-pod,
  not shared, so a restart loses the watermark. `ha.coordinator=dynamodb` requires
  `ha.checkpoint=dynamodb` (one table backs both).
- `source_instance` may not contain `/`: it is the `CheckpointKey` delimiter, and it namespaces the
  watermark, so keep it stable across deployments.
- **`queue.emit_workers` must be 1** - per-loop single-flight emit.
- Window and cadence math: `cadence >= minCadence`, `window <= maxWindow`, and `window` must cover
  `cadence*(1+2*jitterFrac) + bucket_settle` and be `<= max_backfill`. The three constants are at the
  top of `config.go`.
- A source `base_url` must be https unless `http.allow_private=true` (the in-VPC exception).

## Defaults applied in `Load` (0 is never a safe silent value here)

- `max_dpm` 1 - 0 would mean "emit nothing".
- `per_metric_cardinality_budget` 10000 - this is a **per-metric** cap (distinct label sets per metric
  name, not global); 0 would mean unlimited in the guard. Negative is rejected.
- `max_stream_label_keys` 15, the Grafana Cloud Loki `max_label_names_per_series` default (Grafana
  staff can raise it per tenant, in which case raise this knob to match). `internal/app` re-applies
  the default at point of use because struct-built test configs bypass `Load`. The metrics plane is
  not affected: 3 resource attributes against Mimir's 40.
- `queue.max_batches` 256 and `queue.max_batch_bytes` 1 MiB. At 0, `max_batches` falls through to the
  runner's depth-1 clamp (about a minute of buffering instead of the documented hours) and
  `max_batch_bytes` disables the emitter's proactive over-cap split, leaving only the reactive 413
  path. The runner and emitter clamps stay as defence-in-depth for struct-built configs.
- `bucket_settle` 10m, live-measured. 3m was insufficient.
- `max_backfill` 90m, sized to the Grafana Cloud Mimir `out_of_order_time_window` of 2h with margin
  for clock skew and catch-up walk. **Unrelated to the 55m `maxWindow` granularity clamp**, which is
  enforced separately.

## Other keys

- `governance.allow_label_keys` - extra content-free indexed/label keys the operator adds on top of
  each vendor package's declared keys. `internal/app` unions and dedupes them and rejects a
  content-floor key fail-fast. **GS1 limitation**, documented on the field doc-comment so it reaches
  `values.yaml`: a key allowed past the guard only becomes a queryable Loki *stream label* if it is
  in the Grafana OTel gateway's default label config; anything else needs a Grafana support ticket
  and until then lands as structured metadata.
- `log.format` (`logfmt` | `json`, empty means logfmt) - the handler for the service's own stdout
  logs, which are scraped to Loki, never sent over OTLP. Built in `internal/logging`.
- `selfobs.profiling` - opt-in, default off. Validated only when `enabled`, and the cross-checks
  reject a config that lies about intent: `push.*` set with `mode: pull`, or `pull.addr` set with
  `mode: push`.
