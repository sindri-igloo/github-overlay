BIN  := ./github-overlay
ARGS ?=

.DEFAULT_GOAL := help
.PHONY: help build install start stop restart status open logs serve fmt lint test check clean

help: ## List the targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-9s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
	@echo "  Pass server flags with ARGS, e.g. make restart ARGS='-closed-days 14'"

build: ## Build the binary from this checkout
	go build -o $(BIN) .

install: ## Install this checkout into $(go env GOPATH)/bin
	go install .

start: build ## Start the local build in the background
	$(BIN) start $(ARGS)

stop: build ## Stop the background server
	$(BIN) stop

restart: build ## Rebuild, then restart the background server
	$(BIN) restart $(ARGS)

status: build ## Show whether the server is running and where
	$(BIN) status

open: build ## Open the dashboard, starting the server if needed
	$(BIN) open $(ARGS)

logs: build ## Follow the server log
	$(BIN) logs -f

serve: build ## Run the local build in the foreground
	$(BIN) serve $(ARGS)

fmt: ## Format the Go code
	gofmt -w .

lint: ## Run the CI lint checks (gofmt, go vet, staticcheck on the go.mod Go)
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "Not gofmt-ed:"; echo "$$out"; exit 1; fi
	go vet ./...
	GOTOOLCHAIN=go$$(go list -m -f '{{.GoVersion}}') go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 ./...

test: ## Run the tests with the race detector
	go test -race ./...

check: lint build test ## Run everything CI runs

clean: ## Remove the built binary
	rm -f $(BIN)
