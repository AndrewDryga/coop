package forkctl

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// RefuseShallowFork refuses a fork whose repository is shallow. A shallow file, which the box can
// write, makes git treat the commit it names as a root: a rebase replays that commit as if it were
// new, and every ancestry answer git gives reads a cut history. The audits refuse one the same way.
func RefuseShallowFork(ws, name string) error {
	shallow, err := forkspace.GitOutErr(ws, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return fmt.Errorf("fork %s: could not tell whether its repository is shallow: %w", name, err)
	}
	if shallow == "false" {
		return nil
	}
	path := filepath.Join(ws, ".git", "shallow")
	// The fork's own git dir, not Coop's trusted view of it, which only links to the file.
	if out, err := forkspace.GitRefCommand(context.Background(), ws, "rev-parse", "--path-format=absolute", "--git-path", "shallow").Output(); err == nil {
		path = strings.TrimSpace(string(out))
	}
	return ui.CommandFailed("Fork "+name+"'s repository is shallow",
		"Git treats a commit its shallow file names as having no parents, so Coop cannot tell what its commits change, and neither merges nor reviews it. A fork's own clone is never shallow, so the file was written in the fork.",
		[2]string{"Remove the file, then run the command again:", shellLine([]string{"rm", path})},
		[2]string{"Or discard its work:", "coop fork rm " + name + " --force"})
}

// landRefusal is a land the checks stopped after the rebase: err says why, and again says the
// refusal records a rebase request for cause (a tasks.RebaseCause value): the reviewed change must be
// rebased onto the branch and reviewed in full before it can land, or, for a skipped commit, a person
// decides.
type landRefusal struct {
	err   error
	again bool
	cause string
}

// landedAsReviewed checks that the rebase lands the branch plus exactly the reviewed change. When
// the branch has nothing the reviewed commits lack (decided from raw objects, never git's view of the
// fork), the landed tree must be the reviewed tree. When the branch moved, the landed change, from
// the branch to the rebased head, must make the same line edits as the reviewed change, from where
// the reviewed commits start to the reviewed head. A rebase skips a commit whose patch the branch
// already has, so reviewed commits that remove a line and then add it back, on a branch that also
// removed it, land the line though the reviewed change never touches it.
func landedAsReviewed(ws, name, branch string, intent landIntent) *landRefusal {
	parent, candidate := intent.ParentBefore, intent.Candidate
	unverifiable := func(err error) *landRefusal {
		return &landRefusal{err: fmt.Errorf("%s: could not check that the rebase lands exactly the reviewed change: %w; nothing merged and the reviewed commit is back", name, err)}
	}
	unmoved, err := tasks.InRawHistory(ws, parent, candidate.Head)
	if err != nil {
		return unverifiable(err)
	}
	if unmoved {
		if intent.RebasedTree == candidate.Tree {
			return nil
		}
		return &landRefusal{err: fmt.Errorf("%s: its rebase onto %s changed the reviewed tree %s to %s although %s had nothing new, so it would land content no review saw; nothing merged and the reviewed commit is back. Discard its work and return its tasks to the queue: coop fork rm %s --force",
			name, branch, candidate.Tree[:12], intent.RebasedTree[:12], branch, name)}
	}
	base, err := tasks.ForkCommitsBase(ws, parent, candidate.Head)
	if err != nil {
		return unverifiable(err)
	}
	reviewed, err := lineEdits(ws, base, candidate.Head)
	if err != nil {
		return unverifiable(err)
	}
	landed, err := lineEdits(ws, parent, intent.RebasedHead)
	if err != nil {
		return unverifiable(err)
	}
	// A skipped commit bound to a task leaves that task without a commit of its own, even when the
	// line edits still match (the parent has both a change and its undoing). A rebase pass would skip
	// it again, or keep it empty, which review rightly fails, so this stops for a person. A rebase
	// only drops commits, so the bindings need reading only when it dropped some.
	dropped, err := rebaseDroppedCommits(ws, parent, intent.RebasedHead, base, candidate.Head)
	if err != nil {
		return unverifiable(err)
	}
	if dropped {
		problems, err := tasks.RebaseBindingProblems(ws, parent, candidate.Head, intent.RebasedHead)
		if err != nil {
			return unverifiable(err)
		}
		if len(problems) > 0 {
			return &landRefusal{again: true, cause: tasks.RebaseCauseSkipped, err: skippedCommitError(ws, name, branch, strings.Join(problems, " "), " Nothing was merged.", "")}
		}
	}
	if reviewed == landed {
		return nil
	}
	return &landRefusal{again: true, cause: tasks.RebaseCauseChanged, err: ui.CommandFailed("Fork "+name+"'s reviewed change would land changed on "+parentBranchWords(branch),
		"Rebased onto "+parentBranchWords(branch)+", its commits would not land exactly the reviewed change, for example because "+
			parentBranchWords(branch)+" already has part of it. Nothing was merged. The fork's loop rebases them onto "+
			parentBranchWords(branch)+" and the result is reviewed again before it can merge.",
		[2]string{"Rebase and review:", "coop fork " + name + " --loop"},
		[2]string{"Then merge it:", "coop fork merge " + name},
		[2]string{"Or discard its work:", "coop fork rm " + name + " --force"})}
}

