#!/usr/bin/env bash
set -euo pipefail

version=$(awk '$1 == "go" { print $2; exit }' go.mod)
if [[ -z $version ]]; then
    printf 'Missing Go version in go.mod\n' >&2
    exit 1
fi
export GOTOOLCHAIN="go$version"
goroot=$(go env GOROOT)
if [[ $(GOTOOLCHAIN=local "$goroot/bin/go" env GOVERSION) != "$GOTOOLCHAIN" || ! -x $goroot/bin/gofmt ]]; then
    printf 'Expected Go %s and gofmt in %s\n' "$version" "$goroot/bin" >&2
    exit 1
fi
export PATH="$goroot/bin:$PATH"
exec just "$@"
