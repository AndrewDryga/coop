---
name: fork-candidate-review-rounds
description: immutable reviewed fork snapshots, the replayable current manifest, fresh candidate-wide signoff, and zero-ahead merge bookkeeping
subsystem: fork
sources: [internal/forkctl/candidate_drift.go, internal/tasks/candidate.go, internal/tasks/candidate_rounds.go, internal/tasks/candidate_rework.go, internal/tasks/projection.go, internal/cli/fork_cmd.go, internal/forkctl/merge.go, internal/forkctl/land.go, internal/loop/prompts.go, internal/loop/rebase.go, internal/tasks/candidate_rebase.go, internal/tasks/candidate_merges.go, internal/forkctl/candidate_merges.go, internal/forkctl/land_check.go, internal/forkctl/status.go, internal/forkctl/isolated_candidate.go, internal/forkctl/isolated_commands.go, internal/cli/fork_loop_report.go, internal/forkspace/gitcommand.go]
updated: 2026-11-20
---

A task fork's land authority is one exact candidate ID, HEAD, tree, assignment set and projection
digest set. The mutable generation manifest at the historical candidate path points at immutable
round records under `.coop/candidate-history/<name>.<generation>/`; normal reads follow the current
reference and its direct predecessor rather than scanning history. A bare version 1 candidate file,
which Coop v12.1.0 adopted as round one at the next change, is refused by name and left as it is, as
is a land whose candidate pointer is gone (v10.1.2 and earlier removed it before the land journal).
Unknown, malformed, linked or inconsistent state refuses instead of falling back.

The live phases are `active`, `superseding`, `reviewing`, `reworking` and `publishing`. Supersession makes the
old round non-landable before returning its exact owners to review. The loop then freezes a pending
candidate and runs a separate read-only candidate-wide signoff; only a successful receipt for every
exact subject lets the host move it to publishing. Publication writes the immutable round, makes
each matching owner ready, and switches the manifest to active last. Every boundary is replayable,
but a changed HEAD, tree, assignment, projection, owner or candidate ID authorizes nothing. A new
HEAD must descend from the last reviewed round.

A failed candidate verdict journals its exact rejected snapshot, findings and audit-reopen
authority before touching projections. Only flagged assignments can resume while `reworking`;
new canonical tasks cannot join that frozen cohort. Reopening and canonical acceptance replay
under the lifecycle lock, recovering a durable blocking intent before changing its digest.
Records-only repair uses the existing audit authority without another commit, then requires a
fresh candidate ID and complete signoff. Unauthorizable rework or a repeated identical finding
after unchanged-HEAD repair blocks with the finding. Failure cleanup preserves blocked projections;
only an explicitly unblocked canonical owner can resume them.

Audit rewrites can invalidate another projection's audit authority; the loop parks that task
with recovery guidance rather than granting replacement authority. Rewrites before an already
published round still fail the ancestry fence: preserve the repair and restore the recorded
pre-rework history named by the refusal. The immutable round is never silently replaced.

`landed` and `discarded` are passive terminal phases that retain history without keeping workspace
cleanup active. An unreviewed pending attempt is not promoted into history on discard; an already
authorized publishing attempt is. Any land or discard intent fences new review work. In batch
merge, the zero-ahead decision happens under the same lifecycle lock after land-journal replay: a
fork is skipped only when it has neither commits nor active candidate/task bookkeeping.

