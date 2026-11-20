---
name: existing-pr-publication
description: exact selected-parent review and human PR publication without metadata edits or history replacement
subsystem: sessions
sources: [internal/sessionsvc/review.go, internal/sessionsvc/source.go, internal/sessionsvc/publication.go, internal/workerproto/publication.go, internal/workerconnector/publication.go, internal/workerconnector/publication_existing.go, internal/workerconnector/publication_progress.go, internal/workerconnector/capabilities.go, internal/sessionsvc/http.go, docs/session-api.md]
updated: 2026-11-19
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
receipt. Files and directory entries are synced. The service serializes each publication operation;
the writer additionally refuses to replace a confirmed result with a preparation marker. Results
replay before asking for credentials. An unresolved previous attempt followed by revocation or a
conflict yields `publication_push_outcome_unverified`: manual reconciliation, never a claim that
nothing was pushed. The marker survives command acknowledgement, session discard and ordinary
source cleanup. It contains no credentials and must not move into disposable command receipts.

## Changelog
- 2026-11-19 — Bounded PR re-read after the push: a single read hit GitHub's lag on 4 of 4 real runs (manual test D1).
- 2026-11-17 — Added after service and connector tests verified selected-parent review, atomic ancestry-preserving publication, race refusals and receipt recovery.
