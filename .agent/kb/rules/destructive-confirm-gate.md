---
name: destructive-confirm-gate
description: "every unrecoverable delete routes through the one shared `destroyGate`"
scope: security
sources: [internal/cli/util.go]
check: "none"
updated: 2026-08-09
---

# Every unrecoverable delete goes through the one shared confirmation gate

Any command that irreversibly deletes user state (a task folder, a credential profile + its login
token, a fork clone) routes its confirmation through the single `destroyGate(what, yes)` helper
(`internal/cli/util.go`), never a hand-rolled prompt or a silent `os.RemoveAll`. The gate: with
`--yes` it proceeds; piped (no TTY) it REFUSES and says to pass `--yes`; at a TTY it asks
"`<what>`? this can't be undone" defaulting to **No**, so a stray Enter cancels. `what` names the
blast radius — the resolved id, the profile, or the count — echoed BEFORE the delete, so the user
sees exactly what's at stake.

**Why:** the v3 audit found `coop tasks rm` `os.RemoveAll`-ing a substring-matched folder with no
prompt (and echoing the id only *after*), `rm --all-done` wiping every archive unprompted, and
`profiles rm` dropping a login token with no confirmation — while the templates called rm "a MANUAL
human action" with nothing mechanical enforcing it, and `fork merge` had already set the stricter
`approve()` precedent. Divergent per-command prompts are how one of them ends up with no prompt at all.

**How to apply:**
- A new destructive verb → gate it with `destroyGate(<blast-radius phrase>, hasYes(args))` immediately
  before the delete; return its error as `(2, err)`. Add `--yes`/`-y` to the command's accepted flags.
- Resolve and name the target (id/profile/count) BEFORE deleting, and put it in `what`.
- Keep `--yes` (skip the prompt) distinct from `--force` (override a safety guard like unmerged/dirty).
  `--force` is never a prompt-skip; `--yes` is never a guard-override.
- Deletion prompts default to **No**; only a land-then-remove flow may default Yes on the *land* step.
- Narrow exception: `<task>/tmp/` is lifecycle-declared disposable scratch, not retained user state.
  Reaching done may remove exactly that containment-checked child without a second prompt (the loop
  cannot prompt), but it must preserve `artifacts/` and every other task file, refuse path escape or
  task-folder symlinks, and fail completion loudly if cleanup fails. Never generalize this exception
  to screenshots, artifacts, task folders, archives, credentials, forks, or unspecified scratch.

See also [[destructive-verb-rm]] (the verb is named `rm`) and [[bare-subcommand-shows-help]].

## Changelog
- 2026-07-02 — created
- 2026-07-14 — revised
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: read `destroyGate` (util.go:542) and exhaustively
  enumerated + traced all 17 `os.RemoveAll` call sites in internal/cli (non-test). 0 violations —
  every user-state deletion (backlog.go:143→146, profiles.go:269→272, tasks.go:401,
  taskcmd.go:1090/1110/1634, fork.go:355/1344, fork_merge.go:508, fork_fleet.go:487) is gated by
  `destroyGate` immediately before it; taskcmd.go:821's tmp/ cleanup matches the documented
  exception exactly (refuses a non-directory/symlinked task folder, unlinks a tmp symlink instead
  of following it); the rest (fork.go:883 a scratch review clone, commands.go:1001/1026 ACP cid
  bookkeeping, doctor.go/selfupdate.go/sign.go temp dirs, controller.go tree snapshots,
  taskdir.go:359 disposable rebuildable split slices, tasklease.go:329/365 lease-adoption
  staging) are ephemeral/derived/internal state outside "user state," not gate-bypasses.
