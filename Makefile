# coop — see README.md
.DEFAULT_GOAL := help

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/AndrewDryga/coop/internal/cli.Version=$(VERSION)

# The ONE Staticcheck pin. CI installs exactly this version (it reads it from here, via
# 'make -s staticcheck-version') and 'make lint' refuses any other build, so a laptop, a box,
# and CI can never lint with three different rule sets. Bump it here AND in internal/box/image.go
# (the box ships the same binary for the in-box gate); a test in internal/box holds the two together.
STATICCHECK_VERSION := v0.7.0

build: ## Build the coop binary to ./coop
	@go build -trimpath -ldflags "$(LDFLAGS)" -o coop .

install: ## Build from source and install to ~/.local/bin/coop
	@go build -trimpath -ldflags "$(LDFLAGS)" -o "$(HOME)/.local/bin/coop" .
	@echo "installed $(HOME)/.local/bin/coop ($(VERSION)) — run 'coop build' to build the box image"

test: ## Run unit tests (no container runtime needed)
	@go test ./...

cover: ## Run unit tests with a coverage summary
	@go test -cover ./...

lint: ## gofmt check + go vet + Staticcheck at the pinned version
	@gofmt -l . | (! grep .) || { echo "gofmt: files need formatting (run: gofmt -w .)"; exit 1; }
	@go vet ./...
	@command -v staticcheck >/dev/null 2>&1 || { echo "staticcheck is not installed — run: go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)"; exit 1; }
	@staticcheck -version | grep -qF "($(STATICCHECK_VERSION))" || { echo "$$(staticcheck -version) is not the pinned $(STATICCHECK_VERSION) — run: go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)"; exit 1; }
	@staticcheck ./...

# Plumbing for CI's install step, which reads the pin from here instead of repeating it.
staticcheck-version:
	@echo $(STATICCHECK_VERSION)

shellcheck: ## ShellCheck install.sh (the curl one-liner every new user runs)
	@command -v shellcheck >/dev/null 2>&1 || { echo "shellcheck is not installed — run: brew install shellcheck (macOS) or apt-get install -y shellcheck (Debian)"; exit 1; }
	@shellcheck install.sh

# Guard for the python-backed targets: name the fix instead of leaving make to print a bare
# "python3: No such file or directory". No ## — it's a prerequisite, not something you run.
require-python3:
	@command -v python3 >/dev/null 2>&1 || { echo "python3 is not installed — run: brew install python3 (macOS) or apt-get install -y python3 (Debian)"; exit 1; }

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

align: require-python3 ## Check trailing-# comment alignment in README + site + CLI docs (--write to fix)
	@python3 tools/align-comments.py --check

casts: build require-python3 ## Regenerate + safety-check site terminal casts (refuses a dirty/untagged ./coop; needs python3)
	@python3 tools/gen_casts.py
	@python3 tools/cast_hygiene.py site/casts

casts-check: require-python3 ## Validate published casts for private paths, credentials, and secret-shaped values
	@python3 tools/cast_hygiene.py site/casts

tools-test: require-python3 ## Run standard-library tests for repository maintenance tools
	@python3 -m unittest discover -s tools -p 'test_*.py'

rules-check: require-python3 ## Fail if a .agent/rules card is malformed, unindexed, or names a source/check that doesn't exist
	@python3 tools/check_rules.py

build-all: ## Compile every package (a package no test imports can still break the build)
	@go build ./...

# internal/acpproxy is concurrent (the editor-reader goroutine and the main loop share
# p.mu-guarded state) — a data race there does not fail the plain `make test` run. Its own
# target so a race failure is legible; -race is ~2-3× slower, which is why it runs last.
race: ## Full unit suite under the race detector (the slowest gate step)
	@go test -race ./...

# THE GATE. One recipe, run identically on a laptop, in a box, and by CI's check job — which
# installs the pinned tools and then calls this target. A new check belongs HERE, never in the
# workflow's step list: the two were maintained separately, drifted in both directions (race and
# build were CI-only; cast/rules/tools checks were local-only), and main rotted red with no local
# gate able to see it. Ordered so the cheap and most common failures surface first and the race
# suite runs last. Required tools hard-fail with their install line — a soft skip is how a check
# silently stops running.
# CI-only by necessity: the doctor runtime matrix and the review-writes job need a real container
# runtime, so they stay separate CI jobs and this target stays runtime-independent. Run them by
# hand with 'make doctor', 'make box-runtime-e2e', and 'make review-writes-e2e'.
check: lint shellcheck build-all align docs-check casts-check tools-test rules-check test provider-scripted-e2e live-process-control race ## The gate, identical to CI's check job: lint + freshness + tests (plain, e2e, race) + build

