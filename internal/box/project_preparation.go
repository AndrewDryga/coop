package box

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// ProjectBuildRequired is a preparation miss, not permission to run repository instructions. Approved
// says these exact files already have a person's approval, so rebuilding them runs nothing new; a
// final check on the staged snapshot still decides (buildPreparedFilteredProject). For any other miss
// only an explicitly eligible host caller may offer a build; other callers return the remedy.
type ProjectBuildRequired struct {
	Dockerfile string
	Reason     string
	Approved   bool
}

func (e *ProjectBuildRequired) Error() string {
	return fmt.Sprintf("this project's %s is not ready: %s. Review the Dockerfile and copied build files, then run 'coop build --egress filtered' on the host. Project builds use ordinary networking", e.Dockerfile, e.Reason)
}

// BrokenProjectFile is a project file that does not load. Nothing in it applies, and only a person can
// settle it, so to an editor session it is an answer about the box and not a failure to look.
type BrokenProjectFile struct{ Err error }

func (e *BrokenProjectFile) Error() string { return e.Err.Error() }
func (e *BrokenProjectFile) Unwrap() error { return e.Err }

// ErrProjectNotAdmitted is a session held on a broken file whose file loads again, while no admission of
// it has replaced the capture that only holds. A refusal of that admission went to the agent log.
var ErrProjectNotAdmitted = errors.New("the fixed project file has not been admitted")

func projectDockerfileForRun(spec RunSpec) string {
	if spec.Login || spec.ControllerJob {
		return ""
	}
	return filteredProjectDockerfile(projectPolicyRepo(spec))
}

// Readiness precedes host qualification and mount scanning. It grants nothing: the final launch
// still rereads inputs and proves the immutable image's derivation and pinned client bytes.
func checkFilteredProjectReadiness(ctx context.Context, docker filteredDocker, store *networkstate.Store, spec RunSpec, binding networkstate.RuntimeBinding) error {
	dfRel := projectDockerfileForRun(spec)
	if dfRel == "" {
		return nil
	}
	repo := projectPolicyRepo(spec)
	entries, err := buildContextSelection(ctx, repo)
	if err != nil {
		return err
	}
	tree, err := contextDigest(ctx, repo, entries, nil)
	if err != nil {
		return err
	}
	canonical, err := canonicalProjectDir(repo)
	if err != nil {
		return err
	}
	approved := store.ProjectBuildReviewed(canonical, projectBuildReview(tree, binding.DaemonID, dfRel))
	required := func(reason string) error {
		return &ProjectBuildRequired{Dockerfile: dfRel, Reason: reason, Approved: approved}
	}
	definition, _, closure, err := lockedImageDefinition(agents.ClientPlatform{OS: binding.OS, Architecture: binding.Architecture, Libc: "glibc"})
	if err != nil {
		return err
	}
	client, _, err := docker.Image(ctx, definition.Tag)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, runtime.ErrDockerImageMissing) {
			return required("Coop's current client image has not been prepared")
		}
		return err
	}
	tag := filteredProjectTag(repo, client)
	candidate := networkstate.CandidateSpec{Runtime: binding, ClientImage: client}
	image, reason := store.ProjectBuildStatus(tag, projectBuildInputs(tree, candidate, closure, definition.Tag, tag, dfRel))
	if image == "" {
		return required(reason)
	}
	if id, _, err := docker.Image(ctx, image); err != nil || id != image {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !errors.Is(err, runtime.ErrDockerImageMissing) {
			return err
		}
		return required("the explicitly built project image is missing")
	}
	return nil
}

// PreparedProjectBuild owns one inert, sanitized snapshot under the owner-private network root,
// outside every mount. It is short-lived, non-concurrent and consumed at most once after consent, or
// after its digest proves a person already approved these files (automatic).
type PreparedProjectBuild struct {
	repo, dockerfile, dir, digest, settings string
	entries                                 []contextEntry
	consumed                                bool
	automatic                               bool   // no person started this build, so it builds only approved files
	starting                                func() // called once it has decided to run the builder
	lease                                   *os.File
	cleanupDir                              string
}

func (b *PreparedProjectBuild) Dockerfile() string { return b.dockerfile }
func (b *PreparedProjectBuild) Directory() string  { return b.dir }

