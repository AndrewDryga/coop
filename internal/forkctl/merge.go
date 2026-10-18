package forkctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/hostsurface"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// HostSurfaces lists the fork's changed files that alter what runs on your machine — hooks,
// editor and agent settings, compose, the Makefile — for the review brief. The automatic ones
// also appear in PolicyScan, where they block a merge unless forced.
func HostSurfaces(repo, ref string) []hostsurface.Finding {
	return hostsurface.Findings(forkspace.GitOut(repo, "diff", "--name-status", "HEAD..."+ref))
}

// PolicyScan returns human-readable concerns about a fork's added/changed files:
// secret-looking filenames, large blobs, and — by scanning each changed blob's
// content — real tokens sitting in ordinary files (which a filename check can't see).
// Empty means nothing flagged. A box can write the fork's repository, so it can make git fail to
// read it, for example by giving the fork a history unrelated to the project's: the error comes
// back, never an empty scan that would read as nothing flagged.
func PolicyScan(repo, ref string) ([]string, error) {
	out, err := forkspace.GitOutErr(repo, "diff", "--name-status", "HEAD..."+ref)
	if err != nil {
		return nil, fmt.Errorf("read the fork's changes: %w", err)
	}
	if out == "" {
		return nil, nil
	}
	// Flag a credential-looking filename with the SAME decider that shadows secrets from the box,
	// so the merge gate can't drift from the shadow denylist (it used to be a separate hand-rolled
	// regex that missed kubeconfig/.npmrc/.netrc/service_account.json/*.kdbx/… that SecretGlobs covers).
	shadowed := box.NewShadowDecider(repo)
	unreadable := map[string]bool{} // each .coopignore coop cannot read is one warning, not one per file
	var warns []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		// A .coopignore decides what every later box is shown, so a fork that adds, edits, removes
		// or renames one waits for --force. One that is not a regular file is named below instead.
		changed := ""
		for _, p := range f[1:] {
			if box.IsCoopignoreName(filepath.Base(p)) {
				changed = p
			}
		}
		if changed != "" && (f[0] == "D" || !notRegularCoopignore(repo, ref, f[len(f)-1])) {
			warns = append(warns, changed+" — the fork changes a .coopignore, which decides what boxes are shown")
		}
		if f[0] == "D" { // deleting any other file is never a concern
			continue
		}
		path := f[len(f)-1]
		// A file the readable rules or a secret name hide comes back without an error even under a
		// .coopignore coop cannot read, so it is named whatever that file says.
		switch hidden, err := shadowed(filepath.ToSlash(path)); {
		case err != nil: // coop cannot tell whether the file is hidden, so the merge waits for --force
			if !unreadable[err.Error()] {
				unreadable[err.Error()] = true
				warns = append(warns, err.Error()+" — remove it if you did not create it, or make it a readable regular file of at most 1 MiB, then merge again")
			}
		case hidden:
			warns = append(warns, "secret-like file: "+path)
		}
		// The fork's own policy file must stay one coop can read, or every later launch refuses.
		if notRegularCoopignore(repo, ref, path) {
			warns = append(warns, path+" — the fork makes a .coopignore something other than a regular file, which Coop cannot read")
		}
		// Files that change what runs on the HOST the moment a human touches the merged tree — a
		// hook on their next commit, a settings file their editor session runs, a compose file
		// their Docker runs — path-based, so a huge/binary blob can't dodge it below.
		if reason, automatic := hostsurface.Classify(f[0], path); automatic {
			warns = append(warns, path+" — "+reason) // runs by itself: blocks the merge unless forced
		}
		entry, err := forkspace.GitOutErr(repo, "ls-tree", ref, "--", path)
		if err != nil {
			return nil, fmt.Errorf("read %s from the fork: %w", path, err)
		}
		if strings.HasPrefix(entry, "160000 ") {
			continue // a submodule's commit: the fork changed which one, and has no content here to read
		}
		size, err := gitBlobSize(repo, ref, path)
		if err != nil {
			return nil, err
		}
		if size > 5<<20 {
			warns = append(warns, fmt.Sprintf("large file (%dMB): %s", size>>20, path))
			continue // don't read a huge blob's content
		}
		content, err := forkspace.GitOutErr(repo, "show", ref+":"+path)
		if err != nil {
			return nil, fmt.Errorf("read %s from the fork: %w", path, err)
		}
		if strings.IndexByte(content, 0) >= 0 { // skip binaries
			continue
		}
		if filepath.Base(path) == "package.json" {
			if k := addedLifecycleScript(repo, ref, path, content); k != "" {
				warns = append(warns, path+" adds a "+k+" script — npm runs it automatically on `npm install`")
			}
		}
		for _, s := range box.ScanSecrets(content) {
			warns = append(warns, fmt.Sprintf("possible secret in %s:%d (%s) — review the finding and remove any real credential from the fork before merging", path, s.Line, s.Kind))
		}
	}
	return warns, nil
}

// notRegularCoopignore reports whether the fork's change at path leaves a .coopignore, in any case,
// that is not a regular file: a link named .coopignore, or anything inside a folder named .coopignore.
func notRegularCoopignore(repo, ref, path string) bool {
	slash := filepath.ToSlash(path)
	parts := strings.Split(slash, "/")
	for _, folder := range parts[:len(parts)-1] {
		if box.IsCoopignoreName(folder) {
			return true
		}
	}
	if !box.IsCoopignoreName(parts[len(parts)-1]) {
		return false
	}
	mode, _, _ := strings.Cut(forkspace.GitOut(repo, "ls-tree", ref, "--", path), " ")
	return mode != "" && mode != "100644" && mode != "100755"
}

func gitBlobSize(repo, ref, path string) (int64, error) {
	out, err := forkspace.GitOutErr(repo, "cat-file", "-s", ref+":"+path)
	if err != nil {
		return 0, fmt.Errorf("read the size of %s in the fork: %w", path, err)
	}
	return strconv.ParseInt(out, 10, 64)
}

// addedLifecycleScript returns the name of an npm install/prepare lifecycle script the fork ADDS
// or changes in package.json (preinstall/install/postinstall/prepare) — npm runs these
// automatically on `npm install`, so a fork can plant one to execute host code post-merge — or ""
// when the change touches no such script (an ordinary dependency bump isn't flagged).
func addedLifecycleScript(repo, ref, path, newContent string) string {
	return changedLifecycleScript(forkspace.GitOut(repo, "show", "HEAD:"+path), newContent)
}

