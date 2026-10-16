# Contributing

To build co:op from source, clone the repo and run `make install`:

```bash
git clone https://github.com/AndrewDryga/coop && cd coop && make install
```

`make install` puts the binary at `~/.local/bin/coop`. `make check` is the gate; CI runs its
Make-owned parts on separate runners.

## Layout

co:op is a single static Go binary plus a config folder. A repo you work on can also carry a
`.agent/Dockerfile` for its toolchain and a `.agent/compose.yml` for its services. The main
packages:

```
main.go               entrypoint
internal/agent/       one file per coding agent (claude/codex/gemini/grok): commands, resume, MCP, defaults, packages
internal/box/         the engine: secret-shadowing mounts, git env, image selection, container run
internal/acpproxy/    the ACP session proxy: survives box restarts, replays the handshake, coop's editor hooks
internal/consult/     read-only peer consultation instructions + wrapper
internal/preset/      orchestration presets (.agent/presets/<name>/preset.yaml): roles, ladders, routing
internal/project/     .agent/project.yaml — a monorepo's subprojects + the serve ports
internal/mcp/         one mcp.json → Claude / Codex / Gemini / Grok native configs (pure Go, no Python)
internal/session/     durable local remote sessions: idempotent operations, FIFO turns, events, recovery
internal/sessionsvc/  the remote-session service ON that store: immutable jobs, Unix API, isolated ACP, workspaces, review, publication
internal/scaffold/    `coop init` templates + the workflow skills (embedded in the binary)
internal/cli/         command dispatch, fork lifecycle, the loop + ACP control planes, doctor
internal/config·runtime·ui/   settings · runtime detection · terminal output
install.sh            the curl one-liner: download the prebuilt binary onto PATH
```

## Local checks and the broad gate

Use the smallest owner while editing:

```bash
make check-local SCOPE=rules     # KB cards and rules
make check-local SCOPE=docs      # docs, site content and comment alignment
make check-local SCOPE=agent     # adapters and their reverse consumers
make check-local SCOPE=runtime   # runtime and its reverse consumers
make check-local SCOPE=sessions  # session store/service and their reverse consumers
make check-local SCOPE=tasks     # internal/tasks and its reverse consumers
make check-local SCOPE=connector # worker connector and its reverse consumers
make check-local SCOPE=cli       # CLI, entrypoint and docs generator
```

These are explicit scopes, not an automatic changed-file selector. Use a focused
`go test ./internal/<owner> -run <TestName>` for a quick boundary check while editing.
Missing or unknown scope falls back to the full gate. Modules, toolchains, build tags,
shared foundations, subprocess fixtures and workflow changes need broad proof, not a guessed
owner. Prose-only checks do not run Docker, race or a provider process matrix.
The tasks scope is `internal/tasks`, not every task-related package: shared primitives such as
`taskstate`, `taskchannel`, `safefile`, `config` and process identity use the broad gate.

Run `make check` once on the completed executable candidate. Review can proceed while it runs;
do not repeat unchanged broad proof after every small edit. A pending check is not a pass.

