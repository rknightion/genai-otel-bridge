---
id: GOB-0016
title: Migrate the repo task surface to just and retire Makefiles and ad-hoc scripts
status: Done
assignee: []
created_date: '2026-08-28 19:15'
updated_date: '2026-08-29 14:14'
labels:
  - 'wave:2-fleet'
dependencies: []
priority: medium
type: chore
ordinal: 16000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Fleet-wide migration of the repo task surface from `make` to the `just` command runner. This repo is
Go 1.27 + a Helm-deployed k8s workload + an ECS/Terraform module + a k3d e2e harness + SBOM/licence
release tooling. It has one 13.6 KB `Makefile` (38 targets), 11 tracked shell scripts, 1 tracked Python
generator, 14 workflow files, and no `justfile` today.

Read the frozen fleet standard first if it is available in the session; this task is written to be
self-contained and consistent with it. Every recipe body below uses this repo's real commands, taken
from the current `Makefile`, `.golangci.yml`, `go.mod` and `.github/workflows/ci.yml`. Do not
re-litigate the recipe vocabulary or the group taxonomy.

## 1. Outcome

A single top-level `justfile` is the whole developer and CI task surface. `just check` is THE gate and
is exactly the union of what `ci.yml`'s `gate` matrix enforces (build, vet, both lint legs, test, race,
acceptance, envtest, forbidden-words, spdx-check, helm-lint, tf-validate) plus the generated-artifact
drift gate and formatting. `just ci` is the honest superset that additionally covers the CI legs which
need Docker or a service container (`e2e`, `test-dynamodb`, `secret-scan`, `coverage`). `Makefile` is
deleted. `scripts/envtest.sh` and `scripts/sbom.sh` are absorbed into recipes and deleted; the other
nine shell scripts and the Python dashboard generator survive as files, each reachable through a
recipe. Every `run:` block in `ci.yml` that carried build/test/lint logic collapses to a one-line
`run: just <recipe>`; `ci-success` and every reusable `uses:` call are untouched. `AGENTS.md`,
`CONTRIBUTING.md`, `README.md`, the nested `CLAUDE.md` files, the Go generator marker strings and
`backlog/config.yml`'s `definition_of_done` all say `just`, not `make`.

## 2. The complete justfile

Drop this in at the repo root as `justfile` (lowercase, no extension). Verified against `just 1.58.0`.

