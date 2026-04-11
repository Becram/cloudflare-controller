IMG ?= cloudflare-controller:latest
CONTROLLER_GEN_VERSION ?= v0.14.0
CONTROLLER_GEN = go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

.PHONY: all generate manifests build run fmt vet docker-build install uninstall test

all: build

## generate: Regenerate zz_generated.deepcopy.go
generate:
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

## manifests: Regenerate CRD and RBAC manifests from markers
manifests:
	$(CONTROLLER_GEN) \
		rbac:roleName=cloudflare-controller-manager-role \
		crd \
		paths="./..." \
		output:crd:artifacts:config=config/crd \
		output:rbac:artifacts:config=config/rbac

## build: Compile manager binary
build: generate fmt vet
	go build -o bin/manager ./cmd/main.go

## run: Run controller locally against the current kubeconfig context
run: manifests generate fmt vet
	go run ./cmd/main.go

## fmt: Run gofmt
fmt:
	go fmt ./...

## vet: Run go vet
vet:
	go vet ./...

## docker-build: Build the controller image
docker-build:
	docker build -t $(IMG) .

## install: Apply CRDs to the cluster
install: manifests
	kubectl apply -f config/crd/

## uninstall: Remove CRDs from the cluster
uninstall:
	kubectl delete -f config/crd/ --ignore-not-found=true

## test: Run unit tests with race detector
test: generate fmt vet
	go test -race ./... -coverprofile cover.out
	go tool cover -func cover.out
