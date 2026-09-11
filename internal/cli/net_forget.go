package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/ui"
)

func parseNetForgetArgs(args []string) (string, error) {
	const command, usage = "coop net forget", "coop net forget [--project <path>]"
	project := ""
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		if name != "--project" {
			if !strings.HasPrefix(args[i], "-") {
				return "", ui.UnexpectedArgument(args[i], command, usage)
			}
			return "", unknownOptionErr(args[i], command, []string{"--project"})
		}
		if project != "" {
			return "", ui.RepeatedOption("--project", command)
		}
		if !inline {
			i++
			if i == len(args) {
				return "", ui.MissingOptionValue("--project", command, "coop net forget --project ~/Projects/old-app")
			}
			value = args[i]
		}
		if value == "" {
			return "", ui.MissingOptionValue("--project", command, "coop net forget --project ~/Projects/old-app")
		}
		project = value
	}
	return project, nil
}

// cmdNetForget drops ONE project's remembered approval. It is `coop net
// approve` in reverse and asks the same way, at a terminal — but it is not an
// `rm`: what it removes can be remembered again by approving again, and a run
// that finds no approval asks for one instead of reaching further. So it never
// claims the deletion cannot be undone.
func (a *app) cmdNetForget(args []string) (int, error) {
	path, err := parseNetForgetArgs(args)
	if err != nil {
		return 2, err
	}
	if !ui.IsTerminal(os.Stdin) || !ui.IsTerminal(os.Stderr) {
		return 1, netTerminalOnly("coop net forget")
	}
	if path == "" {
		// No --project means this project, resolved the way every scoped net
		// verb resolves it. A gone checkout has no git top level, which is why
		// --project exists at all.
		if path, err = netProject(a.cfg.RepoOverride); err != nil {
			return 1, err
		}
	}
	if path, err = filepath.Abs(path); err != nil {
		return 1, err
	}
	review, err := box.ReviewProjectNetworkForget(path)
	if err != nil {
		return 1, err
	}
	if review.Approval() == nil {
		if err := review.Close(); err != nil {
			return 1, err
		}
		ui.Note("%s", netForgetNothing)
		if review.Gone() {
			ui.Detail("a path that was a symlink was approved as the directory it pointed at — forget that path instead")
		}
		return 0, nil
	}
	err = confirmNetForget(context.Background(), review, os.Stderr, func() bool {
		return ui.Confirm(netForgetPrompt, false)
	})
	declined := errors.Is(err, errNetDeclined)
	if declined {
		err = nil
	}
	if err = errors.Join(err, review.Close()); err != nil {
		return 1, err
	}
	// Answering no is an answer, not a failure: nothing changed, and the run
	// says so without an error marker or a nonzero status.
	if declined {
		ui.Note("%s", netForgetCancelled)
		return 0, nil
	}
	ui.OK("%s", netForgetForgotten)
	return 0, nil
}

// errNetDeclined is a confirmation answered "no". It carries no message of its
// own: the caller says what was kept.
var errNetDeclined = errors.New("declined")

// The question and the answer carry what withdrawing costs — the next run waits
// for a fresh approval — so the review above them can be the approval itself.
const (
	netForgetTitle     = "Withdraw this project's network approval?"
	netForgetPrompt    = "Withdraw this approval?"
	netForgetForgotten = "Network approval withdrawn"
	netForgetCancelled = "Cancelled. Network approval was kept."
	netForgetNothing   = "No network approval is saved for this project."
	netForgetRestore   = "Restore approval with:"
	netForgetGone      = "The project folder no longer exists."
)

// netForgetConsequences is what withdrawing does, grouped the way a person
// checks it: what does not change, what stops, and what is kept.
var netForgetConsequences = []string{
	"The settings in .agent/project.yaml are unchanged.",
	"New runs will wait for you to approve network access again.",
	"Existing boxes, recorded runs, and host setup will be kept.",
}

// netForgetReview is what the confirmation needs: the project, the approval it
// would remove, and one commit that consumes it.
type netForgetReview interface {
	Project() string
	Approval() *networkstate.Approval
	Gone() bool
	Commit(context.Context) error
}

func confirmNetForget(ctx context.Context, review netForgetReview, out io.Writer, confirm func() bool) error {
	approval := review.Approval()
	if approval == nil {
		return errors.New("there is nothing to forget for this project")
	}
	var b strings.Builder
	p := ui.For(os.Stderr)
	fmt.Fprintf(&b, "%s\n", p.Bold(netForgetTitle))
	// What is being withdrawn: the approved mode, and the rules it granted when
	// it granted any. The same words every other network view uses for a mode.
	fmt.Fprintf(&b, "\n  Approved: %s\n", netModeWord(approval.Posture))
	for _, rule := range approval.Envelope {
		fmt.Fprintf(&b, "    %s\n", netRuleText(rule))
	}
	// A checkout that is gone is why this command takes a path at all: say so
	// before the consequences, since the usual "edit the file instead" does not
	// apply to a project that no longer has one.
	if review.Gone() {
		fmt.Fprintf(&b, "\n  %s\n", netForgetGone)
	}
	fmt.Fprintln(&b)
	for _, line := range netForgetConsequences {
		fmt.Fprintf(&b, "  - %s\n", line)
	}
	fmt.Fprintf(&b, "\n%s\n  %s\n\n", netForgetRestore, p.Cyan("coop net approve"))
	if _, err := io.WriteString(out, b.String()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !confirm() {
		return errNetDeclined
	}
	return review.Commit(ctx)
}