```just
set shell := ["bash", "-euo", "pipefail", "-c"]

# ── pinned tool versions (override via env; majors are load-bearing) ──────────
golangci_lint_version := env('GOLANGCI_LINT_VERSION', 'v2.13.2')
setup_envtest_version := env('SETUP_ENVTEST_VERSION', 'release-0.23')
envtest_k8s_version := env('ENVTEST_K8S_VERSION', '1.35.0')
helm_version := env('HELM_VERSION', 'v4.2.4')
k3d_version := env('K3D_VERSION', 'v5.9.0')
k3s_image := env('K3S_IMAGE', 'rancher/k3s:v1.36.4-k3s1')
go_licenses_version := env('GO_LICENSES_VERSION', 'v2.0.1')
syft_version := env('SYFT_VERSION', 'v1.51.1')
gitleaks_version := env('GITLEAKS_VERSION', '8.30.1')

image := env('IMAGE', 'genai-otel-bridge:dev')
e2e_helper_image := env('E2E_HELPER_IMAGE', 'genai-otel-bridge-e2e-helper:dev')

go := env('GO', 'go')
tools_dir := justfile_directory() / ".tools"
git_version := `git describe --tags --always --dirty 2>/dev/null || echo dev`
ldflags := "-X github.com/rknightion/genai-otel-bridge/internal/version.Version=" + git_version

# Force module mode for ALL go tooling (build/test/vet/lint/generate). A local, gitignored `vendor/`
# dir would otherwise flip Go into -mod=vendor, which goes stale on every dependency bump (the
# "inconsistent vendoring" error) and diverges from CI — CI has no vendor/ tree. Pinning
# -mod=readonly makes `just check` behave EXACTLY like CI and ignore any stale local vendor/.
export GOFLAGS := env('GOFLAGS', '-mod=readonly')

# show the task surface
default:
    @just --list

# install the pinned dev + e2e toolchain into .tools/ and prefetch Go modules and envtest assets
setup: _tools _tools-e2e
    {{ go }} mod download
    {{ tools_dir }}/setup-envtest use {{ envtest_k8s_version }} --bin-dir {{ tools_dir }}/envtest >/dev/null

# ── the gate ─────────────────────────────────────────────────────────────────

# THE GATE — everything a commit must pass; the union of ci.yml's `gate` matrix legs
[group('check')]
check: fmt-check build typecheck lint gen-check test test-race test-acceptance test-envtest forbidden-words spdx-check helm-lint tf-validate

# the gate plus the ci.yml legs that need Docker or a service container
[group('check')]
ci: check coverage secret-scan test-dynamodb e2e

# format Go sources and this justfile in place
[group('check')]
fmt:
    gofmt -s -w .
    just --fmt

# verify Go + justfile formatting; non-zero when reformatting is needed
[group('check')]
[no-exit-message]
fmt-check:
    just --fmt --check
    @out="$(gofmt -s -l .)"; if [ -n "$out" ]; then echo "gofmt: needs formatting (run 'just fmt'):"; echo "$out"; exit 1; fi

# go vet the whole module
[group('check')]
[no-exit-message]
typecheck:
    {{ go }} vet ./...

# golangci-lint over the plain AND acceptance-tagged builds (both of ci.yml's lint legs)
[group('check')]
[no-exit-message]
lint: _tools
    {{ tools_dir }}/golangci-lint run
    {{ tools_dir }}/golangci-lint run --build-tags acceptance

# run the default Go test suite; `just test <regex>` filters with -run
[group('check')]
[no-exit-message]
test filter="":
    {{ go }} test {{ if filter == "" { "" } else { "-run " + filter } }} ./...

# run the default test suite under the race detector
[group('check')]
[no-exit-message]
test-race:
    {{ go }} test -race ./...

# run the acceptance gates (failover / outage / soak) in internal/app
[group('check')]
[no-exit-message]
test-acceptance:
    {{ go }} test -tags acceptance ./internal/app/

# run the integration suite against a real kube-apiserver+etcd (envtest; no kubelet, no cluster)
[group('check')]
[no-exit-message]
[script('bash')]
test-envtest: _tools
    set -euo pipefail
    assets="$({{ tools_dir }}/setup-envtest use {{ envtest_k8s_version }} --bin-dir {{ tools_dir }}/envtest -p path)"
    export KUBEBUILDER_ASSETS="$assets"
    echo "envtest assets: $KUBEBUILDER_ASSETS"
    {{ go }} test -tags envtest -count=1 ./test/integration/...

# run the DynamoDB backend + acceptance tests against dynamodb-local (needs $DYNAMODB_ENDPOINT)
[group('check')]
[no-exit-message]
test-dynamodb:
    {{ go }} test ./internal/coordinate/dynamodb/... ./internal/checkpoint/dynamodb/...
    {{ go }} test -tags acceptance ./internal/app/ -run TestDynamoDB

# write coverage.out over the unit-test scope (ci.yml uploads it to Codacy)
[group('check')]
[no-exit-message]
coverage:
    {{ go }} test -covermode=atomic -coverprofile=coverage.out ./...

# scan the tracked tree for credential shapes and deployment-specific identifiers
[group('check')]
[no-exit-message]
forbidden-words:
    @if [ -f scripts/forbidden-words.sh ]; then bash scripts/forbidden-words.sh; else echo "forbidden-words: skipped (guard not present in this repo)"; fi

# fail if any tracked .go file is missing the AGPL-3.0-only SPDX header on line 1
[group('check')]
[no-exit-message]
spdx-check:
    bash scripts/spdx-check.sh

# helm lint the bundled chart
[group('check')]
[no-exit-message]
helm-lint: _tools-e2e
    {{ tools_dir }}/helm lint deploy/helm

# fmt/validate/tflint/checkov the ECS Terraform module (tofu-first; each tool self-skips when absent)
[group('check')]
[no-exit-message]
[script('bash')]
tf-validate:
    set -euo pipefail
    dir=deploy/ecs/terraform
    if command -v tofu >/dev/null 2>&1; then TF=tofu
    elif command -v terraform >/dev/null 2>&1; then TF=terraform
    else TF=""; echo "tf-validate: no tofu/terraform found, skipping"
    fi
    if [ -n "$TF" ]; then
      echo "tf-validate: $TF fmt + validate"
      "$TF" -chdir="$dir" fmt -check -recursive
      "$TF" -chdir="$dir" init -backend=false -input=false >/dev/null
      "$TF" -chdir="$dir" validate
    fi
    if command -v tflint >/dev/null 2>&1; then
      echo "tf-validate: tflint"; tflint --chdir="$dir"
    else echo "tf-validate: tflint not found, skipping"; fi
    if command -v checkov >/dev/null 2>&1; then
      echo "tf-validate: checkov"; checkov -d "$dir" --framework terraform --quiet --compact
    else echo "tf-validate: checkov not found, skipping"; fi

# gitleaks scan of the whole history (fetches a pinned, checksum-verified gitleaks into .tools/)
[group('check')]
[no-exit-message]
[script('bash')]
secret-scan:
    set -euo pipefail
    mkdir -p {{ tools_dir }}
    if ! { test -x {{ tools_dir }}/gitleaks && {{ tools_dir }}/gitleaks version >/dev/null 2>&1; }; then
      os="$({{ go }} env GOOS)"; arch="$({{ go }} env GOARCH)"
      case "$arch" in amd64) arch=x64 ;; esac
      asset="gitleaks_{{ gitleaks_version }}_${os}_${arch}.tar.gz"
      base="https://github.com/gitleaks/gitleaks/releases/download/v{{ gitleaks_version }}"
      tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
      curl -sSfLo "$tmp/$asset" "$base/$asset"
      curl -sSfLo "$tmp/checksums.txt" "$base/gitleaks_{{ gitleaks_version }}_checksums.txt"
      (cd "$tmp" && grep " ${asset}\$" checksums.txt | sha256sum -c -)
      tar -xzf "$tmp/$asset" -C "$tmp" gitleaks
      install -m 0755 "$tmp/gitleaks" {{ tools_dir }}/gitleaks
    fi
    {{ tools_dir }}/gitleaks detect --source . --redact --no-banner

# full k3d 3-node failover e2e: package chart, build local images, create cluster, test, tear down
[group('check')]
[no-exit-message]
e2e: helm-package image-local _tools-e2e
    IMAGE={{ image }} E2E_HELPER_IMAGE={{ e2e_helper_image }} K3S_IMAGE={{ k3s_image }} bash scripts/k3d-e2e.sh all

# ── generated artifacts ──────────────────────────────────────────────────────

# regenerate deploy/helm/values.yaml, deploy/ecs/terraform/config.example.yaml and docs/telemetry.md
[group('gen')]
gen:
    {{ go }} run ./internal/config/gen
    {{ go }} run ./internal/docs/gen

# drift gate: regenerate, then fail if any generated artifact is not committed
[group('gen')]
[no-exit-message]
gen-check: gen
    @git diff --exit-code -- deploy/helm/values.yaml deploy/ecs/terraform/config.example.yaml docs/telemetry.md || { echo "generated files are stale — run 'just gen' and commit"; exit 1; }

# regenerate the self-observability Grafana dashboard manifest (needs python3 + PyYAML)
[group('gen')]
gen-dashboard:
    python3 deploy/grafana/self-obs/gen_dashboard.py

# ── build ────────────────────────────────────────────────────────────────────

# compile every package and write bin/genai-otel-bridge with the version stamped from git describe
[group('build')]
build:
    {{ go }} build ./...
    {{ go }} build -ldflags '{{ ldflags }}' -o bin/genai-otel-bridge ./cmd/genai-otel-bridge

# build the multi-arch container image with buildx (no push)
[group('build')]
image tag=image:
    docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION={{ git_version }} -t '{{ tag }}' -f Dockerfile .

# build the single-arch app + e2e-helper images into the local docker daemon
[group('build')]
image-local:
    docker build --build-arg VERSION={{ git_version }} -t {{ image }} -f Dockerfile .
    docker build -t {{ e2e_helper_image }} -f test/e2e/harness/Dockerfile .

# package the Helm chart into dist/
[group('build')]
helm-package: _tools-e2e
    {{ tools_dir }}/helm package deploy/helm -d dist/

# ── dev ──────────────────────────────────────────────────────────────────────

# run the bridge locally against a config file (long-running; ^C to stop)
[group('dev')]
run config="config.yaml":
    {{ go }} run ./cmd/genai-otel-bridge --config '{{ config }}'

# create the local k3d e2e cluster and helm-install the chart into it
[group('dev')]
k3d-up: _tools-e2e
    IMAGE={{ image }} E2E_HELPER_IMAGE={{ e2e_helper_image }} K3S_IMAGE={{ k3s_image }} bash scripts/k3d-e2e.sh up

# delete the local k3d e2e cluster
[group('dev')]
k3d-down: _tools-e2e
    bash scripts/k3d-e2e.sh down

# symlink the repo git hooks into .git/hooks (pre-commit runs the forbidden-words gate on staged files)
[group('dev')]
[script('bash')]
install-hooks:
    set -euo pipefail
    mkdir -p .git/hooks
    for h in scripts/git-hooks/*; do
      ln -sf "../../$h" ".git/hooks/$(basename "$h")"
      echo "installed $(basename "$h")"
    done

# remove build output and the pinned toolchain (`just setup` + `just build` reproduce all of it)
[group('dev')]
clean:
    rm -rf bin dist coverage.out {{ tools_dir }}

# ── release artifacts ────────────────────────────────────────────────────────

# regenerate THIRD_PARTY_NOTICES.md from the shipped binary's import graph (release artifact, uncommitted)
[group('release')]
notices: _tools-licensing
    GO_LICENSES={{ tools_dir }}/go-licenses bash scripts/notices.sh

# write SPDX 2.3 + CycloneDX 1.6 SBOMs into dist/sbom/ (release artifacts, uncommitted)
[group('release')]
[script('bash')]
sbom target="bin/genai-otel-bridge" out_dir="dist/sbom" name="genai-otel-bridge": _tools-sbom build
    set -euo pipefail
    mkdir -p '{{ out_dir }}'
    echo "sbom: scanning {{ target }}"
    {{ tools_dir }}/syft '{{ target }}' -q \
      -o "spdx-json={{ out_dir }}/{{ name }}.spdx.json" \
      -o "cyclonedx-json={{ out_dir }}/{{ name }}.cdx.json"
    echo "sbom: wrote {{ out_dir }}/{{ name }}.spdx.json + {{ out_dir }}/{{ name }}.cdx.json"

# LOCAL/MANUAL fallback publish of image + chart to an OCI registry — NOT the CI release path
[group('release')]
[confirm('publish pushes the image and chart to a real registry — continue?')]
publish: _tools-e2e
    HELM={{ tools_dir }}/helm bash scripts/publish.sh

# ── private tool installers (idempotent; install into .tools/) ────────────────

[private]
[script('bash')]
_tools:
    set -euo pipefail
    mkdir -p {{ tools_dir }}
    # Probe that the cached binary actually EXECUTES on this arch, not just `test -x` — a CI cache
    # restored across architectures passes `test -x` but dies with "Exec format error".
    if ! { test -x {{ tools_dir }}/golangci-lint && {{ tools_dir }}/golangci-lint version >/dev/null 2>&1; }; then
      curl -sSfL "https://raw.githubusercontent.com/golangci/golangci-lint/{{ golangci_lint_version }}/install.sh" \
        | sh -s -- -b {{ tools_dir }} {{ golangci_lint_version }}
    fi
    if ! { test -x {{ tools_dir }}/setup-envtest && {{ tools_dir }}/setup-envtest --help >/dev/null 2>&1; }; then
      GOBIN={{ tools_dir }} {{ go }} install sigs.k8s.io/controller-runtime/tools/setup-envtest@{{ setup_envtest_version }}
    fi

[private]
[script('bash')]
_tools-e2e:
    set -euo pipefail
    mkdir -p {{ tools_dir }}
    # helm/k3d/kubectl are fetched as raw binaries/tarballs (never curl|bash of a script) from the SAME
    # pinned release as the version var, and sha256-verified against that release's own checksum file
    # before install — no mutable branch ref is ever executed. Each cached binary is probed for
    # executability on this arch, not just `test -x` (a cross-arch CI cache restore).
    os="$({{ go }} env GOOS)"; arch="$({{ go }} env GOARCH)"
    if ! { test -x {{ tools_dir }}/helm && {{ tools_dir }}/helm version --short >/dev/null 2>&1; }; then
      tarball="helm-{{ helm_version }}-$os-$arch.tar.gz"
      curl -sSfLo "/tmp/$tarball" "https://get.helm.sh/$tarball"
      curl -sSfLo "/tmp/$tarball.sha256sum" "https://get.helm.sh/$tarball.sha256sum"
      (cd /tmp && sha256sum -c "$tarball.sha256sum")
      tar -xzf "/tmp/$tarball" -C /tmp
      mv "/tmp/$os-$arch/helm" {{ tools_dir }}/helm
    fi
    if ! { test -x {{ tools_dir }}/k3d && {{ tools_dir }}/k3d version >/dev/null 2>&1; }; then
      curl -sSfLo /tmp/k3d-checksums.txt "https://github.com/k3d-io/k3d/releases/download/{{ k3d_version }}/checksums.txt"
      curl -sSfLo /tmp/k3d-bin "https://github.com/k3d-io/k3d/releases/download/{{ k3d_version }}/k3d-$os-$arch"
      want="$(grep "k3d-$os-$arch\$" /tmp/k3d-checksums.txt | awk '{print $1}')"
      test -n "$want" || { echo "k3d: no checksum entry for $os-$arch in checksums.txt"; exit 1; }
      echo "$want  /tmp/k3d-bin" | sha256sum -c -
      install -m 0755 /tmp/k3d-bin {{ tools_dir }}/k3d
    fi
    if ! { test -x {{ tools_dir }}/kubectl && {{ tools_dir }}/kubectl version --client >/dev/null 2>&1; }; then
      curl -sSfLo /tmp/kubectl-bin "https://dl.k8s.io/release/v{{ envtest_k8s_version }}/bin/$os/$arch/kubectl"
      curl -sSfLo /tmp/kubectl-bin.sha256 "https://dl.k8s.io/release/v{{ envtest_k8s_version }}/bin/$os/$arch/kubectl.sha256"
      echo "$(cat /tmp/kubectl-bin.sha256)  /tmp/kubectl-bin" | sha256sum -c -
      install -m 0755 /tmp/kubectl-bin {{ tools_dir }}/kubectl
    fi

[private]
[script('bash')]
_tools-licensing:
    set -euo pipefail
    mkdir -p {{ tools_dir }}
    { test -x {{ tools_dir }}/go-licenses && {{ tools_dir }}/go-licenses --help >/dev/null 2>&1; } \
      || GOBIN={{ tools_dir }} {{ go }} install github.com/google/go-licenses@{{ go_licenses_version }}

[private]
[script('bash')]
_tools-sbom:
    set -euo pipefail
    mkdir -p {{ tools_dir }}
    { test -x {{ tools_dir }}/syft && {{ tools_dir }}/syft version >/dev/null 2>&1; } \
      || GOBIN={{ tools_dir }} {{ go }} install github.com/anchore/syft/cmd/syft@{{ syft_version }}
```

