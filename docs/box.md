# The box

The box is the container your agent runs in. co:op mounts your project at its real path, but
leaves your home folder and SSH keys unmounted. Its networking is filtered by default, inside or
outside a co:op project: the box reaches the agent's provider and the destinations you approved.
Add `--egress open` for unrestricted access or `--egress offline` for none. See [restricted
networking](networking.md#network-modes). For an overview, see
[the sandbox](https://coop.dryga.com/docs.html#sandbox), [hiding
secrets](https://coop.dryga.com/docs.html#secrets) and [toolchain and
services](https://coop.dryga.com/docs.html#toolchain).

`coop run` and `coop shell` pass piped or redirected input to the command, just as on your host:
`printf 'a\nb\n' | coop run -- wc -l` prints 2. As with `docker run -i` or `ssh`, that input is
read as soon as the box starts, even if the command never reads it. In a shell `while read` loop,
give each `coop run` its own input, as in `coop run -- make test < /dev/null`, or it takes the
lines meant for the loop.

## Hiding secrets

co:op hides files and directories whose names look like secrets, including `.env`, `*.tfvars`,
`*.pem`, `secrets/` and `.ssh`:

On supported Mac Docker runtimes, Coop enforces access in the Linux VM with eBPF.
Secret reads and changes return `Operation not permitted`, including accesses through another
letter case, equivalent Unicode spelling, symlinks and existing hardlinks. Public files remain
writable, and ordinary public hardlinks work. `.coopignore` stays readable and read-only.
This avoids adding a mount for every spelling of each secret filename.

Coop selects this backend automatically for APFS projects when the Docker VM supports its BPF
hooks and cgroup configuration. Launches do not test the protection itself. After updating Docker
or OrbStack, run `coop doctor`: it tries to read test secrets in a real box, including through
another letter case, an equivalent Unicode spelling and a hardlink.
The helper and its compiler are built inside Docker from sources shipped with Coop; no host
packages, filesystem changes or kernel settings are needed. The initial supported configuration
uses cgroup v2 with the `cgroupfs` driver. Other filesystems, unsupported VM kernels and incompatible
container options keep the existing mount backend. See [filesystem protection](#filesystem-protection-contract)
for the host-editing contract.

With the mount backend, Coop covers a secret file with a read-only empty file and a secret
directory with a read-only empty directory. On a case-insensitive disk, it also covers the
uppercase and lowercase spellings of a hidden filename with up to 11 letters, and every spelling
of `.coopignore`.

A longer filename, or a project with too many spellings, can exceed the mount limit. Coop then
refuses to launch the agent or service. Either:

- move the file into a hidden folder, or
- run `coop approve --case-exposure` to review the listed files and remember their exposure under
  mount protection. Ordinary `coop approve` reviews network access and grants no file exposure.

That approval covers those exact paths, including their future contents, for new agent and
service runs. Newly exposed paths need another approval, and running boxes stay unchanged.
`.coopignore` cannot be approved this way. `COOP_ALLOW_CASE_EXPOSED_SECRETS=1` still exposes the
files for a single run. A qualified eBPF run protects these paths without either kind of approval.

### Filesystem protection contract

The host and its editors are trusted. Each eBPF admission captures the current hiding rules and
the protected objects. What that means for you:

- Ordinary edits to public files work immediately.
- Host edits and atomic saves to a destination that is already protected stay protected, even
  when a parent folder is replaced. Existing aliases keep their protection when the original file
  is replaced.
- Restart the affected agent or service after you change `.coopignore`, mark an already-public
  object as secret, or create an unprotected alias to a secret from the host. Running protection
  does not reload its rules or revoke memory mappings that were already readable.
- To save secret bytes atomically, write the temporary file outside the shared tree or inside an
  already hidden folder. A new public temporary path is not a protected destination.
- Protection follows the object, not its path. If a remembered secret still has another hardlink
  in the project, removing its hiding rule alone does not make the object public. Remove the other
  aliases and restart before you declassify the original file.
- An explicit service-file grant allows reads of the admitted object, its aliases and later
  in-place host edits. It never allows writes, and replacing the object needs a new admission.

A separate helper owns the kernel enforcement, and the agent receives no BPF privileges. If the
helper exits, access to the protected share is denied until the workload is retired. Stopped
service containers keep their protection for a later restart. Coop removes kernel state only
after all of an admission's consumers are gone. After a VM reboot, the launch and healthcheck guards
refuse to run image code with an old admission. Start the service through Coop to get fresh
protection.

Templates (`*.example`, `*.sample`, `*.template`) stay visible. The default patterns are built
into co:op.

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

An exact-path rule also hides its final file or directory name everywhere that `.coopignore`
applies. Renaming the parent directory therefore cannot reveal the secret.

Coop recognizes the same hidden file or folder even if its path changes. If a box moves it
somewhere no rule matches, the next box and Coop's own tools still hide it wherever it appears
in the project. This includes a file hidden by `config/*.yml` and moved by `mv config cfg`, or a
folder moved outside a nested `.coopignore`'s scope. At its original path, the current rules decide
whether it is visible. With eBPF, a surviving remembered hardlink also keeps the object hidden,
as described above.

A `.coopignore` applies below its folder, including to repositories nested inside it, such as
submodules or worktrees. Coop also reads the `.coopignore` files above the project. It stops
before your home folder, or just below `/` when the project is outside your home. Creating
`sub/.git` therefore does not let an agent reveal `sub/secret.yml` to a new box started in `sub/`.

To share hiding rules across projects, put a `.coopignore` in a parent folder such as
`~/Projects/.coopignore`. Paths in that file start from its own folder: `app/config/*.yml` hides
`config/*.yml` in the `app` project. These rules affect both box visibility and fork merges.
`coop fork merge` refuses changes to a hidden file unless you pass `--force`; isolated forks
have no override.

Coop does not apply `~/.coopignore` to projects below your home folder. It also ignores a
`.coopignore` in a folder owned by another user, such as `/tmp`.

A `.coopignore` must be a readable regular file no larger than 1 MiB. If it is a link, directory
or another file type, Coop cannot read its rules. When it could hide a file that the other
`.coopignore` rules leave visible, Coop refuses to start the box and names the invalid file.

Use `.coopignore` to hide files from the box. A normal `coop run`, `coop loop` or `coop shell`
mounts your whole working tree, and `.gitignore` does not control what the agent can read.
An ignored file such as `serviceAccount.json` is still visible if no hiding rule covers it.
Add it to `.coopignore`.

A normal writable run shares your checkout with the agent. The agent can change Git and editor
files that later run code when you use tools on the host, including files in a new child
repository. Coop adds safeguards to its own host Git commands, but cannot protect arbitrary
commands you run yourself.

Coop's Git commands still follow Git's links to other repository storage: `commondir`, a `.git`
file pointing to another Git folder, linked `objects` or `refs` folders, and
`objects/info/alternates`. An agent can use pointers in `.git` to make these commands read another
repository your user can access. A reset or review can then bring that repository's files into
the box. Use [`coop fork <name> <target> --isolated`](forks.md#isolated-write-forks) for independent
Git storage and checks before committed changes enter your checkout.

### Secrets inside files: coop check-secrets

Use `coop check-secrets` to look for credentials inside files. It reports each finding as
`file:line` and exits 1 when it finds one.

- The scan still reports files hidden by built-in rules or `.coopignore` when Git would commit
  them. Hiding a file from the box does not prevent committing or pushing it.
- `--include-ignored` expands the scan to the whole visible tree.
- If the scan cannot read a file, it fails and names that file. A permission error cannot produce
  a clean result.
- It also lists changed files that can affect what runs on your machine, such as commit hooks,
  editor or agent settings, Compose files and the Makefile. Review them before running anything.
  This separate report never changes the exit code.

If you have reviewed a finding and want this check to ignore it, add the printed entry to
`.coopsecretsignore` at the project root and explain why. The entry identifies that exact
finding without storing the credential. Adding a line above it does not invalidate the entry;
changing the value or moving the file does. The exception applies only to this check. Fork
merges, checkpoint uploads and session redaction still check every finding.

Check your sandbox setup with [`coop doctor`](https://coop.dryga.com/docs.html#doctor).

## Toolchain: .tool-versions

A project may need language tools such as Elixir or Go and services such as Postgres or Redis.
Installing them on every run is slow, can produce different results and is lost when the
container is removed. Define the tools and services once using Dev Containers and Compose.

On an open-network run, the base box installs the versions listed in your `.tool-versions` file
using asdf and caches them in a shared volume. It looks for the file from the working directory
upwards, then falls back to `~/.tool-versions`. A repo with `.tool-versions` needs no
`.agent/Dockerfile` or other setup for this:

```bash
cd ~/code/phoenix-app       # has a .tool-versions
coop claude --egress open   # provisions elixir/erlang/node/… from it, then runs the agent
```

The first install of a new toolchain can be slow (Erlang compiles, for example). After that it is
reused across runs and repos.

Set `COOP_NO_ASDF=1` in `agents/env` to skip installing tools from `.tool-versions`. co:op still
repairs an outdated cached Node shim, the command wrapper that selects Node, when needed to keep
the agent CLIs running.

Installing tools at startup works only with open networking. A filtered run, the default, uses
an image with fixed client versions. The image includes asdf itself but puts no asdf command
wrappers on `PATH` and installs nothing when the box starts. Build your toolchain into an
`.agent/Dockerfile` instead:

- `coop init` writes one when it finds a `.tool-versions`.
- `coop init --stack asdf` writes one later.
- `coop build` builds it.

The default box has no Go, Terraform, Elixir or Rust. When `coop init` finds one of these
languages in a project that has no `.agent/Dockerfile`, for example Go from a `go.mod`, it prints
the commands that pin it in `.tool-versions` and build the project's box. Until then the commit
check cannot format that language's files, so it skips them and prints a warning.

## Your own image: .agent/Dockerfile

```bash
coop init --stack asdf # writes an asdf .agent/Dockerfile (from .tool-versions)
coop build             # builds the image for this project's network mode
```

A repository with an `.agent/Dockerfile` gets a separate image tag, so its image does not replace
another project's. Every `coop`, `coop loop`, `coop fork` and `coop acp` run in that repo uses it.

The generated Dockerfile uses asdf to install the exact versions in `.tool-versions`. Keep those
versions in `.tool-versions` rather than duplicating them in the Dockerfile. For a different
setup, write an `.agent/Dockerfile` that meets [the box contract](#the-box-contract).

When the agent needs a new system package, add it to the `RUN` line and run `coop build`
again. The package becomes part of the image, so it doesn't need installing on each run.

co:op records the image's inputs at build time. If you change `.agent/Dockerfile` or
`.tool-versions` but forget to rebuild, `coop` notices on the next run and reminds you to run
`coop build`.

The selected Dockerfile must be a regular in-repo file, not a symlink.

You can reuse a stage of your app's existing `Dockerfile` or use your own `docker-compose.yml`
for service containers. Set the paths relative to the repository in `.agent/project.yaml`:

| Key | Default |
| --- | --- |
| `box.dockerfile` | `.agent/Dockerfile` |
| `box.compose` | `.agent/compose.yml` |

### Builds and network modes

With filtered networking, the default, review the Dockerfile and the files it copies, then run
`coop build` on the host. Plain `coop build` follows the project's network mode, so it builds the
filtered image unless the project or `COOP_EGRESS` chooses open or offline.
`coop build --egress filtered` asks for it explicitly. This builds on co:op's fixed client image
and uses a separate image tag.

A filtered launch uses an image built from files you approved. Running `coop build` approves the
Dockerfile and the files it copies, for this project, Docker daemon and build settings. If only
co:op's client base changed or the image is missing, the next launch rebuilds those same files by
itself. If the files changed, they wait for you: an agent can edit them, and a project build runs
them with ordinary networking. `.agent/project.yaml` is not one of those files: no build reads it,
so changing a network rule asks only for `coop approve`, and an image never holds it.

When a build of changed files is due, an interactive provider launch in a host terminal can offer
`Build and continue?`, defaulting to No. The prompt shows a saved snapshot of the build files;
review it, because only those saved files are approved for that build. Headless commands and
unattended runs never ask. They stop and name the build to run. `coop update` rebuilds approved
files and leaves changed ones for `coop build`, because an update is not a review. An editor
connects anyway, holds your messages and says what to run; see
[editors](integrations.md#when-the-project-box-is-not-ready).

An editor connection with open networking can build a missing project image automatically.
Ordinary runs do not rebuild an image after its inputs change; they warn, and you run
`coop build --egress open` yourself. Offline launches never run project build instructions
automatically.

Project builds use ordinary Docker networking. The restrictions on the running box do not apply
during the build, and `--egress` selects the runtime image without filtering build traffic. See
[restricted networking](networking.md) for the image checks and the limits of build protection.

After a build, new runs use the rebuilt image. Supervised editor sessions that run the replaced
image restart and reconnect. Other running boxes keep the old image until their next start. An
unchanged image restarts nothing.

### Box-only environment: box.env

Put non-secret defaults for the box under `box.env` and commit them with the project. Values are
literal strings, so quote values that look like numbers. Your `~/.config/coop/agents/env` takes
precedence over these defaults. Names beginning with `COOP_*` are reserved for co:op.

```yaml
box:
  env:
    PGHOST: db
    PGPORT: "5432"
```

### The box contract

You can choose your own base image where the network mode allows it. Filtered runs must inherit
co:op's fixed client base, as described above. An agent box needs the following:

1. It runs as a non-root user. Claude Code refuses `--dangerously-skip-permissions` as root.
2. That user's home is `/home/node`. Coop mounts the selected provider folders from `agents/`
   at `$HOME/.claude`, `$HOME/.codex`, `$HOME/.gemini` and `$HOME/.grok`. On a different base,
   set `COOP_HOME_IN_BOX=/home/<user>`.
3. `claude`, `codex`, `gemini` and `grok` are on `PATH` (the first three need Node), plus the ACP
   adapters if you want `coop acp`. Grok's ACP server is the `grok` binary itself.
4. It runs `git config --system --add safe.directory '*'` so Git can use the repository files
   owned by your host user. They normally appear at the repo's real path; new remote sessions
   use `/workspace` inside their private boxes.

On native Linux, the user inside the image must have the same UID/GID as your host user so it
can read a private checkout and the selected provider's files. co:op builds its managed images
with those IDs automatically. If you supply `COOP_BASE_IMAGE` or another custom image, configure
its user to match. Managed images on Docker Desktop for macOS keep UID/GID 1000.

co:op sets the working directory itself, so no `WORKDIR` is required. A skeleton:

```dockerfile
FROM <your-language-base>
RUN <install your toolchain> \
 && npm install -g @anthropic-ai/claude-code@2.1.285 @openai/codex@0.159.2 @google/gemini-cli@0.62.0 \
      @agentclientprotocol/claude-agent-acp@0.84.0 @agentclientprotocol/codex-acp@2.0.1 \
 && curl -fsSL "https://storage.googleapis.com/grok-build-public-artifacts/cli/grok-1.0.44-linux-$(uname -m).gz" \
      | gunzip > /usr/local/bin/grok && chmod 755 /usr/local/bin/grok \
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

Install Node first if your base does not include it; NodeSource is one option. This example
also needs the standard `useradd`, `usermod` and `groupmod` tools. `coop build` passes your host
UID/GID as build arguments for project Dockerfiles. When building `COOP_BASE_IMAGE` separately,
you must supply those arguments yourself.

This example pins the client versions checked for this co:op release. If you use another base,
update those versions when you update co:op. You can also [inherit co:op's
base](#inherit-coops-base), which supplies the clients for you.

Images based on co:op already include Gemini's settings in the root-owned `/etc/gemini-cli/`
folder. If you build an independent image, add `thinking/low.json`, `thinking/high.json` and
`login.json` there too. Coop needs them to set thinking effort for each call and restrict tools
during sign-in. Gemini ignores system settings inside a user-owned home. Inheriting co:op's base
avoids rebuilding this client setup yourself.

Before starting a command, co:op's current startup scripts check that Claude's selected account
settings and trust configuration are ready. Rebuild images that inherit from co:op to get this
check. An independent startup script does not provide it.

### Inherit co:op's base

Start from co:op's image to keep its client setup and add your own toolchain. `coop build`
selects the base image, builds it if needed and passes it to your Dockerfile:

```dockerfile
ARG COOP_BASE_IMAGE=coop-box   # coop build overrides this with the resolved base
FROM ${COOP_BASE_IMAGE}        # agent CLIs + ACP adapters, asdf, browser libs, non-root node — all inherited
USER root
RUN <install your toolchain>
USER node
```

Each box definition gets its own base-image tag, `coop-box:<definition>`, which `coop build`
prints. Two co:op versions on one machine can therefore keep separate base images. After an
upgrade, the next launch builds the new base if needed.

A plain `docker build` of this file needs that tag passed in:
`--build-arg COOP_BASE_IMAGE=coop-box:<definition>`. `docker image ls coop-box` lists them.

Builds also remove old images that co:op owns. An image is eligible only when no run has used it
for 14 days and no container references it. Coop prints the images it removes. This covers its
versioned `coop-box`, `coop-clients` and `coop-network` images, plus `<project>-filtered` images
when you explicitly build the project's box. The latter are tagged by their client base image.

Older images without co:op's ownership mark are left alone. Every launch records which image it
used, so an older co:op version you still run keeps its base. Images you created yourself are
never removed, regardless of their tags.

### Reusing a devcontainer

If a repo already has a `.devcontainer/`, reuse its image as your base and add the agent layer on
top:

```dockerfile
FROM your-devcontainer-image          # the team's source of truth for the env
RUN npm install -g @anthropic-ai/claude-code@2.1.285 @openai/codex@0.159.2 @google/gemini-cli@0.62.0 \
      @agentclientprotocol/claude-agent-acp@0.84.0 @agentclientprotocol/codex-acp@2.0.1 \
 && curl -fsSL "https://storage.googleapis.com/grok-build-public-artifacts/cli/grok-1.0.44-linux-$(uname -m).gz" \
      | gunzip > /usr/local/bin/grok && chmod 755 /usr/local/bin/grok \
 && git config --system --add safe.directory '*'
USER <the devcontainer's non-root user>
# If that user's home isn't /home/node, run with COOP_HOME_IN_BOX=/home/<user>.
```

The devcontainer defines your tools, features and how to reproduce the environment. co:op adds
secret hiding, its container restrictions, the task queue and the supervisor that runs it.
A devcontainer alone mounts the whole workspace, including `.env`. A malicious project running
under `--dangerously-skip-permissions` can send the contents of `~/.claude` outside your machine.
Do not rely on the devcontainer alone to protect those files.

## Project services

Services such as databases run in separate containers alongside the box. None are added by
default. `coop init` asks which ones you want, or you can pass `--services postgres,redis`.
It writes their configuration to `.agent/compose.yml`. In a project with filtered network
access, it also asks for a network rule for each service in `.agent/project.yaml`. Run
`coop approve` once so the box can reach them.

```bash
coop up                      # starts the configured Compose services, waits until ready
coop claude                  # the box reaches each service by its Compose name
coop down                    # stop services; stored data is kept
coop down --delete-volumes   # stop them and permanently delete their volumes (asks first)
coop init --services         # add another service to a project that already has some
```

Services run as their own containers. An open box joins their private network; a filtered box
reaches each service only through its approved rule. Connect with a URL such as
`DATABASE_URL=postgres://postgres:dev-password@db:5432/app_dev`, and put it in `agents/env`.

### Development and loop stacks

Your development services keep the same workspace identity and data between runs. A loop gets
its own Compose project, network, volumes and published ports for the entire run. It reuses them
across task and provider retries, then removes them when the run ends. A killed loop or editor
session cannot do that, so the next `coop acp`, loop or `coop down` in that checkout does, and
leaves alone the services of a session that is still running. Both stacks can run in one checkout
without sharing service data or ports. They still share the source files, so edits are visible
through both stacks' mounts.

### Starting and stopping

Before starting services, `coop up` reads their names from Compose. When they are ready, it
prints a message such as `✓ Services ready: db, redis`, using the actual service names in Compose
order, followed by any URLs it published. If it cannot read the service list, co:op starts
nothing and does not report that the services are ready.

`coop down --delete-volumes` first asks the runtime which volumes belong to this project.
It lists each volume and its contents, then asks before deleting anything. The default answer
is No; `-y`/`--yes` confirms without a prompt. It never assumes consent because there is no
terminal. An `external: true` volume or a project file mounted from the host is never deleted.

The next `coop up` or box launch applies changes to the Compose file, including stopping
services you removed. `coop down` also stops those removed services. Each workspace gets a
Compose project name derived from its own identity, so repositories with the same folder name
remain separate.

Agents can change data in services they can reach, so use disposable development databases.
`coop down` stops the services and keeps their volumes. `coop down --delete-volumes` asks before
permanently deleting the project's service volumes.

The shared `coop-cache` volume at `~/.cache` keeps downloads between runs. Claude's MCP logs
at `~/.cache/claude-cli-nodejs` are kept separately: each run starts with an empty log folder,
so one project's logs cannot enter another project's box.

### Volumes from outside the project

An `external: true` or custom-named volume may contain another project's data. When you run
`coop up` in a terminal, it shows the actual Docker volume name, which services will use it and
whether they can write to it. It then asks you to approve access for this repository. The review
also shows the selected Docker daemon and, for an existing plain local volume, where it stores
the data.

- Only plain local volumes are supported. Coop refuses a volume backed by a bind mount, driver
  options or a plugin; mount an explicit repository path instead.
- Coop creates a missing custom-named volume as plain local storage only after you answer Yes.
- An approval does not automatically transfer to a different daemon or a replaced volume.
- Answering No or closing the input stops the command before Compose runs.
- Automatic box launches, filtered service containers and `coop up` without a terminal refuse
  these volumes, even if you approved them before.

Named volumes that belong only to this project need no approval prompt.

### Secret files in services

Services get the same secret hiding as the box. A mount of `.env`, `*.key` or a path hidden by
`.coopignore` cannot expose its contents to the service. This prevents an agent-written Compose
file from passing those secrets to a container the agent controls.

If a service needs a hidden file, such as a generated development TLS key for Keycloak, mount
that individual file read-only with `:ro` or `read_only: true`. Run `coop up` in a terminal to
review the eligible files and approve them. Hidden directories and files also reachable through
writable mounts stay protected. Use read-only mounts for the individual
secret files and named volumes for data the service needs to change.

The approval is tied to:

- The exact Compose file you reviewed. Coop stores approval outside the repo, so an agent cannot
  update it by editing the file. After any edit, box launches hide these files until you
  approve again. Coop explains which files are hidden and why.
- This checkout's private identity. Copying the Compose file or its marker does not transfer
  approval to another checkout. Older approvals based only on file content need to be renewed
  with `coop up`; automatic starts hide these files until then.
- The exact files you reviewed. A new secret added under an approved directory mount stays
  hidden until you approve that file too.

When you approve secret files or outside volumes for a service, co:op also records the exact
local Docker image ID shown in the review. It starts that image with pulling disabled.
Changing what a tag points to cannot give a different image the same access without review.

- Run `coop up` in a terminal to review an updated local tag. If you decline, the old approval
  stays in place and this startup stops.
- If the approved image was removed, startup stops before creating a container. Pull the tag,
  then run `coop up` to review it again.
- Before asking for a first approval, Coop may pull an image that is not cached, using Docker's
  existing registry login. If you decline, the service does not start or receive access. The
  downloaded image stays in Docker's cache.

Services without access to approved secret files or outside volumes keep their normal image
update behavior.

Coop gives Compose a private Docker client configuration. It keeps the registry login details,
including a validated `DOCKER_AUTH_CONFIG`, but leaves out proxy credentials from your host
configuration so they cannot reach the services.

List the files your services need in the repo, so teammates can review the access
request before a service fails to start:

```yaml
# .agent/project.yaml — committed with the repo
services:
  require_real_files:
    - dev/keycloak/certs/generated/tls.key  # a generated dev TLS key Keycloak reads
```

The list helps reviewers recognize expected files, but does not grant access. An agent can edit
it, so Coop still asks you to approve the files. In that prompt:

- Files listed by the repo are marked as expected.
- Files the repo did not list appear first so you can review the unexpected access.
- Files added since your last approval are identified.

You must approve access separately on each machine.

### One URL inside and out

Some services need the same URL in your host browser and in the app running inside the box.
An OIDC issuer such as Keycloak is one example. Give the service an `expose:` port, which is
normally container-only, in `.agent/compose.yml`:

```yaml
services:
  keycloak:
    image: quay.io/keycloak/keycloak
    expose: ["8443"]                     # coop publishes this to a stable, per-workspace localhost port
    labels:
      coop.service.scheme: https         # scheme for COOP_SERVICE_*_URL (default http)
```

co:op assigns a stable host port for each development workspace or loop run. It derives the
port from the owner, service and container port so concurrent stacks have separate ports.
The host port listens only on loopback. Inside the box, a small TCP forwarder makes
`https://localhost:<port>` reach the same Keycloak service. Both sides therefore use the same
issuer URL without `host.docker.internal` or broader network access.

The box receives `COOP_SERVICE_KEYCLOAK_URL=https://localhost:<port>`, using the scheme from the
label. Host tools can read all workspace service URLs from `coop fork ls --json`.

### What a Compose file may do

Coop sends `.agent/compose.yml` to your host's Docker daemon to create the service containers.
It validates the file before `coop up` and before every networked box launch.

An agent could replace a checked mount folder with a link to another part of your machine
before Docker opens it. To prevent that race, co:op takes an exclusive lock while checking the
mounts and asking Docker to start the services. Boxes that can edit the checkout hold a matching
shared lock. `coop up` waits until it can take the exclusive lock, then starts the separate
development stack.

Coop accepts these service settings:

- an `image`
- inline `environment`
- named volumes or repo-relative binds
- `healthcheck`
- `depends_on`
- loopback-only published ports

Coop refuses settings that give a service access beyond the allowed project container and
explains which setting failed. These include:

- `privileged`
- `cap_add`
- a host bind like `/:/host` or `/var/run/docker.sock`
- `network_mode: host`
- `env_file`
- `build`
- a port published on `0.0.0.0`
- a symlink that points outside the allowed project path
- bind options that relabel host files or change mount propagation

A read-only session, such as an investigation or review, can still use service containers.
Every service mount of the repository must also be read-only, using `:ro` or
`read_only: true`. Otherwise the service could change files the agent is not allowed to write.
The mount source must already exist; Compose cannot create a host directory for a read-only
session.

When a service mounts the repo or one of its directories, Coop applies the same hiding rules
as in the box. It prevents reads of `.env`, keys and paths hidden by `.coopignore`,
including when the mount source is itself a hidden file. The file-approval rules above still
apply. An agent can create `.agent/compose.yml` for you, but its configuration must pass these
checks before Coop starts it.

Stop any Compose service that can write to the checkout before starting another service
stack. Coop's error names the writable mount and suggests `coop down`. Coop also refuses mounts
nested inside a writable service mount: on restart, Docker could open a path that the service
had replaced.

Coop automatically starts only configurations within these restrictions. Run other
configurations yourself.

## Dev servers in your browser

When the agent builds a website in the box, you can open it on your machine. List the ports the
dev server listens on in `.agent/project.yaml`:

```yaml
serve:
  ports: [5173]        # what the server binds INSIDE the box
```

Each box for the repo (`coop acp`, `coop run`) maps these ports to stable host ports based on
the repo's path. The URL stays the same between launches and differs between projects, so one
Zed agent definition can serve all of them. When an ACP thread starts, co:op posts a browser link
for each port and for the [project services](#project-services) it starts. It also names problems
it found while starting, such as a saved sign-in that no longer works. Terminal runs print the
port links on stderr.

- The dev server must bind `0.0.0.0` inside the box (`vite --host`, `next dev -H 0.0.0.0` and so
  on). A server on the container's localhost isn't reachable through the mapping. In an editor
  thread, co:op tells you when a server starts: it names the command and the link, or warns you
  when the server is only on localhost.
- Ports bind to your localhost only, never the LAN.
- Publishing needs networking: `filtered` (the default), which publishes the port on the run's
  gateway, or `open`. See [restricted networking](networking.md).
- A host port already in use is skipped with a note. `COOP_SERVE_URL_<port>` still carries the
  workspace's assigned URL for configuration and discovery. An editor session says so in the thread
  instead of posting a link, and takes the port when it frees.
- In an editor session the link always reaches the box the thread is using, including after you
  switch provider or account. The session holds the port and forwards it, and its other boxes keep
  to private ports.
- A box restart (a credential switch, a rebuild) takes the dev server down with the old box. The URL
  stays the same, so run the server again.

## Keeping the box current

```bash
coop update              # self-update coop, then prepare this project's box image
coop update --self-only  # just upgrade the coop binary
coop update --box-only   # just prepare this project's box image
```

`coop update` prepares the image for the project's network mode, using the definitions of the
co:op version it installed. A filtered project, the default, keeps co:op's pinned client base. An
open or offline project gets fresh OS packages. See
[stable and fresh rebuilds](#stable-and-fresh-rebuilds).

If GitHub has a newer release, `coop update` downloads its versioned archive and checksum,
verifies the archive and replaces the `coop` binary in one atomic operation. The running process
can finish safely; the new binary takes effect on your next `coop` run.

When the binary is not replaced:

- A dev or source build, or a binary that is already current or newer, skips the self-update with a
  note and still prepares the image.
- If `coop` is installed somewhere it cannot write (a package-manager prefix), the self-update
  fails with an error that tells you to update co:op with the tool that installed it. `coop update`
  still prepares the image, then exits 1. `coop update --self-only` stops at the error.

### A newer co:op repairs its own box

If your installed co:op uses a different box definition from the current shared image, the next
interactive launch rebuilds that image before starting the agent. This can happen after
`coop update --self-only` or `go install`. The `Checking the Coop box` section names both versions
and ends with `✓ Box updated`. Filtered runs use the client image checked for filtered networking,
so they do not trigger this repair.

- A current image prints nothing.
- An old image with the same box definition gets a reminder to run `coop update` under that
  heading. Coop does not rebuild it without being asked.
- An image co:op did not build (`COOP_IMAGE`) is never replaced.
- If the repair cannot run because Docker is not answering, the launch stops there and says what to
  start before you repeat the same command.

### Stable and fresh rebuilds

| Command | Base image | Result |
| --- | --- | --- |
| `coop build` (stable) | fixed to a specific image digest | rebuilding uses the same OS and runtime |
| `coop update`, filtered project | co:op's pinned client base | the OS and runtime change only with a co:op release |
| `coop update`, open or offline project (fresh) | uses the current `node:24-slim` image and rebuilds with `--pull --no-cache` | updates the OS packages and runtime to the newest available versions |

### Agent clients

All standard co:op boxes use the same fixed versions of the agent CLIs and ACP adapters,
including plain runs, filtered runs, loops, presets and editor sessions. These are the versions
checked for this co:op release. Coop installs them from its built-in lockfile and verifies Grok's
binary against its checksum. Each client's own updater is disabled, so the versions change only
with co:op.

With open or offline networking, you can use other client versions in a custom image selected
with `COOP_IMAGE` or built from an `.agent/Dockerfile` on another base. Those clients are not
covered by the checks for co:op's own image. Filtered runs require co:op's fixed client base
and reject images that replace its clients.
