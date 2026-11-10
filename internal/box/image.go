package box

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/gatewayimage"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// baseImageDefinition is the shared base image's build context for one platform: Node, the
// qualified clients — the closure the filtered image installs, from the same embedded lock — and
// asdf, which a project's toolchain image (toolchain_image.go) installs its .tool-versions with. It runs
// as the non-root `node` user and builds from these embedded files alone, so the base never needs a
// checkout. The launchers lead PATH, ahead of the asdf shims.
func baseImageDefinition(platform agents.ClientPlatform) (map[string][]byte, error) {
	closure, err := agents.LockedClientClosure(platform)
	if err != nil {
		return nil, err
	}
	parts := lockedClientParts(closure)
	parts.loginPath = `printf 'export PATH="` + agents.LauncherDir + `:/home/node/.asdf/shims:$PATH"\n' > /etc/profile.d/asdf.sh`
	parts.pathEnv = agents.LauncherDir + `:/home/node/.asdf/shims:${PATH}`
	closure.Files["Dockerfile"] = []byte(renderBaseDockerfile(platform.Architecture, parts))
	return closure.Files, nil
}

// BaseDockerfile is the shared base's Dockerfile as a build for linux/<arch> renders it — for code
// that reads its fixed parts, such as the entrypoint's defaults.
func BaseDockerfile(arch string) (string, error) {
	files, err := baseImageDefinition(agents.ClientPlatform{OS: "linux", Architecture: arch, Libc: "glibc"})
	if err != nil {
		return "", err
	}
	return string(files["Dockerfile"]), nil
}

type baseImageParts struct{ files, install, browserDeps, loginPath, pathEnv, scripts string }

func renderBaseDockerfile(arch string, parts baseImageParts) string {
	return fmt.Sprintf(baseDockerfileTemplate, gatewayimage.GoBase(), parts.files, parts.install, parts.browserDeps, asdfInstall(arch), parts.loginPath, npmInstall(), configPublicationShell, parts.pathEnv, parts.scripts)
}

// asdf comes from a release URL, and both images carry it: toolchain and project images built on
// the base, or on the filtered client image, install their pins with it. So, like a
// native client (lockedClientParts), its tarball must match the digest pinned for the build's
// architecture before it is unpacked. A bump moves the version and both digests together: hash each
// linux tarball of the release and match GitHub's recorded asset digest (the release itself
// publishes only .md5 files).
const asdfVersion = "0.20.2"

var asdfSHA256 = map[string]string{
	"amd64": "6d45e6fab12f50a259b5643adf06a2e77af30a87300b4402440eda09af467a6d",
	"arm64": "356e135a608bdfba34038c1f18c4b82434d7b6d546e30dbf372c61baaa2f442c",
}

// asdfInstall is the build step that downloads asdf for arch, checks it, and only then unpacks it.
func asdfInstall(arch string) string {
	url := "https://github.com/asdf-vm/asdf/releases/download/v" + asdfVersion + "/asdf-v" + asdfVersion + "-linux-" + arch + ".tar.gz"
	return fmt.Sprintf("curl --fail --silent --show-error --location --proto '=https' --output /tmp/coop-asdf.tar.gz '%s' \\\n"+
		" && printf '%%s  %%s\\n' '%s' /tmp/coop-asdf.tar.gz | sha256sum -c - \\\n"+
		" && tar -C /usr/local/bin -xzf /tmp/coop-asdf.tar.gz asdf \\\n"+
		" && rm -f /tmp/coop-asdf.tar.gz", url, asdfSHA256[arch])
}

// npm's complete distribution includes its runtime dependency tree. Replace the
// inherited directory after verification so obsolete bundled packages cannot survive.
const imageNPMVersion = "11.21.2"
const imageNPMSHA256 = "8ad8563b81e71b90ef405b140bccf3ba9b5e7884ab210bd49ee4e2f4bc6f61f5"

func npmInstall() string {
	return fmt.Sprintf(`curl --fail --silent --show-error --location --proto '=https' --output /tmp/coop-npm.tgz 'https://registry.npmjs.org/npm/-/npm-%s.tgz' \
 && printf '%%s  %%s\n' '%s' /tmp/coop-npm.tgz | sha256sum -c - \
 && rm -rf /usr/local/lib/node_modules/npm \
 && mkdir -p /usr/local/lib/node_modules/npm \
 && tar -C /usr/local/lib/node_modules/npm --strip-components=1 -xzf /tmp/coop-npm.tgz \
 && rm -f /tmp/coop-npm.tgz`, imageNPMVersion, imageNPMSHA256)
}

// Base-image references for the shared box. coop build pins the FROM image to a
// digest for a reproducible box; coop update (fresh) floats it to the tag so --pull
// fetches the newest. Bump pinnedNodeImage when you intentionally move the stable
// base (e.g. after a `coop update` proves a newer node works).
const (
	pinnedNodeImage   = "node:24-bookworm-slim@sha256:01d566e8d94d8c619591d78652babfccd3c6992a3dffa9ed5ab39e11cf6c70e9" // node 24 (slim)
	floatingNodeImage = "node:24-bookworm-slim"
)