func changedLifecycleScript(oldContent, newContent string) string {
	newS := pkgScripts(newContent)
	oldS := pkgScripts(oldContent)
	for _, k := range []string{"preinstall", "install", "postinstall", "prepare"} {
		if v := newS[k]; v != "" && v != oldS[k] {
			return k
		}
	}
	return ""
}

// pkgScripts parses a package.json's "scripts" map, or nil if it doesn't parse.
func pkgScripts(content string) map[string]string {
	var p struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal([]byte(content), &p) != nil {
		return nil
	}
	return p.Scripts
}

// gateFor resolves the fork-merge revalidation gate for repo: an explicit COOP_GATE (env/conf)
// wins; otherwise the repo's committed .agent/project.yaml gate:. The gate runs IN THE BOX (see
// runGate), so a repo-authored command executes sandboxed — same trust class as the code it merges.
func (c *Control) gateFor(repo string) ([]string, error) {
	p, err := project.Load(repo)
	if err != nil {
		return nil, err
	}
	if c.cfg.Explicit("COOP_GATE") {
		return c.cfg.Gate, nil
	}
	if g := strings.TrimSpace(p.Gate); g != "" {
		return config.ShellSplit(g), nil
	}
	return c.cfg.Gate, nil
}

// MergeGate resolves the box image when a merge gate is configured (so a merge can be revalidated
// in the box), or returns "" when none is.
func (c *Control) MergeGate(repo string) (string, error) {
	return c.gateImage(repo, box.ImageForRepo(repo, c.cfg.BaseImage, c.cfg.ImageOverride))
}

func (c *Control) gateImage(repo, img string) (string, error) {
	gate, err := c.gateFor(repo)
	if err != nil {
		return "", err
	}
	if len(gate) == 0 {
		return "", nil // no gate configured → the merge is pure-local, no runtime needed
	}
	if err := c.ensureRuntime(); err != nil {
		return "", err
	}
	access, err := box.ProjectNetworkAccess(context.Background(), c.cfg, repo)
	if err != nil {
		return "", err
	}
	if access.Mode == "filtered" {
		// Admission in runGateMode proves the separately approved filtered image. Requiring
		// the ordinary tag here would strand a project prepared with `coop build --egress filtered`.
		return img, nil
	}
	return img, c.ensureGateImage(img, nil)
}

func (c *Control) ensureGateImage(img string, output io.Writer) error {
	if img == c.cfg.BaseImage {
		if err := c.host.ensureBaseImage(output); err != nil {
			return err
		}
	}
	if !box.ImageExists(c.rt, img) {
		// Same rule as requireLaunchImage: `image inspect` cannot tell a missing image from a dead daemon,
		// and a gate blocked on the wrong one sends the fix at a build that would not have helped.
		if err := c.rt.EnsureDaemon(); err != nil {
			return err
		}
		return fmt.Errorf("a gate is set but image %q isn't built — run 'coop build --egress open'", img)
	}
	return nil
}

// ReviewGateRun is what a controller job's review gate ran and how it ended. ExitCode is set
// only when the gate's command ran to an exit status.
type ReviewGateRun struct {
	Configured bool
	Command    []string
	ExitCode   *int
}

// Passed is a gate that ran and exited 0.
func (r ReviewGateRun) Passed() bool { return r.Configured && r.ExitCode != nil && *r.ExitCode == 0 }

// ReviewControllerJob runs the worker-owned checker against a disposable controller-job candidate.
// The ordinary local merge path keeps its project settings; only this path ignores repository box
// policy and the daemon's ambient run args, environment, MCP, and networking defaults. Everything
// the gate prints, and what Coop prints while starting it, goes to the request's Output.
func (c *Control) ReviewControllerJob(ctx context.Context, request sessionsvc.ReviewGateRequest) (ReviewGateRun, error) {
	gate := append([]string(nil), request.Command...)
	if len(gate) == 0 {
		return ReviewGateRun{}, nil
	}
	run := ReviewGateRun{Configured: true, Command: append([]string(nil), gate...)}
	if request.NetworkMode != "open" && request.NetworkMode != "none" && request.NetworkMode != "filtered" {
		return run, errors.New("controller review has an invalid saved network mode")
	}
	if err := c.ensureRuntime(); err != nil {
		return run, err
	}
	launchCfg, err := sessionsvc.AdmitJobResources(c.cfg, c.rt, request.Resources)
	if err != nil {
		return run, err
	}
	image := box.SharedImage(c.cfg.BaseImage, c.cfg.ImageOverride)
	if request.NetworkMode != "filtered" {
		if err := c.ensureGateImage(image, request.Output); err != nil {
			return run, err
		}
	}
	base := strings.TrimSpace(forkspace.GitOut(request.Candidate, "rev-parse", "--verify", "refs/coop/session-parent^{commit}"))
	if base == "" {
		return run, errors.New("resolve trusted review base commit")
	}
	launchCfg.SetEgress(request.NetworkMode)
	launchCfg.ExtraRunArgs = nil
	launchCfg.MCPFile = ""
	spec := box.RunSpec{
		Image: image, Repo: request.Candidate, PolicyRepo: request.Repository, Cmd: gate,
		ControllerJob: true, Review: true, Batch: true, Ctx: ctx,
		ControllerReviewRoot: request.StateRoot, ControllerReviewID: request.OperationID,
		ActivityRepo: request.Repository, ActivityKind: forkspace.ExecutionReview,
		ExtraArgs: []string{"-e", "COOP_REVIEW_BASE=" + base},
		Stdout:    request.Output, Stderr: request.Output,
	}
	keys := make([]string, 0, len(request.Environment))
	for key := range request.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		spec.ExtraArgs = append(spec.ExtraArgs, "-e", key+"="+request.Environment[key])
	}
	if request.NetworkMode == "filtered" {
		capture, err := box.CapturedEgressFromReference(launchCfg, spec, box.SessionNetworkCapture{
			Project: request.Repository, Fingerprint: request.NetworkFingerprint,
			Qualification: request.NetworkQualification, SessionID: request.SessionID,
			AttemptID: request.OperationID, JobDigest: request.JobDigest,
		})
		if err != nil {
			return run, err
		}
		defer capture.Close()
		spec.CapturedEgress = capture
		c.host.settleFilteredRuns(c.rt)
	}
	code, err := box.Run(launchCfg, c.rt, spec)
	if err != nil {
		return run, err
	}
	run.ExitCode = &code
	return run, nil
}