A land journal in `preparing` restores the candidate before it rebases, which is right after a crash
mid-rebase but would also throw away anything a person committed in the fork. So a rebase git refuses
(a conflict, or signing) and Coop aborts back onto the exact candidate commit, tree and branch,
clean, retires the journal as a red gate does (`candidateWorkspaceIntact`, internal/forkctl/land.go);
any other failure keeps it for replay. Unmerged paths left by the refused rebase tell a conflict from
any other refusal (`rebaseFailedError.conflicts`); only a signing rebase's refusal says signing, and
a conflict's git hints never reach the terminal (`gitSignQuiet`). A conflict also records a rebase
request (`<name>.<generation>.candidate-rebase.json` beside the manifest, version 1: the active
candidate ID, the exact parent commit `onto` and the branch the merge used; host-written only,
internal/tasks/candidate_rebase.go), and the refusal names `coop fork <name> --loop`; a later
conflicting merge replaces it. The loop, seeing the request and a rebase not finished
(`forkctl.CheckRebase`: own branch, clean, no rebase state, builds on `onto` with commits of its
own), first moves `onto` to where that branch is now (`RefreshForkCandidateRebase`, under the
lifecycle lock), fetches it into the fork and runs one task-less write pass (`loop.RunSpec.Rebase`,
`forkRebasePrompt`, with the work iteration's rotation and retries, `runRebasePass`). The host then
refuses an empty rebase (HEAD is `onto`; `droppedReviewedWork` tells "the branch already has the
work", which needs a person to close the tasks, from "the rebase dropped it", which a rebase from
the reviewed commit redoes; loop, merge and ls all say it through `emptyRebaseError`/`driftEmpty`)
and any change of Coop-Task bindings over `onto..HEAD` against `onto..candidate`
(`RebaseBindingProblems`, counted from raw objects with the audits' trailer parser, since `git log`
follows a box-written shallow file), and keeps the lines of the agent's final message that start
with `REBASE CHECK —` (the decoders' `finalMessage`, never tool output or stderr; lines naming the
report markers dropped) with the request, bound to the rebased tree
(`RecordForkCandidateRebaseVerification`): the tasks' handoffs predate the resolution, so without
them the reviewer failed every rebased round as stale. A finished rebase no pass reported on (a
person rebased by hand, often after the merge-commit refusal, or the pass reported nothing) gets one
check-only pass per tree per run (`RebaseSpec.CheckOnly`, `forkRebaseCheckPrompt`,
`checkForkRebaseTree`): its agent runs the merge gate and `git diff --check` and changes nothing; the
host refuses it if HEAD, the tree or a clean finished rebase moved (only dirty tracked files: the
refusal blames a gate that writes them, since every rerun ends the same), and keeps its lines bound
to that tree as above. Coop itself runs no gate on the host, so the check is an agent claim like the rebase
pass's, never "Coop ran it". The review prompt quotes each line inside untrusted markers and says the
agent claims them and Coop did not verify them. Then the usual supersession and review. The ancestry fence (`buildsOnReviewedRound`) accepts a HEAD that descends
from the reviewed round or, while the request names the current round, from `onto` but is not `onto`
itself; nothing else changes what may land, since the rebased HEAD is a new pending round that needs
a full candidate review, whose prompt says to review the whole diff from `onto`
(`CandidateReviewSpec.RebasedOnto`) and carries the pass's checks only for the tree they were run
on. The request is open only while the round it names is the manifest's current round in a live
phase, so it survives the supersede/review/rework phases and a crash anywhere resumes (a finished
rebase skips the pass but not the host's checks), while publishing the next round, a land or a
discard closes it without touching the file. It sits outside the manifest so the manifest's format,
and the builds that read it, stay as they were. It keys on `onto`, a parent commit, so re-signing
the fork's commits does not disturb it. A failed round keeps its number for its rework's review,
which the review heading names (`ForkCandidateReview.AfterRework`). Isolated forks have no such
path: the source holds every parent branch only as it was at creation (a `--no-local` clone of a
view copying all branches as `origin/*`, forkspace TestIsolatedForkHoldsHistoryUpToCreationAndNothingLater)
and nothing of the parent enters it later, through a conflicting merge or a land either
(forkctl TestIsolatedConflictNamesTheWaysOutAndShowsInLs). `RefuseDivergedFork` ignores the
isolated record off the reviewed commit (and refuses a skipped one), so a rewritten isolated fork is refused before a loop
starts. Their private rebase's conflict (`isolatedConflictError`, from unmerged paths;
signing and other failures keep the old guidance) names the two ways out (change the branch, or
discard and re-fork) and writes a request with the `isolated-conflict` cause only so ls shows
"conflicts" with those ways; a loop on the reviewed commit runs nothing for it and is refused with
exit 1, the same ways out and the person's restart commands (`IsolatedRefusalLoopError`, from
`RefuseDivergedFork` before any start and from the loop's pass), so `--loop && merge` stops; the
fence admits nothing by it, and a merge whose private rebase applies closes it. A changed land's
two-way refusal (`isolatedChangedError`) is recorded the same way with the `isolated-changed`
cause, which ls shows as `changed`; `tasks.IsolatedRebaseCause` is the one test every loop, fence and
drift site uses for both. ls reads either record only while the parent's HEAD is still its `onto`,
the commit that merge met, so a changed branch shows ready until the next merge decides; the loop
still refuses on the record alone (exit 1, no agent), but then says the branch changed since and
names `coop fork merge`. The publish header names
the fork's own commit and the new commit it publishes as, rebased onto the branch.
The private rebase's join commits keep it from replaying published
source but not from skipping a source commit whose patch the parent gained since the boundary, so a
reviewed round's merge runs `isolatedLandedAsReviewed` in the private clone: when the parent's tree
is still the boundary's, the rebased tree must be the reviewed source tree (line edits drop
positions); then the parent-to-rebased line edits must equal the boundary-to-source ones, and when
commits dropped, `tasks.CopiedBindingProblems` (from..before against onto..after, raw) must find no
lost binding. The two-way refusal records `isolated-changed` (above); the unmoved-tree one records
nothing. All run before the merge's
confirmation question (they only read), the gate after it (forkctl
TestIsolatedLandRefusesAChangeTheRebaseWouldAlter, TestIsolatedLandRefusesASkippedTaskCommit,
TestIsolatedLandOnAnUnmovedBranchNeedsTheReviewedTree).