// baseDockerfileTemplate is shared by the base and the filtered client image: OS tools, the
// qualified clients and process supervision. Its slots carry the client installation and each
// image's PATH.
const baseDockerfileTemplate = `ARG NODE_IMAGE=node:24-bookworm-slim
FROM %s AS supervisor
COPY supervisor/main.go /src/main.go
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /coop-supervise /src/main.go

FROM ${NODE_IMAGE}
COPY --from=supervisor /coop-supervise /usr/local/libexec/coop-supervise

%s

# Agent CLIs + ACP adapters, plus asdf and the build deps it needs to install or
# compile toolchains a repo pins in .tool-versions, in the image built on this one. A Postgres client,
# procps, and inotify-tools come along so the runtime path matches a baked image.
# ripgrep/fd/jq/tree are the search + inspect tools agents reach for constantly
# (Debian ships fd as "fdfind", so it's symlinked to "fd"). ShellCheck rides along because a
# repo's gate runs IN the box, and a non-root agent can't apt-get a missing linter at gate time
# (a project's own .agent/Dockerfile adds its language's analyzers). python3 + pip with a bare
# "python"/"pip" (python-is-python3 plus a pip symlink) so an agent that reaches for
# python or pip just runs, instead of burning a turn self-debugging, when a repo hasn't
# pinned python in .tool-versions (an asdf-pinned python still shims ahead of these on
# PATH). Playwright's Chromium system libraries are baked in as root (the part an agent,
# running as non-root node, can't apt-get) so the bundled @playwright/mcp server — or any
# Playwright script — gets a browser that launches; the browser binary itself downloads on
# first use into the cached ~/.cache volume, and Chromium runs --no-sandbox (the box already
# IS the sandbox). ~/.asdf and ~/.cache are pre-created node-owned: images built on this one install
# into ~/.asdf as node, and each project's ~/.cache volume inherits that owner — a fresh volume on
# a path absent from the image would mount root-owned.
# A /etc/profile.d drop-in re-adds the asdf shims to PATH for login shells: they source
# /etc/profile, which resets PATH to the Debian default and would otherwise hide go/ruby/…
# pinned in .tool-versions (the ENV PATH below only reaches the agent process and non-login
# shells — but agents commonly shell out through a profile-sourcing login shell).
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      build-essential autoconf m4 libncurses-dev libssl-dev unzip locales curl git git-lfs ca-certificates \
      postgresql-client procps inotify-tools util-linux socat \
      python3 python-is-python3 python3-pip \
      ripgrep fd-find jq tree shellcheck \
 && sed -i '/en_US.UTF-8/s/^# //g' /etc/locale.gen && locale-gen \
 && command -v flock >/dev/null \
 && ln -s "$(command -v fdfind)" /usr/local/bin/fd \
 && ln -s "$(command -v pip3)" /usr/local/bin/pip \
 && %s \
 && %s \
 && %s \
 && apt-get clean && rm -rf /var/lib/apt/lists/* \
 && git config --system --add safe.directory '*' \
 && git lfs install --system --skip-repo \
 && mkdir -p /home/node/.asdf /home/node/.cache && chown node:node /home/node/.asdf /home/node/.cache \
 && %s

RUN %s

# Entrypoint: shared process supervision.
COPY <<'ENTRY' /usr/local/bin/coop-entry
#!/bin/sh
%s# Sidecar forwarders: for each COOP_FORWARD entry "<hostport>:<service>:<containerport>", listen on
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
    IFS=$oldifs; hp=${forward%%%%:*}; rest=${forward#*:}; svc=${rest%%%%:*}; sp=${rest#*:}
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

# The PATH the box started with, for coop-consult and coop-delegate to hand their arms: a lead's
# client prepends its own tool directories to every command it runs (Codex its vendored rg), and an
# arm is another client, not the lead's shell.
COOP_BOX_PATH=$PATH
export COOP_BOX_PATH

[ "$COOP_SUPERVISE_DESCENDANTS" = 1 ] || exec "$@"

# Pass only the identities captured above, never an inherited exemption variable.
exec /usr/local/libexec/coop-supervise "$forward_sessions" "$@"
ENTRY
RUN chmod +x /usr/local/bin/coop-entry

ENV ASDF_DATA_DIR=/home/node/.asdf \
    PATH="%s" \
    LANG=en_US.UTF-8 LANGUAGE=en_US:en LC_ALL=en_US.UTF-8 \
    KERL_BUILD_DOCS=no \
    KERL_CONFIGURE_OPTIONS="--without-wx --without-observer --without-debugger --without-et --without-megaco --without-javac"

# The clients' launchers, the checks that each runs what the lock installed, and every client's
# update controls — as root, before the image drops to node.
%s
ARG COOP_BOX_UID=1000
ARG COOP_BOX_GID=1000
RUN if [ "$COOP_BOX_UID:$COOP_BOX_GID" != "1000:1000" ]; then \
      if ! getent group "$COOP_BOX_GID" >/dev/null; then groupmod -g "$COOP_BOX_GID" node; fi \
   && usermod -u "$COOP_BOX_UID" -g "$COOP_BOX_GID" node \
   && chown -R "$COOP_BOX_UID:$COOP_BOX_GID" /home/node; \
    fi
USER node
ENTRYPOINT ["/usr/local/bin/coop-entry"]
WORKDIR /workspace
`

