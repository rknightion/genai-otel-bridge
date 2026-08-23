# Contributing to genai-otel-bridge

Thanks for your interest in contributing. This document covers how to build, test, and submit
changes.

## Ground rules

- **Decoupled by design.** No customer-, vendor-deployment-, or domain-specific knowledge belongs in
  core code or defaults. Metric names, label keys, endpoints, cadences, windows, and environment
  identifiers are all configuration. Vendor-specific code lives only in its `internal/source/<vendor>`
  package behind the common interface.
- **Content-free is a release gate, not a preference.** The service must never request prompt/response
  bodies, and the outbound field allow/deny-list must keep governing every emitted field. Changes that
  weaken this will not be accepted.
- **Operationally honest.** Every polling/emit gap or skipped sample must remain alertable — never
  silent.

## Development setup

Requires **Go 1.27+**. The single green-bar command is:

```bash
make gate     # vet + test + lint + spdx-check + build
```

Other useful targets:

```bash
make build    # -> bin/genai-otel-bridge (version stamped via git describe)
make test     # go test ./...
make lint     # golangci-lint run
go test -tags acceptance ./internal/app/   # acceptance gates (failover / outage / soak)
```

`make gate` must pass before any change is merged. CI runs the same gate plus a k3d end-to-end test.

## Making a change

1. Fork the repository and create a topic branch.
2. **Write tests first** (TDD): a failing test, then the minimal code to make it pass. Table-driven
   tests where they fit; `httptest.Server` fakes for HTTP; injectable clocks for determinism. Tests
   must not make live network calls.
3. Every new `.go` file must carry the license header:
   `// SPDX-License-Identifier: AGPL-3.0-only` (enforced by `scripts/spdx-check.sh`).
4. Keep `make gate` green.
5. Open a pull request with a clear description of the change and its motivation.

## Commit messages

This project uses [Conventional Commits](https://www.conventionalcommits.org/) — the subject line
drives the generated changelog. Use `feat:`, `fix:`, `docs:`, `refactor:`, `chore:`, etc. Mark
breaking changes with a `!` (e.g. `feat!:`) and a `BREAKING CHANGE:` footer.

## Releases

Releases are automated with [release-please](https://github.com/googleapis/release-please): once
changes land on `main`, it opens a release PR that bumps the version + `CHANGELOG.md` from the
Conventional Commits. Merging that PR publishes the GitHub Release and the image/chart. Maintainers
cut releases — contributors only need correct commit subjects (`feat`/`fix`/breaking drive the version).

## Frozen interfaces

Some types and interfaces are marked **FROZEN** in `ARCHITECTURE.md` (the `model.*` types and the
`source.Source` / `source.Loop` interfaces). Adding, renaming, or removing fields/methods there is a
design change that requires an `ARCHITECTURE.md` update and discussion first — not a casual edit.

## License

By contributing, you agree that your contributions are licensed under the
[GNU Affero General Public License v3.0 only](./LICENSE) (`AGPL-3.0-only`), consistent with the rest
of the project. See [LICENSING.md](./LICENSING.md).

## Cloud agent environments

Codex Cloud and Claude Code cloud tasks share a repository-owned setup script so their agent phases
have the same tools as CI, including the Backlog.md task tracker. Configure this command as the
**Manual setup** command in Codex Cloud or the **Setup script** in a Claude Code cloud environment:

```bash
bash scripts/cloud-environment-setup.sh
```

For Codex Cloud, set the environment's Go package version to the `go` version declared in `go.mod`.
Do not run the script in a local development environment. It is idempotent for cached cloud
containers, persists user-installed tool paths for the separate agent shell, prefetches Go modules and
envtest assets, and installs the tools used by `make gate`. Setup scripts have network access; keeping downloads in setup also lets the agent phase run with its default
restricted network access.