No round may hold a merge commit. A review reads the candidate's diff, but `git rebase` lands its
commits one by one, flattening a merge and replaying every commit beneath it, so a change a merge
resolution dropped lands unseen. `tasks.MergeCommitsOutside` finds merges from raw objects (a
box-written shallow file makes it fail, never report none). The fork loop refuses to freeze a round
over one (`forkctl.RefuseCandidateMergeCommits`: an ordinary fork's commits the parent's HEAD lacks,
fetched into the fork; an isolated fork's above its unpublished base); the fence refuses one above
the reviewed round or above `onto` (`tasks.ErrForkMergeCommits`, never a false "no longer
descends"), and `CheckRebase` reports such a rebase as `RebaseMerges`, which ends the loop where a
request's `onto` was already in a merged candidate's history and every rebase "finished" at once.
Every refusal is `forkctl.MergeCommitsError` with the `git rebase` onto the reviewed head (or onto
`onto` while a request is open): from the loop before supersession, from the rebase pass before any
agent runs and after one that kept a merge (another agent run would only keep it again), and from
merge and ls (`driftMerges`). The rebase prompt says rebase, never merge. A round reviewed before this check is refused at merge: an ordinary fork gets a rebase
request with the `merges` cause (`refuseReviewedMergeCommits`, which fetches the parent's commit into
the fork and names the `git rebase` onto it; ls shows `driftMerges`), since no agent pass straightens
a merge; once a person ran it, the loop's check pass and a full review follow. An isolated one can
only be discarded.
The land then checks it lands exactly the reviewed change (`landedAsReviewed`, land_check.go).
"The parent did not move" is decided from raw objects (`tasks.InRawHistory`); then the rebased tree
must be the reviewed tree. When it moved, the rebased change from the parent must make the same line
edits (`lineEdits`: `git diff -U0`, no line numbers or blob names) as the reviewed change from where
the candidate's straight line of commits starts (`tasks.ForkCommitsBase`, raw); a mismatch retires
the journal and records a rebase request with a cause (`tasks.RebaseCause*`, `cause` in the request
file, omitted for a conflict). When the rebased commits lost a Coop-Task binding
(`RebaseBindingProblems` over the land's own rebased head, read only when the rebase dropped
commits, and checked before the line edits, since the parent having a change and its undoing leaves
them matching) the parent already has a task's commit:
a rebase pass could only skip it again or keep it empty, which review fails, so the cause is
`skipped` and merge, loop (before any agent, `SkippedCommitError`) and ls (`driftSkipped`) all stop
with "discard, or remove the branch's commit with that change from its history and merge again"
(titled "already in <branch>'s history", since that is true after a revert too). Its second way
out is a plain sentence row, then the go-back command alone, then "Then merge again:", so no long
label pads the commands past 100 columns (TestSkippedCommitRefusalRowsFitTheTerminal). A
revert is not enough: `git rebase` skips a commit whose patch-id matches any commit in
merge-base..branch (patch-id ignores whitespace, so a differing file can still match), a reverted one included (`--reapply-cherry-picks` would replay it, and the land's
line-edit check would still hold, but the land does not use it). The fence admits no rebase onto
such a request's `onto`. Otherwise the cause is `changed`, so the loop rebases and the result is
reviewed, and ls says `changed`, never "conflicts". A land that later gets past `landedAsReviewed`
closes any open request (`CloseForkCandidateRebaseLocked`), so a red merge gate after the person
fixed the branch leaves an ordinary reviewed round, not a stale refusal. A conflict-caused rebase
(pass or by hand) that skipped a task's commit gets the same refusal from `checkForkRebase`, which
then marks the request `skipped` (`MarkForkCandidateRebaseSkipped`), so ls, merge and the next loop
agree without another agent run. Only a skipped commit whose own binding lost a commit counts
(`RebaseDroppedTaskCommits` over git's `--cherry-mark` matches, `SkippedTaskCommits` for wording);
an untrailered match beside another binding problem gets the ordinary binding refusal.
A plain
rebase skips a commit whose patch the parent has, and `--reapply-cherry-picks` still drops it as
"already upstream", so reviewed A (remove Z), B (add Z), C on a parent with A's patch landed Z
unseen; only the change comparison catches that. `git merge-tree --write-tree --merge-base` (git
2.40) would give the same guarantee but this Mac's git is 2.39 and merge-tree's own merge-base reads
the view. An honest moved-parent land whose change differs (the parent has part of it) costs a
rebase and a review, never a refusal. A shallow fork is refused before the merge fetches it (and,
for an ordinary fork, before the merge's commit-count header, `fetchForkForMerge`, which an isolated
merge never reaches), in the
land replay and by `CheckRebase` (`RebaseShallow`), and the trusted view sets `core.commitGraph=false`
so a box-written commit-graph cannot answer for the commit objects. An isolated fork's shallow file
cannot hide a commit from its merge or review: the scan, private rebase and landed-as-reviewed check
run in the host's private clone, which never becomes shallow (git rejects the shallow root with only
a warning, exit 0, and leaves the cut-off commits out, so the scan fails on a missing object).
`refuseIsolatedShallowCut` names the file up front when it names a commit that is not the published
boundary or its ancestor; a cut at or below it is a shallow parent's and is allowed. Write such a
file in a test to `<ws>/.git/shallow`: `rev-parse --absolute-git-dir` answers Coop's view, and a
file written there tests nothing.

A reviewed fork whose HEAD moved off its active candidate is not landable, so `coop fork ls` shows it
as `changed` ("changed after review") rather than `ready`, and one with an open conflict request whose
rebase is not finished as `conflict` ("conflicts with your branch"); a `changed` or `skipped`
request and a shallow repository (`driftShallow`, ls only) read as `changed`. For an isolated fork
ls asks `isolatedBoundaryDrift` the two questions merge asks before it fetches, in merge's order: a
merge boundary it cannot read (`isolatedSourceBoundary` against the branch's HEAD, shallow file or
not) is `driftBoundary` (ls only, naming `coop fork merge <name>` for the reason), then a shallow
file that cuts the fork's commits or cannot be read is `driftShallow`; both read only local refs and
files, so ls never calls ready a fork merge refuses. It counts its changes from where
it meets the branch, as merge does; otherwise `forkspace.WorkspaceChanges` counts from where HEAD
meets the fork's base (`origin/HEAD`), since a red-gate land restores the candidate after its rebase
moved that base to the newer parent, which counted the parent's own lines as the fork's removals. `forkCandidateDrift` tells the cases apart: new commits on top,
and a finished rebase onto an open request's `onto` (`driftRebased`), go back to review with the
fork's loop; with a request open, any other HEAD (`driftConflicts`, `driftRebasing`, even a rebase
stopped half way) is the loop's to finish, never a hand reset; without one, a HEAD that no longer
descends is refused by merge and, up front, by a loop start (`RefuseDivergedFork`), both naming the
ways on: reset the fork's branch to the candidate commit, or `coop fork rm <name> --force`. Every
go-back command comes from `forkctl.GoBackCommand`, which starts with `git rebase --abort` while a
rebase is in progress (mid-rebase HEAD is detached, so a reset would leave the branch where it was);
`GoBackLabel` then warns on the row that the abort drops uncommitted conflict resolutions, which no
reflog keeps.

## Changelog
- 2026-11-20 — the skipped-commit refusal's ways out fit about 100 columns: the long way is a
  sentence row and the go-back command its own row (task 2026-11-20-tidy-three-notes-from-the-15-2-live-pass-wide-re).
- 2026-11-20 — ls called an isolated fork with no shallow file ready when merge could not read its
  boundary and refused it; it now shows it as not ready (`driftBoundary`) and names the merge (task
  2026-11-20-show-an-isolated-fork-as-not-ready-in-coop-fork).
- 2026-11-20 — ls no longer calls an isolated fork with a shallow file ready when it cannot read the
  merge boundary (task 2026-11-20-make-the-fold-advice-for-a-task-s-bound-commit-s).
- 2026-11-20 — ls called any shallow isolated fork changed; it now asks `isolatedShallowCut`, as merge
  does, so a shallow clone's inherited file reads ready. A task pass stopped by a first Ctrl-C
  (exit 130, final review skipped) is restored to in progress and redone with the bound-rework fold
  on the rerun: checked, never accepted unreviewed, one work run, as after a crash. The isolated
  publish prompt stays [Y/n] by the destructive-confirm-gate rule's land-step exception (task
  2026-11-20-fix-four-rough-edges-found-by-the-live-isolated).
