---
name: repository-refresh-bounds-stalls-not-catch-up
description: "bound a stalled repository refresh without treating a normal catch-up fetch like one remote lookup"
scope: architecture
sources: [internal/workerconnector/job_sources.go, internal/workerconnector/job_sources_refresh_test.go]
check: "go test ./internal/workerconnector -run 'TestRepositoryFetchMayOutliveRemoteIdentityLookup|TestRepositoryRefreshUsesAnExistingVerifiedCommitWithoutFetching|TestRepositoryFetchRemainsCancellableAtItsOwnDeadline'"
updated: 2026-09-26
---

# Give repository identity lookup and object transfer separate bounds

Keep the remote-ref lookup tightly bounded, but give an exact-commit fetch its own transfer
stall bound. A progressing repository catch-up is valid workspace preparation, not a failed lookup.
The fetch must remain cancellable and eventually time out when it is genuinely stalled.

**Why:** A production alert remained silently pending through repeated session-create attempts
because a checkout 265 commits behind needed longer than the one 30-second deadline shared by
`ls-remote` and `fetch`. The operator's correction was: "make repository refresh tolerate a normal
catch-up fetch ... it must not leave the Slack card silently pending across 20 retries."

**How to apply:** Use a short budget for remote identity lookup and a distinct idle-transfer bound
for object transfer, without imposing the lookup's total-time cap. Exercise both the valid slow path and the cancelled stalled path with hermetic Git
tests; follow [[hermetic-git-tests]]. The caller must surface the bounded preparation failure before
retrying it silently.

## Changelog
- 2026-09-26 — moved source access from the retired service policy adapter to job-authorized
  host Git transport. Reverified separate lookup context, cached-object reuse, cancellation and
  HTTP low-speed limits; refreshed objects never rewrite the frozen selected source.
- 2026-08-20 — re-verified both source files after live watch sessions repeatedly fetched a remote
  head object already present locally. Added the existing-object regression and kept the slow
  transfer and stalled-transfer paths covered.
- 2026-08-17 — created after sweeping both session-source files. The shared deadline was the one
  violation and is fixed here. The new positive test failed with `context deadline exceeded` before
  the split; the stalled-transfer test keeps the longer path bounded.
- 2026-09-10 — the may-outlive-lookup regression now observes both budgets on the injected runner seam and completes a real fetch only after the lookup context has ended; a shared deadline and two near-misses fail instantly. No production timeout changed.
