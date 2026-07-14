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

# Signing is intentionally skipped: release signatures are keyless (Sigstore via GitHub
# OIDC), which only exists in the release workflow — a local snapshot validates packaging.
snapshot: ## Build a local release snapshot with GoReleaser (no publish, no signing)
	@goreleaser release --snapshot --clean --skip=sign

doctor: ## Integration check: prove isolation holds (needs a runtime)
	@go run . doctor

docs: ## Regenerate docs/cli.md + site/llms.txt from internal/cli (help.go is the single source)
	@go run ./tools/gendocs

docs-check: ## Fail if the committed CLI docs drifted from help.go (run 'make docs' to fix)
	@go run ./tools/gendocs -check

align: ## Check trailing-# comment alignment in README + site + CLI docs (--write to fix)
	@python3 tools/align-comments.py --check

casts: build ## Regenerate the site terminal casts (refuses a dirty/untagged ./coop; needs python3)
	@python3 tools/gen_casts.py

check: lint test docs-check ## What CI runs: lint + unit tests + CLI-docs freshness

acp-e2e: install ## ACP adapter e2e (needs Docker + a built box + signed-in providers)
	@go test -tags acpe2e -run 'Test(SuperviseResume|ForeignSessionLoadRejectsUnknownID|PresetOwnsSelectorState)$$' -count=1 -v ./internal/acpproxy/

review-writes-e2e: ## Review write-policy e2e (needs Docker; pulls a small test image once)
	@docker image inspect alpine:3.21 >/dev/null 2>&1 || docker pull alpine:3.21
	@go test -tags reviewwritee2e -run '^TestReviewWritesDockerRuntime$$' -count=1 -v ./internal/box/

clean: ## Remove build artifacts
	@rm -f coop
	@rm -rf dist

help: ## List targets
	@grep -hE '^[a-z][a-z0-9-]*:.*##' $(MAKEFILE_LIST) | sed -E 's/:.*## / — /' | sort

.PHONY: build install test cover lint snapshot doctor docs docs-check casts check acp-e2e review-writes-e2e clean help