// runGateMode runs the merge gate in the box against treeDir. The gate POLICY is resolved from
// gateRepo (the trusted parent), never treeDir — a fork can't weaken its own checker — but it RUNS
// against treeDir (the rebased candidate), so a red gate never touches the parent. A non-zero gate
// is a normal red result; an error means the box never started.
func (c *Control) runGateMode(gateRepo, treeDir, img string, review bool) (bool, error) {
	return c.runGateBoundary(gateRepo, treeDir, img, review, "")
}

func (c *Control) runGateBoundary(gateRepo, treeDir, img string, review bool, isolatedParent string) (bool, error) {
	gate, err := c.gateFor(gateRepo)
	if err != nil {
		return false, err
	}
	// Live progress: the checks are about to run against a temporary copy rebased onto the parent
	// branch, so say which tree is being checked and with what, before anything can pass or fail.
	if review {
		branch := "the captured parent"
		if isolatedParent == "" {
			branch = gitBranch(gateRepo)
		}
		ui.Note("Checking the changes after rebasing onto %s", branch)
	} else {
		ui.Note("Running project checks")
	}
	ui.Note("  Running: %s", strings.Join(gate, " "))
	reviewBase := strings.TrimSpace(forkspace.GitOut(treeDir, "rev-parse", "--verify", "refs/coop/session-parent^{commit}"))
	if reviewBase == "" {
		if isolatedParent == "" {
			reviewBase = strings.TrimSpace(forkspace.GitOut(gateRepo, "rev-parse", "--verify", "HEAD^{commit}"))
		} else {
			base, readErr := forkspace.ObserveGit(context.Background(), gateRepo, "rev-parse", "--verify", "HEAD^{commit}")
			if readErr != nil {
				return false, readErr
			}
			reviewBase = strings.TrimSpace(string(base))
		}
	}
	if reviewBase == "" {
		return false, errors.New("resolve trusted review base commit")
	}
	activityKind := forkspace.ExecutionGate
	if review {
		activityKind = forkspace.ExecutionReview
	}
	spec := box.RunSpec{
		Image: img, Repo: treeDir, Cmd: gate, Batch: true,
		PolicyRepo: gateRepo, IsolatedParent: isolatedParent,
		Review:    review,
		Serve:     review,
		ExtraArgs: []string{"-e", "COOP_REVIEW_BASE=" + reviewBase},
		Homes:     c.cfg.Homes, Network: c.cfg.Network, Cache: c.cfg.Cache,
		ActivityRepo: gateRepo, ActivityKind: activityKind,
	}
	capture, err := box.AdmitNetwork(c.cfg, c.rt, spec, box.NetworkAdmission{})
	if err != nil {
		return false, err
	}
	defer capture.Close()
	spec.CapturedEgress = capture
	if capture != nil {
		c.host.settleFilteredRuns(c.rt)
	}
	code, err := box.Run(c.cfg, c.rt, spec)
	if err != nil {
		return false, err
	}
	return code == 0, nil
}

// runGate preserves merge's existing bool-only contract: a box startup failure is a failed gate.
func (c *Control) runGate(gateRepo, treeDir, img string) bool {
	ok, _ := c.runGateMode(gateRepo, treeDir, img, false)
	return ok
}

// gatePasses runs the merge gate (or the test seam, when set).
func (c *Control) gatePasses(gateRepo, treeDir, img string) bool {
	if c.gateOK != nil {
		return c.gateOK(gateRepo, treeDir, img)
	}
	return c.runGate(gateRepo, treeDir, img)
}

// ReviewGatePasses is the review-only gate path. The disposable candidate may write ignored build
// output; its caller verifies the pinned source identity and cleanliness after the gate returns.
// Startup errors remain distinguishable from an ordinary red gate.
func (c *Control) ReviewGatePasses(gateRepo, treeDir, img string) (bool, error) {
	if c.gateOK != nil {
		return c.gateOK(gateRepo, treeDir, img), nil
	}
	return c.runGateMode(gateRepo, treeDir, img, true)
}

type forkMergeLifecycleError struct{ cause error }

func (e *forkMergeLifecycleError) Error() string { return e.cause.Error() }
func (e *forkMergeLifecycleError) Unwrap() error { return e.cause }

func isForkMergeLifecycleError(err error) bool {
	var target *forkMergeLifecycleError
	return errors.As(err, &target)
}

// lockForkForMerge is the destructive merge authority. The early command preflight gives a useful
// refusal before runtime work, but only this flock closes the check-to-rebase/delete race with a
// concurrent detached start. Callers hold it for every host mutation of the fork or its workspace.
func lockForkForMerge(repo, name string) (func(), error) {
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		return nil, fmt.Errorf("lock fork %s state: %w", name, err)
	}
	if err := CheckWorkerStateFormat(repo, name); err != nil {
		unlock()
		return nil, &forkMergeLifecycleError{cause: err}
	}
	if forkspace.NeedsStop(repo, name) {
		unlock()
		return nil, &forkMergeLifecycleError{cause: fmt.Errorf("fork %q is running or awaiting cleanup — stop it first: coop fork stop %s", name, name)}
	}
	if identity, ok, readErr := forkspace.ReadGeneration(repo, name); readErr != nil {
		unlock()
		return nil, &forkMergeLifecycleError{cause: fmt.Errorf("read fork %s generation: %w", name, readErr)}
	} else if ok {
		if activityErr := forkspace.RequireNoForkExecutionsLocked(repo, identity); activityErr != nil {
			unlock()
			return nil, &forkMergeLifecycleError{cause: activityErr}
		}
		if reservationErr := forkspace.RequireNoWorkspaceReservationLocked(repo, identity); reservationErr != nil {
			unlock()
			return nil, &forkMergeLifecycleError{cause: reservationErr}
		}
	}
	return unlock, nil
}

