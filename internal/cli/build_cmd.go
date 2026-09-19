package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// `coop build` and `coop update`: the two commands that change the image every later run uses.
// Both recycle the supervised editor boxes the global supervisor label selects — which is every
// supervised box on this host, not just this project's — and neither claims a session reconnected,
// only that it will. A loop or an ordinary interactive box is never touched: killing one would
// lose work, and it picks up the new image the next time it starts.

func (a *app) cmdBuild(args []string) (int, error) {
	if err := rejectArgs("build", args); err != nil {
		return 2, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	plan, err := box.PlanBuild(a.rt, a.cfg, repo, false)
	if err != nil {
		return 1, buildFailure("Could not build the box image", err, a.rt.Name, "coop build")
	}
	announceBuild(plan, "Building the Coop box", "Building the project box")
	if err := box.BuildPlanned(a.rt, a.cfg, repo, plan, false, resolveVersion(), os.Stdin, os.Stdout); err != nil {
		ui.Note("")
		return 1, buildFailure("Could not build the box image", err, a.rt.Name, "coop build")
	}
	ui.Note("")
	ui.OK("Box image built")
	return a.reportRecycle(repo, "Box image built", "coop build")
}

// announceBuild says which image is about to be built and from what. The shared base has no
// Dockerfile of the project's own, so it names only its tag — one per box definition, so a reader
// can tell which Coop's base this is; a project build also names the exact configured path.
func announceBuild(plan box.BuildPlan, baseTitle, projectTitle string) {
	if plan.Untracked {
		warnBlock("The box Dockerfile is not tracked in Git",
			plan.Dockerfile+" controls what runs in the box.",
			"Review this file before using the rebuilt box.")
		ui.Note("")
	}
	if !plan.Project {
		ui.Note("%s", baseTitle)
		ui.Note("  Image:      %s", plan.Image)
		ui.Note("")
		return
	}
	ui.Note("%s", projectTitle)
	ui.Note("  Dockerfile: %s", plan.Dockerfile)
	ui.Note("  Image:      %s", plan.Image)
	ui.Note("")
	if plan.BaseFirst {
		ui.Note("The shared Coop image must be built first.")
		ui.Note("")
	}
}

// buildFailure separates the three ways a build ends badly, because the fix differs: no runtime
// to build on, a context coop could not assemble (nothing ran), and a build that ran and failed.
func buildFailure(headline string, err error, runtimeName, retry string) error {
	var stage *box.StageError
	switch {
	case errors.As(err, &stage):
		return reported("Could not prepare the build files", pathReason("", "read", stage.Err),
			"Fix the path or permissions, then run "+retry+" again.")
	case runtimeUnavailable(err):
		return reported(headline, fmt.Sprintf("%s is unavailable.", runtimeTitle(runtimeName)),
			fmt.Sprintf("Start %s, then run %s again.", runtimeTitle(runtimeName), retry))
	default:
		return reported(headline, sentence(firstLine(err)),
			"Fix the build error, then run "+retry+" again.")
	}
}

// reportRecycle restarts the supervised editor boxes onto the new image and reports the effect on
// running work. The counts come from the runtime's own answer, and the sentence says sessions
// WILL reconnect: coop removed their boxes, it did not watch an editor come back.
func (a *app) reportRecycle(repo, built, retry string) (int, error) {
	supervised, others, err := a.recycleBoxes(repo)
	if err != nil {
		ui.Note("")
		return 1, reported("Could not restart all supervised editor sessions", sentence(firstLine(err)),
			fmt.Sprintf("Check %s, then run %s again.", runtimeTitle(a.rt.Name), retry),
			"New runs can use the "+strings.ToLower(strings.TrimPrefix(built, "Box image "))+" image.")
	}
	if supervised > 0 {
		ui.Note("  %s will reconnect with the new image.", ui.Count(supervised, "supervised editor session"))
	}
	if others > 0 {
		ui.Note("  %s will use the new image on its next start.", ui.Count(others, "other running box", "other running boxes"))
	}
	return 0, nil
}

// recycleBoxes restarts supervised boxes after a rebuild so they reconnect on the new
// image — a coop acp supervisor replays the ACP handshake, so the editor doesn't
// notice. New runs use the fresh image anyway (containers are anonymous). Other
// running boxes (loops, forks, an un-supervised session) are left alone; SIGKILLing
// them would lose work, and they pick up the new image when they next start.
// It reaps repo's orphans first, so a box nobody supervises is never counted (or reported to the
// user) as work still running on the old image. Every authoritative query and removal shares one
// deadline; the preceding image build has already succeeded if an error is returned.
const recycleBoxesTimeout = 10 * time.Second

func (a *app) recycleBoxes(repo string) (supervised, others int, err error) {
	a.sweepOrphanBoxes(repo)
	ctx, cancel := context.WithTimeout(context.Background(), recycleBoxesTimeout)
	defer cancel()
	// Each failure keeps the runtime's own diagnostic after the sentence that says WHICH step
	// failed: the step is what the reader acts on, the diagnostic is what they paste into a
	// search when the step alone is not enough.
	runtime := runtimeTitle(a.rt.Name)
	running, err := a.rt.RunningContainerIDsByLabel(ctx, box.LabelKey, box.LabelBox)
	if err != nil {
		return 0, 0, fmt.Errorf("%s did not respond while listing running boxes: %w", runtime, err)
	}
	live, err := a.rt.RunningContainerIDsByLabel(ctx, box.LabelSupervised, box.LabelOn)
	if err != nil {
		return 0, 0, fmt.Errorf("%s did not respond while listing supervised boxes: %w", runtime, err)
	}
	n, err := a.rt.RemoveByLabel(ctx, box.LabelSupervised, box.LabelOn)
	if err != nil {
		return 0, 0, fmt.Errorf("%s could not remove an old supervised box, %d removed before failure: %w", runtime, n, err)
	}
	return n, len(running) - len(live), nil
}

// cmdUpdate self-updates the coop binary to the latest release, then force-rebuilds
// the box image (--pull --no-cache) on the newest OS packages and Node — the agent
// clients stay the set this Coop qualifies — then reports which clients it carries.
// --self-only does just the binary; --box-only does just the image (the old behavior).
//
// The binary is replaced BEFORE the rebuild, and the rebuild runs in THIS, pre-update process —
// so the image is built by the old binary's box definition. Nothing here may say otherwise.
func (a *app) cmdUpdate(args []string) (int, error) {
	selfOnly, boxOnly, check, err := parseUpdateFlags(args)
	if err != nil {
		return 2, err
	}
	if check {
		return a.cmdUpdateCheck()
	}

	// Self-update the binary first. A failed *check* (offline/rate limit) is soft and
	// must not block the box rebuild; a write or install failure is loud and exits
	// non-zero, but the box still rebuilds (it's independent) so the run isn't wasted.
	selfFailed := false
	if !boxOnly {
		result, err := selfUpdate()
		var soft checkError
		switch {
		case err == nil:
			reportSelfUpdate(result)
		case selfOnly:
			return 1, selfUpdateFailure(err, "coop update --self-only")
		case errors.As(err, &soft):
			warnBlock("Could not check for a newer Coop release", sentence(firstLine(err)),
				"Continuing with the box update.")
			ui.Note("")
		default:
			failBlock("Could not update the Coop binary", sentence(firstLine(err)),
				"Update Coop with the tool that installed it.",
				"Continuing with the box update.")
			ui.Note("")
			selfFailed = true
		}
		if selfOnly {
			return 0, nil
		}
	}

	// The box rebuild needs the runtime; --self-only returned above, so detect only here (not eagerly
	// in dispatch), keeping `coop update --self-only` usable on a box with no container runtime.
	repo, _, err := loadProject(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if err := a.ensureRuntime(); err != nil {
		return 1, updateBoxFailure(err, a.rt.Name, selfFailed)
	}
	plan, err := box.PlanBuild(a.rt, a.cfg, repo, true)
	if err != nil {
		return 1, updateBoxFailure(err, a.rt.Name, selfFailed)
	}
	ui.Note("Updating the Coop box")
	ui.Note("  Fetching newer base-image components. Agent clients stay at the versions this Coop qualifies.")
	ui.Note("  Image:      %s", plan.Image)
	ui.Note("")
	if err := box.BuildPlanned(a.rt, a.cfg, repo, plan, true, resolveVersion(), os.Stdin, os.Stdout); err != nil {
		ui.Note("")
		return 1, updateBoxFailure(err, a.rt.Name, selfFailed)
	}
	ui.Note("")
	ui.OK("Box image updated")
	if code, err := a.reportRecycle(repo, "Box image updated", "coop update --box-only"); err != nil {
		return code, err
	}
	ui.Note("")
	reportInstalledTools(plan)
	if selfFailed {
		ui.Note("")
		warnBlock("The box was updated, but the Coop binary was not", "")
		return 1, nil // box updated, binary didn't — signal the partial failure
	}
	return 0, nil
}

// displayVersion is how a version is written for a person: a git-describe build is a version and
// gets its v, while a bare marker like "dev" is a word and must not be dressed up as one.
func displayVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return v
	}
	if v[0] >= '0' && v[0] <= '9' {
		return "v" + v
	}
	return v
}

