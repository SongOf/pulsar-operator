CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1
SETUP_ENVTEST ?= go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.23
ENVTEST_K8S_VERSION ?= 1.36.x
IMG ?= pulsar-operator:dev
STORAGE_IMG ?= pulsar-operator-storage:dev
PYTHON ?= python3
KUBECTL ?= kubectl

.DEFAULT_GOAL := help
.PHONY: help generate manifests test test-go test-web test-storage test-repository test-offline build web image image-storage images run demo-storage inspect-disks render render-two-node preflight-two-node deploy-two-node prepare-pulsar e2e-storage e2e-pulsar-storage e2e-pulsar-shared-disk

help:
	@echo 'test-offline: Python module + repository checks (no cluster); test: full Go/envtest + Node + Python checks'
	@echo 'demo-storage: localhost:8080 simulated inventory; inspect-disks: read-only host inventory'
	@echo 'images: build Pulsar and storage images; render: print unified Kubernetes manifest'
	@echo 'prepare-pulsar ARGS="...": generate reviewed integration manifest without applying'
	@echo 'preflight-two-node: inspect .149/.150; deploy-two-node: install management/discovery using explicit image refs'
	@echo 'e2e-storage STORAGE_CLASS=...; e2e-pulsar-storage NAMESPACE=... CLUSTER=...: dedicated real cluster only'
	@echo 'e2e-pulsar-shared-disk NAMESPACE=... CLUSTER=...: explicit two-node shared-data-disk lab acceptance'

generate:
	$(CONTROLLER_GEN) object paths=./api/...

manifests:
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd
	$(CONTROLLER_GEN) rbac:roleName=pulsar-operator paths=./internal/... output:rbac:artifacts:config=config/rbac

test: test-go test-web test-offline

test-go:
	@envtest_assets="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" || exit $$?; \
	test -x "$$envtest_assets/kube-apiserver" && test -x "$$envtest_assets/etcd" || { echo 'envtest assets are missing; controller tests must not be silently skipped'; exit 1; }; \
	KUBEBUILDER_ASSETS="$$envtest_assets" go test ./... -count=1

test-web:
	cd web && npm ci && npm test && npm run build
	node modules/storage/scripts/check-ui.cjs

test-storage:
	cd modules/storage && $(PYTHON) -m unittest discover -s tests -v

test-repository:
	$(PYTHON) -m unittest discover -s tests -v

test-offline: test-storage test-repository

build:
	go build -o bin/pulsar-operator ./cmd/operator

web:
	cd web && npm ci && npm run build

image:
	docker build -t $(IMG) .

image-storage:
	docker build -t $(STORAGE_IMG) modules/storage

images: image image-storage

run:
	WEB_ROOT=web/dist go run ./cmd/operator

demo-storage:
	cd modules/storage && $(PYTHON) -m diskfleet.api

inspect-disks:
	$(PYTHON) modules/storage/scripts/inspect-disks.py

render:
	$(KUBECTL) kustomize deploy/all

render-two-node:
	$(KUBECTL) kustomize deploy/two-node

preflight-two-node:
	bash scripts/deploy-two-node.sh preflight $(ARGS)

deploy-two-node:
	bash scripts/deploy-two-node.sh install $(ARGS)

prepare-pulsar:
	$(PYTHON) scripts/prepare-pulsar.py $(ARGS)

e2e-storage:
	@test -n "$(STORAGE_CLASS)" || { echo 'Set STORAGE_CLASS to a verified DiskFleet class'; exit 2; }
	bash test/e2e/storage.sh "$(STORAGE_CLASS)"

e2e-pulsar-storage:
	@test -n "$(NAMESPACE)" -a -n "$(CLUSTER)" || { echo 'Set NAMESPACE and CLUSTER to dedicated test resources'; exit 2; }
	bash test/e2e/pulsar-storage.sh "$(NAMESPACE)" "$(CLUSTER)"

e2e-pulsar-shared-disk:
	@test -n "$(NAMESPACE)" -a -n "$(CLUSTER)" || { echo 'Set NAMESPACE and CLUSTER to the dedicated shared-disk lab'; exit 2; }
	bash test/e2e/pulsar-shared-disk.sh "$(NAMESPACE)" "$(CLUSTER)"
