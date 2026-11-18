# GenAI OpenTelemetry bridge review guide

## What this is

This Go service polls AI-platform operational APIs and emits bounded product metrics and logs through OpenTelemetry. `cmd/genai-otel-bridge/` and `internal/app/` assemble the sources, scheduler, checkpoints and emitters.

## Invariants

- Fetch only operational data. Prompts, completions and message bodies must stay out of requests and emissions. Check every label, log field and metadata field through `internal/source/guard.go` and the denylist in `internal/app/`. An empty label allowlist denies all labels.
- Preserve emit-once-after-settle, byte-identical OTLP encoding and monotonic, lease-epoch-fenced checkpoints. Keep polling single-flight, queues bounded and discarded data counted. Known per-record rejects advance with a counted gap. Terminal request failures preserve the watermark and degrade the loop.
- `model` and the `Source` and `Loop` interfaces are frozen seams. Update `ARCHITECTURE.md` for deliberate contract changes. Keep configuration defaults and validation aligned across the Go loader, Helm, ECS and EKS examples.

## Where to look hardest

- Treat source filters, optional extra fields, and the product metrics and logs as one content egress boundary. Use the content gate tests and independently written vendor fixtures. Check the metrics and logs paths when either changes.
- Follow replay and leadership changes through `internal/schedule/`, `internal/checkpoint/`, `internal/coordinate/` and `internal/emit/otlp/`. Check both HA backends, partial acceptance, post-settle revisions and leadership loss. Keep the adversarial regression tests with their covered behavior.
- Check chart or deployment changes against the corresponding defaults, config validation tests and the selected coordination and checkpoint backend.

## Checks already covered

The repository gate covers formatting, build, lint, unit, race and acceptance tests, pinned envtest, generated-file drift, secret and identifier scans, Helm and Terraform validation when the required inputs are available. CI also runs dependency review and the service-container paths.

## Verification

Run `just setup` and `just check </dev/null>`. Use `just ci </dev/null>` for the Docker end-to-end and DynamoDB legs. Report skipped tooling or scan inputs. Use fake vendor endpoints and injectable clocks for behavior tests.