// ImageForRepo decides which image a repo runs in: an explicit override wins; a
// repo with its own box Dockerfile gets its own tag (so a project's toolchain
// never clobbers the shared base); a repo with only a .tool-versions gets its
// toolchain image (toolchain_image.go); everything else uses the base image.
func ImageForRepo(repo, baseImage, override string) string {
	if override != "" {
		return override
	}
	if fileExists(filepath.Join(repo, project.DockerfilePath(repo))) {
		return boxAgentIdentity().projectImage(ServicesProject(repo))
	}
	if img := toolchainImage(repo, baseImage); img != "" {
		return img
	}
	return baseImage
}

// SharedImage selects the operator's override or the resolved base without consulting repository
// files. Jobs without a project-runtime grant and eval trials use this shared selection.
func SharedImage(baseImage, override string) string {
	if override != "" {
		return override
	}
	return baseImage
}

// ManagedBaseRepository is where Coop builds its shared base. The tag is this binary's box
// definition, so each Coop version on a host keeps a base of its own: one shared tag had two
// versions rebuilding it back and forth, each running the other's clients in between.
const ManagedBaseRepository = "coop-box"

// ResolveBaseImage points the default base, the bare repository, at this binary's own definition:
// coop-box:<definition>. Any other COOP_BASE_IMAGE is the operator's image and stands as written.
func ResolveBaseImage(cfg *config.Config) {
	if cfg.BaseImage != ManagedBaseRepository {
		return
	}
	if definition := baseDefHash(); definition != "" {
		cfg.BaseImage = managedBaseImage(definition)
	}
}

func managedBaseImage(definition string) string {
	return ManagedBaseRepository + ":" + definition[:32]
}

// IsManagedBase reports whether image is a base Coop builds for a box definition.
func IsManagedBase(image string) bool {
	tag, ok := strings.CutPrefix(image, ManagedBaseRepository+":")
	return ok && len(tag) == 32 && strings.Trim(tag, "0123456789abcdef") == ""
}

// ManagedBaseRepair reports whether Coop should build its own base itself: the base is managed and
// missing, the daemon answers, and a Coop built a managed base on this host before, so an upgrade
// named a new tag (or a prune took this one) rather than a first build being due, which stays the
// operator's. builtBy names who built the latest earlier base; removed is set when this very tag was
// built here before.
func ManagedBaseRepair(rt runtime.Runtime, cfg *config.Config) (builtBy string, removed, ok bool) {
	if !IsManagedBase(cfg.BaseImage) || ImageExists(rt, cfg.BaseImage) || rt.EnsureDaemon() != nil {
		return "", false, false
	}
	if builtBy, ok = EarlierManagedBase(cfg); !ok {
		return "", false, false
	}
	_, _, err := readImageMeta(imageMetaPath(cfg, cfg.BaseImage))
	return builtBy, err == nil, true
}

// BuildManagedBase builds the managed base alone. It reads nothing and writes build output to w:
// an ACP child or a session daemon can reach it, and their stdio is not a terminal.
func BuildManagedBase(rt runtime.Runtime, cfg *config.Config, version string, w io.Writer) error {
	return buildBaseImage(rt, cfg, false, version, w, w)
}

// ImageExists reports whether the given image is present locally.
func ImageExists(rt runtime.Runtime, image string) bool {
	return rt.Silent("image", "inspect", image)
}

// Build builds the box image: a repo with a .agent/Dockerfile builds that (its
// own toolchain), otherwise the shared base (baseImageDefinition). When fresh is
// set it adds --pull --no-cache so the OS and Node underneath are refreshed (this
// is `coop update`); the clients are Coop's qualified set either way.
// version is the building coop's version, stamped beside the image so a later
// launch can flag binary/image skew (box can't resolve it itself — cli owns it).
func Build(rt runtime.Runtime, cfg *config.Config, repo string, fresh bool, version string) error {
	return BuildWith(rt, cfg, repo, fresh, version, os.Stdin, os.Stdout)
}