func fetchForkForMerge(repo, ws, name string) error {
	unlock, err := lockForkForMerge(repo, name)
	if err != nil {
		return err
	}
	defer unlock()
	if !pathExists(ws) {
		return fmt.Errorf("no such fork: %s", name)
	}
	if err := gitFetchInto(repo, ws, name); err != nil {
		return fmt.Errorf("%s: git fetch: %w", name, err)
	}
	return nil
}

type mergeOutcome struct {
	landed   bool
	skipped  bool
	approval *landedFork
}

// A successful land transfers the open workspace pin to its caller. A name alone
// is not deletion authority: the user may keep editing while answering the prompt.
type landedFork struct {
	pin           *os.File
	info          os.FileInfo
	identity      forkspace.Identity
	hasGeneration bool
	head          string
	publishedHead string // isolated land rewrites private history, never the model source
}

func (f *landedFork) close() {
	if f != nil && f.pin != nil {
		f.pin.Close()
		f.pin = nil
	}
}

func (f *landedFork) validateLand(repo, name string) error {
	if f == nil || f.pin == nil || f.info == nil || f.head == "" {
		return errors.New("landed fork has no open deletion approval")
	}
	if _, err := f.pin.Stat(); err != nil {
		return fmt.Errorf("landed fork pin is no longer open: %w", err)
	}
	ws := forkspace.Workspace(repo, name)
	if !forkspace.SamePinned(ws, f.info) {
		return errors.New("landed fork workspace changed before removal")
	}
	identity, exists, err := forkspace.ReadGeneration(repo, name)
	if err != nil {
		return err
	}
	if exists != f.hasGeneration || identity != f.identity {
		return errors.New("landed fork generation changed before removal")
	}
	var head string
	if f.publishedHead != "" {
		head, err = observed(ws, "rev-parse", "--verify", "HEAD^{commit}")
	} else {
		head, err = forkspace.GitOutErr(ws, "rev-parse", "--verify", "HEAD^{commit}")
	}
	if err != nil {
		return fmt.Errorf("inspect landed fork HEAD: %w", err)
	}
	if head != f.head {
		return errors.New("landed fork HEAD changed before removal")
	}
	publication := f.head
	if f.publishedHead != "" {
		publication = f.publishedHead
		if _, err := observed(repo, "merge-base", "--is-ancestor", publication, "HEAD"); err != nil {
			return fmt.Errorf("cannot confirm isolated publication in the parent: %w", err)
		}
		return nil
	}
	if _, err := forkspace.GitOutErr(repo, "merge-base", "--is-ancestor", publication, "HEAD"); err != nil {
		return fmt.Errorf("cannot confirm the parent contains the landed commit: %w", err)
	}
	return nil
}

func (f *landedFork) validateRemoval(repo, name string) error {
	if err := f.validateLand(repo, name); err != nil {
		return err
	}
	if f.publishedHead != "" {
		ws := forkspace.Workspace(repo, name)
		if _, err := captureIsolatedCheckout(ws, true, 0); err != nil {
			return err
		}
		remaining := 1024
		return isolatedSourceUntracked(ws, 0, &remaining)
	}
	return ForkWorktreeClean(forkspace.Workspace(repo, name))
}

func destroyLandedFork(rt runtime.Runtime, repo, name string, approval *landedFork, exposedRoots ...string) error {
	unlock, err := lockForkForMerge(repo, name)
	if err != nil {
		return err
	}
	defer unlock()
	if err := approval.validateRemoval(repo, name); err != nil {
		return err
	}
	identity, hasGeneration, err := forkspace.ReadGeneration(repo, name)
	if err != nil {
		return err
	}
	if hasGeneration {
		if pending, err := ForkHasPendingLand(repo, identity); err != nil {
			return err
		} else if pending {
			return errors.New("land finalization is still pending; rerun merge before destroying the fork")
		}
		if active, err := tasks.ForkTaskState(repo, identity); err != nil {
			return err
		} else if active {
			return errors.New("landed fork still owns canonical task authority")
		}
	}
	_ = stopForkServices(rt, repo, name, exposedRoots...) // best effort; the warnings name what failed
	// Services have writable binds and stopping them can take time. Check again
	// after teardown, not merely before giving those processes their last turn.
	if err := approval.validateRemoval(repo, name); err != nil {
		return err
	}
	if err := forkspace.Destroy(repo, name); err != nil {
		return err
	}
	if hasGeneration {
		return forkspace.RemoveGenerationIfMatchesLocked(repo, identity)
	}
	return nil
}

// mergeOne fetches a fork's branch, merges it into the parent's HEAD, and — when a
// gate is configured — revalidates the merged result, rolling back on failure.
// "green" thus means green against the tree as it stands now, not the stale base the
// fork was cut from. Reports whether the merge landed: landed=false with an error is a merge that
// did NOT happen, while landed=true WITH an error means the commits are in the parent but exact
// candidate bookkeeping or final validation failed — the caller reports it and stops, never rolls
// the land back. Task landing resumes from its durable intent; trailers alone never complete tasks.
func (c *Control) mergeOne(repo, img, name string, force bool) (outcome mergeOutcome, retErr error) {
	return c.mergeOneMode(repo, img, name, force, false)
}

