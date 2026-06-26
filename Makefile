GO ?= go
LDFLAGS := -X github.com/rknightion/genai-otel-bridge/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# ── pinned tool versions (override via env; majors are load-bearing) ──────────
GOLANGCI_LINT_VERSION ?= v2.12.2
SETUP_ENVTEST_VERSION ?= release-0.23
ENVTEST_K8S_VERSION   ?= 1.35.0
HELM_VERSION          ?= v3.18.3
K3D_VERSION           ?= v5.9.0
K3S_IMAGE             ?= rancher/k3s:v1.35.1-k3s1
GIT_CLIFF_VERSION     ?= v2.13.1
IMAGE                 ?= genai-otel-bridge:dev
E2E_HELPER_IMAGE      ?= genai-otel-bridge-e2e-helper:dev

TOOLS_DIR := $(CURDIR)/.tools
export PATH := $(TOOLS_DIR):$(PATH)

# git-cliff: prefer one already on PATH (e.g. `brew install git-cliff`), else the pinned .tools binary.
GIT_CLIFF := $(shell command -v git-cliff 2>/dev/null || echo $(TOOLS_DIR)/git-cliff)

.PHONY: build test vet lint gate generate generate-check \
        tools tools-e2e \
        ci ci-build ci-vet ci-lint ci-lint-acceptance ci-test ci-race ci-acceptance ci-envtest \
        forbidden-words spdx-check helm-lint changelog install-hooks gen-dashboard \
        ci-e2e image image-local helm-package k3d-up k3d-down k3d-e2e \
        publish

# ── legacy (kept for local muscle memory) ─────────────────────────────────────
build:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/genai-otel-bridge ./cmd/genai-otel-bridge
test:
	$(GO) test ./...
vet:
	$(GO) vet ./...
lint: tools
	$(TOOLS_DIR)/golangci-lint run
gate: vet test lint forbidden-words spdx-check
	$(GO) build ./...

# ── code generation ───────────────────────────────────────────────────────────
# Regenerate the Helm chart's default `config:` block in deploy/helm/values.yaml from the Go config
# schema (internal/config/config.go). Run after changing any config field/tag/default/doc-comment.
# TestHelmGeneratedConfigUpToDate (in the gate's `test`) fails if this output is not committed.
generate:
	$(GO) run ./internal/config/gen
# generate-check verifies the committed values.yaml is up to date without modifying the tree (CI use).
generate-check: generate
	@git diff --exit-code -- deploy/helm/values.yaml || \
	  (echo "deploy/helm/values.yaml is stale — run 'make generate' and commit" && exit 1)
# Regenerate the self-observability dashboard manifest (deploy/grafana/self-obs/dashboard-self-obs.yaml)
# from its Python generator. Run after editing gen_dashboard.py; commit the emitted YAML. Needs PyYAML.
gen-dashboard:
	python3 deploy/grafana/self-obs/gen_dashboard.py

# ── tooling (idempotent; installs into .tools/) ───────────────────────────────
tools:
	@mkdir -p $(TOOLS_DIR)
	@# Probe that the cached binary actually EXECUTES on this arch, not just `test -x` — a CI cache
	@# restored across architectures (a wrong-arch binary) passes `test -x` but dies with "Exec format
	@# error", and the old guard would never rebuild it. Re-install when missing OR not runnable here.
	@{ test -x $(TOOLS_DIR)/golangci-lint && $(TOOLS_DIR)/golangci-lint version >/dev/null 2>&1; } || \
	  curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/$(GOLANGCI_LINT_VERSION)/install.sh | sh -s -- -b $(TOOLS_DIR) $(GOLANGCI_LINT_VERSION)
	@{ test -x $(TOOLS_DIR)/setup-envtest && $(TOOLS_DIR)/setup-envtest --help >/dev/null 2>&1; } || \
	  GOBIN=$(TOOLS_DIR) $(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

tools-e2e:
	@mkdir -p $(TOOLS_DIR)
	@# Probe each cached binary actually executes on this arch (not just `test -x`): a CI cache restored
	@# across architectures leaves a wrong-arch binary that passes `test -x` but dies ("Exec format error",
	@# or a shell "Syntax error" when sh tries to interpret it). Re-install when missing OR not runnable.
	@{ test -x $(TOOLS_DIR)/helm && $(TOOLS_DIR)/helm version --short >/dev/null 2>&1; } || \
	  (curl -sSfL https://get.helm.sh/helm-$(HELM_VERSION)-$$($(GO) env GOOS)-$$($(GO) env GOARCH).tar.gz | tar -xz -C /tmp && \
	   mv /tmp/$$($(GO) env GOOS)-$$($(GO) env GOARCH)/helm $(TOOLS_DIR)/helm)
	@{ test -x $(TOOLS_DIR)/k3d && $(TOOLS_DIR)/k3d version >/dev/null 2>&1; } || \
	  (curl -sSfL https://raw.githubusercontent.com/k3d-io/k3d/main/install.sh | TAG=$(K3D_VERSION) K3D_INSTALL_DIR=$(TOOLS_DIR) USE_SUDO=false bash)
	@{ test -x $(TOOLS_DIR)/kubectl && $(TOOLS_DIR)/kubectl version --client >/dev/null 2>&1; } || \
	  (curl -sSfLo $(TOOLS_DIR)/kubectl "https://dl.k8s.io/release/v$(ENVTEST_K8S_VERSION)/bin/$$($(GO) env GOOS)/$$($(GO) env GOARCH)/kubectl" && chmod +x $(TOOLS_DIR)/kubectl)

# ── fast gate (no Docker) ─────────────────────────────────────────────────────
ci-build:
	$(GO) build ./...
	$(GO) build -o /dev/null ./cmd/genai-otel-bridge
ci-vet:
	$(GO) vet ./...
ci-lint: tools
	$(TOOLS_DIR)/golangci-lint run
ci-lint-acceptance: tools
	$(TOOLS_DIR)/golangci-lint run --build-tags acceptance
ci-test:
	$(GO) test ./...
ci-race:
	$(GO) test -race ./...
ci-acceptance:
	$(GO) test -tags acceptance ./internal/app/
ci-envtest: tools
	bash scripts/envtest.sh
# forbidden-words: hygiene guard — scans the tree for deployment-specific identifiers that must never be
# committed (see scripts/forbidden-words.sh). Self-skips when the script is absent (e.g. a clone without
# it). When present it runs and propagates its exit code (a real hit fails the build).
forbidden-words:
	@if [ -f scripts/forbidden-words.sh ]; then bash scripts/forbidden-words.sh; else echo "forbidden-words: skipped (guard not present in this repo)"; fi
spdx-check:
	bash scripts/spdx-check.sh
helm-lint: tools-e2e
	$(TOOLS_DIR)/helm lint deploy/helm

ci: ci-build ci-vet ci-lint ci-lint-acceptance ci-test ci-race ci-acceptance ci-envtest forbidden-words spdx-check helm-lint

# ── Docker gate (image + k3d e2e) ─────────────────────────────────────────────
image:
	docker buildx build --platform linux/amd64,linux/arm64 \
	  --build-arg VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev) \
	  -t $(IMAGE) -f Dockerfile .
image-local:
	docker build --build-arg VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev) \
	  -t $(IMAGE) -f Dockerfile .
	docker build -t $(E2E_HELPER_IMAGE) -f test/e2e/harness/Dockerfile .
