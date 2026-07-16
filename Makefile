# Image URL to use all building/pushing image targets
IMG ?= ghcr.io/defilantech/llmkube-controller:0.9.7 # x-release-please-version

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

.PHONY: all
all: build

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

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
# crd:allowDangerousTypes=true is required for HardwareSpec.MemoryFraction (*float64).
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd:allowDangerousTypes=true webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: chart-crds
chart-crds: manifests ## Generate CRDs and sync to Helm chart templates
	@./scripts/sync-crds.sh

.PHONY: foreman-chart-crds
foreman-chart-crds: manifests ## Sync foreman.llmkube.dev CRDs to the foreman chart.
	@mkdir -p charts/foreman/templates/crds
	@synced=0; for src in config/crd/bases/foreman.llmkube.dev_*.yaml; do \
	  [ -e "$$src" ] || { echo "no foreman CRDs in config/crd/bases (did make manifests run?)"; exit 1; }; \
	  base=$$(basename $$src); short=$${base#foreman.llmkube.dev_}; \
	  echo "Syncing $$base -> $$short"; \
	  echo '{{- if .Values.crds.install }}' > charts/foreman/templates/crds/$$short; \
	  cat $$src >> charts/foreman/templates/crds/$$short; \
	  echo '' >> charts/foreman/templates/crds/$$short; \
	  echo '{{- end }}' >> charts/foreman/templates/crds/$$short; \
	  synced=$$((synced+1)); \
	done; echo "Synced $$synced foreman CRD(s)"

.PHONY: check-helm-rbac
check-helm-rbac: manifests ## Verify the Helm charts' RBAC covers every kubebuilder-generated rule (#379).
	@./scripts/check-helm-rbac.sh

.PHONY: sync-reviewer-prompts
sync-reviewer-prompts: ## Sync reviewer.md into spec.systemPrompt of every reviewer Agent manifest (#804).
	@go run ./cmd/sync-reviewer-prompts

.PHONY: check-reviewer-prompts
check-reviewer-prompts: ## Drift-check: fail if any reviewer Agent's systemPrompt diverges from reviewer.md (#804).
	@go run ./cmd/sync-reviewer-prompts --check

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= llmkube-test-e2e

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

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-all
lint-all: golangci-lint ## Lint under all supported GOOS settings (slower; run before pushing).
	@echo "==> Lint GOOS=darwin"
	GOOS=darwin $(GOLANGCI_LINT) run ./...
	@echo "==> Lint GOOS=linux"
	GOOS=linux $(GOLANGCI_LINT) run ./...

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

.PHONY: validate-samples
validate-samples: ## Validate config/samples against CRD schemas (catch hallucinated manifests).
	@command -v python3 >/dev/null 2>&1 || { echo "ERROR: python3 is required for validate-samples"; exit 1; }
	@python3 -c "import jsonschema" 2>/dev/null || { echo "ERROR: python3 jsonschema package required (pip3 install jsonschema pyyaml)"; exit 1; }
	@python3 scripts/validate-samples.py

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: build-cli
build-cli: fmt vet ## Build llmkube CLI binary.
	go build -o bin/llmkube ./cmd/cli

.PHONY: install-cli
install-cli: build-cli ## Install llmkube CLI to /usr/local/bin (requires sudo).
	sudo cp bin/llmkube /usr/local/bin/llmkube

##@ Metal Agent (macOS only)

# METAL_AGENT_VERSION is stamped into the ldflags Version variable. Defaults to
# the git-describe tag (matching build-foreman-agent-versioned); override on the
# command line when releasing, e.g. `make build-metal-agent METAL_AGENT_VERSION=0.8.6`.
METAL_AGENT_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

.PHONY: build-metal-agent
build-metal-agent: fmt vet ## Build Metal agent for macOS (requires macOS).
	@echo "Building Metal agent for macOS ($(METAL_AGENT_VERSION))..."
	GOOS=darwin GOARCH=$(shell uname -m | sed 's/x86_64/amd64/;s/arm64/arm64/') \
		go build \
		-ldflags "-X main.Version=$(METAL_AGENT_VERSION) -X main.GitCommit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) -X main.BuildDate=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)" \
		-o bin/llmkube-metal-agent ./cmd/metal-agent

# LLMKUBE_METAL_AGENT_LABEL is the launchd service label for metal-agent.
LLMKUBE_METAL_AGENT_LABEL ?= com.llmkube.metal-agent

.PHONY: install-metal-agent
install-metal-agent: build-metal-agent ## Install Metal agent and launchd service (macOS only).
	@echo "Installing Metal agent..."
	sudo cp bin/llmkube-metal-agent /usr/local/bin/llmkube-metal-agent
	@echo "Installing launchd service..."
	mkdir -p ~/Library/LaunchAgents
	cp deployment/macos/com.llmkube.metal-agent.plist ~/Library/LaunchAgents/
	@echo "Starting Metal agent service..."
	@# Bootstrap the plist the first time (noop if already bootstrapped).
	@launchctl bootstrap gui/$$(id -u) ~/Library/LaunchAgents/$(LLMKUBE_METAL_AGENT_LABEL).plist 2>/dev/null || true
	@# kickstart -k stops the running instance (if any) and starts a fresh one,
	@# so reinstalls always exec the new binary. Unlike `launchctl load || true`
	@# this actually restarts a running service.
	launchctl kickstart -k gui/$$(id -u)/$(LLMKUBE_METAL_AGENT_LABEL)
	@echo "Metal agent installed and restarted"
	@echo ""
	@echo "To check status: launchctl list | grep llmkube"
	@echo "To view logs: tail -f /tmp/llmkube-metal-agent.log"

.PHONY: uninstall-metal-agent
uninstall-metal-agent: ## Uninstall Metal agent and launchd service.
	@echo "Stopping Metal agent service..."
	@launchctl bootout gui/$$(id -u)/$(LLMKUBE_METAL_AGENT_LABEL) 2>/dev/null || true
	@echo "Removing Metal agent..."
	sudo rm -f /usr/local/bin/llmkube-metal-agent
	rm -f ~/Library/LaunchAgents/$(LLMKUBE_METAL_AGENT_LABEL).plist
	@echo "Metal agent uninstalled"

##@ Foreman Agent (macOS install)

# LLMKUBE_FOREMAN_AGENT_LABEL is the launchd service label for foreman-agent.
LLMKUBE_FOREMAN_AGENT_LABEL ?= com.llmkube.foreman-agent

# FOREMAN_AGENT_VERSION is stamped into the versioned install layout and the
# ldflags Version variable. Override on the command line when releasing.
FOREMAN_AGENT_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

.PHONY: build-foreman-agent-versioned
build-foreman-agent-versioned: fmt vet ## Build foreman-agent with ldflags version info.
	go build \
		-ldflags "-X main.Version=$(FOREMAN_AGENT_VERSION) -X main.GitCommit=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown) -X main.BuildDate=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)" \
		-o bin/foreman-agent \
		./cmd/foreman-agent

# FOREMAN_INSTALL_ROOT is the user-owned managed layout root for foreman-agent.
# On macOS the default matches ResolveInstallRoot("foreman-agent"). Do NOT
# backslash-escape the space: this value is always consumed inside double quotes
# (recipe commands) or a sed replacement, where a literal backslash would be
# kept verbatim and corrupt the path.
FOREMAN_INSTALL_ROOT ?= $(HOME)/Library/Application Support/llmkube/foreman-agent

# foreman-agent runtime configuration rendered into the launchd plist by
# install-foreman-agent. Override on the command line, e.g.:
#   make install-foreman-agent \
#     FOREMAN_INSTALLED_MODELS=qwen36-35b-carnice-mtp \
#     FOREMAN_NODE_NAME=m5max-coder \
#     FOREMAN_KUBECONFIG=$(HOME)/.kube/shadowstack.yaml \
#     FOREMAN_ACCELERATOR=metal
FOREMAN_NODE_NAME        ?= $(shell hostname -s)
FOREMAN_KUBECONFIG       ?= $(HOME)/.kube/config
FOREMAN_ACCELERATOR      ?= metal
FOREMAN_GIT_REMOTE_URL   ?= $(shell git remote get-url origin 2>/dev/null)
FOREMAN_COMMIT_EMAIL     ?= $(shell git config user.email 2>/dev/null)
# Required: comma-separated model names this node serves (used to match tasks).
FOREMAN_INSTALLED_MODELS ?=

.PHONY: check-foreman-install-vars
check-foreman-install-vars:
	@if [ -z "$(FOREMAN_INSTALLED_MODELS)" ]; then \
		echo "ERROR: FOREMAN_INSTALLED_MODELS is required (comma-separated model names this node serves)."; \
		echo "       e.g. make install-foreman-agent FOREMAN_INSTALLED_MODELS=qwen36-35b-carnice-mtp"; \
		exit 1; \
	fi
	@if [ -z "$(FOREMAN_NODE_NAME)" ]; then \
		echo "ERROR: FOREMAN_NODE_NAME is empty (hostname -s returned nothing); set it explicitly."; \
		exit 1; \
	fi

.PHONY: install-foreman-agent
install-foreman-agent: check-foreman-install-vars build-foreman-agent-versioned ## Install foreman-agent into user-owned managed layout and start launchd service (macOS only, no sudo).
	@echo "Installing foreman-agent $(FOREMAN_AGENT_VERSION)..."
	@# Create the versioned directory and stage the binary.
	mkdir -p "$(FOREMAN_INSTALL_ROOT)/versions/$(FOREMAN_AGENT_VERSION)"
	cp bin/foreman-agent "$(FOREMAN_INSTALL_ROOT)/versions/$(FOREMAN_AGENT_VERSION)/foreman-agent"
	chmod 0755 "$(FOREMAN_INSTALL_ROOT)/versions/$(FOREMAN_AGENT_VERSION)/foreman-agent"
	@# Flip the current symlink. `ln -sfn` replaces the link in place; -n keeps
	@# it from following an existing current -> versions/<v> symlink. The old
	@# "ln to current.tmp then mv -f current.tmp current" form was broken on
	@# re-install: when current already points at a directory, mv descends INTO
	@# that directory instead of replacing the link, so the flip silently no-ops
	@# and the agent keeps running the previous version. (The self-update code
	@# path uses os.Rename, which replaces the symlink atomically and is correct.)
	@ln -sfn "$(FOREMAN_INSTALL_ROOT)/versions/$(FOREMAN_AGENT_VERSION)" \
		"$(FOREMAN_INSTALL_ROOT)/current"
	@echo "Staged at $(FOREMAN_INSTALL_ROOT)/current -> versions/$(FOREMAN_AGENT_VERSION)"
	@echo "Installing launchd service..."
	@# Render the launchd plist: substitute the install-root path AND every
	@# REPLACE_WITH_* placeholder, so the installed service has a complete,
	@# working command line with no leftover placeholders.
	@sed \
		-e "s|/Users/YOUR_USERNAME/Library/Application Support/llmkube/foreman-agent|$(FOREMAN_INSTALL_ROOT)|g" \
		-e "s|REPLACE_WITH_NODE_NAME|$(FOREMAN_NODE_NAME)|g" \
		-e "s|REPLACE_WITH_ACCELERATOR|$(FOREMAN_ACCELERATOR)|g" \
		-e "s|REPLACE_WITH_INSTALLED_MODELS|$(FOREMAN_INSTALLED_MODELS)|g" \
		-e "s|REPLACE_WITH_KUBECONFIG|$(FOREMAN_KUBECONFIG)|g" \
		-e "s|REPLACE_WITH_GIT_REMOTE_URL|$(FOREMAN_GIT_REMOTE_URL)|g" \
		-e "s|REPLACE_WITH_EMAIL|$(FOREMAN_COMMIT_EMAIL)|g" \
		deployment/macos/com.llmkube.foreman-agent.plist \
		> ~/Library/LaunchAgents/$(LLMKUBE_FOREMAN_AGENT_LABEL).plist
	@echo "Starting foreman-agent service..."
	@# Bootstrap the plist the first time (noop if already bootstrapped).
	@launchctl bootstrap gui/$$(id -u) ~/Library/LaunchAgents/$(LLMKUBE_FOREMAN_AGENT_LABEL).plist 2>/dev/null || true
	@# kickstart -k ensures a running instance picks up the new binary.
	launchctl kickstart -k gui/$$(id -u)/$(LLMKUBE_FOREMAN_AGENT_LABEL)
	@echo "foreman-agent installed and restarted"
	@echo ""
	@echo "Edit ~/Library/LaunchAgents/$(LLMKUBE_FOREMAN_AGENT_LABEL).plist to set flags, then run:"
	@echo "  launchctl kickstart -k gui/$$(id -u)/$(LLMKUBE_FOREMAN_AGENT_LABEL)"
	@echo "To view logs: tail -f /tmp/llmkube-foreman-agent.log"

.PHONY: uninstall-foreman-agent
uninstall-foreman-agent: ## Uninstall foreman-agent launchd service (leaves install root intact for rollback).
	@echo "Stopping foreman-agent service..."
	@launchctl bootout gui/$$(id -u)/$(LLMKUBE_FOREMAN_AGENT_LABEL) 2>/dev/null || true
	rm -f ~/Library/LaunchAgents/$(LLMKUBE_FOREMAN_AGENT_LABEL).plist
	@echo "foreman-agent service uninstalled (install root preserved at $(FOREMAN_INSTALL_ROOT))"

.PHONY: install-powermetrics-sudo
install-powermetrics-sudo: ## Install NOPASSWD sudoers entry for /usr/bin/powermetrics (required for --apple-power-enabled).
	@echo "Installing sudoers entry for the Apple power sampler..."
	@echo "This grants the current user ($(shell whoami)) NOPASSWD access to"
	@echo "/usr/bin/powermetrics with the EXACT argv the agent uses, and nothing else."
	@echo ""
	@TMP=$$(mktemp); \
		sed "s/__LLMKUBE_USER__/$(shell whoami)/" deployment/macos/sudoers.d/llmkube-powermetrics > "$$TMP"; \
		sudo visudo -cf "$$TMP" || (echo "❌ sudoers syntax check failed; aborting"; rm -f "$$TMP"; exit 1); \
		sudo install -m 0440 -o root -g wheel "$$TMP" /etc/sudoers.d/llmkube-powermetrics; \
		rm -f "$$TMP"
	@echo ""
	@echo "✅ Sudoers entry installed. Granted command:"
	@sudo -ln 2>/dev/null | grep powermetrics || echo "  (run 'sudo -ln | grep powermetrics' to verify)"
	@echo ""
	@echo "Now restart the Metal agent with --apple-power-enabled to publish the gauges."

.PHONY: uninstall-powermetrics-sudo
uninstall-powermetrics-sudo: ## Remove the NOPASSWD sudoers entry installed by install-powermetrics-sudo.
	@echo "Removing /etc/sudoers.d/llmkube-powermetrics..."
	sudo rm -f /etc/sudoers.d/llmkube-powermetrics
	@echo "✅ Sudoers entry removed. Apple power gauges will now read 0 if the agent is run with --apple-power-enabled."

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go $(ARGS)

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

ROUTER_PROXY_IMG ?= ghcr.io/defilantech/llmkube-router-proxy:dev
.PHONY: docker-build-router-proxy
docker-build-router-proxy: ## Build docker image for the router-proxy data plane.
	$(CONTAINER_TOOL) build -f Dockerfile.router-proxy -t ${ROUTER_PROXY_IMG} .

.PHONY: docker-push-router-proxy
docker-push-router-proxy: ## Push the router-proxy image.
	$(CONTAINER_TOOL) push ${ROUTER_PROXY_IMG}

.PHONY: build-router-proxy
build-router-proxy: fmt vet ## Build router-proxy binary into bin/.
	go build -o bin/router-proxy ./cmd/router-proxy

# Foreman operator + agent: built per-arch via goreleaser at release
# time; these targets exist for local development against a kind
# cluster and for the helm-chart CI's smoke install.
FOREMAN_OPERATOR_IMG     ?= ghcr.io/defilantech/llmkube-foreman-operator:dev
FOREMAN_AGENT_IMG        ?= ghcr.io/defilantech/llmkube-foreman-agent:dev
# Coder-Job builder image (#620) and in-cluster foreman-agent toolchain
# image (#797): foreman-agent binary + go/make/git/golangci-lint/
# controller-gen on the gate's golang:1.26 base, so the in-Job coder
# and the in-cluster foreman-agent fast gate can run `make fmt vet lint
# test`. Referenced by Agent.spec.execution.image.
FOREMAN_AGENT_BUILDER_IMG ?= ghcr.io/defilantech/llmkube-foreman-agent-builder:dev

.PHONY: docker-build-foreman-operator
docker-build-foreman-operator: ## Build docker image for the foreman-operator.
	$(CONTAINER_TOOL) build -f Dockerfile.foreman-operator -t ${FOREMAN_OPERATOR_IMG} .

.PHONY: docker-build-foreman-agent
docker-build-foreman-agent: ## Build docker image for the foreman-agent.
	$(CONTAINER_TOOL) build -f Dockerfile.foreman-agent -t ${FOREMAN_AGENT_IMG} .

.PHONY: docker-build-foreman-agent-builder
docker-build-foreman-agent-builder: ## Build the coder-Job builder image (foreman-agent + go/make/git/golangci-lint/controller-gen).
	$(CONTAINER_TOOL) build -f Dockerfile.foreman-agent-builder -t ${FOREMAN_AGENT_BUILDER_IMG} .

.PHONY: docker-push-foreman-operator
docker-push-foreman-operator: ## Push the foreman-operator image.
	$(CONTAINER_TOOL) push ${FOREMAN_OPERATOR_IMG}

.PHONY: docker-push-foreman-agent
docker-push-foreman-agent: ## Push the foreman-agent image.
	$(CONTAINER_TOOL) push ${FOREMAN_AGENT_IMG}

.PHONY: docker-push-foreman-agent-builder
docker-push-foreman-agent-builder: ## Push the coder-Job builder image.
	$(CONTAINER_TOOL) push ${FOREMAN_AGENT_BUILDER_IMG}

.PHONY: build-foreman-operator
build-foreman-operator: fmt vet ## Build foreman-operator binary into bin/.
	go build -o bin/foreman-operator ./cmd/foreman-operator

.PHONY: build-foreman-agent
build-foreman-agent: fmt vet ## Build foreman-agent binary into bin/.
	go build -o bin/foreman-agent ./cmd/foreman-agent

# Stub upstream used by the ModelRouter cluster e2e tests to play both
# the "local InferenceService" and "cloud provider" roles. Image is only
# built into the kind cluster during the e2e job; not published.
STUB_UPSTREAM_IMG ?= localhost/llmkube-stub-upstream:e2e
.PHONY: docker-build-stub-upstream
docker-build-stub-upstream: ## Build docker image for the e2e stub upstream.
	$(CONTAINER_TOOL) build -f test/e2e/stubupstream/Dockerfile -t ${STUB_UPSTREAM_IMG} .

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name llmkube-builder
	$(CONTAINER_TOOL) buildx use llmkube-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm llmkube-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( $(KUSTOMIZE) build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | $(KUBECTL) apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( $(KUSTOMIZE) build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.7.1
CONTROLLER_TOOLS_VERSION ?= v0.19.0
#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v2.12.2

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
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
rm -f $(1) ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $$(realpath $(1)-$(3)) $(1)
endef
