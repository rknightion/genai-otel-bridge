# Loop: genai-otel-bridge
tier: guarded
gate: just check
ci-required: ci-success
release-on-push: yes
deploy-on-push: no
grafana-stack: none

Public repository: `scripts/forbidden-words.sh` blocks deployment-specific identifiers anywhere in
the tracked tree, `backlog/` included. Write the shape, not the instance. Run `just` with stdin from
`/dev/null`. `just ci` adds the two Docker or service-container legs (`e2e`, `test-dynamodb`).

## Credentials

- None held by the loop. `just publish` is confirm-gated and pushes to a real registry: never run it
  or pass `--yes` or `JUST_YES=1`. Config `.env`, `*.local.yaml` and `*.secret.*` are gitignored.

## Traps

- Every push to `main` runs `publish.yml` (edge image and chart); a release PR merge tags and
  publishes. Published artifacts carry no `v` prefix: use `X.Y.Z` in image references.
- A clean local `just check` is weaker than CI on two legs: `tf-validate` self-skips without the IaC
  tools, and `forbidden-words` scans only built-in credential shapes without
  `$FORBIDDEN_WORDS_PATTERN` or `scripts/forbidden-words.local`.
- `ci-success` is the only required check on `main`; Renovate PRs self-automerge once it is green.
- `model.*`, `source.Source` and `source.Loop` are FROZEN seams: a change needs an `ARCHITECTURE.md`
  update. Do not weaken emit-once-after-settle, deterministic encoding or epoch-fenced checkpoints.
- `*_review_test.go` files are regression guards for known attack and race scenarios: keep them.
- Third-party notices and SBOMs are release artifacts outside `just check`; do not gate on them.
- Stack-side Grafana prerequisites are in `deploy/grafana/README.md` (GS1-GS4); the repo cannot set them.
- A config knob exists in four files that move as one lane's job: `internal/config/config.go`
  (schema, `helm:` tags, `Load` defaults), `deploy/helm/values.yaml`,
  `deploy/ecs/terraform/config.example.yaml` and `test/eks/values-eks.yaml`. Config accepted but
  behaviour silently wrong is this codebase's worst failure: a new or changed knob owes a `Validate`
  case and a test asserting the rejection, not only the happy path.
- Doc-vs-code drift is a defect class: a behaviour change sweeps the package `CLAUDE.md`,
  `ARCHITECTURE.md`, `docs/DESIGN.md`, the `docs/*.md` page, the `values.yaml` comment and any alert
  or dashboard naming the metric. The twelve package `CLAUDE.md` files load recursively only in
  Claude Code: give a Codex lane the package doc's path explicitly.
- A diff touching `internal/source/guard.go`, the denylist wiring in `internal/app` or an
  `extra_*_fields` path is a content-egress change and is reviewed as one however small. The
  `*_content_gate_test.go` files are regression guards: extend them, never relax them.
- Parallel implementations drift: the two backends of each HA seam (`checkpoint/{configmap,dynamodb}`,
  `coordinate/{lease,dynamodb}`) and the metrics and logs planes. A lane changing one states what it
  checked in the sibling.
- A brief saying "it is already tested" means verify the test fails when the behaviour breaks: HTTP
  fixtures encoded from the decode struct itself cannot catch a field-name regression.
- If you add a CI leg, add it to the matching just recipe in the same change. A green fork PR is
  not evidence the identifier sweep passed: the real identifier list is absent there.

## Mutexes

- One lane at a time: the k3d cluster (`just k3d-up`, `just e2e`, `just k3d-down`, one named
  cluster per machine), the EKS test environment (`test/eks/`, real spend) and `.tools/`
  (`just setup`, concurrent installs corrupt it).
- Live vendor keys: Portkey workspace scope is key-bound, and the LangSmith rate budget is
  tenant-wide (about 10 requests per 10 s, shared by every loop and lane). No lane or test calls a
  live vendor API unless the charter names it for that lane; tests use `httptest.Server` fakes and
  injectable clocks.
- Single owner: `internal/app/app.go` (composition root, wiring only), the four config files above,
  `internal/source/*/signals.go` plus `labels.go` (`SeriesNames` and `ValidateOwnership` are the only
  cross-loop series-collision gate), `justfile` plus `.github/workflows/ci.yml`, and the design
  record (`ARCHITECTURE.md`, `docs/DESIGN.md`, `followup.md`).

## Lane brief

Stop and return the question for: a FROZEN-seam change, deleting a review-guard assertion, a new
emitted label key or series name (a cardinality decision), anything that could put vendor content
on the wire, or a live vendor key you were not given. Decide everything else and record it in the
task notes. Cite closed pre-migration work as `#NNN` and new work as `gob-NNNN`.
