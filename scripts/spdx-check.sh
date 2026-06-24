#!/usr/bin/env bash
# spdx-check.sh — fail if any tracked .go file is missing the AGPL-3.0-only SPDX header on line 1.
# Grafana convention: every source file carries `// SPDX-License-Identifier: AGPL-3.0-only` as its
# first line (no copyright line; authorship is the LICENSE file + git history). See LICENSING.md.
set -euo pipefail
header='SPDX-License-Identifier: AGPL-3.0-only'
missing=()
while IFS= read -r f; do
  # SPDX is always line 1 — including files that also carry a //go:build constraint (line 3).
  head -1 "$f" | grep -q "$header" || missing+=("$f")
done < <(git ls-files '*.go')
if [ "${#missing[@]}" -gt 0 ]; then
  echo "FAIL: .go files missing '$header' on line 1:"
  printf '  %s\n' "${missing[@]}"
  echo "Add the header (see LICENSING.md) — e.g. via: scripts/spdx-check.sh"
  exit 1
fi
echo "spdx-check: all $(git ls-files '*.go' | wc -l | tr -d ' ') .go files carry the AGPL-3.0-only header."
