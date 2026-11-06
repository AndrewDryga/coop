package tasks

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/AndrewDryga/coop/internal/ui"
)

// readyMerge is a fork whose reviewed work waits for a person to merge it: coop tasks decisions
// lists it after the questions, because the board's Needs you section sends people there for both.
type readyMerge struct {
	fork   string
	titles []string // the tasks its candidate carries
}

// readyMerges reads the forks ready to merge from the project snapshot coop tasks watch draws its
// Needs you section from: a published candidate whose merge has not started (a merge that started
// is already moving). A candidate's tasks are the ones the board folds into its merge row. A test
// that drives decisions without a project ("" repo) has no forks.
func readyMerges(repo string, roots []string) []readyMerge {
	if repo == "" {
		return nil
	}
	snapshot := ReadProjectSnapshot(repo, roots)
	var merges []readyMerge
	for _, fork := range snapshot.Forks {
		if fork.Identity == nil || !fork.Candidate || fork.PendingLand {
			continue
		}
		merge := readyMerge{fork: fork.Name}
		for _, task := range snapshot.Tasks {
			if task.Fork != nil && *task.Fork == *fork.Identity &&
				(task.Phase == ForkAssignmentReady || task.Phase == ForkAssignmentReviewing) {
				merge.titles = append(merge.titles, task.Title)
			}
		}
		merges = append(merges, merge)
	}
	return merges
}

// forkReadyToMerge reports whether fork still has a candidate waiting. The browser asks after it
// runs a merge: coop fork merge exits 0 when the person declines, so the exit status cannot say.
func forkReadyToMerge(repo, fork string) bool {
	for _, merge := range readyMerges(repo, nil) {
		if merge.fork == fork {
			return true
		}
	}
	return false
}

// mergeRefs turns the forks ready to merge into browser refs, after the questions.
func mergeRefs(merges []readyMerge) []decisionRef {
	refs := make([]decisionRef, len(merges))
	for i := range merges {
		refs[i] = decisionRef{merge: &merges[i]}
	}
	return refs
}

// printReadyMerges lists the forks ready to merge: each with its tasks, its name and the commands
// that move it on. gap separates it from a listing above.
func printReadyMerges(p ui.Palette, merges []readyMerge, gap bool) {
	if len(merges) == 0 {
		return
	}
	if gap {
		fmt.Print("\n")
	}
	fmt.Printf("%s%s\n", p.Bold("Forks ready to merge"), p.Dim(fmt.Sprintf(" · %d", len(merges))))
	for _, merge := range merges {
		fmt.Println()
		if len(merge.titles) == 0 {
			fmt.Printf("  Merge fork %s\n", merge.fork)
		}
		for _, title := range merge.titles {
			fmt.Printf("  %s\n", sanitizeCell(title))
		}
		fmt.Printf("    %s %s\n", p.Dim("Fork:"), merge.fork)
		fmt.Printf("    %s coop fork review %s, then coop fork merge %s\n", p.Dim("Review and merge:"), merge.fork, merge.fork)
	}
}

// fprintMergePage shows one fork ready to merge in the decisions browser, with its keys.
func fprintMergePage(out io.Writer, p ui.Palette, merge readyMerge) {
	fmt.Fprintf(out, "\n%s\n", p.Bold("Merge fork "+merge.fork+"?"))
	for _, title := range merge.titles {
		fmt.Fprintf(out, "  %s\n", sanitizeCell(title))
	}
	key := func(k string) string { return p.Cyan(k) }
	fmt.Fprintf(out, "\nReview it, then merge it (Enter to skip):\n  %s%s%s%s%s%s%s%s%s\n%s ",
		key(":r"), p.Dim(" review · "), key(":m"), p.Dim(" merge · "),
		key(":n"), p.Dim(" next · "), key(":p"), p.Dim(" previous · "), key(":q")+p.Dim(" quit"), p.Dim(">"))
}

// runForkCommand runs coop fork <args> as the person would type it, on this terminal: review shows
// its diff and merge asks its own questions. Tests replace it.
var runForkCommand = execForkCommand

// execForkCommand starts coop's own binary, never git, so the Git boundary test can review it as a
// named site rather than a constructor hidden in a function value.
func execForkCommand(args ...string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, append([]string{"fork"}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// runForkFrom runs coop fork <verb> <name> for the browser. The command reports its own refusals,
// so only a command that could not start is reported here.
func runForkFrom(out io.Writer, verb, fork string) {
	var exit *exec.ExitError
	if err := runForkCommand(verb, fork); err != nil && !errors.As(err, &exit) {
		fmt.Fprintf(out, "  Could not run coop fork %s %s: %v\n", verb, fork, err)
	}
}