// BuildWith is Build with the runtime's own stdin/stdout supplied by the caller. `coop acp` speaks
// JSON-RPC over os.Stdout and reads the editor's requests from os.Stdin, so a build on that path
// must NOT touch either: build chatter on stdout corrupts the wire, and reading stdin swallows the
// editor's initialize. It passes an empty reader and os.Stderr instead (ui.* is already stderr-only,
// so progress lands in the editor's agent log either way).
func BuildWith(rt runtime.Runtime, cfg *config.Config, repo string, fresh bool, version string, stdin io.Reader, stdout io.Writer) error {
	plan, err := PlanBuild(rt, cfg, repo, fresh)
	if err != nil {
		return err
	}
	// The CLI announces a build in the approved words (cli.announceBuild), so this entry — used by
	// an automatic rebuild inside a launch — says only the plain fact, once.
	if plan.Toolchain && cfg.Egress != "" && cfg.Egress != "open" {
		mode := egress.Mode(cfg.Egress).Word()
		return fmt.Errorf("%s networking does not automatically build this project's tools — run 'coop build --egress open' on the host to build them from .tool-versions", mode)
	}
	if plan.Project && !plan.Toolchain && cfg.Egress != "" && cfg.Egress != "open" {
		mode := egress.Mode(cfg.Egress).Word()
		return fmt.Errorf("%s networking does not automatically build project instructions — review %s and its copied build files, then run 'coop build --egress %s' on the host; project builds use ordinary networking, not the run's network restrictions", mode, plan.Dockerfile, mode)
	}
	switch {
	case !plan.Project:
		ui.Note("building %s (shared base)", cfg.BaseImage)
	case plan.Toolchain:
		ui.Note("building %s from .tool-versions (this project's tools; the first build may compile)", plan.Image)
		if plan.BaseFirst {
			ui.Note("building %s (shared base) first — the tools are installed on it", cfg.BaseImage)
		}
	default:
		if plan.Untracked {
			ui.Note("note: %s is untracked in git — it defines this box, and an agent can author one; review it before building", plan.Dockerfile)
		}
		ui.Note("building %s from %s (this project's toolchain)", plan.Image, plan.Dockerfile)
		if plan.BaseFirst {
			ui.Note("building %s (shared base) first — %s inherits it via COOP_BASE_IMAGE", cfg.BaseImage, plan.Dockerfile)
		}
	}
	return BuildPlanned(rt, cfg, repo, plan, fresh, version, stdin, stdout)
}

// BuildPlan is what a build is about to do, resolved before it starts so a caller can say so in
// its own words — which image, from which Dockerfile, and whether the shared base has to be built
// first. Every field is an observation, never a default: Image is the tag that will exist
// afterwards, and BaseFirst is true only when the base really is missing or being refreshed.
type BuildPlan struct {
	Project    bool   // building this project's own image rather than the shared base
	Dockerfile string // repo-relative box Dockerfile; empty for a shared-base build
	Image      string // the tag the build produces
	Untracked  bool   // the box Dockerfile is not tracked in git — an agent could have authored it
	BaseFirst  bool   // the shared base must be built before this project's image
	Toolchain  bool   // building the project's toolchain image from .tool-versions (no box Dockerfile)
	usesBase   bool   // the Dockerfile inherits the shared base via COOP_BASE_IMAGE
}

// CarriesQualifiedClients reports whether the built image holds Coop's qualified clients: the
// shared base does, and so does a project image built on it; one built on another base brings
// its own.
func (p BuildPlan) CarriesQualifiedClients() bool { return !p.Project || p.usesBase }

// ImageChange is what a build did to one image tag: the image ID the tag named before the build
// and the one it names after. Either is empty where there was none, or the runtime could not say.
type ImageChange struct{ Before, After string }

// Replaced reports whether the build moved the tag off an image a running box may still use.
func (c ImageChange) Replaced() bool { return c.Before != "" && c.After != "" && c.Before != c.After }

// Unchanged reports whether the tag still names the image it named before the build.
func (c ImageChange) Unchanged() bool { return c.Before != "" && c.Before == c.After }

// PlanBuild resolves what a build would do, without building anything. It loads project.yaml (so
// a malformed one fails loudly here rather than halfway through) and requires the runtime, since
// whether the shared base already exists is part of the answer.
func PlanBuild(rt runtime.Runtime, cfg *config.Config, repo string, fresh bool) (BuildPlan, error) {
	if repo == "" {
		// No project: the shared image, whose definition is Coop's own, so no folder is read.
		return BuildPlan{Image: cfg.BaseImage}, rt.EnsureDaemon()
	}
	proj, err := project.Load(repo)
	if err != nil {
		return BuildPlan{}, err
	}
	if err := rt.EnsureDaemon(); err != nil {
		return BuildPlan{}, err
	}
	dfRel := proj.DockerfileRel() // box.dockerfile, else .agent/Dockerfile
	content, readErr := readRepositoryRegularFileNoFollow(repo, dfRel, imageInputFileLimit)
	if errors.Is(readErr, os.ErrNotExist) {
		if _, _, ok, err := toolchainDefinition(repo); err != nil || !ok {
			return BuildPlan{Image: cfg.BaseImage}, err
		}
		return BuildPlan{
			Project:   true,
			Toolchain: true,
			Image:     toolchainImage(repo, cfg.BaseImage),
			BaseFirst: fresh || !ImageExists(rt, cfg.BaseImage),
			usesBase:  true,
		}, nil
	}
	if errors.Is(readErr, syscall.ELOOP) {
		if err := refuseLinkedBuildFile(repo, dfRel); err != nil {
			return BuildPlan{}, err
		}
	}
	if readErr != nil {
		return BuildPlan{}, fmt.Errorf("read project Dockerfile %s: %w", dfRel, readErr)
	}
	// A linked ignore file is refused like a linked Dockerfile: here, before any setup or staging.
	for _, rel := range dockerIgnoreFiles(dfRel) {
		if err := refuseLinkedBuildFile(repo, rel); err != nil {
			return BuildPlan{}, err
		}
	}
	if err := refuseCopiedProjectFile(content, dfRel); err != nil {
		return BuildPlan{}, err
	}
	// A project Dockerfile may inherit coop's trusted base (agent CLIs + ACP adapters, browser
	// libraries, writable-home + security setup) with `ARG COOP_BASE_IMAGE` / `FROM ${COOP_BASE_IMAGE}`,
	// adding only its own toolchain. When it does, the base has to be present (built if missing,
	// rebuilt on --fresh) and passed in as a build-arg.
	usesBase := strings.Contains(string(content), "COOP_BASE_IMAGE")
	return BuildPlan{
		Project:    true,
		Dockerfile: dfRel,
		Image:      ImageForRepo(repo, cfg.BaseImage, cfg.ImageOverride),
		// The box Dockerfile defines the box's next sandbox (its USER/RUN/ENTRYPOINT), and an agent
		// with write access to the repo can author one. The build is always an explicit human
		// action, but an untracked box definition is exactly the agent-authored case — surface it
		// so a moved/planted file isn't built silently. Cheap visibility, not a gate.
		Untracked: fileUntracked(repo, dfRel),
		BaseFirst: usesBase && (fresh || !ImageExists(rt, cfg.BaseImage)),
		usesBase:  usesBase,
	}, nil
}

