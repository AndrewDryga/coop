package forkctl

import (
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// candidateDrift is how a fork's branch stands against its final review. A merge lands only the
// exact reviewed commit. A loop reviews new commits on top of it again, and while a conflicting merge's
// rebase request is open it rebases the reviewed commits onto the parent and reviews that, but a branch
// that otherwise no longer builds on it (a person rebased or amended in the fork) is one neither a
// merge nor a loop takes.
type candidateDrift int

const (
	driftNone candidateDrift = iota
	driftAhead
	driftConflicts // a merge conflicted; the branch is still the reviewed commit
	driftChanged   // a merge would have landed another change; the branch is still the reviewed commit
	driftSkipped   // the parent already has one of its task-bound commits, which a rebase skips
	driftRebasing  // a rebase onto the parent is started or stopped, not finished
	driftEmpty     // a finished rebase left none of the fork's commits
	driftMerges    // its new commits, or its rebase, include a merge commit
	driftShallow   // its repository is shallow (ls only: merge and loop refuse it on their own)
	driftRebased
	driftDiverged
)

// forkCandidateDrift compares the fork's HEAD with its reviewed candidate. rebase is the open rebase
// request, nil for none. A HEAD Coop cannot read is no drift: whatever reads it next says what is
// wrong.
func forkCandidateDrift(ws, name string, candidate tasks.ForkCandidate, head string, rebase *tasks.ForkCandidateRebase) candidateDrift {
	if !forkspace.ValidObjectID(head) {
		return driftNone
	}
	// No rebase keeps a task commit the parent already has, wherever the branch is now.
	if rebase != nil && rebase.Cause == tasks.RebaseCauseSkipped {
		return driftSkipped
	}
	// An isolated fork's merge refusal is only a record of the last merge: it holds while the branch is
	// the reviewed commit, and anything else reads as if there were no request.
	if rebase != nil && tasks.IsolatedRebaseCause(rebase.Cause) {
		if head == candidate.Head && rebase.Cause == tasks.RebaseCauseIsolatedChanged {
			return driftChanged
		}
		if head == candidate.Head {
			return driftConflicts
		}
		rebase = nil
	}
	if head == candidate.Head {
		switch {
		case rebase == nil:
			return driftNone
		case rebase.Cause == tasks.RebaseCauseChanged:
			return driftChanged
		case rebase.Cause == tasks.RebaseCauseMerges:
			return driftMerges // a person straightens it; no agent pass does
		}
		return driftConflicts
	}
	if _, err := forkspace.GitOutErr(ws, "merge-base", "--is-ancestor", candidate.Head, head); err == nil {
		if merges, err := tasks.MergeCommitsOutside(ws, candidate.Head, head); err == nil && len(merges) > 0 {
			return driftMerges
		}
		return driftAhead
	}
	if rebase == nil {
		return driftDiverged
	}
	// While a request is open, any other branch is a rebase the loop finishes or redoes: never one to
	// reset by hand, which in the middle of a rebase would throw its work away.
	switch CheckRebase(ws, name, rebase.Onto) {
	case RebaseFinished:
		return driftRebased
	case RebaseEmpty:
		return driftEmpty
	case RebaseMerges:
		return driftMerges
	}
	if merges, err := tasks.MergeCommitsOutside(ws, rebase.Onto, head); err == nil && len(merges) > 0 {
		return driftMerges // the loop would refuse it before any agent pass, for this reason
	}
	return driftRebasing
}

// droppedReviewedWork names the files where the parent commit onto lacks a change of the reviewed
// commits, those from where they meet onto to head: a line they add that onto's file does not hold,
// or a file they delete or change that onto keeps differently. None means the parent already has the
// reviewed work, so a rebase that left no commits lost nothing; any means it dropped work.
func droppedReviewedWork(ws, head, onto string) []string {
	base := forkspace.GitOut(ws, "merge-base", onto, head)
	if base == "" {
		return []string{"(Coop could not tell where the reviewed commits start)"}
	}
	diff, err := forkspace.GitOutErr(ws, "diff", "-U0", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", base, head)
	if err != nil {
		return []string{"(Coop could not read the reviewed changes)"}
	}
	var missing []string
	path, deleted := "", false
	added := map[string][]string{}
	var order []string
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			path, deleted = "", false
		case strings.HasPrefix(line, "deleted file mode"):
			deleted = true
		case strings.HasPrefix(line, "--- a/") && deleted:
			path = strings.TrimPrefix(line, "--- a/")
			if forkspace.GitOut(ws, "rev-parse", "--verify", "--quiet", onto+":"+path) != "" {
				missing = append(missing, path)
			}
		case strings.HasPrefix(line, "+++ b/"):
			path = strings.TrimPrefix(line, "+++ b/")
			order = append(order, path)
		case strings.HasPrefix(line, "Binary files ") && path == "":
			fields := strings.Fields(line)
			if len(fields) >= 5 && strings.HasPrefix(fields[4], "b/") {
				binary := strings.TrimPrefix(fields[4], "b/")
				if forkspace.GitOut(ws, "rev-parse", onto+":"+binary) != forkspace.GitOut(ws, "rev-parse", head+":"+binary) {
					missing = append(missing, binary)
				}
			}
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++ ") && path != "" && !deleted:
			added[path] = append(added[path], line[1:])
		}
	}
	for _, file := range order {
		have := map[string]bool{}
		for _, line := range strings.Split(forkspace.GitOut(ws, "show", onto+":"+file), "\n") {
			have[strings.TrimRight(line, "\r")] = true
		}
		for _, line := range added[file] {
			if !have[strings.TrimRight(line, "\r")] {
				missing = append(missing, file)
				break
			}
		}
	}
	for i := range missing {
		missing[i] = ui.SafeInline(missing[i])
	}
	return missing
}