// mergeOneMode keeps the batch-only empty decision inside the same lifecycle lock as land-journal
// replay and task authority inspection. Single-fork merge retains its historical no-op landing
// approval, which is also used by cleanup validation.
func (c *Control) mergeOneMode(repo, img, name string, force, skipEmpty bool) (outcome mergeOutcome, retErr error) {
	ws := forkspace.Workspace(repo, name)
	if !pathExists(ws) {
		return mergeOutcome{}, fmt.Errorf("no such fork: %s", name)
	}
	unlock, err := lockForkForMerge(repo, name)
	if err != nil {
		return mergeOutcome{}, err
	}
	defer unlock()
	if !pathExists(ws) {
		return mergeOutcome{}, fmt.Errorf("no such fork: %s", name)
	}
	if identity, isolated, err := isolatedForkIdentity(repo, name); err != nil {
		return mergeOutcome{}, err
	} else if isolated {
		return c.mergeIsolatedLocked(repo, img, name, identity, force, skipEmpty, nil)
	}
	pin, info, err := forkspace.Pin(ws)
	if err != nil {
		return mergeOutcome{}, fmt.Errorf("pin fork before land: %w", err)
	}
	defer func() {
		if outcome.approval == nil {
			pin.Close()
		}
	}()
	legacy, err := tasks.LegacyForkQueueWithWork(ws)
	if err != nil {
		return mergeOutcome{}, err
	}
	if legacy != "" {
		return mergeOutcome{}, fmt.Errorf("%s contains a legacy copied task queue at %s; refusing a Git-only merge because it could duplicate or lose canonical work — preserve any fork-only task notes, recreate the fork with --fresh, and rerun the canonical task loop", name, legacy)
	}
	if err := gitFetchInto(repo, ws, name); err != nil {
		return mergeOutcome{}, fmt.Errorf("%s: git fetch: %w", name, err)
	}
	identity, hasGeneration, err := forkspace.ReadGeneration(repo, name)
	if err != nil {
		return mergeOutcome{}, err
	}
	finishLand := func(landed bool, head string, err error) (mergeOutcome, error) {
		result := mergeOutcome{landed: landed}
		if !landed || err != nil {
			return result, err
		}
		approval := &landedFork{pin: pin, info: info, identity: identity, hasGeneration: hasGeneration, head: head}
		if err := approval.validateLand(repo, name); err != nil {
			return result, err
		}
		result.approval = approval
		return result, nil
	}
	if hasGeneration {
		if intent, ok, err := readLandIntent(repo, identity); err != nil {
			return mergeOutcome{}, err
		} else if ok {
			ui.Note("Finishing interrupted merge bookkeeping for %s.", name)
			finished, landed, err := c.advanceTaskLand(repo, ws, name, img, intent)
			return finishLand(landed, finished.RebasedHead, err)
		}
	}
	ref := "review/" + name
	if skipEmpty && forkspace.GitOut(repo, "rev-list", "--count", "HEAD.."+ref) == "0" {
		if !hasGeneration {
			return mergeOutcome{skipped: true}, nil
		}
		summary, err := tasks.ReadForkTaskStateSummary(repo, identity)
		if err != nil {
			return mergeOutcome{}, err
		}
		if !summary.Active() {
			return mergeOutcome{skipped: true}, nil
		}
	}
	warns, scanErr := PolicyScan(repo, ref)
	if scanErr != nil && !force {
		return mergeOutcome{}, fmt.Errorf("%s: Coop could not check the fork's changes against the merge policy, so it did not merge them:\n%s\n(use --force to merge without the check)", name, indent(scanErr.Error()))
	}
	if len(warns) > 0 && !force {
		return mergeOutcome{}, fmt.Errorf("%s: policy flagged risky changes:\n%s\n(use --force to merge anyway)", name, indent(strings.Join(warns, "\n")))
	}
	// Say which branch we're landing onto — merge rebases onto your *current* branch,
	// so this is your chance to notice you're on the wrong one.
	target := forkspace.GitOut(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if target == "" || target == "HEAD" {
		target = "the current commit (detached HEAD)"
	}
	ui.Note("Rebasing onto %s", target)
	if hasGeneration {
		candidate, hasCandidate, err := tasks.ReadForkCandidate(repo, identity)
		if err != nil {
			return mergeOutcome{}, err
		}
		indexes, problems := tasks.IndexedForkAssignments(repo, identity)
		if len(problems) > 0 {
			return mergeOutcome{}, errors.Join(problems...)
		}
		if len(indexes) > 0 && !hasCandidate {
			return mergeOutcome{}, fmt.Errorf("%s has sandbox-assigned tasks but no final reviewed candidate — resume its loop before merge", name)
		}
		if hasCandidate {
			if err := forkspace.ValidateGenerationWorkspace(repo, identity); err != nil {
				return mergeOutcome{}, err
			}
			head, tree := forkspace.GitOut(ws, "rev-parse", "HEAD"), forkspace.GitOut(ws, "rev-parse", "HEAD^{tree}")
			if err := tasks.ValidateForkCandidateLocked(repo, candidate, head, tree); err != nil {
				return mergeOutcome{}, fmt.Errorf("%s reviewed candidate is stale: %w", name, err)
			}
			intent := landIntent{
				Version: landIntentVersion, Candidate: candidate,
				ParentBefore: forkspace.GitOut(repo, "rev-parse", "HEAD"), Phase: landPreparing, CreatedAt: time.Now().UTC(),
			}
			if err := writeLandIntent(repo, intent); err != nil {
				return mergeOutcome{}, err
			}
			finished, landed, err := c.advanceTaskLand(repo, ws, name, img, intent)
			return finishLand(landed, finished.RebasedHead, err)
		}
	}
	// Rebase the fork onto the parent's HEAD inside the fork's OWN clone — an isolated candidate.
	// The parent tree is NOT touched here, so a red gate below has nothing to roll back.
	if err := c.rebaseForkOntoParent(repo, ws, name); err != nil {
		return mergeOutcome{}, err
	}
	// Gate the CANDIDATE (the rebased fork), never the live parent — with the parent's own gate
	// policy. A red gate leaves the parent exactly as it was: no reset --hard of a shared tree.
	if img != "" && !c.gatePasses(repo, ws, img) {
		return mergeOutcome{}, fmt.Errorf("%s: gate failed on the rebased fork — parent untouched; fix it in the fork (%s), then re-run", name, ws)
	}
	// Advance the parent by a fast-forward-ONLY merge — an atomic compare-and-swap: it refuses if a
	// concurrent commit moved the parent since the rebase, so a divergence lands nothing (re-run to
	// rebase onto the new HEAD) instead of being erased by a rollback.
	landedHead, err := forkspace.GitOutErr(ws, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return mergeOutcome{}, fmt.Errorf("inspect candidate before land: %w", err)
	}
	if err := c.FastForwardParent(repo, ws, name); err != nil {
		return mergeOutcome{}, err
	}
	// Legacy/non-task forks land Git only. Canonical task completion is never inferred from a
	// trailer; generation candidates above carry exact task and projection authority.
	return finishLand(true, landedHead, nil)
}

// landFork rebases the fork's branch onto the parent's current HEAD — in the fork,
// where that branch is checked out — then fast-forwards the parent onto the result.
// Forks therefore land as a linear replay, never a merge commit. A rebase conflict
// leaves the fork untouched and points at where to resolve.
// rebaseForkOntoParent rebases the fork's branch onto the parent's current HEAD inside the fork's
// OWN clone (ws), producing the landing candidate WITHOUT touching the parent. Box commits are
// unsigned (the box holds no key); when you sign, the rebase re-signs the rewritten commits with
// your host key (-f forces the rewrite so even a fast-forward land gets signed), the signing config
// coming from the parent via forkspace.TrustedSignArgs, never the fork.
func (c *Control) rebaseForkOntoParent(repo, ws, name string) error {
	// A rebase that FAILS is aborted below, but a coop killed mid-rebase (a host crash, a SIGKILL)
	// leaves git's state dir behind, and every later merge then dies on the leftover state instead of
	// recovering it. Clear it first — under the same ownership rule as every other destructive path.
	if err := recoverInterruptedRebase(repo, ws, name); err != nil {
		return err
	}
	head := forkspace.GitOut(repo, "rev-parse", "HEAD")
	if head == "" {
		// A parent with no commits yet → `git rebase ""` fails with a cryptic "invalid upstream";
		// give a clear cause instead.
		return fmt.Errorf("%s: the parent repo has no commits yet — make an initial commit before landing a fork", name)
	}
	// Every git command here runs on an agent-controlled tree (the fork ws AND the parent repo,
	// whose .git the agent could have poisoned), so all go through the hardened helpers — a planted
	// .git/hooks/* or malicious .git/config must not execute on the host (forkspace.GitHardening).
	if err := forkspace.GitRun(ws, "fetch", "--quiet", repo); err != nil {
		return fmt.Errorf("%s: fetching parent into the fork: %w", name, err)
	}
	// Rebase the fork's branch by NAME, not whatever the agent left checked out, so the branch we
	// sign and rebase is provably the same one the parent fast-forwards to (an agent that `git
	// checkout`ed a different branch in the ws can't make us land un-rebased, unsigned commits).
	// The switch itself happens on the real git dir (HEAD is rewritten there; the trusted view
	// would keep it to itself) and only on a clean tree; the rebase then runs under the view, where
	// the repository's config can name no filter, merge, or diff driver for it to run.
	if current := forkspace.GitOut(ws, "symbolic-ref", "--quiet", "--short", "HEAD"); current != name {
		if gitDirty(ws) {
			return fmt.Errorf("%s: the fork has %q checked out with uncommitted changes — commit or discard them, then re-run", name, current)
		}
		if err := forkspace.GitSwitchBranch(context.Background(), ws, name); err != nil {
			return fmt.Errorf("%s: switching the fork to its own branch: %w", name, err)
		}
	}
	var rebaseErr error
	if forkspace.WantsSigning() {
		rebaseErr = gitSign(ws, append(forkspace.TrustedSignArgs(), "rebase", "-f", "--gpg-sign", head, name)...)
	} else {
		rebaseErr = forkspace.GitRun(ws, "rebase", head, name)
	}
	if rebaseErr != nil {
		_ = forkspace.GitRun(ws, "rebase", "--abort")
		return fmt.Errorf("%s: rebase onto %s failed (conflicts or signing) — fix it in the fork (cd %q && git rebase %s %s), then re-run", name, gitBranch(repo), ws, head, name)
	}
	return nil
}

// leftoverRebaseState returns the state directory of an unfinished rebase in ws, or "". Git keeps it
// in the worktree's own git dir — rebase-merge for the default backend, rebase-apply for the am one —
// and --git-path resolves either, in a plain clone as in a linked worktree.
func leftoverRebaseState(ws string) string {
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		path := forkspace.GitOut(ws, "rev-parse", "--git-path", dir)
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(ws, path)
		}
		if pathExists(path) {
			return path
		}
	}
	return ""
}

