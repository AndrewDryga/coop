---
name: acp-scripted-e2e
description: ACP state machines are exhaustive in the scripted runtime; real adapters get an isolated conformance layer
subsystem: acp
sources: [Makefile, internal/acpproxy/scripted_matrix_e2e_test.go, internal/acpproxy/e2e_test.go, internal/acpproxy/rpcclient_test.go, internal/acpproxy/testdata/acpfixture/main.go, internal/cli/acp_process_live.go, internal/liveprocess/contract.go, internal/processidentity/identity.go, internal/testutil/liveprovider/credentials.go, internal/testutil/liveprovider/contract.go, internal/testutil/liveprovider/copytree.go, internal/testutil/liveprovider/cleanup.go]
updated: 2026-07-15
---

`COOP_RUNTIME` lets process tests drive the built outer supervisor, inner re-exec, controller,
proxy, target resolution, and stdio while a semantic JSON fixture owns provider replies. The
scripted matrix covers every directed provider pair and deterministic model, limit, replay, and
failure orderings (`internal/acpproxy/scripted_matrix_e2e_test.go:41`,
`internal/acpproxy/scripted_matrix_e2e_test.go:111`). Run it with `make acp-scripted-e2e`; it does
not require provider credentials (`Makefile:66`).

`make acp-e2e` is the smaller installed-adapter contract. It shares the safe live-provider copier:
access-token-only auth artifacts, no Gemini host keychain, new `0600` inodes, an allowlist-only
process environment, and source-integrity checks around each ACP process. A bare target contributes
its marked default; an explicit account ladder contributes every named account. Preset prerequisites
come from the loaded lead ladder, so changing Frontier's fallback providers or pinned accounts cannot
leave the live gate stale. Frontier itself enters the disposable repo through a bounded transactional
copy that rejects links and special files. The disposable repo stays writable because ACP tool-call
and replay behavior is under test; no source repository or source credential home is mounted through
the shared no-follow isolation boundary. It proves live prompt/model/SIGHUP behavior and
cross-provider carry. Never induce real quota exhaustion there; provider fault injection belongs in
the scripted layer.
The live ACP layer refuses a scenario whose projected access token cannot outlive the suite or whose
credential is host-bound. Its temp root is removed on every setup failure as well as normal exit.
Real ACP stderr, wire frames, and provider error text are bounded but never emitted in test failures.
The live client rejects oversized frames, transcripts, or frame counts before JSON decoding and
reports only phase, error class, numeric RPC code, frame/byte counts, and truncation. Controlled
scripted fixtures keep their exact transcript assertions because they contain no user credentials.
`make acp-e2e` sets strict prerequisite mode, so missing, host-bound, or near-expiry credentials
fail rather than allowing all scenarios to skip successfully. A direct tagged `go test` remains a
permissive diagnostic. Every started process gets a unique `coop.live-test` label; teardown always
polls that label across running and stopped containers after normal exit, SIGTERM, or SIGKILL.
The isolated ACP binary also carries the `cooplivetest` tag. Each generation waits behind a pipe
gate until its resident PGID leader has published a private record with a stable kernel start token.
The record binds to the harness's `coop.live-test` cleanup nonce, not Coop's separate internal
`coop.sup` nonce; deterministic tests keep those values different. That leader remains after the
inner Coop exits, closing the leader-first orphan case. Teardown
atomically revokes projected credentials, awaits the outer supervisor so no new generation can be
admitted, revalidates and terminates every recorded group, and begins CID/label cleanup only after
all groups are gone. A SIGHUP self-reload revalidates and preserves the outer control descriptor for
that exec only; child execs still lose it. Record admission is serialized and reserves the pending
hardlink slot. An unreadable or over-limit registry can never satisfy producer quiescence, so label
sweeps continue to the bounded cleanup deadline and fail. Default Coop builds retain normal
per-generation process behavior and ignore the live-test environment. `make check` runs the
no-credential gate, identity, descriptor-leak, reload, admission-limit, and process-before-label
denial tests; the installed-adapter prompts remain opt-in.

Related: [[acp-replay-publication]], [[acp-target-commit]], [[acp-carry-echo]].

## Changelog
- 2026-07-15 - bound process records to the external cleanup nonce, kept control across authenticated SIGHUP reloads, and denied registry overflow or unverifiable quiescence
- 2026-07-15 - made tagged ACP generation admission and process-first teardown authoritative before label cleanup
- 2026-07-15 - made the Make target strict, staged exact target/preset accounts, bounded preset copies, and added authoritative post-process label cleanup
- 2026-07-15 - bounded live ACP frames/transcripts/stderr and reduced failures to structural diagnostics
- 2026-07-15 - derived live preset prerequisites from the loaded lead ladder
- 2026-07-15 - narrowed ACP to access-token-only/default-account credentials and per-scenario gates
- 2026-07-15 - refused ACP runs whose copied OAuth credentials could rotate remote refresh state
- 2026-07-15 - moved live ACP credentials and process env onto the shared no-follow isolation layer
- 2026-07-15 - split test-boundary fact from replay, target, and carry contracts; verified matrix and live drivers
- 2026-07-14 - created after replacing manual Zed reproduction with the scripted runtime driver
