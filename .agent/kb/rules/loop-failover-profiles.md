---
name: loop-failover-profiles
description: "in the loop, failover swaps the active credential and never a session; the session API is the one surface that rotates the session itself"
scope: loop
sources: [internal/ladder/limit.go, internal/cli/rotation.go, internal/loop/rotation.go, internal/loop/ratelimit.go, internal/config/config.go, internal/sessionsvc/acp.go]
check: "none"
updated: 2026-08-10
---

# Loop failover swaps the active credential profile, never a session

When the unattended loop hits a rate/usage limit and more than one credential profile is
available, it switches the *active profile* and retries the same task item on the next
subscription — it does not resume or carry a conversation. This is correct because **the
loop has no session**: each iteration is a fresh `Headless` run (`claude -p …`), and
continuity lives entirely in the task folder (`.agent/tasks/`, incl. the in-progress
task's `state.md`) + git, not a chat thread. Resuming a
session (the adapters' interactive `Resume`/`StartSession` path — `--resume <id>` /
`codex resume <id>`) is interactive-only; the loop never touches it. So swapping accounts mid-run costs nothing — that's the whole
reason failover is cheap, and why "but the session resets when the sub changes" doesn't
apply here.

The feature pivots on one seam. Every path that reads an agent's home — the box mount,
`AuthedAgents`, `EnsureDefaults`, and the adapters' own command-building — resolves
`cfg.AgentDir(agent)`. Making that resolve to the *active profile* under
`<agent>/profiles/<name>/` makes the whole machinery profile-aware with no
adapter-interface change and no `RunSpec` change. The loop rotates by calling
`SetActiveProfile` between iterations; the next mount and agent command follow it for free.

**Why it's safe and sealed:** only the active profile's dir is mounted at `~/.<agent>`, so
a running agent can read just the one account it's using — never the rest of the vault.
There is no per-repo pool file anymore: the rotation is expanded at loop start from the
`agent:` ladder in play (a loop.yaml step, a preset's lead, or the command-line target)
against the signed-in accounts. A ladder names *accounts* (`provider:model@account`), never their logins —
the credentials themselves stay in the vault outside the repo, so nothing lands where it
could be committed (this is the tool whose job is catching exactly that).

**The session API is the one surface where failover DOES move a session.** `coop sessions serve`
holds durable sessions, so it cannot borrow the loop's "nothing to lose" reasoning. There, a
rate-limited turn rotates the SESSION onto the next rung of its policy ladder
(`Store.RotateTurnTarget`): the durable `target` becomes the new rung, a cross-provider hop clears
`native_session_id` because the new provider cannot load the old transcript, and the turn's
delivery ledger is rewound so the same prompt can be sent again — all in one transaction. It also
does NOT wait out an all-rungs-limited ladder the way the editor path does; it fails the turn with
`rate_limited` and lets the client's own backoff own the retry. Cooldowns still live only in
memory, as below.

**How to apply / extend:**
- Anything that needs "the agent's home for this run" goes through `cfg.AgentDir`, never a
  hand-built `filepath.Join(ConfigDir, agent)` — that join is the seam the active profile
  rides on, and bypassing it pins you to the default profile.
- Rotation triggers only on a detected rate limit with a *non-zero exit* (`decideIteration`
  gates on `code != 0` by design, so a coding loop that prints "rate limit" in a diff
  doesn't falsely rotate). A new agent whose limit output isn't caught → add a marker to
  `ladder.DetectLimit` (internal/ladder/limit.go); don't loosen the exit gate.
- Keep rotation strictly rate-limit-driven. An expired/revoked credential looks like a
  failure, not a limit, so it surfaces instead of rotating — intended for v1.
- A free rotation resets the wait counter; only consecutive *all-profiles-limited* waits
  count toward the stop cap. Otherwise a healthy multi-account run would trip the cap.
- Never put a credential in a repo. A preset's `agent:` ladder may name accounts
  (`model@account`), but the logins themselves stay in the vault, never committed.

## Changelog
- 2026-08-09 — sources repointed: the sessions service moved out of `internal/cli/session_*.go` into `internal/sessionsvc/`; the facts here are unchanged (a move-only extraction).
- 2026-06-17 — created
- 2026-07-11 — revised
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-07 — recorded the session API exception: it rotates the session's rung, not the active
  credential, and fails instead of waiting when the whole ladder is cooling. The old title claim
  ("never a session") was true only while the loop was the sole failover surface.
- 2026-08-09 — re-verified; limit DETECTION moved to the `internal/ladder` leaf
  (`ladder.DetectLimit`), so the loop, the ACP control, and the session API now classify a limit
  through one shared function. The rule itself is unchanged: `decideIteration`'s non-zero-exit gate
  and the rotate-only-on-a-limit policy stayed in `internal/cli`.
- 2026-08-09 — validate-on-write backfill: grepped for a hand-built `filepath.Join(cfg.ConfigDir,
  agent, ...)` that bypasses `cfg.AgentDir`. Every hit is legitimate: config.go:567
  (`AgentProfileDir`, the primitive `AgentDir` itself calls) and :573 (`Profiles`, a read-only
  name-listing helper) aren't bypasses, and box/run.go:1517 (`acpSharedDir`, the ACP
  session-transcript store) is deliberately credential-independent BY DESIGN so a transcript
  survives a mid-session account switch — the rule's own "share only what must be common"
  pattern, not a violation. Also reconfirmed `ladder.DetectLimit`'s three callers and
  `decideIteration`'s non-zero-exit gate (ratelimit.go:276). 0 violations.
- 2026-08-10 — sources repointed for the loop-engine extraction: the rotate-on-a-limit policy this
  rule is about (`rotateOnLimit`, `decideIteration`, `applyTarget`'s `SetActiveProfile`) is now
  `internal/loop/`; `internal/cli/rotation.go` keeps ladder EXPANSION. The rule is unchanged — the
  loop still swaps the active credential profile and never a session, and the session API is still
  the one surface that rotates the session itself.
