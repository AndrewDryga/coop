# coop — see README.md
.DEFAULT_GOAL := help

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/AndrewDryga/coop/internal/cli.Version=$(VERSION)

# The gate tool pins. CI installs exactly these versions (it reads them from here), and the
# corresponding targets refuse any other build, so a laptop, a box, and CI cannot silently use
# different analyzers. Staticcheck's isolated module also pins its Go export-data importer.
# This repository's .agent/Dockerfile installs both for the gate in its box: Staticcheck from
# that module, the scanner at a pin mirrored there (agent_config_test.go holds them equal).
STATICCHECK_MODFILE := tools/staticcheck.mod
STATICCHECK_VERSION := $(shell awk '$$1 == "honnef.co/go/tools" {print $$2}' $(STATICCHECK_MODFILE))
STATICCHECK_XTOOLS_VERSION := $(shell awk '$$1 == "golang.org/x/tools" {print $$2}' $(STATICCHECK_MODFILE))
GOVULNCHECK_VERSION := v1.8.0

build: ## Build the coop binary to ./coop
	@go build -trimpath -ldflags "$(LDFLAGS)" -o coop .

install: ## Build from source and install to ~/.local/bin/coop
	@go build -trimpath -ldflags "$(LDFLAGS)" -o "$(HOME)/.local/bin/coop" .
	@echo "installed $(HOME)/.local/bin/coop ($(VERSION)) — run 'coop build' to build the box image"

test: require-git-lfs ## Run unit tests (no container runtime needed)
	@# -p 4 for the same reason as the race target: every package at once oversubscribes a laptop
	@# until fixture guards and production grace periods expire in tests that pass alone.
	@# Serial session Git/LFS fixtures exceeded Go's default 10 m package timeout under gate load.
	@# This 20 m bound guards a hung package, not operation deadlines; behavioral
	@# timing assertions remain tight, as in the race target.
	@go test -count=1 -p 4 -timeout 20m ./...

# The host programs a unit test may run without skipping, besides go. A test that needs any other
# program (jq, node, shellcheck, ssh-keygen, bash, zsh, expect, script, cloc, sha256sum, ...) skips
# and names it when it is missing; CONTRIBUTING.md lists both sets. `which`, not `command -v`,
# because true and false are shell builtins and the tests need the programs. CGO_ENABLED=0 builds
# the tests the way the release builds coop, which no C compiler on PATH would force anyway.
TEST_HOST_PROGRAMS := git git-lfs sh awk basename cat chmod cmp cp cut dd dirname env false find grep gzip head install ln mkdir mkfifo mktemp mv od ps rm rmdir sed sleep stat tail tar tee touch tr true uname wc

test-minimal-path: require-git-lfs ## Run unit tests with only TEST_HOST_PROGRAMS and go on PATH
	@bin="$$(mktemp -d)" && trap 'rm -rf "$$bin"' EXIT && ln -s "$$(go env GOROOT)/bin/go" "$$bin/go" && \
	for p in $(TEST_HOST_PROGRAMS); do \
	  src="$$(which "$$p")" || { echo "test-minimal-path: $$p is not installed"; exit 1; }; \
	  ln -s "$$src" "$$bin/$$p" || exit 1; \
	done && \
	CGO_ENABLED=0 PATH="$$bin" "$$bin/go" test -count=1 -p 4 -timeout 20m ./...

cover: require-git-lfs ## Run unit tests with a coverage summary
	@go test -cover ./...

mod-check: ## Verify module metadata is tidy without changing qualified sources
	@go mod tidy -diff

gateway-sources-check: ## Verify embedded network helper sources match the checkout
	@go test ./internal/gatewayimage -run '^TestEmbeddedGatewaySourcesExactlyMatchCheckout$$' -count=1

lint: ## gofmt check + go vet + Staticcheck at the pinned version, for Linux and macOS alike
	@files="$$(gofmt -l .)" || exit 1; [ -z "$$files" ] || { printf '%s\n' "$$files"; echo 'gofmt: files need formatting (run: gofmt -w .)'; exit 1; }
