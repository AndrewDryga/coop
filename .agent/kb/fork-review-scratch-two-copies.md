---
name: fork-review-scratch-two-copies
description: forkctl and sessionsvc each keep their own review-scratch clone on purpose — the same 18-line scaffold under opposite anchoring contracts; do not unify them
subsystem: fork
sources: [internal/forkctl/review.go, internal/sessionsvc/review.go, internal/forkspace/git.go, internal/forkspace/create.go]
updated: 2026-10-17
---

Two packages build a disposable rebased clone for a review gate, and they read like copy-paste:
`forkctl.forkReviewCandidate` with `prepareForkReviewCandidate` (internal/forkctl/review.go) and
`sessionsvc.reviewScratch` with `Service.prepareForkReviewCandidateFromIntent`
(internal/sessionsvc/review.go). The duplication is DELIBERATE — flagged during the
sessionsvc extraction, ruled again in the fork Stage 1 (ruling 8), then assessed on its own. Do not
"fix" it.

**What is genuinely identical** — the four-field value (`dir/base/name/conflict`), scratch-directory
ownership, cleanup and leak assertions. Session review now uses the shared pinned clone/fetch
primitives with request cancellation; ordinary fork previews retain their own creation path.

**What differs is the entire contract** — each one's anchor would be a bug in the other:

| | forkctl `prepareForkReviewCandidate` — a preview | sessionsvc `prepareForkReviewCandidateFromIntent` — an attested rebuild |
|---|---|---|
| base | the parent's CURRENT HEAD, read inside the scratch | the CAPTURED `intent.ParentHead`, cloned/fetched by SHA through the trusted source view |
| source | the fork branch by NAME, whatever it points at now | `intent.SourceHead` by SHA |
| verification | base is non-empty | parent head+tree, source head+tree, and `CreationBase` ancestry must all match the intent or it refuses |
| replay | `rebase base name` | `rebase --onto base creationBase name` — exactly the commits captured at session creation |

The last row is load-bearing, and the code says why (the comment above that `rebase --onto`):
inferring the upstream from the current parent would replay rewritten parent history after a
force-push. One shared `prepare*` would have to pick one of these two contracts and silently impose
it on the other caller.

**Why not one shared home.** A `forkspace.ReviewScratch` IS feasible, and nobody should have to
re-derive that: forkspace is the leaf both consumers already import, and it holds every primitive
the scaffold needs (`GitClone`, `PropagateGitIdentity`, `GitHardening`; forkctl's `gitRun` is its
own). It is simply not worth it. Both `prepare*` must stay where they are regardless — each calls
package-local helpers (`ReviewGitIdentity`/`sessionReviewIsAncestor` in sessionsvc, `gitOut` in
forkctl) — so unifying would export a four-field type plus three methods out of a leaf and rewrite
~40 field references to delete ~18 duplicated lines. The line count goes UP, and what remains is a
shared base type quietly inviting the next reader to merge the two prepare policies: a correctness
regression wearing a cleanup's clothes.

**The usual drift argument does not apply.** `git log -S` finds four commits on this machinery:
`e8eabb9` created the fork copy, `459442f` created the session copy ALREADY different (born
diverged, not copy-pasted and then rotted), and `9eff655` + `bb6c3e4` only moved each into its new
package. There is no "fixed in one, missed in the other" history — the usual reason to consolidate.

The one difference that WAS accidental rather than semantic is now aligned: both zero their named
result on a failed prepare (the deferred cleanup in each `prepare*`), so neither hands back a struct
whose `dir` names the directory its own defer just removed. It was inert — the only caller,
`Service.executeReviewIntent`, discards the value on error — but a future caller
inspecting the failed value would have gotten a live-looking path to a deleted directory.
`TestSessionReviewScratchIsZeroedOnFailedPrepare` pins the session side. Every remaining difference
below the table is the contract itself; there is nothing left to "tidy".

**Reconsider only if the contracts converge** — a session review that gates on the parent's live
HEAD, or a fork preview that verifies a captured intent. Then one type would carry one meaning and
this note is obsolete. See [[fork-lifecycle-state-file]] for the fork state this machinery reviews.

## Changelog
- 2026-10-17 — named the two `prepare*` functions, their deferred cleanup and
  `Service.executeReviewIntent` instead of review.go line numbers (the table cells now sit under
  their function); re-verified every table row against both functions in internal/forkctl/review.go
  and internal/sessionsvc/review.go. forkspace has had no `gitRun` since b30a22f6, so the
  shared-home paragraph no longer lists it among forkspace's primitives.
- 2026-09-27 — session scratch now uses exact, credential-free pinned clone/fetch with request
  cancellation. The separate anchoring contracts still apply; no shared review orchestration added.
- 2026-08-10 — the last accidental difference is gone: sessionsvc now zeroes its named result on a
  failed prepare like forkctl, pinned by `TestSessionReviewScratchIsZeroedOnFailedPrepare`. Every
  `internal/sessionsvc/review.go` line reference re-verified (the fix shifted them by one).
- 2026-08-10 — created: the assess-then-act ruling queued by fork Stage 1 (ruling 8) and the
  sessionsvc extraction's RISK-4 note. Verified against both files and `git log -S`; decision is
  KEEP TWO, with a pointer comment now at each site.
