# syntax=docker/dockerfile:1@sha256:87999aa3d42bdc6bea60565083ee17e86d1f3339802f543c0d03998580f9cb89
# Pin the builder to the BUILD platform and cross-compile (Go, CGO_ENABLED=0,
# GOARCH=$TARGETARCH) — avoids QEMU-emulating the non-native arch build.
FROM --platform=$BUILDPLATFORM mirror.gcr.io/library/golang:1.26-bookworm AS builder
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

FROM gcr.io/distroless/static-debian12:nonroot@sha256:d093aa3e30dbadd3efe1310db061a14da60299baff8450a17fe0ccc514a16639
COPY --from=builder /genai-otel-bridge /genai-otel-bridge
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/genai-otel-bridge"]
