set shell := ["bash", "-euo", "pipefail", "-c"]

# renovate: datasource=github-releases depName=golangci/golangci-lint versioning=semver
golangci_lint_version := env('GOLANGCI_LINT_VERSION', 'v2.15.0')
# renovate: datasource=github-tags depName=kubernetes-sigs/controller-runtime versioning=loose
setup_envtest_version := env('SETUP_ENVTEST_VERSION', 'release-0.23')
# renovate: datasource=github-releases depName=kubernetes-sigs/controller-tools versioning=semver extractVersion=^envtest-v(?<version>.+)$
envtest_k8s_version := env('ENVTEST_K8S_VERSION', '1.37.0')
# renovate: datasource=github-releases depName=helm/helm versioning=semver
helm_version := env('HELM_VERSION', 'v4.3.2')
# renovate: datasource=github-releases depName=k3d-io/k3d versioning=semver
k3d_version := env('K3D_VERSION', 'v5.9.0')
# renovate: datasource=docker depName=rancher/k3s versioning=regex:^v(?<major>\d+)\.(?<minor>\d+)\.(?<patch>\d+)-k3s(?<build>\d+)$
k3s_image := env('K3S_IMAGE', 'rancher/k3s:v1.37.2-k3s1')
# renovate: datasource=github-releases depName=google/go-licenses versioning=semver
go_licenses_version := env('GO_LICENSES_VERSION', 'v2.0.1')
# renovate: datasource=github-releases depName=anchore/syft versioning=semver
syft_version := env('SYFT_VERSION', 'v1.57.2')
# renovate: datasource=github-releases depName=gitleaks/gitleaks versioning=semver extractVersion=^v(?<version>.*)$
gitleaks_version := env('GITLEAKS_VERSION', '8.30.1')

# renovate: datasource=pypi depName=pyyaml versioning=pep440
pyyaml_version := env('PYYAML_VERSION', '6.0.4')

image := env('IMAGE', 'genai-otel-bridge:dev')
e2e_helper_image := env('E2E_HELPER_IMAGE', 'genai-otel-bridge-e2e-helper:dev')

go := env('GO', 'go')
tools_dir := justfile_directory() / ".tools"
git_version := `git describe --tags --always --dirty 2>/dev/null || echo dev`
ldflags := "-X github.com/rknightion/genai-otel-bridge/internal/version.Version=" + git_version

# Force module mode for all Go tooling so local vendor/ trees cannot diverge from CI.
export GOFLAGS := env('GOFLAGS', '-mod=readonly')

# Show the task surface.
default:
    @just --list

# Install the pinned dev and e2e tools, fetch modules, and prefetch envtest assets.
setup: _tools _tools-e2e
    {{ go }} mod download
    {{ tools_dir }}/setup-envtest use {{ envtest_k8s_version }} --bin-dir {{ tools_dir }}/envtest >/dev/null

# Prepare the source gate toolchain and compile the service and integration test binaries.
cloud-bootstrap: setup _tools-gitleaks
    {{ go }} build ./...
    scratch="$(mktemp -d)"; trap 'rm -rf "$scratch"' EXIT; {{ go }} test -c -tags acceptance -o "$scratch/acceptance.test" ./internal/app/; {{ go }} test -c -tags envtest -o "$scratch/envtest.test" ./test/integration/...

# The pre-commit gate: every CI leg that runs with only the language toolchain installed.
[group('check')]
check: fmt-check build typecheck lint gen-check test test-race test-acceptance test-envtest coverage secret-scan forbidden-words spdx-check helm-lint tf-validate

# CI superset: test-dynamodb needs the DynamoDB Local service container; e2e needs a Docker daemon for k3d and image builds.
[group('check')]
ci: check test-dynamodb e2e

# Format Go sources and this justfile in place.
[group('check')]
fmt:
    git ls-files -z -- '*.go' | xargs -0 gofmt -s -w
    just --fmt

# Verify Go and justfile formatting without mutating tracked files.
[group('check')]
[no-exit-message]
fmt-check:
    just --fmt --check
    @out="$(git ls-files -z -- '*.go' | xargs -0 gofmt -s -l)"; if [ -n "$out" ]; then echo "gofmt: needs formatting (run 'just fmt'):"; echo "$out"; exit 1; fi

# Run go vet for the whole module.
[group('check')]
[no-exit-message]
typecheck:
    {{ go }} vet ./...