// BuildPlanned performs a plan PlanBuild resolved. It prints nothing of its own — the runtime's
// build output goes to stdout/stderr, and the caller owns every sentence coop speaks around it.
func BuildPlanned(rt runtime.Runtime, cfg *config.Config, repo string, plan BuildPlan, fresh bool, version string, stdin io.Reader, stdout io.Writer) error {
	if !plan.Project {
		return buildBaseImage(rt, cfg, fresh, version, stdout, os.Stderr)
	}
	if plan.BaseFirst {
		if err := buildBaseImage(rt, cfg, fresh, version, stdout, os.Stderr); err != nil {
			return err
		}
	}
	if plan.Toolchain {
		return buildToolchainImage(rt, cfg, repo, plan, fresh, stdin, stdout)
	}
	// Build from a shadow-filtered COPY of the repo, not the repo itself: secret shadowing is a
	// run-time -v overlay, so without this a `COPY .env /` / `COPY . .` in an agent-authored
	// .agent/Dockerfile would bake a shadowed secret into a persistent (pushable) image layer. The
	// staged context omits every shadowed path (and .git), so a targeted COPY of a secret fails the
	// build loudly instead of leaking silently.
	ctx, cleanup, err := stageBuildContext(repo)
	if err != nil {
		return &StageError{Err: err}
	}
	defer cleanup()
	err = runBuild(rt, stdin, stdout, os.Stderr, projectBuildArgs(ctx, plan.Dockerfile, plan.Image, cfg.BaseImage, plan.usesBase, fresh)...)
	if err == nil {
		StampImageInputs(cfg, repo, plan.Image) // record inputs so a later run can flag drift
	}
	return err
}

// StageError is a build that never started: coop could not assemble the secret-filtered copy of
// the repository it builds from. Distinct from a build that ran and failed — nothing was built,
// and the fix is a path or a permission rather than the Dockerfile.
type StageError struct{ Err error }

func (e *StageError) Error() string { return "staging the build context: " + e.Err.Error() }

func (e *StageError) Unwrap() error { return e.Err }

// The staged context may have been reviewed before a host confirmation. Never reread the
// checkout here: that would execute instructions the confirmation did not cover.
func buildStagedProjectOnBase(ctx context.Context, rt runtime.Runtime, dir string, entries []contextEntry, dfRel, tag, base string, automatic bool, lease *os.File, stderr io.Writer) (image string, err error) {
	if err := validateProjectBuildFiles(dir, entries, dfRel); err != nil {
		return "", &StageError{Err: err}
	}
	idFile, err := os.CreateTemp("", "coop-build-id-")
	if err != nil {
		return "", err
	}
	_ = idFile.Close()
	defer os.Remove(idFile.Name())
	// This build's own mark: the project it is for, so a later build of the same project can tell
	// its predecessors from an image someone else built into that name (reclaim.go).
	var labels []string
	if project := derivedImageProject(tag); project != "" {
		labels = append(labels, derivedImageLabel+"="+project)
	}
	// Built unnamed: the caller tags the image once it has passed its proofs.
	args := projectBuildArgs(dir, dfRel, "", base, true, false, labels...)
	if automatic {
		rt = rt.ForApprovedBuild()
	}
	code, runErr := rt.RunProjectBuild(ctx, lease, nil, buildOutput(stderr), append([]string{"build", "--iidfile", idFile.Name()}, args[1:]...)...)
	if runErr != nil {
		return "", runErr
	}
	if code != 0 {
		return "", fmt.Errorf("%s build exited with status %d", filepath.Base(rt.Name), code)
	}
	if image, err = readImageID(idFile.Name()); err != nil {
		return "", err
	}
	return image, nil
}