After dropping it in, run `just --fmt` once and commit whatever it rewrites — the layout above is
hand-written and `--fmt` output is authoritative. `just --fmt --check` must then be clean.

## 3. Makefile disposition

Every target in `Makefile` (13,639 bytes, 38 `.PHONY` entries). Nothing survives.

| Make target | Replacement recipe | Notes |
|---|---|---|
| `build` | `just build` | `build` now also runs `go build ./...` first, folding in `ci-build`. |
| `test` | `just test` | Gains an optional `filter` param mapping to `-run`. |
| `coverage` | `just coverage` | Unchanged body; `ci.yml`'s `coverage` job calls it. |
| `vet` | `just typecheck` | Renamed to the frozen vocabulary name. `vet` is not a fleet recipe name. |
| `lint` | `just lint` | Both legs (plain + `--build-tags acceptance`) preserved. |
| `gate` | `just check` | **Semantics widen deliberately.** `make gate` omitted race/acceptance/envtest; `just check` includes them, so `check` is the full ci.yml `gate`-matrix union, plus `fmt-check` and `gen-check`. |
| `generate` | `just gen` | Same two `go run` generators. |
| `generate-check` | `just gen-check` | Same `git diff --exit-code` over the same three paths. Now inside `check`. |
| `gen-dashboard` | `just gen-dashboard` | Still `python3 deploy/grafana/self-obs/gen_dashboard.py`. Deliberately NOT in `gen`/`check` — it needs python3 + PyYAML and has no drift gate. |
| `tools` | `just _tools` (private) | Pulled in as a dependency of `lint`, `test-envtest`, `setup`. |
| `tools-e2e` | `just _tools-e2e` (private) | Dependency of `helm-lint`, `helm-package`, `k3d-*`, `e2e`, `publish`, `setup`. |
| `tools-licensing` | `just _tools-licensing` (private) | Dependency of `notices`. |
| `tools-sbom` | `just _tools-sbom` (private) | Dependency of `sbom`. |
| `ci-build` | folded into `just build` | `go build ./...` + a `-o /dev/null` cmd compile; the `-o bin/…` build supersedes the latter. |
| `ci-vet` | `just typecheck` | Identical body. |
| `ci-lint` | `just lint` (first line) | `lint` runs both legs; the matrix leg calls `just lint` once. |
| `ci-lint-acceptance` | `just lint` (second line) | Merged — ci.yml already ran `make ci-lint ci-lint-acceptance` in one leg. |
| `ci-test` | `just test` | Identical body. |
| `ci-race` | `just test-race` | Identical body. |
| `ci-acceptance` | `just test-acceptance` | Identical body. |
| `ci-envtest` | `just test-envtest` | Script absorbed — see §4. |
| `ci` | `just check` | ci.yml's matrix legs now call the individual recipes; `just ci` is the wider superset. |
| `ci-e2e` | `just e2e` | `helm-package` + `image-local` become recipe dependencies. |
| `forbidden-words` | `just forbidden-words` | Same self-skip guard, same script. |
| `spdx-check` | `just spdx-check` | Same script. |
| `tf-validate` | `just tf-validate` | Ported to `[script('bash')]`. **Behaviour fix required — see §9 trap 3.** |
| `helm-lint` | `just helm-lint` | Same body. |
| `image` | `just image` | Gains a `tag` param defaulting to `{{ image }}`. |
| `image-local` | `just image-local` | Same two docker builds. |
| `helm-package` | `just helm-package` | Same body. |
| `k3d-up` | `just k3d-up` | Same env passthrough into `scripts/k3d-e2e.sh up`. |
| `k3d-down` | `just k3d-down` | Same. |
| `k3d-e2e` | `just e2e` | Merged with `ci-e2e`; one recipe, both callers. |
| `notices` | `just notices` | Same `GO_LICENSES=` passthrough into the kept script. |
| `sbom` | `just sbom` | Script absorbed — see §4. |
| `publish` | `just publish` | Now `[confirm]`-gated; still drives the kept `scripts/publish.sh`. |
| `install-hooks` | `just install-hooks` | For-loop ported to `[script('bash')]`. |
| `.PHONY` block | delete | Meaningless in just. |
| `export PATH := $(TOOLS_DIR):$(PATH)` | **not ported** | Every tool is invoked by explicit `{{ tools_dir }}/…` path instead, exactly as the Makefile's own recipe bodies already did. `scripts/k3d-e2e.sh` and `scripts/envtest.sh`'s logic set their own `PATH`/`TOOLS_DIR`. Avoids the `export`-var/backtick interaction in §9 trap 8. |