// reportSelfUpdate says what happened to the binary, in the versions the user can check.
func reportSelfUpdate(result selfUpdateResult) {
	switch result.Outcome {
	case selfUpdateDev:
		ui.Note("Keeping this development build: %s.", displayVersion(result.Current))
	case selfUpdateCurrent:
		ui.Note("Coop v%s is already up to date.", result.Current)
	case selfUpdateAhead:
		ui.Note("Keeping Coop v%s; the latest release is v%s.", result.Current, result.Latest)
	case selfUpdateInstalled:
		ui.OK("Coop updated to v%s", result.Latest)
	}
	ui.Note("")
}

// selfUpdateFailure renders a --self-only failure. "The installed binary was not changed" is
// printed only when the failure provably happened before the atomic replace.
func selfUpdateFailure(err error, retry string) error {
	var soft checkError
	if errors.As(err, &soft) {
		return reported("Could not check for a Coop update", sentence(firstLine(err)),
			"Try "+retry+" again later.")
	}
	actions := []string{"Update Coop with the tool that installed it."}
	var hard *installFailure
	if errors.As(err, &hard) && hard.unchanged && !strings.Contains(hard.reason, "could not be replaced") {
		actions = []string{"Run " + retry + " again to download a fresh copy.", "The installed binary was not changed."}
	}
	return reported("Could not update the Coop binary", sentence(firstLine(err)), actions...)
}