# Coop ships Linux and macOS and CI runs Linux, while this gate often runs on a Mac, where a
# *_linux.go file never compiles: a stale Linux-only test kept CI red for a week while every local
# gate passed. So each check below covers both platforms, whichever host runs it — with cgo off,
# which the tree never uses, so the other platform never needs a C toolchain.
	@for os in linux darwin; do CGO_ENABLED=0 GOOS=$$os go vet ./... || exit 1; done
# The e2e/live files sit behind build tags, so the pass above never compiles them — a lost cancel
# lived there unseen. Every tag in the tree in one pass.
	@for os in linux darwin; do CGO_ENABLED=0 GOOS=$$os go vet -tags acpe2e,boxruntimee2e,networkruntimee2e,providere2e,providerlivee2e,reviewwritee2e ./... || exit 1; done
	@command -v staticcheck >/dev/null 2>&1 || { echo "staticcheck is not installed — run: make install-staticcheck"; exit 1; }
	@staticcheck -version | grep -qF "($(patsubst v%,%,$(STATICCHECK_VERSION)))" || { echo "$$(staticcheck -version) is not the pinned $(STATICCHECK_VERSION) — run: make install-staticcheck"; exit 1; }
	@staticcheck -debug.version | awk '$$1 == "golang.org/x/tools@$(STATICCHECK_XTOOLS_VERSION)" {found=1} END {exit !found}' || { echo "staticcheck needs the pinned Go importer $(STATICCHECK_XTOOLS_VERSION) — run: make install-staticcheck"; exit 1; }
	@for os in linux darwin; do CGO_ENABLED=0 GOOS=$$os staticcheck ./... || exit 1; done

# Plumbing for CI's install step, which reads the pin from here instead of repeating it.
staticcheck-version:
	@echo $(STATICCHECK_VERSION)

install-staticcheck: ## Install the pinned analyzer and compatible Go importer
	@go install -modfile=$(STATICCHECK_MODFILE) -mod=readonly honnef.co/go/tools/cmd/staticcheck

govulncheck-version:
	@echo $(GOVULNCHECK_VERSION)

vuln: ## Report reachable Go vulnerabilities with the pinned govulncheck
	@command -v govulncheck >/dev/null 2>&1 || { echo "govulncheck is not installed — run: go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)"; exit 1; }
	@version="$$(govulncheck -version)" || { echo "govulncheck version check failed — run: go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)"; exit 1; }; \
		scanner="$$(printf '%s\n' "$$version" | sed -n 's/^Scanner: govulncheck@//p')"; \
		[ "$$scanner" = "$(GOVULNCHECK_VERSION)" ] || { echo "govulncheck@$${scanner:-unknown} is not the pinned $(GOVULNCHECK_VERSION) — run: go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)"; exit 1; }
	@govulncheck ./...

shellcheck: ## ShellCheck every tracked .sh: the installer, both sweep queue guards, the commit hook
	@command -v shellcheck >/dev/null 2>&1 || { echo "shellcheck is not installed — run: brew install shellcheck (macOS) or apt-get install -y shellcheck (Debian)"; exit 1; }
# The file list comes from git, so a new .sh is covered the moment it is tracked — no list here to
# forget to update. An empty list means git failed or the tree moved, never "nothing to check":
# that is the one way a lint silently stops running, so it fails closed.
	@git ls-files '*.sh' | grep -q . || { echo "shellcheck: no tracked .sh files found — refusing to pass by checking nothing"; exit 1; }
# An eval starter's `files/` are WORKLOADS, not Coop's shell: they contain the bug the case is about,
# so linting them would demand we fix the thing a model is supposed to find. Everything else, including
# every starter's verifier, is ours and is checked.
	@git ls-files -z '*.sh' | grep -zv '^internal/eval/starters/[^/]*/files/' | xargs -0 shellcheck

require-git-lfs:
	@command -v git-lfs >/dev/null 2>&1 || { echo "git-lfs is not installed — run: brew install git-lfs (macOS) or apt-get install -y git-lfs (Debian)"; exit 1; }

# Signing is intentionally skipped: release signatures are keyless (Sigstore via GitHub
# OIDC), which only exists in the release workflow — a local snapshot validates packaging.
snapshot: ## Build a local release snapshot with GoReleaser (no publish, no signing)
	@goreleaser release --snapshot --clean --skip=sign

