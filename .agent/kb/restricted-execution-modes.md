---
name: restricted-execution-modes
description: readonly and bare share one tmpfs-only filesystem profile; the provider is seeded through a read-only bind OUTSIDE the tmpfs home, because a bind under it would be root-owned; over ACP the provider's switches ride session/new, not the adapter's argv
subsystem: box
sources: [internal/box/restricted.go, internal/box/restricted_filtered_composition_test.go, internal/box/filtered.go, internal/box/filtered_cleanup.go, internal/box/run.go, internal/agent/agent.go, internal/agent/claude.go, internal/cli/help.go, internal/cli/commands.go, internal/cli/exposure_flags.go, internal/cli/acp_cmd.go, internal/cli/fork_cmd.go, internal/runtime/runtime.go, internal/sessionsvc/service.go, internal/sessionsvc/acp.go, internal/sessionsvc/network.go, internal/session/records.go]
updated: 2026-09-23
---

`RunSpec.Mode` (`agents.ExecutionMode`: normal, readonly, bare; empty is normal) is fixed at
creation. `box.Run` dispatches a restricted mode to `runRestricted` (`internal/box/restricted.go`)
before anything the normal launch assembles — homes, caches, services, project policy, MCP, git
and instruction mounts — so the restricted profile is what that file builds, not what the normal
path leaves out. Normal launches are byte-identical to before (`TestAssembleArgsNormalModeGolden`).

The profile: `--read-only` root; owned tmpfs at the box home and `/tmp` (bare adds `/workspace`
as its cwd), each `rw,exec,nosuid,nodev,uid=1000,gid=1000,size=1g` (`exec` spelled out: Docker's tmpfs default is noexec, which broke `./script` and built test binaries in the live probe) — 1000 is the base image's `node`, which is why
restricted runs accept `cfg.BaseImage` at entry and never a project/custom image (filtered launch
substitutes its managed client image); readonly's repo and companions `:ro`; one read-only seed
bind at `/coop/seed`, plus run-owned read-only broker configuration when needed. `validateRestrictedOptions` proves the
assembled options against that plan as an allowlist, so a mount added to `assembleOptions` later
refuses a restricted launch by name instead of widening it.

The trap that shaped the seed: a tmpfs home cannot take a nested bind. Docker creates a missing
bind parent inside the already-mounted tmpfs as root, so `-v note:/home/node/.claude/CLAUDE.md`
would leave `/home/node/.claude` root-owned and the provider unable to write beside it
([[box-home-nested-mounts]] is the same mechanism under a real home). Hence the seed tree is
bound OUTSIDE the home and copied in by a `sh -c 'cp -R /coop/seed/. "$1"/ && shift && exec "$@"'`
prelude with positional parameters (`seedPrelude`) — no path or argument is shell-parsed. The
seed holds the adapter's access-only credential projection (`LiveCredentials`, after `Prepare`
renewed it for `RestrictedCredentialHorizon`), `EnsureDefaults` rendered into an EMPTY profile
(so no hook or skill of the host profile travels), and the mode's instruction note. A profile
with no marker seeds no credential and the scoped env file stays the login, as in normal mode.

Provider switches ride `Agent.RestrictedCommand(mode, cmd)`: claude adds `--strict-mcp-config
--setting-sources user` (no project `.mcp.json`, no repo `.claude/settings.json` hooks) and, for
bare, `--tools ""` first so the empty variadic value is closed by the next flag, plus
`--append-system-prompt` stating that the tool set is empty. The statement rides the system-prompt
channel on purpose: put in the seeded CLAUDE.md it argued with Claude Code's own system prompt and
a live bare run refused a trivial prompt as "prompt injection"; on the CLI's channel the model
answers "no tools" and does not role-play tool calls. The enforcement is `--tools ""` alone — the
session's `system/init` event shows `tools: []`, `mcp_servers: []` (stream-json, 2026-09-10).
Caller flags that hand tools or settings back are refused by name. Every other adapter answers
`unqualifiedRestrictedCommand` until a live run proves its switch — and `coop help <agent>` asks
that same method (`restrictedModesOffered`, internal/cli/help.go) before it prints the
`--readonly`/`--bare` rows, so qualifying an adapter publishes its own help and an unqualified one
never advertises a flag its launch would reject. Docker is the only runtime
(`Runtime.SupportsRestrictedFilesystem`); peers, presets, shared ACP
transcripts, an editor supervisor, maintenance commands under an agent scope, review stages,
`COOP_IMAGE` and runtime arguments beyond `-e KEY=VALUE` are refused, never dropped — a host-wide
`COOP_RUN_ARGS` bind (this host mounts Go caches) is the common refusal; the message names the
one-run escape, an empty `COOP_RUN_ARGS=` in front of the command.
Direct readonly runs compose with filtered networking; bare has no project to admit a policy for
and still refuses it. Restricted remote-session policies also refuse filtered networking.
Readonly moves an implicit host-path workdir under `/tmp` or the tmpfs home to `/workspace` inside
the box; otherwise a disposable Linux checkout would collide with scratch and refuse an ordinary
run. An explicit different destination in those scratch trees still refuses. The host source may
remain under `/tmp`; `/workspace` is a tmpfs only in bare mode.

