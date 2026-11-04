# Contributing

To build co:op from source, clone the repo and run `make install`:

```bash
git clone https://github.com/AndrewDryga/coop && cd coop && make install
```

`make install` puts the binary at `~/.local/bin/coop`. `make check` is the gate. CI runs the same
checks from the Makefile on separate runners.

## Layout

co:op is a single static Go binary plus a config folder. The main packages:

```
main.go               entrypoint
internal/agent/       one file per coding agent (claude/codex/gemini/grok): commands, resume, MCP, defaults, packages
internal/box/         the engine: secret-shadowing mounts, git env, image selection, container run
internal/acpproxy/    the ACP session proxy: survives box restarts, replays the handshake, coop's editor hooks
internal/consult/     read-only peer consultation instructions + wrapper
internal/preset/      orchestration presets (.agent/presets/<name>/preset.yaml): roles, ladders, routing
internal/project/     .agent/project.yaml — a monorepo's subprojects + the serve ports
internal/mcp/         one mcp.json → Claude / Codex / Gemini / Grok native configs
internal/session/     durable local remote sessions: idempotent operations, FIFO turns, events, recovery
internal/sessionsvc/  the remote-session service ON that store: immutable jobs, Unix API, isolated ACP, workspaces, review, publication
internal/scaffold/    `coop init` templates + the workflow skills (embedded in the binary)
internal/cli/         command dispatch, fork lifecycle, the loop + ACP control planes, doctor
internal/config·runtime·ui/   settings · runtime detection · terminal output
install.sh            the curl one-liner: download the prebuilt binary onto PATH
```

## Local checks and the broad gate

While you edit, run the smallest owner that covers your change:

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

Pick the scope yourself. The Makefile doesn't infer it from your changes. For a quick boundary
check, use a focused `go test ./internal/<owner> -run <TestName>`.

A missing or unknown scope falls back to the full gate. Changes to modules, toolchains, build
tags, shared foundations, subprocess fixtures and workflows need the broad gate, so don't guess an
owner for them.

The tasks scope is `internal/tasks`, not every task-related package. Shared primitives such as
`taskstate`, `taskchannel`, `safefile`, `config` and process identity use the broad gate.

Run `make check` once, when your code change is complete, not after every small edit. Review can
continue while it runs, but a check that's still pending isn't a pass.

`make check` runs the static owner first, then the test owners side by side, each writing its own
log under `.agent/gate/`. A pass prints one line, and a failure prints the log's tail.

`make check` leaves out `check-unit`, because the race owners run the same packages and tests under
the race detector. CI runs every owner in the Makefile's `CI_OWNERS` on its own runner, including
`check-unit`, on every push to main, pull request and release tag. The required `check` status waits
for every owner, doctor and review-writes, and a failed, cancelled, skipped or missing job can't
pass it. The doctor and review-writes jobs stay out of `make check` because they need Docker.

Read-only CI jobs cache downloaded modules and compiled Go objects, and only pushes to main save to
that cache. Behavioral tests always rerun. The release publisher starts fresh, without that cache or
imported artifacts.

Before GoReleaser publishes a tag, the release job runs `go run ./tools/releasenotes notes <tag>`.
It refuses a tag that isn't the checked-out commit or whose CHANGELOG.md section isn't finalized.
Otherwise it prints the tag's section of `.github/release-notes.md`, the short summary the release
commit carries, as the release notes. Then `go run ./tools/releasenotes verify-remote <tag>` refuses
if origin's tag changed since checkout. To preview the notes, run `notes` on a freshly tagged
commit.

The unit tests need Go, `git`, `git-lfs`, a POSIX `sh` and the standard utilities listed in the
Makefile's `TEST_HOST_PROGRAMS`, which macOS and Linux ship. Some tests also use `jq`, `node`,
`shellcheck`, `ssh-keygen`, `bash`, `zsh`, `expect`, `script`, `flock`, `cloc`, `sha256sum` or
`shasum` when they're installed. If one is missing, the test skips and names the program.

CI's `check-unit` runs the tests with only the required programs on PATH, so a test that needs
another program and doesn't skip fails there. The race owners can't repeat that locally, so run
`make test-minimal-path` by hand when a test starts using a program.

The gate needs `staticcheck`, `govulncheck`, `shellcheck` and `git-lfs`. The Makefile pins
`govulncheck` and installs Staticcheck with `make install-staticcheck` from
`tools/staticcheck.mod`. CI and this repository's own box (`.agent/Dockerfile`) install the same
pins. co:op's box images ship neither `staticcheck` nor `govulncheck`. A missing or wrong-version
tool fails the gate and prints the command that installs it. The gate never skips a check silently.