# Run golangci-lint for plain, acceptance-tagged and envtest/e2e-tagged builds.
[group('check')]
[no-exit-message]
lint: _tools
    {{ tools_dir }}/golangci-lint run
    {{ tools_dir }}/golangci-lint run --build-tags acceptance
    {{ tools_dir }}/golangci-lint run --build-tags "acceptance envtest e2e"

# Run the default Go suite; an optional filter maps to go test -run.
[group('check')]
[no-exit-message]
test filter="":
    {{ go }} test {{ if filter == "" { "" } else { "-run " + filter } }} ./...

# Run the default Go suite under the race detector.
[group('check')]
[no-exit-message]
test-race:
    {{ go }} test -race ./...

# Run the acceptance gates.
[group('check')]
[no-exit-message]
test-acceptance:
    {{ go }} test -race -tags acceptance ./internal/app/

# Run envtest integration against a local kube-apiserver and etcd.
[group('check')]
[no-exit-message]
[script('bash')]
test-envtest: _tools
    set -euo pipefail
    assets="$({{ tools_dir }}/setup-envtest use {{ envtest_k8s_version }} --bin-dir {{ tools_dir }}/envtest -p path)"
    export KUBEBUILDER_ASSETS="$assets"
    echo "envtest assets: $KUBEBUILDER_ASSETS"
    {{ go }} test -tags envtest -count=1 ./test/integration/...

# Run DynamoDB tests; this CI-only leg needs the dynamodb-local service container at DYNAMODB_ENDPOINT.
[group('check')]
[no-exit-message]
test-dynamodb:
    {{ go }} test ./internal/coordinate/dynamodb/... ./internal/checkpoint/dynamodb/...
    {{ go }} test -tags acceptance ./internal/app/ -run TestDynamoDB

# Write the unit-scope coverage profile used by the Codacy CI job.
[group('check')]
[no-exit-message]
coverage:
    {{ go }} test -covermode=atomic -coverprofile=coverage.out ./...

# Scan the whole Git history for credential shapes and forbidden identifiers.
[group('check')]
[no-exit-message]
secret-scan: _tools-gitleaks
    {{ tools_dir }}/gitleaks detect --source . --redact --no-banner

[private]
[script('bash')]
_tools-gitleaks:
    set -euo pipefail
    mkdir -p {{ tools_dir }}
    if ! { test -x {{ tools_dir }}/gitleaks && {{ tools_dir }}/gitleaks version 2>/dev/null | grep -Fqx -e "{{ gitleaks_version }}" -e "v{{ gitleaks_version }}"; }; then
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

# Scan the tracked tree for credential shapes and forbidden identifiers.
[group('check')]
[no-exit-message]
forbidden-words:
    @if [ -f scripts/forbidden-words.sh ]; then bash scripts/forbidden-words.sh; else echo "forbidden-words: skipped (guard not present in this repo)"; fi

# Require Apache-2.0 SPDX headers on tracked non-vendor Go sources.
[group('check')]
[no-exit-message]
spdx-check:
    bash scripts/spdx-check.sh