**Then: `git rm Makefile`.** There is exactly one Makefile in this repo (`git ls-files | grep -i makefile` → `Makefile`). No `GNUmakefile`, no sub-directory makefiles, nothing under `vendor/`. Delete it in the final step of §8, not before.

## 4. Script disposition

`git ls-files | grep -E '\.(sh|bash|zsh|ps1)$'` → 11 files. Plus one tracked Python generator.

| Script | Verdict | Recipe | Detail |
|---|---|---|---|
| `scripts/envtest.sh` | **ABSORB** → `git rm` | `just test-envtest` | 10 lines: two env defaults, one `setup-envtest use … -p path` capture, an export, one `go test`. Pure "run a tool with some flags". Needs a persistent shell for the capture→export→test sequence, hence `[script('bash')]`. Exact recipe lines: `assets="$({{ tools_dir }}/setup-envtest use {{ envtest_k8s_version }} --bin-dir {{ tools_dir }}/envtest -p path)"` / `export KUBEBUILDER_ASSETS="$assets"` / `echo "envtest assets: $KUBEBUILDER_ASSETS"` / `{{ go }} test -tags envtest -count=1 ./test/integration/...`. Nothing else in the repo references it (`grep -rn 'envtest.sh'` → `Makefile:ci-envtest` only). |
| `scripts/sbom.sh` | **ABSORB** → `git rm` | `just sbom` | 20 effective lines: four env defaults, a `command -v` guard, `mkdir -p`, one `syft` invocation with two `-o` outputs. The env knobs become recipe params (`target`, `out_dir`, `name`); the `command -v` guard is replaced by the `_tools-sbom` dependency, which installs syft. Exact recipe lines are in §2's `sbom` recipe. Only caller was `Makefile:sbom`; **not** referenced by any workflow or Dockerfile (verified: `grep -rn 'sbom.sh'` → `Makefile` + `LICENSING.md` prose only). |
| `scripts/notices.sh` | **KEEP** | `just notices` | Executed inside the container build — `Dockerfile:23` and `Dockerfile.kaniko:30` both run `GO_LICENSES=go-licenses OUT=/build/THIRD_PARTY_NOTICES.md bash scripts/notices.sh`, where no `just` exists. It is also a real program (~90 lines: mktemp+trap, a TSV pipeline, a while-read loop emitting markdown, an Apache-2.0 §4(d) NOTICE sweep). |
| `scripts/publish.sh` | **KEEP** | `just publish` | A real program (~110 lines): RELEASE_TAG parsing, SemVer §11 chart-version construction, registry login, buildx setup, multi-tag build, chart package+push. Non-trivial control flow throughout. |
| `scripts/k3d-e2e.sh` | **KEEP** | `just e2e`, `just k3d-up`, `just k3d-down` | Functions, a `trap down EXIT`, a `case` dispatch on `$1` (`up`/`down`/`verify-cleanup`/`all`), DooD branching, bash-3.2 empty-array guards. Also carries a fourth entry point (`verify-cleanup`) that no make target ever exposed. |
| `scripts/forbidden-words.sh` | **KEEP** | `just forbidden-words` | Sources two libs, accepts an optional file list argument, runs a while-read scan loop, accumulates hits. Also invoked directly by `scripts/git-hooks/pre-commit` on staged files — a shipped hook, not a developer command. |
| `scripts/spdx-check.sh` | **KEEP** | `just spdx-check` | While-read loop over `git ls-files '*.go'`, a bash array, formatted failure report. Non-trivial control flow. |
| `scripts/lib/private-paths.sh` | **KEEP** | none (sourced) | A sourced bash library defining `PRIVATE_PATHS`, `is_private()`, `public_surface()`. Not a task. |
| `scripts/lib/forbidden-words-pattern.sh` | **KEEP** | none (sourced) | A sourced bash library that builds `$PATTERN`. Not a task. |
| `scripts/git-hooks/pre-commit` | **KEEP** | installed by `just install-hooks` | A shipped runtime artifact — git executes it directly from `.git/hooks/`; it must not depend on `just` being installed. |
| `scripts/cloud-environment-setup.sh` | **KEEP (edit required)** | none — it is the bootstrap that precedes `just` | Runs as the Codex Cloud / Claude Code cloud **setup command** on a bare container, before any repo tooling exists. ~110 lines with apt bootstrapping, a Go-version assertion, an `install_github_zip` helper with checksum verification, pipx and npm installs. **It must be edited** — see §6. |
| `test/eks/make-genai-otel-bridge-secret.sh` | **KEEP** | none | An operator script run by a human against a live EKS cluster with a namespace argument (`./test/eks/make-genai-otel-bridge-secret.sh default`). Not a dev or CI task; documented in `test/eks/README.md:20`. Do not give it a recipe and do not touch it. |
| `deploy/grafana/self-obs/gen_dashboard.py` | **KEEP** | `just gen-dashboard` | A real Python program that renders the Grafana dashboard manifest. |
| `scripts/notices.tsv.tmpl`, `scripts/forbidden-words.local.example` | **KEEP** | none | Data files consumed by kept scripts. |

Net script deletions: `scripts/envtest.sh`, `scripts/sbom.sh`. Nothing else.

## 5. CI changes

Fourteen workflow files. Only two are touched: `ci.yml` and `publish.yml`.

### The setup-just step (exact YAML)

Insert immediately **after** the `actions/setup-go` step in every job that runs a `just` recipe. SHA
resolved live against `extractions/setup-just` tag `v4`:

```yaml
      - uses: extractions/setup-just@53165ef7e734c5c07cb06b3c8e7b647c5aa16db3 # v4
        with:
          just-version: '1.58.0'
```

Pin `just-version` exactly — `just --fmt` output is explicitly outside any backwards-compatibility
guarantee, so an unpinned bump can turn `fmt-check` red with no repo change. Do not use `apt install
just`; it is unreliable on the hosted runners.

### `.github/workflows/ci.yml`

| Job / leg | Current `run:` | New `run:` |
|---|---|---|
| `gate` matrix `build-vet` | `make ci-build ci-vet` | `just build typecheck` |
| `gate` matrix `lint` | `make ci-lint ci-lint-acceptance` | `just lint` |
| `gate` matrix `test` | `make ci-test` | `just test` |
| `gate` matrix `race` | `make ci-race` | `just test-race` |
| `gate` matrix `acceptance` | `make ci-acceptance` | `just test-acceptance` |
| `gate` matrix `envtest` | `make ci-envtest` | `just test-envtest` |
| `gate` matrix `hygiene` | `make forbidden-words spdx-check helm-lint tf-validate` | `just forbidden-words spdx-check helm-lint tf-validate` |
| `e2e` | `make ci-e2e` | `just e2e` |
| `secret-scan` | the 8-line `run: \|` gitleaks download+verify+detect block | `just secret-scan` |
| `dynamodb-backends` | two steps: `go test ./internal/coordinate/dynamodb/... ./internal/checkpoint/dynamodb/...` and `go test -tags acceptance ./internal/app/ -run TestDynamoDB` | one step: `just test-dynamodb` |
| `coverage` | `make coverage` | `just coverage` |

