---
name: network-authority-is-proven-not-passed
description: a filtered launch proves its network authority against the owner-private store; a boundary crossing carries a name, never a grant
scope: security
sources: [internal/box/run.go, internal/box/network_session.go, internal/networkstate/authority.go, internal/networkstate/approval_review.go, internal/sessionsvc/network.go, internal/cli/net_approve.go]
check: "go test ./internal/box -run 'TestFilteredPublicLaunchRequiresCaptureAndRejectsExtraArgs|TestProjectFilteredLaunchRequiresCaptureBeforeRuntime|TestCapturedEgressFromEnvironmentAuthenticatesAgainstTheOwnerStore'"
updated: 2026-09-14
---

# Network authority is proven against the owner store, never accepted from its carrier

When restricted networking crosses a process, an API or a box boundary, what crosses is a
REFERENCE — the canonical project plus the owner-keyed snapshot fingerprint. The receiving side
reopens `~/.local/state/coop/network` and loads that exact snapshot; if it cannot, the launch is
refused, never downgraded to open. Nothing that a repository, a request payload or a box can write
is ever authority.

**Why:** the pre-salvage networking WIP moved authority around instead: a launch catalog, one-use
handoff frames and FD bootstrap carried grants between processes, and the whole apparatus went in
the salvage. The read-only review of the replacement said the same thing from the other side — an
environment handoff is not enough on its own; bind it to the immutable session row and verify the
run on registration. Both corrections point one way: the carrier names a policy, the receiver
proves it.

**How to apply:**
- Fail CLOSED at the boundary. `box.Run` refuses a filtered posture that arrived without a host
  capture (`internal/box/run.go:305`); it never launches open instead.
- A child re-authenticates: `OpenExisting` + `LoadSnapshot` of exactly the referenced project and
  fingerprint (`internal/box/network_session.go:195`). `OpenExisting`, never `Open` — a child must
  not create an owner key as a side effect of starting.
- Keep the carrier unforgeable by keeping it host-only. `COOP_NETWORK_CAPTURE` is set by the daemon
  parent (`internal/sessionsvc/network.go:148`) and read in exactly ONE function; the daemon scrubs
  every `COOP_*` from the environment it builds, so no box can set it.
- Keep the capture off every wire shape. `RunSpec.CapturedEgress` is `json:"-"`; a DTO, a worker
  command or a session request that could carry one is the bug.
- Approvals have ONE host writer. `Store.Approve` is reached through `coop approve`
  (`internal/cli/net_approve.go`), which requires a terminal. `coop net approve` only points to
  that command; it is not a second authority path. See [[project-edits-request-access]]. A launch,
  an API call or an unattended loop never widens access — see [[destructive-confirm-gate]].
- Verify after the fact too: the daemon checks every run's recorded fingerprint against the
  immutable session row and fails the turn on a mismatch.

Background: [[restricted-networking]] (where authority lives), [[network-consumers]] (who carries a
reference).

## Changelog
- 2026-09-14 — moved the box's capture check after project-policy resolution and added a regression
  proving project-requested filtered mode cannot reach the runtime through an unadmitted caller.
- 2026-09-13 — moved the sole CLI writer to `coop approve`; the retired network subcommand now
  returns migration guidance before it can reach approval state.
- 2026-09-12 — reread the CLI review/commit path; net approve is still the writer. Recorded its
  approved replacement without claiming implementation. Unified permission/service/data work is
  queued as 2026-09-12-unify-project-access-approval-under-coop-approve; existing capture checks
  remain the scope of this card's test, not proof of the unimplemented wider contract.
- 2026-09-10 — created after the restricted-networking salvage. Swept the tree: `*CapturedEgress`
  appears on exactly one struct field (`RunSpec`, `json:"-"`) and otherwise only in function
  signatures; `COOP_NETWORK_CAPTURE` has one reader and one host-parent writer; `Store.Approve` has
  one production caller. 0 violations. The `check:` gates the two central ones — the fail-closed
  launch boundary and the child's re-authentication; the serialization and single-writer corollaries
  are still review-only.
