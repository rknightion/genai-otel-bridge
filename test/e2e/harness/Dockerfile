# syntax=docker/dockerfile:1
FROM golang:1.26-bookworm AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /helper ./test/e2e/harness

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /helper /helper
USER 65532:65532
ENTRYPOINT ["/helper"]
