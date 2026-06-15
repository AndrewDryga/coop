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

// BaseDockerfile is the shared base image: Node, the three agent CLIs, the ACP
// adapters, and asdf — so the box honors a repo's .tool-versions at runtime, with
// no per-project Dockerfile needed. It runs as the non-root `node` user and is
// built from stdin, so the base never needs a repo checkout.
const BaseDockerfile = `FROM node:24

ARG ASDF_VERSION=0.19.0

# Agent CLIs + ACP adapters, plus asdf and the build deps it needs to install or
# compile toolchains a repo pins in .tool-versions at runtime.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      build-essential autoconf m4 libncurses-dev libssl-dev unzip locales curl git ca-certificates \
 && sed -i '/en_US.UTF-8/s/^# //g' /etc/locale.gen && locale-gen \
 && npm install -g @anthropic-ai/claude-code @openai/codex @google/gemini-cli \
      @agentclientprotocol/claude-agent-acp @zed-industries/codex-acp \
 && curl -fsSL "https://github.com/asdf-vm/asdf/releases/download/v${ASDF_VERSION}/asdf-v${ASDF_VERSION}-linux-$(dpkg --print-architecture).tar.gz" \
      | tar -C /usr/local/bin -xzf - asdf \
 && apt-get clean && rm -rf /var/lib/apt/lists/* \
 && git config --system --add safe.directory '*' \
 && mkdir -p /home/node/.asdf && chown node:node /home/node/.asdf

# Entrypoint: install whatever a repo's .tool-versions (or ~/.tool-versions) pins
# via asdf, then run the requested command. A no-op when there is no .tool-versions.
# The first install of a toolchain can be slow (e.g. Erlang compiles), but it
# persists in the mounted ~/.asdf volume and is reused across runs and repos.
COPY <<'ENTRY' /usr/local/bin/coop-entry
#!/bin/sh
if [ -z "$COOP_NO_ASDF" ] && command -v asdf >/dev/null 2>&1; then
  f=; d=$PWD
  while :; do [ -f "$d/.tool-versions" ] && { f=$d/.tool-versions; break; }; [ "$d" = / ] && break; d=$(dirname "$d"); done
  [ -z "$f" ] && [ -f "$HOME/.tool-versions" ] && f=$HOME/.tool-versions
  if [ -n "$f" ]; then
    echo "coop: provisioning toolchain from $f (first run may compile; cached after)" >&2
    for t in $(awk 'NF && $1 !~ /^#/ {print $1}' "$f"); do
      asdf plugin list 2>/dev/null | grep -qx "$t" || asdf plugin add "$t" >&2 || true
    done
    asdf install >&2 || true
    asdf reshim >/dev/null 2>&1 || true
  fi
fi
exec "$@"
ENTRY
RUN chmod +x /usr/local/bin/coop-entry

ENV ASDF_DATA_DIR=/home/node/.asdf \
    PATH="/home/node/.asdf/shims:${PATH}" \
    LANG=en_US.UTF-8 LANGUAGE=en_US:en LC_ALL=en_US.UTF-8 \
    KERL_BUILD_DOCS=no \
    KERL_CONFIGURE_OPTIONS="--without-wx --without-observer --without-debugger --without-et --without-megaco --without-javac"

USER node
ENTRYPOINT ["/usr/local/bin/coop-entry"]
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
// own toolchain), otherwise the shared base is built from BaseDockerfile. When
// fresh is set it adds --pull --no-cache so the base image and the npm-installed
// agent CLIs + ACP adapters are pulled to their latest (this is `coop update`).
func Build(rt runtime.Runtime, cfg *config.Config, repo string, fresh bool) error {
	if err := rt.EnsureDaemon(); err != nil {
		return err
	}
	args, base := buildArgs(cfg, repo, fresh)
	if base {
		ui.Info("building %s (shared base)", cfg.BaseImage)
		return buildErr(rt.Run(strings.NewReader(BaseDockerfile), os.Stdout, os.Stderr, args...))
	}
	ui.Info("building %s from Dockerfile.agent (this project's toolchain)",
		ImageForRepo(repo, cfg.BaseImage, cfg.ImageOverride))
	return buildErr(rt.Run(os.Stdin, os.Stdout, os.Stderr, args...))
}

// buildArgs assembles the runtime build arguments for repo's image. fresh adds
// --pull --no-cache so the base image and the agent CLIs / ACP adapters refresh
// to their latest; base reports whether the shared base (BaseDockerfile via
// stdin) is built, vs the repo's own Dockerfile.agent (a build context).
func buildArgs(cfg *config.Config, repo string, fresh bool) (args []string, base bool) {
	args = []string{"build"}
	if fresh {
		args = append(args, "--pull", "--no-cache")
	}
	if fileExists(filepath.Join(repo, "Dockerfile.agent")) {
		img := ImageForRepo(repo, cfg.BaseImage, cfg.ImageOverride)
		return append(args, "-t", img, "-f", filepath.Join(repo, "Dockerfile.agent"), repo), false
	}
	return append(args, "-t", cfg.BaseImage, "-"), true
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
