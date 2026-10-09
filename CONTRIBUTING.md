# Contributing

To build co:op from source, clone the repo and run `make install`:

```bash
git clone https://github.com/AndrewDryga/coop && cd coop && make install
```

`make install` puts the binary at `~/.local/bin/coop`. `make check` is the gate, and CI runs the
same target.

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

## The gate: make check

CI's check job installs the pinned tools and runs `make check`, so the two can't drift. CI also
runs the doctor and review-writes jobs. They stay out of `make check` because they need a container
runtime.

The gate needs `staticcheck`, `govulncheck`, `shellcheck`, `python3` and `git-lfs`.
The Makefile pins `govulncheck` and installs Staticcheck with `make install-staticcheck` from
`internal/box/staticcheck.mod`. That separate dependency graph pins both the analyzer and its
Go importer without changing Coop's runtime dependencies. CI and the shipped images use the same
graph. A missing or wrong-version tool fails the gate and prints the command that installs it.
The gate never skips a check silently.

`.tool-versions` pins the exact Go toolchain, so an asdf user and co:op's own box get the
repository's required `go` and `gofmt` version automatically. CI reads the version from `go.mod`.

[AGENTS.md](AGENTS.md) is the contract every agent in this repo follows. One implemented task is
one commit. The commit message ends with a `Coop-Task: <id>` trailer, where the id is the task's
folder name.

## Test layers

The table below is the reference for a source checkout. It used to be appended to
`coop help --all`. The user manual now lists commands only.

| Layer | Targets | What they prove |
|---|---|---|
| Blocking | `make check` | formatting, vet, Staticcheck, ShellCheck, `go build ./...`, govulncheck, unit tests plain and under `-race`, deterministic provider process E2E, tagged process-control races, generated docs, rules cards, maintenance tools, and comment alignment; no runtime or credentials |
| Focused deterministic | `make provider-scripted-e2e` · `make acp-scripted-e2e` · `make live-process-control` | provider CLI/loop/fork policy, ACP switching/recovery, and live-harness ownership denials with fixtures |
| Runtime boundary | `make doctor` · `make box-runtime-e2e` · `make review-writes-e2e` | real box isolation, process reaping/signal forwarding, and report-only review mounts; requires Docker (or Apple `container` for doctor) |
| Upstream compatibility | provider live targets [below](#live-provider-suites) · `make acp-e2e` | installed CLIs plus isolated credentials; opt-in and quota-consuming |
| Full client qualification | `make provider-qualify` | operator-only image build, offline client probes and paid live suites; writes a version-pinned record only after all required checks pass |

The deterministic provider suite, `make provider-scripted-e2e`, crosses the real co:op
CLI/box/runtime boundary with strict provider-native fixture oracles for Claude, Codex, Gemini and
Grok. It owns target/account/model/effort propagation, all directed fallback pairs,
rate/auth/output failures, cancellation, loop audit stages, detached-fork lifecycle, session
lookup, telemetry and cleanup. Its fake runtime validates assembly. It does not validate container
enforcement: `make doctor` and `make review-writes-e2e` own that boundary. On failure, rerun the
printed subtest with `-v` and inspect its bounded, redacted trace.

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

## Qualifying clients

Run `make provider-qualify` only as an operator, after the ordinary gate and other engineering work
are finished. It rebuilds this host's box/filtered images and spends quota across every provider.
It prints a directory containing full suite logs. All suites must pass before it writes
`internal/agent/locked-clients/qualification.json`.

The schema-2 record names required suite/provider coverage, exact client pins and the tested
platform. It does not establish live macOS or Linux/arm64 parity. The final output names any
providers with unverified account recovery. Only that row may say `not_configured`, and an
unreadable account directory fails. Offline MCP (`make mcp-e2e`) covers Codex, Gemini and Grok.
The live network suite requires a shared MCP tool call from every provider.

`make check` rejects stale, malformed or incomplete existing records. An absent record leaves this
gate dormant. That does not imply live qualification, or parity between platforms or providers.

The [client qualification KB](.agent/kb/provider-client-qualification.md) covers the locked
clients and the schema-2 record in detail.

## Adding a provider

To add a provider, register its production `Agent`, implement the compiler-required
`LiveCredentials`, and add its independent native argv/output oracle. Registry completeness tests
fail until the scripted ACP/provider tables and help fragments acknowledge it. Strict `all`
includes it automatically.

The detailed fixture, credential-projection, process-ownership and session-history contracts live
in the [provider testing KB](.agent/kb/provider-scripted-e2e.md), the
[live testing KB](.agent/kb/provider-live-e2e.md) and the
[ACP testing KB](.agent/kb/acp-scripted-e2e.md).
