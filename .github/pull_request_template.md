<!--
Thanks for contributing! Please keep PRs focused. See CONTRIBUTING.md.
Do not include secrets, tokens, or any prompt/response content.
-->

## What & why

<!-- What does this change do, and what problem does it solve? -->

## Checklist

- [ ] `just check` is green (fmt + build + vet + lint + gen + tests + hygiene)
- [ ] Tests added/updated (TDD: failing test first), no live network in tests
- [ ] New `.go` files carry the `SPDX-License-Identifier: Apache-2.0` header
- [ ] Conventional Commit title (`feat:` / `fix:` / `docs:` / … ; `!` for breaking)
- [ ] No customer/vendor/domain specifics added to core code or defaults (kept configurable)
- [ ] Content-free invariant preserved (no prompt/response bodies; field allow/deny-list intact)
- [ ] `ARCHITECTURE.md` updated if a FROZEN type/interface changed

## Notes for reviewers

<!-- Anything reviewers should focus on, risks, follow-ups. -->

## Issue

<!-- Contributors write `Refs #<number>` on a line of its own for the issue the change works on: no closing keyword. Maintainers write `Closes #<number>` on a line of its own for each issue of this repository the change completes, with the table below. A change under `.github/workflows/` or `.github/actions/` never carries a closing keyword: write `Task: none, <reason>` on a line of its own (with `Refs #<number>` beside it if you like). A change with no issue also says `Task: none, <reason>`. -->

## Acceptance evidence

<!-- One row per acceptance criterion of every issue this change closes, copied exactly from the issue's "- [ ] ACn" list. Evidence class is code, live or browser. In Proof, put repository paths in backticks, and write anything else (a command, a read-back, a run) without them. Delete the example row. Leave the table out only when the change closes no issue. -->

| Issue | AC | AC text | Evidence class | Proof |
| --- | --- | --- | --- | --- |
| #N | AC1 | The acceptance criterion, word for word | code | `path/to/the_test.go` TestWhatItProves |
