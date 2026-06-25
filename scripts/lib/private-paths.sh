#!/usr/bin/env bash
# Paths excluded from the forbidden-words hygiene scan: dev-only tooling that legitimately holds the
# banned-term list (scanning it would self-trip) plus gitignored scratch. Sourced by forbidden-words.sh.
# Keep portable (bash 3.2 / macOS): indexed arrays + case only, no mapfile / no associative arrays.

# shellcheck disable=SC2034
PRIVATE_PATHS=(
  "docs/superpowers"            # gitignored scratch (defensive)
  "scripts/forbidden-words.sh"  # the gate enumerates the banned terms — excluded so it doesn't self-trip
  "scripts/lib"                 # this file (sourced by the gate)
  "scripts/git-hooks"           # local dev hooks
)

# Basenames that are private wherever they appear. Empty: CLAUDE.md ships as contributor docs and the
# tree is otherwise the public surface.
# shellcheck disable=SC2034
PRIVATE_NAMES=()

# is_private <path> — return 0 if the path is (under) a private-only path OR has a private basename.
is_private() {
  local f="$1" p n
  for p in "${PRIVATE_PATHS[@]}"; do
    case "$f" in "$p"|"$p"/*) return 0 ;; esac
  done
  for n in ${PRIVATE_NAMES[@]+"${PRIVATE_NAMES[@]}"}; do  # safe empty-array expansion (bash 3.2 + set -u)
    case "$f" in "$n"|*/"$n") return 0 ;; esac
  done
  return 1
}

# public_surface — print tracked files that ARE scanned (tracked minus PRIVATE_PATHS).
public_surface() {
  local f
  git ls-files | while IFS= read -r f; do
    is_private "$f" || printf '%s\n' "$f"
  done
}
