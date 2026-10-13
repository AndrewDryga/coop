package box

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// ProjectBuildRequired is a preparation miss, not permission to run repository instructions.
// Only an explicitly eligible host caller may offer a build; other callers return the remedy.
type ProjectBuildRequired struct {
	Dockerfile string
	Reason     string
}

func (e *ProjectBuildRequired) Error() string {
	return fmt.Sprintf("this project's %s is not ready: %s. Review the Dockerfile and copied build files, then run 'coop build --egress filtered' on the host. Project builds use ordinary networking", e.Dockerfile, e.Reason)
}

func projectDockerfileForRun(spec RunSpec) string {
	mode, _ := executionMode(spec)
	if spec.Login || spec.ControllerJob || mode.Restricted() {
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
	required := func(reason string) error { return &ProjectBuildRequired{Dockerfile: dfRel, Reason: reason} }
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
	repo := projectPolicyRepo(spec)
	entries, err := buildContextSelection(ctx, repo)
	if err != nil {
		return err
	}
	tree, err := contextDigest(ctx, repo, entries, nil)
	if err != nil {
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
// outside every mount. It is short-lived, non-concurrent and consumed at most once after consent.
type PreparedProjectBuild struct {
	repo, dockerfile, dir, digest, settings string
	entries                                 []contextEntry
	consumed                                bool
	lease                                   *os.File
	cleanupDir                              string
	directoryInfo, leaseInfo                os.FileInfo
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
	prepared.directoryInfo, err = os.Lstat(private)
	if err == nil {
		prepared.leaseInfo, err = lease.Stat()
	}
	if err != nil {
		_ = lease.Close()
		return nil, err
	}
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
		_, err = reapOwnedProjectReviewSnapshot(root, filepath.Base(b.cleanupDir), b.directoryInfo, b.leaseInfo)
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
	return buildFilteredProjectWith(ctx, rt, cfg, b.repo, out, errOut, b)
}

func projectBuildSettings() string {
	var values []string
	for _, key := range []string{"DOCKER_DEFAULT_PLATFORM", "DOCKER_BUILDKIT", "BUILDX_BUILDER", "SOURCE_DATE_EPOCH", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
		values = append(values, key+"="+os.Getenv(key))
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(values, "\x00"))))
}
