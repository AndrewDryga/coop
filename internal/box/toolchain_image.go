package box

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/scaffold"
	"github.com/AndrewDryga/coop/internal/ui"
)

// A project with a .tool-versions and no box Dockerfile runs in a toolchain image: the shared base
// plus the pinned tools, built by Coop from the Dockerfile `coop init --stack asdf` would write.
// Every box of the project shares the image, and none can change it: each writes only its own
// container layer, thrown away with the box. Boxes used to install the tools when they started,
// into one ~/.asdf volume every project shared, so one project's box could change the tools the
// next project's box ran.
//
// The tag is a digest of the base it builds on and the files it builds from, so a changed
// .tool-versions or a new base is a new image, built on the next launch, and projects that pin the
// same tools share one. Like the base, these images are reclaimed once nothing has used them for
// two weeks (reclaim.go); the next launch builds one again.
const toolchainRepository = "coop-toolchain"

// toolchainDefinition is what a toolchain image builds from: the rendered Dockerfile and the
// project's .tool-versions. ok is false when the repo has no regular .tool-versions at its root.
func toolchainDefinition(repo string) (dockerfile string, toolVersions []byte, ok bool, err error) {
	if repo == "" {
		return "", nil, false, nil
	}
	toolVersions, err = readRepositoryRegularFileNoFollow(repo, ".tool-versions", imageInputFileLimit)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, fmt.Errorf("read .tool-versions: %w", err)
	}
	dockerfile, err = scaffold.ToolchainDockerfile(toolVersions)
	if err != nil {
		return "", nil, false, err
	}
	return dockerfile, toolVersions, true, nil
}

// toolchainImage is the toolchain image tag for repo on base, or "" when repo pins no tools or its
// .tool-versions cannot be read (the build reports why).
func toolchainImage(repo, base string) string {
	dockerfile, toolVersions, ok, err := toolchainDefinition(repo)
	if !ok || err != nil {
		return ""
	}
	h := sha256.New()
	for _, part := range [][]byte{[]byte(base), []byte(dockerfile), toolVersions} {
		h.Write(part)
		h.Write([]byte{0})
	}
	return toolchainRepository + ":" + hex.EncodeToString(h.Sum(nil))[:32]
}

// IsToolchainImage reports whether img is a toolchain image Coop builds.
func IsToolchainImage(img string) bool {
	repository, _, ok := strings.Cut(img, ":")
	return ok && repository == toolchainRepository
}

// EnsureToolchainImage builds repo's toolchain image when img is one and is missing, on an
// open-network launch; build output goes to w. Other modes leave a missing image to the caller's
// "not built" refusal: Coop never builds automatically for them.
func EnsureToolchainImage(rt runtime.Runtime, cfg *config.Config, repo, img, version string, w io.Writer) error {
	if img == cfg.BaseImage {
		// A .tool-versions Coop cannot read (a link, say) leaves the launch on the base: say so,
		// rather than start a box quietly missing its tools.
		if _, _, _, err := toolchainDefinition(repo); err != nil {
			ui.Warning("This box runs without the project's tools", err.Error(), "Make .tool-versions a regular file in the repository root.")
		}
		return nil
	}
	if !IsToolchainImage(img) || ImageExists(rt, img) {
		return nil
	}
	if cfg.Egress != "" && egress.Mode(cfg.Egress) != egress.Open {
		return nil
	}
	if err := rt.EnsureDaemon(); err != nil {
		return err
	}
	if err := BuildWith(rt, cfg, repo, false, version, strings.NewReader(""), w); err != nil {
		return toolchainBuildError(err)
	}
	return nil
}

// toolchainBuildError points a failed toolchain build at the two ways out: fix .tool-versions, or
// take over the build with a project Dockerfile.
func toolchainBuildError(err error) error {
	return fmt.Errorf("building this project's tools from .tool-versions failed: %w — fix .tool-versions, "+
		"or write an .agent/Dockerfile to control the build ('coop init --stack asdf' writes one from it)", err)
}

// stageToolchainContext writes the two files a toolchain build reads into a new folder, so the
// build never sees the rest of the repository.
func stageToolchainContext(dockerfile string, toolVersions []byte) (dir string, cleanup func(), err error) {
	dir, err = os.MkdirTemp("", "coop-toolchain-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	if err := errors.Join(
		os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644),
		os.WriteFile(filepath.Join(dir, ".tool-versions"), toolVersions, 0o644),
	); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

// buildToolchainImage builds plan's toolchain image from a context holding only the generated
// Dockerfile and .tool-versions.
func buildToolchainImage(rt runtime.Runtime, cfg *config.Config, repo string, plan BuildPlan, fresh bool, stdin io.Reader, stdout io.Writer) error {
	dockerfile, toolVersions, ok, err := toolchainDefinition(repo)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New(".tool-versions is gone; nothing to build")
	}
	dir, cleanup, err := stageToolchainContext(dockerfile, toolVersions)
	if err != nil {
		return &StageError{Err: err}
	}
	defer cleanup()
	previous := rt.ImageID(plan.Image) // a fresh rebuild under the same tag leaves this one untagged
	if err := runBuild(rt, stdin, stdout, os.Stderr, projectBuildArgs(dir, "Dockerfile", plan.Image, cfg.BaseImage, true, fresh)...); err != nil {
		return err
	}
	StampImageInputs(cfg, repo, plan.Image) // for coop build --check and the age reminder
	reclaimAfterBuild(context.Background(), rt, cfg, plan.Image, previous, stdout, nil)
	return nil
}
