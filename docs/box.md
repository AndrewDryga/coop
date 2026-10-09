# The box

The box is the container your agent runs in: your project is mounted at its real path, and your
home folder, SSH keys and the rest of your disk aren't there. This page is the reference for what
the box hides, the restricted read-only and bare runs, the toolchain and image, project services,
dev server ports and updates. The website has the overview: [the
sandbox](https://coop.dryga.com/docs.html#sandbox), [hiding
secrets](https://coop.dryga.com/docs.html#secrets) and [toolchain and
services](https://coop.dryga.com/docs.html#toolchain).

## Hiding secrets

Normal writable runs are trusted shared-checkout collaboration: the agent can alter Git/editor
metadata that your native host tools later execute, including metadata in a new child repository.
Coop hardens its own host Git calls, not arbitrary commands you run. Use
[`coop fork <name> <target> --isolated`](forks.md#isolated-write-forks) for independent Git storage
and controlled committed publication, or `--readonly` when the work needs no repository writes.

co:op shadows secret-looking files and directories, such as `.env`, `*.tfvars`, `*.pem`,
`secrets/` and `.ssh`:

- A secret file is covered by a read-only empty file.
- A secret directory is covered by a read-only empty directory.

Templates (`*.example`, `*.sample`, `*.template`) stay visible. The defaults are compiled into
co:op (`internal/shadowpath/shadowpath.go`).

### Your own paths: .coopignore

Add a `.coopignore` at the repo root to hide your own paths:

```gitignore
# a file name, matched at any depth
prod.yml
# an exact path; its final name stays hidden if the parent moves
config/stripe.live.json
# a directory, hidden whole
vault/
```

Comments go on their own lines, as in `.gitignore`. `prod.yml  # note` is one pattern, comment and
all, and a launch warns about it.

An exact path entry keeps its final file or directory name hidden throughout that policy's scope.
This stops a box from exposing the same secret later by renaming its parent directory.

The boundary is `.coopignore`, not `.gitignore`. A normal `coop run`, `coop loop` or `coop shell`
binds your whole working tree. A file that Git ignores but that is on disk (a
`serviceAccount.json`, say) is fully visible to the agent. Shadow it with `.coopignore`.

### Secrets inside files: coop check-secrets

For a token hiding inside a file, `coop check-secrets` scans by content. It reports each hit as
`file:line` and exits 1 on a hit.

- Files hidden by built-in names or `.coopignore` are still reported when Git would commit them.
  Shadowing protects the box, not the push.
- `--include-ignored` widens the scan to the whole visible tree.
- A file it could not read fails the scan by name, so a permission error never reads as a clean
  result.
- It also lists the changed files that alter what runs on your machine (a commit hook, editor or
  agent settings, compose, the Makefile), so you read those before running anything. That report
  never changes the exit code.

A finding you have reviewed and disagree with goes in `.coopsecretsignore` at the project root.
Paste the entry the scan prints and write why. The entry names that exact finding: a new line above
it keeps it, and a changed value or a moved file invalidates it. It carries no credential material.
It applies to this check only: fork merge, checkpoint upload and session redaction keep seeing
every finding.

Prove your setup holds with [`coop doctor`](https://coop.dryga.com/docs.html#doctor).

## Read-only and bare runs

Two launches take the sandbox further, for work that must not change anything:

```bash
coop claude --readonly                  # investigate this repo; every write to it fails
coop claude --bare -- -p "..."          # Q&A with no repo, no project context, no tools
coop run --readonly -- sh -c 'touch x'  # the same profile around a raw command: the probe
```

| | `--readonly` | `--bare` |
| --- | --- | --- |
| Repository | mounted read-only, git history included, plus any approved companions | none mounted |
| Tools | the agent can read, search and run experiments in scratch | co:op adds the provider's own no-tools switch, so the model's request carries no tool: the conversation in, the answer out |
| Networking | direct runs support `--egress filtered` | open or offline only |

Both run on an image co:op manages, under one restricted filesystem profile:

- The container root is read-only.
- The only writable places are run-private in-memory scratch: the box home and `/tmp`, plus an
  empty `/workspace` for bare. It is discarded when the run ends.
- Every host path that enters the box enters read-only.

Nothing the normal launch mounts writable exists here: no credential home, no dependency cache, no
asdf volume, no skills copy, no ACP transcripts. Project environment, services, hooks, MCP servers
and `.tool-versions` provisioning are not loaded. Read-only runs still use the normal project
network approval and admission flow.

The provider gets a seed instead of its home, copied into the tmpfs home before it starts:

- public native auth selectors (real access and refresh grants stay outside the box);
- the run-local provider broker entrance, with host-managed continuous renewal;
- its first-run defaults;
- a note stating the mode's contract.

If the checkout's host path is under the box's `/tmp` or home scratch, a read-only run mounts it at
`/workspace` inside the box instead. An explicit workdir inside that scratch is refused.

Both refuse what they cannot enforce instead of launching on a promise:

- They run on Docker only.
- Native restricted modes are offered only for `claude`.
- `--peer`, presets and `COOP_IMAGE` are refused.
- A `COOP_RUN_ARGS` entry other than `-e KEY=VALUE` stops the launch by name.
- Restricted runs do not start services, so a filtered policy with service grants is refused.
- Remote-session jobs using either restricted mode still reject filtered networking.

The answer is the run's only output. A run that needs artifacts back is a normal run.

## Toolchain: .tool-versions

Real projects need a language toolchain (Elixir, Go and so on) and stateful services (Postgres,
Redis). Having the agent install those at runtime is slow, isn't reproducible, and dies with the
container. Declare them once instead. This is the Dev Containers plus Compose model, without the
ceremony.

If your repo pins versions in a `.tool-versions` file (asdf), the base box provisions that toolchain
at runtime and caches it in a shared volume. It resolves the file from the working directory up the
tree, or else uses `~/.tool-versions`. So a repo with just a `.tool-versions` (no `.agent/Dockerfile`,
no scaffolding) gets its toolchain with zero setup on an open-network run:

```bash
cd ~/code/phoenix-app   # has a .tool-versions
coop claude             # provisions elixir/erlang/node/… from it, then runs the agent
```

This runtime setup is for open networking only. A filtered run (the default once you run
`coop init`) uses the locked client image, which has no asdf, so nothing is installed when the box
starts. There the toolchain has to be built into an `.agent/Dockerfile`:

- `coop init` writes one when it finds a `.tool-versions`.
- `coop init --stack asdf` writes one later.
- `coop build` builds it.

The first install of a new toolchain can be slow (Erlang compiles, for example). After that it is
reused across runs and repos.

Set `COOP_NO_ASDF=1` (in `agents/env`) to skip provisioning from `.tool-versions`. co:op still
repairs a stale persisted Node shim when needed, so the agent CLIs keep running.

For a baked, fully reproducible image instead, `coop init --stack asdf` scaffolds an asdf
`.agent/Dockerfile` that installs the same `.tool-versions` at build time.

## Your own image: .agent/Dockerfile

```bash
coop init --stack asdf # writes an asdf .agent/Dockerfile (from .tool-versions)
coop build             # builds the image for this project's network mode
```

A repo with its own `.agent/Dockerfile` gets its own image tag, so projects never collide. Every
`coop`, `coop loop`, `coop fork` and `coop acp` in that repo uses it.

The scaffolded Dockerfile is the asdf image. It bakes in the exact `.tool-versions` toolchain; the
versions live in `.tool-versions`, not in the Dockerfile. For anything more exotic, hand-write a
`.agent/Dockerfile` that meets [the box contract](#the-box-contract).

When the agent needs a new system package, add it to the `RUN` line and run `coop build` again. The
dependency graduates into the image instead of being installed each run.

co:op records the image's inputs at build time. If you change `.agent/Dockerfile` or
`.tool-versions` but forget to rebuild, `coop` notices on the next run and reminds you to run
`coop build`.

The selected Dockerfile must be a regular in-repo file, not a symlink.

You can keep the box definition elsewhere: reuse a stage of your app's existing `Dockerfile`, or
point sidecars at your own `docker-compose.yml` instead of maintaining a separate one. Set these
repo-relative paths in `.agent/project.yaml`:

| Key | Default |
| --- | --- |
| `box.dockerfile` | `.agent/Dockerfile` |
| `box.compose` | `.agent/compose.yml` |

### Builds and network modes

Under `--egress filtered`, review the Dockerfile and its copied build files, then build explicitly
with `coop build --egress filtered`. Plain `coop build` selects filtered on its own when that is the
project's effective network mode. The filtered build uses co:op's locked client base and a separate
image tag.

A filtered launch reuses the exact approved image. Changed build-context inputs, an upgraded client
base or a missing image need another explicit build.

Open-network launches still prepare a missing editor image automatically, but ordinary runs don't
rebuild stale project images. After you change build inputs, run `coop build --egress open`
yourself. Offline launches don't run project build instructions automatically.

Explicit project builds use ordinary networking, not the run's restrictions. See [restricted
networking](networking.md) for image proofs and the build boundary.

### Box-only environment: box.env

Put committed, non-secret defaults that only the box needs under `box.env`. Values are literal
strings, so quote numeric-looking values. Your `~/.config/coop/agents/env` overrides these
defaults, and `COOP_*` names are reserved for co:op's runtime contract.

```yaml
box:
  env:
    PGHOST: db
    PGPORT: "5432"
```

### The box contract

You can build the box on any base. An image is a valid agent box when:

1. It runs as a non-root user. Claude Code refuses `--dangerously-skip-permissions` as root.
2. That user's home is `/home/node`, because the `agents/` auth mounts land at `$HOME/.claude`,
   `$HOME/.codex` and `$HOME/.gemini`. On a different base, set `COOP_HOME_IN_BOX=/home/<user>`.
3. `claude`, `codex` and `gemini` are on `PATH` (so it needs Node), plus the ACP adapters if you
   want `coop acp`.
4. It runs `git config --system --add safe.directory '*'`, so git works on the host-owned bind
   mount. That mount is normally at the repo's real path; new remote sessions use `/workspace`
   inside their private boxes.

On native Linux, the image's non-root user must also have the host user's UID/GID to read a private
checkout and the selected credential's files. co:op builds its managed images for that user
automatically. If you supply `COOP_BASE_IMAGE` or another custom image, set its user to match.
Docker Desktop on macOS keeps the managed image's UID/GID 1000.

co:op sets the working directory itself, so no `WORKDIR` is required. A skeleton:

```dockerfile
FROM <your-language-base>
RUN <install your toolchain> \
 && npm install -g @anthropic-ai/claude-code@2.1.285 @openai/codex@0.159.2 @google/gemini-cli@0.62.0 \
      @agentclientprotocol/claude-agent-acp@0.84.0 @agentclientprotocol/codex-acp@2.0.1 \
 && git config --system --add safe.directory '*'
ARG COOP_BOX_UID=1000
ARG COOP_BOX_GID=1000
RUN if id -u node >/dev/null 2>&1; then \
      if ! getent group "$COOP_BOX_GID" >/dev/null; then groupmod -g "$COOP_BOX_GID" "$(id -gn node)"; fi \
   && usermod -u "$COOP_BOX_UID" -g "$COOP_BOX_GID" node \
   && chown -R "$COOP_BOX_UID:$COOP_BOX_GID" /home/node; \
    else \
      if ! getent group "$COOP_BOX_GID" >/dev/null; then groupadd -g "$COOP_BOX_GID" node; fi \
   && useradd -m -u "$COOP_BOX_UID" -g "$COOP_BOX_GID" -s /bin/bash node; \
    fi
USER node
```

If the base lacks Node, install it first (NodeSource works). The skeleton assumes the base has the
standard `useradd`, `usermod` and `groupmod` tools. `coop build` supplies the host UID/GID build
args for project Dockerfiles. A separately built `COOP_BASE_IMAGE` needs those args supplied by its
builder.

The skeleton pins the versions this co:op release qualifies. An image on another base runs whatever
it installs, so move those versions when you update co:op, or [inherit co:op's
base](#inherit-coops-base) and never think about it.

Images based on co:op inherit Gemini's root-owned `/etc/gemini-cli/` settings. Independent images
must also carry `thinking/low.json`, `thinking/high.json` and `login.json` there, for co:op's
per-call thinking and sign-in tool restrictions. Gemini ignores system files below a user-owned
home. Prefer inheriting co:op's base over recreating its client layer.

Current co:op entrypoints also verify publication of Claude's selected account and trust config
before starting a command. Rebuild inherited images to pick up this check. Independent entrypoints
do not provide it.

### Inherit co:op's base

Rather than meet the contract yourself, start from co:op's own box and add only your toolchain.
`coop build` resolves the base image and passes it in, building it first if needed:

```dockerfile
ARG COOP_BASE_IMAGE=coop-box   # coop build overrides this with the resolved base
FROM ${COOP_BASE_IMAGE}        # agent CLIs + ACP adapters, asdf, browser libs, non-root node — all inherited
USER root
RUN <install your toolchain>
USER node
```

co:op tags its base by the box definition it was built from, as `coop-box:<definition>`
(`coop build` names it). So two co:op versions on one machine each keep their own instead of
rebuilding one shared tag in turn. A launch after an upgrade builds the new one itself.

A plain `docker build` of this file needs that tag passed in:
`--build-arg COOP_BASE_IMAGE=coop-box:<definition>`. `docker image ls coop-box` lists them.

A build also reclaims the images it superseded. It removes an image co:op tags by its definition
when no run has used it in 14 days and no container references it, and it says which. Those images
are `coop-box`, `coop-clients`, `coop-network` and, when you explicitly build your project's own box
image, `<project>-filtered` (tagged by the client image it was built on). Images built before co:op
reclaimed them carry no co:op mark, so they are never candidates. Every launch records the image it
used, so a second co:op version you still run keeps its own base. An image of your own is never
touched, whatever it is tagged.

### Reusing a devcontainer

If a repo already has a `.devcontainer/`, reuse its image as your base and add the agent layer on
top:

```dockerfile
FROM your-devcontainer-image          # the team's source of truth for the env
RUN npm install -g @anthropic-ai/claude-code@2.1.285 @openai/codex@0.159.2 @google/gemini-cli@0.62.0 \
      @agentclientprotocol/claude-agent-acp@0.84.0 @agentclientprotocol/codex-acp@2.0.1 \
 && git config --system --add safe.directory '*'
USER <the devcontainer's non-root user>
# If that user's home isn't /home/node, run with COOP_HOME_IN_BOX=/home/<user>.
```

The devcontainer decides what's in the environment: toolchain, features, reproducibility. co:op
runs an untrusted agent in it safely, with secret shadowing, the container boundary, the queue and
the foreman. Don't lean on the devcontainer as the security boundary. By itself it mounts your
whole workspace (`.env` included), and a malicious project under `--dangerously-skip-permissions`
can exfiltrate `~/.claude`. The shadowing and the box are what co:op adds on top.

## Project services

Sibling services are opt-in. `coop init` asks which to add (or pass `--services postgres,redis`)
and scaffolds a `.agent/compose.yml`. There are none by default.

```bash
coop up                      # starts the configured Compose services, waits until ready
coop claude                  # the box reaches each service by its Compose name
coop down                    # stop services; stored data is kept
coop down --delete-volumes   # stop them and permanently delete their volumes (asks first)
coop init --services         # add another service to a project that already has some
```

Services run as their own containers on a private network the box joins. Connect with a URL such
as `DATABASE_URL=postgres://postgres:dev-password@db:5432/app_dev`, and put it in `agents/env`.

### Development and loop stacks

The development stack keeps the same workspace identity and data it has always used. A loop gets
its own Compose project, network, volumes and published ports for the whole logical run. They are
reused across task and provider retries and removed when that run ends. The two stacks can run in
one checkout without sharing service data or ports. Their source tree is still shared on purpose:
an ordinary file edit stays visible through either stack's live bind mount.

### Starting and stopping

Before startup, `coop up` asks Compose for the resolved service list. It ends with
`✓ Services ready: db, redis` (those exact names, in Compose order) and the URL of any port it
really published. If discovery fails, co:op does not start the project or claim a ready result.

`coop down --delete-volumes` resolves this project's volumes from the runtime first, prints each one
with what it holds, and asks before removing any. The default answer is No, and `-y`/`--yes` skips
the question. Without a terminal it refuses rather than guessing. An `external: true` volume and a
bind-mounted project file are never in scope.

A change to the configured Compose file is reconciled on the next `coop up` or box launch: services
removed from the file are stopped. `coop down` does the same. Every workspace uses its own hashed
Compose project name, so repositories with the same basename stay isolated.

Agents can change data in services they can reach, so use disposable development databases.
`coop down` stops the services and keeps their volumes. `coop down --delete-volumes` asks before
permanently deleting the project's service volumes.

A shared `coop-cache` volume at `~/.cache` keeps disposable runs from re-downloading everything.
Claude's per-project MCP logs (`~/.cache/claude-cli-nodejs`) are the exception: each run gets an
empty folder of its own, so one project's logs never reach another project's box.

### Volumes from outside the project

An `external: true` or custom-named volume may hold data from another project. At a terminal,
`coop up` shows its actual Docker name, its read-only or read-write access and its service targets
before asking for repository-scoped approval. It also shows the selected Docker daemon and an
existing plain local volume's backing location.

- A missing custom-named volume is created as plain local storage only after you answer Yes.
- A different daemon, a replaced volume, or a bind-backed or plugin volume cannot silently inherit
  the grant.
- No or EOF stops before Compose runs.
- Automatic box launches, filtered sidecars and non-terminal `coop up` refuse these volumes, even
  after a prior approval.

Ordinary project-scoped named volumes need no prompt.

### Secret files in services

Secret-looking files stay hidden from services too. A bind of a `.env`, a `*.key` or a
`.coopignore`d path hands the service an empty decoy, exactly as the box sees it. Otherwise an
agent-written Compose file could ship your secrets to a container it controls.

When a service really needs such a file (a generated dev TLS key for Keycloak, say), bind that one
file read-only (`:ro` or `read_only: true`) and run `coop up` in a terminal. It lists eligible
files and asks once. Secret directories, and files reachable through writable binds, always keep
their decoys. Mount the individual files you need read-only, and use a named volume for mutable
service data.

The approval is tied to:

- The Compose file's exact content. The approval is stored outside the repo, so an edit to the file
  (the one thing a box can do) resets it. Until you approve again, box launches start the services
  with decoys and say which file is hidden and why.
- This checkout's private identity. Copying the Compose file or its marker to a second checkout
  does not transfer it. The first `coop up` after upgrading older content-only approvals asks
  again, and automatic starts use decoys until then.
- The exact files you saw. A secret that lands later under an approved directory bind stays hidden
  until you approve it too.

For services that receive approved secret files or outside volumes, co:op also records the exact
local Docker image ID shown at review, and starts that ID with pulling disabled. An unchanged
approval cannot silently follow a moved image tag.

- `coop up` at a terminal can review a locally updated tag. Declining that renewal leaves the
  previous approval in place and stops this start.
- If the pinned image was removed, startup stops before creating a container. Pull the tag and run
  `coop up` to review it again.
- Reviewing a first approval may pull an uncached image through Docker's existing registry
  authentication before you confirm. Declining does not start the service or grant it access, but
  the image remains in Docker's cache.

Services without these elevated grants keep normal image updates.

Compose commands use a private Docker client config. It keeps registry authentication, including a
validated `DOCKER_AUTH_CONFIG`, without passing the host's configured proxy credentials into
services.

The repo can say which files its services really need, so the ask is documented and travels to
your teammates instead of arriving as a crash:

```yaml
# .agent/project.yaml — committed with the repo
services:
  require_real_files:
    - dev/keycloak/certs/generated/tls.key  # a generated dev TLS key Keycloak reads
```

That list grants nothing on its own, because an agent in the box can edit it like any other
committed file. It only labels the prompt:

- A file the repo asked for reads as expected.
- A file nothing asked for is listed first, as the one to look at.
- A file that appeared since your last approval says so.

You still say yes once on each machine.

### One URL inside and out

Some services must be reachable at one URL from both the host browser and the app in the box, such
as an OIDC issuer like Keycloak. Give the service an `expose:` (container-only) port in
`.agent/compose.yml`:

```yaml
services:
  keycloak:
    image: quay.io/keycloak/keycloak
    expose: ["8443"]                     # coop publishes this to a stable, per-workspace localhost port
    labels:
      coop.service.scheme: https         # scheme for COOP_SERVICE_*_URL (default http)
```

co:op assigns a stable host port per development workspace or logical loop run. The port is keyed
on the owner, service and port, so concurrent stacks never collide. co:op publishes it on loopback
only and runs a tiny raw-TCP forwarder inside the box, so `https://localhost:<port>` resolves to
Keycloak from both sides. The issuer string matches, with no `host.docker.internal` and no weakened
isolation.

The box gets `COOP_SERVICE_KEYCLOAK_URL=https://localhost:<port>`, with the scheme from the label.
`coop fork ls --json` lists every workspace's service URLs for host tooling.

### What a Compose file may do

`.agent/compose.yml` runs on your host daemon (that's how a service becomes a real container), so
co:op validates it before every run: `coop up` and each networked launch alike.

A running agent could replace a validated bind folder with a link to somewhere else on your machine
before Docker opens it. So co:op holds a short exclusive launch barrier around validation and
daemon startup, and a box that can edit the checkout holds the matching shared side. `coop up` waits
for that safe boundary instead of refusing the whole workspace, then starts the independent
development stack.

Only plain sibling-service directives pass:

- an `image`
- inline `environment`
- named volumes or repo-relative binds
- `healthcheck`
- `depends_on`
- loopback-only published ports

Anything that would reach past a repo-scoped container is refused with the exact reason:

- `privileged`
- `cap_add`
- a host bind like `/:/host` or `/var/run/docker.sock`
- `network_mode: host`
- `env_file`
- `build`
- a `0.0.0.0` port
- an escaping symlink
- bind options that relabel host files or change mount propagation

A session whose repository is mounted read-only (an investigation, a review candidate) still gets
its sidecars. Any bind of the repository into one must be read-only too (`:ro` or
`read_only: true`). A writable bind is refused for that session, since it would be a write path
into a checkout the agent itself cannot write. Its read-only bind source must already exist,
because Compose cannot create a host directory on its behalf.

A bind of the repo (or any directory in it) into a sidecar gets the box's own secret shadowing.
Every `.env`, key and `.coopignore`d path under it is an empty decoy inside the sidecar too, and a
bind whose source is itself a secret file is replaced by one. So the file is safe to auto-run no
matter who wrote it. An agent can scaffold services for you, and a prompt-injected one still can't
turn `.agent/compose.yml` into host root.

A running Compose service with a writable bind over the checkout must be stopped before co:op can
safely launch another service stack. The error names the bind and suggests `coop down`. Nested bind
sources under a writable service bind are refused, since a restart could reopen a path the service
replaced.

To run something outside that subset, run it yourself. co:op only auto-runs the safe subset.

## Dev servers in your browser

When the agent builds a website in the box, you can open it on your machine. List the ports the
dev server listens on in `.agent/project.yaml`:

```yaml
serve:
  ports: [5173]        # what the server binds INSIDE the box
```

Every box for this repo (`coop acp`, `coop run`) publishes each port to a stable host port derived
from the repo path. You get the same URL on every launch and a different one per project, so one
shared Zed agent definition serves all your projects without port collisions. In an ACP thread
co:op announces the mapping (`🌐 box :5173 → http://localhost:24187`). On a terminal run it's
printed on stderr.

- The dev server must bind `0.0.0.0` inside the box (`vite --host`, `next dev -H 0.0.0.0` and so
  on). A server on the container's localhost isn't reachable through the mapping.
- Ports bind to your localhost only, never the LAN.
- Publishing needs network egress: `COOP_EGRESS=open` (the default) or `filtered`, where the port
  is published on the run's gateway (see [restricted networking](networking.md)).
- A host port already in use is skipped with a note. `COOP_SERVE_URL_<port>` still carries the
  workspace's assigned URL for configuration and discovery.
- A box restart (a credential switch, a rebuild) takes the dev server down with the old box. The URL
  stays the same, so run the server again.

## Keeping the box current

```bash
coop update              # self-update coop, then rebuild the image fresh
coop update --self-only  # just upgrade the coop binary
coop update --box-only   # just rebuild the image (the old behavior)
```

`coop update` first replaces the `coop` binary when GitHub has a newer release. It downloads that
release's versioned archive and checksum, verifies the archive locally, then swaps it in
atomically, so replacing the running binary is safe. The binary swap takes effect on your next
`coop` run.

When the binary is not replaced:

- A dev or source build, or a binary that is already current or newer, skips the self-update with a
  note and still rebuilds the image.
- If `coop` is installed somewhere it cannot write (a package-manager prefix), the self-update
  fails with an error that tells you to update co:op with the tool that installed it. `coop update`
  still rebuilds the image, then exits 1. `coop update --self-only` stops at the error.

### A newer co:op repairs its own box

When the shared image was built by a co:op whose box definition differs from the one you are
running (after `coop update --self-only`, or a `go install`), the next interactive launch that runs
it rebuilds it before the agent starts. That happens in one `Checking the Coop box` section that
names both versions and ends with `✓ Box updated`. A filtered box runs the qualified client image
instead, so it never triggers this.

- A current image prints nothing.
- An image that is merely old is still only nudged toward `coop update` under that heading, never
  rebuilt unasked.
- An image co:op did not build (`COOP_IMAGE`) is never replaced.
- If the repair cannot run because Docker is not answering, the launch stops there and says what to
  start before you repeat the same command.

### Stable and fresh rebuilds

| Command | Base image | Result |
| --- | --- | --- |
| `coop build` (stable) | pinned to a specific digest | a rebuild gets the same OS and runtime every time |
| `coop update` (fresh) | floated back to the `node:24-slim` tag (`golang:1.27.2-bookworm` for the Go image) and rebuilt with `--pull --no-cache` | the OS packages and the runtime move to their newest |

To move the pinned base permanently, bump `pinnedNodeImage` in `internal/box/image.go`.

### Agent clients

Every co:op box runs the same agent CLIs and ACP adapters: a plain run, a filtered one, a loop, a
preset, an editor session. They are the exact versions this co:op release qualified, installed from
a lockfile built into co:op (Grok's binary is checked against its digest), with each client's own
updater switched off. They move only when co:op does.

For other versions, build your own image (`COOP_IMAGE`, or a `.agent/Dockerfile` on another base).
co:op runs it, but it is not a qualified client set.

co:op also applies a best-effort SQLite trigger to the active Codex credential before launch, so
inserts into the `logs_2.sqlite` feedback-log table are ignored. Session history, auth, MCP config
and memories are not touched.