// updateBoxFailure keeps a successful binary installation visible: a failed rebuild does not undo
// the new binary, and saying nothing about it would read as a whole update rolled back.
func updateBoxFailure(err error, runtimeName string, selfFailed bool) error {
	actions := []string{"Fix the build error, then run coop update --box-only."}
	reason := sentence(firstLine(err))
	var stage *box.StageError
	switch {
	case errors.As(err, &stage):
		actions = []string{"Fix the path or permissions, then run coop update --box-only."}
	case runtimeUnavailable(err):
		reason = fmt.Sprintf("%s is unavailable.", runtimeTitle(runtimeName))
		actions = []string{fmt.Sprintf("Start %s, then run coop update --box-only.", runtimeTitle(runtimeName))}
	}
	if !selfFailed && releaseVersion(resolveVersion()) {
		actions = append(actions, fmt.Sprintf("Coop v%s is already installed.", normalizeVersion(resolveVersion())))
	}
	return reported("Could not update the box image", reason, actions...)
}

// reportInstalledTools names the agent clients the rebuilt image carries: Coop's qualified set,
// which the build installed from the lock this binary embeds (it fails rather than install
// anything else) — unless the project's Dockerfile builds on another base and brings its own.
func reportInstalledTools(plan box.BuildPlan) {
	ui.Note("Agent clients")
	if !plan.CarriesQualifiedClients() {
		ui.Note("  This project's %s does not build on Coop's box, so it brings its own.", plan.Dockerfile)
		return
	}
	for _, client := range agents.QualifiedClients() {
		ui.Note("  %s", client)
	}
}

// updateOptions are `coop update`'s mutually exclusive modes — the whole grammar, and the list its
// refusals read: one of them, or none (update both).
var updateOptions = []string{"--self-only", "--box-only", "--check"}

