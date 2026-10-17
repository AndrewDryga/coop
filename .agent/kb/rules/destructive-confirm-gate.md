---
name: destructive-confirm-gate
description: "every unrecoverable delete routes through the one shared `ui.DestroyGate`"
scope: security
sources: [internal/ui/confirm.go, internal/box/reclaim.go, internal/box/derived_image.go, internal/box/sweep.go, internal/box/network_recover.go, internal/networkstate/execution_retention.go, internal/networkstate/snapshot_retention.go, internal/box/image.go, internal/box/network_setup.go]
check: "none"
updated: 2026-10-17
---

# Every unrecoverable delete goes through the one shared confirmation gate

Any command that irreversibly deletes user state (a task folder, a credential profile + its login
token, a fork clone) routes its confirmation through the single `ui.DestroyGate(what, yes)` helper
(`internal/ui/confirm.go`), never a hand-rolled prompt or a silent `os.RemoveAll`. The gate: with
`--yes` it proceeds; piped (no TTY) it REFUSES and says to pass `--yes`; at a TTY it asks for confirmation defaulting to **No**, so a stray Enter cancels. `what` names the
blast radius — the resolved id, the profile, or the count — echoed BEFORE the delete, so the user
sees exactly what's at stake. The preview names the future permanent deletion; a short
`Continue? [y/N]` can follow it without repeating the whole consequence. Never print a completed
`deleted` label before the user agrees. The existing helper needs the reviewed presentation
update; keep its safety decisions shared, not a new per-command prompt implementation.

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
- Narrow exception: a delete a human can simply redo is not this rule's subject. `coop net forget`
  removes a remembered network approval, which `coop approve` recreates in one command, so it
  uses approve's own preview-and-confirm instead of the gate: `DestroyGate` would tell the operator
  "this can't be undone", which is false, and its `--yes` would let an unattended run drop a
  decision the networking contract says only a human makes. It is stricter where it counts — no
  `--yes` at all, a terminal required, default No. Reach for the gate whenever redoing the thing
  needs anything more than one command.
- Narrow exception: an image Coop built for itself is not user state. A build — including an explicit
  filtered project build (`internal/box/derived_image.go`) — reclaims the images it
  superseded (`internal/box/reclaim.go`) with no prompt, because the thing removed is rebuilt by the
  same command that removed it and nothing the user authored is in it. It earns that by being narrow:
  only a repository Coop tags by definition (a project's own also carrying the label Coop's build
  applies), only after 14 days with no recorded use, never on first sight, never one a container
  still references, and never `-f`. An image of the user's own — whatever it is tagged — stays. This
  one runs unattended, in a `coop loop` iteration with no human present, which is exactly why every
  clause above is a precondition rather than a prompt. A same-tag rebuild's predecessor
  (`removeReplacedImages`) is the same exception on a shorter clock, and narrower for it: only the ID
  the tag named just before the rebuild of a managed base or a derived image, read only where
  Coop's ownership is proven (the managed base tag, the derived label), only once the rebuild left it
  with no tag at all (an image anyone tagged since stays), never one a container still references,
  never `-f`. Network setup never removes its predecessor pair: earlier captures run it by ID.
- Narrow exception: a loop's or editor session's private Compose project is that run's scratch copy
  of the project's services, not user state. The loop removes it with its volumes at a clean exit,
  and the loop/fork/build start sweep (`ReapOrphanServices`, `internal/box/sweep.go`) and `coop fork
  rm` remove a killed run's, unattended. It earns that by being narrow: only this workspace's exact
  per-run project names, only services carrying the supervisor label Coop's own override wrote, only
  once the kernel proves that supervisor gone, volumes only by that project's label, and Docker still
  refuses a volume a container uses. The development project `coop up` runs is never this exception:
  its volumes go only through `coop down --delete-volumes` and its gate.
