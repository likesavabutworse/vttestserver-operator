# Image URL to use for the image targets. DATACTL_IMG defaults to the same name with a -datactl suffix.
IMG ?= vttestserver-operator:latest
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# CONTAINER_TOOL defines the container tool to be used for building images.
# The targets are only tested with Docker.
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized beneath their categories.
# The categories are represented by '##@' and the target descriptions by '##'.
.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate the CRDs (config/crd/bases) and the ClusterRole (config/rbac/role.yaml) from the code markers.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run the tests (unit tests and envtest).
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test ./...

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

.PHONY: helm-check
helm-check: ## Lint the Helm chart and check that its CRDs and RBAC rules match the generated ones.
	helm lint dist/chart
	hack/check-chart.sh

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build the operator and datactl binaries.
	go build -o bin/manager ./cmd
	go build -o bin/datactl ./cmd/datactl

.PHONY: run
run: manifests generate fmt vet ## Run the operator from your host, against the current kubeconfig context.
	go run ./cmd

# Release images come from the two targets of the Dockerfile: manager and datactl.
# Override BASE_IMAGE to build from another registry, e.g.
# make docker-build IMG=<img> BASE_IMAGE=docker.io/library/golang:1.26
DATACTL_IMG ?= ${IMG}-datactl

.PHONY: docker-build
docker-build: ## Build the manager image (IMG) and the datactl image (DATACTL_IMG) for this machine.
	$(CONTAINER_TOOL) build --target manager $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) -t ${IMG} .
	$(CONTAINER_TOOL) build --target datactl $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) -t ${DATACTL_IMG} .

.PHONY: docker-push
docker-push: ## Push the manager and datactl images.
	$(CONTAINER_TOOL) push ${IMG}
	$(CONTAINER_TOOL) push ${DATACTL_IMG}

# Multi-platform release images. The Dockerfile cross compiles on the build host, so no emulation is needed.
# make docker-buildx IMG=<registry>/vttestserver-operator:<tag> DATACTL_IMG=<registry>/vttestserver-operator-datactl:<tag>
# To use these targets you need docker buildx and permission to push to the registry.
PLATFORMS ?= linux/amd64,linux/arm64
BUILDX_BUILDER ?= vttestserver-operator-builder

.PHONY: docker-buildx
docker-buildx: docker-buildx-manager docker-buildx-datactl ## Build and push the manager and datactl images for all PLATFORMS.

.PHONY: docker-buildx-manager
docker-buildx-manager: ## Build and push the manager image for all PLATFORMS.
	$(call buildx,manager,${IMG})

.PHONY: docker-buildx-datactl
docker-buildx-datactl: ## Build and push the datactl image for all PLATFORMS.
	$(call buildx,datactl,${DATACTL_IMG})

define buildx
	- $(CONTAINER_TOOL) buildx create --name $(BUILDX_BUILDER)
	$(CONTAINER_TOOL) buildx build --builder $(BUILDX_BUILDER) --push --target $(1) --platform=$(PLATFORMS) $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) --tag $(2) .
endef

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
CONTROLLER_TOOLS_VERSION ?= v0.22.0
GOLANGCI_LINT_VERSION ?= v2.14.0

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