func (b *PreparedProjectBuild) verifySnapshot(ctx context.Context) error {
	// Include every saved entry, not a second sanitized selection that could hide new files.
	var entries []contextEntry
	err := filepath.WalkDir(b.dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(b.dir, path)
		if err != nil || rel == "." {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() && info.Mode().Perm() != 0o755 {
			return errors.New("saved build snapshot directory permissions changed — run it again")
		}
		e := contextEntry{rel: rel, mode: info.Mode(), file: info}
		if info.Mode()&fs.ModeSymlink != 0 {
			e.target, err = os.Readlink(path)
			if err != nil {
				return err
			}
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("saved build snapshot contains an unsupported file")
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return err
	}
	digest, err := contextDigest(ctx, b.dir, entries, nil)
	if err != nil {
		return err
	}
	if digest != b.digest {
		return errors.New("the saved build snapshot changed after preparation — run it again")
	}
	return nil
}

func PrepareFilteredProjectBuild(ctx context.Context, cfg *config.Config, spec RunSpec) (*PreparedProjectBuild, error) {
	repo, err := canonicalProjectDir(projectPolicyRepo(spec))
	if err != nil {
		return nil, err
	}
	root, err := NetworkStatePath()
	if err != nil {
		return nil, err
	}
	exposed, err := networkExposureRoots(cfg, spec)
	if err != nil {
		return nil, err
	}
	store, err := networkstate.Open(root, exposed)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return snapshotProjectBuild(ctx, store.Path(), repo)
}

// snapshotProjectBuild stages what repo's build would read under parent, the owner-private network
// root, and hashes exactly what it wrote.
func snapshotProjectBuild(ctx context.Context, parent, repo string) (*PreparedProjectBuild, error) {
	dfRel := filteredProjectDockerfile(repo)
	if dfRel == "" {
		return nil, errors.New("this project no longer has a box Dockerfile — run it again")
	}
	private, lease, err := newProjectReviewSnapshot(ctx, parent)
	if err != nil {
		return nil, err
	}
	prepared := &PreparedProjectBuild{cleanupDir: private, lease: lease}
	ready := false
	defer func() {
		if !ready {
			_ = prepared.Close()
		}
	}()
	entries, err := buildContextSelection(ctx, repo)
	if err != nil {
		return nil, &StageError{Err: err}
	}
	dir, digest, _, err := stageContextEntriesUnder(ctx, repo, entries, private)
	if err != nil {
		return nil, &StageError{Err: err}
	}
	if err := validateProjectBuildFiles(dir, entries, dfRel); err != nil {
		return nil, &StageError{Err: err}
	}
	dockerfile, err := readRepositoryRegularFileNoFollow(dir, dfRel, imageInputFileLimit)
	if err != nil {
		return nil, &StageError{Err: err}
	}
	if err := refuseCopiedProjectFile(dockerfile, dfRel); err != nil {
		return nil, err
	}
	prepared.repo, prepared.dockerfile, prepared.dir = repo, dfRel, dir
	prepared.digest, prepared.entries, prepared.settings = digest, entries, projectBuildSettings()
	ready = true
	return prepared, nil
}

func (b *PreparedProjectBuild) Close() error {
	if b == nil || b.dir == "" && b.cleanupDir == "" {
		return nil
	}
	if b.lease != nil {
		// Closing our copy preserves an inherited child's lock; explicit LOCK_UN would not.
		if err := b.lease.Close(); err != nil {
			return err
		}
		b.lease = nil
	}
	if b.cleanupDir != "" {
		root, custody, err := projectReviewCustody(context.Background(), filepath.Dir(b.cleanupDir))
		if err != nil {
			return err
		}
		defer root.Close()
		defer custody.Close()
		_, err = reapProjectReviewSnapshot(root, filepath.Base(b.cleanupDir))
		if err == nil {
			b.dir, b.cleanupDir = "", ""
		}
		return err
	}
	err := os.RemoveAll(b.dir)
	if err == nil {
		b.dir = ""
	}
	return err
}

func (b *PreparedProjectBuild) Build(ctx context.Context, rt runtime.Runtime, cfg *config.Config, out, errOut io.Writer) error {
	if b == nil || b.dir == "" || b.consumed {
		return errors.New("the reviewed project build is no longer available")
	}
	b.consumed = true
	if b.settings != projectBuildSettings() {
		return errors.New("project build settings changed while awaiting confirmation — run it again")
	}
	_, err := buildFilteredProjectWith(ctx, rt, cfg, b.repo, out, errOut, func(rt runtime.Runtime, docker filteredDocker, store *networkstate.Store, repo string, candidate networkstate.CandidateSpec) (ImageChange, error) {
		if b.repo != repo {
			return ImageChange{}, errors.New("the reviewed build belongs to a different project")
		}
		return buildPreparedFilteredProject(ctx, rt, cfg, docker, store, b, candidate, errOut)
	})
	return err
}

// EditorBoxReadiness says whether a filtered editor session's next box can start: nil, or the
// *BrokenProjectFile, *ProjectBuildRequired, *ServicesNeedRules, *networkstate.PendingApproval,
// ErrProjectNotAdmitted or *ServiceLaunchBusy a person or a rebuild has to settle first. Any other error
// is a failure to look, and proves nothing either way. It runs the checks the launch itself will, and
// grants nothing: the launch still makes them.
func EditorBoxReadiness(ctx context.Context, cfg *config.Config, rt runtime.Runtime, capture *CapturedEgress, spec RunSpec) error {
	if capture == nil || capture.Store == nil {
		return errors.New("editor box readiness requires a host capture")
	}
	// First, since every launch refuses a file that does not load, and the checks below would read it as
	// a file that asks for nothing.
	p, err := project.Load(projectPolicyRepo(spec))
	if err != nil {
		return &BrokenProjectFile{Err: err}
	}
	switch {
	case capture.Pending != nil:
		return capture.Pending
	case capture.BrokenFile:
		return ErrProjectNotAdmitted
	}
	q, err := capture.Store.Qualification(capture.QualificationID)
	if err != nil {
		return err
	}
	docker, err := runtime.BindDocker(ctx, rt, q.Candidate.Runtime.Endpoint, q.Candidate.Runtime.DaemonID)
	if err != nil {
		return err
	}
	defer docker.Close()
	if err := checkFilteredProjectReadiness(ctx, docker, capture.Store, spec, q.Candidate.Runtime); err != nil {
		return err
	}
	policy, err := capture.Store.LoadSnapshot(capture.Project, capture.Fingerprint)
	if err != nil {
		return err
	}
	if err := editorServicesReadiness(cfg, spec, p, policy); err != nil {
		return err
	}
	if len(serviceGrants(policy)) == 0 {
		return nil // it starts no services, so nothing running keeps it out
	}
	return EditorServiceLaunchBusy(spec)
}

// editorServicesReadiness is prepareFilteredExecution's services refusal, asked early.
func editorServicesReadiness(cfg *config.Config, spec RunSpec, p *project.Project, policy egress.Snapshot) error {
	cfg = applyProjectPolicy(cfg, p, &spec)
	return checkServicesHaveRules(cfg, spec, ComposeFileAt(spec.Repo, p.ComposeRel()), serviceGrants(policy))
}

// ProjectServicesRefusal asks editorServicesReadiness for a person with no session: the refusal a filtered
// box of this project would get under what is approved now, or nil when it would get none. It only reads,
// and the mode is the one a launch would resolve, so `coop approve` can ask it when it has nothing to approve.
func ProjectServicesRefusal(ctx context.Context, cfg *config.Config, repo string) (*ServicesNeedRules, error) {
	access, err := ProjectNetworkAccess(ctx, cfg, repo)
	if err != nil {
		return nil, err
	}
	if access.Mode != egress.Filtered {
		return nil, nil
	}
	var approved egress.Snapshot
	if access.Approval != nil {
		for _, rule := range access.Approval.Envelope {
			approved.Grants = append(approved.Grants, egress.Grant{Rule: rule})
		}
	}
	p, err := project.Load(repo)
	if err != nil {
		return nil, err
	}
	err = editorServicesReadiness(cfg, RunSpec{Repo: repo, Network: cfg.Network}, p, approved)
	var refused *ServicesNeedRules
	if errors.As(err, &refused) {
		refused.name = "This project's box"
		return refused, nil
	}
	return nil, err
}

// EditorBoxStamp changes when a person may have settled what EditorBoxReadiness reported: a project
// build or an approval finished, the project's configuration or Dockerfile changed, or the service
// launch lock came free. Reading the build's files on every poll would cost a large project about a
// second; this costs a few stats and one small read. It proves nothing: a waiting session reads it to
// know when to ask EditorBoxReadiness again. An open or offline session has only the lock to watch.
func EditorBoxStamp(capture *CapturedEgress, spec RunSpec) string {
	repo := projectPolicyRepo(spec)
	authority, _, err := forkspace.ResolveProjectBinding(repo)
	if err != nil {
		authority = repo
	}
	taken, _ := forkspace.ServiceLaunchTaken(authority) // a failure to look reads as free: only a hint
	if capture == nil {
		return fmt.Sprint(taken)
	}
	stamp := func(path string) string {
		info, err := os.Stat(path)
		if err != nil {
			return "-"
		}
		return fmt.Sprintf("%d/%d", info.ModTime().UnixNano(), info.Size())
	}
	generation, _ := capture.Store.ApprovalGeneration() // unreadable reads as zero: only a hint, and the recheck remains
	return strings.Join([]string{capture.Fingerprint, capture.Store.ProjectBuildReviewStamp(capture.Project), fmt.Sprint(generation),
		stamp(filepath.Join(repo, project.File)), stamp(filepath.Join(repo, project.DockerfilePath(repo))), fmt.Sprint(taken)}, " ")
}

// RebuildApprovedProject rebuilds files a person already approved, with no prompt. It decides on the
// snapshot, not on the check that sent it here: buildPreparedFilteredProject compares the snapshot's
// digest with the approval, so an edit that lands in between builds nothing and returns
// *ProjectBuildRequired. Builds run in their own process group, so a launch that is interrupted has to
// cancel its build explicitly.
func RebuildApprovedProject(cfg *config.Config, rt runtime.Runtime, spec RunSpec, dfRel string) error {
	ctx, stop := signal.NotifyContext(networkAdmissionContext(spec), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var starting func()
	if !spec.Quiet {
		starting = func() {
			ui.Section("Project box")
			ui.Note("  Rebuilding %s from the files you approved: Coop's image changed or the last one is gone.", ui.SafeInline(dfRel))
		}
	}
	stderr := networkLaunchStderr(spec)
	change, err := rebuildApprovedProject(ctx, rt, cfg, projectPolicyRepo(spec), starting, stderr, stderr)
	if err != nil {
		return err
	}
	if !spec.Quiet && change.After != "" {
		ui.Pass("Project box built")
		ui.Note("")
	}
	return nil
}

// UpdateApprovedProject is what `coop update` does for a filtered project. An update is not a review:
// the person asked for a new Coop, and may not know an agent edited the Dockerfile. So it prepares the
// host and rebuilds approved files, and any other files return *ProjectBuildRequired for `coop build`.
func UpdateApprovedProject(ctx context.Context, rt runtime.Runtime, cfg *config.Config, repo string, out, errOut io.Writer) (ImageChange, error) {
	if filteredProjectDockerfile(repo) == "" {
		return BuildFilteredProject(ctx, rt, cfg, repo, out, errOut) // Coop's own images only
	}
	return rebuildApprovedProject(ctx, rt, cfg, repo, nil, out, errOut)
}

// rebuildApprovedProject takes the project's lock for a build no person started, and does all of its
// reading under it: several editor windows finding one image missing wait with no copies of the
// checkout on disk. starting, if set, is called once it has decided to run the builder.
func rebuildApprovedProject(ctx context.Context, rt runtime.Runtime, cfg *config.Config, repo string, starting func(), out, errOut io.Writer) (ImageChange, error) {
	canonical, err := canonicalProjectDir(repo)
	if err != nil {
		return ImageChange{}, err
	}
	return buildFilteredProjectWith(ctx, rt, cfg, canonical, out, errOut, func(rt runtime.Runtime, docker filteredDocker, store *networkstate.Store, repo string, candidate networkstate.CandidateSpec) (ImageChange, error) {
		return rebuildApprovedLocked(ctx, rt, cfg, docker, store, repo, candidate, starting, errOut)
	})
}

// rebuildApprovedLocked runs under the project's build lock. A launch that waited for it usually finds
// the image built, and a read of the checkout can say so, since that runs nothing and the launch reads
// these inputs again. Only a snapshot can say to build: this is the one place a build is marked
// automatic, which makes it refuse unless the snapshot is what a person approved.
func rebuildApprovedLocked(ctx context.Context, rt runtime.Runtime, cfg *config.Config, docker filteredDocker, store *networkstate.Store, repo string, candidate networkstate.CandidateSpec, starting func(), errOut io.Writer) (ImageChange, error) {
	if checkFilteredProjectReadiness(ctx, docker, store, RunSpec{Repo: repo}, candidate.Runtime) == nil {
		return ImageChange{}, nil
	}
	prepared, err := snapshotProjectBuild(ctx, store.Path(), repo)
	if err != nil {
		return ImageChange{}, err
	}
	defer prepared.Close()
	prepared.automatic, prepared.starting = true, starting
	return buildPreparedFilteredProject(ctx, rt, cfg, docker, store, prepared, candidate, errOut)
}

func projectBuildSettings() string {
	var values []string
	for _, key := range []string{"DOCKER_DEFAULT_PLATFORM", "DOCKER_BUILDKIT", "BUILDX_BUILDER", "SOURCE_DATE_EPOCH", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
		values = append(values, key+"="+os.Getenv(key))
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(values, "\x00"))))
}