// untrustedRebaseState is rebase state sitting in the fork's OWN git dir rather than coop's
// trusted view of it — left by a rebase run outside coop (or planted). Coop never aborts it:
// an abort checks files out with that git dir's config, which is exactly what the view exists to
// keep off the host. It is named so the human can finish it by hand.
func untrustedRebaseState(ws string) string {
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		out, err := forkspace.GitRefCommand(context.Background(), ws, "rev-parse", "--path-format=absolute", "--git-path", dir).Output()
		if err != nil {
			continue
		}
		if path := strings.TrimSpace(string(out)); path != "" && pathExists(path) {
			return path
		}
	}
	return ""
}

// recoverInterruptedRebase clears rebase state a CRASHED land left in the fork's clone, so the next
// merge recovers instead of failing on it forever. Recovery is destructive — `rebase --abort` resets
// that worktree — so it runs only when the fork's lifecycle state names nobody who could still be
// running (same pid + start-token test the stop path signals by; see forkspace.StateOwner). Both
// outcomes are loud: what was found and what was done, or who owns it and how to stop them.
func recoverInterruptedRebase(repo, ws, name string) error {
	if dir := untrustedRebaseState(ws); dir != "" {
		return fmt.Errorf("%s: could not abort the unfinished rebase in %s: its state (%s) is in the fork's own git dir, which coop does not run checkouts against — finish it by hand (cd %q && git status; git rebase --abort), then re-run the merge", name, ws, filepath.Base(dir), ws)
	}
	dir := leftoverRebaseState(ws)
	if dir == "" {
		return nil
	}
	if pid, held := forkspace.StateOwner(repo, name); held {
		owner := "an owner coop cannot verify"
		if pid > 0 {
			owner = fmt.Sprintf("pid %d", pid)
		}
		return fmt.Errorf("%s: the fork worktree has an unfinished rebase (%s) from an interrupted land, but its lifecycle state is still held by %s — stop it first: coop fork stop %s", name, filepath.Base(dir), owner, name)
	}
	ui.Warn("fork %s has an unfinished rebase (%s) from an interrupted land — aborting it to recover the worktree", name, filepath.Base(dir))
	// forkspace.GitOutErr, not forkspace.GitRun: git's own stderr is the only explanation a human gets for a failed abort.
	if _, err := forkspace.GitOutErr(ws, "rebase", "--abort"); err != nil {
		return fmt.Errorf("%s: could not abort the unfinished rebase in %s: %w — finish it by hand (cd %q && git status; git rebase --abort), then re-run the merge", name, filepath.Base(dir), err, ws)
	}
	ui.Detail("aborted the unfinished rebase; %s is back on its branch", name)
	return nil
}

