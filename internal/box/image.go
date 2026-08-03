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
	"github.com/AndrewDryga/coop/internal/project"
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
	pinnedNodeImage   = "node:24-slim@sha256:cb4e8f7c443347358b7875e717c29e27bf9befc8f5a26cf18af3c3dec80e58c5" // node 24 (slim)
	floatingNodeImage = "node:24-slim"
	pinnedGoImage     = "golang:1.26.5-bookworm@sha256:18aedc16aa19b3fd7ded7245fc14b109e054d65d22ed53c355c899582bbb2113"
	floatingGoImage   = "golang:1.26.5-bookworm"
)

// baseDockerfileTemplate is BaseDockerfile with %s for the npm package list. The
// FROM images (NODE_IMAGE and GO_IMAGE) and the agent npm specs (AGENT_PACKAGES) are
// build args so a build can pin them; the defaults preserve the floating behavior for
// a raw build.
const baseDockerfileTemplate = `ARG NODE_IMAGE=node:24-slim
ARG GO_IMAGE=golang:1.26.5-bookworm

FROM ${GO_IMAGE} AS staticcheck-builder
ARG STATICCHECK_VERSION=v0.7.0
RUN GOBIN=/out CGO_ENABLED=0 go install honnef.co/go/tools/cmd/staticcheck@${STATICCHECK_VERSION}

FROM ${NODE_IMAGE}

COPY --from=staticcheck-builder /out/staticcheck /usr/local/bin/staticcheck

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
      postgresql-client procps inotify-tools util-linux socat \
      python3 python-is-python3 python3-pip \
      ripgrep fd-find jq tree \
 && sed -i '/en_US.UTF-8/s/^# //g' /etc/locale.gen && locale-gen \
 && command -v flock >/dev/null \
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
# Sidecar forwarders: for each COOP_FORWARD entry "<hostport>:<service>:<containerport>", listen on
# the box's own 127.0.0.1:<hostport> and forward raw TCP to <service>:<containerport> on the compose
# network. Raw TCP passes TLS through untouched, so the app in the box reaches a sidecar at the SAME
# localhost:<hostport> URL the host browser uses (OIDC issuer match).
proc_start_token() {
  IFS= read -r stat < "/proc/$1/stat" || return
  fields=${stat##*) }
  [ "$fields" = "$stat" ] && return
  set -- $fields
  [ "$#" -ge 20 ] && echo "${20}"
} 2>/dev/null

forward_sessions=
if [ -n "$COOP_FORWARD" ] && command -v socat >/dev/null 2>&1; then
  oldifs=$IFS; IFS=,
  for forward in $COOP_FORWARD; do
    IFS=$oldifs; hp=${forward%%:*}; rest=${forward#*:}; svc=${rest%%:*}; sp=${rest#*:}
    [ -n "$hp" ] && [ -n "$svc" ] && [ -n "$sp" ] || { IFS=,; continue; }
    # Each forwarder gets its own session. Supervision authenticates an exemption with this
    # session leader's PID *and* Linux start token, never with a spoofable executable name.
    setsid socat "TCP-LISTEN:$hp,bind=127.0.0.1,fork,reuseaddr" "TCP:$svc:$sp" >/dev/null 2>&1 &
    fp=$!
    token=$(proc_start_token "$fp")
    [ -n "$token" ] && forward_sessions="$forward_sessions $fp:$token"
    IFS=,
  done
  IFS=$oldifs
fi

[ "$COOP_SUPERVISE_DESCENDANTS" = 1 ] || exec "$@"

# Reserved results are returned only by this supervisor. A provider that exits with either code
# is deliberately mapped to a normal failure so it cannot forge a host handoff.
coop_drained_exit=190
coop_timeout_exit=191

# A live process is agent-owned unless it is PID 1, this entrypoint, or belongs to a forwarder
# session whose leader still has its recorded start token. This scans /proc directly: PPID and
# provider-session walks lose double-setsid orphans, while names are provider-controlled.
live_jobs() {
  for p in /proc/[0-9]*; do
    pid=${p##*/}
    # /proc/<pid>/stat's comm field is parenthesized and may itself contain spaces. Strip it
    # before splitting: the remaining fields start at state (3), so session is $4 and start $20.
    IFS= read -r stat < "$p/stat" || continue
    fields=${stat##*) }
    [ "$fields" != "$stat" ] || continue
    set -- $fields
    [ "$#" -ge 20 ] || continue
    state=$1; parent=$2; session=$4
    [ "$state" = Z ] && continue
    # PID 1 is the container init; this entrypoint and its transient direct children are not
    # provider work. Everything else remains visible even when it reparented or called setsid.
    [ "$pid" = 1 ] || [ "$pid" = "$$" ] || [ "$parent" = "$$" ] && continue
    exempt=
    for record in $forward_sessions; do
      leader=${record%%:*}; token=${record#*:}
      current=
      if IFS= read -r leader_stat < "/proc/$leader/stat"; then
        leader_fields=${leader_stat##*) }
        [ "$leader_fields" = "$leader_stat" ] || { set -- $leader_fields; [ "$#" -ge 20 ] && current=${20}; }
      fi
      [ "$session" = "$leader" ] && [ "$current" = "$token" ] && exempt=1
    done
    [ -n "$exempt" ] || echo "$pid"
  done
} 2>/dev/null

# Distinct command names behind a PID list, so a drain notice says "chromium-headle" rather than a
# row of numbers the operator would have to exec into the box to resolve.
job_names() {
  names=
  for pid in $1; do
    IFS= read -r comm < "/proc/$pid/comm" || continue
    case " $names " in *" $comm "*) ;; *) names="$names $comm" ;; esac
  done
  # echo, not printf: a literal format verb here would look like an unresolved Dockerfile
  # template placeholder. Command substitution strips the trailing newline, so the notice stays
  # on one line.
  if [ -n "$names" ]; then echo "${names# }"; else echo unknown; fi
} 2>/dev/null

terminate_jobs() {
  jobs=$1
  [ -n "$jobs" ] || return
  for pid in $jobs; do kill -TERM "$pid" 2>/dev/null || true; done
  sleep 2
  for pid in $(live_jobs); do kill -KILL "$pid" 2>/dev/null || true; done
}

# coop's OWN detached consult, marked with COOP_CONSULT_OWNED=1 by internal/fusion/wrapper.go
# before it detaches (children inherit the environment).
owned_jobs() {
  for pid in $1; do
    tr '\0' '\n' < "/proc/$pid/environ" 2>/dev/null | grep -q "^COOP_CONSULT_OWNED=1$" && echo "$pid"
  done
} 2>/dev/null

# Reap ONLY the listed pids. terminate_jobs deliberately KILLs everything still live after its
# grace period, which is right at shutdown but wrong here — genuine agent background work must
# keep its full drain window.
reap_jobs() {
  [ -n "$1" ] || return
  for pid in $1; do kill -TERM "$pid" 2>/dev/null || true; done
  sleep 1
  for pid in $1; do kill -KILL "$pid" 2>/dev/null || true; done
}

# Drop the second list from the first. A pid that was just reaped can linger for a moment while it
# dies, and its /proc entry no longer carries the ownership marker — so without this it would be
# miscounted as agent background work and earn a handoff exit.
without_pids() {
  keep=
  for pid in $1; do
    skip=
    for gone in $2; do
      [ "$pid" = "$gone" ] && { skip=1; break; }
    done
    [ -n "$skip" ] || keep="$keep $pid"
  done
  echo "${keep# }"
}

provider_exit=0
setsid "$@" & provider=$!
trap 'kill -TERM -- -$provider 2>/dev/null || true; wait "$provider" 2>/dev/null; terminate_jobs "$(live_jobs)"; exit 143' INT TERM HUP
wait "$provider" || provider_exit=$?
if [ "$provider_exit" -ne 0 ]; then
  [ "$provider_exit" -eq "$coop_drained_exit" ] && provider_exit=1
  [ "$provider_exit" -eq "$coop_timeout_exit" ] && provider_exit=1
  terminate_jobs "$(live_jobs)"
  exit "$provider_exit"
fi

# A stranded consult is coop's own work, not the agent's, and it is worthless the moment the
# provider that asked the question exits — nothing can read the reply any more. Reap it here, so
# it costs neither the drain window nor a background handoff (which un-completes a finished task
# and re-runs it). This replaces the old timing contract "consult timeout < drain < watchdog idle":
# both outer bounds are now unlimited by default, so ordering can no longer be relied on. Ownership
# is a fact; a deadline was only ever a guess.
# The reap itself runs inside the scan loop below, not once here: a double-setsid child can be
# invisible on the first scan (the same race quiescence_rescan exists for), so a one-shot sweep
# misses exactly the consult it is meant to catch.
#
# The remaining wait is for GENUINE agent background work. It must stay well under the host
# watchdog's provider idle deadline when one is configured (providerIdleDeadline in
# internal/cli/watchdog.go; 0/disabled by default). The drain emits no stream events, so both
# clocks would run from the provider's last activity: at equal values the watchdog wins the race,
# the box is reported as a wedged provider instead of a descendant handoff, and the drain's own
# exit codes become unreachable. Tests and operators may shorten it at the container boundary;
# invalid values fail closed to the same bounded default.
handoff_wait=${COOP_DESCENDANT_TIMEOUT:-900}
case "$handoff_wait" in ''|*[!0-9]*) handoff_wait=900;; esac
IFS=. read -r now _ < /proc/uptime
deadline=$(( now + handoff_wait ))
saw_live_job=
quiescence_rescan=
reaped_consult=
reaped_pids=
while :; do
  jobs=$(live_jobs)
  # coop's own stranded consult is reaped on sight and never counts as background work: its reply
  # died with the provider that asked for it, so waiting the window and then reporting a handoff
  # would un-complete a finished task for nothing.
  owned=$(owned_jobs "$jobs")
  if [ -n "$owned" ]; then
    [ -n "$reaped_consult" ] || echo "coop: consult(s) outlived the provider that asked — their reply can no longer be read; reaping: $(job_names "$owned")" >&2
    reaped_consult=1
    reap_jobs "$owned"
    reaped_pids="$reaped_pids $owned"
  fi
  jobs=$(without_pids "$jobs" "$reaped_pids")
  if [ -n "$jobs" ]; then
    # Announce the wait ONCE. Without this the box is silent for the whole window, which reads as a
    # hung loop rather than a drain, and the operator cannot tell what is holding it open.
    [ -n "$saw_live_job" ] || echo "coop: provider exited with background work still live — waiting up to ${handoff_wait}s for: $(job_names "$jobs")" >&2
    saw_live_job=1
    IFS=. read -r now _ < /proc/uptime
    [ "$now" -lt "$deadline" ] || break
    sleep 1
    continue
  fi
  [ -n "$saw_live_job" ] && break
  # A double-setsid child can be between its short-lived launch process and its reparented
  # session leader during the first scan. Rescan once before treating a clean provider exit as
  # quiescent, while keeping the ordinary success path bounded.
  [ -n "$quiescence_rescan" ] && break
  quiescence_rescan=1
  IFS=. read -r now _ < /proc/uptime
  [ "$now" -lt "$deadline" ] || break
  sleep 0.1
done
[ -n "$saw_live_job" ] || exit 0
[ -z "$jobs" ] && exit "$coop_drained_exit"
echo "coop: background work did not exit within ${handoff_wait}s — terminating: $(job_names "$jobs")" >&2
terminate_jobs "$jobs"
exit "$coop_timeout_exit"
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
// repo with its own box Dockerfile gets its own tag (so a project's toolchain
// never clobbers the shared base); everything else uses the base image.
func ImageForRepo(repo, baseImage, override string) string {
	if override != "" {
		return override
	}
	if fileExists(filepath.Join(repo, project.DockerfilePath(repo))) {
		return ServicesProject(repo)
	}
	return baseImage
}

// ImageExists reports whether the given image is present locally.
func ImageExists(rt runtime.Runtime, image string) bool {
	return rt.Silent("image", "inspect", image)
}

// Build builds the box image: a repo with a .agent/Dockerfile builds that (its
// own toolchain), otherwise the shared base is built from BaseDockerfile. When
// fresh is set it adds --pull --no-cache so the base image and the npm-installed
// agent CLIs + ACP adapters are pulled to their latest (this is `coop update`).
// version is the building coop's version, stamped beside the image so a later
// launch can flag binary/image skew (box can't resolve it itself — cli owns it).
func Build(rt runtime.Runtime, cfg *config.Config, repo string, fresh bool, version string) error {
	if err := rt.EnsureDaemon(); err != nil {
		return err
	}
	// Load once so a malformed project.yaml fails the build loudly, and to resolve box.dockerfile.
	proj, err := project.Load(repo)
	if err != nil {
		return err
	}
	dfRel := proj.DockerfileRel() // box.dockerfile, else .agent/Dockerfile
	if !fileExists(filepath.Join(repo, dfRel)) {
		ui.Info("building %s (shared base)", cfg.BaseImage)
		err := buildErr(rt.Run(strings.NewReader(BaseDockerfile()), os.Stdout, os.Stderr, baseBuildArgs(cfg, fresh)...))
		if err == nil {
			StampImageMeta(cfg, cfg.BaseImage, version) // record builder + definition so a later run can flag skew/age
		}
		return err
	}
	img := ImageForRepo(repo, cfg.BaseImage, cfg.ImageOverride)
	// The box Dockerfile defines the box's next sandbox (its USER/RUN/ENTRYPOINT), and an agent with
	// write access to the repo can author one. The build is always an explicit human action, but an
	// untracked box definition is exactly the agent-authored case — surface it so a moved/planted
	// file isn't built silently. Cheap visibility, not a gate.
	if fileUntracked(repo, dfRel) {
		ui.Info("note: %s is untracked in git — it defines this box, and an agent can author one; review it before building", dfRel)
	}
	ui.Info("building %s from %s (this project's toolchain)", img, dfRel)
	// A project Dockerfile may inherit coop's trusted base (agent CLIs + ACP adapters, browser
	// libraries, writable-home + security setup) with `ARG COOP_BASE_IMAGE` / `FROM ${COOP_BASE_IMAGE}`,
	// adding only its own toolchain. When it does, make sure the base is present (build it if missing,
	// rebuild it on --fresh) and pass it in as a build-arg.
	content, _ := os.ReadFile(filepath.Join(repo, dfRel))
	usesBase := strings.Contains(string(content), "COOP_BASE_IMAGE")
	if usesBase && (fresh || !ImageExists(rt, cfg.BaseImage)) {
		ui.Info("building %s (shared base) first — %s inherits it via COOP_BASE_IMAGE", cfg.BaseImage, dfRel)
		if err := buildErr(rt.Run(strings.NewReader(BaseDockerfile()), os.Stdout, os.Stderr, baseBuildArgs(cfg, fresh)...)); err != nil {
			return err
		}
		StampImageMeta(cfg, cfg.BaseImage, version)
	}
	// Build from a shadow-filtered COPY of the repo, not the repo itself: secret shadowing is a
	// run-time -v overlay, so without this a `COPY .env /` / `COPY . .` in an agent-authored
	// .agent/Dockerfile would bake a shadowed secret into a persistent (pushable) image layer. The
	// staged context omits every shadowed path (and .git), so a targeted COPY of a secret fails the
	// build loudly instead of leaking silently.
	ctx, cleanup, err := stageBuildContext(repo)
	if err != nil {
		return fmt.Errorf("staging the build context: %w", err)
	}
	defer cleanup()
	err = buildErr(rt.Run(os.Stdin, os.Stdout, os.Stderr, projectBuildArgs(ctx, dfRel, img, cfg.BaseImage, usesBase, fresh)...))
	if err == nil {
		StampImageInputs(cfg, repo, img) // record inputs so a later run can flag drift
	}
	return err
}

// projectBuildArgs assembles the `<runtime> build` args for a project Dockerfile at dfRel inside the
// staged ctx, tagged img. When the Dockerfile inherits coop's base (usesBase), it passes the base as
// a build-arg and, on --fresh, uses --no-cache WITHOUT --pull — the base is a local image tag, not a
// registry ref, so --pull would fail trying to fetch it. An external FROM still gets --pull on fresh.
func projectBuildArgs(ctx, dfRel, img, baseImage string, usesBase, fresh bool) []string {
	args := []string{"build"}
	if fresh {
		args = append(args, "--no-cache")
		if !usesBase {
			args = append(args, "--pull")
		}
	}
	if usesBase {
		args = append(args, "--build-arg", "COOP_BASE_IMAGE="+baseImage)
	}
	return append(args, "-t", img, "-f", filepath.Join(ctx, dfRel), ctx)
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
	goImage := pinnedGoImage
	if fresh {
		node = floatingNodeImage
		goImage = floatingGoImage
	}
	args = append(args,
		"--build-arg", "NODE_IMAGE="+node,
		"--build-arg", "GO_IMAGE="+goImage,
	)
	if cfg.AgentPackages != "" {
		args = append(args, "--build-arg", "AGENT_PACKAGES="+cfg.AgentPackages)
	}
	return append(args, "-t", cfg.BaseImage, "-")
}

// stageBuildContext copies repo into a throwaway dir, OMITTING every shadowed path (NewShadowDecider
// — the same denylist that hides secrets from a run) and .git, so a .agent/Dockerfile build can't bake
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
// builds or auto-runs (.agent/Dockerfile, .agent/compose.yml). It uses read-only `ls-files`
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