doctor: build ## Integration check: prove isolation holds (needs a runtime)
	@./coop doctor

docs: ## Regenerate the CLI docs and manpage from internal/cli/help.go, and the site's pages from tools/site/
	@go run ./tools/gendocs

docs-check: ## Fail if the generated docs or site pages drifted (run 'make docs' to fix) or the site fails its content tests
	@go run ./tools/gendocs -check
	@go test -count=1 ./tools/gendocs

align: ## Check trailing-# comment alignment in README + site + CLI docs (run 'go run ./tools/aligncomments -write' to fix)
	@go run ./tools/aligncomments -check

bench-tools-test: ## Test the lifecycle and services benches (each bench runs this first)
	@go test -count=1 ./tools/lifecyclebench ./tools/servicesbench

# Deliberately OUT of `check`: the benches launch real boxes, so they need a container runtime and
# take minutes, and their numbers are a measurement to compare against — not a threshold to fail on.
# A wall-clock assertion in the gate fails on a busy laptop and says nothing about the change.
lifecycle-bench: build bench-tools-test ## Measure real box start/stop latency (needs a runtime; writes samples + a report)
	@go run ./tools/lifecyclebench --coop ./coop --repo $(or $(WORKSPACE),$(CURDIR)) \
	  --out $(or $(OUT),$(shell mktemp -d)/coop-lifecycle-bench) $(BENCH_ARGS)

services-bench: require-services-workspace build bench-tools-test ## Measure a warm start beside Compose services (needs a runtime; WORKSPACE=<project with approved services>)
	@go run ./tools/servicesbench --coop ./coop --repo $(WORKSPACE) \
	  --out $(or $(OUT),$(shell mktemp -d)/coop-services-bench) $(BENCH_ARGS)

require-services-workspace:
	@test -n "$(WORKSPACE)" || { echo 'WORKSPACE is required: a project whose Compose services are approved'; exit 2; }

rules-check: ## Fail if a .agent/kb card or rule is malformed, unindexed, or names a source/check that doesn't exist
	@go run ./tools/checkrules
	@go test -count=1 ./internal/agent -run 'TestProviderDecisionsStayInAdapters|TestProviderDecisionGuard'

build-all: ## Compile every package (a package no test imports can still break the build)
	@go build ./...

# internal/acpproxy is concurrent (the editor-reader goroutine and the main loop share
# p.mu-guarded state) — a data race there does not fail the plain `make test` run. Its own
# target so a race failure is legible; -race is ~2-3× slower, which is why its slowest package
# runs as two shards.
race: check-race-rest check-race-sessionsvc-1 check-race-sessionsvc-2 ## Full unit suite under the race detector (the slowest gate step)

# The race suite's long pole is internal/sessionsvc: 945 s of race time on a Mac, 557 s on CI, where
# it started 3.5 minutes into the job and finished last. It runs as two shards beside the rest.
# -run and -skip take the same pattern, so every test, example and fuzz seed is in exactly one
# shard, and a new or renamed test can only move between them, never out. The pattern halves the
# measured race time (2026-10-15: 475 s and 469 s); release_test.go keeps both sides non-empty.
SESSIONSVC := github.com/AndrewDryga/coop/internal/sessionsvc
SESSIONSVC_SHARD := ^Test(Session|Large|Replacement)

check-race-rest: require-git-lfs ## Race-detector tests of every package except internal/sessionsvc
	@# -p 4: the race detector multiplies each test binary's CPU, and running every package at
	@# once oversubscribes the host badly enough that production grace periods (a 3 s TERM wait
	@# before a fork's box is reaped) expire in tests that pass alone. Bounded parallelism, not a
	@# longer grace, is the fix: the gate must fail on a real regression, not on a busy laptop.
	@# Serial session Git/LFS fixtures take over 20 m with race instrumentation. The 30 m bound
	@# gives this whole-package hang guard contention headroom, not a longer per-operation deadline.
	@# Keep behavioral timing assertions tight.
	@pkgs="$$(go list ./...)" || exit 1; rest="$$(printf '%s\n' "$$pkgs" | grep -vx '$(SESSIONSVC)')"; \
	{ [ -n "$$rest" ] && [ "$$rest" != "$$pkgs" ]; } || { echo 'check-race-rest: go list must name $(SESSIONSVC) and other packages'; exit 1; }; \
	go test -count=1 -race -p 4 -timeout 30m $$rest

