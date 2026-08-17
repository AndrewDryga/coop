---
name: loop-rotation-advance-triggers
description: the loop rotation advances on rate limits (time-keyed, self-healing) and auth failures (sticky for the run); known-invalid credentials never become rungs
subsystem: loop
sources: [internal/ladder/ladder.go, internal/ladder/limit.go, internal/cli/rotation.go, internal/loop/rotation.go, internal/loop/ratelimit.go, internal/loop/loop.go, internal/acpctl/control.go, internal/agent/agent.go, internal/agent/claude.go, internal/box/auth.go, internal/box/profiles.go]
updated: 2026-08-17
---
A loop's rotation starts from credential presence and then applies
`box.ProfileCredentialReady`. A native marker that its adapter classifies as
`StoredCredentialReauthRequired` is omitted before the provider launches; ready or refreshable
markers remain eligible. Env-backed credentials and markers whose adapter returns `Unknown` retain
presence behavior because Coop cannot safely prove them dead. `coop credentials` uses the same
predicate, so a credential shown as `re-login required` cannot simultaneously become a rotation
rung. See [[credential-presence-is-adapter-declared]] and
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
the CLI's actual wording. `agent.AuthenticationFailure` anchors each signal at line start (exact, or
`signal:`/`signal.`, or inside an `error:`/`fatal:`/JSON line) — deliberately, so ordinary prose
mentioning a signal isn't a terminal failure. The loop and ACP notice gate share that classifier;
ACP terminal recovery separately requires its structured error shape. The cost is that a provider
rewords its message and the loop failure silently downgrades to a generic `process_failure`, which
burns the whole retry budget on a rung no retry can fix. That is exactly how an expired claude token
("Failed to authenticate: OAuth session expired and could not be refreshed", matched by none of the
then-current signals) killed a 133-task overnight run while a signed-in second account sat idle in
the same rotation. When a provider's auth wording changes, the signal list is the thing to update.

## Changelog
- 2026-08-17 — changed rung membership to exclude adapter-inspectable re-login credentials before
  launch; moved provider prose matching behind the shared adapter-owned classifier used by loop and
  ACP notices; re-verified env/opaque fallbacks and replaced marker-only process fixtures
- 2026-08-09 — re-verified; the CURSOR (both maps, `free`/`live`, `selectTarget`,
  `AdvanceOnTimeout`, `OnAuthFailure`) is now `internal/ladder` — a pure leaf the loop, the ACP
  control, and the sessions API share. Rung MEMBERSHIP is unchanged and still cli's:
  `expandLadder` → `accountsFor` → `box.ProfileAuthed`, as is `iterationAuthentication`, so both
  traps below read exactly as before.
- 2026-07-31 — created: auth failures now rotate instead of stopping the loop; documents the two trigger lifetimes, presence-vs-validity rung membership, and the AuthSignals wording trap.
- 2026-08-10 — sources repointed for the loop-engine extraction: rotation is now SPLIT — ladder
  EXPANSION (`expandLadder`/`accountsFor`/`buildRotation`, the credential-presence half this card
  opens with) stays in `internal/cli/rotation.go`, while APPLYING a target and rotating on a limit
  (`applyTarget`/`rotateOnLimit`) moved to `internal/loop/rotation.go`. Advance triggers unchanged.
