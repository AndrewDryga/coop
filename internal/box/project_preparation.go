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
	"github.com/AndrewDryga/coop/internal/networkstate"
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
	built                                   bool   // the builder ran and its image proved good
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
	dfRel := filteredProjectDockerfile(repo)
	if dfRel == "" {
		return nil, errors.New("this project no longer has a box Dockerfile — run it again")
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
	private, lease, err := newProjectReviewSnapshot(ctx, store.Path())
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
	change, err := buildFilteredProjectWith(ctx, rt, cfg, b.repo, out, errOut, b)
	b.built = change.After != ""
	return err
}

// RebuildApprovedProject rebuilds files a person already approved, with no prompt. It decides on the
// snapshot, not on the check that sent it here: buildPreparedFilteredProject compares the snapshot's
// digest with the approval, so an edit that lands in between builds nothing and returns
// *ProjectBuildRequired. Builds run in their own process group, so a launch that is interrupted has to
// cancel its build explicitly.
func RebuildApprovedProject(cfg *config.Config, rt runtime.Runtime, spec RunSpec, dfRel string) error {
	ctx, stop := signal.NotifyContext(networkAdmissionContext(spec), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	prepared, err := prepareApprovedRebuild(ctx, cfg, spec)
	if err != nil {
		return err
	}
	defer prepared.Close()
	if !spec.Quiet {
		prepared.starting = func() {
			ui.Section("Project box")
			ui.Note("  Rebuilding %s from the files you approved: Coop's image changed or the last one is gone.", ui.SafeInline(dfRel))
		}
	}
	stderr := networkLaunchStderr(spec)
	if err := prepared.Build(ctx, rt, cfg, stderr, stderr); err != nil {
		return err
	}
	if !spec.Quiet && prepared.built {
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
	prepared, err := prepareApprovedRebuild(ctx, cfg, RunSpec{Repo: repo})
	if err != nil {
		return ImageChange{}, err
	}
	defer prepared.Close()
	return buildFilteredProjectWith(ctx, rt, cfg, prepared.repo, out, errOut, prepared)
}

// prepareApprovedRebuild snapshots the checkout for a build no person started: the only way to mark
// one automatic, which is what makes the build refuse unless the snapshot is what a person approved.
func prepareApprovedRebuild(ctx context.Context, cfg *config.Config, spec RunSpec) (*PreparedProjectBuild, error) {
	prepared, err := PrepareFilteredProjectBuild(ctx, cfg, spec)
	if err != nil {
		return nil, err
	}
	prepared.automatic = true
	return prepared, nil
}

func projectBuildSettings() string {
	var values []string
	for _, key := range []string{"DOCKER_DEFAULT_PLATFORM", "DOCKER_BUILDKIT", "BUILDX_BUILDER", "SOURCE_DATE_EPOCH", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
		values = append(values, key+"="+os.Getenv(key))
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(values, "\x00"))))
}
