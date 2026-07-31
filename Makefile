# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# BUILD_VERSION is stamped into crystalbackup_build_info at link time. Until M6 nothing passed
# -X anywhere, so every binary — released images included — reported version="dev": the one
# series that exists so a dashboard can join a metric to the build that produced it named no
# build at all.
#
# The release pipeline derives it from the tag, the same source charts/crystal-backup/Chart.yaml's
# appVersion comes from (.github/workflows/images.yml). Locally `git describe` is the closest
# honest equivalent, and --dirty matters: a binary built from an edited tree is NOT the tag it is
# nearest to, and a version that quietly claims otherwise is worse than "dev".
BUILD_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
VERSION_LDFLAGS := -X github.com/CrystalBackup/CrystalBackup/internal/metrics.Version=$(BUILD_VERSION)

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
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

# TEST_TIMEOUT overrides go test's 10-minute default, which the envtest controller suite now
# exceeds on its own. That default does not fail a test — it PANICS the binary, so a suite that
# merely grew past it reports as a hang with no failing spec named, which is the least
# actionable signal CI can give. Each spec's Eventually budget is 90s, so a handful of genuine
# failures alone can spend most of this.
TEST_TIMEOUT ?= 20m

# E2E_TIMEOUT is the same guard for the e2e suite, which runs against a real Kind cluster and
# so is far slower and far more variable than envtest. It had been sitting right at go test's
# 10-minute default — one CI run finished in 7m57s and passed while its sibling hit 10m and
# PANICKED — which reads as a flake and is not one: the suite simply outgrew the default. Raised
# again for M3: the manifest round-trip runs REAL mover Jobs (repository init, per-PVC CSI-snapshot
# backup, namespaced + cluster-scoped manifest capture/restore), minutes each, on top of the M0/M2
# deploy cycles — the whole suite now legitimately needs ~35m, so 45m leaves headroom.
E2E_TIMEOUT ?= 45m

# GINKGO_E2E_TIMEOUT is what actually bounds the suite, and it is deliberately SHORTER than the
# go test deadline above. Ginkgo enforces its own suite timeout independently of `go test
# -timeout`, and its default is one hour — so with only the go-test guard set, go test always
# panicked FIRST, and a go-test panic prints a goroutine dump with no indication of which spec was
# running. Two consecutive CI failures were diagnosed as far as "43 minutes of silence after this
# STEP line" and no further, because the signal simply was not there. Letting Ginkgo interrupt
# first turns the same hang into a report naming the spec, the step, and its progress. The
# crucible harness learned this in M4 (test/crucible/scripts/run-tests.sh); the e2e suite never
# got the same treatment.
GINKGO_E2E_TIMEOUT ?= 40m

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -timeout $(TEST_TIMEOUT) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# kubectl kuberc is disabled by default for test isolation; enable with:
# - KUBECTL_KUBERC=true
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= bs-k8s-backup-test-e2e
# Optional pinned Kind node image for reproducible CI (e.g. kindest/node:v1.33.1@sha256:...);
# empty => use the node image bundled with the installed Kind release.
KIND_IMAGE ?=
# Operator image built + loaded into Kind for the e2e run (M0 appVersion 0.0.0, adr/0014).
E2E_IMG ?= example.com/crystalbackup-operator:v0.0.0-e2e
# Mover image (crystal-mover + pinned restic) built + loaded into Kind for the M3 data-path
# suite. The production mover is apko/Wolfi (build/apko/mover.yaml); Dockerfile.mover is the
# plain-Docker equivalent the kind e2e uses so real mover Jobs can run without the melange/apko
# toolchain. The M3 Helm install points --mover-image at this loaded tag.
E2E_MOVER_IMG ?= example.com/crystalbackup-mover:v0.0.0-e2e
# Sync image (crystal-mover + pinned restic + rclone) for the M5 external-sync suite. A THIRD
# image, not a bigger mover: rclone is a hard requirement of sync and of nothing else, and the
# production split exists so its dependency surface stays off the backup/restore path
# (spec/adr/0013). Dockerfile.sync mirrors that split for kind. The M5 Helm install points
# --sync-image at this loaded tag.
E2E_SYNC_IMG ?= example.com/crystalbackup-sync:v0.0.0-e2e
# Pinned versions of the e2e data-path infrastructure, installed by
# test/e2e/manifests/install-csi-snapshot.sh (external-snapshotter + csi-driver-host-path).
EXTERNAL_SNAPSHOTTER_VERSION ?= v8.2.0
CSI_DRIVER_HOSTPATH_VERSION ?= v1.15.0

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
			$(KIND) create cluster --name $(KIND_CLUSTER) --config test/e2e/kind-config.yaml $(if $(KIND_IMAGE),--image $(KIND_IMAGE),) ;; \
	esac
	@# Raise the inotify limits inside every node: Docker Desktop's Linux VM ships defaults far
	@# too low for a whole control plane + CSI + operator of file watchers, which surfaces as
	@# kube-proxy/snapshot-controller CrashLoopBackOff with "too many open files" (the standard
	@# kind gotcha, https://kind.sigs.k8s.io/docs/user/known-issues/#pod-errors-due-to-too-many-open-files).
	@# Idempotent and a no-op on hosts already configured; applied on every run so a pre-existing
	@# cluster is fixed too.
	@for node in $$($(KIND) get nodes --name $(KIND_CLUSTER)); do \
		docker exec "$$node" sysctl -q -w fs.inotify.max_user_watches=524288 fs.inotify.max_user_instances=512; \
	done

