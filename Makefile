IMG_MANAGER ?= ghcr.io/jackal/rookpp:latest
IMG_AGENT   ?= ghcr.io/jackal/rookpp-agent:latest

# Location of envtest control-plane binaries (etcd, kube-apiserver, kubectl).
# Override if setup-envtest installs them elsewhere.
ENVTEST_ASSETS ?= $(HOME)/.local/share/kubebuilder-envtest/k8s/1.36.0-linux-amd64

.PHONY: build test vet fmt test-integration test-e2e e2e-kind docker-manager docker-agent install deploy undeploy

build:
	go build ./...

# Unit tests only (no control plane required).
test:
	go test ./internal/topology/... ./internal/migration/... ./internal/disk/... ./internal/provisioning/...

# Integration tests against an envtest control plane.
test-integration:
	KUBEBUILDER_ASSETS=$(ENVTEST_ASSETS) go test ./internal/controller/... -count=1

# End-to-end tests against the current KUBECONFIG cluster (needs Rook + operator).
test-e2e:
	go test -tags e2e ./test/e2e/... -timeout 40m -v

# Spin up a kind cluster, install Rook + operator, and run e2e end-to-end.
e2e-kind:
	./hack/e2e-kind.sh

vet:
	go vet ./...

fmt:
	go fmt ./...

docker-manager:
	docker build --target manager -t $(IMG_MANAGER) .

docker-agent:
	docker build --target agent -t $(IMG_AGENT) .

install:
	kubectl apply -f config/crd/storagecluster.yaml

deploy: install
	kubectl apply -f config/rbac/role.yaml
	kubectl apply -f config/manager/manager.yaml

undeploy:
	-kubectl delete -f config/manager/manager.yaml
	-kubectl delete -f config/rbac/role.yaml
	-kubectl delete -f config/crd/storagecluster.yaml
