GO ?= go
# Force module mode for ALL go tooling (build/test/vet/lint/generate). A local, gitignored `vendor/`
# dir would otherwise flip Go into -mod=vendor, which goes stale on every dependency bump (the
# "inconsistent vendoring" error) and diverges from CI — CI has no vendor/ tree so it runs module mode.
# Pinning -mod=readonly makes `make gate` behave EXACTLY like CI and ignore any stale local vendor/.
# Override via env (e.g. GOFLAGS=-mod=mod) if you ever need to.
GOFLAGS ?= -mod=readonly
export GOFLAGS
LDFLAGS := -X github.com/rknightion/genai-otel-bridge/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# ── pinned tool versions (override via env; majors are load-bearing) ──────────
GOLANGCI_LINT_VERSION ?= v2.12.2
SETUP_ENVTEST_VERSION ?= release-0.23
ENVTEST_K8S_VERSION   ?= 1.35.0
HELM_VERSION          ?= v4.2.3
K3D_VERSION           ?= v5.9.0
K3S_IMAGE             ?= rancher/k3s:v1.36.2-k3s1
IMAGE                 ?= genai-otel-bridge:dev
E2E_HELPER_IMAGE      ?= genai-otel-bridge-e2e-helper:dev
GO_LICENSES_VERSION   ?= v2.0.1
SYFT_VERSION          ?= v1.48.0

TOOLS_DIR := $(CURDIR)/.tools
export PATH := $(TOOLS_DIR):$(PATH)

.PHONY: build test coverage vet lint gate generate generate-check \
        tools tools-e2e \
        ci ci-build ci-vet ci-lint ci-lint-acceptance ci-test ci-race ci-acceptance ci-envtest \
        forbidden-words spdx-check helm-lint tf-validate install-hooks gen-dashboard \
        ci-e2e image image-local helm-package k3d-up k3d-down k3d-e2e \
        notices sbom tools-licensing tools-sbom \
        publish

# ── legacy (kept for local muscle memory) ─────────────────────────────────────
build:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/genai-otel-bridge ./cmd/genai-otel-bridge
test:
	$(GO) test ./...
# coverage: profile over the unit-test scope (same packages as `ci-test`; the acceptance/envtest/
# dynamodb integration suites are build-tagged or need services, so they're out of the profile).
# Uploaded to Codacy by the ci.yml `coverage` job. Locally: `go tool cover -html=coverage.out`.
coverage:
	$(GO) test -covermode=atomic -coverprofile=coverage.out ./...
vet:
	$(GO) vet ./...
## lint mirrors CI's two lint legs (ci.yml runs `make ci-lint ci-lint-acceptance`): the plain build AND
## the acceptance-tagged build, so `make gate` catches exactly what CI does — including issues that only
## surface in acceptance-tagged files (and vice-versa).
lint: tools
	$(TOOLS_DIR)/golangci-lint run
	$(TOOLS_DIR)/golangci-lint run --build-tags acceptance
gate: vet test lint forbidden-words spdx-check tf-validate helm-lint
	$(GO) build ./...

# ── code generation ───────────────────────────────────────────────────────────
# Regenerate the config artifacts from the Go config schema (internal/config/config.go):
# deploy/helm/values.yaml (chart default `config:` block) AND deploy/ecs/terraform/config.example.yaml
# (the DynamoDB-backed ECS default config, same schema under the ECS render profile). Run after
# changing any config field/tag/default/doc-comment. Their drift gates (TestHelmGeneratedConfigUpToDate /
# TestECSConfigExampleUpToDate, in the gate's `test`) fail if the output is not committed.
generate:
	$(GO) run ./internal/config/gen
	$(GO) run ./internal/docs/gen
