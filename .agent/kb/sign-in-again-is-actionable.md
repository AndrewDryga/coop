---
name: sign-in-again-is-actionable
description: refreshable OAuth stays usable; "Sign in again" means the stored login cannot recover — run the coop login it names
subsystem: credentials
sources: [internal/agent/agent.go, internal/agent/claude.go, internal/agent/codex.go, internal/agent/grok.go, internal/box/profiles.go, internal/cli/profiles.go, internal/cli/rotation.go, internal/sessionsvc/acp.go]
updated: 2026-10-17
---
`coop credentials` lists an account whose OAuth access token has expired but whose refresh authority
is valid as healthy: the provider CLI renews it on use, so the row carries no warning. A marker that
is malformed, stripped, missing required scopes or routing metadata, or both expired and
nonrenewable reads `⚠ Sign in again` (an account that never signed in reads `⚠ Not signed in`), with
the `coop login <agent>@<account>` remedy beside it (`profileIssue` in internal/cli/profiles.go).
That flag is an actionable failure, not the old expiry false alarm; run the displayed `coop login`
remedy rather than trying a provider request first.

The provider adapter owns this distinction through `StoredCredentialStatus`. Claude, Codex, and
Grok validate their native OAuth records; Gemini retains opaque presence behavior. Env-only
credentials also remain presence-based because there is no native marker to inspect. Runnable
account ladders exclude a marker that is known to require another login before launching a provider;
if a pinned account or every candidate is excluded, the error names the exact
`coop login provider@account` remedy. Before an ACP turn, Codex and Claude each renew an expiring
access token while the complete credential is still in trusted host storage, persist any token
rotation atomically, and only then let the access-only box projection proceed. Refresh authority
never crosses into the box.

A healthy row's `refreshed <age>` reads the marker mtime through `box.ProfileTokenMtime`
(`credentialAge`); a login or refresh rewrite advances it. An env-backed account says
`environment file` instead, having no token file to date. See [[box-time-is-utc]] for the wall clock
behind provider expiries.

## Changelog
- 2026-10-17 — renamed from credentials-expired-is-a-false-alarm, whose name said the opposite of
  the body: the flag is actionable. The listing's labels are now `⚠ Sign in again`/`⚠ Not signed in`
  and a healthy row's column is `refreshed <age>`, not `rotated` (9f5b033c); verified against
  `accountBlock`, `profileIssue` and `credentialAge` in internal/cli/profiles.go and
  `ProfileCredentialReady` in internal/box/profiles.go; the 2026-07-12 entry's `profileState` is
  noted as today's `profileIssue`.
- 2026-08-17 — made known re-login state an input to runnable account ladders and their actionable
  empty-ladder error while preserving opaque and env-only presence behavior
- 2026-08-09 — this card still singled out Codex for pre-ACP renewal after Claude joined it (`renewClaudeCredential`, 2026-08-08); reworded the paragraph to name both, no behavior change.
- 2026-08-09 — sources repointed: the sessions service moved out of `internal/cli/session_*.go` into `internal/sessionsvc/`; the facts here are unchanged (a move-only extraction).
- 2026-08-06 — added serialized host-side Codex renewal before access-only ACP projection; swept the credential preparation and projection paths with focused concurrency and failure tests
- 2026-07-16 — replaced the stale "try a run" advice with adapter-owned readiness and an authoritative re-login remedy
- 2026-07-12 — created: expired-but-renewable claude tokens still work in-box; `profileState` (since 2026-09-11 `profileIssue`) only reports "expired" when there's no refresh token.