// emptyRebaseError is the refusal for a finished rebase that left none of the fork's commits. Coop,
// the loop, merge and ls all say the same: either the parent already has the reviewed work, which
// only a person can close, or the rebase dropped it, which a rebase from the reviewed commit redoes.
func emptyRebaseError(ws, name string, candidate tasks.ForkCandidate, onto string, merging bool) error {
	back := GoBackCommand(ws, "reset", "--hard", candidate.Head)
	tail := ""
	if merging {
		tail = " Nothing was merged."
	}
	if dropped := droppedReviewedWork(ws, candidate.Head, onto); len(dropped) > 0 {
		cause := "Its branch is now " + onto[:12] + " itself, and the parent branch lacks reviewed changes in " + strings.Join(dropped, ", ") + "."
		if problems, err := tasks.RebaseBindingProblems(ws, onto, candidate.Head, onto); err == nil && len(problems) > 0 {
			cause += " " + strings.Join(problems, " ")
		}
		return ui.CommandFailed("Fork "+name+"'s rebase dropped its reviewed work", cause+" Nothing was reviewed."+tail,
			[2]string{GoBackLabel(ws, "Go back to the reviewed commit:"), back},
			[2]string{"Then rebase again:", "coop fork " + name + " --loop"},
			[2]string{"Or discard its work:", "coop fork rm " + name + " --force"})
	}
	// Completing the tasks of work that is already on the branch would need a review of nothing,
	// so this stays a person's call.
	return ui.CommandFailed("Fork "+name+"'s rebase left no commits",
		"Its branch is now "+onto[:12]+" itself: the parent branch already has every change of the reviewed commits, so there is nothing to review or merge. Coop does not complete tasks without reviewed work of their own."+tail,
		[2]string{"Discard its work and return its tasks to the queue:", "coop fork rm " + name + " --force"},
		[2]string{GoBackLabel(ws, "Or go back to the reviewed commit:"), back})
}

// EmptyRebaseError is emptyRebaseError for the fork loop, which stops on it before any review.
func EmptyRebaseError(ws, name string, candidate tasks.ForkCandidate, onto string) error {
	return emptyRebaseError(ws, name, candidate, onto, false)
}

// parentBranchWords names the parent branch a request recorded, or says what it was: git names a
// detached HEAD "HEAD".
func parentBranchWords(branch string) string {
	if branch == "" || branch == "HEAD" {
		return "your branch"
	}
	return ui.SafeInline(branch)
}

