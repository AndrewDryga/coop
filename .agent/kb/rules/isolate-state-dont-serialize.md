---
name: isolate-state-dont-serialize
description: "when shared state breaks concurrency, isolate the state; never lock the users of it"
scope: box
sources: [internal/box/profiles.go, internal/box/run.go, internal/agent/codex.go]
check: "none"
updated: 2026-08-09
---

# Isolate the state, don't serialize the users of it

**The rule:** when shared mutable state breaks concurrency (codex ≥0.144's single-writer
sqlite in `~/.codex` crashing the second box on an account), the fix is to STOP SHARING the
state — give each consumer its own copy and share only what must be common (the credential,
durable user content) — never to serialize the consumers with a lock. Parallel sessions are
the product; a lock that "protects" state nobody wanted shared is a capability regression
wearing a safety vest.

**Why:** the first fix for the codex sqlite collision was a per-account flock that made the
second box fail fast. The user rejected it flat ("lock is a stupid idea — I want multiple
sessions working in parallel"), and it also compounded failures: a respawn racing its own
half-dead predecessor burned the ACP proxy's rapid-fail cap and killed the whole server. The
real fix — first a per-box private home, ultimately the surgical `CODEX_SQLITE_HOME`
redirect described below — kept every session and removed the collision entirely; nothing
left to lock.

**How to apply:**
- Contention on a mounted dir/file → first ask "does anyone WANT this shared?" Split the
  answer: isolate the per-consumer state, keep sharing the genuinely-common pieces.
- **Check whether the tool already has a knob before building a mount dance.** The codex
  collision's real fix was one env var — codex exposes `CODEX_SQLITE_HOME` to relocate exactly
  the single-writer sqlite off the shared home, so coop points it at a container-local path and
  leaves the whole home (auth + its in-place refresh, sessions, config) shared and UNTOUCHED.
  That beat the first working design (a private per-box home seeded from the profile with a
  single-file `auth.json` bind): fewer moving parts, and it makes no assumption about how the
  credential is written. Google the upstream issue tracker before inventing an isolation layer —
  the community usually hit it first (here: per-`CODEX_HOME` isolation is the documented pattern,
  and `sqlite_home`/`CODEX_SQLITE_HOME` the surgical one).
- Prefer redirecting the STATE to isolating the whole HOME: the less you move off the shared
  mount, the less can silently diverge (a credential refresh, a config edit).
- A guard that can fire on a respawn/retry path multiplies: fail-fast checks at spawn time
  interact with supervisor respawn loops (rapid-fail caps). If a guard is ever needed, it
  must be idempotent across generations of the same logical session.

## Changelog
- 2026-07-12 — created
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: confirmed `CODEX_SQLITE_HOME` still wired exactly as
  described (internal/agent/codex.go:671). Checked the `syscall.Flock` at codex.go:281-284: it
  serializes concurrent WRITES to one credential's own `.refresh.lock` in trusted host storage
  (renewCodexCredential) — not the rejected per-account session lock; matches the rule's own
  exception ("share only what must be common — the credential"), not a violation. 2 findings on
  the card itself (not the code): (1) `sources: internal/box/mounts.go` is stale/wrong —
  mounts.go handles repo-content `.coopignore` shadowing; the per-profile isolation this rule
  describes lives in internal/box/profiles.go (`EffectiveProfiles`/`ProfileAuthed`) and the mount
  wiring in internal/box/run.go:1574 (`cfg.AgentDir(agent)`, one active-profile dir per box —
  `make rules-check` only verifies the path exists, not its relevance, so it can't catch this).
  (2) The "Why" section's `Agent.SharedHomePaths` no longer exists anywhere in the tree (0 hits)
  — it named the FIRST fix's mechanism (a private per-box home), which the card's own "How to
  apply" section already says was superseded by the simpler `CODEX_SQLITE_HOME` env var. Both
  flagged for the lead as a card correction (re-point sources, refresh the Why); the rule's actual
  guidance still holds in the current code.
- 2026-08-09 — drift repair from the backfill sweep's findings: sources repointed (mounts.go was unrelated; profiles.go + run.go are the real logic) and the dead Agent.SharedHomePaths reference replaced with the fix's actual history.
