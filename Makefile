CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1
SETUP_ENVTEST ?= go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.23
ENVTEST_K8S_VERSION ?= 1.36.x
IMG ?= ghcr.io/songof/pulsar-operator:dev

.PHONY: generate manifests test build web image run

## generate: deepcopy functions for the API types
generate:
	$(CONTROLLER_GEN) object paths=./api/...

## manifests: CRDs and the operator's RBAC role
manifests:
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd
	$(CONTROLLER_GEN) rbac:roleName=pulsar-operator paths=./internal/... output:rbac:artifacts:config=config/rbac

## test: unit and envtest controller tests
test:
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" go test ./... -count=1

build:
	go build -o bin/pulsar-operator ./cmd/operator

web:
	cd web && npm ci && npm run build

image:
	docker build -t $(IMG) .

## run: the operator and console against the current kubeconfig
run:
	WEB_ROOT=web/dist go run ./cmd/operator