- Narrow exception: Coop's network records are its own bookkeeping, kept for a window. The settle
  pass every filtered launch, loop or fork start and `coop net recover` runs
  (`internal/box/network_recover.go`) removes, unattended, a finished run's record 90 days after the
  run ended, and an unused policy snapshot with its `admitted-` marker (a launch's is compiled from
  the inputs that admitted it, which the next admission with those inputs writes again). It earns
  that by being narrow: only `execution-<id>.json`, and `snapshot-<fingerprint>.json` with its
  `admitted-<fingerprint>.json`, in the network state directory; never a run still owed cleanup,
  never a snapshot a project admission did not create (a controller job's, which its session row
  names forever, or one from before the marker, except the setup check's below), and never an
  approval, withdrawal, bundle or qualification record; a record that does not read and validate,
  or is a link, stays; a snapshot goes only after a listing of every run record read whole, when
  no run names it and nothing admitted or loaded it for 90 days; and the last look happens under
  the run's or the snapshot lock, so nothing put to use since is removed. A `coop net setup`
  check's snapshot is never written again, and is safe to remove for another reason: only the
  check's own run names or loads it, nothing else admits a rule with `networkstate.SetupOrigin()`,
  and a crashed setup is rerun in a new scratch project, never resumed (`QualificationSmoke`), so
  nothing loads it once that run has ended. Setup also deletes the scratch project the snapshot is
  scoped to when it returns, though not when it is killed. One from before the marker goes only on
  this proof: the check's expiring run record (`execution-<id>.json`, purpose `qualification`)
  names it; it has at least one grant; every grant's only origin is `networkstate.SetupOrigin()`;
  and it names no provider (no dependencies). That record marks it just before it goes, dated to
  the run's end, and the next pass removes it.
- Narrow exception: `<task>/tmp/` is lifecycle-declared disposable scratch, not retained user state.
  Reaching done may remove exactly that containment-checked child without a second prompt (the loop
  cannot prompt), but it must preserve `artifacts/` and every other task file, refuse path escape or
  task-folder symlinks, and fail completion loudly if cleanup fails. Never generalize this exception
  to screenshots, artifacts, task folders, archives, credentials, forks, or unspecified scratch.

See also [[destructive-verb-rm]] (the verb is named `rm`) and [[bare-subcommand-shows-help]].

## Changelog
- 2026-10-17 — the 2026-08-09 and 2026-08-10 sweep entries name each call site's function (as it
  was then) instead of file:line numbers, resolved against the files at those commits.
- 2026-10-17 — the network-records exception now covers a `coop net setup` check's snapshot,
  safe to remove because only the check's own run names or loads it and a crashed setup is rerun,
  never resumed (not because an admission writes it again). One from before markers needs the
  exact proof the bullet lists, checked as the check's expiring run record (`execution-<id>.json`,
  purpose `qualification`) goes; that record marks it, dated to the run's end, and the next pass
  removes it. Swept the change: the one new write is that marker, the one new removal candidate is
  such a snapshot, and every other snapshot from before markers still stays.
- 2026-10-16 — named the settle pass's network retention as a narrow exception when it began
  removing unused policy snapshots. Swept every delete under `~/.local/state/coop/network`: the
  90-day run-record retention (`RemoveExpired`, 2026-10-15) had landed without a bullet and is
  covered by the same one; a run's `artifacts-<id>` directory goes with that run's own exact-owned
  cleanup; `coop net forget` keeps its own exception and `coop approve` clears a withdrawal;
  bundles, qualifications and job snapshots are never removed.
- 2026-10-16 — named a same-tag rebuild's untagged predecessor under the image exception when the
  base and derived builds began removing it (network setup does not: captures pin its pair by ID).
  Swept the other new unattended removals: a dead supervisor's broker helper, and a stopped
  container carrying the image-proof label, over an hour old and in no Compose project, removed by
  the daemon's own prune through a launch-allowed binding, are Coop's own containers, not user
  state; neither prune nor sweep ever forces an image.
- 2026-10-16 — named a killed run's private Compose project as a narrow exception when the
  loop/fork/build start sweep and `coop fork rm` began removing it with its volumes. Swept the other
  unprompted volume deletes: the loop's clean-exit teardown (`StopServicesForOwner`) is the same
  per-run scratch; `coop down --delete-volumes` keeps its gate.
- 2026-09-22 — filtered launches now consume an explicit build and never reclaim images. The
  existing narrow exception stays on the explicit build path; swept derived_image.go and reclaim.go.
