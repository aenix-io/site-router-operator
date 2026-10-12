# Override BASE_IMAGE to build from another registry, e.g. docker.io/library/golang:1.27,
# and RUNTIME_IMAGE to take the base of the image from a mirror.
ARG BASE_IMAGE=golang:1.27
ARG RUNTIME_IMAGE=gcr.io/distroless/static:nonroot

# The builder runs on the platform of the machine that builds and cross-compiles
# the manager for the platform of the image, so that an image for another
# architecture is built without emulating it: nothing is run in the stage of the
# target platform.
FROM --platform=$BUILDPLATFORM ${BASE_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# The module files come first, so that the download is a layer that a change of
# the source does not invalidate. The api module is a replace target of the root
# module, so its go.mod has to be present before the download. The engines the
# operator is built with are modules like any other and are downloaded here.
COPY go.mod go.sum ./
COPY api/go.mod api/go.sum api/
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# The whole package is built, not main.go alone: cmd/engines.go is where the
# engines are imported. TARGETOS and TARGETARCH are set by BuildKit; left empty,
# GOARCH builds for the platform of the builder.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/manager ./cmd

# distroless/static has no shell and no package manager, and nothing in it is
# written to at run time, so the root filesystem can be mounted read-only.
FROM ${RUNTIME_IMAGE}
LABEL org.opencontainers.image.source="https://github.com/aenix-io/site-router-operator" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=builder /out/manager /manager
# A numeric user: the kubelet can only check runAsNonRoot against a number.
USER 65532:65532

ENTRYPOINT ["/manager"]
