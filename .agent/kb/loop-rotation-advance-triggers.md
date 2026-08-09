---
name: loop-rotation-advance-triggers
description: the loop rotation advances on rate limits (time-keyed, self-healing) and auth failures (sticky for the run); rungs are built from credential presence, not validity
subsystem: loop
sources: [internal/ladder/ladder.go, internal/ladder/limit.go, internal/cli/rotation.go, internal/cli/ratelimit.go, internal/cli/commands.go, internal/agent/claude.go, internal/box/auth.go]
updated: 2026-08-09
---
A loop's rotation is built from credential **presence**, never validity: `expandLadder` →
`accountsFor` → `box.ProfileAuthed`, which `internal/box/auth.go:15` calls "a presence heuristic,
not a live validity check". The adapter check that *can* tell a dead login from a refreshable one,
`StoredCredentialStatus`, has exactly one non-test caller — the `coop credentials` display at
`internal/cli/profiles.go:121`. So an account `coop credentials` already prints as
`re-login required` is still built in as a rung, and often as rung #1 (the marked default sorts
first). See [[credential-presence-is-adapter-declared]] and
[[credentials-expired-is-a-false-alarm]].

Two triggers advance the rotation, and their lifetimes differ — don't reach for one map to hold
both:
- **rate limit** → `limited map[string]time.Time`, time-keyed and self-healing. `ClearExpired`
  drops the mark once the reset passes, so a cooled rung comes straight back around.
- **auth failure** → `authFailed map[string]bool`, **sticky for the whole run**. Nothing revives a
  logged-out account but a human re-login, and the box mounts credentials at *launch*, so a
  re-login mid-run is not picked up until the loop restarts. Stickiness is also what bounds the
  policy: at most one rotation per rung, so it cannot spin.

`selectTarget` and `AdvanceOnTimeout` both filter through `free`/`live`, so a rate-limit or timeout
rotation can never wander back onto an auth-dead rung. When every rung has failed auth the loop
stops and names all of them — restoring only the last one tried would hit the same wall on the next.
A single-rung run still fails fast, exactly as before.

**The trap:** an auth failure only reaches this path if the provider adapter's `AuthSignals` match
the CLI's actual wording. `iterationAuthentication` anchors each signal at line start (exact, or
`signal:`/`signal.`, or inside an `error:`/`fatal:`/JSON line) — deliberately, so ordinary prose
mentioning a signal isn't a terminal failure. The cost is that a provider rewords its message and
the failure silently downgrades to a generic `process_failure`, which burns the loop's whole retry
budget on a rung no retry can fix. That is exactly how an expired claude refresh token
("Failed to authenticate: OAuth session expired and could not be refreshed", matched by none of the
then-current signals) killed a 133-task overnight run while a signed-in second account sat idle in
the same rotation. When a provider's auth wording changes, the signal list is the thing to update.

## Changelog
- 2026-08-09 — re-verified; the CURSOR (both maps, `free`/`live`, `selectTarget`,
  `AdvanceOnTimeout`, `OnAuthFailure`) is now `internal/ladder` — a pure leaf the loop, the ACP
  control, and the sessions API share. Rung MEMBERSHIP is unchanged and still cli's:
  `expandLadder` → `accountsFor` → `box.ProfileAuthed`, as is `iterationAuthentication`, so both
  traps below read exactly as before.
- 2026-07-31 — created: auth failures now rotate instead of stopping the loop; documents the two trigger lifetimes, presence-vs-validity rung membership, and the AuthSignals wording trap.