- 2026-11-20 — a first Ctrl-C during a rebase, check-only or candidate-review pass that succeeded did
  not stop the fork loop (each pass is its own engine run with its own soft stop), so it went on to a
  review or rework. The loop now shares `RunSpec.StopRequested` across passes and stops before the
  next pass, keeping the finished rebase and its checks; each pass's notice says that
  (`softStopWords`). Stopping before the review, not after it, spends no agent run the person asked
  not to start (task 2026-11-20-decide-what-a-first-ctrl-c-does-during-a-fork-s).
- 2026-11-20 — an isolated fork whose merge would land a changed change still showed ready in ls; the
  refusal now records the display-only `isolated-changed` cause, and ls reads isolated records only
  while the branch is where the merge met it (task 2026-11-20-show-in-coop-fork-ls-an-isolated-fork-whose-merg).
- 2026-11-20 — proved a box-written `.git/shallow` in an isolated fork hides nothing from the scan
  (the old test wrote it into Coop's view and tested nothing); a cut into the fork's commits now
  gets a refusal naming the file instead of git's bare exit status (task
  2026-11-20-check-whether-a-box-written-git-shallow-in-an-is).
- 2026-11-19 — wording from the live follow-up pass: merge of a shallow fork printed a "0 commits"
  header before its refusal; the skipped-commit title said "already on main" after a revert; a loop
  on a conflicted isolated fork listed a merge that is refused; the isolated publish header called
  the rebased commit the reviewed one; merge after a stopped final review named `--stop-after-review`
  where a plain rerun finishes it; ls counted a red-gate-restored task fork's base as removals. All
  fixed (task 2026-11-19-fix-seven-wording-and-consistency-notes-from-the).
