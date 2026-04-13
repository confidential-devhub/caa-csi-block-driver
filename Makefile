BINARY      := cloud-csi-adaptor
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
IMAGE_REPO  ?= cloud-csi-adaptor
IMAGE_TAG   ?= $(VERSION)
GOOS        ?= linux
GOARCH      ?= amd64

LDFLAGS := -X main.version=$(VERSION)

.PHONY: build clean image test test-verbose help

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-18s %s\n", $$1, $$2}'

build: ## Build the driver binary
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/

clean: ## Remove build artifacts
	rm -rf bin/
	rm -rf /tmp/csi-sanity-test/

image: build ## Build the container image
	podman build -t $(IMAGE_REPO):$(IMAGE_TAG) .

test: build ## Run csi-sanity conformance tests
	@hack/run-csi-sanity.sh

test-verbose: build ## Run csi-sanity conformance tests (verbose)
	@hack/run-csi-sanity.sh --ginkgo.v

lint: ## Run go vet
	go vet ./...

fmt: ## Run gofmt
	gofmt -w -s .

mod: ## Tidy go modules
	go mod tidy