# generate-check verifies ALL generated artifacts are up to date without modifying the tree (CI use).
generate-check: generate
	@git diff --exit-code -- deploy/helm/values.yaml deploy/ecs/terraform/config.example.yaml docs/telemetry.md || \
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
	@# helm/k3d/kubectl are fetched as raw binaries/tarballs (never curl|bash of a script) from the SAME
	@# pinned release as the version var, and sha256-verified against that release's own checksum file
	@# before install — no mutable branch ref is ever executed, and a corrupted/tampered download fails
	@# the build instead of silently installing.
	@{ test -x $(TOOLS_DIR)/helm && $(TOOLS_DIR)/helm version --short >/dev/null 2>&1; } || \
	  ( set -e; \
	    os=$$($(GO) env GOOS); arch=$$($(GO) env GOARCH); \
	    tarball="helm-$(HELM_VERSION)-$$os-$$arch.tar.gz"; \
	    curl -sSfLo "/tmp/$$tarball" "https://get.helm.sh/$$tarball"; \
	    curl -sSfLo "/tmp/$$tarball.sha256sum" "https://get.helm.sh/$$tarball.sha256sum"; \
	    (cd /tmp && sha256sum -c "$$tarball.sha256sum"); \
	    tar -xzf "/tmp/$$tarball" -C /tmp && \
	    mv "/tmp/$$os-$$arch/helm" $(TOOLS_DIR)/helm )
	@{ test -x $(TOOLS_DIR)/k3d && $(TOOLS_DIR)/k3d version >/dev/null 2>&1; } || \
	  ( set -e; \
	    os=$$($(GO) env GOOS); arch=$$($(GO) env GOARCH); \
	    curl -sSfLo /tmp/k3d-checksums.txt "https://github.com/k3d-io/k3d/releases/download/$(K3D_VERSION)/checksums.txt"; \
	    curl -sSfLo /tmp/k3d-bin "https://github.com/k3d-io/k3d/releases/download/$(K3D_VERSION)/k3d-$$os-$$arch"; \
	    want=$$(grep "k3d-$$os-$$arch$$" /tmp/k3d-checksums.txt | awk '{print $$1}'); \
	    test -n "$$want" || { echo "k3d: no checksum entry for $$os-$$arch in checksums.txt"; exit 1; }; \
	    echo "$$want  /tmp/k3d-bin" | sha256sum -c -; \
	    install -m 0755 /tmp/k3d-bin $(TOOLS_DIR)/k3d )
	@{ test -x $(TOOLS_DIR)/kubectl && $(TOOLS_DIR)/kubectl version --client >/dev/null 2>&1; } || \
	  ( set -e; \
	    os=$$($(GO) env GOOS); arch=$$($(GO) env GOARCH); \
	    curl -sSfLo /tmp/kubectl-bin "https://dl.k8s.io/release/v$(ENVTEST_K8S_VERSION)/bin/$$os/$$arch/kubectl"; \
	    curl -sSfLo /tmp/kubectl-bin.sha256 "https://dl.k8s.io/release/v$(ENVTEST_K8S_VERSION)/bin/$$os/$$arch/kubectl.sha256"; \
	    echo "$$(cat /tmp/kubectl-bin.sha256)  /tmp/kubectl-bin" | sha256sum -c -; \
	    install -m 0755 /tmp/kubectl-bin $(TOOLS_DIR)/kubectl )

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

ci: ci-build ci-vet ci-lint ci-lint-acceptance ci-test ci-race ci-acceptance ci-envtest forbidden-words spdx-check helm-lint tf-validate

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

# ── third-party license notices + SBOM (RELEASE ARTIFACTS; not committed/gated) ────
# THIRD_PARTY_NOTICES.md and the SBOMs change on every dependency bump, so they are
# regenerated at release time rather than committed: the image build bakes notices into
# /licenses/, and publish.yml attaches notices + both SBOMs to the GitHub Release. They are
# deliberately NOT in `make gate` — committing+gating a deps-derived file would block hosted-
# Renovate automerge (it can't self-regenerate it). These targets are for manual/local + CI use.
tools-licensing:
	@mkdir -p $(TOOLS_DIR)
	@{ test -x $(TOOLS_DIR)/go-licenses && $(TOOLS_DIR)/go-licenses --help >/dev/null 2>&1; } || \
	  GOBIN=$(TOOLS_DIR) $(GO) install github.com/google/go-licenses@$(GO_LICENSES_VERSION)
tools-sbom:
	@mkdir -p $(TOOLS_DIR)
	@{ test -x $(TOOLS_DIR)/syft && $(TOOLS_DIR)/syft version >/dev/null 2>&1; } || \
	  GOBIN=$(TOOLS_DIR) $(GO) install github.com/anchore/syft/cmd/syft@$(SYFT_VERSION)

# Regenerate THIRD_PARTY_NOTICES.md (LICENSE + NOTICE texts) from the binary's import graph.
notices: tools-licensing
	GO_LICENSES=$(TOOLS_DIR)/go-licenses bash scripts/notices.sh
# Generate SPDX + CycloneDX SBOMs into dist/sbom/. Scans the built binary by default;
# override SBOM_TARGET (e.g. an image ref) to scan something else.
sbom: tools-sbom build
	SYFT=$(TOOLS_DIR)/syft bash scripts/sbom.sh

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
