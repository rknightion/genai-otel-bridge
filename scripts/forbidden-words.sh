#!/usr/bin/env bash
# Forbidden-words hygiene guard — scans the tree for two classes of content that must NEVER be committed:
#   1. generic CREDENTIAL SHAPES (private keys, PAT/token prefixes, AWS key ids) — the built-in base set
#      below; public patterns, always scanned even without the deployment list (catches pasted secrets).
#   2. deployment-specific IDENTIFIERS (customer names, internal hostnames, account ids, sibling repos)
#      — sensitive, so kept OUT of this repo. Loaded from, in order:
#        a. $FORBIDDEN_WORDS_PATTERN      — a ready alternation regex (CI, injected from a secret)
#        b. scripts/forbidden-words.local — gitignored; one regex fragment per line (#-comments + blanks ok)
#      Absent (e.g. a fork PR / fresh clone) → only the credential shapes are scanned.
# This is a lightweight guard, NOT a full secret scanner — gitleaks (CI) + GitHub secret scanning (once
# public) are the comprehensive layer. See scripts/forbidden-words.local.example for the list format.
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
# shellcheck source=scripts/lib/forbidden-words-pattern.sh
. scripts/lib/forbidden-words-pattern.sh   # sets $PATTERN

list_candidates() {
  if [ "$#" -gt 0 ]; then printf '%s\n' "$@"; else git ls-files; fi
}

hits=""
scanned=0
while IFS= read -r f; do
  is_private "$f" && continue          # never scan private-only paths (they legitimately hold infra)
  [ -f "$f" ] || continue              # skip deletions / non-files
  scanned=$((scanned + 1))
  if m=$(grep -nIiE -e "$PATTERN" "$f" 2>/dev/null); then  # -e: pattern may start with '-' (private-key header)
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
