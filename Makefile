# All command implementations live in module/shared scripts.
.DEFAULT_GOAL := help
export CONTROLLER_GEN SETUP_ENVTEST ENVTEST_K8S_VERSION IMG STORAGE_IMG AGENT_IMG PYTHON KUBECTL
PYTHON ?= python3
KUBECTL ?= kubectl

help:
	@echo 'init [ARGS=--integration]: local Go, Node and optional envtest dependencies'
	@echo 'test-unit / test-integration / test-system: separate test layers; test: all three'
	@echo 'test-offline: unit + Storage integration + system fixtures (no envtest or real cluster)'
	@echo 'Module commands: modules/{pulsar,storage,agent}/scripts/dev.sh --help; web/scripts/dev.sh --help'
	@echo 'deploy-two-node and e2e-* are explicit real-cluster operations; see docs/testing/README.md'

.PHONY: help init init-pulsar init-storage init-agent init-web generate manifests test test-unit test-integration test-system test-repository test-go test-offline test-web build build-storage web image images run demo-storage benchmark-storage prepare-pulsar inspect-disks render render-two-node preflight-two-node deploy-two-node e2e-pulsar e2e-storage e2e-pulsar-storage e2e-pulsar-shared-disk test-pulsar test-integration-pulsar image-pulsar render-pulsar build-pulsar test-storage test-integration-storage image-storage render-storage test-agent test-integration-agent image-agent render-agent build-agent build-storage-api build-storage-plan build-storage-benchmark

init:
	bash scripts/dev.sh init $(ARGS)

init-pulsar:
	bash modules/pulsar/scripts/dev.sh init $(ARGS)

init-storage:
	bash modules/storage/scripts/dev.sh init $(ARGS)

init-agent:
	bash modules/agent/scripts/dev.sh init $(ARGS)

init-web:
	bash web/scripts/dev.sh init

generate:
	bash modules/pulsar/scripts/dev.sh generate
	bash modules/storage/scripts/dev.sh generate

manifests:
	bash modules/pulsar/scripts/dev.sh manifests

test:
	bash scripts/test.sh all $(ARGS)

test-unit:
	bash scripts/test.sh unit $(ARGS)

test-integration:
	bash scripts/test.sh integration $(ARGS)

test-system:
	bash scripts/test.sh system

test-repository: test-system

test-go:
	bash scripts/dev.sh test-go $(ARGS)

test-offline:
	bash scripts/test.sh offline $(ARGS)

test-web:
	bash web/scripts/dev.sh unit
	bash web/scripts/dev.sh build

build: build-pulsar build-storage build-agent

build-storage:
	bash modules/storage/scripts/dev.sh build

web:
	bash web/scripts/dev.sh build

image: image-pulsar

images: image-pulsar image-storage image-agent

run:
	bash modules/pulsar/scripts/dev.sh run $(ARGS)

demo-storage:
	bash modules/storage/scripts/dev.sh run $(ARGS)

benchmark-storage:
	bash modules/storage/scripts/dev.sh benchmark $(ARGS)

prepare-pulsar:
	bash modules/storage/scripts/dev.sh plan $(ARGS)

inspect-disks:
	bash modules/agent/scripts/dev.sh discover $(ARGS)

render:
	bash scripts/dev.sh render

render-two-node:
	bash scripts/dev.sh render two-node

preflight-two-node:
	bash scripts/dev.sh preflight-two-node $(ARGS)

deploy-two-node:
	bash scripts/dev.sh deploy-two-node $(ARGS)

e2e-pulsar:
	bash modules/pulsar/scripts/dev.sh e2e $(ARGS)

e2e-storage:
	bash modules/storage/scripts/dev.sh e2e "$(STORAGE_CLASS)"

e2e-pulsar-storage:
	bash scripts/dev.sh e2e-pulsar-storage "$(NAMESPACE)" "$(CLUSTER)"

e2e-pulsar-shared-disk:
	bash scripts/dev.sh e2e-pulsar-shared-disk "$(NAMESPACE)" "$(CLUSTER)"

test-pulsar:
	bash modules/pulsar/scripts/dev.sh unit $(ARGS)

test-integration-pulsar:
	bash modules/pulsar/scripts/dev.sh integration $(ARGS)

image-pulsar:
	bash modules/pulsar/scripts/dev.sh image $(ARGS)

render-pulsar:
	bash modules/pulsar/scripts/dev.sh render $(ARGS)

build-pulsar:
	bash modules/pulsar/scripts/dev.sh build

test-storage:
	bash modules/storage/scripts/dev.sh unit $(ARGS)

test-integration-storage:
	bash modules/storage/scripts/dev.sh integration $(ARGS)

image-storage:
	bash modules/storage/scripts/dev.sh image $(ARGS)

render-storage:
	bash modules/storage/scripts/dev.sh render $(ARGS)

test-agent:
	bash modules/agent/scripts/dev.sh unit $(ARGS)

test-integration-agent:
	bash modules/agent/scripts/dev.sh integration $(ARGS)

image-agent:
	bash modules/agent/scripts/dev.sh image $(ARGS)

render-agent:
	bash modules/agent/scripts/dev.sh render $(ARGS)

build-agent:
	bash modules/agent/scripts/dev.sh build

build-storage-api:
	bash modules/storage/scripts/dev.sh build-api

build-storage-plan:
	bash modules/storage/scripts/dev.sh build-plan

build-storage-benchmark:
	bash modules/storage/scripts/dev.sh build-benchmark
