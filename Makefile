# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# IMAGE_NAME is the repository the Nix flake gives the image it builds: both
# `docker-build` and `docker-build-e2e-old` load that name and re-tag it.
IMAGE_NAME ?= ghcr.io/ushitora-anqou/koujouo
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# VERSION is baked into the manager and the agent binaries through LDFLAGS.
# It is the short git revision, suffixed with -dirty over uncommitted changes:
# the same value the Nix flake bakes into the image, so a locally built
# manager recognizes the agents of an image built from the same tree as
# agents of its own version.
#
# A dirty tree adds the build time: building twice from the same commit with
# different uncommitted changes would otherwise stamp both builds with the
# same version, and the manager would take the new agent for a build of its
# own version and skip the in-place upgrade. Releases are built from a clean
# commit, so only development builds carry the suffix.
VERSION ?= $(shell v=$$(git describe --always --dirty --exclude='*' 2>/dev/null || echo dev); \
  if [ "$${v%-dirty}" != "$$v" ]; then echo "$$v-$$(date +%s)"; else echo "$$v"; fi)
LDFLAGS ?= -X github.com/ushitora-anqou/koujouo/internal/version.Agent=$(VERSION)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# `all` is the gate a commit has to pass, and the default goal: it regenerates
# the code and the chart, verifies the linter configuration, lints, and runs
# both test layers. `build` is not part of it on purpose: `test` compiles the
# manager and the agent, and `test-e2e` builds the image and runs it.
.PHONY: all
all: fmt generate manifests lint-config lint test test-e2e ## Run everything a commit has to pass: regenerate, lint and test (e2e included).

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

# The chart holds the generated manifests: the CRD goes straight into its
# templates, and the manager ClusterRole is rewritten so that Helm names it
# after the release. The intermediate RBAC output stays under bin/, which git
# and the flake ignore.
.PHONY: manifests
manifests: ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects into the Helm chart.
	rm -rf bin/rbac
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." \
		output:crd:artifacts:config=charts/koujouo/templates \
		output:rbac:artifacts:config=bin/rbac
	"$(YQ)" -i '.metadata.annotations."helm.sh/resource-policy" = "keep"' charts/koujouo/templates/koujouo.anqou.net_factorioservers.yaml
	"$(YQ)" '.metadata.name = "{{ include \"koujouo.fullname\" . }}-manager-role"' bin/rbac/role.yaml > charts/koujouo/templates/manager-role.yaml
	rm -rf bin/rbac

.PHONY: generate-capnp
generate-capnp: ## Generate Go code from rpc/koujouo.capnp.
	go generate ./rpc

.PHONY: generate
generate: generate-capnp ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

# The scan reads the git history, not the working tree: an untracked file (a
# developer's .envrc, an ignored scratch manifest) is not part of the
# repository, and scanning it would fail `make test` for that developer only.
# `--redact` keeps a found secret out of the log.
.PHONY: secrets
secrets: ## Scan the git history for committed secrets.
	"$(GITLEAKS)" git . --no-banner --redact

.PHONY: test
test: secrets manifests generate fmt ## Run the secret scan and the tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# The suite deploys the operator by installing charts/koujouo with
# `helm upgrade --install` and removes it with `helm uninstall`; there is no
# `make install` / `make deploy` any more.
# kubectl kuberc is disabled by default for test isolation; enable with:
# - KUBECTL_KUBERC=true
# CertManager is not needed: the project has no webhook, so test-e2e skips its installation.
# DISCORD_WEBHOOK_URL, when set, is handed to the FactorioServer of the suite
# (as a Secret); the suite runs without Discord notifications otherwise.
KIND_CLUSTER ?= koujouo-test-e2e
# The suite builds the image with Nix, installs the chart with Helm and lets
# the agent download the Factorio headless build, which takes longer than the
# ten minutes go test allows by default. The agent upgrade scenario adds the
# build and load of the image of a previous build, the rollout of the manager
# Deployment and the upgrade itself, so the budget is 60 minutes (the CI job
# has timeout-minutes: 90).
E2E_TIMEOUT ?= 60m

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