.PHONY: install-test-e2e-infra
install-test-e2e-infra: setup-test-e2e ## Install external-snapshotter + csi-driver-host-path + SeaweedFS (S3) into the e2e Kind cluster.
	KUBECTL="$(KUBECTL)" \
	EXTERNAL_SNAPSHOTTER_VERSION="$(EXTERNAL_SNAPSHOTTER_VERSION)" \
	CSI_DRIVER_HOSTPATH_VERSION="$(CSI_DRIVER_HOSTPATH_VERSION)" \
		bash test/e2e/manifests/install-csi-snapshot.sh

.PHONY: test-e2e
test-e2e: install-test-e2e-infra manifests generate fmt vet ## Create the Kind cluster (CSI snapshots + SeaweedFS), load the images, deploy, and run the Ginkgo e2e suite.
	$(MAKE) docker-build IMG=$(E2E_IMG)
	$(KIND) load docker-image $(E2E_IMG) --name $(KIND_CLUSTER)
	$(MAKE) docker-build-mover E2E_MOVER_IMG=$(E2E_MOVER_IMG)
	$(KIND) load docker-image $(E2E_MOVER_IMG) --name $(KIND_CLUSTER)
	$(MAKE) docker-build-sync E2E_SYNC_IMG=$(E2E_SYNC_IMG)
	$(KIND) load docker-image $(E2E_SYNC_IMG) --name $(KIND_CLUSTER)
	E2E_IMG=$(E2E_IMG) E2E_MOVER_IMG=$(E2E_MOVER_IMG) E2E_SYNC_IMG=$(E2E_SYNC_IMG) E2E_BUILD_IMAGE=false KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) \
		go test -tags=e2e ./test/e2e/ -timeout $(E2E_TIMEOUT) -v -ginkgo.v \
			--ginkgo.timeout=$(GINKGO_E2E_TIMEOUT)
	$(MAKE) cleanup-test-e2e

