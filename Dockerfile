# syntax=docker/dockerfile:1

# Single Dockerfile for local development and CI. The builder runs the same
# `task generate build` a developer runs, so the image cannot drift from the
# workstation build.

ARG GO_VERSION=1.27.0
ARG TASK_VERSION=v3.46.4

# --- builder ----------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS builder

# protoc itself has no Go distribution; the protoc-gen-* plugins are pinned by
# the `tool` directives in go.mod and built from the module cache.
RUN apk add --no-cache protobuf protobuf-dev git

ARG TASK_VERSION
RUN go install github.com/go-task/task/v3/cmd/task@${TASK_VERSION}

WORKDIR /src

# Dependencies change far less often than source, so cache them separately.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG VERSION=dev
ENV CGO_ENABLED=0
# PROTOC_VERSION_CHECK=0: alpine ships a different protoc than .protoc-version
# pins, but here the stubs are generated and compiled in the same step and
# never committed, so protoc's version stamp (a comment) cannot matter. The
# plugins that decide the generated API are pinned by go.mod either way.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    task generate build VERSION=${VERSION} PROTOC_VERSION_CHECK=0

# --- runtime ----------------------------------------------------------------
# distroless/static: no shell, no package manager, non-root by default.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=builder /src/bin/tagger /usr/local/bin/tagger

USER nonroot:nonroot
EXPOSE 8080 9090

ENV HTTP_ADDR=":8080" \
    GRPC_ADDR=":9090"

# The image has no curl; the binary probes itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/tagger", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/tagger"]
