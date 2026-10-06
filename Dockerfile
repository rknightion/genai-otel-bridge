# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
# Pin the builder to the BUILD platform and cross-compile (Go, CGO_ENABLED=0,
# GOARCH=$TARGETARCH) — avoids QEMU-emulating the non-native arch build.
FROM --platform=$BUILDPLATFORM mirror.gcr.io/library/golang:1.27.1-bookworm@sha256:8d48e12ec56735e9358640898b9d9b9fcca110612ed8a5567438c0a1baa24e66 AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags="-s -w -X github.com/rknightion/genai-otel-bridge/internal/version.Version=${VERSION}" \
      -o /genai-otel-bridge ./cmd/genai-otel-bridge

# Third-party notices (LICENSE + NOTICE texts of the linked modules) baked into /licenses/
# below. Runs once on the BUILDPLATFORM (not per target arch); pinned go-licenses. See
# scripts/notices.sh. Network is already required by `go mod download` above, so no new constraint.
ARG GO_LICENSES_VERSION=v1.6.0
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go install github.com/google/go-licenses@${GO_LICENSES_VERSION} && \
    GO_LICENSES=go-licenses OUT=/build/THIRD_PARTY_NOTICES.md bash scripts/notices.sh

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=builder /genai-otel-bridge /genai-otel-bridge
# License compliance travels with the image (OCI /licenses convention): Apache-2.0 text + third-party notices.
COPY --from=builder /build/LICENSE /licenses/LICENSE
COPY --from=builder /build/THIRD_PARTY_NOTICES.md /licenses/THIRD_PARTY_NOTICES.md
LABEL org.opencontainers.image.licenses="Apache-2.0"
USER 65532:65532
EXPOSE 8080
# Exec form (no shell in distroless): reuses the binary's own shell-free -healthcheck probe mode
# (cmd/genai-otel-bridge/main.go) — gives plain `docker run`/compose/podman/EC2-Docker deployments a
# health signal. Kubernetes ignores image HEALTHCHECK (the Helm chart's liveness/readiness probes
# cover that path) and the ECS Terraform module sets its own task-def healthCheck, so this is purely
# for non-orchestrated docker/podman use.
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 CMD ["/genai-otel-bridge", "-healthcheck"]
ENTRYPOINT ["/genai-otel-bridge"]