check-race-sessionsvc-1: require-git-lfs ## Race-detector tests of internal/sessionsvc that match the shard pattern
	@go test -count=1 -race -timeout 30m -run '$(SESSIONSVC_SHARD)' ./internal/sessionsvc

check-race-sessionsvc-2: require-git-lfs ## Race-detector tests of internal/sessionsvc outside the shard pattern
	@go test -count=1 -race -timeout 30m -skip '$(SESSIONSVC_SHARD)' ./internal/sessionsvc

# THE GATE. One Make-owned union of owners: CI runs each owner in CI_OWNERS on a runner of its
# own; `make check` runs the static owner first, then the test owners side by side. CI installs the
# pinned tools. A new check belongs HERE, never in the
# workflow's step list: the two were maintained separately, drifted in both directions (race and
# build were CI-only; cast/rules/tools checks were local-only), and main rotted red with no local
# gate able to see it. Ordered so the cheap and most common failures surface first. Required tools
# hard-fail with their install line — a soft skip is how a check silently stops running.
# Locally the race owners stand in for check-unit: they run the same packages and tests under the
# race detector, since no Go file has a race build constraint (release_test.go holds that), and
# the plain pass took a quarter of the 52-minute local gate. CI runs both. One part of check-unit
# has no local stand-in: it runs the tests with only TEST_HOST_PROGRAMS on PATH and CGO_ENABLED=0
# (test-minimal-path), which the race owners cannot do: on Linux -race needs cgo, and they keep the
# full PATH so the tests of optional programs still run somewhere. Run 'make test-minimal-path' by
# hand after a test starts using a program.
# CI-only by necessity: the doctor runtime matrix and the review-writes job need a real container
# runtime, so they stay separate CI jobs and this target stays runtime-independent. Run them by
# hand with 'make doctor', 'make box-runtime-e2e', and 'make review-writes-e2e'.
CI_OWNERS := check-static check-unit check-process-loop check-process-rest check-race-rest check-race-sessionsvc-1 check-race-sessionsvc-2
# The local test owners, longest first. Four run at once and the last starts when one ends, so at
# most check-race-rest's four packages and three single-package owners share the host (see
# check-race-rest for why the gate bounds its parallelism).
GATE_TEST_OWNERS := check-race-sessionsvc-1 check-race-sessionsvc-2 check-process-loop check-race-rest check-process-rest
# The gate builds with a cache of its own, which no other checkout or `go clean -cache` shares:
# one gate died when another session cleared the shared cache under it.
GATE_GOCACHE ?= $(HOME)/.cache/coop-gate/go-build

check: export GOCACHE := $(GATE_GOCACHE)
check: check-static ## Full free gate: the static owner, then the test owners side by side (CI also runs check-unit)
	@rm -rf .agent/gate && mkdir -p .agent/gate && echo '== $(GATE_TEST_OWNERS) side by side; logs in .agent/gate'
	@$(MAKE) --no-print-directory -j4 $(addprefix gate-,$(GATE_TEST_OWNERS))

# One test owner of the local gate, its output in a log of its own, so owners running side by side
# never interleave: a pass is one line, a failure is the log's tail.
gate-%: FORCE
	@mkdir -p .agent/gate; start=$$(date +%s); if $(MAKE) --no-print-directory $* > .agent/gate/$*.log 2>&1; then \
	  echo "elapsed $$(( $$(date +%s) - start )) s" >> .agent/gate/$*.log; echo 'ok   $*'; \
	else \
	  tail -n 60 .agent/gate/$*.log; echo 'FAIL $* (full log: .agent/gate/$*.log)'; exit 1; \
	fi

FORCE:

check-static: mod-check gateway-sources-check lint shellcheck build-all vuln align docs-check rules-check ## Free source, analyzer, build, docs and tool checks
check-unit: test-minimal-path ## Free ordinary unit tests, with only the host programs CONTRIBUTING.md requires
check-process: check-process-loop check-process-rest ## Free scripted-provider and process-control tests
check-process-loop: provider-scripted-loop-e2e ## The scripted provider suite's loop tests
check-process-rest: provider-scripted-other-e2e live-process-control ## The rest of the scripted provider suite and process-control tests
check-race: race ## Free race-detector tests

# Explicit reviewed owners, not an automatic changed-file router. The import-closure test
# includes tagged/test/root/tool consumers; shared foundations/build inputs still need check.
# LOCAL_* package lists are guarded by TestLocalCheckScopesCoverReverseConsumers.
LOCAL_agent := . ./internal ./internal/acpctl ./internal/acpproxy ./internal/acpproxy/testdata/acpfixture ./internal/agent ./internal/box ./internal/box/testdata/initprobe ./internal/box/testdata/mcpprobe ./internal/cli ./internal/cli/testdata/providerfixture ./internal/consult ./internal/forkctl ./internal/ladder ./internal/loop ./internal/loopcfg ./internal/preset ./internal/scaffold ./internal/sessionsvc ./internal/taskmcp ./internal/tasks ./internal/testutil/liveprovider ./tools/gendocs ./tools/lifecyclebench
LOCAL_runtime := . ./internal ./internal/acpctl ./internal/acpproxy ./internal/acpproxy/testdata/acpfixture ./internal/box ./internal/box/testdata/initprobe ./internal/box/testdata/mcpprobe ./internal/cli ./internal/cli/testdata/providerfixture ./internal/forkctl ./internal/gatewayimage ./internal/loop ./internal/runtime ./internal/scaffold ./internal/sessionsvc ./internal/taskmcp ./internal/tasks ./internal/testutil/liveprovider ./internal/testutil/nativedocker ./tools/gendocs ./tools/gennetimage ./tools/lifecyclebench
LOCAL_sessions := . ./internal ./internal/acpctl ./internal/acpproxy ./internal/acpproxy/testdata/acpfixture ./internal/cli ./internal/cli/testdata/providerfixture ./internal/forkctl ./internal/session ./internal/sessionsvc ./internal/workerconnector ./internal/workerproto ./tools/gendocs ./internal/box/testdata/mcpprobe
LOCAL_tasks := . ./internal ./internal/acpctl ./internal/acpproxy ./internal/acpproxy/testdata/acpfixture ./internal/box ./internal/box/testdata/initprobe ./internal/box/testdata/mcpprobe ./internal/cli ./internal/cli/testdata/providerfixture ./internal/forkctl ./internal/loop ./internal/scaffold ./internal/sessionsvc ./internal/taskmcp ./internal/tasks ./internal/testutil/liveprovider ./tools/gendocs ./tools/lifecyclebench
LOCAL_connector := . ./internal ./internal/acpctl ./internal/acpproxy ./internal/acpproxy/testdata/acpfixture ./internal/cli ./internal/cli/testdata/providerfixture ./internal/forkctl ./internal/sessionsvc ./internal/workerconnector ./tools/gendocs ./internal/box/testdata/mcpprobe
LOCAL_cli := . ./internal ./internal/acpctl ./internal/acpproxy ./internal/acpproxy/testdata/acpfixture ./internal/cli ./internal/cli/testdata/providerfixture ./tools/gendocs ./internal/box/testdata/mcpprobe

ITERATION_SCOPE := $(if $(filter 1,$(words $(SCOPE))),$(filter agent runtime sessions tasks connector cli,$(SCOPE)))
ifneq ($(ITERATION_SCOPE),)
check-local: require-git-lfs ## Scoped local checks; SCOPE=docs|rules|agent|runtime|sessions|tasks|connector|cli (unknown = broad)
	@echo 'Local checks: $(ITERATION_SCOPE) owner and reverse consumers; no race or live providers'
	@files="$$(gofmt -l .)" || exit 1; [ -z "$$files" ] || { printf '%s\n' "$$files"; echo 'gofmt: files need formatting'; exit 1; }
	@for os in linux darwin; do CGO_ENABLED=0 GOOS=$$os go vet $(LOCAL_$(ITERATION_SCOPE)) || exit 1; done
	@for os in linux darwin; do CGO_ENABLED=0 GOOS=$$os go vet -tags acpe2e,boxruntimee2e,networkruntimee2e,providere2e,providerlivee2e,reviewwritee2e $(LOCAL_$(ITERATION_SCOPE)) || exit 1; done
	@go test -count=1 -p 4 -timeout 20m $(LOCAL_$(ITERATION_SCOPE))