Add the setup-just step to: `gate`, `e2e`, `secret-scan`, `dynamodb-backends`, `coverage`. The
`secret-scan` job has no `setup-go` step today, so put setup-just after its `actions/checkout` step —
but note `just secret-scan` calls `go env GOOS`, so that job also needs `actions/setup-go`
(`go-version-file: go.mod`, `cache: false`) added before setup-just, **or** replace the two `go env`
lines in the `secret-scan` recipe with `uname`-based detection. Prefer adding `setup-go` with
`cache: false` — it keeps the recipe uniform and the job already checks out the repo.

Optionally add a `gen` matrix leg (`run: just gen-check`). Not required: the drift is already gated by
`TestHelmGeneratedConfigUpToDate`, `TestECSConfigExampleUpToDate` and the telemetry generated test,
all of which run in the `test` leg.

**Must not change in `ci.yml`:**
- The `ci-success` job — its `name: ci-success`, `if: always()`, `needs: [gate, e2e, secret-scan, dynamodb-backends]` list, and both steps. The branch ruleset on `main` gates on that exact check name and Renovate automerge waits on it.
- `permissions: contents: read` at workflow level.
- The `gate` job's `strategy.fail-fast: false` and the matrix `include:` structure (only the `run:` values change).
- Every SHA-pinned `uses:` and its `# vN` comment: `actions/checkout@3d3c42e5…`, `actions/setup-go@b7ad1dad…`, `opentofu/setup-opentofu@a1320f89…`, `terraform-linters/setup-tflint@6e1e0642…`, `codacy/codacy-coverage-reporter-action@89d6c85c…`.
- `persist-credentials: false` on every checkout, and `fetch-depth: 0` on `secret-scan`'s.
- The `# zizmor: ignore[cache-poisoning]` comments above each `cache: true`.
- The `FORBIDDEN_WORDS_PATTERN: ${{ secrets.FORBIDDEN_WORDS_PATTERN }}` env on the gate step — `just` recipes inherit the step environment, so no `--set` plumbing.
- The `dynamodb-backends` `services:` block (SHA-pinned `amazon/dynamodb-local`) and its `DYNAMODB_ENDPOINT` env.
- The `coverage` job's `if: ${{ env.CODACY_PROJECT_TOKEN != '' }}` guard and its deliberate absence from `ci-success`'s `needs`.
- `if:` conditions on the `hygiene`-only tofu/tflint/checkov setup steps and the `pipx install checkov` step.

### `.github/workflows/publish.yml`

One change, in the `notices` job's final step:

```yaml
        run: |
          make notices
          gh release upload "${RELEASE_TAG}" THIRD_PARTY_NOTICES.md --clobber
```
becomes
```yaml
        run: |
          just notices
          gh release upload "${RELEASE_TAG}" THIRD_PARTY_NOTICES.md --clobber
```

and add the setup-just step after that job's `actions/setup-go` step.

**Must not change in `publish.yml`:** the `wait-for-ci` job's `ci-success` polling loop (it reads the
check-run named `ci-success`); the `image` job's `uses: rknightion/.github/.github/workflows/container-publish.yml@f3169068… # v1.3.1` and its `with:`/`permissions:` blocks; `workflow_call` inputs; the `step-security/harden-runner` step.

### Untouched workflows — do not open them

`release-please.yml`, `auto-rc.yml`, `arm-automerge.yml`, `ghcr-cleanup.yml`, `trigger-docs-sync.yml`,
`codeql.yml`, `scorecard.yml`, `zizmor.yml`, `actionlint.yml`, `dependency-review.yml`,
`docker-security.yml`. Every one is either a GitHub-native security workflow or a thin caller of a
`rknightion/.github` reusable. Never convert a `uses:` into a `run: just`.

## 6. Docs and agent-contract changes

Exact locations (line numbers are current-tree; re-grep before editing):

| File:line | Current | Change |
|---|---|---|
| `README.md:59` | `make build` | `just build` |
| `README.md:87` | `make gate     # vet + test + lint + spdx-check + build  — the green bar…` | `just check   # fmt + build + vet + lint + gen + tests + hygiene — the green bar before any commit` |
| `README.md:88` | `make build    # -> bin/genai-otel-bridge …` | `just build` |
| `README.md:89` | `make test     # go test ./...` | `just test` |
| `README.md:90` | `make lint     # golangci-lint run` | `just lint` |
| `CONTRIBUTING.md:23` | `make gate     # vet + test + lint + spdx-check + build` | `just check` |
| `CONTRIBUTING.md:29-31` | `make build` / `make test` / `make lint` | `just build` / `just test` / `just lint` |
| `CONTRIBUTING.md:35` | ``` `make gate` must pass before any change is merged.``` | ``` `just check` must pass … ``` |
| `CONTRIBUTING.md:44` | `(enforced by \`scripts/spdx-check.sh\`)` | `(enforced by \`just spdx-check\`)` |
| `CONTRIBUTING.md:45` | ``Keep `make gate` green.`` | ``Keep `just check` green.`` |
| `CONTRIBUTING.md:86` | `…installs the tools used by \`make gate\`` | `…installs the tools used by \`just check\`` |
| `AGENTS.md:36-39` | the four-line `make gate/build/test/lint` block | the Task-interface block below |
| `AGENTS.md:91` | ``` `make gate` green before *every* commit``` | ``` `just check` green before *every* commit``` |
| `AGENTS.md:95` | ``` `make ci` is split into a parallel `gate` matrix``` | ``` `just check` is split into a parallel `gate` matrix``` |
| `AGENTS.md:101` | ``**Gate extras:** `make gate` runs `forbidden-words` …`` | ``**Gate extras:** `just check` runs `forbidden-words` …`` |
| `AGENTS.md:122` | ``There is no manual `make changelog` / `git tag` step.`` | drop the `make changelog` phrasing; say "no manual changelog / tag step". |
| `AGENTS.md:136` | ``runs `make notices` and attaches`` | ``runs `just notices` and attaches`` |
| `AGENTS.md:140` | ``deliberately kept out of `make gate``` | ``deliberately kept out of `just check``` |
| `LICENSING.md:26` | ``**`make notices`** → `THIRD_PARTY_NOTICES.md``` | ``**`just notices`** → …`` |
| `LICENSING.md:30` | ``**`make sbom`** → `dist/sbom/…``` | ``**`just sbom`** → …`` |
| `LICENSING.md:35` | ``**not** part of `make gate``` | ``**not** part of `just check``` |
| `cmd/genai-otel-bridge/CLAUDE.md:73` | ``via `make build` ldflags`` | ``via `just build` ldflags`` |
| `deploy/CLAUDE.md:63` | ``by `make generate``` | ``by `just gen``` |
| `deploy/CLAUDE.md:78` | ``(`make gen-dashboard` → commit YAML)`` | ``(`just gen-dashboard` → commit YAML)`` |
| `internal/config/CLAUDE.md:5,18,20,35` | four `make generate` refs | `just gen` |
| `internal/source/langsmith/CLAUDE.md:168` | ``` `make generate` regenerates …``` | ``` `just gen` regenerates …``` |
| `internal/source/portkey/CLAUDE.md:245` | ``` `make generate` regenerates …``` | ``` `just gen` regenerates …``` |
| `deploy/grafana/README.md:74` | ``then `make gen-dashboard` and commit`` | ``then `just gen-dashboard` and commit`` |
| `test/eks/README.md:15,18` | two `make generate` / `make gate` refs | `just gen` / `just check` |
| `docs/installation.md:28` | `make build` | `just build` |
| `docs/dashboards.md:64` | ``run `make gen-dashboard``` | ``run `just gen-dashboard``` |
| `internal/config/config.go:10` | ``` `make generate` (the go:generate runs from the module root…)``` | ``` `just gen` …``` (comment only) |
| `internal/config/config.go:13` | `//go:generate sh -c "cd ../.. && go run ./internal/config/gen"` | leave as-is — `go generate ./internal/config/` must keep working independently of `just`. |
| `test/eks/values_sync_test.go:9` | ``(and `make generate` refreshes …)`` | ``(and `just gen` refreshes …)`` (comment only) |

