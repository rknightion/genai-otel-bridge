GO ?= go
LDFLAGS := -X github.com/rknightion/genai-otel-bridge/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# ── pinned tool versions (override via env; majors are load-bearing) ──────────
GOLANGCI_LINT_VERSION ?= v2.12.2
SETUP_ENVTEST_VERSION ?= release-0.23
ENVTEST_K8S_VERSION   ?= 1.35.0
HELM_VERSION          ?= v3.18.3
K3D_VERSION           ?= v5.9.0
K3S_IMAGE             ?= rancher/k3s:v1.35.1-k3s1
IMAGE                 ?= genai-otel-bridge:dev
E2E_HELPER_IMAGE      ?= genai-otel-bridge-e2e-helper:dev

TOOLS_DIR := $(CURDIR)/.tools
export PATH := $(TOOLS_DIR):$(PATH)

.PHONY: build test vet lint gate generate generate-check \
        tools tools-e2e \
        ci ci-build ci-vet ci-lint ci-lint-acceptance ci-test ci-race ci-acceptance ci-envtest \
        forbidden-words spdx-check helm-lint tf-validate install-hooks gen-dashboard \
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
gate: vet test lint forbidden-words spdx-check tf-validate
	$(GO) build ./...

# ── code generation ───────────────────────────────────────────────────────────
# Regenerate the Helm chart's default `config:` block in deploy/helm/values.yaml from the Go config
# schema (internal/config/config.go). Run after changing any config field/tag/default/doc-comment.
# TestHelmGeneratedConfigUpToDate (in the gate's `test`) fails if this output is not committed.
generate:
	$(GO) run ./internal/config/gen
	$(GO) run ./internal/docs/gen
# generate-check verifies BOTH generated artifacts are up to date without modifying the tree (CI use).
generate-check: generate
	@git diff --exit-code -- deploy/helm/values.yaml docs/telemetry.md || \
	  (echo "generated files are stale — run 'make generate' and commit" && exit 1)
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
# tf-validate: validate + lint + security-scan the ECS Terraform module.
# OpenTofu-first (tofu native), falling back to terraform. tflint (correctness/hygiene) and checkov
# (AWS security posture) each self-skip when not installed — mirroring forbidden-words — so a bare
# `make gate` stays green without the IaC toolchain. CI installs all three (ci.yml hygiene leg).
tf-validate: ## validate + lint + scan the ECS Terraform module (tofu-first; self-skips absent tools)
	@if command -v tofu >/dev/null 2>&1; then TF=tofu; \
	elif command -v terraform >/dev/null 2>&1; then TF=terraform; \
	else echo "tf-validate: no tofu/terraform found, skipping"; exit 0; fi; \
	echo "tf-validate: $$TF fmt + validate"; \
	$$TF -chdir=deploy/ecs/terraform fmt -check -recursive && \
	$$TF -chdir=deploy/ecs/terraform init -backend=false -input=false >/dev/null && \
	$$TF -chdir=deploy/ecs/terraform validate
	@if command -v tflint >/dev/null 2>&1; then \
		echo "tf-validate: tflint"; tflint --chdir=deploy/ecs/terraform; \
	else echo "tf-validate: tflint not found, skipping"; fi
	@if command -v checkov >/dev/null 2>&1; then \
		echo "tf-validate: checkov"; checkov -d deploy/ecs/terraform --framework terraform --quiet --compact; \
	else echo "tf-validate: checkov not found, skipping"; fi
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
# Releases are automated by release-please (.github/workflows/release-please.yml): it maintains a
# release PR from Conventional Commits, and on merge bumps CHANGELOG.md + deploy/helm/Chart.yaml,
# tags vX.Y.Z, creates the GitHub Release, and triggers publish.yml. No local changelog target.

# Symlink the repo git hooks into .git/hooks (pre-commit runs the forbidden-words gate on staged files).
install-hooks:
	@mkdir -p .git/hooks
	@for h in scripts/git-hooks/*; do ln -sf "../../$$h" ".git/hooks/$$(basename $$h)"; echo "installed $$(basename $$h)"; done
