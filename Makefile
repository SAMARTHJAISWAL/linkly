# Common development tasks. Run `make help` for a list.

BIN         := bin/linkly
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@2026.2.2

.DEFAULT_GOAL := help
.PHONY: help build run test cover bench lint fmt fmt-check vet staticcheck docker compose-up clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build a static binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/linkly

run: ## Run the server locally with default settings
	go run ./cmd/linkly

test: ## Run all tests with the race detector
	go test -race ./...

cover: ## Run tests with coverage and print the total
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 1

bench: ## Run benchmarks
	go test -run '^$$' -bench . -benchmem ./...

lint: fmt-check vet staticcheck ## Run every static check

fmt: ## Format all Go code
	gofmt -w .

fmt-check: ## Fail if any file needs gofmt
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet: ## Run go vet
	go vet ./...

staticcheck: ## Run staticcheck (pinned version)
	go run $(STATICCHECK) ./...

docker: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t linkly:$(VERSION) .

compose-up: ## Build and start with docker compose
	docker compose up --build

clean: ## Remove build and test artifacts
	rm -rf bin coverage.out
