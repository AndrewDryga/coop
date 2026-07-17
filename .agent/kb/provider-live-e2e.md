---
name: provider-live-e2e
description: Probe installed upstream CLIs with isolated read-only, native-resume, and task-completion workflows
subsystem: testing
sources: [Makefile, internal/agent/agent.go, internal/agent/claude.go, internal/agent/codex.go, internal/agent/gemini.go, internal/agent/grok.go, internal/box/run.go, internal/liveprocess/contract.go, internal/processidentity/identity.go, internal/runtime/process_group_live.go, internal/testutil/liveprovider/credentials.go, internal/testutil/liveprovider/contract.go, internal/testutil/liveprovider/copytree.go, internal/testutil/liveprovider/orchestration.go, internal/testutil/liveprovider/cleanup.go, internal/cli/acp_process_live.go, internal/cli/provider_live_e2e_test.go, internal/cli/provider_resume_live_e2e_test.go, internal/cli/provider_loop_live_e2e_test.go, internal/acpproxy/e2e_test.go, internal/acpproxy/rpcclient_test.go]
updated: 2026-07-16
---

`make provider-live-e2e COOP_LIVE_TARGETS='...'` is the permissive prerequisite probe;
`make provider-live-e2e-all` is the strict registry acceptance gate. The former may skip only a
missing runtime, image, CLI, selected credential, a host-bound credential, or a projected access
token that cannot outlive the run. The latter expands `all` from `agents.Names()` by default; it
also accepts a complete registry-ordered explicit target list for account selection, and succeeds
only when every registered provider was attempted once and passed. Anything after the marker
command starts is a failure, including quota/auth errors; there are no retries.

`make provider-loop-live-e2e COOP_LIVE_TARGETS='...'` and its strict `-all` form reuse that same
admission and evidence contract for one writable task-completion attempt. The deterministic suite
already owns the real external loop controller, lease, reconciliation, and telemetry path. The
live child therefore makes one direct adapter `Headless` call with the production loop work prompt
against one pre-claimed mechanical task, avoiding the controller's ordinary paid retries. Success
requires the exact marker file and sole task-bound commit, unchanged task instructions, clean Git
state, final state/log, no scratch, no extra ignored files, and the task in done. It emits the same
path/account/token-free `COOP_PROVIDER_LOOP_LIVE_SUMMARY` and remains opt-in because each admitted
provider starts one headless session. Before any post-provider Git command, the verifier walks the
entire Git administrative tree without following links; it then requires the exact commit-message
file, raw reflog append, and an index structurally equal to a canonical index rebuilt from `HEAD`
apart from cross-mount stat fields, so ignored Git metadata cannot hide provider output.

`make provider-resume-live-e2e COOP_LIVE_TARGETS='...'` spends two requests per admitted provider:
one clean helper creates a marker-bearing native session, and a second provider request receives
only its canonical ID and must return the prior assistant response without seeing the marker in its
process environment or prompt. The helper retains the marker only as verifier state. Both use the same
isolated history and read-only repository. Claude, Gemini, and Grok exercise their normal exact
adapter lookup; Codex discovers `thread.started.thread_id` from bounded JSON and uses native
`codex exec resume <id> --json`. The private ID handoff is bounded/no-follow and every stage gets
separate process/container cleanup. The strict `-all` form fails on any skip and there are no retries.
The cleanup supervisor label does not imply ACP behavior: `ShareACPSessions` is set only by the ACP
launch path, so supervised prompt/loop/consult/resume probes keep native history in their selected
profile instead of shadowing it with the credential-independent ACP store.

The parent reads resolved Coop config only to select credential inputs and the host-runtime
capability. `internal/testutil/liveprovider` writes one selected account's adapter-declared auth
artifacts into new `0600` single-link files. Claude retains only a scoped, unexpired inference
access token; Codex retains exactly one API-key or ChatGPT access-token branch and represents the
required refresh field as an empty string. Grok retains an access key, expiry, and the current CLI's
required auth-mode/OIDC/principal/user/team/create-time routing fields; it drops refresh authority
and user-facing profile metadata. A key/expiry-only Grok record parses but is treated as logged out.
Refresh authority never enters staging. Gemini's host-bound keychain is fingerprinted but not
copied, and its settings projection contains only `security.auth.selectedType`. A Gemini API-key
selection grants only `GEMINI_API_KEY`; Vertex express mode grants only `GOOGLE_API_KEY`. The helper
rejects unsafe modes, symlinks, hardlinks, special/oversized/replaced files, bare env imports, and
partial destinations.

The child starts from a fresh environment map, so ambient `COOP_*` and provider keys cannot alter
the command. Explicit Docker connection fields are retained directly; otherwise the parent resolves
the active Docker context to endpoint/TLS fields. Podman is reduced to its selected URI/identity and
storage fields. `DOCKER_CONFIG`, `DOCKER_CONTEXT`, `CONTAINERS_CONF`, connection selectors, and other
behavior-bearing config are never forwarded. The narrow values reach the host runtime process, not
the container argv/env; isolated `HOME` and XDG config roots still reach the provider. ACP's opt-in
live suite copies the marked default for bare targets and every account explicitly named by a direct
target or preset ladder.

