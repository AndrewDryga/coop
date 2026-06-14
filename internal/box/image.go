package box

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// BaseDockerfile is the shared base image: Node, the three agent CLIs, and the
// ACP adapters, running as the non-root `node` user. It is built from stdin so
// the base never needs a repo checkout.
const BaseDockerfile = `FROM node:22
RUN npm install -g @anthropic-ai/claude-code @openai/codex @google/gemini-cli \
      @zed-industries/claude-code-acp @zed-industries/codex-acp \
 && git config --system --add safe.directory '*'
USER node
WORKDIR /workspace
`

// ImageForRepo decides which image a repo runs in: an explicit override wins; a
// repo with its own Dockerfile.agent gets its own tag (so a project's toolchain
// never clobbers the shared base); everything else uses the base image.
func ImageForRepo(repo, baseImage, override string) string {
	if override != "" {
		return override
	}
	if fileExists(filepath.Join(repo, "Dockerfile.agent")) {
		return ServicesProject(repo)
	}
	return baseImage
}

// ImageExists reports whether the given image is present locally.
func ImageExists(rt runtime.Runtime, image string) bool {
	return rt.Silent("image", "inspect", image)
}

// Build builds the box image: a repo with a Dockerfile.agent builds that (its
// own toolchain), otherwise the shared base is built from BaseDockerfile.
func Build(rt runtime.Runtime, cfg *config.Config, repo string) error {
	if err := rt.EnsureDaemon(); err != nil {
		return err
	}
	if fileExists(filepath.Join(repo, "Dockerfile.agent")) {
		img := ImageForRepo(repo, cfg.BaseImage, cfg.ImageOverride)
		ui.Info("building %s from Dockerfile.agent (this project's toolchain)", img)
		code, err := rt.Run(os.Stdin, os.Stdout, os.Stderr,
			"build", "-t", img, "-f", filepath.Join(repo, "Dockerfile.agent"), repo)
		return buildErr(code, err)
	}
	ui.Info("building %s (shared base)", cfg.BaseImage)
	code, err := rt.Run(strings.NewReader(BaseDockerfile), os.Stdout, os.Stderr,
		"build", "-t", cfg.BaseImage, "-")
	return buildErr(code, err)
}

func buildErr(code int, err error) error {
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("image build failed (exit %d)", code)
	}
	return nil
}
