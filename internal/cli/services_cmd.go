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
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/ui"
)

// `coop up` / `coop down`: this project's Compose services, the one pair of commands that runs a
// file an in-box agent may have written on the HOST daemon. Every refusal below therefore names
// the file and the exact violation rather than retrying around it.

func (a *app) cmdUp(args []string) (int, error) {
	if err := rejectArgs("up", args); err != nil {
		return 2, err
	}
	repo, p, err := loadProject(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if err := a.rt.EnsureDaemon(); err != nil {
		return 1, reported("Could not start services",
			fmt.Sprintf("%s is unavailable.", runtimeTitle(a.rt.Name)),
			fmt.Sprintf("Start %s, then run coop up again.", runtimeTitle(a.rt.Name)))
	}
	if !a.rt.SupportsCompose() {
		return 1, reported("Could not start services",
			fmt.Sprintf("%s does not support Compose.", runtimeTitle(a.rt.Name)),
			"Use Docker for this project's services.")
	}
	file := box.ComposeFileAt(repo, p.ComposeRel())
	if file == "" {
		return 1, reported("No services are configured",
			fmt.Sprintf("%s does not exist.", p.ComposeRel()),
			"Add services with coop init --services.")
	}
	// A running box now holds the shared side of the mount-launch barrier. Name the wait before
	// taking the exclusive side so an interactive command never looks hung.
	live, err := box.LiveBoxes(repo, "")
	if err != nil {
		return -1, err
	}
	if len(live) > 0 {
		ui.Note("Waiting for a safe service-launch window (%s).", box.DescribeLiveBoxes(live))
	}
	rel, _ := filepath.Rel(repo, file)
	// A service that legitimately needs a secret-looking file (a generated dev TLS key) gets it
	// only after a human at a terminal approves this exact compose content; a script or an agent
	// cannot answer for them, and the auto-start on box launch never asks — it warns and uses
	// decoys until someone runs this command and says yes.
	review, err := box.ReviewServiceSecrets(repo, file)
	if err != nil {
		return 1, composeRefusal("Could not start services from "+rel, rel, err, "coop up")
	}
	if code, err := askServiceSecrets(review, rel); err != nil {
		return code, err
	}
	// Resolve the private snapshot area before saying a start is under way: a TMPDIR an agent can
	// see is a refusal, and a refusal printed under "Starting services…" claims work that never began.
	if err := box.CheckServiceTempDir(repo, box.ConfigExposureRoots(a.cfg)...); err != nil {
		return 1, composeRefusal("Could not start services from "+rel, rel, err, "coop up")
	}
	unlockLaunch, err := forkspace.LockServiceLaunch(context.Background(), repo, true)
	if err != nil {
		return 1, reported("Could not start services", sentence(firstLine(err)), "Run coop up again.")
	}
	defer unlockLaunch()
	ui.Note("Starting services from %s", rel)
	ui.Note("  Waiting for services to be ready.")
	ui.Note("")
	out, errOut := &seenWriter{w: os.Stdout}, &seenWriter{w: os.Stderr}
	started, err := box.UpServices(a.rt, repo, file, out, errOut, box.ConfigExposureRoots(a.cfg)...)
	if out.seen || errOut.seen {
		ui.Note("")
	}
	var refused *box.ComposeRefused
	switch {
	case err == nil:
	case errors.Is(err, box.ErrNoComposeServices):
		return 1, reported("Could not start services",
			fmt.Sprintf("%s defines no services.", rel),
			"Add services with coop init --services.")
	case errors.As(err, &refused):
		return 1, composeRefusal("Could not start services from "+rel, rel, refused.Err, "coop up")
	default:
		return 1, reported("Could not start services", sentence(firstLine(err)),
			"Check the service output, fix the cause, then run coop up again.",
			"Some services may already be running.")
	}
	ui.OK("Services ready: %s", strings.Join(started.Names, ", "))
	// Only an actually published port earns a URL: a db or cache the box reaches by name has no
	// address a browser could use, and inventing one would send the reader nowhere.
	for _, port := range started.Ports {
		ui.Note("    %s://localhost:%d", port.Scheme, port.HostPort)
	}
	return 0, nil
}

// composeRefusal renders the refusals that happen before anything runs: coop will not run this
// Compose file (the violation names itself), or TMPDIR resolves somewhere an agent can see. Both
// name the file that did not run and the command to repeat once it is fixed.
func composeRefusal(headline, rel string, err error, retry string) error {
	if errors.Is(err, box.ErrExposedTempDir) {
		return reported(headline, "The temporary directory is inside a folder exposed to an agent.",
			"Set TMPDIR to a directory outside those folders, then run "+retry+" again.")
	}
	return reported(headline, sentence(firstLine(err)),
		fmt.Sprintf("Fix %s, then run %s again.", rel, retry))
}

// askServiceSecrets puts the secret-looking files a Compose file binds in front of a human and
// records their answer. Declining is not cancellation: the services start with empty files, which
// is what they would have got had nobody asked. Without a terminal nobody can answer, so it says
// which files stay empty and how to approve them, then starts anyway — the same decision the box
// auto-start makes. A non-nil error means the caller must stop.
func askServiceSecrets(review *box.ServiceSecretReview, rel string) (int, error) {
	if review == nil {
		return 0, nil
	}
	ask, atTerminal := askTerminal()
	if !atTerminal {
		emptyFilesNotice(review.Paths(), "  To approve access, run coop up at a terminal.")
		return 0, nil
	}
	ui.Note("Services in %s ask to read %s.", rel, ui.Count(len(review.Files), "secret file"))
	for _, f := range review.Files {
		ui.Note("")
		ui.Note("  %s", f.Path)
		if f.Requested {
			ui.Note("    Requested in %s.", project.File)
		} else {
			ui.Note("    Not requested in %s.", project.File)
		}
		// Only when it IS new: "not new" is the ordinary case and saying so every time buries the
		// one file the reader has not seen before.
		if f.New {
			ui.Note("    Added since your last approval.")
		}
	}
	ui.Note("")
	ui.Note("The services receive empty files unless you approve access.")
	ui.Note("Approval applies to this Compose file and expires when its contents change.")
	ui.Note("")
	answer, _ := askOneOK(ask, "Allow services to read these files? [y/N]: ")
	ui.Note("")
	if !ui.ConfirmationResponse(answer, false) {
		emptyFilesNotice(review.Paths(), "")
		return 0, nil
	}
	if err := review.Approve(); err != nil {
		return 1, reported("Could not save secret-file approval",
			sentence(osCause(err)+" while writing the host approval record"),
			"Fix the permissions, then run coop up again.",
			"Services were not started.")
	}
	ui.OK("Secret-file access approved")
	ui.Note("")
	return 0, nil
}

// emptyFilesNotice names the files the services will not be able to read, and — when nobody could
// be asked — how to approve them.
func emptyFilesNotice(paths []string, action string) {
	ui.Warn("Services will receive empty files")
	for _, p := range paths {
		ui.Note("  %s", p)
	}
	if action != "" {
		ui.Note("")
		ui.Note("%s", action)
	}
	ui.Note("")
}

// seenWriter records whether anything reached the runtime's own output, so the result below it is
// separated by exactly one blank line whether or not Compose had something to say.
type seenWriter struct {
	w    io.Writer
	seen bool
}

func (s *seenWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		s.seen = true
	}
	return s.w.Write(p)
}

