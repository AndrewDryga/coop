---
name: existing-pr-publication
description: exact selected-parent review and human PR publication without metadata edits or history replacement
subsystem: sessions
sources: [internal/cli/session_cmd.go, internal/sessionsvc/review.go, internal/sessionsvc/source.go, internal/sessionsvc/publication.go, internal/workerproto/publication.go, internal/workerconnector/publication.go, internal/workerconnector/publication_existing.go, internal/workerconnector/publication_progress.go, internal/workerconnector/capabilities.go, internal/sessionsvc/http.go, docs/session-api.md]
updated: 2026-11-20
---

`existing-pr-publication:1` is advertised only when the local daemon proves its version. The
controller sends `parent_head` to review; it must equal the job and session's selected commit.
Both review preparation and the post-gate check keep that parent, even when default advances.
Existing PR publication requires a retained publishable candidate with a passed gate on that
exact parent. The service and connector independently prove the old head is an ancestor.

`mode: existing_pull_request` names an exact PR, repository, branch, base and expected head.
The controller owns human authorization; the worker does not infer it from the author's login.
The connector renews the grant after LFS preparation and rechecks PR identity before Git. An
explicit `--force-with-lease=<ref>:<expected>` supplies atomic comparison; the separately proved
ancestry means its only successful effect is fast-forward. Plain push would accept an advanced
or recreated branch in some races. Never replace this with an implicit lease or `--force`.
This mode never creates a PR or edits title, body or draft state.

Git and PR metadata cannot be changed atomically. After a confirmed branch write, changed or
unavailable PR metadata produces `published_target_changed` or `published_target_unverified`
with the original receipt and a bounded observation. These are confirmed writes with warnings,
not conflicts or permission to perform follow-up edits. GitHub's PR view lags a push by a few
seconds, so after a confirmed write (fresh or recovered) the connector re-reads the PR with fixed
waits (`existingPullConfirmationWaits`, 15 s total) while it still looks unverified, under the
same grant. A changed target settles at once; `published` needs a read showing the pushed head.
The saved result stays final: retrying the service operation would not help, since it replays it.
So a stop (context end) during the waits returns a `pull_request_confirmation` retry and saves
nothing; the attempted marker and the `observed == CandidateHead` path confirm it on restart.

`publication-results/<full-intent-digest>.json` bridges the Git write and completed service
operation. It persists the complete intent and an attempted marker before Git, then the validated
receipt. New-PR mode writes the same marker, same shape, right before its branch push, and saves
only a `published` receipt (its PR number is the one it opened, so `exactSavedPublicationResult`
takes any positive number when the request names none). v15.0.0 never wrote new-PR files, so their
absence reads as "not attempted". `PrunePublicationProgress` removes a file only when it is older
than `publicationProgressRetention` (30 days, by mtime) AND the service's PublishReview row for
`intent.CommandKey` is `succeeded` or `failed` (sessionsvc `prunePublicationJournal`, at Start and
after each background publish settles; cli wires it in `serveLocalSession`). Safe because operation
rows are never deleted and a settled row replays to every redelivery without calling the publisher;
the age margin covers a state folder restored from an older copy. Unknown key, running row, other
method: kept. A file that fails to verify (content does not hash to its name) is kept forever: it
may be the only push evidence. Age is read from Lstat before any file is opened, so a pass reads
only old files. WriteFileAtomic temporaries (`.<digest>.json-*`) older than the same 30 days are
removed (no live write is that old). Never prune on session discard.
Files and directory entries are synced. The service serializes each
publication operation; the writer additionally refuses to replace a confirmed result with a
preparation marker. Results replay before asking for credentials. An unresolved previous attempt
followed by revocation or a conflict yields `publication_push_outcome_unverified`: manual
reconciliation, never a claim that nothing was pushed. The exception is a new-PR refusal found
after that try saw the branch at the candidate (`confirmed` in `publishCandidate`: a post-push
lookup or the final PR check): the push is known, so it keeps its code, and a wrong `actor_id`
still reads as `publication_pull_request_mismatch`. The marker survives command acknowledgement,
session discard and ordinary source cleanup. It contains no credentials and must not move into
disposable command receipts.

New-PR mode looks PRs up by branch with `state=all`, so recovery after a lost response finds the
App's own PR even if closed. Exactly one exact open App-owned draft is reused; anything else
refuses. When every listed PR is closed and none is the App's bot's, the refusal is
`publication_branch_has_closed_pull_request` with `closed_pull_request` (newest number, URL built
from the repository, never GitHub's string; `closedForeignPull`). It comes from the lookup before
the push, or from the one after the push and before the PR create. The lookup reads one page of
`pullLookupPage` (100) and never follows pagination, so a full page answers the general
`publication_existing_pull_request_changed`: an open PR could be on the next page. This stays a
refusal on purpose: new-PR mode has no ancestry proof, so a lease on that branch could replace a
person's history, and a second PR on it would sit beside theirs. A closed App-owned PR keeps
`publication_existing_pull_request_changed`: publishing again would undo the person who closed it.

## Changelog
- 2026-11-20 — the journal is pruned: files older than 30 days whose PublishReview operation settled go, at start and after each settle (task 2026-11-20-bound-the-publication-journal-folder-which-grows).
- 2026-11-19 — new-PR mode journals the attempted marker before its push, so a refusal after a lost push reports `publication_push_outcome_unverified`; the service accepts that code in both modes (task 2026-11-19-record-that-a-new-pr-publish-attempted-its-push).
- 2026-11-19 — a closed PR the App does not own now refuses a new-PR publish by name (`publication_branch_has_closed_pull_request`) instead of as "changed"; loosening was rejected (task 2026-11-19-let-a-new-pr-publish-use-a-branch-again-after-a).
- 2026-11-19 — Bounded PR re-read after the push: a single read hit GitHub's lag on 4 of 4 real runs (manual test D1).
- 2026-11-17 — Added after service and connector tests verified selected-parent review, atomic ancestry-preserving publication, race refusals and receipt recovery.
