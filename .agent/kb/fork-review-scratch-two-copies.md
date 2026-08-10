---
name: fork-review-scratch-two-copies
description: forkctl and sessionsvc each keep their own review-scratch clone on purpose — the same 18-line scaffold under opposite anchoring contracts; do not unify them
subsystem: fork
sources: [internal/forkctl/review.go, internal/sessionsvc/review.go, internal/forkspace/git.go, internal/forkspace/create.go]
updated: 2026-08-10
---

Two packages build a disposable rebased clone for a review gate, and they read like copy-paste:
`forkctl.forkReviewCandidate` (`internal/forkctl/review.go:30-101`) and `sessionsvc.reviewScratch`
(`internal/sessionsvc/review.go:487-574`). The duplication is DELIBERATE — flagged during the
sessionsvc extraction, ruled again in the fork Stage 1 (ruling 8), then assessed on its own. Do not
"fix" it.

**What is genuinely identical** — roughly 18 lines of scaffolding: the four-field value
(`dir/base/name/conflict`), `cleanup()` (one `os.RemoveAll`), `detachBase()` (one
`git checkout --quiet --detach`), and `os.MkdirTemp("", "coop-fork-review-")` + `forkspace.GitClone`
under the same wrapped error. Both packages' `gitRun` are identical too (`forkspace.GitHardening`
through a local `gitArgs`), as is the leak assertion each test suite carries
(`internal/forkctl/review_gate_test.go:358`, `internal/sessionsvc/helpers_test.go:56`).

**What differs is the entire contract** — each one's anchor would be a bug in the other:

| | forkctl — a preview | sessionsvc — an attested rebuild |
|---|---|---|
| base | the parent's CURRENT HEAD, read inside the scratch (`review.go:80-82`) | the CAPTURED `intent.ParentHead`, fetched by SHA into `refs/coop/session-parent` (`review.go:528-531`) |
| source | the fork branch by NAME, whatever it points at now (`:90`) | `intent.SourceHead` by SHA (`:544`) |
| verification | base is non-empty (`:80-82`) | parent head+tree, source head+tree, and `CreationBase` ancestry must all match the intent or it refuses (`:536-538,551-553,558-564`) |
| replay | `rebase base name` (`:93`) | `rebase --onto base creationBase name` (`:567`) — exactly the commits captured at session creation |

The last row is load-bearing, and the code says why (`review.go:565-566`): inferring the upstream
from the current parent would replay rewritten parent history after a force-push. One shared
`prepare*` would have to pick one of these two contracts and silently impose it on the other caller.

**Why not one shared home.** A `forkspace.ReviewScratch` IS feasible, and nobody should have to
re-derive that: forkspace is the leaf both consumers already import, and it holds every primitive
the scaffold needs (`GitClone`, `PropagateGitIdentity`, `gitRun`, `GitHardening`). It is simply not
worth it. Both `prepare*` must stay where they are regardless — each calls package-local helpers
(`ReviewGitIdentity`/`sessionReviewIsAncestor` in sessionsvc, `gitOut` in forkctl) — so unifying
would export a four-field type plus three methods out of a leaf and rewrite ~40 field references to
delete ~18 duplicated lines. The line count goes UP, and what remains is a shared base type quietly
inviting the next reader to merge the two prepare policies: a correctness regression wearing a
cleanup's clothes.

**The usual drift argument does not apply.** `git log -S` finds four commits on this machinery:
`e8eabb9` created the fork copy, `459442f` created the session copy ALREADY different (born
diverged, not copy-pasted and then rotted), and `9eff655` + `bb6c3e4` only moved each into its new
package. There is no "fixed in one, missed in the other" history — the usual reason to consolidate.

One difference IS accidental rather than semantic: forkctl zeroes its named result on a failed
prepare (`review.go:69-75`), while sessionsvc returns the struct with `dir` still naming the
directory its own defer just removed (`review.go:518-523`). It is inert today — the only caller
discards the value on error (`review.go:368-371`) — and it is the one place the two could be
aligned without touching either contract.

**Reconsider only if the contracts converge** — a session review that gates on the parent's live
HEAD, or a fork preview that verifies a captured intent. Then one type would carry one meaning and
this note is obsolete. See [[fork-lifecycle-state-file]] for the fork state this machinery reviews.

## Changelog
- 2026-08-10 — created: the assess-then-act ruling queued by fork Stage 1 (ruling 8) and the
  sessionsvc extraction's RISK-4 note. Verified against both files and `git log -S`; decision is
  KEEP TWO, with a pointer comment now at each site.