helm-package: tools-e2e
	$(TOOLS_DIR)/helm package deploy/helm -d dist/
k3d-up: tools-e2e
	IMAGE=$(IMAGE) E2E_HELPER_IMAGE=$(E2E_HELPER_IMAGE) K3S_IMAGE=$(K3S_IMAGE) bash scripts/k3d-e2e.sh up
k3d-down: tools-e2e
	bash scripts/k3d-e2e.sh down
k3d-e2e: tools-e2e image-local
	IMAGE=$(IMAGE) E2E_HELPER_IMAGE=$(E2E_HELPER_IMAGE) K3S_IMAGE=$(K3S_IMAGE) bash scripts/k3d-e2e.sh all

ci-e2e: helm-package k3d-e2e

# ── publish (image + chart -> OCI registry) ───────────────────────────────────
# Drives scripts/publish.sh. RELEASE_TAG=vX.Y.Z -> release tags; else :main build.
# Requires REGISTRY + REGISTRY_USER / REGISTRY_PASSWORD in the environment.
publish: tools-e2e
	HELM=$(TOOLS_DIR)/helm bash scripts/publish.sh

# ── release tooling ───────────────────────────────────────────────────────────
# git-cliff: install a pinned binary into .tools/ unless one is already on PATH.
tools-changelog:
	@command -v git-cliff >/dev/null 2>&1 && exit 0; \
	 test -x $(TOOLS_DIR)/git-cliff && exit 0; \
	 mkdir -p $(TOOLS_DIR); \
	 os=$$($(GO) env GOOS); arch=$$($(GO) env GOARCH); \
	 case "$$os/$$arch" in \
	   darwin/arm64) t=aarch64-apple-darwin ;; darwin/amd64) t=x86_64-apple-darwin ;; \
	   linux/amd64)  t=x86_64-unknown-linux-gnu ;; linux/arm64) t=aarch64-unknown-linux-gnu ;; \
	   *) echo "no git-cliff prebuilt for $$os/$$arch — install manually (brew/cargo)" >&2; exit 1 ;; \
	 esac; \
	 v=$(GIT_CLIFF_VERSION); b=$${v#v}; tmp=$$(mktemp -d); \
	 curl -sSfL "https://github.com/orhun/git-cliff/releases/download/$$v/git-cliff-$$b-$$t.tar.gz" | tar -xz -C $$tmp; \
	 install -m0755 "$$(find $$tmp -name git-cliff -type f | head -1)" $(TOOLS_DIR)/git-cliff; rm -rf $$tmp

# Prepend the next release's section to CHANGELOG.md from UNRELEASED conventional commits (post the
# latest vX.Y.Z tag — so only clean, post-1.0.0 history is ever included). Usage: make changelog VERSION=v1.1.0
changelog: tools-changelog
	@test -n "$(VERSION)" || { echo "usage: make changelog VERSION=vX.Y.Z" >&2; exit 1; }
	$(GIT_CLIFF) --unreleased --tag $(VERSION) --prepend CHANGELOG.md
	@echo "CHANGELOG.md: prepended $(VERSION)"
	@v="$(VERSION)"; v="$${v#v}"; \
	  sed -e "s/^appVersion:.*/appVersion: \"$$v\"/" deploy/helm/Chart.yaml > deploy/helm/Chart.yaml.tmp \
	  && mv deploy/helm/Chart.yaml.tmp deploy/helm/Chart.yaml; \
	  echo "Chart.yaml: appVersion -> $$v (app.kubernetes.io/version label)"

# Symlink the repo git hooks into .git/hooks (pre-commit runs the forbidden-words gate on staged files).
install-hooks:
	@mkdir -p .git/hooks
	@for h in scripts/git-hooks/*; do ln -sf "../../$$h" ".git/hooks/$$(basename $$h)"; echo "installed $$(basename $$h)"; done
