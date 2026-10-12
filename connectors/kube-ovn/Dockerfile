# Build the connector for kube-ovn. The context is the root of the repository:
# the module replaces the api module with ../../api, so both have to be in it.
# Dockerfile.dockerignore, next to this file, says what of the context is sent.
#
#   docker build -f connectors/kube-ovn/Dockerfile .
#
# Override BASE_IMAGE to build from another registry, e.g. docker.io/library/golang:1.27
ARG BASE_IMAGE=golang:1.27
# The builder runs on the platform of the machine and cross-compiles for the one
# asked for, so a multi-platform build does not emulate the compiler.
FROM --platform=${BUILDPLATFORM} ${BASE_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# The module manifests first, so that the dependencies are downloaded in a layer
# of their own that a change of the source does not invalidate.
COPY api/go.* api/
COPY connectors/kube-ovn/go.mod connectors/kube-ovn/go.sum connectors/kube-ovn/
RUN cd connectors/kube-ovn && go mod download

COPY api/ api/
COPY connectors/kube-ovn/ connectors/kube-ovn/
RUN cd connectors/kube-ovn && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -o /kube-ovn-connector ./cmd

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /kube-ovn-connector /kube-ovn-connector
USER 65532:65532

ENTRYPOINT ["/kube-ovn-connector"]
