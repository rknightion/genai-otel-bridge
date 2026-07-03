#!/usr/bin/env bash
# Shared forbidden-words pattern builder — sourced by both scripts/forbidden-words.sh (full-tree scan,
# pre-commit + CI on the repo's own trusted checkout) and scripts/forbidden-words-diff.sh (maintainer-side
# fork-PR backstop, scans a fetched PR diff instead of a checkout). Keeping the pattern in one place means
# the two scanners can never silently drift apart.
#
# Sets $PATTERN from:
#   1. generic CREDENTIAL SHAPES (private keys, PAT/token prefixes, AWS key ids) — always scanned.
#   2. deployment-specific IDENTIFIERS — from $FORBIDDEN_WORDS_PATTERN (CI secret) if set, else the
#      gitignored scripts/forbidden-words.local. Absent (fork PR without the secret, fresh clone) →
#      credential shapes only.
BASE_PATTERN='-----BEGIN [A-Z ]*PRIVATE KEY-----|AKIA[0-9A-Z]{16}|ghp_[0-9A-Za-z]{36}|github_pat_[0-9A-Za-z_]{40,}|glpat-[0-9A-Za-z_-]{20}|xox[baprs]-[0-9A-Za-z-]{10,}'

TERMS="${FORBIDDEN_WORDS_PATTERN:-}"
if [ -z "$TERMS" ] && [ -f scripts/forbidden-words.local ]; then
  TERMS="$(grep -vE '^[[:space:]]*(#|$)' scripts/forbidden-words.local | paste -sd'|' -)"
fi

# shellcheck disable=SC2034  # consumed by the sourcing script (forbidden-words.sh / forbidden-words-diff.sh)
if [ -n "$TERMS" ]; then PATTERN="${BASE_PATTERN}|${TERMS}"; else PATTERN="$BASE_PATTERN"; fi
