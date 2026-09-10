---
name: restricted-execution-modes
description: readonly and bare share one tmpfs-only filesystem profile; the provider is seeded through a read-only bind OUTSIDE the tmpfs home, because a bind under it would be root-owned
subsystem: box
sources: [internal/box/restricted.go, internal/box/run.go, internal/agent/agent.go, internal/agent/claude.go, internal/cli/commands.go, internal/cli/exposure_flags.go, internal/runtime/runtime.go]
updated: 2026-09-10
---

`RunSpec.Mode` (`agents.ExecutionMode`: normal, readonly, bare; empty is normal) is fixed at
creation. `box.Run` dispatches a restricted mode to `runRestricted` (`internal/box/restricted.go`)
before anything the normal launch assembles — homes, caches, services, project policy, MCP, git
and instruction mounts — so the restricted profile is what that file builds, not what the normal
path leaves out. Normal launches are byte-identical to before (`TestAssembleArgsNormalModeGolden`).

The profile: `--read-only` root; owned tmpfs at the box home and `/tmp` (bare adds `/workspace`
as its cwd), each `rw,exec,nosuid,nodev,uid=1000,gid=1000,size=1g` (`exec` spelled out: Docker's tmpfs default is noexec, which broke `./script` and built test binaries in the live probe) — 1000 is the base image's `node`, which is why
restricted runs refuse any image but `cfg.BaseImage`; readonly's repo and companions `:ro`;
one read-only seed bind at `/coop/seed`; no other `-v`. `validateRestrictedOptions` proves the
assembled options against that plan as an allowlist, so a mount added to `assembleOptions` later
refuses a restricted launch by name instead of widening it.

The trap that shaped the seed: a tmpfs home cannot take a nested bind. Docker creates a missing
bind parent inside the already-mounted tmpfs as root, so `-v note:/home/node/.claude/CLAUDE.md`
would leave `/home/node/.claude` root-owned and the provider unable to write beside it
([[box-home-nested-mounts]] is the same mechanism under a real home). Hence the seed tree is
bound OUTSIDE the home and copied in by a `sh -c 'cp -R /coop/seed/. "$1"/ && shift && exec "$@"'`
prelude with positional parameters (`seedPrelude`) — no path or argument is shell-parsed. The
seed holds the adapter's access-only credential projection (`LiveCredentials`, after `Prepare`
renewed it for `restrictedCredentialHorizon`), `EnsureDefaults` rendered into an EMPTY profile
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
`unqualifiedRestrictedCommand` until a live run proves its switch. Docker is the only runtime
(`Runtime.SupportsRestrictedFilesystem`); `--egress filtered`, peers, presets, ACP, review stages,
`COOP_IMAGE` and runtime arguments beyond `-e KEY=VALUE` are refused, never dropped — a host-wide
`COOP_RUN_ARGS` bind (this host mounts Go caches) is the common refusal; the message names the
one-run escape, an empty `COOP_RUN_ARGS=` in front of the command.

Live-qualified on this host's Docker 29.4 with claude 2.1.267 (task artifacts `exposure-*.log`):
repo, root and seed writes fail with EROFS, `git commit` fails on `index.lock`, scratch writes and
executables work, no `coop-cache`/`coop-asdf`, bare's cwd is an empty owned tmpfs with no host path
present, readonly claude reads the repo and cannot write it, bare claude answers and states it has
no tools. Expect one host-side change: a login whose access token expires inside the horizon is
renewed in the host profile BEFORE projection ([[renew-before-access-only-projection]]), so
`.credentials.json` on the host may change; the box still receives only the access-only shape.

What the CLI half does not do (the session half's work): persist the mode on a session record and
project it in the API DTO, select it from a host-owned policy, run under ACP (`startChildWithRunID`)
with the exact-runtime receipt, and register activity — a restricted CLI run has no
`ActivityKind` and a bare one has no workspace scope label, so a sweep reports it, never reaps it.

## Changelog
- 2026-09-10 — phase 2 live qualification: added `exec` to the scratch tmpfs (Docker's default is
  noexec) and moved the bare no-tools statement to `--append-system-prompt`; recorded what a run
  proved and the host-side credential renewal a hash comparison will show.
- 2026-09-10 — created with the modes (phase 1, local half). Verified against the sources above and
  the goldens in `internal/box/restricted_test.go`.
