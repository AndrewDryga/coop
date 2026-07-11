package box

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
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
	return fmt.Sprintf(baseDockerfileTemplate, strings.Join(agents.Packages(), " "), installLayer())
}

// installLayer renders a RUN line for each agent whose CLI installs via a script rather than
// npm (Agent.InstallScript) — run as root before USER node, after the npm layer. Empty for the
// npm-only agents, so the layer is absent unless a script-installed agent is registered.
func installLayer() string {
	var b strings.Builder
	for _, n := range agents.Names() { // sorted → a reproducible image
		if a, ok := agents.Get(n); ok {
			if s := a.InstallScript(); s != "" {
				b.WriteString("RUN " + s + "\n")
			}
		}
	}
	return b.String()
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
# (Debian ships fd as "fdfind", so it's symlinked to "fd"). python3 + pip with a bare
# "python"/"pip" (python-is-python3 plus a pip symlink) so an agent that reaches for
# python or pip just runs, instead of burning a turn self-debugging, when a repo hasn't
# pinned python in .tool-versions (an asdf-pinned python still shims ahead of these on
# PATH). Playwright's Chromium system libraries are baked in as root (the part an agent,
# running as non-root node, can't apt-get) so the bundled @playwright/mcp server — or any
# Playwright script — gets a browser that launches; the browser binary itself downloads on
# first use into the cached ~/.cache volume, and Chromium runs --no-sandbox (the box already
# IS the sandbox). ~/.asdf and ~/.cache are pre-created node-owned so their named volumes
# inherit that owner — a fresh volume on a path absent from the image would mount root-owned.
# A /etc/profile.d drop-in re-adds the asdf shims to PATH for login shells: they source
# /etc/profile, which resets PATH to the Debian default and would otherwise hide go/ruby/…
# pinned in .tool-versions (the ENV PATH below only reaches the agent process and non-login
# shells — but agents commonly shell out through a profile-sourcing login shell).
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      build-essential autoconf m4 libncurses-dev libssl-dev unzip locales curl git ca-certificates \
      postgresql-client procps inotify-tools \
      python3 python-is-python3 python3-pip \
      ripgrep fd-find jq tree \
 && sed -i '/en_US.UTF-8/s/^# //g' /etc/locale.gen && locale-gen \
 && ln -s "$(command -v fdfind)" /usr/local/bin/fd \
 && ln -s "$(command -v pip3)" /usr/local/bin/pip \
 && npm install -g ${AGENT_PACKAGES} \
 && npx -y playwright install-deps chromium \
 && curl -fsSL "https://github.com/asdf-vm/asdf/releases/download/v${ASDF_VERSION}/asdf-v${ASDF_VERSION}-linux-$(dpkg --print-architecture).tar.gz" \
      | tar -C /usr/local/bin -xzf - asdf \
 && apt-get clean && rm -rf /var/lib/apt/lists/* \
 && git config --system --add safe.directory '*' \
 && mkdir -p /home/node/.asdf /home/node/.cache && chown node:node /home/node/.asdf /home/node/.cache \
 && printf 'export PATH="/home/node/.asdf/shims:$PATH"\n' > /etc/profile.d/asdf.sh

# Entrypoint: install whatever a repo's .tool-versions (or ~/.tool-versions) pins
# via asdf, then run the requested command. A no-op when there is no .tool-versions.
# The first install of a toolchain can be slow (e.g. Erlang compiles), but it
# persists in the mounted ~/.asdf volume and is reused across runs and repos.
COPY <<'ENTRY' /usr/local/bin/coop-entry
#!/bin/sh
if command -v asdf >/dev/null 2>&1; then
  if [ -z "$COOP_NO_ASDF" ]; then
    f=; d=$PWD
    while :; do [ -f "$d/.tool-versions" ] && { f=$d/.tool-versions; break; }; [ "$d" = / ] && break; d=$(dirname "$d"); done
    [ -z "$f" ] && [ -f "$HOME/.tool-versions" ] && f=$HOME/.tool-versions
    if [ -n "$f" ]; then
      # Only provision (and say so) when a pinned tool is actually missing. Otherwise this
      # ran on every launch and printed a "provisioning" line with nothing to do — just spam.
      need=
      while read -r t v _; do
        case "$t" in ''|'#'*) continue ;; esac
        [ -d "${ASDF_DATA_DIR:-$HOME/.asdf}/installs/$t/$v" ] || { need=1; break; }
      done < "$f"
      if [ -n "$need" ]; then
        # COOP_QUIET (set by coop acp) provisions silently: ACP's consumer is an editor over
        # stdio, not a human. Otherwise narrate with a dimmed coop: prefix (matching ui).
        log=/dev/stderr
        if [ -n "$COOP_QUIET" ]; then
          log=/dev/null
        else
          if [ -t 2 ]; then d=$(printf '\033[2m'); r=$(printf '\033[0m'); else d=; r=; fi
          echo "${d}coop:${r} provisioning toolchain from $f (first run may compile; cached after)" >&2
        fi
        for t in $(awk 'NF && $1 !~ /^#/ {print $1}' "$f"); do
          asdf plugin list 2>/dev/null | grep -qx "$t" || asdf plugin add "$t" >"$log" 2>&1 || true
        done
        asdf install >"$log" 2>&1 || true
      fi
      asdf reshim >/dev/null 2>&1 || true
    fi
  fi
  # The agent CLIs are Node apps, so a bare node must always resolve. A prior repo's
  # nodejs pin leaves a node shim in the persisted ~/.asdf volume; in a repo that does not
  # pin nodejs (and with no global) that shim shadows the image node and errors with
  # "No version is set for command node". COOP_NO_ASDF skips provisioning, not this repair.
  # If node is broken but asdf has a nodejs installed, set the newest as the global fallback
  # -- a repo's own .tool-versions still overrides it, so a pinned project node keeps winning.
  if ! node --version >/dev/null 2>&1; then
    v=$(asdf list nodejs 2>/dev/null | tr -cd '0-9.\n ' | tr ' ' '\n' | grep . | sort -V | tail -n1)
    [ -n "$v" ] && asdf set --home nodejs "$v" >/dev/null 2>&1 && asdf reshim nodejs >/dev/null 2>&1
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

# Script-installed agent CLIs (Agent.InstallScript) — run as root, after the npm layer. Empty
# for the npm-only agents, so this expands to nothing unless such an agent is registered.
%s
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
// version is the building coop's version, stamped beside the image so a later
// launch can flag binary/image skew (box can't resolve it itself — cli owns it).
func Build(rt runtime.Runtime, cfg *config.Config, repo string, fresh bool, version string) error {
	if err := rt.EnsureDaemon(); err != nil {
		return err
	}
	if !fileExists(filepath.Join(repo, "Dockerfile.agent")) {
		ui.Info("building %s (shared base)", cfg.BaseImage)
		err := buildErr(rt.Run(strings.NewReader(BaseDockerfile()), os.Stdout, os.Stderr, baseBuildArgs(cfg, fresh)...))
		if err == nil {
			StampImageMeta(cfg, cfg.BaseImage, version) // record builder + definition so a later run can flag skew/age
		}
		return err
	}
	img := ImageForRepo(repo, cfg.BaseImage, cfg.ImageOverride)
	// Dockerfile.agent defines the box's next sandbox (its USER/RUN/ENTRYPOINT), and an agent with
	// write access to the repo can author one. The build is always an explicit human action, but an
	// untracked box definition is exactly the agent-authored case — surface it so a moved/planted
	// file isn't built silently. Cheap visibility, not a gate.
	if fileUntracked(repo, "Dockerfile.agent") {
		ui.Info("note: Dockerfile.agent is untracked in git — it defines this box, and an agent can author one; review it before building")
	}
	ui.Info("building %s from Dockerfile.agent (this project's toolchain)", img)
	// Build from a shadow-filtered COPY of the repo, not the repo itself: secret shadowing is a
	// run-time -v overlay, so without this a `COPY .env /` / `COPY . .` in an agent-authored
	// Dockerfile.agent would bake a shadowed secret into a persistent (pushable) image layer. The
	// staged context omits every shadowed path (and .git), so a targeted COPY of a secret fails the
	// build loudly instead of leaking silently.
	ctx, cleanup, err := stageBuildContext(repo)
	if err != nil {
		return fmt.Errorf("staging the build context: %w", err)
	}
	defer cleanup()
	args := []string{"build"}
	if fresh {
		args = append(args, "--pull", "--no-cache")
	}
	args = append(args, "-t", img, "-f", filepath.Join(ctx, "Dockerfile.agent"), ctx)
	err = buildErr(rt.Run(os.Stdin, os.Stdout, os.Stderr, args...))
	if err == nil {
		StampImageInputs(cfg, repo, img) // record inputs so a later run can flag drift
	}
	return err
}

// baseBuildArgs assembles the runtime args for building the shared base image (BaseDockerfile via
// stdin). fresh adds --pull --no-cache so the base image and the agent CLIs / ACP adapters refresh
// to their latest; otherwise the FROM image is pinned so `coop build` is reproducible. Tool
// versions stay latest unless pinned via COOP_AGENT_PACKAGES.
func baseBuildArgs(cfg *config.Config, fresh bool) []string {
	args := []string{"build"}
	if fresh {
		args = append(args, "--pull", "--no-cache")
	}
	node := pinnedNodeImage
	if fresh {
		node = floatingNodeImage
	}
	args = append(args, "--build-arg", "NODE_IMAGE="+node)
	if cfg.AgentPackages != "" {
		args = append(args, "--build-arg", "AGENT_PACKAGES="+cfg.AgentPackages)
	}
	return append(args, "-t", cfg.BaseImage, "-")
}

// stageBuildContext copies repo into a throwaway dir, OMITTING every shadowed path (NewShadowDecider
// — the same denylist that hides secrets from a run) and .git, so a Dockerfile.agent build can't bake
// an in-repo secret into an image layer. Returns the staged dir and a cleanup func. A non-secret
// COPY still works (the file is present); a COPY of a shadowed file fails (it's absent), which is the
// intended loud failure rather than a silent leak.
func stageBuildContext(repo string) (string, func(), error) {
	shadowed := NewShadowDecider(repo)
	ctx, err := os.MkdirTemp("", "coop-buildctx-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(ctx) }
	err = filepath.WalkDir(repo, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(repo, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		slash := filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == ".git" || shadowed(slash) {
				return fs.SkipDir // .git isn't needed in a build context; a shadowed dir must not leak
			}
			return os.MkdirAll(filepath.Join(ctx, rel), 0o755)
		}
		if shadowed(slash) {
			return nil // omit the secret — a COPY of it then fails the build instead of baking it
		}
		return copyForBuild(p, filepath.Join(ctx, rel))
	})
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	return ctx, cleanup, nil
}

// copyForBuild copies one entry into the staged context, preserving symlinks (as links, so a link
// to an omitted secret is left dangling, not resolved) and skipping irregular files.
func copyForBuild(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}
	if !fi.Mode().IsRegular() {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
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

// fileUntracked reports whether repo is a git repo in which name (a repo-relative path) is NOT
// tracked (committed or staged) — the agent-authored case worth surfacing for files coop then
// builds or auto-runs (Dockerfile.agent, .agent/compose.yml). It uses read-only `ls-files`
// (hardened: no fsmonitor/hooks fire on the agent-writable repo) and returns false for a non-git
// repo, where "untracked" isn't a meaningful signal.
func fileUntracked(repo, name string) bool {
	if exec.Command("git", "-C", repo, "rev-parse", "--git-dir").Run() != nil {
		return false // not a git repo — nothing to compare against
	}
	tracked := exec.Command("git", "-C", repo, "-c", "core.fsmonitor=", "-c", "core.hooksPath=/dev/null",
		"ls-files", "--error-unmatch", "--", name).Run()
	return tracked != nil // non-zero exit → the file isn't tracked
}