# Alias for the milestone exit-criteria wording in spec/90-roadmap.md ("make e2e").
.PHONY: e2e
e2e: test-e2e ## Alias for test-e2e (spec/90-roadmap.md M0 exit criteria).

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint over the whole tree, INCLUDING the build-tagged crucible suite.
	"$(GOLANGCI_LINT)" run
	# The crucible suite is behind `//go:build crucible`, so the run above cannot see it: an
	# entire test package went unlinted until M6, and a dead variable silently made the report's
	# section order depend on Go map iteration. Hence a second, scoped pass.
	#
	# Scoped, and not `--build-tags crucible` on the whole tree, deliberately: the tag is a
	# two-way switch. test/crucible/tests/runname_hermeticity_test.go is `//go:build !crucible`
	# precisely so it runs in the ORDINARY suite while inspecting the tagged one — enabling the
	# tag globally would hide the very guard that keeps the tagged suite honest.
	"$(GOLANGCI_LINT)" run --build-tags crucible ./test/crucible/tests/...

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

# The Grafana dashboards are JSON, so nothing in the Go build can notice that a panel queries
# a series the operator does not emit — it just renders "No data", which on a backup dashboard
# is indistinguishable from "all clear". This target makes internal/metrics/names.go the
# authority for both the series names and the label sets used in every dashboard query.
.PHONY: check-dashboards
check-dashboards: ## Verify the Grafana dashboards only use series/labels declared in internal/metrics.
	python3 hack/check-dashboard-metrics.py

## The chart's alert rules are GENERATED from the table in internal/alerts/rules.go, where every
## expression is concatenated from the constants in internal/metrics/names.go. That is the whole
## point: renaming a series becomes a compile error instead of an alert that quietly stops firing,
## which is the state five of these nine rules shipped in until M6. The YAML is an artifact —
## regenerated, never edited.
ALERT_RULES_OUT ?= $(CHART_DIR)/rules/crystalbackup.rules.yaml

.PHONY: alert-rules
alert-rules: ## Generate the chart's PrometheusRule body from internal/alerts/rules.go.
	go run ./internal/alerts/cmd/genrules --chart-dir "$(CHART_DIR)"

## Regenerates into a scratch directory and diffs, rather than regenerating in place and asking
## git: a file that has never been committed is invisible to `git diff`, and this guard has to
## work on the change that ADDS a rule as well as on the one that edits it.
.PHONY: alert-rules-verify
alert-rules-verify: ## Fail if the committed alert rules are stale (CI guard).
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	go run ./internal/alerts/cmd/genrules --chart-dir "$$tmp" >/dev/null; \
	if ! diff -u "$(ALERT_RULES_OUT)" "$$tmp/rules/crystalbackup.rules.yaml"; then \
		echo "ERROR: $(ALERT_RULES_OUT) is out of date with internal/alerts/rules.go."; \
		echo "       Run 'make alert-rules' and commit the result."; \
		exit 1; \
	fi; \
	echo "$(ALERT_RULES_OUT) is up to date."

## promtool is the only thing that actually PARSES the PromQL and the annotation templates, and —
## through `promtool test rules` — the only thing that EVALUATES them. Everything else in this
## build reads the rules; promtool runs them.
##
## It used to be "used if the machine happens to have one", which made the PromQL check the fourth
## self-disabling guard in this project: green on every machine that lacked the binary, including
## CI. It is now downloaded, pinned by version AND by SHA-256, into $(LOCALBIN) like every other
## tool here. It is fetched from the official release archive rather than `go install`ed because
## prometheus/prometheus cannot be `go install`ed at all (v3 module path plus `replace`
## directives), and vendoring it as a library would drag its whole dependency tree through this
## project's vulnerability surface for a syntax check.
.PHONY: check-alert-rules
check-alert-rules: promtool ## Syntax-check the generated alert rules with promtool.
	"$(PROMTOOL)" check rules "$(ALERT_RULES_OUT)"

