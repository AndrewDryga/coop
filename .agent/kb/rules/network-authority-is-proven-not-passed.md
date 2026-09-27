---
name: network-authority-is-proven-not-passed
description: a filtered child proves its network authority against the owner-private store; controller grants are admitted once, never passed to the child
scope: security
sources: [internal/box/run.go, internal/box/network_session.go, internal/networkstate/authority.go, internal/networkstate/job.go, internal/networkstate/approval_review.go, internal/sessionsvc/network.go, internal/cli/net_approve.go]
check: "go test ./internal/box -run 'TestFilteredPublicLaunchRequiresCaptureAndRejectsExtraArgs|TestProjectFilteredLaunchRequiresCaptureBeforeRuntime|TestCapturedEgressFromEnvironmentAuthenticatesAgainstTheOwnerStore|TestControllerJobChildReprovesItsOwnSnapshot'"
updated: 2026-09-26
---

# Network authority is proven against the owner store, never accepted from its carrier

An authenticated controller may submit a job's network rules to the worker daemon, which validates
and captures them once in the owner-private store. From the daemon to an ACP child or box, what
crosses is only a REFERENCE — the owner-keyed snapshot fingerprint and its project or job identity.
The child reopens `~/.local/state/coop/network` and loads that exact snapshot; if it cannot, the
launch is refused, never downgraded to open. Repository content, task text and the box never
publish network authority.

**Why:** the pre-salvage networking WIP moved authority around instead: a launch catalog, one-use
handoff frames and FD bootstrap carried grants between processes, and the whole apparatus went in
the salvage. The read-only review of the replacement said the same thing from the other side — an
environment handoff is not enough on its own; bind it to the immutable session row and verify the
run on registration. Both corrections point one way: the carrier names a policy, the receiver
proves it.

**How to apply:**
- Fail CLOSED at the boundary. `box.Run` refuses a filtered posture that arrived without a host
  capture (`internal/box/run.go:305`); it never launches open instead.
- A child re-authenticates: `OpenExisting` + `LoadSnapshot` for a legacy project capture, or
  `LoadJobSnapshot` for the exact job digest and session ID. `OpenExisting`, never `Open` — a child
  must not create an owner key as a side effect of starting.
- Keep the carrier unforgeable by keeping it host-only. `COOP_NETWORK_CAPTURE` is set by the daemon
  parent (`internal/sessionsvc/network.go:148`) and read in exactly ONE function; the daemon scrubs
  every `COOP_*` from the environment it builds, so no box can set it.
- Keep the capture off every wire shape. `RunSpec.CapturedEgress` is `json:"-"`; a DTO, a worker
  command or a session request that could carry one is the bug.
- Local approvals have ONE host writer. `Store.Approve` is reached through `coop approve`
  (`internal/cli/net_approve.go`), which requires a terminal. `coop net approve` only points to
  that command; it is not a second local approval path. See [[project-edits-request-access]].
  `CaptureJob` is a separate authenticated-controller admission path: it never consumes a local
  approval or repository request, and cannot be called by a child.
- Verify after the fact too: the daemon checks every run's recorded fingerprint against the
  immutable session row and fails the turn on a mismatch.

Background: [[restricted-networking]] (where authority lives), [[network-consumers]] (who carries a
reference).

## Changelog
- 2026-09-26 — checked the four production snapshot loaders and added a separate job-scoped
  capture/re-proof path; local project approvals remain only on legacy/direct launches. The
  controller's grant-bearing admission is distinct from the daemon-to-child reference boundary.
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