// projectBuildArgs assembles the `<runtime> build` args for a project Dockerfile at dfRel inside the
// staged ctx, tagged img unless img is "". When the Dockerfile inherits coop's base (usesBase), it
// passes the base as a build-arg and, on --fresh, uses --no-cache WITHOUT --pull — the base is a local
// image tag, not a registry ref, so --pull would fail trying to fetch it. The user identity args let
// standalone Dockerfiles create a matching non-root account. An external FROM still gets --pull on
// fresh.
func projectBuildArgs(ctx, dfRel, img, baseImage string, usesBase, fresh bool, labels ...string) []string {
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
	id := boxAgentIdentity()
	args = append(args, "--build-arg", "COOP_BOX_UID="+fmt.Sprint(id.uid), "--build-arg", "COOP_BOX_GID="+fmt.Sprint(id.gid))
	for _, label := range labels {
		args = append(args, "--label", label)
	}
	if img != "" {
		args = append(args, "-t", img)
	}
	return append(args, "-f", filepath.Join(ctx, dfRel), ctx)
}

// buildBaseImage builds the shared base for the platform the runtime builds for, from a staged
// context holding only the embedded definition, then stamps it for the staleness checks.
func buildBaseImage(rt runtime.Runtime, cfg *config.Config, fresh bool, version string, stdout, stderr io.Writer) error {
	if !boxAgentIdentity().valid() {
		return errors.New("this Linux user ID cannot build Coop boxes: the box user must be non-root and distinct from the gateway user 65532")
	}
	platform, flags, err := rt.BuildPlatform()
	if err != nil {
		return err
	}
	if platform != "linux/amd64" && platform != "linux/arm64" {
		return fmt.Errorf("the box image's clients are built only for linux/amd64 and linux/arm64 — %s builds for %s", filepath.Base(rt.Name), platform)
	}
	system, arch, _ := strings.Cut(platform, "/")
	files, err := baseImageDefinition(agents.ClientPlatform{OS: system, Architecture: arch, Libc: "glibc"})
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "coop-base-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := stageBaseImageFiles(dir, files); err != nil {
		return err
	}
	// What the tag names now, which a rebuild of the same definition (`coop update`) untags. Only a
	// base Coop tags by its definition is Coop's to remove; an operator's COOP_BASE_IMAGE never is.
	previous := ""
	if IsManagedBase(cfg.BaseImage) {
		previous = rt.ImageID(cfg.BaseImage)
	}
	if err := runBuild(rt, nil, stdout, stderr, baseBuildArgs(cfg, fresh, flags, dir)...); err != nil {
		return err
	}
	StampImageMeta(cfg, cfg.BaseImage, version) // record builder + definition so a later run can flag skew/age
	// The image an older definition's build left is still tagged by ITS definition, so nothing else
	// would ever remove it. Another Coop still using one keeps it, because its launches record that use.
	reclaimAfterBuild(context.Background(), rt, cfg, cfg.BaseImage, previous, stdout, nil)
	return nil
}

func stageBaseImageFiles(dir string, files map[string][]byte) error {
	for name, data := range files {
		file := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(file, data, 0o644); err != nil {
			return err
		}
	}
	// Docker COPY preserves the source's directory modes. Keep the temporary context root
	// private, but never let the caller's umask turn system/etc into a root-only /etc.
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o755)
		}
		return os.Chmod(path, 0o644)
	})
}

// baseBuildArgs assembles the runtime args for building the shared base image from its staged
// context dir. fresh adds --pull --no-cache and floats the Node base to its tag, so an update
// refreshes the OS and runtime underneath; the clients are the qualified set either way.
// Otherwise the base is pinned so `coop build` is reproducible.
func baseBuildArgs(cfg *config.Config, fresh bool, platformFlags []string, dir string) []string {
	args := []string{"build"}
	node := pinnedNodeImage
	if fresh {
		args = append(args, "--pull", "--no-cache")
		node = floatingNodeImage
	}
	args = append(args, platformFlags...)
	args = append(args,
		"--build-arg", "NODE_IMAGE="+node,
		"--build-arg", "COOP_BOX_UID="+fmt.Sprint(boxAgentIdentity().uid),
		"--build-arg", "COOP_BOX_GID="+fmt.Sprint(boxAgentIdentity().gid),
	)
	return append(args, "-t", cfg.BaseImage, "-f", filepath.Join(dir, "Dockerfile"), dir)
}

// stageBuildContext copies repo's build context (buildContextSelection) into a throwaway dir. This
// keeps generated firmware, tool caches, and other disposable artifacts from exhausting host storage
// before Docker can apply its own ignore rules.
func stageBuildContext(repo string) (string, func(), error) {
	entries, err := buildContextSelection(context.Background(), repo)
	if err != nil {
		return "", func() {}, err
	}
	dir, _, cleanup, err := stageContextEntries(context.Background(), repo, entries)
	return dir, cleanup, err
}

// contextEntry is one path a project build's context holds: a directory, a regular file with its
// permission bits, or a symlink kept as a link (so a link to an omitted secret is left dangling, not
// resolved).
type contextEntry struct {
	rel    string      // repo-relative, in OS form
	mode   fs.FileMode // fs.ModeDir, fs.ModeSymlink, or a regular file's permission bits
	target string      // a symlink's target
	file   fs.FileInfo // a regular file as the selection saw it: a later read must be this same file
}

