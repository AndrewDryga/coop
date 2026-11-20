package forkctl

import (
	"os"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ui"
)

// goneListLimit is how many gone projects the confirmation names before it counts the rest.
const goneListLimit = 20

// goneStdinIsTerminal is the check that keeps `coop fork rm --gone` a person's command; tests
// replace it.
var goneStdinIsTerminal = func() bool { return ui.IsTerminal(os.Stdin) }

// errGoneNeedsTerminal keeps `coop fork rm --gone` a person's tool even with --yes: it reaches every
// project's fork records with no undo, and an agent's shell has no terminal.
var errGoneNeedsTerminal = &ui.UsageError{
	Headline: "coop fork rm --gone needs a person at a terminal",
	Cause:    "It removes fork records of every project on this machine, with no undo, so --yes does not replace a person.",
}

// removeGoneRecords is `coop fork rm --gone`: it names the fork records of projects that no longer
// exist, and the records folders that hold nothing, then removes them once a person confirms. It
// runs from anywhere, since those projects are gone. The records have no undo, and a project only
// missing for now (a disk not connected, a folder moved away) would lose its forks' history, so it
// is a person's command at a terminal that shows every path first, never an automatic sweep.
func (c *Control) removeGoneRecords(yes bool) (int, error) {
	if !goneStdinIsTerminal() {
		return 2, errGoneNeedsTerminal
	}
	now := time.Now()
	found, err := forkspace.GoneForkRecords(now)
	if err != nil {
		return 1, err
	}
	kept := func() {
		if found.Kept > 0 {
			ui.Note("Keeping %s: its repository exists, Coop cannot be sure it is gone, or no record names it.",
				ui.Count(found.Kept, "other record folder"))
		}
	}
	if len(found.Projects) == 0 && len(found.Empty) == 0 {
		ui.OK("No fork records to remove")
		kept()
		return 0, nil
	}
	empty := ui.Count(len(found.Empty), "empty record folder")
	if len(found.Projects) > 0 {
		ui.Note("Remove the fork records of these repositories, which no longer exist?")
		for i, project := range found.Projects {
			if i == goneListLimit {
				ui.Note("  and %d more", len(found.Projects)-goneListLimit)
				break
			}
			line := "  - " + ui.SafeInline(project.Path)
			if len(project.Forks) > 0 {
				line += " (forks " + ui.SafeInline(strings.Join(project.Forks, ", ")) + ")"
			}
			ui.Note("%s", line)
		}
		ui.Note("If you moved one of them, its forks lose these records.")
		if len(found.Empty) > 0 {
			ui.Note("Also remove %s?", empty)
		}
	} else {
		ui.Note("Remove %s?", empty)
	}
	kept()
	ui.Note("")
	if err := c.destroyGate("Remove them", yes); err != nil {
		return ui.DeclineExit(err), ForkCancelled(err, "coop fork rm", "Nothing was removed.")
	}
	if !yes {
		ui.Note("") // an answered question is a section of its own; --yes asked nothing
	}
	removed, err := forkspace.RemoveGoneForkRecords(found, now)
	if err != nil {
		return 1, err
	}
	ui.OK("Removed %s", ui.Count(removed, "record folder"))
	return 0, nil
}
