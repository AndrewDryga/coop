---
name: loop-failover-profiles
description: "loop, editor ACP, and remote sessions share target rotation but keep distinct continuation semantics"
scope: architecture
sources: [internal/ladder/ladder.go, internal/cli/rotation.go, internal/loop/rotation.go, internal/loop/loop.go, internal/acpctl/control.go, internal/sessionsvc/acp.go, internal/session/store.go]
check: "none"
updated: 2026-09-03
---

# Preserve each surface's session lifecycle when rotating targets

All three long-running execution surfaces select the same unit: a complete target
(`provider[:model][/effort][@account]`). They reuse ladder and rate-limit primitives where the
policy matches, but continuation and authentication behavior remain surface-owned:

- The unattended loop changes the active provider, model, effort, and account before the next
  headless attempt. It does not carry a provider chat across rungs; durable continuity is the task
  folder and Git. A bare target fans out across runnable accounts, and a cross-provider ladder may
  change the provider as well as the subscription. A proven authentication failure retires that
  rung for the run.
- Editor ACP owns a live editor session. A target change may restart its box and recreate the
  provider session; cross-provider recreation carries bounded text history best-effort and an
  in-flight prompt is re-sent after the replacement is ready. When every usable rung is cooling,
  ACP can wait and resume transparently rather than abandoning the editor request. An automatic
  plain-account selection may move past a failed credential, while pinned and preset selections
  surface the login repair instead.
- The remote-session service owns a durable session row. A rate limit or an explicit target floor
  advances that recorded target transactionally, rewinds the turn delivery ledger, and clears a
  provider-native session ID when the provider or account changes. When no rung is available it
  records a `rate_limited` turn result and leaves retry timing to the client. Expired or revoked
  credentials never rotate; they surface so an operator repairs them.

**Why:** these surfaces used to be described as one profile swap with the remote-session API as the
only stateful exception. That became dangerous once full target ladders and editor ACP recovery
landed: following the stale card would remove current cross-provider fallback, conversation carry,
or durable replay behavior as if it were obsolete compatibility.

**How to apply:**
- Build and log rotation in terms of the complete target, never only an account/profile.
- Keep compatible target/limit mechanics shared, but leave authentication, retry, wait, recreation,
  transcript carry, and durable-write policy with the owning surface.
- Do not infer a limit from arbitrary prose. Each provider's structured/owned signals feed the
  shared detector, and a loop rotation still requires a failed attempt.
- Resolve every selected account through the private credential store; targets may name accounts,
  but credentials themselves never enter repository state.

Related: [[model-is-the-rotation-axis]] and [[credentials-not-profiles]].

## Changelog
- 2026-09-03 — rewrote the card after sweeping the loop, ACP control, and remote-session runner.
  Replaced the retired profile-only/remote-only model with the three current continuation
  lifecycles; added the ACP and durable-store sources. No runtime behavior changed.
- 2026-08-17 — added authentication-failure rotation and preflight credential filtering.
- 2026-08-15 — recorded remote-session `min_target_index` escalation.
- 2026-08-10 — moved unattended-loop rotation policy into `internal/loop`.
- 2026-08-09 — moved shared limit detection into `internal/ladder` and the session service into
  `internal/sessionsvc`.
- 2026-08-07 — added the durable remote-session rotation exception.
- 2026-06-17 — created; revised 2026-07-11.