Historical live qualification (2026-09-10) on this host's Docker 29.4 with claude 2.1.267
(task artifacts `exposure-*.log`; not a claim about the current locked clients or filtered composition):
repo, root and seed writes fail with EROFS, `git commit` fails on `index.lock`, scratch writes and
executables work, no `coop-cache`/`coop-asdf`, bare's cwd is an empty owned tmpfs with no host path
present, readonly claude reads the repo and cannot write it, bare claude answers and states it has
no tools. Expect one host-side change: a login whose access token expires inside the horizon is
renewed in the host profile BEFORE projection ([[renew-before-access-only-projection]]), so
`.credentials.json` on the host may change; the box still receives only the access-only shape.

## The session API half

A policy's `mode:` (`Policy.Mode`, parsed with `agents.ParseExecutionMode`; absent is normal) is
bound into both digests only when restricted (`digestedSessionMode`), so every existing normal
policy's digests are byte-identical — `TestExecutionModeIsBoundIntoPolicyDigestsOnlyWhenRestricted`
pins the known cold digest. The mode is persisted on the session row (schema v22, `mode TEXT
DEFAULT ''`; `normalizedMode` reads '' as normal, legacy rows are never rewritten) and projected
as `"mode"` in `SessionDTO`, so a restarted daemon relaunches a session under the mode it was
created with even after the policy was edited. `validateRestrictedSessionPolicy` is the list of
refusals by name; `validateRestrictedTurn` refuses semantic validation on either mode and a
Responder binding on bare at submit; `captureCreateIntent` refuses a pull request or Responder
binding on a bare create.

A bare create is the workspace-less branch of `executeCreateIntent`: no pin, no fork, no
companions, no admission (its posture is the policy's own `egress` block, open by default —
`admitSessionNetwork`/`ResolvePolicyNetwork` return early, since there is no project to admit
against), and a `CreateSessionRequest` whose four repository bindings are empty; the store
accepts that shape only for `Mode: "bare"` and demands no freshness receipt for it. Every
repository-specific operation goes through `requireSessionWorkspace` first (changes, checkpoint,
restore, workspace task; review refuses on its own bound-fork check) and answers
`invalid_session_state` — a 409, not a 400: the client asked a bare session for something it is,
not something its request lacked. Discard of a workspace-less session is `retireWorkspacelessSession`.

