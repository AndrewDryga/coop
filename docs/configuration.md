# Configuration

co:op reads its settings from environment variables, from `~/.config/coop/coop.conf`, and from the
committed `.agent/project.yaml`. The [website guide](https://coop.dryga.com/docs.html#config)
covers the settings you're most likely to change.

## Environment variables

Set these in your shell or in [`coop.conf`](#coopconf). For settings that turn a feature on or
off, use `1`/`true`/`yes`/`on` or `0`/`false`/`no`/`off`. Letter case does not matter. co:op rejects
any other value.

### Box and runtime

| Variable | Default | What it does |
|---|---|---|
| `COOP_RUNTIME` | auto (Docker preferred) | Picks the runtime: `docker` or `container`. |
| `COOP_IMAGE` | auto | Forces a specific image. It overrides `.agent/Dockerfile` detection. |
| `COOP_BASE_IMAGE` | `coop-box` | The shared base image. `coop-box` selects co:op's own image, with a `coop-box:<definition>` tag for each box definition. |
| `COOP_REPO` | the nearest folder with `.git` | Chooses the repo instead of detecting it from the current directory. co:op follows links but refuses your home folder or any folder above it. On a Mac, it resolves differences in letter case to the folder's actual name. Outside a repository, it uses the current folder as the project, with the same home-folder restriction. |
| `COOP_WORKDIR` | the real path | Where the repo mounts in the box. |
| `COOP_HOME_IN_BOX` | `/home/node` | Where provider homes and instructions mount in the box. |
| `COOP_RUN_ARGS` | unset | Extra container runtime args. Filtered runs accept only bind mounts, `-e KEY=VALUE` and `--label KEY=VALUE`. |
| `COOP_BOX` | `1` in every box | Lets a process detect that it is running inside a co:op box. Its value does not depend on serving or networking. |
| `COOP_PIDS` | `4096` | Limits the number of processes in the box, including runaway process creation. `0`, `unlimited` or empty turns it off. |
| `COOP_MEMORY`, `COOP_CPUS` | unset | Box memory and CPU caps, like `4g` and `2`. |
| `COOP_NO_NEW_PRIVILEGES` | `1` | Runs the box with `--security-opt no-new-privileges`. |
| `COOP_ALLOW_CASE_EXPOSED_SECRETS` | off | On a Mac, allows a box or `coop up` services to start even when Coop cannot hide every letter-case spelling of a long filename. The box or service could then read a secret through a name such as `.ENV.PRODUCTION`. See [hiding secrets](box.md#hiding-secrets). |
| `COOP_HOMES` | `1` | Mounts each selected agent's repository home into the box. `0` keeps those homes out and disables preset and consult runs, which need them to route work to their agents. |
| `COOP_EGRESS` | `open` | Chooses how the box can use the network. See the choices below and [networking](networking.md). |
| `COOP_NO_ASDF` | off | Skips runtime `.tool-versions` provisioning. Stale Node shim repair still runs. The box reads it, so set it in `agents/env` (forwarded into the box), not your host shell. |
| `COOP_NETWORK` | `1` | Joins the services network. |
| `COOP_CACHE` | `1` | Mounts the cache volume. |
| `COOP_AUTO_UP` | `1` | Runs `compose up` for working boxes when `.agent/compose.yml` exists and egress is `open`. A project created by `coop init` requests filtered networking, so start its services with `coop up`. Loop runs reuse their own service stack; reviewers rely on the worker's reported checks. Set `0` to manage development services yourself with `coop up` and `coop down`. |
| `COOP_SERVICES_NET` | auto | The services network to join, so parallel forks can share one db. |

Choose one of these values for `COOP_EGRESS`:

- `open` allows full outbound traffic. This is the default.
- `filtered` allows the AI provider and domains you have approved.
- `none` disables the box's network (`--network none`). The box cannot send the repo, secrets
  or credentials over the network. Installs and model API calls stop working too, so you must
  choose this setting explicitly.

An `--egress` flag overrides `COOP_EGRESS`. A project's saved `coop approve` decision also
overrides it. `COOP_EGRESS` itself overrides `box.egress` in `.agent/project.yaml`.

The resource and privilege limits (`COOP_PIDS`, `COOP_MEMORY`, `COOP_CPUS` and
`COOP_NO_NEW_PRIVILEGES`) apply on Docker. co:op currently skips them with Apple's `container`
CLI because that runtime works differently.

On Docker, co:op also removes all Linux capabilities (`--cap-drop ALL`); agent workloads do not
need them. Even if a repo's `.agent/Dockerfile` sets `USER root`, the container process does not
get powers such as `CAP_DAC_OVERRIDE`, `CAP_NET_RAW` or `CAP_MKNOD`.

### Agents and config

| Variable | Default | What it does |
|---|---|---|
| `COOP_CONFIG_DIR` | `~/.config/coop/agents` | The per-agent auth and settings folder. |
| `COOP_CONF` | `~/.config/coop/coop.conf` | Environment only. Relocates `coop.conf`. Unlike the optional default path, an explicit path must name a readable regular file. |
| `NO_COLOR` | unset | Environment only. Present at any value, even empty, it disables ANSI color everywhere ([no-color.org](https://no-color.org)). |
| `COOP_<AGENT>_CMD`, like `COOP_CLAUDE_CMD` | the autonomous default | Overrides an agent's base command. |
| `COOP_<AGENT>_MODEL`, like `COOP_CLAUDE_MODEL` | the CLI's default | The agent-wide default model, everywhere that agent runs. See [picking models](agents.md#picking-models). |
| `COOP_CONSULT_TIMEOUT` | `0` (unlimited) | How long `coop-consult` waits for each peer, in seconds. See below. |
| `COOP_MCP_FILE` | `~/.config/coop/agents/mcp.json` | The file that defines MCP servers. It must be a regular file of at most 4 MiB, outside any directory co:op mounts in full. |
| `COOP_SHELL` | `bash` | The shell `coop shell` opens. |
| `COOP_NO_UPDATE_CHECK` | off | Set it to opt out of the once-a-day "a newer coop/box is available" check. |

By default, `COOP_CONSULT_TIMEOUT` lets each peer take as long as it needs, subject to the calling
attempt's limits on tool use and total duration. Set a whole number of seconds (max `86400`) to
add a timeout. If a peer does not answer in time, co:op skips it and the lead uses the replies
that arrived.

A timeout cannot tell a slow answer from a stalled peer. If it interrupts a peer that was still
working, that answer is lost and needs a retry.

### Forks and loop

| Variable | Default | What it does |
|---|---|---|
| `COOP_GATE` | unset | The check command run in the box during a local fork merge, or during review with `--gate`, such as `make check`. Controller-job reviews use the check in the submitted job instead. |
| `COOP_EDITOR` | detected | The editor for `coop fork review --open`. |
| `COOP_REVIEW_CMD` | unset | A full override for `coop fork review`, run with `sh -c`. |
| `COOP_TASKS` | derived | Chooses the task queue directories for `coop tasks` and the loop. Separate multiple paths with spaces. If unset, co:op uses the queues listed in `.agent/project.yaml` for a [monorepo](loop.md#monorepos), or `.agent/tasks` otherwise. `--tasks` replaces this choice for one run; it does not add to it. |
| `COOP_CAFFEINATE` | `1` | On a Mac, uses `caffeinate` to prevent idle sleep while the loop runs. The protection ends with the loop. `0` or `false` turns it off. |
| `COOP_ACP_WARM` | `1` | Environment only. Keeps a box ready for each other signed-in editor ACP provider. Switching to that provider's default model then skips box startup. Set `0` or `false` to save memory. |
| `COOP_SPINNER` | `1` | Environment only. Animates the five-column Box Run beside progress bars and the one-column Corner Run (`◰ ◳ ◲ ◱`) in task rows. Set `0` or `false` to freeze them and stop frequent loop-screen redraws, for example when debugging or recording the terminal. |
| `COOP_STREAM_TRACE` | off | Set it to persist each streaming loop attempt's raw provider JSONL and rendered output under `.agent/runs/<run>.streams/`. |

You configure loop behavior in `.agent/loop.yaml`. See [the loop](loop.md) for the whole file.

| Key | What it does |
|---|---|
| `work.command` | Sets the command for each work iteration, replacing the retired loop command variable. |
| `work.agent`, `between.agent`, `signoff.agent` | Choose the agents for work, per-task review and final review. |
| `signoff.rounds` | Limits the number of final review rounds. |
| `preflight.enabled` | Runs the queue cleanup pass by default in this repository. |

The `--preflight` flag turns on the same cleanup for one invocation.

### Command-valued settings

co:op splits `COOP_GATE`, `COOP_RUN_ARGS` and the `COOP_<AGENT>_CMD` overrides into a list of
command arguments (`argv`). Single and double quotes group text into one argument, and `\`
escapes a character. co:op does not run these settings through a shell, so it does not expand
wildcard paths or `$VAR` values.

For example, `COOP_GATE='bash -lc "make check && make lint"'` produces three arguments, not five.
A bare `&&`, `|` or `$VAR` is passed literally. Wrap commands that need those shell features in
`bash -lc "…"`. `COOP_REVIEW_CMD` is the exception: co:op runs it through `sh -c`.

## coop.conf

`~/.config/coop/coop.conf` takes the same settings as `KEY=VALUE` lines. The environment wins over
the file.

The default file is optional. If you set `COOP_CONF`, the chosen file must exist. If co:op cannot
read it, the error names the path. If a setting is malformed, duplicated, retired or unknown,
the error names the path and line. An environment override does not let a broken file pass.

`COOP_CONF`, `NO_COLOR`, `COOP_ACP_WARM` and `COOP_SPINNER` are process-environment controls, so
you can't set them inside `coop.conf`.

Optional [provider usage reports](usage-reports.md) use a separate host-only
`~/.config/coop/usage-reports.json`. Their API spend is never added to local usage estimates.
Reporting keys belong in host environment variables, not `coop.conf`, project files or `agents/env`.

## Project settings: .agent/project.yaml

`.agent/project.yaml` is the committed per-project config.

| Key | What it holds |
|---|---|
| `subprojects:` | A monorepo's members. See [monorepos](loop.md#monorepos). |
| `serve:` | The dev server ports co:op publishes. See [the dev server in your browser](box.md#dev-servers-in-your-browser). |
| `box:` | The box policy: egress and [`egress_rules:`](networking.md), resource caps, `auto_up` and `network`. |
| `gate:` | The merge gate. |
| `gate_sources:` | Exact project-specific paths that receive protected loop review. |
| `context:` | The routes for [path-routed context](#path-routed-context). |

Explicit `COOP_*` settings in your environment or `coop.conf` override `box:` and `gate:`.
Because the project settings file is committed and read on the host, it may tighten security
restrictions but cannot relax them on its own:

- `egress` can pin `filtered` or `offline`.
- `open` is a request that a human approves with `coop approve`.
- `no_new_privileges` isn't settable here.

## Path-routed context

Use `coop context` to give an agent the committed docs relevant to the files it is working on.
This avoids loading every rule and knowledge-base note for a small change. co:op always includes
the complete `AGENTS.md`/`CLAUDE.md`, then adds documents whose configured path patterns match.

```yaml
# .agent/project.yaml
context:
  routes:
    - paths: [portal/**, "**/*.ex"]   # * within a segment, ** across segments
      include: [.agent/kb/portal.md]  # repo-relative docs to add when a path matches
```

```bash
coop context --changed            # scope = the paths git reports changed
coop context --task <id>          # scope = a task's declared `paths:` frontmatter
coop context portal/lib/user.ex   # scope = explicit repo-relative paths
coop context --changed --json     # same, as data (files + the route that selected each)
coop context --changed --rendered # the compiled content itself, canonical first
```

co:op chooses paths from your arguments, Git's changed files, the task's declared paths or the
current subproject. It never guesses them from a prompt. It reports an error if a route includes
a missing document or a path outside the repo. `coop context` always keeps the main instructions
complete; it does not summarize or truncate them.

Routes come from the committed `project.yaml`. A fork therefore uses its parent's routes, but
matches them against paths in the fork's own tree.

## Exit codes

Commands use these exit codes so CI and scripts can check the result without parsing messages.

| Code | Meaning |
|---|---|
| `0` | Success. |
| `1` | A failure, or findings, like `coop check-secrets` on a hit. |
| `2` | A usage error: an unknown command or flag, or bad arguments. |
| `3` | `coop loop` only: it stopped with a task blocked on a human decision. |
| `130` | `coop loop` only: Ctrl-C interrupted it before the final verdict. |

If final verification is enabled and fails, `coop loop` keeps the completed work and exits with
a nonzero code. It uses `1` unless the outcome is blocked work only, which takes precedence.
An intentional `--max-tasks N` pause exits successfully but leaves final review pending. See
[the loop's exit codes](loop.md#exit-codes).

## JSON output

Most commands that change state report success or failure through exit codes rather than
`--json`. Read-only commands offer JSON when other tools need the current state. The two main
views are:

- `coop tasks watch --json` is the canonical project task and activity snapshot.
- `coop fork ls --json` discovers workspace URLs plus fork status.

`coop context`, the `coop net` views and `coop sessions doctor` also accept `--json`. `coop net
export` always writes JSON. Use exit codes to check commands that change state, and the host's
JSON views to read the current combined state.
