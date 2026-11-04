# Common development tasks. Run `make help` for a list.

BIN         := bin/linkly
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@2026.2.2

.DEFAULT_GOAL := help
.PHONY: help build run test cover bench lint fmt fmt-check vet staticcheck docker compose-up demo demo-serve clean

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

DEMO_DIR := dist-demo
# Path the demo is served under; on GitHub Pages that is /<repository>/.
# 404.html forwards <DEMO_BASE><code> to <DEMO_BASE>#/<code>.
DEMO_BASE ?= /linkly/

demo: ## Build the in-browser WebAssembly demo site into dist-demo/
	rm -rf $(DEMO_DIR)
	mkdir -p $(DEMO_DIR)
	GOOS=js GOARCH=wasm CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(DEMO_DIR)/linkly.wasm ./cmd/linkly-wasm
	@goroot=$$(go env GOROOT); \
	if [ -f "$$goroot/lib/wasm/wasm_exec.js" ]; then cp "$$goroot/lib/wasm/wasm_exec.js" $(DEMO_DIR)/; \
	else cp "$$goroot/misc/wasm/wasm_exec.js" $(DEMO_DIR)/; fi
	go run ./demo/assemble -out $(DEMO_DIR) -base $(DEMO_BASE)
	@ls -l $(DEMO_DIR)/linkly.wasm

# python's http.server does not serve 404.html for missing paths the way
# GitHub Pages does, so /linkly/<code> forwarding cannot be tried here.
demo-serve: demo ## Serve the demo at http://localhost:8000/linkly/
	@mkdir -p $(DEMO_DIR)-serve && rm -f $(DEMO_DIR)-serve/linkly && ln -s ../$(DEMO_DIR) $(DEMO_DIR)-serve/linkly
	@echo "http://localhost:8000/linkly/"
	cd $(DEMO_DIR)-serve && python3 -m http.server 8000

clean: ## Remove build and test artifacts
	rm -rf bin coverage.out $(DEMO_DIR) $(DEMO_DIR)-serve
