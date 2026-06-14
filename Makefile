# coop — see README.md
.DEFAULT_GOAL := help

build: ## Build the coop binary to ./coop
	@go build -trimpath -o coop .

install: ## Build + install onto PATH, build the image, run doctor
	@./install.sh

test: ## Run unit tests (no container runtime needed)
	@go test ./...

cover: ## Run unit tests with a coverage summary
	@go test -cover ./...

lint: ## gofmt check + go vet (+ staticcheck if installed)
	@gofmt -l . | (! grep .) || { echo "gofmt: files need formatting (run: gofmt -w .)"; exit 1; }
	@go vet ./...
	@command -v staticcheck >/dev/null 2>&1 && staticcheck ./... || echo "(staticcheck not installed — skipping)"

doctor: ## Integration check: prove isolation holds (needs a runtime)
	@go run . doctor

check: lint test ## What CI runs: lint + unit tests

clean: ## Remove build artifacts
	@rm -f coop

help: ## List targets
	@grep -hE '^[a-z]+:.*##' $(MAKEFILE_LIST) | sed -E 's/:.*## / — /' | sort

.PHONY: build install test cover lint doctor check clean help