### Generated-marker strings — these change generated OUTPUT

These four Go string constants are baked into generated files that are gate-checked byte-for-byte.
Editing them **requires** re-running `just gen` and committing the regenerated artifacts in the same
commit, or `gen-check` and the Go drift tests go red:

- `internal/config/gen/helmgen/helmgen.go:43` — ``BeginMarker = "# >>> BEGIN generated config — do not edit by hand; run `make generate` <<<"``
- `internal/config/gen/helmgen/helmgen.go:49` — ``ExampleBeginMarker = "# >>> BEGIN generated source examples — do not edit by hand; run `make generate` <<<"``
- `internal/config/gen/helmgen/helmgen.go:84,87` — the header text embedding `` `make generate` `` twice
- `internal/docs/gen/render/render.go:17` — ``Begin = "<!-- >>> BEGIN generated telemetry catalogue — do not edit by hand; run `make generate` <<< -->"``

Change all of them to `` `just gen` ``, then run `just gen` and commit
`deploy/helm/values.yaml`, `deploy/ecs/terraform/config.example.yaml` and `docs/telemetry.md`
together with the Go change. The corresponding failure messages in
`internal/config/helm_generated_test.go:20,41`, `internal/config/helm_example_test.go:39`,
`internal/config/ecs_config_example_test.go:19,22,41` and
`internal/docs/gen/telemetry_generated_test.go:6,28` are plain test output, not generated content —
update the wording too, but they cannot break the gate.

`docs/telemetry.md:15` and `:18` are generated content; do not hand-edit them, they change when
`just gen` runs.

### `scripts/cloud-environment-setup.sh` — required edits

This is the setup command for Codex Cloud and Claude Code cloud containers. It currently calls
`make tools tools-e2e` (line ~103) and prints `Run make gate to validate changes.` (final line).

1. Add a pinned `JUST_VERSION="1.58.0"`.
2. Add a `install_github_tar` helper mirroring the existing `install_github_zip` (same checksum
   discipline) and install just from
   `https://github.com/casey/just/releases/download/1.58.0/just-1.58.0-x86_64-unknown-linux-musl.tar.gz`
   (aarch64 variant: `just-1.58.0-aarch64-unknown-linux-musl.tar.gz`), verified against that
   release's `SHA256SUMS`, into `$user_bin`. **The just release tag has no `v` prefix** and the
   checksum file is named `SHA256SUMS`, not `checksums.txt` — the existing helper's URL shape
   (`$base/v$version/$archive`) does not fit; do not reuse it blindly.
3. Replace `make tools tools-e2e` with `just setup` (which also does the `go mod download` and the
   `setup-envtest use` prefetch the script performs on the following lines — delete those two now
   that `just setup` covers them, or leave them; they are idempotent).
4. Change the closing line to `Run just check to validate changes.`
5. Add `just --version` to the version-echo block at the end.
6. Keep the `LOCAL AGENTS: Do not execute this script…` header verbatim.
7. `CONTRIBUTING.md:80` still shows `bash scripts/cloud-environment-setup.sh` — that is correct and
   stays; only line 86's `make gate` reference changes.

### AGENTS.md "Task interface" section

Replace the `## Build / test / lint gate` fenced block at `AGENTS.md:34-41` with:

```markdown
## Task interface

This repo's task surface is a `justfile`. Discover it, don't guess it:

    just --list                        # human-readable
    just --dump --dump-format json     # machine-readable
    just --show <recipe>               # what a recipe actually runs

- `just check` is the full gate and is exactly what CI enforces. It must pass before you commit.
- `just ci` additionally runs the Docker/service-container legs (`e2e`, `test-dynamodb`,
  `secret-scan`, `coverage`) that ci.yml runs in separate jobs.
- Prefer `just <recipe>` over the underlying tool. If you are typing `go test`, you want `just test`.
- Run `just` with stdin from /dev/null. Recipes marked `[confirm]` are destructive — stop and ask
  before running one; never pass `--yes` or `JUST_YES=1`.
- If a task you need does not exist, add a recipe with a `#` doc comment and a `[group(...)]`
  rather than running a bare command.
```

Do **not** paste the recipe list into `AGENTS.md`; `just --list` is the live answer. `CLAUDE.md` at
the repo root is a 4-line pointer to `AGENTS.md` and needs no change.

## 7. backlog/config.yml

Current (`backlog/config.yml:4-6`):

```yaml
definition_of_done:
  - "make gate"
  - "go test -tags acceptance ./internal/app/ (only if a §9 acceptance seam changed)"
```

Replace with:

```yaml
definition_of_done:
  - "just check"
  - "just test-acceptance (only if a §9 acceptance seam changed)"