else ifeq ($(SCOPE),docs)
check-local: docs-check align rules-check
	@echo 'Local checks: docs, site content, alignment and rules'
else ifeq ($(SCOPE),rules)
check-local: rules-check
	@echo 'Local checks: rules'
else
check-local: check
	@echo 'Local checks: missing/unknown scope used the full free gate'
endif

provider-scripted-e2e: provider-scripted-loop-e2e provider-scripted-other-e2e ## Deterministic all-provider process e2e (no runtime or credentials needed)

# The suite runs ~11 minutes on a Mac and 4.5 on CI, so it runs as two shards, the loop tests (60%
# of its time) and the rest: -run and -skip take the same pattern, so each test is in exactly one.
# Go's 10-minute default failed the whole suite under ordinary load with every test passing. The
# 20-minute bound is a hang guard; the behavior deadlines live inside the tests.
PROVIDER_SHARD := ^TestProviderScriptedLoop

provider-scripted-loop-e2e: ## The scripted provider suite's loop tests
	@go test -tags providere2e -run '$(PROVIDER_SHARD)' -count=1 -timeout 20m -v ./internal/cli/

provider-scripted-other-e2e: ## The scripted provider suite's other tests and its fixtures
	@go test -count=1 ./internal/testutil/procharness ./internal/cli/testdata/providerfixture
	@go test -tags providere2e -run '^TestProviderScripted' -skip '$(PROVIDER_SHARD)' -count=1 -timeout 20m -v ./internal/cli/

# Selects every deterministic test in the providerlivee2e files; a test that needs a
# real provider is named …LiveCompatibility (its subprocess …LiveChild) and runs only on demand.
# live_selection_test.go fails when a tagged test is neither, its package is not listed here, or a
# prefix selects a paid test, so a rename or a new package cannot drop one or run one silently. Keep
# one go test line that runs tests; others may only compile (-c).
live-process-control: ## Deterministic tests of the tagged live-provider harness and contracts
	@go test -race -tags providerlivee2e -run '^Test(ProviderConsultLiveContract|ProviderLoopLiveContract|ProviderResumeLiveContract|ProviderNetworkLiveContract|ProviderLiveStreamContract|ProviderAccountsLiveContract)' -count=1 ./internal/cli/ ./internal/loop/
	@tmp="$$(mktemp)"; trap 'rm -f "$$tmp"' 0; go test -c -tags acpe2e -o "$$tmp" ./internal/acpproxy/