- 2026-11-19 — the go-back advice for a fork whose branch left the reviewed commit was a bare
  `git reset --hard`, which mid-rebase moves the detached HEAD and not the branch; every go-back
  advice now aborts a rebase in progress first (task
  2026-11-19-name-git-rebase-abort-in-the-go-back-advice-for).
- 2026-11-19 — an isolated fork's merge had no landed-as-reviewed check and the join commits did not
  make one unnecessary: reviewed A (remove a line), B (add it back), C on a branch with A's patch
  landed the line unseen; the merge now compares line edits and bindings and refuses (task
  2026-11-19-confirm-an-isolated-fork-s-merge-lands-exactly-t).
- 2026-11-19 — an isolated fork's conflict said "rework" and ls did not show it, and the card said
  its source shares no history with the parent; the refusal names the two ways out, ls shows it, and
  the history boundary is stated and tested (task 2026-11-19-say-plainly-what-a-conflicted-isolated-fork-can).
- 2026-11-19 — a reviewed round with a merge commit read as "conflicts" and promised a loop that runs
  no agent; it now has the `merges` cause and names the git rebase. A skipped task commit whose
  edits still matched landed with its task commit-less; it is now refused as the other skipped case.
  The check pass's go-back advice names `git rebase --abort` when a rebase is in progress (task
  2026-11-19-tidy-three-leftovers-on-the-fork-rebase-path-mer).
