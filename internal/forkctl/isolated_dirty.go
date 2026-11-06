package forkctl

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ui"
)

// parentDirtyError is a checkout a publication will not start from because it differs from its last
// commit: files staged or changed, an unfinished merge, or files hidden from Git's change checks.
// dir is the checkout, a submodule's when nested. The lists hold what Git reports, and parentRefusal
// draws them as the block that names the files and the command that clears them.
type parentDirtyError struct {
	dir    string
	nested bool

	staged, changed, conflicts []string
	assumed, skipped           []string // marked assume-unchanged or skip-worktree
}

func (e *parentDirtyError) Error() string {
	var found []string
	for _, group := range []struct {
		what  string
		files []string
	}{{"staged", e.staged}, {"changed", e.changed}, {"in conflict", e.conflicts},
		{"assume-unchanged", e.assumed}, {"skip-worktree", e.skipped}} {
		if len(group.files) > 0 {
			found = append(found, group.what+" "+fileList(group.files))
		}
	}
	if len(found) == 0 {
		return e.dir + " differs from its last commit"
	}
	return e.dir + " differs from its last commit: " + strings.Join(found, "; ")
}

// dirtyCheckout reads what Git reports as different in the checkout at dir, and adds path, the file
// Coop's own comparison stopped at, when Git does not list it. It is nil when Git sees no difference
// at all, which leaves the refusal to the comparison that found one.
func dirtyCheckout(dir string, nested bool, path string) *parentDirtyError {
	e := readDirtyChanges(dir, nested)
	if len(e.staged)+len(e.changed)+len(e.conflicts) == 0 {
		return nil
	}
	if !slices.Contains(e.staged, path) && !slices.Contains(e.changed, path) && !slices.Contains(e.conflicts, path) {
		e.changed = append(e.changed, path)
	}
	return e
}

// readDirtyChanges lists the files Git reports as staged, changed or in conflict in the checkout at
// dir. A failure to read leaves the lists empty, and the block then names no files.
func readDirtyChanges(dir string, nested bool) *parentDirtyError {
	e := &parentDirtyError{dir: dir, nested: nested}
	out, err := forkspace.ObserveGit(context.Background(), dir, "status", "--porcelain=v1", "-z", "--untracked-files=no")
	if err != nil {
		return e
	}
	tokens := strings.Split(string(out), "\x00")
	for i := 0; i < len(tokens); i++ {
		entry := tokens[i]
		if len(entry) < 4 {
			continue
		}
		x, y, path := entry[0], entry[1], entry[3:]
		if x == 'R' || x == 'C' || y == 'R' || y == 'C' {
			i++ // a rename or copy is followed by the path it came from
		}
		switch {
		case x == 'U' || y == 'U' || x == 'A' && y == 'A' || x == 'D' && y == 'D':
			e.conflicts = append(e.conflicts, path)
		default:
			if x != ' ' && x != '?' {
				e.staged = append(e.staged, path)
			}
			if y != ' ' && y != '?' {
				e.changed = append(e.changed, path)
			}
		}
	}
	return e
}

// parentDirtyBlock is the block for a checkout that differs from its last commit: what differs, and
// the exact command that clears it.
func parentDirtyBlock(e *parentDirtyError) *ui.UsageError {
	where, cause, gitCmd := "Your project", "A merge needs your checkout to match its last commit.", "git"
	if e.nested {
		where, cause = "A submodule of your project", "A merge needs every checkout to match its last commit. This one is "+e.dir+"."
		if quoted, ok := ui.ShellWord(e.dir); ok {
			gitCmd = "git -C " + quoted
		}
	}
	inside := strings.ToLower(where[:1]) + where[1:]
	add := func(what string, files []string) {
		if len(files) > 0 {
			cause += "\n" + what + ": " + fileList(files)
		}
	}
	again := [2]string{"", "Then merge again."}
	switch {
	case len(e.conflicts) > 0:
		add("In conflict", e.conflicts)
		return ui.CommandFailed(where+" has unresolved merge conflicts", cause,
			[2]string{"", "Resolve them and commit, or abort the merge, rebase or cherry-pick in progress, then merge again."})
	case len(e.assumed)+len(e.skipped) > 0:
		cause = "Git does not report changes to files marked assume-unchanged or skip-worktree, so a merge cannot tell whether they changed."
		add("Marked assume-unchanged", e.assumed)
		add("Marked skip-worktree", e.skipped)
		var rows [][2]string
		if len(e.assumed) > 0 {
			rows = append(rows, undoFlagRow("Undo assume-unchanged:", gitCmd, "--no-assume-unchanged", e.assumed,
				[2]string{"List them:", gitCmd + " ls-files -v"}))
		}
		if len(e.skipped) > 0 {
			rows = append(rows, undoFlagRow("Undo skip-worktree:", gitCmd, "--no-skip-worktree", e.skipped,
				[2]string{"A sparse checkout?", gitCmd + " sparse-checkout disable"}))
		}
		return ui.CommandFailed("Some files in "+inside+" are hidden from Git's change checks", cause, append(rows, again)...)
	}
	add("Staged", e.staged)
	add("Changed", e.changed)
	return ui.CommandFailed(where+" has uncommitted changes", cause,
		[2]string{"Commit them:", gitCmd + ` commit -a -m "your message"`},
		[2]string{"Or set them aside:", gitCmd + ` stash push -m "before coop fork merge"`}, again)
}

// undoFlagRow is the command that clears flag on files, or fallback when there are too many files, or
// one of them cannot be written as a shell word, for one command to name them all.
func undoFlagRow(label, gitCmd, flag string, files []string, fallback [2]string) [2]string {
	const most = 20
	words := []string{gitCmd, "update-index", flag, "--"}
	for _, file := range files {
		quoted, ok := ui.ShellWord(file)
		if !ok || len(files) > most {
			return fallback
		}
		words = append(words, quoted)
	}
	return [2]string{label, strings.Join(words, " ")}
}

// fileList names up to five files, each quoted, and counts the rest.
func fileList(files []string) string {
	const shown = 5
	var names []string
	for _, file := range files[:min(len(files), shown)] {
		names = append(names, fmt.Sprintf("%q", file))
	}
	if len(files) > shown {
		return strings.Join(names, ", ") + " and " + fmt.Sprint(len(files)-shown) + " more"
	}
	return ui.List(names, "and")
}
