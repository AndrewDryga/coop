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
real fix (per-box private home, `Agent.SharedHomePaths`) kept every session and removed the
collision entirely — nothing left to lock.

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
