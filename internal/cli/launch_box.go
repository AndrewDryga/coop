package cli

import (
	"os"

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
	// Coop's own base comes first, sign-in included: an upgrade names a new tag, and building Coop's
	// base is not a project build.
	if img == a.cfg.BaseImage {
		if err := a.ensureManagedBase(); err != nil {
			return err
		}
	}
	if a.loginProvider != "" {
		return nil // sign-in uses the existing image; it must not execute a project build
	}
	builtBy, skewed := box.BaseImageSkew(a.cfg, img)
	if !skewed {
		return nil
	}
	return a.updateCoopBox(updatingNarration(builtBy), func() error { return box.Build(a.rt, a.cfg, repo, false, resolveVersion()) })
}

// ensureManagedBase builds this Coop's own base when a launch needs it and it is missing on a host
// where a Coop built one before. The tag names the box definition, so an upgrade is a new tag: it
// is repaired here, as a mismatched base always was, and never by replacing another version's
// base. A host that never built one keeps the plain "run 'coop build'" — a first build is the
// operator's to start.
func (a *app) ensureManagedBase() error {
	builtBy, removed, ok := box.ManagedBaseRepair(a.rt, a.cfg)
	if !ok {
		return nil // current, a first build, an operator's base, or a stopped daemon the caller reports
	}
	narration := updatingNarration(builtBy)
	if removed {
		narration = [2]string{"The box image is missing.", "Building it now…"}
	}
	// Build output goes to stderr and the build reads nothing: an ACP child reaches this too, and
	// its stdio is the editor's protocol.
	return a.updateCoopBox(narration, func() error { return box.BuildManagedBase(a.rt, a.cfg, resolveVersion(), os.Stderr) })
}

// updatingNarration says which Coop built the image being replaced and which one it is updated for.
func updatingNarration(builtBy string) [2]string {
	if builtBy == "" {
		builtBy = "an earlier Coop"
	} else {
		builtBy = "Coop " + builtBy
	}
	return [2]string{"Current image was built by " + builtBy, "Updating it for Coop " + resolveVersion() + "…"}
}

// updateCoopBox is the one `Checking the Coop box` section a launch opens to bring Coop's own
// image up to date before the box starts.
func (a *app) updateCoopBox(narration [2]string, build func() error) error {
	ui.Section("Checking the Coop box")
	ui.Note("  %s", narration[0])
	ui.Note("  %s", narration[1])
	again := "run '" + a.launchCommand() + "' again"
	if err := a.rt.EnsureDaemon(); err != nil {
		ui.Fail("Could not update the box", "Docker is unavailable.", "Start Docker, then "+again+".")
		return ui.Reported(err)
	}
	if err := build(); err != nil {
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
