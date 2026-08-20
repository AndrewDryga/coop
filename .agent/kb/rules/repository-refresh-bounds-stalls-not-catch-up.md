---
name: repository-refresh-bounds-stalls-not-catch-up
description: "bound a stalled repository refresh without treating a normal catch-up fetch like one remote lookup"
scope: architecture
sources: [internal/sessionsvc/source.go, internal/sessionsvc/source_test.go]
check: "go test ./internal/sessionsvc -run 'TestRepositoryFetchMayOutliveRemoteIdentityLookup|TestRepositoryRefreshUsesAnExistingVerifiedCommitWithoutFetching|TestRepositoryFetchRemainsCancellableAtItsOwnDeadline'"
updated: 2026-08-20
---

# Give repository identity lookup and object transfer separate bounds

Keep the remote-ref lookup tightly bounded, but give an exact-commit fetch its own transfer
deadline. A progressing repository catch-up is valid workspace preparation, not a failed lookup.
The fetch must remain cancellable and eventually time out when it is genuinely stalled.

**Why:** A production alert remained silently pending through repeated session-create attempts
because a checkout 265 commits behind needed longer than the one 30-second deadline shared by
`ls-remote` and `fetch`. The operator's correction was: "make repository refresh tolerate a normal
catch-up fetch ... it must not leave the Slack card silently pending across 20 retries."

**How to apply:** Use a short budget for remote identity lookup and a distinct, longer budget for
object transfer. Exercise both the valid slow path and the cancelled stalled path with hermetic Git
tests; follow [[hermetic-git-tests]]. The caller must surface the bounded preparation failure before
retrying it silently.

## Changelog
- 2026-08-20 — re-verified both source files after live watch sessions repeatedly fetched a remote
  head object already present locally. Added the existing-object regression and kept the slow
  transfer and stalled-transfer paths covered.
- 2026-08-17 — created after sweeping both session-source files. The shared deadline was the one
  violation and is fixed here. The new positive test failed with `context deadline exceeded` before
  the split; the stalled-transfer test keeps the longer path bounded.