provider-scripted-e2e: ## Deterministic all-provider process e2e (no runtime or credentials needed)
	@go test ./internal/testutil/procharness ./internal/cli/testdata/providerfixture
	@go test -tags providere2e -run '^TestProviderScripted' -count=1 -v ./internal/cli/

live-process-control: ## Deterministic denial tests for tagged live-test process ownership
	@go test -race -tags providerlivee2e,cooplivetest -run '^Test(LiveACPProcess|LiveInterruptible|LiveRunInterruptible|ProviderConsultLiveContract|ProviderLoopLiveContract|ProviderResumeLiveContract)' -count=1 ./internal/cli/ ./internal/runtime/
	@tmp="$$(mktemp)"; trap 'rm -f "$$tmp"' 0; go test -c -tags acpe2e -o "$$tmp" ./internal/acpproxy/

provider-live-e2e: ## Opt-in read-only upstream CLI probe (set COOP_LIVE_TARGETS=provider,...)
	@test -n "$$COOP_LIVE_TARGETS" || { echo 'COOP_LIVE_TARGETS is required (for example: codex,gemini@work)'; exit 2; }
	@go test -timeout 30m -tags providerlivee2e,cooplivetest -run '^TestProviderLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-live-e2e-all: ## Strict read-only upstream CLI probe for every registered provider
	@COOP_LIVE_TARGETS="$${COOP_LIVE_TARGETS:-all}" COOP_LIVE_REQUIRE_ALL=1 \
		go test -timeout 30m -tags providerlivee2e,cooplivetest -run '^TestProviderLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-resume-live-e2e: ## Opt-in two-process native session resume (set COOP_LIVE_TARGETS=provider,...)
	@test -n "$$COOP_LIVE_TARGETS" || { echo 'COOP_LIVE_TARGETS is required (for example: codex,gemini@work)'; exit 2; }
	@go test -timeout 30m -tags providerlivee2e,cooplivetest -run '^TestProviderResumeLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-resume-live-e2e-all: ## Strict two-process native session resume for every provider
	@COOP_LIVE_TARGETS="$${COOP_LIVE_TARGETS:-all}" COOP_LIVE_REQUIRE_ALL=1 \
		go test -timeout 30m -tags providerlivee2e,cooplivetest -run '^TestProviderResumeLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-loop-live-e2e: ## Opt-in one-attempt live provider task completion (set COOP_LIVE_TARGETS=provider,...)
	@test -n "$$COOP_LIVE_TARGETS" || { echo 'COOP_LIVE_TARGETS is required (for example: codex,gemini@work)'; exit 2; }
	@go test -timeout 30m -tags providerlivee2e,cooplivetest -run '^TestProviderLoopLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-loop-live-e2e-all: ## Strict one-attempt task completion for every registered provider
	@COOP_LIVE_TARGETS="$${COOP_LIVE_TARGETS:-all}" COOP_LIVE_REQUIRE_ALL=1 \
		go test -timeout 30m -tags providerlivee2e,cooplivetest -run '^TestProviderLoopLiveCompatibility$$' -count=1 -v ./internal/cli/

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

review-writes-e2e: ## Review mount-isolation e2e (needs Docker; pulls a small test image once)
	@docker image inspect alpine:3.21 >/dev/null 2>&1 || docker pull alpine:3.21
	@go test -tags reviewwritee2e -run '^TestReviewWritesDockerRuntime$$' -count=1 -v ./internal/box/

box-runtime-e2e: ## Init/reaping, signal, and entrypoint descendant-supervision contracts (set COOP_RUNTIME=docker or podman)
	@test -n "$$COOP_RUNTIME" || { echo 'COOP_RUNTIME is required (for example: COOP_RUNTIME=docker make box-runtime-e2e)'; exit 2; }
	@go test -tags boxruntimee2e -run '^TestRuntime(Init|Entrypoint)' -count=1 -v ./internal/box/

clean: ## Remove build artifacts
	@rm -f coop
	@rm -rf dist

help: ## List targets
	@grep -hE '^[a-z][a-z0-9-]*:.*##' $(MAKEFILE_LIST) | sed -E 's/:.*## / — /' | sort

.PHONY: build install test cover lint staticcheck-version shellcheck require-python3 snapshot doctor docs docs-check align casts casts-check tools-test rules-check build-all race check provider-scripted-e2e live-process-control provider-live-e2e provider-live-e2e-all provider-resume-live-e2e provider-resume-live-e2e-all provider-loop-live-e2e provider-loop-live-e2e-all provider-consult-live-e2e provider-consult-live-e2e-all acp-scripted-e2e acp-e2e review-writes-e2e box-runtime-e2e clean help
