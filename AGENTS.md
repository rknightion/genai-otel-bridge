# genai-otel-bridge

Vendor-neutral Go service that polls AI-platform APIs (LLM gateways, eval platforms) and emits
**operational** telemetry to Grafana Cloud as OTLP metrics and logs. It sits on the production
observability critical path: leader-elected, single-emit, self-observing, resilient to downstream
slowness.

## Hard rules (do not violate)

- **Decoupled.** No vendor, customer or domain knowledge in core code or defaults. Metric names,
  label keys, endpoints, cadences, windows and env identifiers are all config. Vendor code lives
  only in its `internal/source/<vendor>` package behind the common interface.
- **Content minimisation is a release gate.** Never request message bodies, prompts or completions.
  An outbound field allow/deny-list governs *every* emitted field (labels, log body, metadata), so
  content cannot leak via non-label fields. `internal/app` wires the denylist; `source.Guard`
  enforces label allow-listing, default-deny (an empty allow-list denies all labels).
- **Conditional gap-free is engineered, not assumed.** It emerges from emit-once-after-settle plus
  deterministic byte-identical encoding plus monotonic, lease-epoch-fenced checkpoint writes. Do not
  weaken any of the three expecting the others to compensate.
- **Operationally honest.** Every polling or emit gap and every skipped sample is alertable
  (`window_lag`, `samples_skipped_total`), never silent. A non-retryable reject *advances past* the
  bad bucket with a counted gap so the loop always progresses; it never stalls or drops silently.
- **FROZEN seams.** `model.*` types and `source.Source` / `source.Loop` are marked FROZEN. Adding or
  renaming a field or method there is a design change requiring an `ARCHITECTURE.md` update.

## Architecture

`Source.Loop.Collect` (pull a bounded window forward from the watermark) to `model.Batch` to
`source.Guard.Sanitize` (cardinality and content governance) to `schedule.LoopRunner` (single-flight,
bounded queue, epoch-fenced checkpoint) to `emit.Emitter` (deterministic OTLP encode plus retry).

Read `ARCHITECTURE.md` (durable seams, decision ledger) and `docs/DESIGN.md` (build spec, F1-F47
failure handling) before changing a seam.

## Task interface

`just check` is the gate. `just ci` adds the two legs needing Docker or a service container
(`e2e`, `test-dynamodb`). Repo deltas beyond the standard task-surface rule:

- Run `just` with stdin from `/dev/null`. `publish` is `[confirm]` and pushes to a real registry.
  Never pass `--yes` or `JUST_YES=1`.
- `just check` covers every leg of ci.yml's `hygiene` job (`forbidden-words`, `spdx-check`,
  `helm-lint`, `tf-validate`), so a local green gate implies a green hygiene leg.
- `tf-validate` self-skips when the IaC tools are absent, and `forbidden-words` scans only the
  built-in credential shapes when neither `$FORBIDDEN_WORDS_PATTERN` nor
  `scripts/forbidden-words.local` is present. A clean local run is weaker than CI's on both.
- The e2e utility image must be flattened before k3d import on both hosted Docker and
  Docker-out-of-Docker. Placement does not identify a containerd-backed image store; a
  successful pull alone does not prove its saved OCI index contains every referenced blob.
- `ci-success` is the only required check on `main`; the individual CI legs are not, so adding or
  renaming a leg never touches branch rules. Renovate PRs self-automerge, majors included, once
  `ci-success` is green.

Off the Mac, when the remote gate runner is installed (`which gaterun`): `gaterun -- just ci` (or
`just check`), paced across lanes with `gaterun-slot <lane> -- gaterun -- just ci`; `.gaterunner.yaml`
is its contract. Run one test or package locally.

## Conventions

- **Conventional Commits.** Subjects drive the release-please `CHANGELOG.md`. Only `feat`, `fix` and
  breaking changes bump the version; `chore`, `style` and `test` are hidden from the changelog.
- No live network in tests. `httptest.Server` fakes for HTTP, injectable clocks
  (`SetLoopClockForTest`) for determinism.
- **`*_review_test.go` files encode specific adversarial-review findings** (tagged `[ext-review-14]`,
  `CP-R3b`, `Cdx-C14`). They are regression guards for known attack and race scenarios; keep them.
- Config resolves `${ENV}` and `file:/path` refs at load time; `.env`, `*.local.yaml` and
  `*.secret.*` are gitignored.
- `scripts/forbidden-words.sh` blocks deployment-specific identifiers anywhere in the tracked tree,
  including `.github/tracker/history/`. The repo is public: write the shape, not the instance
  (`<vendor>/<workspace>/<loop>`, "the tenant's second workspace"). Counts, timings and structural
  findings are fine.

## Release

release-please maintains a release PR from the Conventional Commits on `main`; merging it tags
`vX.Y.Z`, creates the GitHub Release and triggers `publish.yml`. There is no manual changelog or tag
step.

- **Version is single-source:** chart `version` = `appVersion` = release version, kept in step by the
  two `# x-release-please-version` lines in `deploy/helm/Chart.yaml`.
- **Published artifacts carry no `v` prefix.** The git tag is `vX.Y.Z`, but `docker/metadata-action`
  inside the shared `container-publish.yml` strips it, so image tags are
  `ghcr.io/rknightion/genai-otel-bridge:X.Y.Z` (plus `:X.Y`, `:X`, `:latest`). Use the unprefixed
  form in `--set image=...` and registry references.
- **Runtime trust:** all Dockerfiles use pinned distroless Debian 13 bases for the eMudhra trust-store update. Keep production, Kaniko and e2e helper runtimes aligned.
- **Third-party notices and SBOMs are release artifacts, deliberately outside `just check`.**
  `just notices` regenerates from the real import graph and churns on every dependency bump, so
  gating on it would break Renovate automerge. SBOMs come from syft inside `container-publish.yml`,
  scanning the built image rather than the local binary. See `LICENSING.md`.
- `scripts/publish.sh` is a local manual fallback, **not** the CI path.

## Tracking work

Work is tracked in this repository's GitHub issues; they are the only queue. Do not add a checklist
file such as `ROADMAP.md` beside them.

- A pull request body says how it relates to an issue, on a line of its own: `Closes #N` with the
  `Acceptance evidence` table from `.github/pull_request_template.md` (one row for each acceptance
  criterion of that issue), `Refs #N` when it only relates to one, or `Task: none, <reason>`.
- A pull request that changes `.github/workflows/` or `.github/actions/` carries no closing keyword:
  write `Task: none, <reason>`, with `Refs #N` beside it if you like.
- Contributors never need the project board or the tracker tooling; a maintainer handles both.
- Issues and pull requests are public, so they carry no real identifiers: no email addresses,
  handles, account or tenant ids, host names, addresses or credentials. Write the shape, not the
  instance.
- Older commits, docs and tests cite legacy board ids such as `GOB-0001`. `just issue <ID>` prints
  the issue each one became; `.github/tracker/id-map.json` is the map. A bare `#N` of 313 or lower
  is a pull request, #2 (the Renovate dashboard) or one of the early issues GitHub no longer has
  (`archive/README.md`).

## Deeper references

- `deploy/grafana/README.md` - read before querying the emitted metrics or changing an alert. Also
  carries GS1-GS4, the Grafana-staff stack-side prerequisites this repo cannot set.