// buildContextSelection is the one decision about what a project build may read: every path under
// repo EXCEPT .git, Coop's own files in the project, a shadowed path (NewShadowDecider — the same
// denylist that hides secrets from a run) and gitignored build output, then filtered by Docker's
// context exclusions. Tracked files remain even when a later Git ignore rule matches them, as do
// ordinary untracked inputs an agent is still authoring; irregular files are left out. It is
// recomputed for every build: an ignore rule, a .coopignore or the built-in denylist changing what
// is selected has to change what is built, so the selection itself must never be cached.
//
// Coop's own files are the settings a person edits to add a network rule and the markers approving
// it leaves. A file out of the context can neither change a build's digest nor reach an image,
// whatever the Dockerfile copies and whether or not the project has an ignore file, so that edit
// asks for no build and `COPY . .` bakes in nothing unreviewed. The one setting a build depends on,
// box.dockerfile, is bound by itself (projectBuildReview).
func buildContextSelection(ctx context.Context, repo string) ([]contextEntry, error) {
	shadowed := NewShadowDecider(repo)
	ignored := ignoredBuildPaths(repo)
	var entries []contextEntry
	err := filepath.WalkDir(repo, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(repo, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		slash := filepath.ToSlash(rel)
		if d.Name() == ".git" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil // a linked worktree's pointer names a host path, and a digest of it proves nothing
		}
		hidden, err := shadowed(slash)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if hidden || ignored(slash) {
				return fs.SkipDir // source control, secrets, and generated output never enter the context
			}
			entries = append(entries, contextEntry{rel: rel, mode: fs.ModeDir})
			return nil
		}
		if hidden || ignored(slash) || slash == project.File || slash == networkstate.ProjectApprovalMarker || slash == serviceApprovalMarker {
			return nil // a COPY fails loudly instead of baking a secret or depending on generated output
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			entries = append(entries, contextEntry{rel: rel, mode: fs.ModeSymlink, target: target})
		case info.Mode().IsRegular():
			entries = append(entries, contextEntry{rel: rel, mode: info.Mode().Perm(), file: info})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	entries, err = filterDockerBuildContext(ctx, repo, entries)
	if err != nil {
		return nil, err
	}
	return withoutEmptyDirectories(entries), nil
}

// withoutEmptyDirectories drops a directory with nothing selected under it. Git cannot carry one, so
// a fork or a fresh clone of the same commit lacks it (`coop init` leaves .agent/presets empty) and
// would never match the approval its parent got. A COPY naming one fails loudly and `COPY . .` leaves
// it out, as on a fresh clone.
func withoutEmptyDirectories(entries []contextEntry) []contextEntry {
	used := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.mode.IsDir() {
			continue
		}
		for dir := filepath.Dir(entry.rel); dir != "." && !used[dir]; dir = filepath.Dir(dir) {
			used[dir] = true
		}
	}
	return slices.DeleteFunc(entries, func(entry contextEntry) bool { return entry.mode.IsDir() && !used[entry.rel] })
}

// contextDigest hashes entries as a build reads them: each entry's kind, path, and mode or link
// target, and each file's bytes, NUL-separated (no path or target holds a NUL). write, when set,
// gets every entry and a file's content from the same read, so a staged copy and its digest cannot
// disagree.
func contextDigest(ctx context.Context, repo string, entries []contextEntry, write func(contextEntry, io.Reader) error) (string, error) {
	digest, _, err := contextDigestFiles(ctx, repo, entries, write)
	return digest, err
}

// contextDigestFiles is contextDigest with each entry's own digest of the same bytes, by path, from the
// same read. A later tree compared with them can name what changed, where the one digest only says that
// something did. They explain a refusal and approve nothing.
func contextDigestFiles(ctx context.Context, repo string, entries []contextEntry, write func(contextEntry, io.Reader) error) (string, map[string]string, error) {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return "", nil, err
	}
	defer root.Close()
	sum := sha256.New()
	files := make(map[string]string, len(entries))
	record := func(rel, line string) {
		fmt.Fprint(sum, line)
		entry := sha256.Sum256([]byte(line))
		files[rel] = hex.EncodeToString(entry[:16])
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		rel := filepath.ToSlash(entry.rel)
		if entry.mode.IsDir() || entry.mode&fs.ModeSymlink != 0 {
			record(rel, fmt.Sprintf("%s\x00%s\x00%s\x00", entry.mode.Type(), rel, entry.target))
			if write != nil {
				if err := write(entry, nil); err != nil {
					return "", nil, err
				}
			}
			continue
		}
		// Read only inside repo — os.Root refuses a path a swapped-in link would take out of it — and
		// only the very file the selection judged: os.Root still follows a link that stays inside, so
		// it is SameFile that leaves out a file deleted or replaced since (an editor's save, a directory
		// swapped for a link into ignored output), as a walk a moment later would have left it. That
		// needs real inode numbers; a filesystem that invents them would drop files here. O_NONBLOCK
		// keeps a FIFO swapped in from blocking the open.
		file, err := root.OpenFile(entry.rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		info, err := file.Stat()
		if err != nil || !os.SameFile(info, entry.file) {
			file.Close()
			if err != nil {
				return "", nil, err
			}
			continue
		}
		content := sha256.New()
		if write != nil {
			err = write(entry, io.TeeReader(file, content))
		} else {
			_, err = io.Copy(content, file)
		}
		file.Close()
		if err != nil {
			return "", nil, err
		}
		record(rel, fmt.Sprintf("file\x00%s\x00%o\x00%x\x00", rel, contextFileMode(entry.mode), content.Sum(nil)))
	}
	return hex.EncodeToString(sum.Sum(nil)), files, nil
}

