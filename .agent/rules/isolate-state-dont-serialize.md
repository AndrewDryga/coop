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
  answer: isolate the per-consumer state (scratch dir, per-box copy), bind through only the
  genuinely-common pieces one by one.
- What stays shared must be safe to share: `auth.json` stays a single bound FILE because
  codex rewrites it in place (inode stable) and its refresh tokens are single-use — private
  copies would strand siblings on a consumed token and brick the login. Verify the write
  pattern (in-place vs temp+rename) and the token semantics BEFORE choosing copy vs bind.
- A guard that can fire on a respawn/retry path multiplies: fail-fast checks at spawn time
  interact with supervisor respawn loops (rapid-fail caps). If a guard is ever needed, it
  must be idempotent across generations of the same logical session.
