package box

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// BaseDockerfile is the shared base image: Node, the agent CLIs + ACP adapters (each
// agent names its own npm packages), and asdf — so the box honors a repo's
// .tool-versions at runtime, with no per-project Dockerfile needed. It runs as the
// non-root `node` user and is built from stdin, so the base never needs a checkout.
func BaseDockerfile() string {
	return fmt.Sprintf(baseDockerfileTemplate, strings.Join(agents.Packages(), " "))
}

// Base-image references for the shared box. coop build pins the FROM image to a
// digest for a reproducible box; coop update (fresh) floats it to the tag so --pull
// fetches the newest. Bump pinnedNodeImage when you intentionally move the stable
// base (e.g. after a `coop update` proves a newer node works).
const (
	pinnedNodeImage   = "node:24@sha256:40ad9f3064e67d6860b4bc3fe1880b2953934fd6320ada990e45fe0efa6badd7" // node v24.16.0
	floatingNodeImage = "node:24"
)

// baseDockerfileTemplate is BaseDockerfile with %s for the npm package list. The
// FROM image (NODE_IMAGE) and the agent npm specs (AGENT_PACKAGES) are build args so
// a build can pin them; the defaults preserve the floating behavior for a raw build.
const baseDockerfileTemplate = `ARG NODE_IMAGE=node:24
FROM ${NODE_IMAGE}

ARG ASDF_VERSION=0.19.0
ARG AGENT_PACKAGES="%s"

# Agent CLIs + ACP adapters, plus asdf and the build deps it needs to install or
# compile toolchains a repo pins in .tool-versions at runtime. A Postgres client,
# procps, and inotify-tools come along so the runtime path matches a baked image.
# ripgrep/fd/jq/tree are the search + inspect tools agents reach for constantly
# (Debian ships fd as "fdfind", so it's symlinked to "fd"). ~/.asdf and ~/.cache are
# pre-created node-owned so their named volumes inherit that owner — a fresh volume on
# a path absent from the image would mount root-owned.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      build-essential autoconf m4 libncurses-dev libssl-dev unzip locales curl git ca-certificates \
      postgresql-client procps inotify-tools \
      ripgrep fd-find jq tree \
 && sed -i '/en_US.UTF-8/s/^# //g' /etc/locale.gen && locale-gen \
 && ln -s "$(command -v fdfind)" /usr/local/bin/fd \
 && npm install -g ${AGENT_PACKAGES} \
 && curl -fsSL "https://github.com/asdf-vm/asdf/releases/download/v${ASDF_VERSION}/asdf-v${ASDF_VERSION}-linux-$(dpkg --print-architecture).tar.gz" \
      | tar -C /usr/local/bin -xzf - asdf \
 && apt-get clean && rm -rf /var/lib/apt/lists/* \
 && git config --system --add safe.directory '*' \
 && mkdir -p /home/node/.asdf /home/node/.cache && chown node:node /home/node/.asdf /home/node/.cache

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
		return buildErr(rt.Run(strings.NewReader(BaseDockerfile()), os.Stdout, os.Stderr, args...))
	}
	img := ImageForRepo(repo, cfg.BaseImage, cfg.ImageOverride)
	ui.Info("building %s from Dockerfile.agent (this project's toolchain)", img)
	err := buildErr(rt.Run(os.Stdin, os.Stdout, os.Stderr, args...))
	if err == nil {
		StampImageInputs(cfg, repo, img) // record inputs so a later run can flag drift
	}
	return err
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
	// Shared base: pin the FROM image so `coop build` is reproducible; `coop update`
	// (fresh) floats it so --pull fetches the newest node:24. Tool versions stay latest
	// unless pinned via COOP_AGENT_PACKAGES.
	node := pinnedNodeImage
	if fresh {
		node = floatingNodeImage
	}
	args = append(args, "--build-arg", "NODE_IMAGE="+node)
	if cfg.AgentPackages != "" {
		args = append(args, "--build-arg", "AGENT_PACKAGES="+cfg.AgentPackages)
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
