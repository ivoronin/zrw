PROJECT := $(shell basename $(CURDIR))
VERSION ?= dev
GO_VERSION := $(shell grep '^go ' go.mod | awk '{print $$2}')
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test test-integration test-e2e test-all lint release clean docker-build

# Build binary locally
build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(PROJECT) ./cmd/$(PROJECT)

# Unit tests
test:
	go test -race ./...

# Integration tests (optional)
test-integration: build
	go test -tags=integration ./...

# E2E tests (optional)
test-e2e: build
	go test -tags=e2e ./...

# CI calls this target
test-all: lint test
	@echo "All tests passed"

# Linting
lint:
	golangci-lint run

# CI calls this target for releases
release:
	goreleaser release --clean

# Clean build artifacts
clean:
	rm -rf bin/ dist/

# Build Docker image (Go version from go.mod)
docker-build:
	docker build --build-arg GO_VERSION=$(GO_VERSION) --build-arg VERSION=$(VERSION) -t $(PROJECT) .
