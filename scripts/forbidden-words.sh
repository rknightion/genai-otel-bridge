#!/usr/bin/env bash
# Forbidden-words hygiene guard — scans the tree for deployment-specific identifiers (customer names,
# internal hostnames, account ids, sibling repos, …) that must NEVER be committed.
#
# The term list is environment-specific and is NOT stored in this repo. It is loaded from, in order:
#   1. $FORBIDDEN_WORDS_PATTERN      — a ready alternation regex (use this in CI, injected from a secret)
#   2. scripts/forbidden-words.local — gitignored; one regex fragment per line (#-comments + blanks ok)
# If neither is present the guard self-skips (e.g. a fresh clone without the local list).
# See scripts/forbidden-words.local.example for the format.
#
# Runs at two points: the pre-commit hook (staged files) and CI (`make forbidden-words`, via `make ci`).
# Portable (bash 3.2 / macOS): no mapfile / associative arrays.
#
# Usage:
#   scripts/forbidden-words.sh            # scan the whole tree
#   scripts/forbidden-words.sh <files...> # scan only these (pre-commit passes staged files)
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/private-paths.sh
. scripts/lib/private-paths.sh

# Load the term list (see header): env var wins; else build one alternation regex from the local file.
PATTERN="${FORBIDDEN_WORDS_PATTERN:-}"
if [ -z "$PATTERN" ] && [ -f scripts/forbidden-words.local ]; then
  PATTERN="$(grep -vE '^[[:space:]]*(#|$)' scripts/forbidden-words.local | paste -sd'|' -)"
fi
if [ -z "$PATTERN" ]; then
  echo "forbidden-words: no term list (set FORBIDDEN_WORDS_PATTERN or create scripts/forbidden-words.local) — skipped"
  exit 0
fi

list_candidates() {
  if [ "$#" -gt 0 ]; then printf '%s\n' "$@"; else git ls-files; fi
}

hits=""
scanned=0
while IFS= read -r f; do
  is_private "$f" && continue          # never scan private-only paths (they legitimately hold infra)
  [ -f "$f" ] || continue              # skip deletions / non-files
  scanned=$((scanned + 1))
  if m=$(grep -nIiE "$PATTERN" "$f" 2>/dev/null); then
    hits="${hits}--- ${f}
${m}
"
  fi
done < <(list_candidates "$@")

if [ -n "$hits" ]; then
  {
    echo "FAIL: forbidden term(s) found (these must not be committed to this repo):"
    printf '%s' "$hits"
    echo "Fix the term, or — if the file is genuinely dev-only — add its path to PRIVATE_PATHS in"
    echo "scripts/lib/private-paths.sh so it is excluded from the scan."
  } >&2
  exit 1
fi
echo "forbidden-words: clean (${scanned} public-surface files scanned)"
