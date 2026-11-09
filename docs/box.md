# The box

The box is the container your agent runs in. co:op mounts your project at its real path, but
leaves your home folder and SSH keys unmounted. By default its networking is filtered, inside or
outside a co:op project, so the box reaches the agent's provider and the destinations you approved.
Add `--egress open` for unrestricted access or `--egress offline` for none. See [restricted
networking](networking.md#network-modes). For an overview, see
[the sandbox](https://coop.dryga.com/docs.html#sandbox), [hiding
secrets](https://coop.dryga.com/docs.html#secrets) and [toolchain and
services](https://coop.dryga.com/docs.html#toolchain).

`coop run` and `coop shell` pass piped or redirected input to the command. As with `docker run -i`
or `ssh`, that input is read as soon as the box starts, even if the command never reads it. In a
shell `while read` loop, give each `coop run` its own input, as in
`coop run -- make test < /dev/null`, or it takes the lines meant for the loop.

## Hiding secrets

co:op hides files and directories whose names look like secrets, including `.env`, `*.tfvars`,
`*.pem`, `*.key`, `id_rsa*`, `secrets/`, `.ssh`, dotfiles ending in `secrets` such as `.shell-secrets`, and config
files such as `secrets.env`. Source and docs like `secrets.go` and `secrets.md` stay readable, and
templates (`*.example`, `*.sample`, `*.template`) stay visible.

co:op protects these files in one of two ways. Projects get eBPF protection automatically on
supported Docker runtimes: APFS projects on a Mac, enforced in the Docker VM, and ext4 or xfs
projects on Linux. Docker must use cgroup v2 with the `cgroupfs` or `systemd` cgroup driver. The
kernel must run the BPF security module and have every file hook the filter attaches to, which
recent kernels such as Fedora 44's do. With the `systemd` driver, co:op asks systemd over the system
bus for the cgroups that hold its boxes. Other filesystems, rootless Docker, older kernels (such as
RHEL 10's 6.12) and incompatible container options use mount protection; `coop doctor` names the
reason.

With eBPF protection, the box can't read or change a secret, including through another letter
case, an equivalent Unicode spelling, a symlink or an existing hardlink. It also can't connect to a
Unix socket in a hidden folder, such as one another container serves from the project. Public files
stay writable, ordinary public hardlinks work, and `.coopignore` stays readable and read-only.

Each hidden file already in the project, up to 1,000 per launch, also gets an empty read-only file
over it. A tool that opens one, such as Terraform with `terraform.tfvars`, reads it as empty and
keeps working, and the secret stays out of reach. The box can't delete or rename a folder that
holds a covered file. In a repository, only a file Git ignores gets a cover, because an agent's
`git add -A` would commit a tracked or new file as empty. Add your secrets to `.gitignore`.

eBPF protects everything else: other letter cases, hardlinks, files past the limit and hidden
directories. Reading or changing one of those returns `Operation not permitted`. A file created
after the box starts is protected at once when its name is on the built-in list, such as `*.pem`,
`*.key` or `.env`: the box can create, overwrite or delete such a file, so installers and key
generators keep working, but it can't read one or give it another name. A new file that only
`.coopignore` hides, and a new hidden folder, need a restart.

`stat` still works on a hidden file without a cover, so `find` and tools that list a folder keep
working. It shows the file's real size, times, mode, owner and link count. The box still can't
read the contents, and extended attributes read as empty. A hidden symlink shows no target, and
`readlink` on it fails. `stat` fails on a hidden directory and on everything inside it, and `ls -l`
shows that directory's entry with an error.

For tools that scan the repository, co:op sets `COOP_BOX=1` inside the box. When it is set,
skip paths that return a permission error (`EPERM` or `EACCES`) during metadata lookup,
directory traversal or file reads. Skip a denied directory's subtree. Report skipped paths so
the result makes clear which files were checked, and keep reporting other errors normally.
A denied path is unreadable, not an empty file. Don't rely on a successful `stat` or a list of
tracked files to mean that their contents can be read. co:op does not expose a hidden-path list
inside the box. Handle the filesystem error rather than trying to reproduce its hiding rules.

co:op builds the eBPF helper inside Docker, so you don't need any host packages, filesystem changes
or kernel settings.

Launches don't test the protection itself, so run `coop doctor` after you update Docker or OrbStack.
It tries to read test secrets in a real box. See
[filesystem protection](#filesystem-protection-contract) for how eBPF protection behaves when you
edit files on the host.

With mount protection, co:op covers a secret file or directory with a read-only empty one, so `stat`
shows the cover's size and times, not the real ones. On a case-insensitive disk, it also covers the
uppercase and lowercase spellings of a hidden filename with up to 11 letters, and every spelling of
`.coopignore`.

Mount protection lays its covers when the agent or service starts. A secret you create on the host
afterwards at a public path, such as a new `late.pem` or `.env.local`, stays readable in that box until
it restarts. A file created inside a folder that is already hidden stays out of reach, because the box
sees only the folder's empty cover. So create secrets before you start an agent, keep them in a hidden
folder such as `secrets/`, or restart the agent after you add one. The box can't rename a hidden
folder, but it can rename a folder that only holds a covered file; the file stays covered at its new
path.

A longer filename, or a project with too many spellings, can exceed the mount limit. The box or
service could then read the file under another letter case, so co:op refuses to launch the agent
or service. Either:

- move the file into a hidden folder, or
- run `coop approve --case-exposure` to review the listed files and accept that they stay readable
  under another letter case with mount protection. co:op remembers your approval. Ordinary
  `coop approve` reviews network access and does not accept any file exposure.

That approval covers those exact paths, including their future contents, for new agent and
service runs. Newly exposed paths need another approval, and running boxes stay unchanged.
`.coopignore` cannot be approved this way. `COOP_ALLOW_CASE_EXPOSED_SECRETS=1` exposes the files
for a single run. eBPF protects these paths without either kind of approval.

### Filesystem protection contract

This contract describes eBPF protection. Mount protection hides only what exists at start, as
described above, and a launch that uses it says so under "Protecting secrets".

co:op trusts the host and its editors. Each time it starts an agent or service with eBPF protection,
it captures the current hiding rules and the protected files and folders, and it keeps checking
every file name the box opens against the built-in list. In practice:

- Ordinary edits to public files work immediately.
- A path that is already protected stays protected when you edit it or save it atomically on the
  host, even when a parent folder is replaced. Existing hardlinks keep their protection when the
  original file is replaced.
- A new file whose name is on the built-in list, such as `late.pem` or `.env`, is unreadable in the
  box as soon as it exists, in any letter case or Unicode spelling. The box can still create,
  overwrite, truncate or delete such a file, but it can't read one or give it another name.
- Restart the affected agent or service after you change `.coopignore`, create a hidden folder,
  mark an already-public file or folder as secret, or create an unprotected hardlink to a secret
  from the host. Protection that is already running does not reload its rules or revoke memory
  mappings that were already readable.
- To save a secret atomically, write the temporary file outside the shared tree or inside an
  already hidden folder. A new temporary file at a public path is protected only when its own name
  is on the built-in list.
- Protection follows the file or folder itself, not its path. Removing its hiding rule alone doesn't
  make a hidden secret public while it still has another hardlink in the project. Remove the other
  hardlinks and restart before you make the original file public.
- When you approve a hidden file for a service, the service can read that file, its other hardlinks
  and your later in-place edits on the host. The approval never allows writes. If the file is
  replaced, start the service through co:op again so that it can read the new file.

A separate helper runs the kernel enforcement, and the agent gets no BPF privileges. If the helper
exits, access to the protected share is denied until co:op removes that agent or service. On Linux
that covers the project's whole disk, so the box usually can't run its own programs either.

With the `systemd` driver the helper talks to systemd over the system bus, which Docker's default
AppArmor profile blocks, so on an AppArmor host such as Ubuntu it runs without AppArmor. Run
`coop setup apparmor` once to run it under co:op's own profile instead: Docker's default rules,
which deny mounts and writes to kernel settings, plus the two systemd calls that start and stop
co:op's slices. Other system bus calls stay blocked. AppArmor can't check those two calls'
arguments, so the profile doesn't limit what the helper could do through systemd. The command
installs the profile in `/etc/apparmor.d` through Docker, without sudo, and it loads on every boot.
`coop doctor` suggests it when it would help. Stopped
service containers keep their protection for a later restart. After a VM reboot, co:op's guard runs
before an image's start command and its healthchecks, and refuses to run image code under the old
protection. Start the service through co:op to get fresh protection.

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
all, and co:op warns about it at launch.

An exact-path rule also hides its final file or directory name everywhere that `.coopignore`
applies, so renaming the parent directory can't reveal the secret.

If a box moves a hidden file or folder somewhere no rule matches, the next box and co:op's own tools
still hide it wherever it appears in the project. This includes a file hidden by `config/*.yml` and
moved by `mv config cfg`, or a folder moved outside a nested `.coopignore`'s scope. At its original
path, the current rules decide whether it's visible.

A `.coopignore` applies below its folder, including to repositories nested inside it, such as
submodules or worktrees. co:op also reads the `.coopignore` files above the project, stopping
before your home folder, or just below `/` when the project is outside your home. Creating
`sub/.git` therefore doesn't let an agent reveal `sub/secret.yml` to a new box started in `sub/`.

To share hiding rules across projects, put a `.coopignore` in a parent folder such as
`~/Projects/.coopignore`. Paths in that file start from its own folder: `app/config/*.yml` hides
`config/*.yml` in the `app` project. These rules affect both box visibility and fork merges:
`coop fork merge` refuses changes to a hidden file unless you pass `--force`, and isolated forks
have no override.

co:op doesn't apply `~/.coopignore` to projects below your home folder, and it ignores a
`.coopignore` in a folder owned by another user, such as `/tmp`.

A `.coopignore` must be a readable regular file no larger than 1 MiB. If an invalid `.coopignore`
could hide a file that no other rule hides, co:op refuses to start the box and names the invalid
file.

A normal `coop run`, `coop loop` or `coop shell` mounts your whole working tree, and `.gitignore`
doesn't control what the agent can read. A file Git ignores, such as `serviceAccount.json`, stays
visible unless a hiding rule covers it, so add it to `.coopignore`.

A normal writable run shares your checkout with the agent. The agent can change Git and editor
files that later run code when you use tools on the host, including files in a new child
repository. co:op adds safeguards to its own host Git commands, but cannot protect other commands
you run yourself.

co:op's Git commands don't follow pointers in `.git` that lead to another repository, since the
agent can write them. They still follow the ones Git creates for linked worktrees and submodules,
which Git can check from both ends. A repository that keeps its Git folder elsewhere
(`git init --separate-git-dir`) or links its `objects`, `refs` or `packed-refs` to another place is
refused, and the message says how to fix it.

Shared clones (`git clone --shared` or `--reference`) keep working. The `objects/info/alternates`
file they use lets Git read another repository's objects, but only ones whose hash is already
known. These checks run before each co:op Git command, so an agent that is still running can swap
a pointer in between. For independent Git storage that is checked before committed changes enter your
checkout, use [`coop fork <name> <target> --isolated`](forks.md#isolated-write-forks).

### Secrets inside files: coop check-secrets

Use `coop check-secrets` to look for credentials inside files. It reports each finding as
`file:line` and exits 1 when it finds one.

- The scan still reports files hidden by built-in rules or `.coopignore` when Git would commit
  them. Hiding a file from the box does not prevent committing or pushing it.
- `--include-ignored` expands the scan to the whole visible tree.
- If the scan can't read a file, it fails and names that file, so a permission error can't produce
  a clean result.
- It also lists changed files that can affect what runs on your machine, such as commit hooks,
  editor or agent settings, Compose files and the Makefile. Review them before running anything.
  This separate report never changes the exit code.

To make this check ignore a finding you've reviewed, add the printed entry to `.coopsecretsignore`
at the project root and explain why. The entry identifies that exact finding without storing the
credential. Adding a line above the finding doesn't invalidate the entry, but changing the value or
moving the file does. The exception applies only to this check. Fork merges, checkpoint uploads and
session redaction still check every finding.

Check your sandbox setup with [`coop doctor`](https://coop.dryga.com/docs.html#doctor).

## Toolchain: .tool-versions

A repo with a `.tool-versions` file at its root and no `.agent/Dockerfile` gets its tools built
into an image of their own: co:op's box plus the versions the file lists, installed with asdf.
Every box of the project uses that image, and no box can change it. An open-network run builds it
when it is missing, so a repo with `.tool-versions` needs no other setup:

```bash
cd ~/code/phoenix-app       # has a .tool-versions
coop claude --egress open   # builds elixir/erlang/node/… into the project's image, then runs the agent
```

co:op reads only the `.tool-versions` at the repository root. The first build of a new toolchain
can be slow (Erlang compiles, for example). Later runs reuse the image, and projects that pin the
same versions share it. Changing `.tool-versions` builds a new image on the next open run. A fork
runs with its project's tools, so a pin a fork changes takes effect after it merges.

Filtered and offline runs never build by themselves. With filtered networking, the default, the
box uses co:op's locked image without your tools, so build your toolchain into an `.agent/Dockerfile`
instead:

- `coop init` writes one when it finds a `.tool-versions`.
- `coop init --stack asdf` writes one later.
- `coop build` builds it.

An offline run uses the tools image once `coop build` has built it.

The default box has no Go, Terraform, Elixir or Rust. When `coop init` finds one of these
languages in a project that has no `.agent/Dockerfile`, for example Go from a `go.mod`, it prints
the commands that pin it in `.tool-versions` and build the project's box. Until then the commit
check cannot format that language's files, so it skips them and prints a warning.

## Your own image: .agent/Dockerfile

```bash
coop init --stack asdf # writes an asdf .agent/Dockerfile (from .tool-versions)
coop build             # builds the image for this project's network mode
```

A repository with an `.agent/Dockerfile` gets its own image tag, and every `coop`, `coop loop`,
`coop fork` and `coop acp` run in that repo uses it.

The generated Dockerfile installs the exact versions in `.tool-versions`, so keep them there and
don't repeat them in the Dockerfile. For a different setup, write an `.agent/Dockerfile` that meets
[the box contract](#the-box-contract).

When the agent needs a new system package, add it to the `RUN` line and run `coop build` again. If
you change `.agent/Dockerfile` or `.tool-versions` but forget to rebuild, `coop` notices on the next
run and reminds you to run `coop build`.

The selected Dockerfile must be a regular file inside the repo, not a symlink. The same goes for the
ignore files a build reads, `.dockerignore` and the one named after the Dockerfile.

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
`coop build --egress filtered` asks for it explicitly.

A filtered launch uses an image built from files you approved. Running `coop build` approves the
Dockerfile and the files it copies, for this project, Docker daemon and build settings. If only
co:op's client base changed or the image is missing, the next launch rebuilds those same files by
itself. If the files changed, co:op waits for you, because an agent can edit them and a project
build runs them with ordinary networking. Changing a network rule asks only for `coop approve`,
because no build reads `.agent/project.yaml` and an image never holds it.

When changed files need a build, an interactive provider launch in a host terminal can offer
`Build and continue?`, defaulting to No. The prompt shows a saved snapshot of the build files.
Review it, because only those saved files are approved for that build. Headless commands and
unattended runs never ask: they stop and name the build to run. `coop update` rebuilds approved
files and leaves changed ones for `coop build`, because an update is not a review. An editor
connects anyway, holds your messages and says what to run. See
[editors](integrations.md#when-the-project-box-is-not-ready).

An editor connection with open networking can build a missing project image automatically.
Ordinary runs don't rebuild an image after its inputs change: they warn, and you run
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
literal strings, so quote values that look like numbers. Your `~/.config/coop/agents/env`
overrides these defaults. Names beginning with `COOP_*` are reserved for co:op.

```yaml
box:
  env:
    PGHOST: db
    PGPORT: "5432"
```

### The box contract

You can choose your own base image where the network mode allows it, but filtered runs must inherit
co:op's fixed client base. An agent box needs the following:

1. It runs as a non-root user. Claude Code refuses `--dangerously-skip-permissions` as root.
2. That user's home is `/home/node`. co:op mounts the selected provider folders from `agents/`
   at `$HOME/.claude`, `$HOME/.codex`, `$HOME/.gemini` and `$HOME/.grok`. On a different base,
   set `COOP_HOME_IN_BOX=/home/<user>`.
3. `claude`, `codex`, `gemini` and `grok` are on `PATH` (the first three need Node), plus the ACP
   adapters if you want `coop acp`. Grok's ACP server is the `grok` binary itself.
4. It runs `git config --system --add safe.directory '*'` so Git can use the repository files
   owned by your host user. They normally appear at the repo's real path. New remote sessions
   use `/workspace` inside their private boxes.

On native Linux, the user inside the image must have the same UID/GID as your host user so it
can read a private checkout and the selected provider's files. co:op builds its managed images
with those IDs automatically. If you supply `COOP_BASE_IMAGE` or another custom image, configure
its user to match. Managed images on Docker Desktop for macOS keep UID/GID 1000.

co:op sets the working directory itself, so no `WORKDIR` is required. Here is a skeleton:

```dockerfile
FROM <your-language-base>
RUN <install your toolchain> \
 && npm install -g @anthropic-ai/claude-code@2.1.319 @openai/codex@0.166.0 @google/gemini-cli@0.67.0 \
      @agentclientprotocol/claude-agent-acp@0.101.0 @agentclientprotocol/codex-acp@2.6.0 \
 && curl -fsSL "https://storage.googleapis.com/grok-build-public-artifacts/cli/grok-1.0.77-linux-$(uname -m).gz" \
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

Install Node first if your base doesn't include it. This example also needs the standard `useradd`,
`usermod` and `groupmod` tools. `coop build` passes your host UID/GID as build arguments for project
Dockerfiles. When you build `COOP_BASE_IMAGE` separately, you must supply those arguments yourself.

This example pins the client versions checked for this co:op release. If you use another base,
update those versions when you update co:op. You can also [inherit co:op's
base](#inherit-coops-base), which supplies the clients for you.

Images based on co:op already include Gemini's settings in the root-owned `/etc/gemini-cli/`
folder. If you build an independent image, add `thinking/low.json`, `thinking/high.json` and
`login.json` there too. co:op needs them to set thinking effort for each call and restrict tools
during sign-in, and Gemini ignores system settings inside a user-owned home.

Before starting a command, co:op's startup scripts check that Claude's selected account settings
and trust configuration are ready. Rebuild images that inherit from co:op to get this check. An
independent startup script does not provide it.

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
prints. After an upgrade, the next launch builds the new base if needed.

A plain `docker build` of this file needs that tag passed in:
`--build-arg COOP_BASE_IMAGE=coop-box:<definition>`. `docker image ls coop-box` lists them.

Builds also remove old images that co:op owns: its versioned `coop-box`, `coop-clients` and
`coop-network` images, plus `<project>-filtered` images when you explicitly build the project's
box. co:op removes an image only when no run has used it for 14 days and no container references
it, and it prints the images it removes.

co:op leaves older images without its ownership mark alone. Every launch records which image it
used, so an older co:op version you still run keeps its base. co:op never removes images you
created yourself, regardless of their tags.

### Reusing a devcontainer

If a repo already has a `.devcontainer/`, reuse its image as your base and add the agent layer on
top:

```dockerfile
FROM your-devcontainer-image          # the team's source of truth for the env
RUN npm install -g @anthropic-ai/claude-code@2.1.319 @openai/codex@0.166.0 @google/gemini-cli@0.67.0 \
      @agentclientprotocol/claude-agent-acp@0.101.0 @agentclientprotocol/codex-acp@2.6.0 \
 && curl -fsSL "https://storage.googleapis.com/grok-build-public-artifacts/cli/grok-1.0.77-linux-$(uname -m).gz" \
      | gunzip > /usr/local/bin/grok && chmod 755 /usr/local/bin/grok \
 && git config --system --add safe.directory '*'
USER <the devcontainer's non-root user>
# If that user's home isn't /home/node, run with COOP_HOME_IN_BOX=/home/<user>.
```

co:op adds secret hiding, its container restrictions, the task queue and the supervisor that runs
it. A devcontainer alone mounts the whole workspace, including `.env`, and a malicious project
running under `--dangerously-skip-permissions` can send the contents of `~/.claude` outside your
machine. Don't rely on the devcontainer alone to protect those files.

## Project services

Services such as databases run in separate containers alongside the box. None are added by
default. `coop init` asks which ones you want, or you can pass `--services postgres,redis`, and it
writes their configuration to `.agent/compose.yml`. In a project with filtered network access, it
also asks for a network rule for each service in `.agent/project.yaml`. Run `coop approve` once so
the box can reach them.

```bash
coop up                      # starts the configured Compose services, waits until ready
coop claude                  # the box reaches each service by its Compose name
coop down                    # stop services; stored data is kept
coop down --delete-volumes   # stop them and permanently delete their volumes (asks first)
coop init --services         # add another service to a project that already has some
```

An open box joins the services' private network. A filtered box starts the services its approved
rules name, so it needs no `coop up` first, and reaches each one only through its rule. Connect with
a URL such as `DATABASE_URL=postgres://postgres:dev-password@db:5432/app_dev`, and put it in
`agents/env`.

### Development and loop stacks

Your development services keep the same Compose project and data between runs. A loop gets its own
Compose project, network, volumes and published ports for the entire run. It reuses them across
task and provider retries, then removes them when the run ends. If a loop or editor session is
killed, the next `coop acp`, loop or `coop down` in that checkout removes them instead, and leaves
alone the services of a session that is still running. Both stacks can run in one checkout without
sharing service data or ports. They still share the source files, so edits are visible through both
stacks' mounts.

### Starting and stopping

When services are ready, `coop up` prints a message such as `✓ Services ready: db, redis`, using the
actual service names in Compose order, followed by any URLs it published. If it can't read the
service list from Compose, it starts nothing.

`coop down --delete-volumes` lists each of this project's volumes and its contents, then asks before
permanently deleting anything. The default answer is No, and `-y`/`--yes` confirms without a prompt.
Having no terminal never counts as consent. An `external: true` volume or a project file mounted
from the host is never deleted.

The next `coop up` or box launch applies changes to the Compose file, including stopping services
you removed, and `coop down` also stops those services. Each workspace gets a Compose project name
derived from its path, so repositories with the same folder name stay separate.

Agents can change data in services they can reach, so use disposable development databases.

Each project's `~/.cache` is a volume of its own that keeps downloads between runs, so one
project's box cannot change the modules another project's box uses. Forks share their project's
cache; isolated forks share one of their own. These volumes are named `coop-cache-<project>-<id>`;
remove one with `docker volume rm` when you no longer need the project. Claude's MCP logs at
`~/.cache/claude-cli-nodejs` are kept separately: each run starts with an empty log folder.

### Volumes from outside the project

An `external: true` or custom-named volume may contain another project's data. When you run
`coop up` in a terminal, it shows the actual Docker volume name, which services will use it and
whether they can write to it, then asks you to approve access for this repository. The review also
shows the selected Docker daemon and, for an existing plain local volume, where it stores the data.

- Only plain local volumes are supported. co:op refuses a volume backed by a bind mount, driver
  options or a plugin. Mount an explicit repository path instead.
- co:op creates a missing custom-named volume as plain local storage only after you answer Yes.
- An approval does not automatically transfer to a different daemon or a replaced volume.
- Answering No or closing the input stops the command before Compose runs.
- Automatic box launches, filtered service containers and `coop up` without a terminal refuse
  these volumes, even if you approved them before.

Named volumes that belong only to this project need no approval prompt.

### Secret files in services

Services get the same secret hiding as the box, including when they mount the repo or one of its
directories. A mount of `.env`, `*.key` or a path hidden by `.coopignore` cannot expose its contents
to the service, even when the mount source is itself a hidden file, so an agent-written Compose file
can't pass those secrets to a container the agent controls.

If a service needs a hidden file, such as a generated development TLS key for Keycloak, mount
that individual file read-only with `:ro` or `read_only: true`. Run `coop up` in a terminal to
review the eligible files and approve them. Hidden directories stay protected, and so do files
that a writable mount can also reach. Use named volumes for data the service needs to change.

The approval is tied to:

- The exact Compose file you reviewed. co:op stores approval outside the repo, so an agent cannot
  update it by editing the file. After any edit, box launches hide these files until you
  approve again. co:op explains which files are hidden and why.
- This checkout's private identity. Copying the Compose file or its marker does not transfer
  approval to another checkout. An approval saved by an older co:op doesn't count until you renew it
  with `coop up`, and automatic starts hide these files until then.
- The exact files you reviewed. A new secret added under an approved directory mount stays
  hidden until you approve that file too.

When you approve secret files or outside volumes for a service, co:op also records the exact
local Docker image ID shown in the review and starts that image with pulling disabled. Changing
what a tag points to can't give a different image the same access without review.

- Run `coop up` in a terminal to review an updated local tag. If you decline, the old approval
  stays in place and this startup stops.
- If the approved image was removed, startup stops before creating a container. Pull the tag,
  then run `coop up` to review it again.
- Before asking for a first approval, co:op may pull an image that is not cached, using Docker's
  existing registry login. If you decline, the service does not start or receive access. The
  downloaded image stays in Docker's cache.

Services without access to approved secret files or outside volumes keep their normal image
update behavior.

co:op gives Compose a private Docker client configuration. It keeps the registry login details,
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
it, so co:op still asks you to approve the files. In that prompt:

- Files listed by the repo are marked as expected.
- Files the repo did not list appear first so you can review the unexpected access.
- Files added since your last approval are identified.

You must approve access separately on each machine.

New forks copy these files and their regular siblings that Compose binds from the parent when
present. Copying grants no service access. Before starting services, co:op refuses a missing bind
source by name; create the file or directory and retry. A requested real file must be a regular
file. Compose cannot create an empty directory in place of a missing source, and `coop down`
remains available when a source is missing.

### One URL inside and out

Some services, such as an OIDC issuer like Keycloak, need the same URL in your host browser and in
the app running inside the box. Give the service an `expose:` port in `.agent/compose.yml`:

```yaml
services:
  keycloak:
    image: quay.io/keycloak/keycloak
    expose: ["8443"]                     # coop publishes this to a stable, per-workspace localhost port
    labels:
      coop.service.scheme: https         # scheme for COOP_SERVICE_*_URL
```

co:op assigns a stable host port for each development workspace or loop run, so concurrent stacks
have separate ports. The host port listens only on loopback. Inside the box,
`https://localhost:<port>` reaches the same Keycloak service, so both sides use the same issuer URL
without `host.docker.internal` or broader network access.

The box receives `COOP_SERVICE_KEYCLOAK_URL=https://localhost:<port>`, using the scheme from the
label. Host tools can read all workspace service URLs from `coop fork ls --json`.

The scheme is the label's. A service with no label gets `postgres://` for a `postgres` image,
`redis://` for a `redis` image and `http://` for any other. co:op never wraps a service's traffic in
TLS, so `https` means the service serves TLS itself, and only its label can say so.

The same URL needs open egress. A filtered box reaches a service only through its approved rule, at
the service's name, so it receives `COOP_SERVICE_KEYCLOAK_URL=https://keycloak:8443` for each
`expose` port its rule allows, with the same scheme. The services it starts run on a private network
with no host port, so use open egress for a service your host browser must reach too.

### What a Compose file may do

co:op sends `.agent/compose.yml` to your host's Docker daemon to create the service containers, and
validates the file before `coop up` and before every networked box launch.

An agent could replace a checked mount folder with a link to another part of your machine before
Docker opens it. To prevent that race, co:op holds an exclusive lock while it checks the mounts and
asks Docker to start the services, and boxes that can edit the checkout hold a matching shared lock.
`coop up` waits until it can take the exclusive lock, then starts the separate development stack.

co:op accepts these service settings:

- an `image`
- inline `environment`
- named volumes or repo-relative binds
- `healthcheck`
- `depends_on`
- loopback-only published ports

co:op refuses settings that give a service access beyond the allowed project container and
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

A read-only session, such as an investigation or review, can still use service containers, but
every service mount of the repository must also be read-only, using `:ro` or `read_only: true`.
Otherwise the service could change files the agent is not allowed to write. The mount source must
already exist, because Compose can't create a host directory for a read-only session.

The file-approval rules above still apply. An agent can create `.agent/compose.yml` for you, but its
configuration must pass these checks before co:op starts it.

Stop any Compose service that can write to the checkout before starting another service
stack. co:op's error names the writable mount and suggests `coop down`. co:op also refuses mounts
nested inside a writable service mount: on restart, Docker could open a path that the service
had replaced.

co:op automatically starts only configurations within these restrictions. Run other
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
  on), because a server on the container's localhost isn't reachable through the mapping. In an
  editor thread, co:op tells you when a server starts, with the command and the link, or warns you
  when the server is only on localhost.
- Ports bind to your localhost only, never the LAN.
- Publishing needs networking: `filtered` (the default), which publishes the port on the run's
  gateway, or `open`. See [restricted networking](networking.md).
- co:op skips a host port that is already in use and prints a note. `COOP_SERVE_URL_<port>` still
  carries the workspace's assigned URL for configuration and discovery. An editor session says so
  in the thread instead of posting a link, and takes the port when it frees.
- In an editor session the link always reaches the box the thread is using, including after you
  switch provider or account.
- A box restart (a credential switch, a rebuild) takes the dev server down with the old box. The
  URL stays the same, so run the server again.

## Keeping the box current

```bash
coop update              # self-update coop, then prepare this project's box image
coop update --self-only  # just upgrade the coop binary
coop update --box-only   # just prepare this project's box image
```

`coop update` prepares the image for the project's network mode, using the definitions of the
co:op version it installed. A filtered project keeps co:op's pinned client base, and an open or
offline project gets fresh OS packages. See [stable and fresh rebuilds](#stable-and-fresh-rebuilds).

If GitHub has a newer release, `coop update` downloads its versioned archive and checksum, verifies
the archive and replaces the `coop` binary atomically. The new binary takes effect on your next
`coop` run.

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
  heading. co:op does not rebuild it without being asked.
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

All standard co:op boxes use the same fixed versions of the agent CLIs and ACP adapters, including
plain runs, filtered runs, loops, presets and editor sessions. These are the versions checked for
this co:op release, and co:op verifies Grok's binary against its checksum. Each client's own updater
is disabled, so the versions change only with co:op.

With open or offline networking, you can use other client versions in a custom image selected
with `COOP_IMAGE` or built from an `.agent/Dockerfile` on another base. Those clients are not
covered by the checks for co:op's own image. Filtered runs require co:op's fixed client base
and reject images that replace its clients.
