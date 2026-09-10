package cli

import (
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// checkCoopBox is an interactive launch's first section, and it appears only when there is
// something to do: a Coop-managed base image whose recorded box definition differs from this
// binary's is deterministic stale machinery, so it is rebuilt through the ordinary build path
// before the box starts instead of nagging "run 'coop build'" on every launch. A current image
// prints nothing. An age nudge is a suggestion, never a mismatch, so it forces no unpinned
// update; an image without Coop's own stamp — an operator's, or a project's — is never replaced.
//
// A failure ends the section with the nested red result and comes back marked reported, so the
// dispatcher prints nothing more. The remedy repeats the original command: the launch retries
// this repair itself, so a manual 'coop build' would be a redundant detour.
func (a *app) checkCoopBox(repo, img string) error {
	builtBy, skewed := box.BaseImageSkew(a.cfg, img)
	if !skewed {
		return nil
	}
	ui.Section("Checking the Coop box")
	if builtBy == "" {
		builtBy = "an earlier Coop"
	} else {
		builtBy = "Coop " + builtBy
	}
	ui.Note("  Current image was built by %s", builtBy)
	ui.Note("  Updating it for Coop %s…", resolveVersion())
	again := "run '" + a.launchCommand() + "' again"
	if err := a.rt.EnsureDaemon(); err != nil {
		ui.Fail("Could not update the box", "Docker is unavailable.", "Start Docker, then "+again+".")
		return ui.Reported(err)
	}
	if err := box.Build(a.rt, a.cfg, repo, false, resolveVersion()); err != nil {
		ui.Fail("Could not update the box", err.Error(), "Fix what the build reported above, then "+again+" — the update is retried automatically.")
		return ui.Reported(err)
	}
	ui.Pass("Box updated")
	return nil
}

// launchCommand is the command the person typed, by its head word — `coop codex`, `coop run` —
// for a remedy that tells them what to repeat.
func (a *app) launchCommand() string {
	if len(a.argv) == 0 {
		return "coop"
	}
	return "coop " + a.argv[0]
}
