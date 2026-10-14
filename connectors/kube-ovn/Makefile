# The connector for kube-ovn is a Go module of its own, beside the operator. Its
# targets use the tools the Makefile of the root of the repository pins and
# downloads, so that both build with the same versions.

# Image URL to use for building and pushing the image of the connector.
IMG ?= ghcr.io/aenix-io/site-router-kube-ovn-connector:latest
ROOT := $(abspath ../..)
LOCALBIN ?= $(ROOT)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint
CONTAINER_TOOL ?= docker
PLATFORMS ?= linux/arm64,linux/amd64
# ENVTEST_K8S_VERSION is the version of Kubernetes of the client libraries.
ENVTEST_K8S_VERSION ?= $(shell go list -m -f '{{.Version}}' k8s.io/api | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Generate the ClusterRole of the connector from its RBAC markers.
	"$(CONTROLLER_GEN)" rbac:roleName=kube-ovn-connector paths="./..." output:rbac:artifacts:config=deploy/rbac

.PHONY: fmt
fmt: ## Run go fmt.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: tidy
tidy: ## Run go mod tidy.
	go mod tidy

.PHONY: test
test: manifests fmt vet $(ENVTEST) ## Run the unit tests and the tests on a real apiserver, with the race detector.
	KUBEBUILDER_ASSETS="$$("$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test -race ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint with the configuration of the repository.
	"$(GOLANGCI_LINT)" run

.PHONY: build
build: fmt vet ## Build the binary of the connector.
	go build -o bin/kube-ovn-connector ./cmd

.PHONY: docker-build
docker-build: ## Build the image of the connector. The context is the root of the repository.
	$(CONTAINER_TOOL) build $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) -f Dockerfile -t $(IMG) $(ROOT)

.PHONY: docker-buildx
docker-buildx: ## Build the image of the connector for every platform in PLATFORMS and push it.
	$(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) -f Dockerfile -t $(IMG) $(ROOT)

$(CONTROLLER_GEN):
	$(MAKE) -C $(ROOT) controller-gen LOCALBIN="$(LOCALBIN)"

$(ENVTEST):
	$(MAKE) -C $(ROOT) envtest LOCALBIN="$(LOCALBIN)"

$(GOLANGCI_LINT):
	$(MAKE) -C $(ROOT) golangci-lint LOCALBIN="$(LOCALBIN)"