// stageContextEntries copies entries into a throwaway dir and returns the digest of exactly what it
// wrote.
func stageContextEntries(ctx context.Context, repo string, entries []contextEntry) (string, string, func(), error) {
	return stageContextEntriesUnder(ctx, repo, entries, "")
}

func stageContextEntriesUnder(ctx context.Context, repo string, entries []contextEntry, parent string) (string, string, func(), error) {
	dir, err := os.MkdirTemp(parent, "coop-buildctx-")
	if err != nil {
		return "", "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	root, err := os.OpenRoot(dir)
	if err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	defer root.Close()
	digest, err := contextDigest(ctx, repo, entries, func(entry contextEntry, content io.Reader) error {
		return stageEntry(root, entry, content)
	})
	if err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	return dir, digest, cleanup, nil
}

// contextFileMode is the mode a staged file carries and its digest names: Git's model, where only the
// owner's execute bit means anything. The other bits come from the umask of whoever checked the file
// out (Ubuntu's default 0002 gives 0664, and a fork's checkout differs from a parent Coop wrote at
// 0644), so the same commit has to stage, and match its approval, the same way under any umask.
func contextFileMode(mode fs.FileMode) fs.FileMode {
	if mode&0o100 != 0 {
		return 0o755
	}
	return 0o644
}

// stageEntry writes one entry into the staged context. Modes are set, not left to the umask: a file
// created under a `umask 077` shell would carry a mode the repository does not, and the image would
// inherit it. Every write goes through the root, and a file must be new: an agent chooses these names,
// and on a volume that folds case a planted link `A` is also the path to `a/x`. Nothing it stages may
// land outside the snapshot, and a name that collides fails instead of overwriting.
func stageEntry(root *os.Root, entry contextEntry, content io.Reader) error {
	switch {
	case entry.mode.IsDir():
		if err := root.MkdirAll(entry.rel, 0o755); err != nil {
			return err
		}
		return root.Chmod(entry.rel, 0o755)
	case entry.mode&fs.ModeSymlink != 0:
		return root.Symlink(entry.target, entry.rel)
	}
	mode := contextFileMode(entry.mode)
	out, err := root.OpenFile(entry.rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, content)
	if err == nil {
		err = out.Chmod(mode)
	}
	return errors.Join(err, out.Close())
}

// ignoredBuildPaths returns a predicate over Git's ignored, untracked paths. A single
// ls-files call avoids invoking Git once per entry while preserving tracked files that happen to
// match an ignore rule. Non-Git repositories retain the previous copy-all-non-secret behavior.
func ignoredBuildPaths(repo string) func(string) bool {
	cmd := exec.Command("git", "-C", repo,
		"-c", "core.fsmonitor=", "-c", "core.hooksPath=/dev/null",
		"ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z", "--")
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return func(string) bool { return false }
	}

	files := make(map[string]struct{})
	var dirs []string
	for _, raw := range strings.Split(string(out), "\x00") {
		path := filepath.ToSlash(strings.TrimPrefix(raw, "./"))
		if path == "" {
			continue
		}
		if strings.HasSuffix(path, "/") {
			dirs = append(dirs, path)
			continue
		}
		files[path] = struct{}{}
	}
	return func(path string) bool {
		path = filepath.ToSlash(strings.TrimPrefix(path, "./"))
		if _, ok := files[path]; ok {
			return true
		}
		path += "/"
		for _, dir := range dirs {
			if strings.HasPrefix(path, dir) {
				return true
			}
		}
		return false
	}
}

// runBuild runs one image build and names its failure the way a person reads it: the runtime
// they invoked, and the status it exited with.
func runBuild(rt runtime.Runtime, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	out, errOut := buildOutput(stdout), buildOutput(stderr)
	if stdout == stderr {
		errOut = out // one log stays one writer, so its lines keep their order and one goroutine writes it
	}
	code, err := rt.Run(stdin, out, errOut, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("%s build exited with status %d", filepath.Base(rt.Name), code)
	}
	return nil
}

// fileUntracked reports whether repo is a git repo in which name (a repo-relative path) is NOT
// tracked (committed or staged) — the agent-authored case worth surfacing for files coop then
// builds or auto-runs (.agent/Dockerfile, .agent/compose.yml). It uses read-only `ls-files` under
// the trusted git view (no driver, hook or fsmonitor of the agent-writable repo runs) and returns
// false for a non-git repo, where "untracked" isn't a meaningful signal.
func fileUntracked(repo, name string) bool {
	if forkspace.GitRun(repo, "rev-parse", "--git-dir") != nil {
		return false // not a git repo — nothing to compare against
	}
	return forkspace.GitRun(repo, "ls-files", "--error-unmatch", "--", name) != nil // non-zero exit → the file isn't tracked
}