// FastForwardParent fetches the rebased candidate back into the parent and advances the parent
// branch by a fast-forward-ONLY merge. --ff-only IS the compare-and-swap: it succeeds only while the
// parent is still the commit the fork was rebased onto, so a concurrent commit during the gate makes
// it refuse — nothing lands, the divergence is preserved — instead of a reset --hard erasing it.
func (c *Control) FastForwardParent(repo, ws, name string) error {
	if err := gitFetchInto(repo, ws, name); err != nil {
		return fmt.Errorf("%s: git fetch: %w", name, err)
	}
	// This is coop's own host-side ref mutation on the PARENT checkout — the same worktree a `coop
	// loop` there may be validating a completion against. The ref-authority lock keeps this
	// fast-forward from landing inside that loop's validate→consume window (and vice versa), so
	// landing a fork can never trip the loop's own compare-and-swap.
	release, lockErr := tasks.LockRefAuthority(c.cfg, repo)
	if lockErr != nil {
		return fmt.Errorf("%s: acquire ref authority for %s: %w", name, repo, lockErr)
	}
	defer release()
	if err := forkspace.GitRun(repo, "merge", "--ff-only", "review/"+name); err != nil {
		return fmt.Errorf("%s: the parent advanced during the merge — nothing landed; re-run to rebase onto the new HEAD", name)
	}
	return nil
}

func (c *Control) ForkMerge(args []string) (int, error) {
	all, force, yes := false, false, false
	var pos []string
	for _, x := range args {
		switch x {
		case "--all":
			all = true
		case "--force", "-f":
			force = true
		case "--yes", "-y":
			yes = true
		default:
			if strings.HasPrefix(x, "-") {
				return 2, fmt.Errorf("coop fork merge: unknown flag %q", x)
			}
			pos = append(pos, x)
		}
	}
	name, err := oneForkName("merge", pos)
	if err != nil {
		return 2, err
	}
	// Validate the static args before the environment: a missing <name> (without --all) is a usage
	// error (exit 2), not the dirty-tree / non-interactive error (exit 1) the env gates below report.
	if !all && name == "" {
		return 2, &ui.UsageError{
			// One command, two spellings: the second row continues the first instead of
			// pretending to be another instruction.
			Headline: `Missing fork name for "coop fork merge"`,
			Rows: [][2]string{
				{"Usage:", "coop fork merge <name> [--force] [--yes]"},
				{ui.Continuation, "coop fork merge --all [--force] [--yes]"},
				{"Help:", ui.HelpCommand("coop fork merge")},
			},
		}
	}
	if all && name != "" {
		return 2, errors.New("coop fork merge: <name> and --all are mutually exclusive")
	}
	if !all && !forkspace.ValidName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
	}
	repo, err := box.ResolveRepo(c.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	var names []string
	if all {
		names, err = forkspace.Names(repo)
		if err != nil {
			return -1, err
		}
		for _, n := range names {
			if err := CheckWorkerStateFormat(repo, n); err != nil {
				return 1, err
			}
		}
	} else if err := CheckWorkerStateFormat(repo, name); err != nil {
		return 1, err
	}
	if all {
		for _, n := range names {
			if _, isolated, err := isolatedForkIdentity(repo, n); err != nil {
				return 1, err
			} else if isolated {
				return c.forkMergeIsolatedBatch(repo, names, force, yes)
			}
		}
	} else if identity, isolated, err := isolatedForkIdentity(repo, name); err != nil {
		return 1, err
	} else if isolated {
		return c.forkMergeIsolatedCommand(repo, name, identity, force, yes, false)
	}
	if gitDirty(repo) {
		return 1, errors.New("your working tree has uncommitted changes — commit or stash before merging")
	}
	// Merging lands work and (by default) deletes the fork. A non-interactive run has
	// no one to answer the prompts, so refuse rather than proceed on the default —
	// pass --yes to opt in explicitly.
	if !yes && !ui.IsTerminal(os.Stdin) {
		return 1, errors.New("coop fork merge: refusing to land in a non-interactive shell — pass --yes to confirm")
	}
	img, err := c.MergeGate(repo)
	if err != nil {
		return -1, err
	}
	if all {
		return c.forkMergeAll(repo, names, img, force, yes)
	}
	ws := forkspace.Workspace(repo, name) // name is non-empty here (the !all && name=="" check above returned)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", name)
	}
	// Rebasing/deleting a fork whose loop is still mid-iteration corrupts the in-flight work and
	// orphans the worker. Stop the loop first.
	if err := fetchForkForMerge(repo, ws, name); err != nil {
		if isForkMergeLifecycleError(err) {
			return 1, err
		}
		return -1, err
	}
	ref := "review/" + name
	ahead := forkspace.GitOut(repo, "rev-list", "--count", "HEAD.."+ref)
	ins, del := parseShortstat(forkspace.GitOut(repo, "diff", "--shortstat", "HEAD..."+ref))
	aheadN, _ := strconv.Atoi(ahead)
	ui.Note("Merge fork %s into %s", name, gitBranch(repo))
	ui.Note("  %s · +%d −%d", ui.Count(aheadN, "commit"), ins, del)
	if _, s := c.host.forkCost(ws); s != "" {
		ui.Note("  Reported cost: %s", s)
	}
	ui.Note("")
	if !approve("Merge these commits?", yes) {
		ui.Note("")
		ui.Note("Cancelled. No commits merged.")
		return 0, nil
	}
	result, err := c.mergeOne(repo, img, name, force)
	defer result.approval.close()
	if result.skipped {
		ui.Note("Skipped %s — no new commits or pending landing.", name)
		return 0, nil
	}
	if result.landed {
		// Say it BEFORE any error: the commits are in the parent branch either way.
		ui.Note("")
		ui.OK("Merged fork %s into %s", name, gitBranch(repo))
	}
	if err != nil {
		// A landed fork whose queue reconciliation failed keeps its workspace and its exit code: the
		// human is being asked to check the queue, so this is no moment to delete anything.
		ui.Error("%v", err)
		return 1, nil
	}
	if !result.landed {
		return 1, nil
	}
	// The merge landed the committed work (via review/<name>); an interrupted iteration can still leave
	// uncommitted changes in the fork's worktree, so keep a dirty fork with a note rather than discard it.
	if gitDirty(ws) {
		ui.Alert("The fork still has uncommitted changes",
			"The fork was kept so you can review those files.",
			[2]string{"Open it:", "coop fork open " + name})
		return 0, nil
	}
	// Default-No delete confirm (the land above was the default-Yes step); --yes is already required
	// for a non-interactive run, so this only prompts at a TTY. Declining just keeps the landed fork.
	ui.Note("")
	if forkHasServices(repo, name) {
		ui.Note("Will be permanently deleted:")
		ui.Note("  - This fork's service containers, Docker volumes, and their stored data.")
		ui.Note("")
	}
	if ui.DestroyGate("Delete the merged fork", yes) == nil {
		// --yes asked nothing, so there is no answered question to separate from what follows.
		if !yes {
			ui.Note("")
		}
		if err := destroyLandedFork(c.rt, repo, name, result.approval, box.ConfigExposureRoots(c.cfg)...); err != nil {
			if isForkMergeLifecycleError(err) {
				return 1, err
			}
			return -1, err
		}
		ui.OK("Deleted fork %s", name)
		return 0, nil
	}
	ui.Note("")
	ui.Note("Kept fork %s.", name)
	return 0, nil
}