```

`just check` already includes `test-acceptance`, so the second line is now belt-and-braces; keep it —
it is the explicit reminder that a §9 seam change needs the acceptance gate looked at, not merely run.

`backlog/config.yml` is the one Backlog file edited by hand (list-valued keys cannot be set through
`backlog config set`); every other file under `backlog/` must be driven through the `backlog` CLI.

Do **not** rewrite historical acceptance criteria inside closed `backlog/tasks/*.md` — twelve of them
carry a `- [ ] #1 make gate` line. They are a record of what was required at the time, and the files
are CLI-owned. Leave them.

## 8. Order of work

Green at every step. Do not delete anything until step 7.

1. **Write `justfile`.** Add it, run `just --list` (must exit 0 and show every public recipe with a
   doc comment and a group), then `just --fmt` and commit whatever it rewrites. Verify
   `just --fmt --check` is clean and `just --dump --dump-format json` parses.
2. **Prove the gate locally, leg by leg**, before wiring anything to it:
   `just fmt-check`, `just build`, `just typecheck`, `just lint`, `just gen-check`, `just test`,
   `just test-race`, `just test-acceptance`, `just test-envtest`, `just forbidden-words`,
   `just spdx-check`, `just helm-lint`, `just tf-validate`. Then `just check` end to end. Evidence,
   not assertion. `Makefile` is still present and still works throughout — the two can coexist.
3. **Prove the heavier recipes** that CI runs in their own jobs: `just coverage`, `just e2e`
   (needs Docker), `just secret-scan`. `just test-dynamodb` needs a `dynamodb-local` on
   `$DYNAMODB_ENDPOINT`; if you cannot stand one up locally, say so and let CI prove it in step 5.
4. **Generator marker strings + regeneration**, as one commit: edit the four `make generate` strings
   in `internal/config/gen/helmgen/helmgen.go` and `internal/docs/gen/render/render.go`, run
   `just gen`, commit the Go change together with `deploy/helm/values.yaml`,
   `deploy/ecs/terraform/config.example.yaml` and `docs/telemetry.md`. Re-run `just check`.
5. **Switch CI.** Edit `ci.yml` and `publish.yml` per §5 — setup-just steps plus the `run:` swaps.
   Push and watch `ci-success` go green. `Makefile` is still in the tree, so a revert is one file.
6. **Docs, agent contract, backlog config, cloud setup script** per §6 and §7.
7. **Deletions, last.** Re-grep first — `grep -rn 'make \|scripts/envtest.sh\|scripts/sbom.sh' --include='*.md' --include='*.yml' --include='*.yaml' --include='*.go' .` must return nothing outside `archive/`, `followup.md`, `docs/superpowers/` (gitignored scratch), `THIRD_PARTY_NOTICES.md` and the closed `backlog/tasks/*.md`. Then:
   `git rm Makefile scripts/envtest.sh scripts/sbom.sh`. Run `just check` once more and push.

## 9. Traps specific to this repo

1. **The generated markers embed the literal string `make generate`.**
   `internal/config/gen/helmgen/helmgen.go:43,49,84,87` and `internal/docs/gen/render/render.go:17`
   are copied verbatim into `deploy/helm/values.yaml`, `deploy/ecs/terraform/config.example.yaml`
   and `docs/telemetry.md`, which are then compared byte-for-byte by
   `TestHelmGeneratedConfigUpToDate`, `TestECSConfigExampleUpToDate` and the telemetry generated
   test — all inside `just test`. Change the string without regenerating and the whole test leg goes
   red. Step 8.4 exists for this.

2. **`gen-check` mutates the tree when it fails.** It runs `gen` first, so a stale checkout comes
   back with regenerated files in the working tree. That is the fix, not a bug — commit them. Do not
   `git checkout --` them away.

3. **`tf-validate`'s make body has three independent shell lines; a naive `[script]` port changes
   behaviour.** In the Makefile, the `echo "…skipping"; exit 0` when neither `tofu` nor `terraform`
   is present exits only the *first* line — tflint and checkov still run. Inside a single
   `[script('bash')]` recipe, `exit 0` would skip them. The §2 body deliberately uses a `TF=""`
   flag plus `if [ -n "$TF" ]` instead of `exit 0`. Do not "simplify" it back.

4. **`[script('bash')]` does not inherit `set shell`.** Every scripted recipe must start with
   `set -euo pipefail` in its own body. Five recipes are scripted: `test-envtest`, `tf-validate`,
   `secret-scan`, `install-hooks`, `sbom`, plus the four private `_tools*` installers.

5. **Multi-line `if`/`for` are impossible in a line-based recipe.** `install-hooks` (a `for` loop
   over `scripts/git-hooks/*`), the `_tools*` installers (probe-then-install `if` blocks with
   subshells) and `tf-validate` all need `[script('bash')]`. Attempting them as continued lines
   fails with "extra leading whitespace".

6. **`sha256sum` is GNU-only.** `_tools-e2e` and `secret-scan` call it, inherited verbatim from the
   Makefile. On a stock macOS box those recipes fail unless `coreutils` is installed. This is
   pre-existing behaviour; preserve it, do not "fix" it in this task. CI runners are Linux.

7. **`.tools/` binaries are probed for executability, not just presence.** `test -x` alone passes for
   a wrong-arch binary restored from a cross-architecture CI cache, which then dies with "Exec format
   error". Every installer block keeps the `{ test -x … && <binary> <version-cmd> >/dev/null 2>&1; }`
   double-check. Do not simplify it to `test -x`.

8. **Do not `export PATH := tools_dir + ":" + env('PATH')`.** just's exported variables are invisible
   to backtick assignments in the same scope, and `git_version` is a backtick assignment. Invoke every
   pinned tool by its explicit `{{ tools_dir }}/…` path, exactly as the Makefile's recipe bodies
   already did. `scripts/k3d-e2e.sh` sets its own `PATH` from `$TOOLS_DIR`.

9. **`GOFLAGS=-mod=readonly` must stay exported.** A gitignored local `vendor/` flips Go into
   `-mod=vendor`, which goes stale on every dependency bump and diverges from CI. `export GOFLAGS :=
   env('GOFLAGS', '-mod=readonly')` preserves both the default and the env override.

10. **`bin/`, `/dist/`, `*.out` and `/.tools/` are gitignored** (`.gitignore:2,3,8,16`), so `build`,
    `helm-package`, `coverage` and the tool installers do not dirty the tree — `check` stays
    non-mutating with respect to tracked files. `/genai-otel-bridge` (a stray root binary) is ignored
    at `.gitignore:4`; `clean` does not touch it, and neither should you.

11. **`secret-scan` needs the full history and `go env`.** `gitleaks detect --source .` walks every
    commit, which is why CI's checkout uses `fetch-depth: 0`. The recipe also shells out to
    `go env GOOS/GOARCH` for the asset name, so that CI job needs `setup-go` (see §5) or the recipe
    needs `uname`-based detection instead.

12. **`e2e` serialises on one named k3d cluster per machine** (`genai-otel-bridge-e2e`). Two agents
    running `just e2e` concurrently produce failures that look like product bugs. Same for `.tools/`
    — concurrent `just setup` runs corrupt it. This is already recorded in the wave-operating-model
    doc; the recipe names change, the hazard does not.

13. **`just test <filter>` interpolation.** `{{ if filter == "" { "" } else { "-run " + filter } }}`
    is a stable just conditional. Do not switch to a list literal or `set positional-arguments`
    without checking; list literals are unstable in 1.58 and one unstable feature makes
    `just --list` and `just --dump` exit 1 for the whole file, blinding every agent.

14. **`Dockerfile:23` and `Dockerfile.kaniko:30` call `bash scripts/notices.sh` directly.** They run
    inside the image build where `just` does not exist. Do not "unify" them onto `just notices`, and
    do not delete `scripts/notices.sh`.

15. **`just --fmt --check` works without `--unstable` on 1.58.0** (verified). Older `just` builds
    require `--unstable` for `--fmt`. CI pins 1.58.0; if a contributor's local `just` is older,
    `fmt-check` will fail on the flag, not on the formatting — tell them to upgrade rather than
    adding `--unstable`.

## 10. Out of scope — do not touch

- **Every KEEP script:** `scripts/notices.sh`, `scripts/publish.sh`, `scripts/k3d-e2e.sh`,
  `scripts/forbidden-words.sh`, `scripts/spdx-check.sh`, `scripts/lib/private-paths.sh`,
  `scripts/lib/forbidden-words-pattern.sh`, `scripts/git-hooks/pre-commit`,
  `test/eks/make-genai-otel-bridge-secret.sh`, `deploy/grafana/self-obs/gen_dashboard.py`,
  `scripts/notices.tsv.tmpl`, `scripts/forbidden-words.local.example`. Their *contents* stay
  byte-identical. (`scripts/cloud-environment-setup.sh` is the one KEEP script that IS edited — §6.)
- **Every GitHub-native / reusable-caller workflow:** `release-please.yml`, `auto-rc.yml`,
  `arm-automerge.yml`, `ghcr-cleanup.yml`, `trigger-docs-sync.yml`, `codeql.yml`, `scorecard.yml`,
  `zizmor.yml`, `actionlint.yml`, `dependency-review.yml`, `docker-security.yml`. Do not open them.
- **The `ci-success` aggregator** in `ci.yml` and the `wait-for-ci` polling job in `publish.yml`.
- **The `image` job in `publish.yml`** and its `container-publish.yml` reusable call.
- **The broker-token steps** in `release-please.yml` and `trigger-docs-sync.yml`. Never provision
  `RELEASE_PLEASE_TOKEN` or `DOCS_SYNC_PAT`.
- **`Dockerfile`, `Dockerfile.kaniko`, `.dockerignore`.**
- **`renovate.json`, `.codacy.yaml`, `.gitleaks.toml`, `.golangci.yml`, `release-please-config.json`,
  `.release-please-manifest.json`, `docs.toml`.**
- **All Go application code.** The only Go edits in this task are the four generated-marker strings
  and the comment/test-message wording in §6.
- **`deploy/ecs/terraform/`, `deploy/helm/` templates, `test/e2e/`, `test/integration/`,
  `test/eks/values-eks.yaml`.** Generated files under `deploy/` change only as output of `just gen`.
- **`backlog/tasks/*.md`, `backlog/docs/*.md`, `backlog/decisions/`** — CLI-owned, never hand-edited.
  Only `backlog/config.yml` is edited by hand.
- **`archive/`, `followup.md`, `THIRD_PARTY_NOTICES.md`, `CHANGELOG.md`, `docs/superpowers/`**
  (gitignored scratch). Their `make` references are historical record or generated output.
- **`ARCHITECTURE.md` FROZEN seams** and anything requiring an ARCHITECTURE.md decision entry. This
  is a task-surface migration, not a design change.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A top-level justfile exists defining all seven mandatory recipes (default, setup, fmt, fmt-check, lint, test, check), with set shell := ["bash", "-euo", "pipefail", "-c"] as its header and no unstable just features
- [x] #2 just check passes locally and is the union of ci.yml's gate matrix legs: build, typecheck (go vet), lint (plain + acceptance build tags), gen-check, test, test-race, test-acceptance, test-envtest, forbidden-words, spdx-check, helm-lint, tf-validate, fmt-check
- [x] #3 just --fmt --check exits 0, and just --list plus just --dump --dump-format json exit 0 with a # doc comment and a [group(...)] on every public recipe; only default and setup are ungrouped
- [x] #4 Makefile is deleted (git rm Makefile); scripts/envtest.sh and scripts/sbom.sh are absorbed into the test-envtest and sbom recipes and deleted
- [x] #5 Every KEEP script is byte-identical and reachable via a recipe: notices.sh via just notices, publish.sh via just publish ([confirm]-gated), k3d-e2e.sh via just e2e / k3d-up / k3d-down, forbidden-words.sh via just forbidden-words, spdx-check.sh via just spdx-check, gen_dashboard.py via just gen-dashboard, git-hooks/pre-commit via just install-hooks
- [x] #6 ci.yml's gate matrix, e2e, secret-scan, dynamodb-backends and coverage jobs call just recipes via a pinned extractions/setup-just@53165ef7e734c5c07cb06b3c8e7b647c5aa16db3 # v4 step with just-version '1.58.0'; publish.yml's notices job runs just notices; the ci-success job (name, if: always(), needs: [gate, e2e, secret-scan, dynamodb-backends]) and every rknightion/.github reusable uses: call are unchanged
- [x] #7 The four generated-marker strings embedding 'make generate' in internal/config/gen/helmgen/helmgen.go and internal/docs/gen/render/render.go say 'just gen', and deploy/helm/values.yaml, deploy/ecs/terraform/config.example.yaml and docs/telemetry.md are regenerated and committed in the same commit so the drift tests stay green
- [x] #8 AGENTS.md carries the Task interface section naming just check as the gate (no pasted recipe list), and README.md, CONTRIBUTING.md, LICENSING.md, docs/installation.md, docs/dashboards.md, deploy/grafana/README.md, test/eks/README.md and the nested CLAUDE.md files under cmd/ deploy/ and internal/ no longer reference make
- [x] #9 scripts/cloud-environment-setup.sh installs just 1.58.0 from casey/just with SHA256SUMS verification, calls just setup instead of make tools tools-e2e, and closes by naming just check
- [x] #10 backlog/config.yml's definition_of_done reads 'just check' and 'just test-acceptance (only if a §9 acceptance seam changed)'
<!-- AC:END -->

## Definition of Done
<!-- DOD:BEGIN -->
- [x] #1 make gate
- [x] #2 go test -tags acceptance ./internal/app/ (only if a §9 acceptance seam changed)
<!-- DOD:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Snapshot the existing Makefile, workflow task calls, kept-script hashes, and allowed documentation references.
2. Add the prescribed justfile, validate its syntax/format/task surface, and prove its bare-toolchain gate leg by leg.
3. Update generator markers and regenerate their committed artifacts; migrate the allowed docs, bootstrap script, and Backlog definition of done.
4. Switch only ci.yml and publish.yml task invocations while preserving reusable calls and ci-success; re-grep before the final deletions.
5. Remove Makefile and absorbed scripts, run proportionate local gates and CodeRabbit, commit/push main, verify CI at the exact final SHA, then finalize atomically.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Campaign coordination authorizes this formerly Wave 2 lane under the current unified campaign; the historical wave-start gate is superseded while the task standards remain binding.

Validation: just check; Just format/list/dump; Actionlint; targeted Zizmor; shell syntax; preserved-file hashes; official Just release checksum probes; and CodeRabbit (0 findings) passed. Exact source commit c3220bb5ecfc08017fd3e3386fd9e09a2de24ef9 passed CI run 33256695268, including Linux e2e and ci-success. The known local macOS/OrbStack e2e failure was not replayed after CI supplied the Linux proof.
<!-- SECTION:NOTES:END -->

## Comments

<!-- COMMENTS:BEGIN -->
author: campaign-ordering
created: 2026-08-29 09:18
---
## Fleet ordering — WAVE 2. Starts after the Wave 0 pilot (`sf2loki` / SFL-0073) and the Wave 1 hubs land.

Within Wave 2 the order is free — these repos do not depend on each other. Batching by language is worthwhile so one lane reuses its Makefile-to-recipe mapping across similar repos.

Do not start before the pilot reports. The standard may be amended off the back of it, and picking this up early risks coding against a superseded seam.

**Provisioning `just` in CI.** Which mechanism depends on the runner, and the two must not be mixed:

| Runner | Mechanism |
| --- | --- |
| `arc-arm64` (m7kni self-hosted) | `just` is **baked into the runner image** by `m7kni/ci-tools` (`runner-image/Dockerfile`, `ARG JUST_VERSION`). Do **not** add `extractions/setup-just`, and delete the step if this repo already has one — it installs a second `just` earlier on `PATH` and turns the image pin into a lie. |
| GitHub-hosted (all `rknightion` repos) | `extractions/setup-just`, SHA-pinned, with an explicit `just-version:`. |

Both sides currently sit on **1.58.0** and are Renovate-managed. `ci-tools`' `Tool version drift` workflow fails if the Dockerfile `ARG` and the published image ever disagree, and lists any repo still carrying a second pin.

**While you are in the workflow files, check the hub pin.** On 2026-08-29 Renovate was unfrozen for `rknightion/.github` in `m7kni/renovate-config` — it had been `enabled: false` on the mistaken belief that callers tracked `@main`, which froze the fleet across 19 different hub SHAs (v1.3.1 June → v1.9.7 August) so that no hub fix ever propagated. Bumps now arrive as one grouped, CI-gated, automerged PR per repo. **A `uses:` whose comment is not a real `# vX.Y.Z` still cannot be bumped** (it resolves to a digest-only update, which the fleet rules disable) — if you find one, repair the comment as part of this task.
---

author: campaign-ordering
created: 2026-08-29 10:42
---
## Standard amendment — `ci` is the sanctioned superset of `check` (RATIFIED)

This supersedes the frozen wording *"`check` is the complete local gate and reproduces every CI job that can run off a GitHub runner"*, which several lanes could not honour without making the pre-commit gate depend on a Docker daemon.

**The definitions now are:**

- **`check`** — everything that runs with **only the language toolchain installed**. This is the pre-commit gate. A leg that runs on a bare toolchain belongs here *however long it takes*.
- **`ci`** — `check` plus the legs CI gates that need a **Docker daemon, a service container, or cross-compilation**, and nothing else. Written as `ci: check <heavy legs>`.

**Every leg you put in `ci` must carry a comment naming which of those three it needs.** That comment is the guard: without it `ci` becomes the bin for anything slow or awkward, `check` quietly stops meaning much, and the fleet is back to a per-repo gate.

Eleven of the 42 lanes arrived at this shape independently before it was ratified, which is why it won.

**If this repo has no such legs, it has no `ci` recipe at all** and `check` is the whole gate. Do not add an empty one.
---
<!-- COMMENTS:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Replaced the Makefile task surface with Just, migrated CI, documentation, generated markers, Renovate pins, and the cloud bootstrap, and removed the absorbed wrappers. Local gates and the exact-SHA GitHub CI run passed. Cloud bootstrap installation was syntax- and release-asset-verified but not run in a cloud container; the successful GitHub-hosted Linux e2e is the runtime proof.
<!-- SECTION:FINAL_SUMMARY:END -->
