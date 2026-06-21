# Loop failover swaps the active credential profile, never a session

When the unattended loop hits a rate/usage limit and more than one credential profile is
available, it switches the *active profile* and retries the same task item on the next
subscription — it does not resume or carry a conversation. This is correct because **the
loop has no session**: each iteration is a fresh `Headless` run (`claude -p …`), and
continuity lives entirely in `.agent/TASKS.md` + git, not a chat thread. Resuming a
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
Profile *names* are the only thing stored per-repo (`pools.json`, keyed by repo path), and
that lives outside the repo tree — a credential never lands where it could be committed
(this is the tool whose job is catching exactly that).

**How to apply / extend:**
- Anything that needs "the agent's home for this run" goes through `cfg.AgentDir`, never a
  hand-built `filepath.Join(ConfigDir, agent)` — that join is the seam the active profile
  rides on, and bypassing it pins you to the default profile.
- Rotation triggers only on a detected rate limit with a *non-zero exit* (`decideIteration`
  gates on `code != 0` by design, so a coding loop that prints "rate limit" in a diff
  doesn't falsely rotate). A new agent whose limit output isn't caught → add a marker to
  `detectLimit` (ratelimit.go); don't loosen the exit gate.
- Keep rotation strictly rate-limit-driven. An expired/revoked credential looks like a
  failure, not a limit, so it surfaces instead of rotating — intended for v1.
- A free rotation resets the wait counter; only consecutive *all-profiles-limited* waits
  count toward the stop cap. Otherwise a healthy multi-account run would trip the cap.
- Never put a credential (only profile names) in a repo or in `pools.json`.
