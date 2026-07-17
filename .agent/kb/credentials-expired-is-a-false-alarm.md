---
name: credentials-expired-is-a-false-alarm
description: refreshable OAuth stays signed in; re-login required means the stored login cannot recover
subsystem: credentials
sources: [internal/agent/agent.go, internal/agent/claude.go, internal/agent/grok.go, internal/box/profiles.go, internal/cli/profiles.go]
updated: 2026-07-16
---
`coop credentials` treats an expired OAuth access token with valid refresh authority as "signed in":
the provider CLI can renew it on use. A marker that is malformed, stripped, missing required scopes
or routing metadata, or both expired and nonrenewable reads "re-login required". That label is an
actionable failure, not the old expiry false alarm; run the displayed `coop login` remedy rather than
trying a provider request first.

The provider adapter owns this distinction through `StoredCredentialStatus`. Claude and Grok can
validate their native OAuth records; Codex and Gemini retain opaque presence behavior. Env-only
credentials also remain presence-based because there is no native marker to inspect. This source
readiness check does not widen live authority: the live projections use separate access-only output
types and never carry a refresh token into the box.

The `rotated <age>` column still reads the marker mtime through `box.ProfileTokenMtime`; a login or
refresh rewrite advances it. See [[box-time-is-utc]] for the wall clock behind provider expiries.

## Changelog
- 2026-07-16 — replaced the stale "try a run" advice with adapter-owned readiness and an authoritative re-login remedy
- 2026-07-12 — created: expired-but-renewable claude tokens still work in-box; `profileState` only reports "expired" when there's no refresh token.