// downOptions is `coop down`'s whole grammar, and the list a correction is drawn from. The
// retired -v/--volumes spelling is not in it: it is gone, not hidden.
var downOptions = []string{"--delete-volumes", "--yes"}

func (a *app) cmdDown(args []string) (int, error) {
	// Validate flags before any runtime/compose work, so a typo fails clearly here instead of
	// later as an unrelated "no .agent/compose.yml".
	deleteVolumes, yes := false, false
	for _, x := range args {
		switch x {
		case "--delete-volumes":
			deleteVolumes = true
		case "-y", "--yes":
			yes = true
		default:
			return 2, unknownOptionErr(x, "coop down", downOptions)
		}
	}
	repo, p, err := loadProject(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if err := a.rt.EnsureDaemon(); err != nil {
		return 1, reported("Could not stop services",
			fmt.Sprintf("%s is unavailable.", runtimeTitle(a.rt.Name)),
			fmt.Sprintf("Start %s, then run coop down again.", runtimeTitle(a.rt.Name)))
	}
	if !a.rt.SupportsCompose() {
		return 1, reported("Could not stop services",
			fmt.Sprintf("%s does not support Compose.", runtimeTitle(a.rt.Name)),
			"Use Docker for this project's services.")
	}
	file := box.ComposeFileAt(repo, p.ComposeRel())
	if file == "" {
		return 1, reported("Could not stop services",
			fmt.Sprintf("%s does not exist.", p.ComposeRel()),
			"Restore the Compose file, then run coop down again.")
	}
	rel, _ := filepath.Rel(repo, file)
	var targets []box.ServiceVolume
	if deleteVolumes {
		// Every target is resolved and shown BEFORE anything is removed: the person confirming a
		// permanent deletion is confirming this list, so an unanswerable runtime stops the command
		// rather than deleting against a guess.
		targets, err = box.ServiceVolumes(a.rt, repo, os.Stderr)
		if err != nil {
			return 1, reported("Could not determine which service volumes would be deleted",
				fmt.Sprintf("%s did not return the project's volume information.", runtimeTitle(a.rt.Name)),
				fmt.Sprintf("Check %s, then run coop down --delete-volumes again.", runtimeTitle(a.rt.Name)))
		}
		if code, err := confirmVolumeDeletion(targets, yes); err != nil {
			return code, err
		}
	}
	ui.Note("Stopping services from %s", rel)
	ui.Note("")
	out, errOut := &seenWriter{w: os.Stdout}, &seenWriter{w: os.Stderr}
	deleting := deleteVolumes && len(targets) > 0
	if deleting {
		err = box.DownServicesFileVolumes(a.rt, repo, file, targets, out, errOut, box.ConfigExposureRoots(a.cfg)...)
	} else {
		err = box.DownServicesFile(a.rt, repo, file, false, out, errOut, box.ConfigExposureRoots(a.cfg)...)
	}
	if out.seen || errOut.seen {
		ui.Note("")
	}
	var refused *box.ComposeRefused
	switch {
	case err != nil && errors.As(err, &refused):
		return 1, composeRefusal("Could not stop services from "+rel, rel, refused.Err, "coop down")
	case err != nil && deleting:
		return 1, reported("Could not finish stopping services and deleting their data",
			sentence(firstLine(err)),
			fmt.Sprintf("Check %s before retrying coop down --delete-volumes.", runtimeTitle(a.rt.Name)),
			"Some services or volumes may already have been removed.")
	case err != nil:
		return 1, reported("Could not stop all services", sentence(firstLine(err)),
			fmt.Sprintf("Check %s, then run coop down again.", runtimeTitle(a.rt.Name)),
			"Some services may already be stopped.")
	case deleting:
		ui.OK("Services stopped; %s deleted", ui.Count(len(targets), "volume"))
	case deleteVolumes:
		ui.OK("Services stopped")
	default:
		ui.OK("Services stopped")
		ui.Note("  Stored data was kept.")
	}
	return 0, nil
}

// confirmVolumeDeletion names every volume the deletion would destroy, then routes the decision
// through the shared destructive gate — the same default-No, the same --yes, the same refusal
// without a terminal as every other unrecoverable delete in coop. A non-nil error means the caller
// must stop; the stop itself keeps data, so an empty target list says so and continues.
func confirmVolumeDeletion(targets []box.ServiceVolume, yes bool) (int, error) {
	if len(targets) == 0 {
		ui.Note("No service volumes to delete.")
		ui.Note("")
		return 0, nil
	}
	ui.Note("Stop this project's services and permanently delete these volumes and their data?")
	ui.Note("")
	for _, v := range targets {
		if v.Holds != "" {
			ui.Note("  - %s — %s", v.Name, v.Holds)
			continue
		}
		ui.Note("  - %s", v.Name)
	}
	ui.Note("")
	// The gate's own no-terminal refusal is a one-liner about --yes; this command can say which
	// invocation to repeat, so it answers that case here and hands the gate the decidable ones.
	ask, atTerminal := askTerminal()
	if !yes && !atTerminal {
		failBlock("Volume deletion needs confirmation", "",
			"Run coop down --delete-volumes at a terminal, or add --yes to confirm deletion.")
		return 2, ui.ErrReported
	}
	// The gate still owns the decision — default No, --yes to skip it — and reads the answer back
	// through this command's own reader, which is what its ask callback exists for.
	if err := ui.DestroyGate("Continue", yes, func(string) bool {
		answer, _ := askOneOK(ask, "Continue? [y/N] ")
		return ui.ConfirmationResponse(answer, false)
	}); err != nil {
		ui.Note("")
		ui.Note("Cancelled.")
		return 2, ui.ErrReported
	}
	// --yes asked nothing, so there is no answered question to separate from what follows.
	if !yes {
		ui.Note("")
	}
	return 0, nil
}
