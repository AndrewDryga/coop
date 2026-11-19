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
// reviewed change must be rebased onto the branch and reviewed in full before it can land.
type landRefusal struct {
	err   error
	again bool
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
	if reviewed == landed {
		return nil
	}
	return &landRefusal{again: true, err: ui.CommandFailed("Fork "+name+"'s reviewed change would land changed on "+parentBranchWords(branch),
		"Rebased onto "+parentBranchWords(branch)+", its commits would not land exactly the reviewed change, for example because "+
			parentBranchWords(branch)+" already has a commit of theirs, which a rebase skips. Nothing was merged. The fork's loop rebases them onto "+
			parentBranchWords(branch)+" and the result is reviewed again before it can merge.",
		[2]string{"Rebase and review:", "coop fork " + name + " --loop"},
		[2]string{"Then merge it:", "coop fork merge " + name},
		[2]string{"Or discard its work:", "coop fork rm " + name + " --force"})}
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