// parseUpdateFlags parses `coop update`'s own flags: --self-only (just the binary),
// --box-only (just the image), and --check (report, change nothing) — mutually exclusive.
func parseUpdateFlags(args []string) (selfOnly, boxOnly, check bool, err error) {
	var picked []string
	for _, x := range args {
		switch x {
		case "--self-only":
			selfOnly = true
		case "--box-only":
			boxOnly = true
		case "--check":
			check = true
		default:
			return false, false, false, unknownOptionErr(x, "coop update", updateOptions)
		}
		if !slices.Contains(picked, x) {
			picked = append(picked, x)
		}
	}
	// Name only the two modes actually supplied — the rest of the exclusion group is not the
	// user's problem.
	if len(picked) > 1 {
		return false, false, false, ui.ConflictingOptions(picked[0], picked[1], "coop update")
	}
	return selfOnly, boxOnly, check, nil
}

// cmdUpdateCheck is `coop update --check`: report what an update WOULD do, changing
// nothing. The binary line needs one GitHub call; the box report reads only the local
// build stamps (no container runtime), so the dry-run works anywhere.
func (a *app) cmdUpdateCheck() (int, error) {
	cur := resolveVersion()
	latest, err := latestReleaseTag()
	if err != nil {
		return 1, reported("Could not check for a Coop update", sentence(firstLine(err)),
			"Try coop update --check again later.")
	}
	c, l := normalizeVersion(cur), normalizeVersion(latest)
	updateAvailable := false
	switch relation := compareReleaseVersions(cur, latest); relation {
	case releaseInvalid:
		if !releaseVersion(latest) {
			return 1, reported("Could not check for a Coop update", "GitHub did not return a valid release version.",
				"Try coop update --check again later.")
		}
		ui.Note("This is a development build: %s.", displayVersion(cur))
		ui.Note("The latest release is v%s. Self-update does not replace development builds.", l)
	case releaseBehind:
		updateAvailable = true
		ui.Note("Coop v%s is available; you have v%s.", l, c)
	case releaseEqual:
		ui.Note("Coop v%s is up to date.", c)
	case releaseAhead:
		ui.Note("Coop v%s is newer than the latest release, v%s.", c, l)
	default:
		ui.Note("This is a development build: %s.", displayVersion(cur))
		ui.Note("The latest release is v%s. Self-update does not replace development builds.", l)
	}

	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil || !pathExists(repo) {
		// The stamps live per project, so with no project folder there is no record to read —
		// which is not the same as a box that has never been built.
		ui.Note("The box image was not checked because no project folder could be resolved.")
		return 0, nil
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	at, built := box.ImageBuildAge(a.cfg, img)
	if !built {
		// No build stamp means this coop never built the image here; "current" would describe an
		// image `coop run` is about to refuse. The runtime is deliberately not consulted so the
		// dry-run keeps working without one.
		ui.Note("")
		ui.Note("No local build record was found for %s.", img)
		ui.Note("Build the box with coop build.")
		return 0, nil
	}
	when := "today"
	if days := int(time.Since(at).Hours() / 24); days > 0 {
		when = ui.Count(days, "day") + " ago"
	}
	ui.Note("Box image %s was last built %s.", img, when)

	// The three freshness concerns are independent and each has its own fix; an image can be
	// stale, mismatched and old at once, and collapsing them would drop a reason. What they
	// SHARE is the next command, so that is said once: inside the lone notice when there is one,
	// and as the closing action when several notices point at the same thing.
	stale := box.StaleImageInputs(a.cfg, repo, img)
	builtBy, skewed := box.BaseImageSkew(a.cfg, img)
	rebuildOnly := !updateAvailable && !(stale && skewed)
	rebuildAction := []string{"Rebuild with coop build."}
	if !rebuildOnly {
		rebuildAction = nil
	}
	if stale {
		ui.Note("")
		warnBlock("The box's build inputs have changed",
			"The box Dockerfile or .tool-versions differs from the last build.", rebuildAction...)
	}
	if skewed {
		ui.Note("")
		warnBlock("The box was built for a different Coop version",
			fmt.Sprintf("It was built by Coop v%s and uses a different box definition.", normalizeVersion(builtBy)),
			rebuildAction...)
	}
	switch {
	case updateAvailable && (stale || skewed):
		ui.Actions("Update Coop and rebuild the box", "coop update")
	case updateAvailable:
		ui.Actions("Update Coop and the box", "coop update")
	case stale && skewed:
		ui.Actions("Rebuild the box", "coop build")
	case !stale && !skewed && time.Since(at) >= box.ImageAgeNudge:
		ui.Actions("Refresh the box's agent tools", "coop update --box-only")
	}
	return 0, nil
}
