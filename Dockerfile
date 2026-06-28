# syntax=docker/dockerfile:1
# Pin the builder to the BUILD platform and cross-compile (Go, CGO_ENABLED=0,
# GOARCH=$TARGETARCH) — avoids QEMU-emulating the non-native arch build.
FROM --platform=$BUILDPLATFORM mirror.gcr.io/library/golang:1.26-bookworm@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b AS builder
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

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /genai-otel-bridge /genai-otel-bridge
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/genai-otel-bridge"]
