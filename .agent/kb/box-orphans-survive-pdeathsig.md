---
name: box-orphans-survive-pdeathsig
description: Pdeathsig is not inherited across fork, so a forking child (Chromium) orphans into a box and holds it open for the whole descendant drain
subsystem: box
sources: [internal/box/supervisor/main.go, internal/loop/watchdog.go, internal/loop/loop.go, internal/box/lifeline.go]
updated: 2026-11-21
---
When a box exits with descendants still alive, the drain names them (see
[[loop-resume-never-rewrites-history]]). The usual reflex — "give the child `Pdeathsig` so the
kernel kills it when its owner dies" — is **not sufficient**, and it is worth knowing why before
spending a fix on it.

`PR_SET_PDEATHSIG` applies to the **immediate child only, and is not inherited across `fork`**. A
process that forks its real worker (Chromium's `chromium-headless-shell` forks the browser process;
so do many launchers) leaves that worker with no death signal at all. Measured in the box image on
2026-08-01: a chromedp `ExecAllocator` session — which sets `Pdeathsig = SIGKILL` on Linux by
default — still left a live browser tree at PPID 1 after its owning process was SIGKILLed.

The same limit applies to a group kill registered in a Go `Cancel` hook or a `defer`: **no Go code
runs on SIGKILL**, which is exactly how the provider watchdog kills a wedged attempt. So neither
mechanism protects the case that actually produces orphans.

What this means for coop:
- Orphaned descendants WILL happen for any repo whose tooling forks a browser or similar worker.
  The box supervisor's `procFS.terminate` (`internal/box/supervisor/main.go`) is the backstop
  that actually collects them, and it works.
- The cost is the drain wait plus a `background_*` handoff, which un-completes an already finished
  task and re-runs it; three handoffs on one task stop the loop, and a provider timeout in between does
  not reset the count (`maxBackgroundHandoffs`, see [[box-entrypoint-descendant-handoff]]). So a repo that leaks on every
  browser-touching task can stall a queue even though each individual box is cleaned up correctly.
- Fixing it in the repo needs a mechanism the kernel enforces over the whole tree — an inherited fd
  whose close terminates the worker (Chromium's `--remote-debugging-pipe`, versus a debugging
  **port**, which does not tie lifetime to the parent) — not a signal on one process.

Do not "fix" this by adding `chromedp.ModifyCmdFunc` to set `Pdeathsig`: chromedp already sets it,
and `ModifyCmdFunc` REPLACES that default rather than adding to it, so it would remove the (partial)
protection that exists.

The same "no signal saves you" limit applies one level up — a SIGKILLed host coop never fires
`--rm`, so the BOX itself orphans. That one is not fixable by a death signal either. It is fixed the
way the browser case above says: by an fd whose close the kernel enforces. Each launch's lifeline
(`internal/box/lifeline.go`) holds the read end of a pipe only that coop can write; its EOF without
"quit" means coop is gone, and the lifeline removes the run's containers once the box's own
supervisor label proves it. The next run's sweep stays the backstop; see
[[box-supervisor-label-and-orphan-sweep]].

## Changelog
- 2026-08-01 — created: measured that a chromedp isolated session survives its owner's SIGKILL, and traced it to Pdeathsig not being inherited across fork.
- 2026-08-09 — re-verified against `sources` (unchanged); linked the host-side counterpart, [[box-supervisor-label-and-orphan-sweep]].
- 2026-08-10 — sources repointed: the loop engine moved out of `internal/cli` into `internal/loop` — `watchdog.go` whole and
  `commands.go`'s drain handling into `loop.go`; re-verified, both unchanged.
- 2026-11-17 — the backstop is the supervisor's `procFS.terminate`, not the retired entrypoint's
  `terminate_jobs`; the handoff bound is three per task, not reset by a timeout. Sources repointed.
- 2026-11-21 — a killed host coop's box is now removed within seconds by its lifeline (a pipe EOF,
  checked against the supervisor label); the next run's sweep is the backstop, no longer the only cure.
