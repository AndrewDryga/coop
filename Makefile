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

casts: build ## Regenerate + safety-check site terminal casts (refuses a dirty/untagged ./coop; needs python3)
	@python3 tools/gen_casts.py
	@python3 tools/cast_hygiene.py site/casts

casts-check: ## Validate published casts for private paths, credentials, and secret-shaped values
	@python3 tools/cast_hygiene.py site/casts

tools-test: ## Run standard-library tests for repository maintenance tools
	@python3 -m unittest discover -s tools -p 'test_*.py'

check: lint test provider-scripted-e2e live-process-control docs-check casts-check tools-test ## What CI runs: lint + tests + deterministic provider process E2E + docs/cast freshness

provider-scripted-e2e: ## Deterministic all-provider process e2e (no runtime or credentials needed)
	@go test ./internal/testutil/procharness ./internal/cli/testdata/providerfixture
	@go test -tags providere2e -run '^TestProviderScripted' -count=1 -v ./internal/cli/

live-process-control: ## Deterministic denial tests for tagged live-test process ownership
	@go test -tags providerlivee2e,cooplivetest -run '^Test(LiveACPProcess|LiveInterruptible|LiveRunInterruptible|ProviderConsultLiveContract)' -count=1 ./internal/cli/ ./internal/runtime/
	@tmp="$$(mktemp)"; trap 'rm -f "$$tmp"' 0; go test -c -tags acpe2e -o "$$tmp" ./internal/acpproxy/

provider-live-e2e: ## Opt-in read-only upstream CLI probe (set COOP_LIVE_TARGETS=provider,...)
	@test -n "$$COOP_LIVE_TARGETS" || { echo 'COOP_LIVE_TARGETS is required (for example: codex,gemini@work)'; exit 2; }
	@go test -timeout 30m -tags providerlivee2e,cooplivetest -run '^TestProviderLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-live-e2e-all: ## Strict read-only upstream CLI probe for every registered provider
	@COOP_LIVE_TARGETS="$${COOP_LIVE_TARGETS:-all}" COOP_LIVE_REQUIRE_ALL=1 \
		go test -timeout 30m -tags providerlivee2e,cooplivetest -run '^TestProviderLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-consult-live-e2e: ## Opt-in four-provider real coop-consult probe (four peer CLI sessions)
	@test -n "$$COOP_LIVE_TARGETS" || { echo 'COOP_LIVE_TARGETS is required (claude,codex,gemini,grok in that order)'; exit 2; }
	@go test -timeout 30m -tags providerlivee2e,cooplivetest -run '^TestProviderConsultLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-consult-live-e2e-all: ## Strict real coop-consult probe for every provider
	@COOP_LIVE_TARGETS="$${COOP_LIVE_TARGETS:-all}" COOP_LIVE_REQUIRE_ALL=1 \
		go test -timeout 30m -tags providerlivee2e,cooplivetest -run '^TestProviderConsultLiveCompatibility$$' -count=1 -v ./internal/cli/

acp-scripted-e2e: ## Deterministic ACP process e2e (no runtime or provider credentials needed)
	@go test -run '^TestScriptedACP' -count=1 -v ./internal/acpproxy/

acp-e2e: ## Real ACP adapter e2e (isolated binary; needs a configured runtime, built box, and credentials)
	@COOP_ACP_LIVE_REQUIRE_ALL=1 go test -timeout 30m -tags acpe2e -run 'Test(LiveProviderConformance|LiveCrossProviderCarry|ForeignSessionLoadRejectsUnknownID|PresetOwnsSelectorState|CodexTargetRolloutTruth|FrontierStoredTargetTruth)$$' -count=1 -v ./internal/acpproxy/

review-writes-e2e: ## Review write-policy e2e (needs Docker; pulls a small test image once)
	@docker image inspect alpine:3.21 >/dev/null 2>&1 || docker pull alpine:3.21
	@go test -tags reviewwritee2e -run '^TestReviewWritesDockerRuntime$$' -count=1 -v ./internal/box/

clean: ## Remove build artifacts
	@rm -f coop
	@rm -rf dist

help: ## List targets
	@grep -hE '^[a-z][a-z0-9-]*:.*##' $(MAKEFILE_LIST) | sed -E 's/:.*## / — /' | sort

.PHONY: build install test cover lint snapshot doctor docs docs-check casts casts-check tools-test check provider-scripted-e2e live-process-control provider-live-e2e provider-live-e2e-all provider-consult-live-e2e provider-consult-live-e2e-all acp-scripted-e2e acp-e2e review-writes-e2e clean help