provider-live-e2e: ## Opt-in read-only upstream CLI probe (set COOP_LIVE_TARGETS=provider,...)
	@test -n "$$COOP_LIVE_TARGETS" || { echo 'COOP_LIVE_TARGETS is required (for example: codex,gemini@work)'; exit 2; }
	@go test -timeout 30m -tags providerlivee2e -run '^TestProviderLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-live-e2e-all: ## Strict read-only upstream CLI probe for every registered provider
	@COOP_LIVE_TARGETS="$${COOP_LIVE_TARGETS:-all}" COOP_LIVE_REQUIRE_ALL=1 \
		go test -timeout 30m -tags providerlivee2e -run '^TestProviderLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-resume-live-e2e: ## Opt-in two-process native session resume (set COOP_LIVE_TARGETS=provider,...)
	@test -n "$$COOP_LIVE_TARGETS" || { echo 'COOP_LIVE_TARGETS is required (for example: codex,gemini@work)'; exit 2; }
	@go test -timeout 30m -tags providerlivee2e -run '^TestProviderResumeLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-resume-live-e2e-all: ## Strict two-process native session resume for every provider
	@COOP_LIVE_TARGETS="$${COOP_LIVE_TARGETS:-all}" COOP_LIVE_REQUIRE_ALL=1 \
		go test -timeout 30m -tags providerlivee2e -run '^TestProviderResumeLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-network-live-e2e: ## Opt-in credentialed provider probe THROUGH the restricted gateway (set COOP_LIVE_TARGETS=provider,...)
	@test -n "$$COOP_LIVE_TARGETS" || { echo 'COOP_LIVE_TARGETS is required (for example: claude,codex@work)'; exit 2; }
	@go test -timeout 55m -tags providerlivee2e -run '^TestProviderNetworkLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-network-live-e2e-all: ## Strict filtered-egress provider probe for every registered provider
	@COOP_LIVE_TARGETS="$${COOP_LIVE_TARGETS:-all}" COOP_LIVE_REQUIRE_ALL=1 \
		go test -timeout 55m -tags providerlivee2e -run '^TestProviderNetworkLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-loop-live-e2e: ## Opt-in one-attempt live provider task completion (set COOP_LIVE_TARGETS=provider,...)
	@test -n "$$COOP_LIVE_TARGETS" || { echo 'COOP_LIVE_TARGETS is required (for example: codex,gemini@work)'; exit 2; }
	@go test -timeout 30m -tags providerlivee2e -run '^TestProviderLoopLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-loop-live-e2e-all: ## Strict one-attempt task completion for every registered provider
	@COOP_LIVE_TARGETS="$${COOP_LIVE_TARGETS:-all}" COOP_LIVE_REQUIRE_ALL=1 \
		go test -timeout 30m -tags providerlivee2e -run '^TestProviderLoopLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-consult-live-e2e: ## Opt-in four-provider real coop-consult probe (four peer CLI sessions)
	@test -n "$$COOP_LIVE_TARGETS" || { echo 'COOP_LIVE_TARGETS is required (claude,codex,gemini,grok in that order)'; exit 2; }
	@go test -timeout 30m -tags providerlivee2e -run '^TestProviderConsultLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-consult-live-e2e-all: ## Strict real coop-consult probe for every provider
	@COOP_LIVE_TARGETS="$${COOP_LIVE_TARGETS:-all}" COOP_LIVE_REQUIRE_ALL=1 \
		go test -timeout 30m -tags providerlivee2e -run '^TestProviderConsultLiveCompatibility$$' -count=1 -v ./internal/cli/

provider-accounts-live-e2e-all: ## Live model calls: real loop recovery for each provider with two configured accounts
	@COOP_LIVE_TARGETS=all go test -timeout 55m -tags providerlivee2e -run '^TestProviderAccountsLiveCompatibility$$' -count=1 -v ./internal/loop/

provider-delegate-live-e2e-all: ## Strict write-capable coop-delegate probe for every provider
	@COOP_LIVE_TARGETS="$${COOP_LIVE_TARGETS:-all}" COOP_LIVE_REQUIRE_ALL=1 \
		go test -timeout 30m -tags providerlivee2e -run '^TestProviderDelegateLiveCompatibility$$' -count=1 -v ./internal/cli/

acp-scripted-e2e: ## Deterministic ACP process e2e (no runtime or provider credentials needed)
	@go test -run '^TestScriptedACP' -count=1 -v ./internal/acpproxy/

# This target and the runtime ones below pick their acpe2e, reviewwritee2e, boxruntimee2e and
# networkruntimee2e tests by -run pattern. runtime_selection_test.go fails on such a test none of
# them runs (its file built by the target's -tags, its package listed, its name matched).
acp-e2e: ## Real ACP adapter e2e (isolated binary; needs a configured runtime, built box, and credentials)
	@COOP_ACP_LIVE_REQUIRE_ALL=1 go test -timeout 30m -tags acpe2e -run 'Test(LiveProviderConformance|LiveCrossProviderCarry|ForeignSessionLoadRejectsUnknownID|PresetOwnsSelectorState|CodexTargetRolloutTruth|FrontierStoredTargetTruth)$$' -count=1 -v ./internal/acpproxy/

