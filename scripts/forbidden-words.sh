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
# Runs at two points: the pre-commit hook (staged files) and CI (`just forbidden-words`, in the hygiene leg and `just check`).
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

# Validate the pattern once. grep exits 2 on a malformed regex; without this check that status would be
# read as "no hit" and the whole sweep (credential shapes included) would pass clean. Never print the
# pattern: it is the private term list.
pre=0
printf '' | grep -E -e "$PATTERN" >/dev/null 2>&1 || pre=$?
if [ "$pre" -gt 1 ]; then
  echo "forbidden-words: invalid pattern (grep exit $pre)" >&2
  exit 2
fi

hits=""
scanned=0
while IFS= read -r f; do
  is_private "$f" && continue          # never scan private-only paths (they legitimately hold infra)
  [ -f "$f" ] || continue              # skip deletions / non-files
  scanned=$((scanned + 1))
  rc=0
  m=$(grep -nIiE -e "$PATTERN" "$f" 2>/dev/null) || rc=$?  # -e: pattern may start with '-' (private-key header)
  if [ "$rc" -eq 0 ]; then
    hits="${hits}--- ${f}
${m}
"
  elif [ "$rc" -ne 1 ]; then  # 1 = no match; anything else is a grep failure, never a pass
    echo "forbidden-words: grep failed (exit $rc) on $f" >&2
    exit 2
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
