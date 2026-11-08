package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/ui"
)

// `coop build` and `coop update`: the two commands that change the image every later run uses.
// Both restart the supervised editor boxes that run an image the build replaced, in any project
// (every project without a Dockerfile of its own shares the base), and neither claims a session
// reconnected, only that it will. A build that replaced nothing restarts nothing. A loop or an
// ordinary interactive box is never touched: killing one would lose work, and it picks up the new
// image the next time it starts.

func (a *app) cmdBuild(args []string) (int, error) {
	mode, err := parseBuildMode(args)
	if err != nil {
		return 2, err
	}
	retry := "coop build"
	if mode != nil {
		retry += " --egress " + mode.Word()
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	switch {
	case errors.Is(err, box.ErrHomeProject) && mode != nil && *mode != egress.Filtered:
		// A new terminal opens in the home folder, and the installer runs this from there. The ordinary
		// shared image is built from Coop's own definition and reads nothing of the folder, so it needs
		// no project. A filtered image belongs to a project, so that build still refuses.
		repo = ""
	case errors.Is(err, box.ErrHomeProject):
		return -1, fmt.Errorf("%w. To build the shared image from here, run coop build --egress open", err)
	case err != nil:
		return -1, err
	}
	if repo != "" {
		if _, err := project.Load(repo); err != nil {
			return -1, err
		}
	}
	if err := a.ensureRuntime(); err != nil {
		return -1, err
	}
	if mode == nil {
		access, err := box.ProjectNetworkAccess(context.Background(), a.cfg, repo)
		if err != nil {
			return 1, err
		}
		mode = &access.Mode
	}
	if *mode == egress.Filtered {
		// Only the source observation is shared with the ordinary build plan. Its image and
		// base-preparation fields do not describe the locked filtered image resolved below.
		plan, err := box.PlanBuild(a.rt, a.cfg, repo, false)
		if err != nil {
			// A build refused here ran nothing, so say why to the next run as well: a run on these files
			// would only send you back to this build.
			box.RememberRefusedProjectBuild(context.Background(), a.rt, a.cfg, box.RunSpec{Repo: repo}, err)
			return 1, buildFailure("Could not build the filtered box image", err, a.rt.Name, "coop build --egress filtered")
		}
		warnUntrackedBuild(plan)
		changes, err := a.buildFilteredBox(repo, plan, "Preparing the filtered box image")
		if err != nil {
			return 1, buildFailure("Could not build the filtered box image", err, a.rt.Name, "coop build --egress filtered")
		}
		ui.OK("Filtered box image ready")
		return a.reportRecycle(repo, "Box image built", changes)
	}
	plan, err := box.PlanBuild(a.rt, a.cfg, repo, false)
	if err != nil {
		return 1, buildFailure("Could not build the box image", err, a.rt.Name, retry)
	}
	announceBuild(plan, "Building the Coop box", "Building the project box")
	changes, err := a.buildPlanned(repo, plan, false)
	if err != nil {
		ui.Note("")
		return 1, buildFailure("Could not build the box image", err, a.rt.Name, retry)
	}
	ui.Note("")
	ui.OK("Box image built")
	return a.reportRecycle(repo, "Box image built", changes)
}

func (a *app) buildFilteredBox(repo string, plan box.BuildPlan, title string) ([]box.ImageChange, error) {
	ui.Note("%s", title)
	if plan.Toolchain {
		ui.Note("  The filtered box does not build the tools in .tool-versions. To have them there, write an")
		ui.Note("  .agent/Dockerfile ('coop init --stack asdf' writes one from it) and build again.")
	}
	if plan.Project && !plan.Toolchain {
		ui.Note("  Dockerfile: %s", plan.Dockerfile)
		ui.Note("  Project builds use ordinary networking, not the run's network restrictions.")
		ui.Note("  Review the Dockerfile and copied build files before building.")
	}
	// Only the filtered builder's detached process group needs explicit cancellation;
	// foreground probes still receive terminal signals.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	change, err := box.BuildFilteredProject(ctx, a.rt, a.cfg, repo, os.Stdout, os.Stderr)
	return []box.ImageChange{change}, err
}

// updateFilteredBox is what an update does to a filtered project: rebuild what a person approved. The
// one command that approves new build files is `coop build`.
func (a *app) updateFilteredBox(repo string, plan box.BuildPlan) ([]box.ImageChange, error) {
	ui.Note("Updating the filtered box image")
	if plan.Project && !plan.Toolchain {
		ui.Note("  Dockerfile: %s", plan.Dockerfile)
		ui.Note("  Project builds use ordinary networking, so only files you approved with coop build rebuild here.")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	change, err := box.UpdateApprovedProject(ctx, a.rt, a.cfg, repo, os.Stdout, os.Stderr)
	return []box.ImageChange{change}, err
}

// buildPlanned runs an ordinary build and reports what it did to every tag it builds: the plan's
// image and, when the plan rebuilds it first, the shared base that boxes of every project without
// a Dockerfile of its own run. Comparing the image IDs before and after is how the build knows
// which running boxes it actually left on an old image.
func (a *app) buildPlanned(repo string, plan box.BuildPlan, fresh bool) ([]box.ImageChange, error) {
	tags := []string{plan.Image}
	if plan.BaseFirst {
		tags = append(tags, a.cfg.BaseImage)
	}
	changes := make([]box.ImageChange, len(tags))
	for i, tag := range tags {
		changes[i].Before = a.rt.ImageID(tag)
	}
	if err := box.BuildPlanned(a.rt, a.cfg, repo, plan, fresh, resolveVersion(), os.Stdin, os.Stdout); err != nil {
		return nil, err
	}
	for i, tag := range tags {
		changes[i].After = a.rt.ImageID(tag)
	}
	return changes, nil
}

// --egress chooses the image a later run needs; it is not a build-time network sandbox.
func parseBuildMode(args []string) (*egress.Mode, error) {
	var mode *egress.Mode
	for i := 0; i < len(args); {
		value, count, match, err := flagValue(args, i, "--egress")
		if !match {
			return nil, extraArgErr(args[i], "coop build", "coop build [--egress <mode>]", []string{"--egress"})
		}
		if err != nil {
			return nil, ui.MissingOptionValue("--egress", "coop build", "coop build --egress filtered")
		}
		if mode != nil {
			return nil, ui.RepeatedOption("--egress", "coop build")
		}
		parsed, err := egress.ParseWord(value)
		if err != nil {
			return nil, ui.InvalidOptionValue(value, "--egress", "coop build", "Choose filtered, open or offline.", "coop build --egress filtered")
		}
		mode = &parsed
		i += count
	}
	return mode, nil
}

// announceBuild says which image is about to be built and from what. The shared base has no
// Dockerfile of the project's own, so it names only its tag — one per box definition, so a reader
// can tell which Coop's base this is; a project build also names the exact configured path.
func announceBuild(plan box.BuildPlan, baseTitle, projectTitle string) {
	warnUntrackedBuild(plan)
	if !plan.Project {
		ui.Note("%s", baseTitle)
		ui.Note("  Image:      %s", plan.Image)
		ui.Note("")
		return
	}
	ui.Note("%s", projectTitle)
	if plan.Toolchain {
		ui.Note("  Tools:      .tool-versions")
	} else {
		ui.Note("  Dockerfile: %s", plan.Dockerfile)
	}
	ui.Note("  Image:      %s", plan.Image)
	ui.Note("")
	if plan.BaseFirst {
		ui.Note("The shared Coop image must be built first.")
		ui.Note("")
	}
}

func warnUntrackedBuild(plan box.BuildPlan) {
	if plan.Untracked {
		warnBlock("The box Dockerfile is not tracked in Git",
			plan.Dockerfile+" controls what runs in the box.",
			"Review this file before using the rebuilt box.")
		ui.Note("")
	}
}

// buildFailure separates the three ways a build ends badly, because the fix differs: no runtime
// to build on, a context coop could not assemble (nothing ran), and a build that ran and failed.
func buildFailure(headline string, err error, runtimeName, retry string) error {
	if refused := reportUnreadableCoopignore(err, retry, ""); refused != nil {
		return refused
	}
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

// reportRecycle restarts the supervised editor boxes running an image the build replaced and
// reports the effect on running work. The counts come from the runtime's own answer, and the
// sentence says sessions WILL reconnect: coop removed their boxes, it did not watch an editor come
// back. A build whose images all stayed the same says so and touches nothing. One that only
// created an image, or ran where image IDs cannot be read, has no old image to report.
func (a *app) reportRecycle(repo, built string, changes []box.ImageChange) (int, error) {
	var replaced []string
	unchanged := len(changes) > 0
	for _, change := range changes {
		if change.Replaced() {
			replaced = append(replaced, change.Before)
		}
		unchanged = unchanged && change.Unchanged()
	}
	if unchanged {
		ui.Note("  The image did not change, so no editor session restarts.")
		return 0, nil
	}
	if len(replaced) == 0 {
		return 0, nil
	}
	supervised, others, err := a.recycleBoxes(repo, replaced)
	if err != nil {
		// Running the build again would not help: it finds the image already current and leaves
		// a session on the old one alone. Reconnecting starts that session on the new image.
		ui.Note("")
		return 1, reported("Could not restart all supervised editor sessions", sentence(firstLine(err)),
			fmt.Sprintf("Check %s, then reconnect your editor to use the new image.", runtimeTitle(a.rt.Name)),
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

// recycleBoxes restarts the supervised boxes running one of the replaced images so they reconnect
// on the new one — a coop acp supervisor replays the ACP handshake, so the editor doesn't notice.
// A box on any other image is untouched: the build did not change it, and a restart would bring
// it back on the same image. Other running boxes on a replaced image (loops, forks, an
// un-supervised session) are only counted; SIGKILLing them would lose work, and they pick up the
// new image when they next start. New runs use the fresh image anyway (containers are anonymous).
// It reaps repo's orphans first, so a box nobody supervises is never counted (or reported to the
// user) as work still running on the old image. Every authoritative query and removal shares one
// deadline; the preceding image build has already succeeded if an error is returned.
const recycleBoxesTimeout = 10 * time.Second

func (a *app) recycleBoxes(repo string, replaced []string) (supervised, others int, err error) {
	a.sweepOrphanBoxes(repo)
	ctx, cancel := context.WithTimeout(context.Background(), recycleBoxesTimeout)
	defer cancel()
	// Each failure keeps the runtime's own diagnostic after the sentence that says WHICH step
	// failed: the step is what the reader acts on, the diagnostic is what they paste into a
	// search when the step alone is not enough.
	runtime := runtimeTitle(a.rt.Name)
	running, err := a.rt.RunningContainerImages(ctx, box.LabelKey, box.LabelBox, box.LabelSupervised)
	if err != nil {
		return 0, 0, fmt.Errorf("%s did not respond while listing running boxes: %w", runtime, err)
	}
	removed := 0
	for _, container := range running {
		if !slices.Contains(replaced, container.Image) {
			continue
		}
		if container.Labels[box.LabelSupervised] != box.LabelOn {
			others++
			continue
		}
		if err := a.rt.RemoveContainerContext(ctx, container.ID); err != nil {
			return 0, 0, fmt.Errorf("%s could not remove an old supervised box, %d removed before failure: %w", runtime, removed, err)
		}
		removed++
	}
	return removed, others, nil
}

// cmdUpdate installs the latest binary and prepares the effective project image. Filtered
// builds retain the qualified pinned base; ordinary builds refresh their upstream base.
// --self-only does just the binary; --box-only does just image preparation.
//
// A successful binary replacement hands off to that executable before image preparation:
// configuration and embedded client/base definitions must come from the installed version.
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
			if result.Outcome == selfUpdateInstalled && !selfOnly {
				if err := updateExec(result.Executable, []string{result.Executable, "update", "--box-only"}, os.Environ()); err != nil {
					return 1, reported("Coop updated, but the box image was not prepared", sentence(firstLine(err)),
						fmt.Sprintf("Coop v%s is installed. Run coop update --box-only to finish.", result.Latest))
				}
				return 0, nil // exec replaced this process; reachable only through the test seam
			}
		case selfOnly:
			return 1, selfUpdateFailure(err, "coop update --self-only")
		case errors.As(err, &soft):
			reason, fix := releaseCheckReason(err)
			actions := []string{"Continuing with the box update."}
			if fix != "" {
				actions = append([]string{fix}, actions...)
			}
			warnBlock("Could not check for a newer Coop release", reason, actions...)
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
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	home := errors.Is(err, box.ErrHomeProject)
	switch {
	case home:
		// A new terminal opens in the home folder, and the installer's own update step runs from there.
		// The shared image reads nothing of the folder, so it needs no project (as in cmdBuild).
		repo = ""
	case err != nil:
		return -1, err
	default:
		if _, err := project.Load(repo); err != nil {
			return -1, err
		}
	}
	if err := a.ensureRuntime(); err != nil {
		return 1, updateBoxFailure(err, a.rt.Name, selfFailed)
	}
	filtered := false
	if !home {
		access, err := box.ProjectNetworkAccess(context.Background(), a.cfg, repo)
		if err != nil {
			return 1, updateBoxFailure(err, a.rt.Name, selfFailed)
		}
		filtered = access.Mode == egress.Filtered
	}
	plan, err := box.PlanBuild(a.rt, a.cfg, repo, !filtered)
	if err != nil {
		return 1, updateBoxFailure(err, a.rt.Name, selfFailed)
	}
	var changes []box.ImageChange
	if filtered {
		var waiting *box.ProjectBuildRequired
		changes, err = a.updateFilteredBox(repo, plan)
		if errors.As(err, &waiting) {
			// Nothing went wrong, and only the person can settle this. An update is not a review.
			ui.Note("")
			warnBlock("The box image was not updated", "You have not approved this project's build files as they are now. Review the Dockerfile and the files it copies, then run coop build --egress filtered.")
			if selfFailed {
				return 1, nil
			}
			return 0, nil
		}
	} else {
		ui.Note("Updating the Coop box")
		ui.Note("  Fetching newer base-image components. Agent clients stay at the versions tested with this Coop release.")
		ui.Note("  Image:      %s", plan.Image)
		ui.Note("")
		changes, err = a.buildPlanned(repo, plan, true)
	}
	if err != nil {
		ui.Note("")
		return 1, updateBoxFailure(err, a.rt.Name, selfFailed)
	}
	ui.Note("")
	if filtered {
		ui.OK("Filtered box image updated")
	} else {
		ui.OK("Box image updated")
	}
	if code, err := a.reportRecycle(repo, "Box image updated", changes); err != nil {
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
		return releaseCheckFailure(err, retry)
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
	if refused := reportUnreadableCoopignore(err, "coop update --box-only", ""); refused != nil {
		return refused
	}
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
			return false, false, false, extraArgErr(x, "coop update", "coop update [--self-only | --box-only | --check]", updateOptions)
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
		return 1, releaseCheckFailure(err, "coop update --check")
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
	access, err := box.ProjectNetworkAccess(context.Background(), a.cfg, repo)
	if err != nil {
		return 1, err
	}
	if access.Mode == egress.Filtered {
		ui.Note("This project uses filtered networking.")
		ui.Note("Coop checks the filtered image when you start a box. --check does not inspect Docker.")
		ui.Note("Use coop update --box-only to prepare this project's filtered image.")
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