// forkMergeAll lands every fork as a revalidating rebase queue: each is rebased onto
// the result of the previous one and re-gated, so a later fork can't ride in green
// against a base that an earlier landing already changed. It stops at the first
// conflict or gate failure, leaving the remaining forks untouched.
func (c *Control) forkMergeAll(repo string, names []string, img string, force, yes bool) (int, error) {
	if len(names) == 0 {
		ui.Note("No forks to merge.")
		return 0, nil
	}
	// Never touch a fork whose loop is still running — rebasing/deleting its live worktree corrupts
	// in-flight work and orphans the worker. Skip those with a notice and land the rest.
	skip := map[string]bool{}
	live := runningForkNames(repo, names)
	for _, n := range live {
		skip[n] = true
	}
	if len(skip) == len(names) {
		ui.Note("No forks are ready to merge. %s running or need cleanup.",
			ui.Count(len(live), "fork is", "forks are"))
		for _, n := range live {
			ui.Note("  Stop it: coop fork stop %s", n)
		}
		return 0, nil
	}
	eligible := make([]string, 0, len(names))
	for _, n := range names {
		if !skip[n] {
			eligible = append(eligible, n)
		}
	}
	ui.Note("Process %s for merge into %s", ui.Count(len(eligible), "fork"), gitBranch(repo))
	for _, n := range eligible {
		ui.Note("  %s", n)
	}
	if len(live) > 0 {
		ui.Note("")
		ui.Note("Will be kept:")
		for _, n := range live {
			ui.Note("  - %s is still running and will be skipped.", n)
		}
	}
	ui.Note("")
	ui.Note("After processing:")
	ui.Note("  - Each fork that lands will be deleted.")
	ui.Note("  - A fork with no new commits or pending landing will be kept.")
	if anyForkHasServices(repo, eligible) {
		ui.Note("  - Service containers, Docker volumes, and stored data for forks that land will be deleted.")
	}
	ui.Note("")
	// Landing every fork also DELETES each one — and unlike the single-fork path (which prompts per
	// fork), this runs unattended. Ask once before destroying anything; --yes (already required for a
	// non-interactive run) skips the prompt.
	if err := ui.DestroyGate("Merge and delete forks that land", yes); err != nil {
		return 2, ForkCancelled(err)
	}
	// --yes asked nothing, so there is no answered question to separate from the first fork.
	if !yes {
		ui.Note("")
	}
	var landed []string
	for _, n := range names {
		if skip[n] {
			continue
		}
		ws := forkspace.Workspace(repo, n)
		result, err := c.mergeOneMode(repo, img, n, force, true)
		if result.skipped {
			ui.Note("Skipped %s — no new commits or pending landing.", n)
			result.approval.close()
			continue
		}
		if result.landed {
			ui.Note("") // a section of its own, after the landing's gate output and any warning
			ui.OK("Merged fork %s into %s", n, gitBranch(repo))
			// Keep the fork when its worktree still holds uncommitted work (an interrupted iteration),
			// and when its queue reconciliation failed — deleting a workspace right after an
			// unexplained bookkeeping failure removes the one thing left to inspect.
			if gitDirty(ws) {
				ui.Note("Kept fork %s — it still has uncommitted changes.", n)
			} else if err == nil {
				if destroyErr := destroyLandedFork(c.rt, repo, n, result.approval, box.ConfigExposureRoots(c.cfg)...); destroyErr != nil {
					ui.Alert("Merged fork "+n+", but its cleanup is incomplete", destroyErr.Error())
				} else {
					ui.OK("Deleted fork %s", n)
				}
			}
			landed = append(landed, n)
		}
		result.approval.close()
		if err != nil {
			remaining := remainingForks(names, skip, landed, n)
			cause := fmt.Sprintf("%v\nMerged: %s.", err, mergedList(landed))
			if len(remaining) > 0 {
				cause += "\nUntouched: " + ui.List(remaining, "and") + "."
			}
			ui.Failure("Could not merge fork "+n, cause)
			return 1, ui.Reported(err)
		}
	}
	if len(landed) == 0 {
		return 0, nil
	}
	ui.Note("")
	ui.OK("Merged %s", ui.Count(len(landed), "fork"))
	return 0, nil
}

// forkHasServices reports whether removing this fork would also remove service containers and
// their volumes, so a confirmation can name that loss instead of implying only a checkout goes.
func forkHasServices(repo, name string) bool {
	return box.ComposeFileAt(forkspace.Workspace(repo, name), project.DefaultCompose) != ""
}

func anyForkHasServices(repo string, names []string) bool {
	for _, n := range names {
		if forkHasServices(repo, n) {
			return true
		}
	}
	return false
}

// mergedList names what actually landed before a failure, so a halted batch reports durable
// results rather than a bare count.
func mergedList(landed []string) string {
	if len(landed) == 0 {
		return "nothing"
	}
	return ui.List(landed, "and")
}

// remainingForks is the eligible forks a halted batch never touched — named, so nobody has to
// diff the list themselves.
func remainingForks(names []string, skip map[string]bool, landed []string, failed string) []string {
	done := map[string]bool{failed: true}
	for _, n := range landed {
		done[n] = true
	}
	var out []string
	for _, n := range names {
		if !skip[n] && !done[n] {
			out = append(out, n)
		}
	}
	return out
}