// candidateDriftError is the refusal for a fork whose branch moved after its final review, with the
// ways on that work today. Going back is a hard reset of the fork's branch, so it names the commit
// the fork's reflog keeps. rebase is the open rebase request, nil for none.
func candidateDriftError(ws, name string, candidate tasks.ForkCandidate, head string, drift candidateDrift, rebase *tasks.ForkCandidateRebase, merging bool) error {
	back := GoBackCommand(ws, "reset", "--hard", candidate.Head)
	loop := "coop fork " + name + " --loop"
	discard := [2]string{"Or discard its work:", "coop fork rm " + name + " --force"}
	tail := ""
	if merging {
		tail = " Nothing was merged."
	}
	switch drift {
	case driftAhead:
		return ui.CommandFailed("Fork "+name+" has new commits since its final review",
			"Coop merges only the reviewed commit, so the new commits need a review first."+tail,
			[2]string{"Review them:", loop},
			[2]string{GoBackLabel(ws, "Or go back to the reviewed commit:"), back}, discard)
	case driftRebased:
		return ui.CommandFailed("Fork "+name+" is rebased but not reviewed again yet",
			"Coop merges only reviewed commits, so the rebased branch needs its review first."+tail,
			[2]string{"Review it:", loop},
			[2]string{"Then merge it:", "coop fork merge " + name}, discard)
	case driftEmpty:
		return emptyRebaseError(ws, name, candidate, rebase.Onto, merging)
	case driftMerges:
		base := candidate.Head
		if rebase != nil {
			base = rebase.Onto
		}
		merges, _ := tasks.MergeCommitsOutside(ws, base, head)
		return MergeCommitsError(ws, name, base, merges, tail)
	case driftSkipped:
		back := ""
		if head != candidate.Head {
			back = candidate.Head
		}
		return skippedCommitError(ws, name, rebase.Branch, skippedCommitsDetail(SkippedTaskCommits(ws, rebase.Onto, candidate.Head)), tail, back)
	case driftShallow:
		return RefuseShallowFork(ws, name)
	case driftConflicts, driftChanged, driftRebasing:
		why := "Its reviewed commits conflict with " + parentBranchWords(rebase.Branch) + ", and its loop rebases them, resolves the conflict and reviews the result before it can merge."
		if rebase.Cause == tasks.RebaseCauseChanged {
			why = "Its reviewed change would land changed on " + parentBranchWords(rebase.Branch) + ", so its loop rebases its commits there and reviews the result before it can merge."
		}
		return ui.CommandFailed("Fork "+name+"'s rebase onto "+parentBranchWords(rebase.Branch)+" is not finished", why+tail,
			[2]string{"Rebase and review:", loop},
			[2]string{"Then merge it:", "coop fork merge " + name}, discard)
	}
	return ui.CommandFailed("Fork "+name+" changed after its final review",
		"Its branch is at "+head[:12]+", which does not build on the reviewed commit "+candidate.Head[:12]+
			", so Coop can neither merge it nor review it again."+tail+
			" Going back keeps "+head[:12]+" in the fork's reflog.",
		[2]string{GoBackLabel(ws, "Go back to the reviewed commit:"), back},
		[2]string{"Then merge it:", "coop fork merge " + name},
		discard)
}

// candidateConflictError is the refusal for a reviewed candidate whose commits conflict with the
// parent branch. It names the way on: the fork's loop rebases the work, its agent resolves the
// conflict, and the result is reviewed again as a new round before it can merge.
func candidateConflictError(name string, failed *rebaseFailedError) error {
	files := failed.conflicts
	more := ""
	if len(files) > 5 {
		files, more = files[:5], " and more"
	}
	for i := range files {
		files[i] = ui.SafeInline(files[i])
	}
	branch := parentBranchWords(failed.branch)
	return ui.CommandFailed("Fork "+name+"'s reviewed commits conflict with "+branch,
		"They conflict in "+strings.Join(files, ", ")+more+", so nothing merged. The fork's loop rebases them onto "+
			branch+", its agent resolves the conflict, and the result is reviewed again before it can merge.",
		[2]string{"Rebase and review:", "coop fork " + name + " --loop"},
		[2]string{"Then merge it:", "coop fork merge " + name},
		[2]string{"Or discard its work:", "coop fork rm " + name + " --force"})
}

