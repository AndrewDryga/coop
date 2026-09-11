package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/ui"
)

func parseNetForgetArgs(args []string) (string, error) {
	project := ""
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		if name != "--project" {
			if !strings.HasPrefix(args[i], "-") {
				return "", errors.New("coop net forget takes the path as '--project <path>', or no path at all for this project")
			}
			return "", unknownOptionErr(args[i], "coop net forget", []string{"--project"})
		}
		if project != "" {
			return "", errors.New("coop net forget takes --project once")
		}
		if !inline {
			i++
			if i == len(args) {
				return "", errors.New("coop net forget --project needs the path of the project to forget")
			}
			value = args[i]
		}
		if value == "" {
			return "", errors.New("coop net forget --project needs the path of the project to forget")
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
		return 1, errors.New("coop net forget needs a terminal to ask you — an unattended run cannot drop an approval a human made")
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
		ui.Note("nothing is approved for %s", review.Project())
		if review.Gone() {
			ui.Detail("a path that was a symlink was approved as the directory it pointed at — forget that path instead")
		}
		return 0, nil
	}
	err = confirmNetForget(context.Background(), review, os.Stderr, func() bool {
		return ui.Confirm(netForgetPrompt, false)
	})
	if err = errors.Join(err, review.Close()); err != nil {
		return 1, err
	}
	ui.OK("%s", netForgetForgotten)
	return 0, nil
}

// The question and the answer carry what forgetting costs — the next run asks
// again — so the review above them can be the approval and nothing else.
const (
	netForgetPrompt    = "Forget this approval?"
	netForgetForgotten = "forgotten — a new run in this project asks for approval again; boxes already running keep their current rules"
)

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
	block := newNetBlock(&b, p, "Forget the network approval for "+review.Project())
	// The same words the approval review and bare `coop net` use for a posture, so forgetting
	// reads as the reverse of approving rather than as a different vocabulary.
	block.field("Access", netModeWord(approval.Posture)+" — approved until now")
	if len(approval.Envelope) == 0 {
		block.field("Rules", "none — this approval granted no websites or services")
	} else {
		block.field("Rules", ui.Count(len(approval.Envelope), "rule")+", all of them")
		for _, rule := range approval.Envelope {
			block.row(p.Red("- " + box.NetworkRuleText(rule)))
		}
	}
	if review.Gone() {
		block.field("Project", "this directory is gone — only its approval is left to remove")
	}
	// The blast radius, said before the question: one approval, and nothing that
	// records what already happened.
	block.field("Keeps", "the runs recorded for this project, their receipts, and this host's setup")
	block.field("Then", "the next run of this project asks for approval again before it starts")
	block.flush(&b)
	if _, err := io.WriteString(out, b.String()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !confirm() {
		return errors.New("cancelled — nothing was removed")
	}
	return review.Commit(ctx)
}
