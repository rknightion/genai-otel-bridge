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

Requires **Go 1.26+**. The single green-bar command is:

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

## Frozen interfaces

Some types and interfaces are marked **FROZEN** in `ARCHITECTURE.md` (the `model.*` types and the
`source.Source` / `source.Loop` interfaces). Adding, renaming, or removing fields/methods there is a
design change that requires an `ARCHITECTURE.md` update and discussion first — not a casual edit.

## License

By contributing, you agree that your contributions are licensed under the
[GNU Affero General Public License v3.0 only](./LICENSE) (`AGPL-3.0-only`), consistent with the rest
of the project. See [LICENSING.md](./LICENSING.md).