- 2026-11-19 — a hand rebase had no REBASE CHECK lines, so its first review failed as stale and a
  rework run followed; a skipped task commit sent the loop into an empty-commit rework that ended in
  a decision; ls said "conflicts" for a changed land and showed nothing for a shallow fork; "Commit
  X, Y merges" and "The rebase kept it" with no rebase run. Now a check-only pass, a request cause
  (`changed`/`skipped`) that stops a skipped commit at once, and fixed wording (task
  2026-11-19-smooth-the-fork-rebase-path-after-a-hand-rebase).
- 2026-11-19 — a shallow fork could make the land replay a reviewed commit as a root and skip the
  tree check, and a rebase that skipped an already-applied commit could land a change no review saw;
  the land refuses a shallow fork and compares the landed change with the reviewed one (task
  2026-11-19-land-exactly-the-reviewed-content-when-a-fork-is).
- 2026-11-19 — a candidate holding a merge commit could land a change its review never saw (the
  merge's flattening rebase replayed a commit the merge resolution had dropped), and a rebase request
  whose `onto` was already in such a candidate's history looped; merge commits are now refused at
  freeze, in the fence and at merge, and the land checks the tree when the parent did not move (task
  2026-11-19-refuse-a-fork-candidate-whose-commits-include-a).
- 2026-11-19 — a reviewed fork whose commits conflicted with the branch at merge could only be
  redone or the branch changed; the owner chose "rebase and review again": the conflict records a
  rebase request, the fork's loop rebases and resolves, and the result is a new fully reviewed round
  (task 2026-11-19-decide-how-a-reviewed-fork-gets-merged-after-its). Two reviews and a live run
  with real Codex then found: `ls` said "ready to merge" after the conflict, a stopped rebase got
  the hand-reset advice, an empty rebase looped, the pass had no failover and no host binding check,
  it rebased onto the commit of the conflict rather than the branch's head, every rebased round
  failed review as "gate: stale", git's hints and an early "Rebasing onto" printed around refusals,
  and two reviews of one round read the same; all fixed in the same change.
- 2026-11-19 — a fork whose branch moved after review showed "ready to merge", merge said only that
  the candidate was stale, and `--loop -d` printed "Started" then died in its log; all three now say
  it changed and name the ways on (task 2026-11-19-say-how-to-get-out-when-a-fork-s-branch-no-longe).
- 2026-11-19 — a candidate whose rebase conflicted kept its `preparing` journal, so the fork stayed
  "merging", refused loop and `rm --force`, and every rerun reset the fork's branch to the candidate
  and hit the same conflict; a clean conflict now retires the journal (task
  2026-11-19-let-a-fork-merge-be-retried-after-its-conflict-i).
- 2026-11-05 — documented candidate rework/replay, records-only completion, dispute blocking and
  the retained audit-history fences; verified against candidate, scheduler and projection paths.
- 2026-11-04 — the version 1 candidate adoption and the land recovery that rebuilt round one are
  retired; both refuse by name (task 2026-11-04-drop-the-old-fork-and-task-record-readers).
- 2026-09-13 — created from the iterative review-round implementation and its migration,
  interruption, mutation, discard and zero-ahead replay tests.