review-writes-e2e: ## Review mount-isolation e2e (needs Docker; pulls a small test image once)
	@docker image inspect alpine:3.24.2 >/dev/null 2>&1 || docker pull alpine:3.24.2
	@go test -tags reviewwritee2e -run '^TestReviewWritesDockerRuntime$$' -count=1 -v ./internal/box/

native-roles-e2e: ## Offline role discovery for Codex/Gemini/Grok; Claude validates explicit role files (needs locked image)
	@go test -tags boxruntimee2e -run '^TestRuntimeNativeRolesAreCheckedByPinnedClients$$' -count=1 -v ./internal/box/

skills-e2e: ## Offline skill discovery for Codex/Gemini/Grok; Claude validates skill layout (needs locked image)
	@go test -tags boxruntimee2e -run '^TestRuntimeSharedSkillsAreCheckedByPinnedClients$$' -count=1 -v ./internal/box/

mcp-e2e: ## Offline shared MCP handshake for Codex/Gemini/Grok; Claude is live-only (needs locked image)
	@go test -tags boxruntimee2e -run '^TestRuntimeSharedMCPServersAreReachedByEveryProbeableClient$$' -count=1 -v ./internal/box/

box-runtime-e2e: ## Init/reaping, signal, and entrypoint descendant-supervision contracts (set COOP_RUNTIME=docker)
	@test -n "$$COOP_RUNTIME" || { echo 'COOP_RUNTIME is required (for example: COOP_RUNTIME=docker make box-runtime-e2e)'; exit 2; }
	@go test -tags boxruntimee2e -run '^TestRuntime(Init|Entrypoint|Compose)' -count=1 -v ./internal/box/

task-channel-e2e: ## The in-box task channel: a real box completes its own task and is refused a held one (needs the coop-box image)
	@go test -tags boxruntimee2e -run '^TestTaskChannelLiveInARealBox$$' -count=1 -v ./internal/box/

# The suite the published network support matrix rests on. It stays out of `check` because it needs
# Docker. Its fixture container has a fixed name, so two runs on one daemon break each other.
# Records go to a fresh owner-only folder or the one COOP_NETWORK_TRIAL_STATE names, never Coop's
# own store.
network-runtime-e2e: ## Filtered-network enforcement, transports and killed-run recovery (needs Docker; builds the image pair)
	@state="$${COOP_NETWORK_TRIAL_STATE:-$$(mktemp -d)}" || exit 1; echo "network trial records: $$state"; \
	COOP_NETWORK_TRIAL_STATE="$$state" go test -tags networkruntimee2e -run '^Test(RestrictedNetwork|KilledFilteredRun)' -count=1 -timeout 45m -v ./internal/box/

clean: ## Remove build artifacts
	@rm -f coop
	@rm -rf dist

help: ## List targets
	@grep -hE '^[a-z][a-z0-9-]*:.*##' $(MAKEFILE_LIST) | sed -E 's/:.*## / — /' | sort

.PHONY: build install test test-minimal-path cover lint staticcheck-version install-staticcheck govulncheck-version vuln shellcheck snapshot doctor docs docs-check align rules-check build-all race check provider-scripted-e2e live-process-control provider-live-e2e provider-live-e2e-all provider-resume-live-e2e provider-resume-live-e2e-all provider-network-live-e2e provider-network-live-e2e-all provider-loop-live-e2e provider-loop-live-e2e-all provider-consult-live-e2e provider-consult-live-e2e-all acp-scripted-e2e acp-e2e review-writes-e2e native-roles-e2e skills-e2e mcp-e2e box-runtime-e2e task-channel-e2e network-runtime-e2e clean help
.PHONY: provider-delegate-live-e2e-all provider-accounts-live-e2e-all
.PHONY: check-local bench-tools-test services-bench require-services-workspace
.PHONY: check-static check-unit check-process check-race
.PHONY: check-process-loop check-process-rest check-race-rest check-race-sessionsvc-1 check-race-sessionsvc-2
.PHONY: provider-scripted-loop-e2e provider-scripted-other-e2e FORCE
.PHONY: require-git-lfs
.PHONY: mod-check
.PHONY: gateway-sources-check
