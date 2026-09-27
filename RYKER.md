# RYKER.md

Written by Ryker from `95be549` on 2026-09-27.

## Purpose

Coop is a Go command-line tool for developers running coding agents against Git repositories. It runs Claude, Codex, Gemini, and Grok in disposable containers with secrets hidden, and supports interactive work, unattended task queues, isolated forks, editor integration, and remote sessions.

## Components

- [main.go](main.go) — Starts the coop CLI through internal/cli.
- [internal/cli/](internal/cli/) — Defines commands, help, and integration with the execution subsystems.
- [internal/agent/](internal/agent/) — Holds provider adapters, authentication behavior, native command formats, and locked client dependencies.
- [internal/box/](internal/box/) — Builds sandbox images and assembles container execution, secret masking, credentials, and mounts.
- [internal/runtime/](internal/runtime/) — Implements container runtime detection and operations.
- [internal/loop/](internal/loop/) — Runs unattended task iterations, provider rotation, progress reporting, review, and signoff.
- [internal/tasks/](internal/tasks/) — Manages the folder-based task queue and its lifecycle.
- [internal/forkctl/](internal/forkctl/) — Controls isolated fork lifecycle, review, and landing changes.
- [internal/acpproxy/](internal/acpproxy/) — Maintains editor ACP sessions across provider switches and box restarts.
- [internal/session/](internal/session/) — Stores durable remote sessions, operations, turns, and events.
- [internal/sessionsvc/](internal/sessionsvc/) — Exposes the local session service over a Unix socket and manages policy, workspaces, execution, and review.
- [internal/workerconnector/](internal/workerconnector/) — Connects a worker to an external controller using outbound mutual TLS and durable command receipts.
- [internal/workerproto/](internal/workerproto/) — Defines worker protocol messages, checkpoints, storage reporting, and session evidence.
- [internal/networkgateway/](internal/networkgateway/) — Enforces restricted network access through gateway controller and guard processes.
- [cmd/coop-net/](cmd/coop-net/) — Provides the executable used inside the trusted gateway image.
- [internal/preset/](internal/preset/) — Loads orchestration presets and renders provider roles and wrappers.
- [internal/mcp/](internal/mcp/) — Translates shared MCP configuration into provider-native configuration.
- [internal/scaffold/](internal/scaffold/) — Embeds the project templates and workflow skills installed by coop init.
- [internal/project/](internal/project/) — Reads project configuration, including subprojects and exposed development ports.
- [internal/ui/](internal/ui/) — Provides terminal formatting, prompts, and live output.
- [docs/](docs/) — Contains generated CLI and man-page references plus networking and session API documentation.
- [site/](site/) — Contains the published static website, documentation, and terminal recordings.
- [brand/](brand/) — Contains website design guidance and visual specimens; it is not published by the Pages workflow.
- [tools/](tools/) — Contains documentation generators, repository checks, release validation, benchmarks, and provider qualification tools.
- [testdata/protocol/](testdata/protocol/) — Contains worker and workspace checkpoint protocol fixtures.
- [.agent/kb/](.agent/kb/) — Indexes subsystem knowledge and contributor rules used when implementing and reviewing changes.
- [.github/workflows/](.github/workflows/) — Defines CI, tagged binary releases, and website deployment.
- [install.sh](install.sh) — Downloads and verifies a platform-specific release and installs the coop binary.

## Build, test and run

- `go install honnef.co/go/tools/cmd/staticcheck@"$(make -s staticcheck-version)"` — Installs Staticcheck at the version required by the Makefile. From [.github/workflows/ci.yml](.github/workflows/ci.yml).
- `go install golang.org/x/vuln/cmd/govulncheck@"$(make -s govulncheck-version)"` — Installs the pinned Go vulnerability scanner. From [.github/workflows/ci.yml](.github/workflows/ci.yml).
- `make build` — Builds ./coop with a version derived from Git; the repository pins Go 1.26.6. From [Makefile](Makefile).
- `make install` — Builds and installs coop into ~/.local/bin. From [Makefile](Makefile).
- `make check` — Runs the shared local and CI gate: lint, build, vulnerability scan, generated-file checks, tools, unit tests, deterministic process tests, and races. Requires Python 3, ShellCheck, and pinned Go analyzers; no container runtime or credentials. From [Makefile](Makefile).
- `make test` — Runs all Go unit tests with bounded package parallelism and no container runtime. From [Makefile](Makefile).
- `make race` — Runs the unit suite under the Go race detector. From [Makefile](Makefile).
- `make lint` — Checks gofmt and runs vet and pinned Staticcheck for Linux and macOS. From [Makefile](Makefile).
- `make provider-scripted-e2e` — Tests provider process behavior through deterministic fixtures without credentials or a container runtime. From [Makefile](Makefile).
- `make acp-scripted-e2e` — Runs deterministic ACP process tests without a runtime or provider credentials. From [Makefile](Makefile).
- `make doctor` — Builds coop and checks sandbox isolation against a real container runtime. From [Makefile](Makefile).
- `make box-runtime-e2e` — Tests container process supervision and signal handling; requires COOP_RUNTIME to be set. From [Makefile](Makefile).
- `make review-writes-e2e` — Tests read-only review mounts against Docker, pulling Alpine if needed. From [Makefile](Makefile).
- `make provider-qualify` — Rebuilds images and runs strict live suites to qualify locked clients; requires provider access and consumes quota. From [Makefile](Makefile).
- `make docs` — Regenerates CLI documentation from internal/cli help definitions. From [Makefile](Makefile).
- `make docs-check` — Checks that committed generated CLI documentation is current. From [Makefile](Makefile).
- `make casts` — Regenerates website terminal recordings and checks them for sensitive content. From [Makefile](Makefile).
- `make rules-check` — Validates knowledge cards and rule references, then checks provider adapter boundaries. From [Makefile](Makefile).
- `make snapshot` — Builds local release packages using GoReleaser without publishing or signing. From [Makefile](Makefile).
- `cd site && python3 -m http.server 8000` — Previews the static website locally, including fetched terminal recordings. From [site/README.md](site/README.md).

