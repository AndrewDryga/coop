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