`make check` runs the static owner first, then the test owners side by side, each into its own
log under `.agent/gate/` (a pass is one line, a failure shows the log's tail), with a Go build
cache of its own. It leaves out `check-unit`: the race owners run the same packages and tests
under the race detector. CI runs every owner in the Makefile's `CI_OWNERS` on its own runner,
`check-unit` included: the race suite's slowest package, `internal/sessionsvc`, and the scripted
provider suite each run as two shards whose `-run` and `-skip` share one pattern, so no test
falls outside both. Each main push, pull request and release tag runs all of them. The required
`check` status waits for every owner, doctor and review-writes; a failed, cancelled, skipped or
missing job cannot pass it. The two runtime jobs stay out of `make check` because they need Docker.

Read-only CI jobs cache downloaded modules and compiled Go objects, each free owner its own,
saved only by pushes to main, but behavioral tests always rerun: Go's test-result cache cannot
see every input that a child process builds or reads. The release publisher starts fresh without
that cache or imported artifacts.

Before GoReleaser publishes a tag, the release job runs `go run ./tools/releasenotes notes <tag>`.
It refuses a tag that is not the checked-out commit and prints the tag's finalized CHANGELOG.md
section as the release notes. `go run ./tools/releasenotes verify-remote <tag>` then refuses if
origin's tag changed since checkout. Run `notes` on a freshly tagged commit to preview the notes.

The gate needs `staticcheck`, `govulncheck`, `shellcheck`, `python3`, `node` and `git-lfs`.
Node runs the shipped site's picker and copy handlers in a small DOM fixture; it needs no
npm install or browser. Missing Node fails that owner instead of skipping the script checks.
The Makefile pins `govulncheck` and installs Staticcheck with `make install-staticcheck` from
`tools/staticcheck.mod`. That separate dependency graph pins both the analyzer and its
Go importer without changing Coop's runtime dependencies. CI and this repository's own box
(`.agent/Dockerfile`) install the same pins; Coop's box images ship neither tool. A missing or
wrong-version tool fails the gate and prints the command that installs it.
The gate never skips a check silently.

`.tool-versions` pins the exact Go toolchain, so an asdf user and co:op's own box get the
repository's required `go` and `gofmt` version automatically. CI reads the version from `go.mod`.

[AGENTS.md](AGENTS.md) is the contract every agent in this repo follows. One implemented task is
one commit. The commit message ends with a `Coop-Task: <id>` trailer, where the id is the task's
folder name.

## Test layers

These test layers apply to a source checkout.

| Layer | Targets | What they prove |
|---|---|---|
| Local iteration | `make check-local SCOPE=<owner>` | actual docs/rules or owner and reverse-consumer checks; unknown scope takes the broad path |
| Blocking | `make check` | formatting, vet, Staticcheck, ShellCheck, `go build ./...`, govulncheck, unit tests under `-race` (CI also runs them plain), deterministic provider process E2E, tagged process-control races, generated docs, rules cards, maintenance tools, and comment alignment; no runtime or credentials |
| Focused deterministic | `make provider-scripted-e2e` · `make acp-scripted-e2e` · `make live-process-control` | provider CLI/loop/fork policy, ACP switching/recovery, and live-harness contracts with fixtures |
| Runtime boundary | `make doctor` · `make box-runtime-e2e` · `make review-writes-e2e` | real box isolation, process reaping/signal forwarding, and report-only review mounts; requires Docker (or Apple `container` for doctor) |
| Pinned clients, offline | `make native-roles-e2e` · `make skills-e2e` · `make mcp-e2e` | each pinned client's own role, skill and MCP handling, with no network, in the locked image `coop net setup` builds; no credentials or model calls |
| Upstream compatibility | provider live targets [below](#live-provider-suites) · `make acp-e2e` | installed CLIs plus isolated credentials; opt-in and quota-consuming |

The deterministic provider suite, `make provider-scripted-e2e`, exercises the real co:op
CLI/box/runtime boundary with strict fixtures matching each provider's native commands
and output for Claude, Codex, Gemini and Grok. It tests target/account/model/effort
propagation, all directed fallback pairs, rate/auth/output failures, cancellation, loop
audit stages, detached-fork lifecycle, session lookup, telemetry and cleanup. Its fake
runtime validates assembly. It does not validate container enforcement: `make doctor`
and `make review-writes-e2e` own that boundary. On failure, rerun the printed subtest
with `-v` and inspect its bounded, redacted trace.

The security-critical logic is pure and unit-tested without a runtime: secret enumeration
(`internal/box/mounts.go`) and run-arg assembly (`internal/box/run.go`). `coop doctor` proves the
whole thing end-to-end against the real box.

## Live provider suites

The opt-in live layer checks compatibility with the provider CLIs currently installed in the box:

```bash
make provider-live-e2e COOP_LIVE_TARGETS='codex,gemini@work'
make provider-live-e2e COOP_LIVE_TARGETS='claude:opus/high@personal'
make provider-live-e2e-all
make provider-live-e2e-all COOP_LIVE_TARGETS='claude@backup,codex,gemini,grok'
make provider-resume-live-e2e COOP_LIVE_TARGETS='codex,gemini@work'
make provider-resume-live-e2e-all
make provider-loop-live-e2e COOP_LIVE_TARGETS='codex,gemini@work'
make provider-loop-live-e2e-all
make provider-consult-live-e2e COOP_LIVE_TARGETS='claude,codex,gemini,grok'
make provider-consult-live-e2e-all
make provider-delegate-live-e2e-all
make provider-network-live-e2e-all
make provider-accounts-live-e2e-all
```

An explicit list uses co:op's normal target grammar. `all` is registry-generated strict mode:
every provider must be attempted and pass, with no prerequisite skip. A complete registry-ordered
explicit list may select non-default accounts.

Once a provider request starts, auth errors, rate limits, timeouts, wrong output,
repository/source changes and incomplete cleanup all count as failures. Most probes make one
attempt. Resume and network have fixed stages, and account recovery permits exactly two work
launches. No suite deliberately exhausts quota.

| Live target | Model sessions / minimum calls | Stable evidence |
|---|---:|---|
| `provider-live-e2e` | 1 per admitted provider | `COOP_PROVIDER_LIVE_SUMMARY` |
| `provider-resume-live-e2e` | 2 per admitted provider | `COOP_PROVIDER_RESUME_LIVE_SUMMARY` |
| `provider-loop-live-e2e` | 1 writable task per admitted provider | `COOP_PROVIDER_LOOP_LIVE_SUMMARY` |
| `provider-consult-live-e2e` | 4 peer sessions for a complete ring; no lead sessions; tool use may add upstream turns | `COOP_CONSULT_LIVE_SUMMARY` |
| `provider-delegate-live-e2e-all` | 4 peer sessions; each must create exactly its requested file, without staging or committing | `COOP_DELEGATE_LIVE_SUMMARY` |
| `provider-network-live-e2e-all` | 3 stages per provider: prompt, native resume and a controlled shared MCP tool call through filtered networking | `COOP_PROVIDER_NETWORK_LIVE_SUMMARY` |
| `provider-accounts-live-e2e-all` | at most 2 work launches per configured compatible pair: rejected first account, successful second account | `COOP_PROVIDER_ACCOUNTS_LIVE_SUMMARY` |
| `acp-e2e` | scenario-dependent; several adapter generations | none (strict test output) |

Each run gets a disposable repository, HOME/XDG roots, access-only projected credentials, no
inherited instructions/MCP/session history, a hard deadline and bounded cleanup. Source
credentials are fingerprinted before and after. Repository checks match the journey: read-only
preservation, exact delegate output, or task-bound committed work. Raw provider output, paths,
accounts, tokens and refresh authority are never retained. These targets stay outside
`make check` because every admitted provider consumes real quota.

A suite's summary line starts with its stable evidence prefix and reports one of these results:

| Summary result | Action |
|---|---|
| `missing_runtime`, `missing_image`, `missing_cli`, `missing_credential` | Install/build/sign in, then rerun. No paid request started. |
| `credential_refresh_required` | Re-authenticate the selected account. Its projected access token cannot outlive the deadline. |
| `credential_not_portable` | Select a portable provider credential. For Gemini live probes, use a `GEMINI_API_KEY` account stored by co:op or backed by an explicit env var. Live suites automatically use filtered networking for supported API keys. They refuse `GOOGLE_API_KEY` even with open networking. |
| `not_configured` | Account recovery only: fewer than two accounts are configured for that provider. No recovery claim is made. |
| `ring_prerequisite` | Repair the named prerequisite. The consult ring admitted zero paid calls. |
| `failed` with `attempted=true` | Treat as an upstream CLI/provider compatibility failure. Reproduce syntax/policy with the deterministic fixture. |
| `repository_changed`, `source_changed`, `cleanup_failed`, `harness_failed` | Treat as a local isolation/harness defect. These override provider success. |

Account recovery, `make provider-accounts-live-e2e-all`, runs the real loop controller on one
task, with preflight/review/peers disabled and a hard two-work-launch cap. It selects two distinct
configured accounts of the same credential family and proves both copies are portable. Then it
replaces only the first copy with invalid synthetic credentials. The source accounts are neither
changed nor refreshed. Success requires a recorded authentication failure on the first account,
successful task completion on the second and one exact task-bound commit. Fewer than two
configured accounts is reported as `not_configured`, which is not a pass. An unsafe, expired or
incompatible configured pair fails. Selection is deterministic, in account-name order, and it is
not an exhaustive account matrix. Provider tool use may require multiple upstream turns.

`make acp-e2e` applies the same credential and runtime boundary to installed ACP adapters and
fails on every skip. It has no stable summary prefix on purpose. Use `make acp-scripted-e2e` for
injected limits, malformed responses, switching, fallback and replay cases that should not spend
live quota.

## Checking the pinned clients

Run the offline client probes before any live suite: they call no model, so a red there costs
nothing. Offline MCP (`make mcp-e2e`) covers Codex, Gemini and Grok. Claude's shared MCP route is
proven only by the live network suite, which requires a shared MCP tool call from every provider.

After a client pin moves, run the smallest affected live suite with its scope and billing clear.
Clear `COOP_IMAGE` and `COOP_BASE_IMAGE` (in the shell and in coop.conf) first: an open-network suite
runs whatever image they name, so it would not test Coop's own pinned clients.
No report is saved: a live run proves that run only, and a missing one stays unverified, never
waived. Builds, CI and releases make no model calls and never wait for a live run.

The [client qualification KB](.agent/kb/provider-client-qualification.md) covers the locked
clients, the conformance rows and the client bump procedure.

## Adding a provider

To add a provider, register its production `Agent`, implement the compiler-required
`LiveCredentials`, and add its independent native argv/output oracle. Registry completeness tests
fail until the scripted ACP/provider tables and help fragments acknowledge it. Strict `all`
includes it automatically.

The detailed fixture, credential-projection, process-ownership and session-history contracts live
in the [provider testing KB](.agent/kb/provider-scripted-e2e.md), the
[live testing KB](.agent/kb/provider-live-e2e.md) and the
[ACP testing KB](.agent/kb/acp-scripted-e2e.md).
