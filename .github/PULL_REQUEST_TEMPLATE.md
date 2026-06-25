<!--
Thanks for contributing! Please keep PRs focused. See CONTRIBUTING.md.
Do not include secrets, tokens, or any prompt/response content.
-->

## What & why

<!-- What does this change do, and what problem does it solve? -->

## Checklist

- [ ] `make gate` is green (vet + test + lint + spdx-check + build)
- [ ] Tests added/updated (TDD: failing test first), no live network in tests
- [ ] New `.go` files carry the `SPDX-License-Identifier: AGPL-3.0-only` header
- [ ] Conventional Commit title (`feat:` / `fix:` / `docs:` / … ; `!` for breaking)
- [ ] No customer/vendor/domain specifics added to core code or defaults (kept configurable)
- [ ] Content-free invariant preserved (no prompt/response bodies; field allow/deny-list intact)
- [ ] `ARCHITECTURE.md` updated if a FROZEN type/interface changed

## Notes for reviewers

<!-- Anything reviewers should focus on, risks, follow-ups. -->
