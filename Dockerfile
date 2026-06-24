# syntax=docker/dockerfile:1
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
      -ldflags="-s -w -X github.com/grafana-ps/aip-oi/internal/version.Version=${VERSION}" \
      -o /aip-oi ./cmd/aip-oi

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /aip-oi /aip-oi
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/aip-oi"]