// rebaseRefusedError is the refusal for a reviewed candidate whose rebase git refused with no
// conflict: signing, when the rebase signs, and otherwise whatever git said.
func rebaseRefusedError(name string, failed *rebaseFailedError) error {
	branch := parentBranchWords(failed.branch)
	discard := [2]string{"Or discard its work:", "coop fork rm " + name + " --force"}
	if failed.signing {
		return ui.CommandFailed("Fork "+name+"'s reviewed commits could not be signed onto "+branch,
			"Git stopped while signing them, so nothing merged. Its output is above.",
			[2]string{"Fix the signing, then merge again:", "coop fork merge " + name}, discard)
	}
	cause := "Git stopped the rebase, so nothing merged."
	if failed.cause != nil {
		cause += " " + ui.SafeInline(failed.cause.Error())
	}
	return ui.CommandFailed("Fork "+name+"'s reviewed commits could not be rebased onto "+branch, cause,
		[2]string{"Merge again:", "coop fork merge " + name}, discard)
}

// RebaseResult is how far a fork's rebase onto a parent commit got.
type RebaseResult int

const (
	// RebaseUnfinished: not started, stopped mid-way, off the fork's branch, or with uncommitted
	// changes. The loop's rebase pass redoes it.
	RebaseUnfinished RebaseResult = iota
	// RebaseFinished: the fork's own branch, clean, builds on the parent commit with commits of its own.
	RebaseFinished
	// RebaseEmpty: the rebase finished but left no commit of the fork's: the parent already has them.
	RebaseEmpty
	// RebaseMerges: the branch builds on the parent commit but keeps a merge commit, which Coop
	// neither reviews nor merges; another agent pass would only keep it again.
	RebaseMerges
	// RebaseShallow: the fork's repository is shallow, so nothing it says can be checked.
	RebaseShallow
)

// RebaseInProgress says git has a rebase in progress in the fork, in its own git dir (where a box or
// a person runs one) or Coop's view of it, so advice that moves its branch must abort it first.
func RebaseInProgress(ws string) bool {
	return untrustedRebaseState(ws) != "" || leftoverRebaseState(ws) != ""
}

// GoBackCommand is the git command a person runs to put the fork's branch back on a commit, such as
// `reset --hard <reviewed>`. In the middle of a rebase HEAD is detached, so a reset would move it and
// not the branch, and git refuses to switch branches at all; the rebase is aborted first then.
func GoBackCommand(ws string, move ...string) string {
	back := shellLine(append([]string{"git", "-C", ws}, move...))
	if RebaseInProgress(ws) {
		back = shellLine([]string{"git", "-C", ws, "rebase", "--abort"}) + " && " + back
	}
	return back
}

// GoBackLabel is label for a row whose command is GoBackCommand's. An abort throws away conflict
// resolutions the person has not committed, which no reflog keeps, so then the label says so.
func GoBackLabel(ws, label string) string {
	if !RebaseInProgress(ws) {
		return label
	}
	return strings.TrimSuffix(label, ":") + " (the abort drops conflict resolutions not yet committed):"
}

// IsolatedRefusalLoopError is the fork loop's refusal of an isolated fork whose reviewed commits
// conflicted with the parent branch at its last merge, or would have landed changed (rebase.Cause):
// no agent may rebase it, so the ways on are a person's, the same two coop fork merge and coop fork
// ls name.
//
// Once the parent's HEAD (repo's) is no longer the commit that merge met, as coop fork ls then shows
// the fork ready, it says the branch changed since and that a merge checks it again.
func IsolatedRefusalLoopError(repo, ws, name string, rebase tasks.ForkCandidateRebase) error {
	words := parentBranchWords(rebase.Branch)
	agent := ReadForkAgent(ws)
	if agent == "" {
		agent = "<agent>"
	}
	discard := [][2]string{
		{"Or, if you choose to discard its work, remove the fork:", "coop fork rm " + name + " --force"},
		{"Then start a new isolated fork from " + words + ":", "coop fork " + name + " " + agent + " --isolated"},
	}
	noAgent := "It is an isolated fork: nothing of " + words + " may enter its box, so no agent can rebase it. No agent ran, and nothing was reviewed or merged."
	refused := "conflicted with " + words
	if rebase.Cause == tasks.RebaseCauseIsolatedChanged {
		refused = "would have landed changed on " + words
	}
	if forkspace.GitOut(repo, "rev-parse", "HEAD") != rebase.Onto {
		return ui.CommandFailed("Fork "+name+" has nothing for its loop to do",
			"Its reviewed commits "+refused+" at its last merge, and "+words+" has changed since. "+noAgent,
			append([][2]string{{"Merge it to check it against " + words + " as it is now:", "coop fork merge " + name}}, discard...)...)
	}
	headline, apply := "Fork "+name+"'s reviewed commits conflict with "+words, "apply"
	if rebase.Cause == tasks.RebaseCauseIsolatedChanged {
		headline, apply = "Fork "+name+"'s reviewed change would land changed on "+words, "apply as reviewed"
	}
	return ui.CommandFailed(headline, noAgent,
		append([][2]string{{"Change " + words + " until the fork's commits " + apply + ", then merge again:", "coop fork merge " + name}}, discard...)...)
}