## Deploy and release

- Prepare a release on clean, current main with make check passing and real code changes represented in CHANGELOG.md's Unreleased section; optionally validate packaging with make snapshot. From [.agent/skills/release/SKILL.md](.agent/skills/release/SKILL.md).
- Choose the semantic version, finalize the changelog under a bare version heading, commit it, and create an annotated v-prefixed tag on that commit. Reopen Unreleased in a separate commit. From [.agent/skills/release/SKILL.md](.agent/skills/release/SKILL.md).
- Obtain explicit confirmation before pushing main and the release tag; the tag push publishes a public release. From [.agent/skills/release/SKILL.md](.agent/skills/release/SKILL.md).
- A v* tag triggers exact-commit CI qualification, finalized release-note validation, and a remote tag identity check before publication. From [.github/workflows/release.yml](.github/workflows/release.yml).
- GoReleaser builds static Linux and macOS binaries for amd64 and arm64, packages documentation and completions, and publishes archives with cosign-signed checksums. From [.goreleaser.yaml](.goreleaser.yaml).
- The release workflow adds build-provenance attestations to the archives; verify the completed workflow and published assets before considering the release shipped. From [.github/workflows/release.yml](.github/workflows/release.yml).
- Website changes on main, or a manual workflow dispatch, upload site directly to GitHub Pages with no build step. Repository Pages settings must use GitHub Actions. From [.github/workflows/pages.yml](.github/workflows/pages.yml).

## Conventions

- Prefer simple, established designs and match surrounding style; assess product scope, usability, security, and maintainability before coding. From [AGENTS.md](AGENTS.md).
- Root-cause bugs before fixing them; do not remove or degrade a feature as a substitute for a fix without approval. From [AGENTS.md](AGENTS.md).
- Finish with formatting, a green gate, and failure-path verification; state any checks that could not run. From [AGENTS.md](AGENTS.md).
- Read the task queue and knowledge indexes first, then open subsystem cards and rules relevant to the change. From [AGENTS.md](AGENTS.md).
- Plan multi-file changes and review substantial work using the repository's workflow skills. From [AGENTS.md](AGENTS.md).
- Claim a task before implementation; keep its plan and resume state self-contained, and mark it done only after verification and commit. From [AGENTS.md](AGENTS.md).
- Make one commit per implemented task and end its message with a Coop-Task: <id> trailer. From [AGENTS.md](AGENTS.md).
- Stay on the assigned branch unless explicitly asked to change it. From [AGENTS.md](AGENTS.md).
- Keep writes serialized in a shared checkout; use other agents as read-only advisors unless they have isolated workspaces. From [AGENTS.md](AGENTS.md).
- Keep changes on topic; queue separate small fixes and reserve the backlog for large or unscoped work. From [AGENTS.md](AGENTS.md).
- Update relevant knowledge cards in the same commit, and turn human corrections into rules checked against sibling code. From [AGENTS.md](AGENTS.md).
- Keep long-running output static and bounded, preserve full logs, and retain the command's exit status. From [AGENTS.md](AGENTS.md).

## Where to look

- Add or change a CLI command and its generated documentation: [internal/cli/help.go](internal/cli/help.go)
- Add a provider or change authentication, command arguments, or resume behavior: [internal/agent/](internal/agent/)
- Update bundled provider client versions: [internal/agent/locked-clients/package.json](internal/agent/locked-clients/package.json)
- Change sandbox mounts, secret masking, or image construction: [internal/box/](internal/box/)
- Change unattended task execution or review behavior: [internal/loop/](internal/loop/)
- Change task queue operations: [internal/tasks/](internal/tasks/)
- Change fork review or merging: [internal/forkctl/](internal/forkctl/)
- Debug editor session switching and recovery: [internal/acpproxy/](internal/acpproxy/)
- Integrate with the session service or remote worker: [docs/session-api.md](docs/session-api.md)
- Understand filtered network setup and supported rules: [docs/networking.md](docs/networking.md)
- Change the files generated by coop init: [internal/scaffold/templates/](internal/scaffold/templates/)
- Add a check to the shared contributor and CI gate: [Makefile](Makefile)
- Find deterministic provider test contracts: [.agent/kb/provider-scripted-e2e.md](.agent/kb/provider-scripted-e2e.md)
- Edit the published website: [site/README.md](site/README.md)
- Find website design guidance before changing its appearance: [brand/DESIGN.md](brand/DESIGN.md)
- Prepare and publish a binary release: [.agent/skills/release/SKILL.md](.agent/skills/release/SKILL.md)

## Open questions

- The provider qualification knowledge card records unresolved live-suite gaps and no enforced qualification record. Confirm the accepted qualification evidence before updating bundled client versions.