# The e2e suite installs the chart with Helm, so the target depends on
# `manifests generate fmt`: the CRD and the manager ClusterRole of the chart
# must match the markers of the working tree the suite tests.
.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) CERT_MANAGER_INSTALL_SKIP=true \
		go test -tags=e2e ./test/e2e/ -v -ginkgo.v -timeout $(E2E_TIMEOUT)
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: ## Run golangci-lint on the Go code and helm lint on the chart.
	"$(GOLANGCI_LINT)" run
	"$(HELM)" lint charts/koujouo

.PHONY: lint-fix
lint-fix: ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

# The target name carries a single "m" (`check-uncomitted`): keep that
# spelling, `check-uncommitted` does not exist. `ci.yml` and the docs use it.
#
# It regenerates the code and the chart and then fails on any change in the
# working tree: a generated file whose change was not committed, an untracked
# file, and an unrelated uncommitted change all fail it. That is intended: run
# it on a tree you are ready to commit.
.PHONY: check-uncomitted
check-uncomitted: fmt generate manifests ## Check that the regeneration leaves no uncommitted changes.
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "Uncommitted changes after regeneration:"; \
		git status --short; \
		exit 1; \
	fi

##@ Build

.PHONY: build
build: manifests generate fmt ## Build manager binary.
	go build -ldflags "$(LDFLAGS)" -o bin/manager ./cmd/manager

.PHONY: run
run: manifests generate fmt ## Run a controller from your host.
	go run -ldflags "$(LDFLAGS)" ./cmd/manager --config examples/manager.yaml

# The image is built by the Nix flake (see .#dockerImage in flake.nix) and
# loaded into the local container runtime, then re-tagged with ${IMG}.
# go2nix resolves the module hashes from go.sum with a Nix plugin, which
# `--option plugin-files` loads here: inside the dev shell the shellHook
# already puts the plugin into NIX_CONFIG, and this option makes the target
# work outside of it too.
# Note: the flake source only contains git-tracked files, so commit (or at
# least `git add`) your changes before building the image.
.PHONY: docker-build
docker-build: ## Build docker image with the manager via Nix.
	nix build --option plugin-files "$$(nix build --no-link --print-out-paths .#go2nix-nix-plugin)/lib/nix/plugins/libgo2nix_plugin.so" -o result .#dockerImage
	$(CONTAINER_TOOL) load --input ./result
	$(CONTAINER_TOOL) tag $(IMAGE_NAME):latest ${IMG}

# The e2e suite needs the image of a previous build: the same source with the
# version "e2e-old" stamped into the manager and the agent instead of the git
# revision. The suite installs that image, upgrades the release to the image
# of this build and checks that the new manager replaces the agent binary of
# the running game. `make all` does not build it: only `test-e2e` needs it.
# `result-e2e-old` is ignored by git through the `result-*` rule of .gitignore.
.PHONY: docker-build-e2e-old
docker-build-e2e-old: ## Build the docker image of a previous build for the e2e agent upgrade.
	nix build --option plugin-files "$$(nix build --no-link --print-out-paths .#go2nix-nix-plugin)/lib/nix/plugins/libgo2nix_plugin.so" -o result-e2e-old .#dockerImageE2EOld
	$(CONTAINER_TOOL) load --input ./result-e2e-old
	$(CONTAINER_TOOL) tag $(IMAGE_NAME):latest ${IMG}

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

##@ Dependencies

## Tool Binaries
##
## All tools below are expected to be provided by the Nix flake dev shell.
## Enter it with:
##
##     nix develop
KUBECTL ?= kubectl
KIND ?= kind
HELM ?= helm
YQ ?= yq
CONTROLLER_GEN ?= controller-gen
ENVTEST ?= setup-envtest
GOLANGCI_LINT ?= golangci-lint
GITLEAKS ?= gitleaks

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
