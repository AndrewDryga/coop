---
name: renew-before-access-only-projection
description: refreshable credentials renew in one canonical host authority before and during brokered runs
scope: security
sources: [internal/agent/agent.go, internal/agent/native_credentials.go, internal/agent/codex.go, internal/agent/claude.go, internal/agent/gemini.go, internal/agent/grok.go, internal/box/account_renewal.go, internal/box/native_broker.go, internal/sessionsvc/acp.go]
check: "go test ./internal/box -run 'TestAccountRenewalPersistsIntentResponseAndShortGrant|TestAccountRenewalCompletesAKeptAnswerWithoutTheProvider|TestAccountRenewalRetriesAfterAnExchangeThatKeptNothing|TestAccountRenewalUnusableKeptAnswerRefuses|TestNativeRunPublishesGuardOnlyExpiringAccess|TestNativeHostSignInRecoversUncertainRenewal|TestNativeBroker|TestNativeRenewalKeepsAnAccessItCannotRenewUntilTheDeadline'"
updated: 2026-10-17
---

# Renew credentials in one canonical host authority

Renew expiring grants before and throughout brokered runs. Serialize renewal and keep the
provider's 200 answer durably before parsing or publishing it, so an interrupted renewal completes
from that answer without contacting the provider again. Never copy refresh
authority into repository homes or workloads, and never replace continuous renewal with a turn-sized
expiry check or forced restart. Private broker snapshots expire closed if host publication stops.

**Why:** Responder surfaced `credential is not portable through the turn deadline` even though the
managed Codex profile retained valid refresh authority. Readiness and execution had implemented
different halves of the credential lifecycle.

**How to apply:** Keep credential parsing and renewal adapter-owned through `NativeCredentialSpec`.
All coding/remote paths use the canonical host authority and public native selectors. Revocation
invalidates the prior epoch; neither a stale profile nor an env token may revive it. A kept answer
is completed, never replaced by a retry with the grant it spent. An exchange that kept nothing may be
retried: its grant is either unspent or lost either way. Only a refused grant (a 400/401
invalid_grant family answer) or a kept answer that cannot be used asks for a host sign-in.

During a run, a renewal that fails only for passing reasons (`ErrRenewalUnavailable`: no answer, a
5xx or 429, a cut-off answer that kept nothing; or the account lock staying busy) keeps serving the
held access while it is valid and retries on a per-run backoff, 5 s doubling to 1 min. Renewal
starts with under 10 minutes left (the lead), but the renewed access only has to outlast the
2-minute deadline every adapter's `Renew` checks: never pass the lead as that deadline, and never let
an adapter skip a renewal because the access still covers the deadline, since the caller decides
when renewal is due. A launch renews only short of the deadline, because its failure is fatal. A
later access renews at half the lifetime it had when the run first saw it (2 to 10 minutes ahead),
and one that cannot be renewed stays in use until it stops covering the deadline. Runs on one
account share one retry wait, kept under the account lock: a renewal that could wait never asks the
provider while another run's failed exchange is that recent, so no box loses a valid access because
the others held the lock. Every error a failure joins must be one of these: a passing error joined to a custody failure is not
retried. The ticks in between only read the account, so they never ask the provider. A lock that
stays busy revokes about 2 minutes after the last locked read, since only a read sees a sign-in or
removal. A refused grant, an unusable kept answer, an epoch change, an expired access and any
unclassified error revoke at once. Never retry a refused grant, and never serve an expired access
just because it is still `Ready`.

Pre-cutover quota inspection may still renew the original legacy store under the same writer
fences as cutover. It must never fork that authority, overlap canonical service, or fall back from
a revoked canonical account. This narrow maintenance path is not a coding-home projection.

## Changelog
- 2026-10-17 — a running box starts renewing 10 minutes ahead (native_broker.go `nativeRenewalLead`),
  separate from the 2-minute deadline the adapters check (`nativeRenewalDeadline`;
  native_account.go `renewNativeAccount(renewBy, deadline)`). Launch, remote preparation and quota
  reads pass the deadline as both. Swept the four adapters' `Renew`: only Gemini skipped a renewal
  whose access still covered the deadline, which would have made every renewal ahead a no-op that
  republished the same access; it now renews when asked (`TestGeminiAuthorityRenewalRenewsAGrantThatStillCoversTheDeadline`
  in ./internal/agent, outside this check's package). Check names the new access-only test. Review
  then found that renewing ahead let boxes on one account starve each other of the lock during a
  hung exchange (6 boxes, 2 revoked with valid access): added the shared retry wait (`retry.json`,
  `errRenewalDeferred`) and replaced a short-access fallback with the half-lifetime lead; covered by
  the `TestNativeBroker` family, including six boxes on one hung provider. Re-review: a wait ending
  over a minute ahead (the host clock stepped back) is ignored, and whether a renewal may wait is
  judged once the lock is held; each guard has a `TestNativeBroker` case that fails without it.
- 2026-10-16 — the native broker retries a renewal that failed for a passing reason instead of
  revoking the box (native_broker.go `serve`, `renewalRetryable`). Classification is an allowlist
  that every error a failure joins must pass: `ErrRenewalUnavailable` (no answer, 5xx, 429),
  `errAccountAuthorityBusy` and the attempt deadline. A refused grant, an unusable kept answer or a
  custody failure joined to one of them still revokes. A busy lock re-serves the held access for at
  most 2 minutes after the last locked read. `publish` now refuses an access past its expiry even
  when `Ready`. Swept the broker loop, prepare, the four adapters' renewal errors,
  `renewAccountAuthority`'s joined errors and the remote-session path (launch-time only,
  unchanged); check now includes the `TestNativeBroker` family.
- 2026-10-16 — interrupted renewals settle instead of wedging (account_renewal.go settleRenewal): only
  200 answers are kept, with their arrival time; a kept answer completes through the adapter's
  `Complete` at the next locked read; nothing kept clears for a retry; the exchange outlives its
  caller's cancellation. Swept all four adapters and the native broker; check names the new tests.
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
