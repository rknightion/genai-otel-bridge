# Loop: genai-otel-bridge
tier: guarded
gate: just check
ci-required: ci-success
release-on-push: yes
deploy-on-push: no
receiver: https://loopwatch.m7kni.com
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

## Mutexes

None recorded.