- 2026-09-20 — the exception now covers a filtered LAUNCH that builds a project's own box image, so
  an unattended loop iteration may remove one; the preconditions are unchanged and are the reason.
- 2026-09-20 — named Coop's own superseded images as a narrow exception when the build-time reclaim
  landed. Swept the tree for other unprompted deletes: the rest are this rule's declared exceptions
  (`<task>/tmp/`, `coop net forget`) or already gated.
- 2026-09-13 — updated the recoverable approval path to `coop approve`; the confirmation boundary
  is unchanged.
- 2026-09-11 — clarified future-tense previews and short confirmations after the user rejected
  a 'Permanently deleted' ledger printed before asking. Swept account and service-volume examples,
  plus task/fork deletion siblings in the design task; preserved the single gate and all authority
  checks. Runtime helper copy updates remain implementation work.
- 2026-08-10 — path-only, no claim change: the fork/fleet extraction moved five of the ten
  `ui.DestroyGate` call sites out of `internal/cli` into `internal/forkctl` (`rm.go`, `fleet.go`,
  `merge.go`); `internal/cli/fork_cmd.go` keeps the `--fresh` one and `profiles.go` its own. Still
  exactly ONE definition (`internal/ui/confirm.go`) — the extraction consumed the shared gate
  instead of minting the third copy this card was rewritten to prevent. `check:` is still `none`.
- 2026-07-02 — created
- 2026-07-14 — revised
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: read `destroyGate` (util.go) and exhaustively
  enumerated + traced all 17 `os.RemoveAll` call sites in internal/cli (non-test). 0 violations —
  every user-state deletion (`backlogFolderRemove`, `removeProfile`, `tasksAcrossQueues`,
  `tasksFolderRemove` twice, `runDecisionBrowser`, `forkCreate`, `forkRm`, `forkMerge`,
  `pruneFleet`) is gated by `destroyGate` immediately before it; `removeTaskTmp`'s tmp/ cleanup
  matches the documented exception exactly (refuses a non-directory/symlinked task folder, unlinks a
  tmp symlink instead of following it); the rest (`forkReviewCandidate.cleanup` a scratch review
  clone, `spawnBox`'s ACP cid bookkeeping, doctor.go/selfupdate.go/sign.go temp dirs, controller.go
  tree snapshots, `splitTodoFolders`' disposable rebuildable split slices,
  `copyLegacyLeaseAuthorityRoot`'s lease-adoption staging) are ephemeral/derived/internal state
  outside "user state," not gate-bypasses.
- 2026-08-10 — the card's central claim had gone false: the `internal/tasks` extraction left a
  SECOND identical `destroyGate` (`internal/tasks/util.go`), and the pending fork/fleet extraction
  would have minted a third. Single-sourced into `internal/ui` as `ui.DestroyGate` (+ `ui.Confirm`,
  `ui.ConfirmationResponse`), byte-identical prompt and errors; both copies plus their duplicate
  `confirm`/`confirmationResponse` deleted. Zero import-DAG change — `ui` is a leaf and both
  consumers already held the edge. Swept the tree: exactly ONE definition remains, with all 10 call
  sites repointed — `internal/cli`'s `forkCreate`, `forkRm`, `pruneFleet`, `forkMerge` and
  `removeProfile`, and `internal/tasks`' `backlogFolderRemove`, `tasksFolderRemove` (two),
  `runDecisionBrowser` and `tasksAcrossQueues`. (The
  2026-08-09 entry's `tasks.go`/`taskcmd.go` paths are that extraction's pre-move names; the same
  sites are now `internal/tasks/queue.go` and `cmd.go`.) The gate's test moved with it to
  `internal/ui/confirm_test.go`. `check:` is still `none` — nothing mechanical catches a fourth copy.
- 2026-09-10 — named the recoverable-delete exception after `coop net forget` landed with its own
  preview-and-confirm: the gate's "can't be undone" wording and its `--yes` were both wrong for an
  approval a human recreates with `coop net approve`. Swept the other destructive verbs; every one
  of them (tasks rm, profiles rm, fork rm) destroys state no single command restores, so none moves.
