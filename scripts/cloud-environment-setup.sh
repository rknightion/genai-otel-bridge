#!/usr/bin/env bash
# LOCAL AGENTS: Do not execute this script unless you are running in a cloud agent environment.
# Prepare a Codex Cloud or Claude Code cloud container for repository development.
# Configure the cloud environment setup command as:
#   bash scripts/cloud-environment-setup.sh
set -euo pipefail

BACKLOG_VERSION="1.50.1"
TOFU_VERSION="1.10.6"
TFLINT_VERSION="0.59.1"
CHECKOV_VERSION="3.2.495"

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

# Setup and agent phases use separate shells. Persist user-installed binaries rather
# than relying on exports made by this script (see the Codex Cloud setup docs).
user_bin="$HOME/.local/bin"
mkdir -p "$user_bin"
path_line='export PATH="$HOME/.local/bin:$PATH"'
touch "$HOME/.bashrc"
grep -Fqx "$path_line" "$HOME/.bashrc" || printf '\n%s\n' "$path_line" >> "$HOME/.bashrc"
export PATH="$user_bin:$PATH"

need_packages=()
for command in curl git make gcc npm python3 pipx unzip; do
  command -v "$command" >/dev/null 2>&1 || need_packages+=("$command")
done
if ((${#need_packages[@]})); then
  apt_prefix=()
  if ((EUID != 0)); then
    apt_prefix=(sudo)
  fi
  "${apt_prefix[@]}" apt-get update
  "${apt_prefix[@]}" env DEBIAN_FRONTEND=noninteractive apt-get install -y \
    ca-certificates curl git make gcc npm python3 pipx unzip
fi

required_go="$(sed -nE 's/^go ([0-9]+\.[0-9]+).*/\1/p' go.mod)"
installed_go="$(go env GOVERSION 2>/dev/null || true)"
if [[ "$installed_go" != go"$required_go".* ]]; then
  printf 'error: go.mod requires Go %s.x, but this environment has %s\n' \
    "$required_go" "${installed_go:-no Go installation}" >&2
  printf 'Set the Go version to %s in the Codex environment package settings.\n' \
    "$required_go" >&2
  exit 1
fi

install_github_zip() {
  local name="$1" version="$2" base="$3" archive="$4" checksum_asset="$5" binary="$6"
  if command -v "$name" >/dev/null 2>&1; then
    local installed_version
    installed_version="$("$name" --version 2>&1 || true)"
    [[ "$installed_version" == *"$version"* ]] && return
  fi

  local work
  work="$(mktemp -d)"
  trap 'rm -rf "$work"' RETURN
  curl -sSfLo "$work/$archive" "$base/v$version/$archive"
  curl -sSfLo "$work/checksums.txt" "$base/v$version/$checksum_asset"
  (cd "$work" && grep "  $archive\$" checksums.txt | sha256sum -c -)
  unzip -q "$work/$archive" -d "$work/unpacked"
  install -m 0755 "$work/unpacked/$binary" "$user_bin/$name"
  rm -rf "$work"
  trap - RETURN
}

os="$(go env GOOS)"
arch="$(go env GOARCH)"
install_github_zip tofu "$TOFU_VERSION" \
  "https://github.com/opentofu/opentofu/releases/download" \
  "tofu_${TOFU_VERSION}_${os}_${arch}.zip" "tofu_${TOFU_VERSION}_SHA256SUMS" tofu
install_github_zip tflint "$TFLINT_VERSION" \
  "https://github.com/terraform-linters/tflint/releases/download" \
  "tflint_${os}_${arch}.zip" checksums.txt tflint

if ! command -v checkov >/dev/null 2>&1 || ! checkov --version | grep -Fqx "$CHECKOV_VERSION"; then
  pipx install --force "checkov==$CHECKOV_VERSION"
fi

# Backlog.md is part of the development workflow, not an application dependency.
# Install it globally for the selected Node runtime and persist npm's bin directory.
npm install --global "backlog.md@$BACKLOG_VERSION"
npm_bin="$(dirname "$(command -v backlog)")"
npm_path_line="export PATH=\"$npm_bin:\$PATH\""
grep -Fqx "$npm_path_line" "$HOME/.bashrc" || printf '%s\n' "$npm_path_line" >> "$HOME/.bashrc"

# Fetch everything needed by offline-by-default agent phases. The Makefile owns the
# versions and checksum verification for golangci-lint, envtest, Helm, k3d, and kubectl.
go mod download
make tools tools-e2e
"$repo_root/.tools/setup-envtest" use "${ENVTEST_K8S_VERSION:-1.35.0}" \
  --bin-dir "$repo_root/.tools/envtest" >/dev/null

tofu -chdir=deploy/ecs/terraform init -backend=false -input=false >/dev/null

backlog --version
tofu version | sed -n '1p'
tflint --version | sed -n '1p'
checkov --version
printf 'Cloud agent environment setup complete. Run make gate to validate changes.\n'