// rebaseDroppedCommits says the rebase of base..head onto parent, which made rebased, left fewer
// commits than it started with. A count git cannot give is an error, never "none dropped": the
// caller refuses rather than skip the binding check.
func rebaseDroppedCommits(ws, parent, rebased, base, head string) (bool, error) {
	count := func(from, to string) (string, error) {
		out, err := forkspace.GitOutErr(ws, "rev-list", "--count", from+".."+to)
		if err == nil && out == "" {
			err = fmt.Errorf("git rev-list --count %s..%s printed nothing", from, to)
		}
		return out, err
	}
	after, err := count(parent, rebased)
	if err != nil {
		return false, err
	}
	before, err := count(base, head)
	if err != nil {
		return false, err
	}
	return after != before, nil
}

// skippedCommitError is the one refusal of reviewed commits one of which the parent branch already
// has, bound to a task: rebased there, the commit drops out and its task keeps no commit of its own.
// Coop lands only each task's reviewed commits, and a rebase cannot keep that one, so the fork's
// loop does not try; merge, loop and ls all send the person to the same two ways on. detail names
// the commits or bindings, "" when unknown; tail says what did not happen. back is the reviewed
// commit when the fork's branch is no longer on it, "" when it is.
func skippedCommitError(ws, name, branch, detail, tail, back string) error {
	words := parentBranchWords(branch)
	// "History", not the branch: a commit a later one reverted still counts, and git matches changes
	// ignoring whitespace, so the branch's files may differ from the fork's.
	cause := words + "'s history already has the change of one of the fork's reviewed commits that belongs to a task (git ignores whitespace when it matches changes), so a rebase onto " +
		words + " skips that commit and its task is left without a commit of its own."
	if detail != "" {
		cause += " " + detail
	}
	// A revert does not help: git rebase skips a commit whose change is anywhere in the branch's
	// history since the fork started, even one a later commit reverted.
	cause += " Coop merges only each task's own reviewed commits, so no rebase or review can fix this, and reverting the change on " +
		words + " does not either: a rebase still skips the commit." + tail
	again := [2]string{"Or remove " + words + "'s commit with that change from its history, then merge again:", "coop fork merge " + name}
	if back != "" {
		again = [2]string{GoBackLabel(ws, "Or remove "+words+"'s commit with that change from its history, go back to the reviewed commit and merge again:"),
			GoBackCommand(ws, "reset", "--hard", back) + " && coop fork merge " + name}
	}
	return ui.CommandFailed("Fork "+name+"'s reviewed commit is already in "+words+"'s history", cause,
		[2]string{"Discard its work and return its tasks to the queue:", "coop fork rm " + name + " --force"}, again)
}

// SkippedCommitError is the fork loop's refusal of reviewed commits one of which the parent branch
// rebase.Branch already has, bound to a task: before its rebase pass, when the open request says a
// merge found it (tail: no agent ran), or after the pass, when its rebase skipped such a commit
// (moved: the fork's branch is off the reviewed commit). shas names the skipped task commits; nil
// names them as git's own patch matching tells, for display only.
func SkippedCommitError(ws, name string, candidate tasks.ForkCandidate, rebase tasks.ForkCandidateRebase, shas []string, tail string, moved bool) error {
	back := ""
	if moved {
		back = candidate.Head
	}
	if shas == nil {
		shas = SkippedTaskCommits(ws, rebase.Onto, candidate.Head)
	}
	return skippedCommitError(ws, name, rebase.Branch, skippedCommitsDetail(shas), tail, back)
}

// SkippedTaskCommits is SkippedCommits narrowed to the commits that carry a Coop-Task binding, read
// from raw objects: a skipped commit with no trailer costs no task its commit.
func SkippedTaskCommits(ws, onto, head string) []string {
	bound, err := tasks.TaskBoundCommits(ws, onto, head, SkippedCommits(ws, onto, head))
	if err != nil {
		return nil
	}
	return bound
}

// SkippedCommits is the commits of head whose change onto already has, by git's own patch matching
// (`--cherry-mark`), the one a rebase uses to skip them; none when git cannot tell. It only chooses
// and words a refusal: what Coop refuses is decided from raw objects.
func SkippedCommits(ws, onto, head string) []string {
	out, err := forkspace.GitOutErr(ws, "rev-list", "--cherry-mark", "--right-only", "--no-merges", onto+"..."+head)
	if err != nil {
		return nil
	}
	var shas []string
	for _, line := range strings.Split(out, "\n") {
		if sha, ok := strings.CutPrefix(line, "="); ok && forkspace.ValidObjectID(sha) {
			shas = append(shas, sha)
		}
	}
	return shas
}

func skippedCommitsDetail(shas []string) string {
	short := make([]string, len(shas))
	for i, sha := range shas {
		short[i] = sha[:12]
	}
	switch len(short) {
	case 0:
		return ""
	case 1:
		return "Its commit " + short[0] + " is already there."
	}
	return "Its commits " + strings.Join(short, ", ") + " are already there."
}

// lineEdits is the change from one commit to another as its line edits alone: each file's removed
// and added lines in order, without line numbers or blob names, which a rebase onto a moved branch
// changes for an honest change too. No driver, text conversion or rename detection takes part.
func lineEdits(ws, from, to string) (string, error) {
	diff, err := forkspace.GitOutErr(ws, "diff", "-U0", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--binary", "--full-index", from, to)
	if err != nil {
		return "", err
	}
	var kept []string
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "@@") || strings.HasPrefix(line, "index ") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), nil
}
