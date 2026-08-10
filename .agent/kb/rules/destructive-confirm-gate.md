---
name: destructive-confirm-gate
description: "every unrecoverable delete routes through the one shared `ui.DestroyGate`"
scope: security
sources: [internal/ui/confirm.go]
check: "none"
updated: 2026-08-10
---

# Every unrecoverable delete goes through the one shared confirmation gate

Any command that irreversibly deletes user state (a task folder, a credential profile + its login
token, a fork clone) routes its confirmation through the single `ui.DestroyGate(what, yes)` helper
(`internal/ui/confirm.go`), never a hand-rolled prompt or a silent `os.RemoveAll`. The gate: with
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
- A new destructive verb → gate it with `ui.DestroyGate(<blast-radius phrase>, hasYes(args))` immediately
  before the delete; return its error as `(2, err)`. Add `--yes`/`-y` to the command's accepted flags.
- It lives in `internal/ui` — the package that owns the terminal — because a confirmation is the one
  thing a command cannot return as data for its caller to print: the answer has to come back from the
  same tty `ui.IsTerminal` already detects. A package that owns a destructive verb but not the
  terminal injects the decision through the `asks` callback instead of growing a second gate.
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
- 2026-08-10 — path-only, no claim change: the fork/fleet extraction moved five of the ten
  `ui.DestroyGate` call sites out of `internal/cli` into `internal/forkctl` (`rm.go`, `fleet.go`,
  `merge.go`); `internal/cli/fork_cmd.go` keeps the `--fresh` one and `profiles.go` its own. Still
  exactly ONE definition (`internal/ui/confirm.go`) — the extraction consumed the shared gate
  instead of minting the third copy this card was rewritten to prevent. `check:` is still `none`.
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
- 2026-08-10 — the card's central claim had gone false: the `internal/tasks` extraction left a
  SECOND identical `destroyGate` (`internal/tasks/util.go`), and the pending fork/fleet extraction
  would have minted a third. Single-sourced into `internal/ui` as `ui.DestroyGate` (+ `ui.Confirm`,
  `ui.ConfirmationResponse`), byte-identical prompt and errors; both copies plus their duplicate
  `confirm`/`confirmationResponse` deleted. Zero import-DAG change — `ui` is a leaf and both
  consumers already held the edge. Swept the tree: exactly ONE definition remains, with all 10 call
  sites repointed — `internal/cli` fork.go:356/1345, fork_fleet.go:487, fork_merge.go:471,
  profiles.go:269 and `internal/tasks` backlog.go:158, cmd.go:1202/1222/1762, queue.go:412. (The
  2026-08-09 entry's `tasks.go`/`taskcmd.go` paths are that extraction's pre-move names; the same
  sites are now `internal/tasks/queue.go` and `cmd.go`.) The gate's test moved with it to
  `internal/ui/confirm_test.go`. `check:` is still `none` — nothing mechanical catches a fourth copy.
