#!/usr/bin/env bash
# Maintainer-side fork-PR backstop for the forbidden-words hygiene gate (see forbidden-words.sh).
#
# Why this exists: FORBIDDEN_WORDS_PATTERN is a repo secret, and GitHub never exposes secrets to a
# `pull_request` run triggered from a fork — so on a fork PR forbidden-words.sh silently falls back to
# scanning only the generic credential-shape base pattern, and a deployment-specific identifier (customer
# name, internal hostname) can land on `ci-success`-green and get merged before the push-to-main run (which
# DOES have the secret) catches it post-merge — too late, history is append-only.
#
# This script closes that gap WITHOUT ever checking out or executing fork-controlled code: the caller
# (.github/workflows/hygiene-fork-backstop.yml, a `pull_request_target` job — has secret access even for
# fork PRs) fetches the PR's changed files via the read-only GitHub REST API (`GET
# .../pulls/{n}/files`, i.e. `gh api ... --jq`) and writes each file's unified-diff `patch` text, prefixed
# with a "--- <filename>" delimiter, to a plain data file. That file is pure text — never sourced,
# `eval`'d, or executed — so a malicious PR cannot use it to run code with this job's secret access. This
# script then greps ONLY the diff's added ('+') lines against the shared forbidden-words pattern.
#
# Usage: scripts/forbidden-words-diff.sh <diff-file>
#   <diff-file> format: repeated blocks of
#     --- <filename>
#     <unified diff patch text for that file, as returned by the GitHub pulls-files API>
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/forbidden-words-pattern.sh
. scripts/lib/forbidden-words-pattern.sh   # sets $PATTERN (shared with forbidden-words.sh)

diff_file="${1:?usage: forbidden-words-diff.sh <diff-file>}"
[ -f "$diff_file" ] || { echo "forbidden-words-diff: no such file: $diff_file" >&2; exit 2; }

hits=""
current="(unknown file)"
while IFS= read -r line; do
  case "$line" in
    "--- "*)
      current="${line#--- }"
      ;;
    "+++"*)
      : # unified-diff file-header noise, never emitted by our own '--- ' delimiter — ignore defensively
      ;;
    "+"*)
      content="${line#+}"
      if printf '%s\n' "$content" | grep -qiE -e "$PATTERN"; then
        hits="${hits}${current}: ${content}
"
      fi
      ;;
    *)
      : # hunk headers ('@@ ... @@') and context/removed lines — not new content, skip
      ;;
  esac
done < "$diff_file"

if [ -n "$hits" ]; then
  {
    echo "FAIL: forbidden term(s) found in this PR's added lines:"
    printf '%s' "$hits"
    echo "This PR adds a term from the deployment-specific denylist (or a credential shape). Remove it"
    echo "before merging — this repo's history is append-only, so a merged hit cannot be scrubbed later."
  } >&2
  exit 1
fi
echo "forbidden-words-diff: clean"