`.tool-versions` pins the exact Go toolchain, so an asdf user and co:op's own box get the
repository's required `go` and `gofmt` version automatically. CI reads the version from `go.mod`.

[AGENTS.md](AGENTS.md) is the contract every agent in this repo follows. One implemented task is
one commit, and the commit message ends with a `Coop-Task: <id>` trailer, where the id is the
task's folder name.

## Test layers

| Layer | Targets | What they prove |
|---|---|---|
| Local iteration | `make check-local SCOPE=<owner>` | the docs or rules checks, or an owner's checks and its reverse consumers |
| Blocking | `make check` | formatting, vet and Staticcheck (both with and without the test build tags), ShellCheck, `go build ./...`, govulncheck, unit tests under `-race` (CI also runs them plain, with only the required host programs on PATH), deterministic provider process E2E, tagged process-control races, generated docs, rules cards, maintenance tools, and comment alignment. Needs no runtime or credentials |
| Focused deterministic | `make provider-scripted-e2e` · `make acp-scripted-e2e` · `make live-process-control` | provider CLI, loop and fork policy, ACP switching and recovery, and live-harness contracts, all with fixtures |
| Runtime boundary | `make doctor` · `make box-runtime-e2e` · `make review-writes-e2e` · `make network-runtime-e2e` · `make task-channel-e2e` | real box isolation, process reaping and signal forwarding, report-only review mounts, filtered networking, and the in-box task channel (in the image `coop build` makes, and CI runs it daily). Requires Docker (or Apple `container` for doctor) |
| Pinned clients, offline | `make native-roles-e2e` · `make skills-e2e` · `make mcp-e2e` | each pinned client's own role, skill and MCP handling, with no network, in the locked image `coop net setup` builds. No credentials or model calls |
| Upstream compatibility | provider live targets [below](#live-provider-suites) · `make acp-e2e` | installed CLIs plus isolated credentials. Opt-in, and they consume quota |

The deterministic provider suite, `make provider-scripted-e2e`, runs the real co:op CLI across its
box and runtime boundary, with strict fixtures that match the native commands and output of Claude,
Codex, Gemini and Grok. It tests how the target, account, model and effort propagate, all directed
fallback pairs, and rate, auth and output failures. It also covers cancellation, loop audit stages,
the detached-fork lifecycle, and session lookup, telemetry and cleanup.

Its fake runtime checks how co:op assembles a run, not container enforcement, which `make doctor`
and `make review-writes-e2e` cover. If the suite fails, rerun the printed subtest with `-v` and
read its bounded, redacted trace.

The security-critical logic, which lists the secrets (`internal/box/mounts.go`) and assembles the
run arguments (`internal/box/run.go`), is pure code, and its unit tests need no runtime.
`coop doctor` proves the whole thing end to end against the real box.

`make network-runtime-e2e` is the evidence behind the [network support matrix](docs/networking.md).
It builds the locked client and gateway images, which is slow on a Docker daemon that lacks them,
then runs real filtered boxes to test allowed and refused traffic and the receipts that record it, a
lost guard, every transport the rule grammar accepts, and the cleanup of a run whose supervisor was
killed.

`make check` never runs it, and CI doesn't run it yet. It keeps its records in a new folder that
only you can read and prints that folder's path. To choose the folder, set
`COOP_NETWORK_TRIAL_STATE` to an absolute path of a new folder, or of one only you can read. Never
point it at co:op's own network state. Its fixture container has a fixed name, so run one copy per
Docker daemon at a time.

## Live provider suites

The live suites are opt-in, and they check compatibility with the provider CLIs currently installed
in the box:

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

An explicit list uses co:op's normal target syntax. `all` is strict mode: its list of providers
comes from the registry, every provider must be attempted and must pass, and a missing prerequisite
can't skip a provider. To select non-default accounts in strict mode, pass an explicit list that
names every provider in registry order.

Once a provider request starts, auth errors, rate limits, timeouts, wrong output, repository or
source changes and incomplete cleanup all count as failures. Most probes make one attempt. Resume
and network have fixed stages, and account recovery permits exactly two work launches. No suite
deliberately exhausts quota.

| Live target | Model sessions / minimum calls | Stable summary prefix |
|---|---:|---|
| `provider-live-e2e` | 1 per provider that passes its prerequisites | `COOP_PROVIDER_LIVE_SUMMARY` |
| `provider-resume-live-e2e` | 2 per provider that passes its prerequisites | `COOP_PROVIDER_RESUME_LIVE_SUMMARY` |
| `provider-loop-live-e2e` | 1 writable task per provider that passes its prerequisites | `COOP_PROVIDER_LOOP_LIVE_SUMMARY` |
| `provider-consult-live-e2e` | 4 peer sessions for a complete ring, and no lead sessions. Tool use may add upstream turns | `COOP_CONSULT_LIVE_SUMMARY` |
| `provider-delegate-live-e2e-all` | 4 peer sessions. Each must create exactly its requested file, without staging or committing | `COOP_DELEGATE_LIVE_SUMMARY` |
| `provider-network-live-e2e-all` | 3 stages per provider: prompt, native resume and a controlled shared MCP tool call through filtered networking | `COOP_PROVIDER_NETWORK_LIVE_SUMMARY` |
| `provider-accounts-live-e2e-all` | at most 2 work launches per configured compatible pair: rejected first account, successful second account | `COOP_PROVIDER_ACCOUNTS_LIVE_SUMMARY` |
| `acp-e2e` | depends on the scenario, which can start the adapter several times | none (strict test output) |

Each run gets a disposable repository, disposable HOME and XDG roots, and access-only copies of the
credentials, and it inherits no instructions, MCP configuration or session history. It has a hard
deadline and bounded cleanup, and the harness fingerprints the source credentials before and after
the run.

A read-only suite must leave the repository unchanged, a delegate suite must produce the exact
output, and a task suite must leave committed work bound to its task. The suites never keep raw
provider output, paths, accounts, tokens or anything that can refresh a sign-in. These targets stay
outside `make check` because every provider that passes its prerequisites consumes real quota.

A suite's summary line starts with its stable prefix and reports one of these results:

| Summary result | Action |
|---|---|
| `missing_runtime`, `missing_image`, `missing_cli`, `missing_credential` | Install, build or sign in as needed, then rerun. No paid request started. |
| `credential_refresh_required` | Sign in to the selected account again. The access token copied for the run would expire before the deadline. |
| `credential_not_portable` | Select a portable provider credential, one that isn't tied to this host. For Gemini live probes, use a `GEMINI_API_KEY` account stored by co:op or backed by an explicit env var. Live suites automatically use filtered networking for supported API keys. They refuse `GOOGLE_API_KEY` even with open networking. |
| `not_configured` | Account recovery only: fewer than two accounts are configured for that provider. This result makes no claim about recovery. |
| `ring_prerequisite` | Fix the named prerequisite. The consult ring started zero paid calls. |
| `failed` with `attempted=true` | Treat it as a compatibility failure in the upstream CLI or provider. Reproduce the syntax or policy problem with the deterministic fixture. |
| `repository_changed`, `source_changed`, `cleanup_failed`, `harness_failed` | Treat it as a local isolation or harness defect. These results override provider success. |

Account recovery, `make provider-accounts-live-e2e-all`, runs the real loop controller on one task,
with preflight, review and peers disabled and a hard cap of two work launches. The suite selects two
different configured accounts of the same credential family, in account-name order, and proves that
both copies are portable. Then it replaces only the first copy with invalid synthetic credentials.
It doesn't change or refresh the source accounts, and it doesn't test every account combination.

To succeed, the run needs a recorded authentication failure on the first account, successful task
completion on the second account, and exactly one commit bound to the task. `not_configured` isn't a
pass, and a configured pair that's unsafe, expired or incompatible fails. Provider tool use may
require multiple upstream turns.

`make acp-e2e` applies the same credential and runtime boundary to installed ACP adapters, and it
fails on every skip. Use `make acp-scripted-e2e` for injected limits, malformed responses,
switching, fallback and replay cases that shouldn't spend live quota.

## Checking the pinned clients

Run the offline client probes before any live suite. They call no model, so a failure there costs
nothing. Offline MCP (`make mcp-e2e`) covers Codex, Gemini and Grok. Only the live network suite
proves Claude's shared MCP route, and it requires a shared MCP tool call from every provider.

After a client pin changes, run the smallest live suite it affects, once you know that suite's scope
and billing. Clear `COOP_IMAGE` and `COOP_BASE_IMAGE` (in the shell and in coop.conf) first. An
open-network suite runs whatever image they name, so it wouldn't test co:op's own pinned clients.

No report is saved. A live run proves that run only, and a missing run stays unverified, never
waived. Builds, CI and releases make no model calls and never wait for a live run.

The [pinned clients KB](.agent/kb/provider-client-qualification.md) covers the locked clients, the
conformance rows and the procedure for bumping a client.

## Adding a provider

1. Register its production `Agent`.
2. Implement `LiveCredentials`, which the compiler requires.
3. Add independent test expectations for its native argv and output.

Registry completeness tests fail until the scripted ACP and provider tables and the help fragments
include the new provider. Strict `all` includes it automatically.

The detailed rules for fixtures, credential copies, process ownership and session history are in
the [provider testing KB](.agent/kb/provider-scripted-e2e.md), the
[live testing KB](.agent/kb/provider-live-e2e.md) and the
[ACP testing KB](.agent/kb/acp-scripted-e2e.md).