# Lint the bundled Helm chart.
[group('check')]
[no-exit-message]
[script('bash')]
helm-lint: _tools-e2e _tools-pyyaml
    set -euo pipefail
    helm="{{ tools_dir }}/helm"
    "$helm" lint deploy/helm
    HELM="$helm" {{ tools_dir }}/pyenv/bin/python test/helm/config_revisions.py
    # Render with default values: a workload with an empty image (e.g. the cleanup hook) fails here, not at uninstall.
    rendered=$("$helm" template rel deploy/helm)
    if printf '%s\n' "$rendered" | grep -nE '^[[:space:]]+image:[[:space:]]*$'; then echo 'helm-lint: a default render has an empty container image' >&2; exit 1; fi
    # The default render must not drift: compare with the committed render (regenerate it with
    # `just helm-default-render > test/helm/default-render.yaml` when a template change is intended).
    if ! just helm-default-render | diff -u test/helm/default-render.yaml -; then echo 'helm-lint: the default render differs from test/helm/default-render.yaml' >&2; exit 1; fi
    # Tenant shape: the example renders one optional Secret volume per tenant.
    example=$("$helm" template rel deploy/helm -f deploy/helm/examples/tenants-values.yaml)
    for t in team-a team-b; do
      printf '%s\n' "$example" | grep -qxF "            secretName: genai-otel-bridge-tenant-$t" || { echo "helm-lint: the example renders no Secret volume for $t" >&2; exit 1; }
    done
    [ "$(printf '%s\n' "$example" | grep -c 'secretName: genai-otel-bridge-tenant-')" = 2 ] || { echo 'helm-lint: the example must render exactly two tenant Secret volumes' >&2; exit 1; }
    # Failing variants: any tenant needs maxUnavailable 0, and a configOverride must parse.
    if "$helm" template rel deploy/helm -f deploy/helm/examples/tenants-values.yaml --set rollingUpdate.maxUnavailable=1 >/dev/null 2>&1; then echo 'helm-lint: tenants with maxUnavailable 1 must fail to render' >&2; exit 1; fi
    if "$helm" template rel deploy/helm --set-string 'configOverride=a: [' >/dev/null 2>&1; then echo 'helm-lint: a configOverride that does not parse must fail to render' >&2; exit 1; fi
    # The packaged chart must not carry the examples.
    pkg=$(mktemp -d); trap 'rm -rf "$pkg"' EXIT
    "$helm" package deploy/helm --destination "$pkg" >/dev/null
    if tar -tzf "$pkg"/*.tgz | grep -F 'examples/'; then echo 'helm-lint: the packaged chart lists examples/' >&2; exit 1; fi

# The default chart render with a fixed release name, minus the lines that carry the chart or app version.
# helm-lint compares it with test/helm/default-render.yaml.
[private]
[script('bash')]
helm-default-render: _tools-e2e
    set -euo pipefail
    {{ tools_dir }}/helm template rel deploy/helm | grep -vE 'helm\.sh/chart:|app\.kubernetes\.io/version:|checksum/config:|image: ghcr\.io/rknightion/genai-otel-bridge:'

# Format, validate, lint, and scan the ECS Terraform module; absent IaC tools self-skip.
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

# Run the full k3d failover suite; this CI-only leg needs a Docker daemon.
[group('check')]
[no-exit-message]
e2e: helm-package image-local _tools-e2e
    IMAGE={{ image }} E2E_HELPER_IMAGE={{ e2e_helper_image }} K3S_IMAGE={{ k3s_image }} bash scripts/k3d-e2e.sh all

# Regenerate Helm, ECS, telemetry documentation and the self-observability dashboard.
[group('gen')]
gen: _tools-pyyaml
    {{ go }} run ./internal/config/gen
    {{ go }} run ./internal/docs/gen
    {{ tools_dir }}/pyenv/bin/python deploy/grafana/self-obs/gen_dashboard.py

# Regenerate artifacts and fail when their committed output drifts.
[group('gen')]
[no-exit-message]
gen-check: gen
    @git diff --exit-code -- deploy/helm/values.yaml deploy/ecs/terraform/config.example.yaml docs/telemetry.md deploy/grafana/self-obs/dashboard-self-obs.yaml || { echo "generated files are stale — run 'just gen' and commit"; exit 1; }

# Regenerate the self-observability Grafana dashboard manifest.
[group('gen')]
gen-dashboard: _tools-pyyaml
    {{ tools_dir }}/pyenv/bin/python deploy/grafana/self-obs/gen_dashboard.py

# Compile every package and write the version-stamped service binary.
[group('build')]
build:
    {{ go }} build ./...
    {{ go }} build -ldflags '{{ ldflags }}' -o bin/genai-otel-bridge ./cmd/genai-otel-bridge

# Build the multi-architecture container image without pushing it.
[group('build')]
image tag=image:
    docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION={{ git_version }} -t '{{ tag }}' -f Dockerfile .

# Build app and e2e-helper images into the local Docker daemon.
[group('build')]
image-local:
    docker build --build-arg VERSION={{ git_version }} -t {{ image }} -f Dockerfile .
    docker build -t {{ e2e_helper_image }} -f test/e2e/harness/Dockerfile .

# Package the Helm chart into dist.
[group('build')]
helm-package: _tools-e2e
    {{ tools_dir }}/helm package deploy/helm -d dist/

# Run the bridge locally against a config file.
[group('dev')]
run config="config.yaml":
    {{ go }} run ./cmd/genai-otel-bridge --config '{{ config }}'

# Create the local k3d e2e cluster and install the chart.
[group('dev')]
k3d-up: _tools-e2e
    IMAGE={{ image }} E2E_HELPER_IMAGE={{ e2e_helper_image }} K3S_IMAGE={{ k3s_image }} bash scripts/k3d-e2e.sh up

# Delete the local k3d e2e cluster.
[group('dev')]
k3d-down: _tools-e2e
    bash scripts/k3d-e2e.sh down

# Symlink repository hooks into .git/hooks.
[group('dev')]
[script('bash')]
install-hooks:
    set -euo pipefail
    mkdir -p .git/hooks
    for h in scripts/git-hooks/*; do
      ln -sf "../../$h" ".git/hooks/$(basename "$h")"
      echo "installed $(basename "$h")"
    done

# Remove reproducible build output and the pinned local toolchain.
[group('dev')]
clean:
    rm -rf bin dist coverage.out {{ tools_dir }}

# Regenerate third-party notices from the shipped binary import graph.
[group('release')]
notices: _tools-licensing
    GO_LICENSES={{ tools_dir }}/go-licenses bash scripts/notices.sh

# Write SPDX and CycloneDX SBOMs for a build artifact.
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

# Publish an image and chart to a real registry after confirmation.
[confirm('publish pushes the image and chart to a real registry — continue?')]
[group('release')]
publish: _tools-e2e
    HELM={{ tools_dir }}/helm bash scripts/publish.sh

[private]
[script('bash')]
_tools:
    set -euo pipefail
    mkdir -p {{ tools_dir }}
    os="$({{ go }} env GOOS)"; arch="$({{ go }} env GOARCH)"
    version="{{ golangci_lint_version }}"; release_version="${version#v}"
    if ! { test -x {{ tools_dir }}/golangci-lint && {{ tools_dir }}/golangci-lint version --short | grep -Fqx "$release_version"; }; then
      asset="golangci-lint-${release_version}-$os-$arch.tar.gz"
      base="https://github.com/golangci/golangci-lint/releases/download/$version"
      tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
      curl -sSfLo "$tmp/$asset" "$base/$asset"
      curl -sSfLo "$tmp/checksums.txt" "$base/golangci-lint-${release_version}-checksums.txt"
      (cd "$tmp" && grep " ${asset}\$" checksums.txt | sha256sum -c -)
      tar -xzf "$tmp/$asset" -C "$tmp"
      install -m 0755 "$tmp/golangci-lint-${release_version}-$os-$arch/golangci-lint" {{ tools_dir }}/golangci-lint
    fi
    setup_envtest_module_version="$({{ go }} list -m -json sigs.k8s.io/controller-runtime/tools/setup-envtest@{{ setup_envtest_version }} | awk -F '"' '$2 == "Version" { print $4; exit }')"
    if ! { test -x {{ tools_dir }}/setup-envtest && {{ go }} version -m {{ tools_dir }}/setup-envtest | awk '$1 == "mod" { print $3; exit }' | grep -Fqx "$setup_envtest_module_version"; }; then
      GOBIN={{ tools_dir }} {{ go }} install sigs.k8s.io/controller-runtime/tools/setup-envtest@{{ setup_envtest_version }}
    fi

[private]
[script('bash')]
_tools-pyyaml:
    set -euo pipefail
    mkdir -p {{ tools_dir }}
    if ! { test -x {{ tools_dir }}/pyenv/bin/python && {{ tools_dir }}/pyenv/bin/python -c 'import sys, yaml; sys.exit(0 if yaml.__version__ == "{{ pyyaml_version }}" else 1)' 2>/dev/null; }; then
      python3 -m venv --clear {{ tools_dir }}/pyenv
      {{ tools_dir }}/pyenv/bin/python -m pip install --quiet --disable-pip-version-check 'pyyaml=={{ pyyaml_version }}'
    fi

[private]
[script('bash')]
_tools-e2e:
    set -euo pipefail
    mkdir -p {{ tools_dir }}
    os="$({{ go }} env GOOS)"; arch="$({{ go }} env GOARCH)"
    # Per-run download directory: concurrent installs on one host must not share file names.
    tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
    if ! { test -x {{ tools_dir }}/helm && {{ tools_dir }}/helm version --short 2>/dev/null | cut -d+ -f1 | grep -Fqx "{{ helm_version }}"; }; then
      tarball="helm-{{ helm_version }}-$os-$arch.tar.gz"
      curl -sSfLo "$tmp/$tarball" "https://get.helm.sh/$tarball"
      curl -sSfLo "$tmp/$tarball.sha256sum" "https://get.helm.sh/$tarball.sha256sum"
      (cd "$tmp" && sha256sum -c "$tarball.sha256sum")
      tar -xzf "$tmp/$tarball" -C "$tmp"
      install -m 0755 "$tmp/$os-$arch/helm" {{ tools_dir }}/helm
    fi
    if ! { test -x {{ tools_dir }}/k3d && {{ tools_dir }}/k3d version 2>/dev/null | awk '$1 == "k3d" && !seen { print $3; seen = 1 }' | grep -Fqx "{{ k3d_version }}"; }; then
      curl -sSfLo "$tmp/k3d-checksums.txt" "https://github.com/k3d-io/k3d/releases/download/{{ k3d_version }}/checksums.txt"
      curl -sSfLo "$tmp/k3d-bin" "https://github.com/k3d-io/k3d/releases/download/{{ k3d_version }}/k3d-$os-$arch"
      want="$(grep "k3d-$os-$arch\$" "$tmp/k3d-checksums.txt" | awk '{print $1}')"
      test -n "$want" || { echo "k3d: no checksum entry for $os-$arch in checksums.txt"; exit 1; }
      echo "$want  $tmp/k3d-bin" | sha256sum -c -
      install -m 0755 "$tmp/k3d-bin" {{ tools_dir }}/k3d
    fi
    if ! { test -x {{ tools_dir }}/kubectl && {{ tools_dir }}/kubectl version --client -o json 2>/dev/null | grep -Fq '"gitVersion": "v{{ envtest_k8s_version }}"'; }; then
      curl -sSfLo "$tmp/kubectl-bin" "https://dl.k8s.io/release/v{{ envtest_k8s_version }}/bin/$os/$arch/kubectl"
      curl -sSfLo "$tmp/kubectl-bin.sha256" "https://dl.k8s.io/release/v{{ envtest_k8s_version }}/bin/$os/$arch/kubectl.sha256"
      echo "$(cat "$tmp/kubectl-bin.sha256")  $tmp/kubectl-bin" | sha256sum -c -
      install -m 0755 "$tmp/kubectl-bin" {{ tools_dir }}/kubectl
    fi

[private]
[script('bash')]
_tools-licensing:
    set -euo pipefail
    mkdir -p {{ tools_dir }}
    version="{{ go_licenses_version }}"
    major="${version#v}"; major="${major%%.*}"
    module="github.com/google/go-licenses"
    if [ "$major" -ge 2 ]; then module="$module/v$major"; fi
    installed_module=""
    installed_version=""
    if { test -x {{ tools_dir }}/go-licenses && {{ tools_dir }}/go-licenses --help >/dev/null 2>&1; }; then
      installed_module="$({{ go }} version -m {{ tools_dir }}/go-licenses | awk '$1 == "path" { print $2; exit }')"
      installed_version="$({{ go }} version -m {{ tools_dir }}/go-licenses | awk '$1 == "mod" { print $3; exit }')"
    fi
    if [ "$installed_module" != "$module" ] || [ "$installed_version" != "$version" ]; then
      GOBIN={{ tools_dir }} {{ go }} install "$module@$version"
    fi

[private]
[script('bash')]
_tools-sbom:
    set -euo pipefail
    mkdir -p {{ tools_dir }}
    if ! { test -x {{ tools_dir }}/syft && {{ go }} version -m {{ tools_dir }}/syft | awk '$1 == "mod" && !seen { print $3; seen = 1 }' | grep -Fqx "{{ syft_version }}"; }; then
      GOBIN={{ tools_dir }} {{ go }} install github.com/anchore/syft/cmd/syft@{{ syft_version }}
    fi

# GitHub issues and the rknightion Project; maintainers only (needs the tracker tools and credentials)
[group('tracker')]
[positional-arguments]
tracker *args:
    #!/usr/bin/env bash
    set -euo pipefail
    export TRACKER_REPO=rknightion/genai-otel-bridge TRACKER_PROJECT_OWNER=rknightion TRACKER_PROJECT=7
    export TRACKER_ISSUE_IDENTITY=app TRACKER_PROJECT_IDENTITY=user
    exec tracker-with-token "$@"

# print the issue a legacy board id (GOB-0001) became
[group('tracker')]
issue id:
    TRACKER_REPO=rknightion/genai-otel-bridge tracker legacy {{ quote(id) }}