## The unit tests. `promtool check rules` proves the PromQL PARSES; only this proves it can fire.
## Five of the nine rules shipped reading series nobody emitted — valid PromQL, evaluated without
## error, unable to match anything, ever. That class of defect is invisible to a syntax check by
## construction: the files below feed each rule synthetic series and assert both that it fires when
## it should and that it stays silent when it should not, including the absence cases (spec §2.4:
## a repository that has never been checked emits nothing and must NOT page).
ALERT_RULE_TESTS ?= $(wildcard internal/alerts/testdata/*.yaml)

.PHONY: test-alert-rules
test-alert-rules: promtool ## Run the promtool unit tests for the alert rules.
	@test -n "$(ALERT_RULE_TESTS)" || { \
		echo "ERROR: no rule-test files found under internal/alerts/testdata/."; \
		echo "       The alert rules would then be 'tested' by running nothing at all."; \
		exit 1; \
	}
	"$(PROMTOOL)" test rules $(ALERT_RULE_TESTS)
	@$(MAKE) --no-print-directory alert-rules-covered

## A rule with no test passes `promtool test rules` by not being mentioned, which is the same shape
## of silence this whole milestone exists to remove. Adding a rule to internal/alerts/rules.go must
## therefore fail the build until somebody has watched it fire. Two rules appeared in this table
## while lot I was being written; without this they would have shipped untested and nothing would
## have said so.
.PHONY: alert-rules-covered
alert-rules-covered: ## Fail if any shipped alert rule has no promtool test case.
	@missing=""; \
	for name in $$(sed -n 's/^ *- alert: //p' "$(ALERT_RULES_OUT)"); do \
		grep -qE "^ *alertname: $${name}\b" $(ALERT_RULE_TESTS) || missing="$$missing $$name"; \
	done; \
	if [ -n "$$missing" ]; then \
		echo "ERROR: alert rules with no promtool test case:$$missing"; \
		echo "       Add cases under internal/alerts/testdata/ — at minimum one that fires and one"; \
		echo "       just under the threshold that does not."; \
		exit 1; \
	fi; \
	echo "every alert rule in $(ALERT_RULES_OUT) has at least one promtool test case."

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -ldflags="$(VERSION_LDFLAGS)" -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	# .dockerignore keeps .git out of the build context, so the Dockerfile cannot run
	# `git describe` itself — the version has to be handed in.
	$(CONTAINER_TOOL) build --build-arg BUILD_VERSION="$(BUILD_VERSION)" -t ${IMG} .

.PHONY: docker-build-mover
docker-build-mover: ## Build the mover image (crystal-mover + pinned restic) for the kind e2e data path.
	$(CONTAINER_TOOL) build -f Dockerfile.mover -t ${E2E_MOVER_IMG} .

.PHONY: docker-build-sync
docker-build-sync: ## Build the sync image (crystal-mover + pinned restic + rclone) for the kind e2e external-sync path.
	$(CONTAINER_TOOL) build -f Dockerfile.sync -t ${E2E_SYNC_IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

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
	- $(CONTAINER_TOOL) buildx create --name bs-k8s-backup-builder
	$(CONTAINER_TOOL) buildx use bs-k8s-backup-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm bs-k8s-backup-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

# KUBECTL_DELETE_TIMEOUT bounds the waits in `uninstall` and `undeploy`. `kubectl delete` waits
# FOREVER by default, and both of these delete objects whose disappearance depends on a controller:
# the CRDs (a delete waits for every custom resource instance) and, through config/default, the
# operator's own namespace (a delete waits for everything inside it). Six of the twelve kinds carry
# a finalizer that only the operator removes — so `make undeploy` with a single live Backup left
# takes the operator down and then waits, in the same command, on an object nobody can release any
# more. Unbounded that is a permanent hang: it cost this project a 35-minute e2e run before Ginkgo
# cut it. Bounded, it fails in minutes with a message you can act on.
#
# This does NOT make the uninstall safe — it makes it terminate. The safe order is in
# docs/DECOMMISSION.md §3: delete the custom resources first, with the operator still running.
KUBECTL_DELETE_TIMEOUT ?= 3m

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) --timeout=$(KUBECTL_DELETE_TIMEOUT) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion. Delete the custom resources FIRST (docs/DECOMMISSION.md §3) — this target takes the operator and the CRDs down together.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) --timeout=$(KUBECTL_DELETE_TIMEOUT) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
CRD_REF_DOCS ?= $(LOCALBIN)/crd-ref-docs
PROMTOOL ?= $(LOCALBIN)/promtool

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.21.0
# v0.2.0 is the floor that still builds on a current Go toolchain: v0.1.0 pins
# golang.org/x/tools v0.19.0, whose tokeninternal package no longer compiles.
CRD_REF_DOCS_VERSION ?= v0.2.0

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

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
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: crd-ref-docs
crd-ref-docs: $(CRD_REF_DOCS) ## Download crd-ref-docs locally if necessary.
$(CRD_REF_DOCS): $(LOCALBIN)
	$(call go-install-tool,$(CRD_REF_DOCS),github.com/elastic/crd-ref-docs,$(CRD_REF_DOCS_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# promtool ships inside the Prometheus release archive and is the ONE tool here that cannot come
# through go-install-tool: `go install github.com/prometheus/prometheus/cmd/promtool@vX` fails on
# the v3 module path plus the `replace` directives in that repo's go.mod. So it is fetched from the
# official release tarball and verified against a checksum committed HERE — a downloaded binary
# that is only as trustworthy as whatever GitHub served today has no business gating a build.
#
# Bumping PROMETHEUS_VERSION means replacing the four lines below with that release's own
# sha256sums.txt entries. That is deliberate friction: a version bump that left stale digests
# behind would fail loudly on the next fetch instead of silently accepting a different binary.
PROMETHEUS_VERSION ?= 3.13.1

# From https://github.com/prometheus/prometheus/releases/download/v$(PROMETHEUS_VERSION)/sha256sums.txt
define prometheus_sha256sums
bc6cef4bdbeb3d0974ac161684dd2a0c4d4e341a13a4a634917b1c09d0f33fc5  prometheus-$(PROMETHEUS_VERSION).darwin-amd64.tar.gz
28d1f1224b2a22f84c801487fad4b3bd58f94a8cb58cf9340557e787030c9703  prometheus-$(PROMETHEUS_VERSION).darwin-arm64.tar.gz
962b812371aff838d152b6ff2d56fdb7a6396f5542f48ebf73421b9721f0d103  prometheus-$(PROMETHEUS_VERSION).linux-amd64.tar.gz
fbd8e5e0f6ad2e7d053e717739186caee4fd0cab2cf9335bfc86c292fe2a2bfe  prometheus-$(PROMETHEUS_VERSION).linux-arm64.tar.gz
endef
export prometheus_sha256sums

.PHONY: promtool
promtool: $(PROMTOOL) ## Download promtool (from the pinned Prometheus release) if necessary.
$(PROMTOOL): $(LOCALBIN)
	@set -eu; \
	versioned="$(PROMTOOL)-$(PROMETHEUS_VERSION)"; \
	if [ -x "$$versioned" ]; then ln -sf "$$versioned" "$(PROMTOOL)"; exit 0; fi; \
	os="$$(uname -s | tr '[:upper:]' '[:lower:]')"; \
	case "$$(uname -m)" in \
		x86_64|amd64) arch=amd64 ;; \
		arm64|aarch64) arch=arm64 ;; \
		*) echo "promtool: no Prometheus release archive for $$(uname -m)" >&2; exit 1 ;; \
	esac; \
	archive="prometheus-$(PROMETHEUS_VERSION).$${os}-$${arch}.tar.gz"; \
	want="$$(printf '%s\n' "$$prometheus_sha256sums" | awk -v a="$$archive" '$$2 == a { print $$1 }')"; \
	[ -n "$$want" ] || { \
		echo "promtool: no pinned checksum for $$archive — add it from that release's sha256sums.txt" >&2; \
		exit 1; \
	}; \
	tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	url="https://github.com/prometheus/prometheus/releases/download/v$(PROMETHEUS_VERSION)/$$archive"; \
	echo "Downloading $$url"; \
	curl -fsSL --retry 3 -o "$$tmp/$$archive" "$$url"; \
	if command -v sha256sum >/dev/null 2>&1; then got="$$(sha256sum "$$tmp/$$archive" | awk '{print $$1}')"; \
	else got="$$(shasum -a 256 "$$tmp/$$archive" | awk '{print $$1}')"; fi; \
	[ "$$got" = "$$want" ] || { \
		echo "promtool: checksum mismatch for $$archive" >&2; \
		echo "  expected $$want" >&2; \
		echo "  got      $$got" >&2; \
		exit 1; \
	}; \
	tar -xzf "$$tmp/$$archive" -C "$$tmp"; \
	install -m 0755 "$$tmp/prometheus-$(PROMETHEUS_VERSION).$${os}-$${arch}/promtool" "$$versioned"; \
	ln -sf "$$versioned" "$(PROMTOOL)"; \
	"$(PROMTOOL)" --version | head -1

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

##@ Chart

## Helm chart locations
HELM ?= helm
CHART_DIR ?= charts/crystal-backup
CRD_SRC ?= config/crd/bases

.PHONY: chart-crds
chart-crds: ## Copy generated CRDs from config/crd/bases into the Helm chart's crds/ (run 'make manifests' first to refresh the bases).
	@mkdir -p "$(CHART_DIR)/crds"
	@echo "Copying CRDs $(CRD_SRC)/*.yaml -> $(CHART_DIR)/crds/"
	@rm -f "$(CHART_DIR)"/crds/*.yaml
	@cp "$(CRD_SRC)"/*.yaml "$(CHART_DIR)/crds/"

.PHONY: chart-lint
chart-lint: chart-crds ## Lint the Helm chart (helm lint), CRDs included.
	$(HELM) lint "$(CHART_DIR)"

.PHONY: chart-package
chart-package: chart-crds ## Package the Helm chart (CRDs included) into dist/.
	@mkdir -p dist
	$(HELM) package "$(CHART_DIR)" --destination dist

##@ Docs

## The user-facing API reference is GENERATED from api/v1alpha1/ into the Starlight
## docs site. Twelve hand-maintained CRD tables drift apart from the Go types within
## one release, and a reference that disagrees with the CRD is worse than none — so
## this is the only source, and it is regenerated, never edited in place.
API_DOCS_SRC ?= api/v1alpha1
API_DOCS_DIR ?= website/src/content/docs/reference/api
API_DOCS_OUT ?= $(API_DOCS_DIR)/index.md
API_DOCS_CONFIG ?= hack/crd-ref-docs.yaml

.PHONY: api-docs
api-docs: crd-ref-docs ## Generate the website's API reference from api/v1alpha1/ (website/src/content/docs/reference/api/).
	@mkdir -p "$(API_DOCS_DIR)"
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	"$(CRD_REF_DOCS)" \
		--config "$(API_DOCS_CONFIG)" \
		--source-path "$(API_DOCS_SRC)" \
		--renderer markdown \
		--output-mode single \
		--output-path "$$tmp/api.md" \
		--max-depth 12 \
		--log-level ERROR; \
	{ \
		printf '%s\n' '---'; \
		printf '%s\n' 'title: API reference'; \
		printf '%s\n' 'description: Every field of every CrystalBackup custom resource, generated from the Go types in api/v1alpha1.'; \
		printf '%s\n' 'tableOfContents:'; \
		printf '%s\n' '  minHeadingLevel: 2'; \
		printf '%s\n' '  maxHeadingLevel: 2'; \
		printf '%s\n' '---'; \
		printf '\n'; \
		printf '%s\n' '<!-- GENERATED FILE — do not edit. Run `make api-docs` after changing api/v1alpha1/. -->'; \
		printf '\n'; \
		printf '%s\n' 'This page is generated from the Go types in `api/v1alpha1/`, so it is exactly what'; \
		printf '%s\n' 'the CRDs installed in your cluster accept. `kubectl explain` on a live cluster is the'; \
		printf '%s\n' 'same information from the same source.'; \
		printf '\n'; \
		sed \
			-e '/^# API Reference$$/d' \
			-e '/^## Packages$$/d' \
			-e '/^- \[crystalbackup\.io\/v1alpha1\](#crystalbackupiov1alpha1)$$/d' \
			-e '/^## crystalbackup\.io\/v1alpha1$$/d' \
			-e '/^Package v1alpha1 contains API Schema definitions/d' \
			-e 's/^### Resource Types$$/## Resource types/' \
			-e 's/^#### /## /' \
			"$$tmp/api.md"; \
	} > "$(API_DOCS_OUT)"
	@echo "Wrote $(API_DOCS_OUT)"

.PHONY: api-docs-verify
api-docs-verify: api-docs ## Fail if the committed API reference is stale (CI guard).
	@if ! git diff --quiet -- "$(API_DOCS_OUT)"; then \
		echo "ERROR: $(API_DOCS_OUT) is out of date. Run 'make api-docs' and commit the result."; \
		git --no-pager diff -- "$(API_DOCS_OUT)"; \
		exit 1; \
	fi
	@echo "$(API_DOCS_OUT) is up to date."

## The Metrics and Alerts reference pages are GENERATED, for the same reason the API reference is
## and at a larger scale of damage. Fifty-three series and eleven rules maintained by hand disagree
## with the operator within one release, and a metrics reference that disagrees with a scrape is
## worse than none: it sends someone to write an alert on a label that does not exist, which is
## valid PromQL that matches nothing, forever, in silence. That is the defect M6 spent itself
## removing from the rules themselves — documenting it back in would be the same bug, one step
## further from the code.
##
## Nothing in the generator restates a name, a label, a help string, a bucket boundary, a threshold
## or an expression: the metric facts are read back out of the prometheus.Desc values the collectors
## register (and out of a real Gather, for the histogram buckets), and the rules out of
## alerts.Rules(), Rationale included.
OBS_DOCS_OUT ?= website/src/content/docs/reference/metrics.md website/src/content/docs/reference/alerts.md

.PHONY: observability-docs
observability-docs: ## Generate the website's Metrics and Alerts reference from internal/metrics and internal/alerts.
	go run ./internal/refdocs/cmd/gendocs --root .

## Regenerates into a scratch directory and diffs, rather than regenerating in place and asking
## git — the same choice alert-rules-verify makes, for the same reason: a page that has never been
## committed is invisible to `git diff`, and this guard has to work on the change that ADDS one.
.PHONY: observability-docs-verify
observability-docs-verify: ## Fail if the committed Metrics/Alerts reference pages are stale (CI guard).
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	go run ./internal/refdocs/cmd/gendocs --root "$$tmp" >/dev/null; \
	for page in $(OBS_DOCS_OUT); do \
		if ! diff -u "$$page" "$$tmp/$$page"; then \
			echo "ERROR: $$page is out of date with internal/metrics and internal/alerts."; \
			echo "       Run 'make observability-docs' and commit the result."; \
			exit 1; \
		fi; \
	done; \
	echo "the Metrics and Alerts reference pages are up to date."

## The preflight script (website/public/preflight.sh) tells an administrator, per StorageClass,
## which exposer CrystalBackup would choose and which volumes it would skip. That answer belongs to
## internal/exposer.Registry.For and to nothing else. A shell re-implementation of the routing is a
## second copy in a language no compiler checks, handed to strangers as an authoritative preview —
## and it drifts silently, because the script keeps printing confident verdicts either way. So the
## routing block is generated, and the generator does not merely restate the constants: it also
## executes the real Registry.For against a fake cluster and refuses to emit a rule that disagrees
## with it, and it fails outright when internal/exposer declares an exposer Kind it has no verdict
## for. The SHA-256 sidecar is written in the same pass and held by the same guard — a checksum
## regenerated separately goes stale one commit after the script, and it fails in the hands of the
## one administrator who bothered to verify.
PREFLIGHT_OUT ?= website/public/preflight.sh website/public/preflight.sh.sha256

.PHONY: preflight-table
preflight-table: ## Regenerate the exposer-selection block and checksum of website/public/preflight.sh from internal/exposer.
	go run ./hack/gen-preflight-table --root .

.PHONY: preflight-table-verify
preflight-table-verify: ## Fail if preflight.sh's exposer table or checksum is stale (CI guard).
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	go run ./hack/gen-preflight-table --root . --out "$$tmp/preflight.sh" >/dev/null; \
	if ! diff -u website/public/preflight.sh "$$tmp/preflight.sh"; then \
		echo "ERROR: website/public/preflight.sh is out of date with internal/exposer."; \
		echo "       Run 'make preflight-table' and commit the result."; \
		exit 1; \
	fi; \
	if ! diff -u website/public/preflight.sh.sha256 "$$tmp/preflight.sh.sha256"; then \
		echo "ERROR: website/public/preflight.sh.sha256 does not match the script it claims to check."; \
		echo "       Run 'make preflight-table' and commit the result."; \
		exit 1; \
	fi; \
	echo "the preflight script and its checksum are up to date."

## The website is bilingual (English at the root, French under /fr/), and a translated page
## that has silently fallen behind its English source is worse than no translation at all: a
## reader trusts a stale page exactly as much as a fresh one, and nothing in a normal review
## catches it — the French file is untouched, so it never appears in the diff that broke it.
##
## So every translated page records the git blob hash of the English bytes it was translated
## from, and this recomputes it. A blob hash changes if and only if the source changes, which
## means editing an English page fails the build in the same pull request that edited it — the
## only moment at which somebody still knows what changed and why.
##
## The check refuses to pass by finding nothing: no locale declared, a declared locale with no
## page, zero English pages, zero translated pages, or no git on PATH are all FAILURES. That is
## not paranoia, it is this project's own history — `check-alert-rules` opened with
## `command -v promtool || exit 0` for five milestones and therefore verified nothing, in green,
## on every runner it ever ran on.
##
## Coverage (an English page with no French counterpart) is a WARNING today and an error under
## --require-full-coverage; the reasoning is written out at the top of the script.
TRANSLATION_CHECK ?= website/tools/check-translations.mjs

.PHONY: check-translations
check-translations: ## Fail if a translated website page is stale against its English source (CI guard).
	@command -v node >/dev/null 2>&1 || { \
		echo "ERROR: node is not on PATH, so the translation staleness guard cannot run."; \
		echo "       It fails rather than skipping: a check that quietly declines to run and"; \
		echo "       reports success is the exact defect this guard exists to prevent."; \
		exit 1; \
	}
	@$(MAKE) --no-print-directory check-translations-selftest
	@node "$(TRANSLATION_CHECK)"

## Runs before the check itself, every time, because it costs a fifth of a second and it is the
## difference between "the guard reported green" and "the guard can still go red". It builds
## fixture trees and runs the real checker against them: drifted source, vanished source, missing
## sourceHash, a sourceFile redirected at some file that never changes, and an empty locale.
.PHONY: check-translations-selftest
check-translations-selftest: ## Prove the translation guard still fails on a drifted page.
	@node "$(TRANSLATION_CHECK)" --self-test

## The button that says "yes, I have re-translated it". It only stamps the new hash — it
## translates nothing, and running it without re-reading the English diff certifies a page that
## is now wrong. It prints every page it touched for that reason.
.PHONY: translations-refresh
translations-refresh: ## Restamp sourceHash on stale translated pages AFTER re-translating them.
	@node "$(TRANSLATION_CHECK)" --refresh
