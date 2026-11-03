---
name: sign-in-again-is-actionable
description: refreshable OAuth stays usable; "Sign in again" means the stored login cannot recover (a provider-refused grant included) and every command words it the same — run the coop login it names
subsystem: credentials
sources: [internal/agent/agent.go, internal/agent/target.go, internal/agent/claude.go, internal/agent/codex.go, internal/agent/grok.go, internal/box/profiles.go, internal/box/account_renewal.go, internal/box/native_account.go, internal/box/native_broker.go, internal/cli/profiles.go, internal/cli/commands.go, internal/cli/modelscache.go, internal/cli/usage.go, internal/cli/rotation.go, internal/sessionsvc/acp.go]
updated: 2026-11-03
---
`coop credentials` lists an account whose OAuth access token has expired but whose refresh authority
is valid as healthy: Coop renews it on use, so the row carries no warning. A marker or canonical
record that is malformed, stripped, missing required scopes or routing metadata, or both expired and
nonrenewable reads `⚠ Sign in again`, as does an account whose grant the provider refused at its last
renewal (an account that never signed in reads `⚠ Not signed in`), with the
`coop login <agent>@<account>` remedy beside it. That flag is an actionable failure, not the old
expiry false alarm; run the displayed `coop login` remedy rather than trying a provider request first.

One local answer, worded once. `box.ProfileSignIn` decides the state from local files alone and
`agents.SignIn` words it: `Label` for the listing, `Remedy` for one line
(`Sign in again: coop login grok@default`). The listing and detail view, the model menu's refresh
cause (`fetchModelCatalog`, `safeModelProbeCause`), `coop usage`'s Limits row, the launch heads-ups
(`nudgeIfUnauthed`, `selectRunProfile`, a new default) and a launch refused in `planNativeAccounts`
(what an editor's warm box prints) all use it. The quota adapters say only `credential refresh
unavailable` for a failed renewal, and `coop usage` asks `ProfileSignIn` again after its lookup.
Rotation's repair errors classify with it but keep their own sentence, and the ACP control's
in-editor `Sign in to <target> on the host` notices are separate.

The refusal record. A canonical record whose grant the provider refused (`agents.ErrRenewalRefused`:
OAuth `invalid_grant`, Codex's `refresh_token_*`) still inspects as renewable, so
`renewAccountAuthority` writes `refused.json` (the record's epoch and revision) beside the account,
under its lock. `nativeAccountReady` reads a matching one as not ready, so presence, readiness,
rotation, the ACP selector and the warm pool drop the account, and later renewals return the refusal
without asking the provider. A publication (a new `coop login`, a kept answer that completes) moves
the revision and ends it; `coop credentials … rm` deletes it. A refusal is learned only when
something renews (a launch, a warm box, `coop usage`), so a login that died since its last use still
lists as healthy until then: `coop credentials` never asks a provider.

A held lock is not a broken sign-in. A readiness read waits one second for the account's lock, and
a renewal asking its provider (or a sign-in or removal) can hold it longer. `nativeAccountReady` then
reads the account as ready (`accountBusy`: the lock wait gave up, nothing else did), so every
command calls it signed in; a launch waits for the lock and judges what that process left. A damaged
or refused record still reads `Sign in again` once it can be read.

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
- 2026-11-03 — a listing taken while a renewal held an account's lock showed `Sign in again` for a
  healthy account; a busy lock now reads as signed in, and `ProfileTokenMtime` dates the record
  without waiting 30 seconds for that lock.
- 2026-11-03 — one expired Grok login read four ways (healthy in the listing, three different
  sign-in sentences in models, usage and an ACP warm box). Added the refusal record and
  `box.ProfileSignIn`/`agents.SignIn` (replacing `profileIssue`, `signInLabel`, `signInToRefresh`);
  a not-ready canonical account now reads `Sign in again`, not `Not signed in`. Swept every caller
  of `ProfileAuthed` that tells a person to sign in: models, usage, both launch heads-ups and
  rotation's repair now use it; help, init and peer checks keep presence.
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
