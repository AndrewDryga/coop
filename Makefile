# coop — see README.md
.DEFAULT_GOAL := help

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/AndrewDryga/coop/internal/cli.Version=$(VERSION)

build: ## Build the coop binary to ./coop
	@go build -trimpath -ldflags "$(LDFLAGS)" -o coop .

install: ## Build from source and install to ~/.local/bin/coop
	@go build -trimpath -ldflags "$(LDFLAGS)" -o "$(HOME)/.local/bin/coop" .
	@echo "installed $(HOME)/.local/bin/coop ($(VERSION)) — run 'coop build' to build the box image"

test: ## Run unit tests (no container runtime needed)
	@go test ./...

cover: ## Run unit tests with a coverage summary
	@go test -cover ./...

lint: ## gofmt check + go vet (+ staticcheck if installed)
	@gofmt -l . | (! grep .) || { echo "gofmt: files need formatting (run: gofmt -w .)"; exit 1; }
	@go vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; else echo "(staticcheck not installed — skipping)"; fi

snapshot: ## Build a local release snapshot with GoReleaser (no publish)
	@goreleaser release --snapshot --clean

doctor: ## Integration check: prove isolation holds (needs a runtime)
	@go run . doctor

check: lint test ## What CI runs: lint + unit tests

acp-e2e: install ## ACP supervise resume e2e (needs Docker + a built box + signed-in claude)
	@go test -tags acpe2e -run TestSuperviseResume -count=1 -v ./internal/acpproxy/

clean: ## Remove build artifacts
	@rm -f coop
	@rm -rf dist

help: ## List targets
	@grep -hE '^[a-z][a-z0-9-]*:.*##' $(MAKEFILE_LIST) | sed -E 's/:.*## / — /' | sort

.PHONY: build install test cover lint snapshot doctor check acp-e2e clean help