The provider command is the adapter's real `Headless` form inside `box.Run`: batch, open egress,
isolated homes, no MCP/instructions/services/cache, and the generated repo mounted read-only. A
version probe makes no model request. The marker prompt makes exactly one. The parent accepts one
bounded, no-follow child result, compares Git status, HEAD, refs, reflogs, and a content/mode tree,
checks the source fingerprint, then follows an ordered process/container cleanup contract. A tagged
direct probe inherits its helper's harness-owned process group only after validating an inherited
private control descriptor; the descriptor and its environment key are close-on-exec/scrubbed. On a
deadline, the harness atomically revokes the projected credential path before its first signal and
waits for the whole group to disappear. The credential helper allocates one random parent-known
tombstone before launch. If child-side deletion fails after the atomic rename, parent teardown adopts
that same private path and retries; a persistent failure becomes `cleanup_failed`. The tombstone
pathname and control variables are scrubbed before the runtime CLI exec. Normal completion performs
the same revoke before container cleanup. Only then does the parent reap fixed cidfiles and poll the unique supervisor label across
running/stopped state through a full quiet period. Cleanup failure, source mutation, and repository
mutation override the child result in that order. The single
prefixed JSON summary is safe to retain: the CLI version is a provider-labelled semver token and
diagnostics are bounded structural codes, never account names, paths, tokens, raw output, or digests.

Live ACP adds per-generation ownership without weakening normal provider/model switching. Its
isolated binary is built with `cooplivetest`; default Coop binaries contain the unchanged production
process-group path. Each inner generation blocks behind a pipe gate while a resident wrapper becomes
its PGID leader. The wrapper publishes a private, versioned record containing the current UID,
harness cleanup nonce, PGID, and Linux/Darwin kernel start token before releasing the inner command.
That nonce is deliberately distinct from Coop's internal `coop.sup` generation label and is scrubbed
before the inner exec. The wrapper
stays resident after the inner leader exits, so a delayed runtime cannot outlive the recorded
identity. Teardown revokes credentials, stops and awaits the outer supervisor (closing admission),
revalidates every token plus `Getpgid` immediately before TERM and KILL, waits for all groups to
disappear, and only then performs CID and label sweeps. SIGHUP revalidates the outer descriptor and
preserves it only across the supervisor's immediate self-exec; the new image marks it close-on-exec
again before any child starts. Publication is serialized and reserves one of the 128 bounded
directory entries for the pending hardlink, so a full registry fails before a generation runs.
Malformed, linked, oversized, wrong-owner, stale, or unverifiable records fail closed without
signalling an unrelated process. They also prevent the label quiet window from declaring success:
best-effort sweeps continue until the caller's cleanup deadline and report the unresolved producer.
`make check` runs the deterministic denial cases through `make live-process-control`; real prompts
remain opt-in.

To extend the suite, register the production `Agent` and implement its compiler-required
`LiveCredentials` method. The registry-generic `TestMetadata` validates unique safe basenames,
exactly one primary, a projector for every artifact, portability, and safe auth signals; no
provider-specific metadata row is needed. A projector may deliberately return nil for host-bound
state. Unknown portability and invalid projection fail as `unsafe_credential`, not a skip. The
`all` selector and strict summary include the registered provider automatically, so the helper has
no provider switch.

A source fingerprint proves local immutability and includes device/inode identity. File credentials
carry no refresh authority. Before the prompt, the adapter must prove the projected access token
outlives the run; otherwise standard mode reports `credential_refresh_required` with
`attempted=false`, and strict mode fails. Gemini OAuth instead reports
`credential_not_portable`; re-login cannot change its host-bound encryption, so live tests require
the selected env-backed API-key mode. The no-quota version probe still runs for these preflight
skips.

Triage a `skipped` result as a prerequisite: `missing_runtime`, `missing_image`, `missing_cli`, or
`missing_credential` needs local setup; `credential_refresh_required` needs re-authentication;
`credential_not_portable` needs an env-backed key; `ring_prerequisite` means the consult ring admitted
zero paid calls. A `failed` result after `attempted=true` is upstream compatibility or provider
behavior. `repository_changed`, `source_changed`, `cleanup_failed`, and `harness_failed` are local
isolation failures and take precedence over a provider result. Stable summaries intentionally omit
raw output; reproduce behavior in the deterministic fixture.

## Changelog
- 2026-07-16 - added native-resume ownership and the shared live-result triage ledger
- 2026-07-16 - separated supervision from ACP transcript sharing after live resume exposed the shadow mount
- 2026-07-16 - added two-process exact native-session continuity with a private bounded ID handoff
- 2026-07-16 - added the single-attempt writable task-completion workflow and exact repository verifier
- 2026-07-16 - retained Grok's required non-refresh routing record after live access-only compatibility proved the two-field projection unauthenticated
- 2026-07-15 - made timeout credential revocation use a parent-known tombstone with retry and persistent-failure reporting
- 2026-07-15 - bound records to the harness cleanup identity, preserved process control across ACP self-reload, and made admission/quiescence proof bounded and fail-closed
- 2026-07-15 - gated tagged live processes behind authenticated control, stable PGID records, credential revocation, and process-first cleanup
- 2026-07-15 - resolved runtime contexts to narrow connection fields and denied behavior-bearing Docker/Podman config
- 2026-07-15 - made live metadata compiler-required; added exact projections, runtime capability isolation, bounded evidence, and deterministic cleanup/result precedence
- 2026-07-15 - preserved Codex's required refresh field as an empty, non-authoritative value
- 2026-07-15 - made live execution opt-in, projector-required, and fail-closed for unknown portability
- 2026-07-15 - removed refresh authority, distinguished host-bound credentials, and added cid/all-state cleanup plus redacted diagnostics
- 2026-07-15 - refused copied credentials that could remotely rotate an unchanged source refresh token
- 2026-07-15 - created with adapter-owned credential isolation and strict four-provider live verification
