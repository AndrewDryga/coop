---
name: renew-before-access-only-projection
description: refreshable credentials renew in one canonical host authority before and during brokered runs
scope: security
sources: [internal/agent/agent.go, internal/agent/codex.go, internal/agent/claude.go, internal/agent/gemini.go, internal/agent/grok.go, internal/box/account_renewal.go, internal/box/native_broker.go, internal/sessionsvc/acp.go]
check: "go test ./internal/box -run 'TestAccountRenewalPersistsIntentResponseAndShortGrant|TestAccountRenewalUncertainResponseCannotRetryOrMutate|TestNativeRunPublishesGuardOnlyExpiringAccess|TestNativeHostSignInRecoversUncertainRenewal'"
updated: 2026-10-09
---

# Renew credentials in one canonical host authority

Renew expiring grants before and throughout brokered runs. Serialize renewal and retain issued
rotations durably before validation/publication, including uncertain responses. Never copy refresh
authority into repository homes or workloads, and never replace continuous renewal with a turn-sized
expiry check or forced restart. Private broker snapshots expire closed if host publication stops.

**Why:** Responder surfaced `credential is not portable through the turn deadline` even though the
managed Codex profile retained valid refresh authority. Readiness and execution had implemented
different halves of the credential lifecycle.

**How to apply:** Keep credential parsing and renewal adapter-owned through `NativeCredentialSpec`.
All coding/remote paths use the canonical host authority and public native selectors. Revocation
invalidates the prior epoch; neither a stale profile nor an env token may revive it. A revoked or
uncertain renewal is an authentication/recovery result, never a blind process-crash retry.

Pre-cutover quota inspection may still renew the original legacy store under the same writer
fences as cutover. It must never fork that authority, overlap canonical service, or fall back from
a revoked canonical account. This narrow maintenance path is not a coding-home projection.

## Changelog
- 2026-10-09 — swept all four adapters, native account renewal, run publication and remote startup.
  Updated the mechanism to canonical host renewal and expiring broker snapshots; legacy renewal
  checks remain applicable only to fenced pre-cutover quota maintenance.
- 2026-09-18 — Grok had the defect this card names: `Portability` and no `Prepare`, so a remote
  session failed once its six-hour token aged until a local run happened to refresh the profile.
  Wired `renewGrokCredential`. The grant shape was captured from the pinned 1.0.25 binary against
  a logging issuer rather than recalled — it adds `principal_type` and `principal_id` to the
  standard form — and it is sent only to the pinned `https://auth.x.ai/oauth2/token`, because the
  stored issuer is box-writable. It takes the client's own flock on `auth.json.lock`. Proved live:
  coop's renewal rotated the real login, and the pinned client then ran on it without refreshing.
  Mechanized the rule as `TestEveryProjectableCredentialCanBeRenewed`; swept every adapter:
  claude, codex and grok renew, and gemini's projection is never portable.
- 2026-08-09 — sources repointed: the sessions service moved out of `internal/cli/session_*.go` into `internal/sessionsvc/`; the facts here are unchanged (a move-only extraction).
- 2026-08-08 — wired the Claude adapter to the boundary. It had shipped with `Portability` and no `Prepare`, so an ~8h OAuth token expired into a hard turn failure while the source profile still held valid refresh authority; two live Responder deployments were down on Claude rungs. Endpoint, client id, and grant shape were read out of the shipped Claude Code binary rather than recalled — the remembered endpoint was wrong
- 2026-08-06 — created after sweeping the Codex readiness, renewal, projection, and ACP admission paths; focused tests cover rotation, eight concurrent callers, failure preservation, symlink rejection, and access-only child state