The child is `coop acp <target> --bare` (`acpBare`: the plain adapter under the profile, no
supervisor, no project — the dispatch guard skips `loadProject` for a bare launch) or
`coop fork <name> acp <target> --readonly` (`forkACP` under the profile: the daemon's reservation
is still required, the legacy writable `.coop-output` bind is never made, and no activity record
is registered — the daemon's run label is the whole receipt). `startChildWithRunID` sends the
adapter's `ACPRestrictedSessionMeta(mode)` as `_meta` on `session/new` with `cwd` = the fork's
in-box path (the host path normally, `/workspace` on a readonly scratch collision), or
`box.BareWorkdir` for bare; claude-agent-acp spreads `_meta.claudeCode.options` into the SDK
options and the SDK renders `settingSources: ["user"]`, `strictMcpConfig: true` and an empty
`tools` array onto the claude argv as `--setting-sources=user --strict-mcp-config --tools ""`,
and `_meta.systemPrompt.append` as `appendSystemPrompt` on the CLI's stream-json initialize
request (read out of adapter 0.76.0 / SDK 0.3.257 and proved against a recording claude
executable — task artifacts `acp-spike-3-argv-{bare,readonly}.log`). The daemon hands a bare
session no MCP servers at all. `checkRestrictedSpec` admits the ACP launch through
`NetworkClient == egress.ClientACP`, and `runRestricted` still asks the adapter for the meta, so
an unqualified provider refuses in the box as well as at policy load.

The older `repository_read_only: true` normal-mode session still supports generated image output.
Its child sees the usual `<workspace>/.coop-output` destination, but the writable bind source is a
session-owned directory under the daemon state root, outside the anchored checkout. The daemon
keeps that source directory for the lifetime of a warm child, removes only each completed turn's
child directory, and deletes the session-owned root during discard. Mounting a checkout descendant
as that source is refused; otherwise it would reopen the same source-resolution race that the
authority mount guard closes.

Four traps the API half found. The daemon ends a turn's child by closing its stdin and, a quarter
second later, signalling its process group — and the seed directory (with the credential
projection in it) is removed only when the child's own run returns, so the first live pass left
one `coop-seed-*` per turn in TMPDIR. A restricted child now gets the bounded stop grace a
filtered one gets (`sessionACPRestrictedStopGrace`: the adapter exits on EOF within a second or
two, then the seed goes) and runs under a `signal.NotifyContext`, so a signal still becomes a
cancellation `runRestricted` cleans up after. The restricted box re-checks the seeded access-only token against
`RestrictedCredentialHorizon` and holds no refresh authority, so `Run` projects a restricted
session's credential for at least that horizon — a short `turn_timeout` is not a short token.
No native session is ever bound for a restricted session (`process.restricted`): the transcript
died with the tmpfs, so every turn is `session/new`, and a schema repair regenerates from the
admitted prompt instead of re-prompting a session nothing can load. And the `<coop-output>`
preamble is omitted with the output root it names: a directory nothing can write is a prompt for
a tool call. What the API half still does not do: register activity for a restricted session
(the run label is the cleanup receipt), keep provider history across turns, or qualify codex,
gemini or grok — each refuses by name until a live run proves its adapter's switch.

## Changelog
- 2026-09-23 — mapped implicit readonly checkouts under Linux `/tmp` to `/workspace` and kept
  the ACP `session/new` cwd aligned with the actual mount; explicit scratch destinations still refuse.
- 2026-09-23 — moved the legacy read-only session's writable output source out of the anchored
  checkout while preserving its in-box `.coop-output` contract and warm-child lifecycle.
- 2026-09-23 — corrected readonly scratch-conflict validation to compare the resolved in-box
  workdir, retaining `/workspace` and rejecting only actual tmpfs destinations.
- 2026-09-22 — corrected current filtered composition and image/credential handling against the
  launch implementation and composition tests. Bare and remote-session policy refusals remain;
  earlier native live evidence stays explicitly historical, not extended to untested combinations.
- 2026-09-11 — noted that per-agent help now derives the `--readonly`/`--bare` rows from
  `RestrictedCommand` itself, so the qualification and its documentation cannot disagree. Added
  internal/cli/help.go to `sources`.
- 2026-09-10 — the session API half: policy `mode`, persisted and projected; workspace-less bare
  create and its refusals; the ACP child launches under the profile with the adapter's session
  meta (mechanism read out of the adapter's dist and proved on a recording executable).
- 2026-09-10 — phase 2 live qualification: added `exec` to the scratch tmpfs (Docker's default is
  noexec) and moved the bare no-tools statement to `--append-system-prompt`; recorded what a run
  proved and the host-side credential renewal a hash comparison will show.
- 2026-09-10 — created with the modes (phase 1, local half). Verified against the sources above and
  the goldens in `internal/box/restricted_test.go`.

## Composing readonly with `--egress filtered`

`runRestricted` now composes the restricted filesystem assembly with the existing filtered launch
and its shared `filteredExecution.teardown`. Direct readonly admission still reads the project's
network requests; it does not load project environment, start services, or use a project image.
A filtered policy with service grants is refused because this path supplies no Compose file.
Bare is refused both by the CLI and `checkRestrictedSpec`; restricted remote-session policies are
separately refused by `validateRestrictedSessionPolicy` and the session admission path.

The first composition attempt exposed five traps, now handled by the implementation:

1. `restrictedAssemblyNetwork` names no network under filtered. The filtered launch alone joins
   the controller's namespace; an extra `--network none` would prevent a usable composed box.
2. `prepareFilteredExecution` skips project images for restricted modes, and both the caller and
   launch check that `record.ProjectImage` is empty before using the managed client image.
3. A non-nil filtered execution is registered for teardown before its preparation error is checked,
   because late preparation failures may already own gateway resources.
4. Preparation and `prepareCredentialBroker` precede environment assembly. MCP token variables are
   scrubbed; the box receives the provider key's substitute, never the reusable key. This permits
   direct Claude API-key readonly runs without qualifying other providers' restricted switches.
5. Bare refuses filtered even with a supplied capture; it never falls back to the caller's cwd.

The composition tests pin both the option allowlist and scratch UID, and drive the filtered launch
against its fake Docker daemon to inspect the created container's namespace and read-only root.
They also inspect the actual brokered env file, not just a credential plan. These are local
regressions, not live provider qualification. Preserve that distinction when extending the modes.
