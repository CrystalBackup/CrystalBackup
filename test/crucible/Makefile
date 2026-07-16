# Crucible — real-conditions e2e for Crystal Backup on Hetzner Cloud.
#
#   make up      provision servers + RKE2 + storage stack + crystal-backup
#   make seed    populate the tenant namespaces (case matrix)
#   make test    run the Ginkgo suite (LABELS=m0 to filter)
#   make down    destroy everything (CONFIRM=yes)
#
# Tools come from ./mise.toml (`mise install` once in this directory).
# Credentials come from <repo>/.secrets/ via scripts/load-env.sh.

SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

LOAD_ENV := source scripts/load-env.sh
TOFU     := tofu -chdir=terraform
KUBECONFIG_PATH := $(abspath artifacts/kubeconfig)

# Ginkgo label filter, e.g. LABELS=infra / LABELS=m0 / LABELS='infra || m0'.
LABELS ?=

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "Typical first run: make up seed test    (then: make down CONFIRM=yes)"

.PHONY: check-env
check-env: ## Verify credentials load correctly
	@$(LOAD_ENV) && \
	  [[ -n "$${HCLOUD_TOKEN:-}" ]] || { echo "FATAL: HCLOUD_TOKEN missing (.secrets/HETZNER_TOKEN)"; exit 1; } && \
	  [[ -n "$${TF_VAR_s3_access_key:-}" ]] || { echo "FATAL: S3 keys missing (.secrets/HETZNER_S3)"; exit 1; } && \
	  [[ -f "$${ANSIBLE_PRIVATE_KEY_FILE:-/nonexistent}" ]] || { echo "FATAL: SSH key missing (.secrets/crystalbackup.key)"; exit 1; } && \
	  echo "credentials OK"

.PHONY: init
init: ## tofu init (providers)
	@$(LOAD_ENV) && $(TOFU) init

.PHONY: plan
plan: check-env init ## Preview the infrastructure changes
	@$(LOAD_ENV) && $(TOFU) plan

.PHONY: infra
infra: check-env init ## Provision Hetzner servers, network, volumes, S3 bucket
	@$(LOAD_ENV) && $(TOFU) apply -auto-approve

.PHONY: cluster
cluster: ## Install RKE2 (ansible) and fetch the kubeconfig
	@$(LOAD_ENV) && cd ansible && ansible-playbook site.yml

.PHONY: components
components: ## Deploy snapshot-controller, rook-ceph, longhorn, local-path, crystal-backup
	@$(LOAD_ENV) && deploy/deploy.sh

.PHONY: up
up: infra cluster components ## infra + cluster + components

.PHONY: seed
seed: ## Populate the tenant namespaces (c-web, c-db, c-media, c-legacy, c-edge, c-empty)
	@$(LOAD_ENV) && seed/seed.sh

.PHONY: test
test: ## Run the Ginkgo suite (filter with LABELS=infra / LABELS=m0 / ...)
	@$(LOAD_ENV) && cd ../.. && \
	  KUBECONFIG="$(KUBECONFIG_PATH)" go test -tags crucible ./test/crucible/tests \
	    -v -timeout 60m -args --ginkgo.v --ginkgo.label-filter='$(LABELS)'

.PHONY: status
status: ## Show terraform outputs + cluster state
	@$(LOAD_ENV) && $(TOFU) output 2>/dev/null || echo "(no terraform state yet)"
	@[[ -f "$(KUBECONFIG_PATH)" ]] && KUBECONFIG="$(KUBECONFIG_PATH)" kubectl get nodes -o wide 2>/dev/null || true

.PHONY: kubeconfig
kubeconfig: ## Print the export line for manual kubectl use
	@echo "export KUBECONFIG=$(KUBECONFIG_PATH)"

.PHONY: ssh
ssh: ## SSH into a node: make ssh HOST=crucible-master-1
	@$(LOAD_ENV) && \
	  ip="$$(awk -v h="$(HOST)" '$$1 == h { for (i=2;i<=NF;i++) if ($$i ~ /^ansible_host=/) { sub("ansible_host=","",$$i); print $$i } }' ansible/inventory/hosts.ini)" && \
	  [[ -n "$$ip" ]] || { echo "unknown HOST=$(HOST) (see ansible/inventory/hosts.ini)"; exit 1; } && \
	  ssh -i "$${ANSIBLE_PRIVATE_KEY_FILE}" -o StrictHostKeyChecking=no root@"$$ip"

.PHONY: down
down: check-env ## Destroy ALL crucible infrastructure (requires CONFIRM=yes)
	@[[ "$(CONFIRM)" == "yes" ]] || { echo "Refusing. Run: make down CONFIRM=yes"; exit 1; }
	@$(LOAD_ENV) && $(TOFU) destroy -auto-approve
	@rm -f artifacts/kubeconfig artifacts/crucible.env

.PHONY: nuke
nuke: ## Delete crucible resources by LABEL (when the tfstate is lost)
	@$(LOAD_ENV) && scripts/nuke.sh

.PHONY: clean
clean: ## Remove local artifacts (does NOT touch cloud resources)
	rm -rf artifacts/ ansible/inventory/ terraform/.terraform terraform/tfplan