// CheckRebase reports how far the fork's rebase onto the parent commit onto got, whoever ran it.
func CheckRebase(ws, name, onto string) RebaseResult {
	if RefuseShallowFork(ws, name) != nil {
		return RebaseShallow // nothing git says about this history can be trusted
	}
	head := forkspace.GitOut(ws, "rev-parse", "HEAD")
	if head == "" || leftoverRebaseState(ws) != "" || untrustedRebaseState(ws) != "" ||
		forkspace.GitOut(ws, "symbolic-ref", "--quiet", "--short", "HEAD") != name || requireTrackedClean(ws, name) != nil {
		return RebaseUnfinished
	}
	if head == onto {
		return RebaseEmpty
	}
	if _, err := forkspace.GitOutErr(ws, "merge-base", "--is-ancestor", onto, head); err != nil {
		return RebaseUnfinished
	}
	// A rebase leaves a straight line of commits on onto. A merge left above it, such as one of the
	// parent into the fork, is what the rebase pass removes, so the pass runs again.
	if merges, err := tasks.MergeCommitsOutside(ws, onto, head); err != nil {
		return RebaseUnfinished
	} else if len(merges) > 0 {
		return RebaseMerges
	}
	return RebaseFinished
}

// RefuseDivergedFork refuses a loop on a fork whose branch no longer builds on its reviewed
// candidate. The loop would stop at its first step, after a background start said "Started", so the
// refusal comes in the terminal, with the same ways on as coop fork merge gives. A fork whose merge
// conflicted is the loop's to rebase, wherever its branch is, except an isolated one, which no loop
// rebases; a skipped task commit, which the loop refuses before any agent, is refused here too.
func RefuseDivergedFork(repo string, identity forkspace.Identity) error {
	candidate, ok, err := tasks.ReadForkCandidate(repo, identity)
	if err != nil || !ok {
		return err
	}
	rebase, requested, err := tasks.ReadForkCandidateRebase(repo, identity)
	if err != nil {
		return err
	}
	ws := forkspace.Workspace(repo, identity.Name)
	head := forkspace.GitOut(ws, "rev-parse", "HEAD")
	switch {
	case requested && rebase.Cause == tasks.RebaseCauseSkipped:
		// The loop refuses this before any agent runs; say it here, not after a "Started".
		return SkippedCommitError(ws, identity.Name, candidate, rebase, nil, " No agent ran, and nothing was reviewed or merged.", head != candidate.Head)
	case requested && tasks.IsolatedRebaseCause(rebase.Cause) && head == candidate.Head:
		// No agent may rebase an isolated fork, so its loop has nothing to do: say so here, not after a
		// "Started", and not with an exit a `&& coop fork merge` would take for success.
		return IsolatedRefusalLoopError(repo, ws, identity.Name, rebase)
	case requested && !tasks.IsolatedRebaseCause(rebase.Cause):
		return nil // the loop's to rebase or check, wherever the branch is
	}
	// An isolated fork's refusal record lets nothing build on the parent, so its branch is held to
	// the reviewed commit exactly as without one.
	if forkCandidateDrift(ws, identity.Name, candidate, head, nil) != driftDiverged {
		return nil
	}
	return candidateDriftError(ws, identity.Name, candidate, head, driftDiverged, nil, false)
}
