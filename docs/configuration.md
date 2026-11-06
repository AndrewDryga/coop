# Configuration

co:op reads its settings from environment variables, from `~/.config/coop/coop.conf`, and from the
committed `.agent/project.yaml`. The [website guide](https://coop.dryga.com/docs.html#config)
covers the settings you're most likely to change.

## Environment variables

Set these in your shell or in [`coop.conf`](#coopconf). On/off settings take `1`/`true`/`yes`/`on`
or `0`/`false`/`no`/`off` in any letter case, and co:op rejects any other value.

### Box and runtime

| Variable | Default | What it does |
|---|---|---|
| `COOP_HOME` | `$XDG_CONFIG_HOME/coop`, or `~/.config/coop` | Absolute path of co:op's settings folder, which holds accounts and `coop.conf`. Use it to keep them out of a sync service, and set it for your editor too. co:op doesn't move your files. |
| `COOP_RUNTIME` | auto (Docker preferred) | Picks the runtime: `docker` or `container`. |
| `COOP_IMAGE` | auto | Forces a specific image, overriding `.agent/Dockerfile` detection. |
| `COOP_BASE_IMAGE` | `coop-box` | The shared base image. `coop-box` selects co:op's own image, with a `coop-box:<definition>` tag for each box definition. |
| `COOP_REPO` | the nearest folder with `.git` | Chooses the repo instead of detecting it from the current directory. co:op follows links, but refuses a link that leads to your home folder or any folder above it. With this unset outside a repository, the current folder is the project, unless it's your home folder or above, which co:op refuses. |
| `COOP_WORKDIR` | the repo's real path | Where the repo mounts in the box. |
| `COOP_HOME_IN_BOX` | `/home/node` | Where provider homes and instructions mount in the box. |
| `COOP_RUN_ARGS` | unset | Extra arguments for the container runtime. Filtered runs accept only bind mounts, `-e KEY=VALUE` and `--label KEY=VALUE`. |
| `COOP_BOX` | `1` in every box | Lets a process detect that it's running inside a co:op box. |
| `COOP_PIDS` | `4096` | Limits the number of processes in the box. `0`, `unlimited` or empty turns it off. |
| `COOP_MEMORY`, `COOP_CPUS` | unset | Limits the box's memory and CPUs, for example `4g` and `2`. |
| `COOP_NO_NEW_PRIVILEGES` | `1` | Runs the box with `--security-opt no-new-privileges`. |
| `COOP_ALLOW_CASE_EXPOSED_SECRETS` | off | On a Mac, lets one run start when mount protection can't hide every letter-case spelling of a long filename. The box or a service could then read a secret through a name such as `.ENV.PRODUCTION`. To remember this access for file paths you've reviewed, use `coop approve --case-exposure`. Runs protected by eBPF keep these files hidden and need neither. See [hiding secrets](box.md#hiding-secrets). |
| `COOP_HOMES` | `1` | Mounts each selected agent's repository home into the box. `0` keeps those homes out and disables preset and consult runs. |
| `COOP_EGRESS` | `filtered` | Chooses how the box can use the network. See the choices below and [networking](networking.md). |
| `COOP_NO_ASDF` | off | Skips installing the tools pinned in `.tool-versions` when the box starts. Set it in `agents/env`, not your host shell. |
| `COOP_NETWORK` | `1` | Joins the services network. |
| `COOP_CACHE` | `1` | Mounts the cache volume. |
| `COOP_AUTO_UP` | `1` | Runs `compose up` for working boxes when `.agent/compose.yml` exists and egress is `open`. Start a filtered project's services with `coop up`. Set `0` to manage development services yourself with `coop up` and `coop down`. |
| `COOP_SERVICES_NET` | auto | The services network to join, so parallel forks can share one database. |

Choose one of these values for `COOP_EGRESS`:

- `filtered` allows the AI provider and domains you've approved. This is the default, and it needs
  Docker.
- `open` allows full outbound traffic.
- `offline` disables the box's network (`--network none`). The box can't send the repo, secrets
  or credentials over the network. Installs and model API calls stop working too, so you must
  choose this setting explicitly.

An `--egress` flag overrides `COOP_EGRESS`, and `COOP_EGRESS=offline` always keeps a run offline
unless you pass that flag. For `filtered` and `open`, a project's saved `coop approve` decision
overrides `COOP_EGRESS`, and the run warns. `COOP_EGRESS` itself overrides `box.egress` in
`.agent/project.yaml`.

The resource and privilege limits (`COOP_PIDS`, `COOP_MEMORY`, `COOP_CPUS` and
`COOP_NO_NEW_PRIVILEGES`) apply on Docker, and co:op skips them with Apple's `container` CLI.

On Docker, co:op also removes all Linux capabilities (`--cap-drop ALL`). Even if a repo's
`.agent/Dockerfile` sets `USER root`, the container process doesn't get powers such as
`CAP_DAC_OVERRIDE`, `CAP_NET_RAW` or `CAP_MKNOD`.

### Agents and config

| Variable | Default | What it does |
|---|---|---|
| `COOP_CONFIG_DIR` | `~/.config/coop/agents` | The folder for each agent's credentials and settings. |
| `COOP_CONF` | `~/.config/coop/coop.conf` | Environment only. Reads `coop.conf` from this path, which must name a readable regular file. |
| `NO_COLOR` | unset | Environment only. Any value, even an empty one, turns off ANSI color everywhere ([no-color.org](https://no-color.org)). |
| `COOP_<AGENT>_CMD`, like `COOP_CLAUDE_CMD` | the agent's autonomous command | Overrides an agent's base command. |
| `COOP_<AGENT>_MODEL`, like `COOP_CLAUDE_MODEL` | the CLI's default | The agent-wide default model, everywhere that agent runs. See [picking models](agents.md#picking-models). |
| `COOP_CONSULT_TIMEOUT` | `0` (unlimited) | How long `coop-consult` waits for each peer, in seconds. See below. |
| `COOP_CONSULT_STREAM_LIMIT` | `0` (unlimited) | The most output, in bytes, `coop-consult` accepts from a peer's reply or diagnostics. It discards a larger reply. Set it in `agents/env`. See [limits](agents.md#limits). |
| `COOP_PRESETS_DIR` | `~/.config/coop/presets` | The folder for global presets. See [where presets live](agents.md#where-presets-live). |
| `COOP_MCP_FILE` | `~/.config/coop/agents/mcp.json` | The file that defines MCP servers. It must be a regular file of at most 4 MiB, outside any directory co:op mounts in full. |
| `COOP_SHELL` | `bash` | The shell `coop shell` opens. |
| `COOP_NO_UPDATE_CHECK` | off | Turns off the once-a-day "a newer coop/box is available" check. |

By default, a peer can take as long as it needs. In a loop, the lead's own limits still apply: 2
hours for one tool call and 48 hours for one attempt. Set `COOP_CONSULT_TIMEOUT` to a whole number of seconds (max `86400`) to add a
timeout. If a peer doesn't answer in time, co:op skips it and the lead uses the replies that
arrived.

A timeout can't tell a slow answer from a stalled peer. If it interrupts a peer that was still
working, that answer is lost and needs a retry.

### Forks and loop

| Variable | Default | What it does |
|---|---|---|
| `COOP_GATE` | unset | The check command, such as `make check`, that runs in the box during a local fork merge or during review with `--gate`. Controller-job reviews use the check in the submitted job instead. |
| `COOP_EDITOR` | detected | The editor for `coop fork review --open`. |
| `COOP_REVIEW_CMD` | unset | A full override for `coop fork review`, run with `sh -c`. |
| `COOP_TASKS` | derived | The task queue directories for `coop tasks` and the loop, separated by spaces. If unset, co:op uses the queues listed in `.agent/project.yaml` for a [monorepo](loop.md#monorepos), or `.agent/tasks` otherwise. For one run, `--tasks` replaces this choice and doesn't add to it. |
| `COOP_CAFFEINATE` | `1` | On a Mac, uses `caffeinate` to prevent idle sleep while the loop runs. `0` or `false` turns it off. |
| `COOP_ACP_WARM` | `1` | Environment only. Keeps a box ready for each other editor ACP provider you're signed in to, so switching to that provider's default model skips box startup. Set `0` or `false` to save memory. |
| `COOP_ACP_CARRY_TOKENS` | `200000` | How much of an editor conversation, in tokens of about 4 bytes, co:op carries to a new provider when you switch, dropping the oldest messages first. |
| `COOP_ACP_TRACE` | unset | Environment only. Set it to any value in the editor's agent environment to record each `coop acp` server's traffic. See [tracing a session](integrations.md#tracing-a-session). |
| `COOP_SPINNER` | `1` | Environment only. Animates the Box Run beside progress bars and the Corner Run (`◰ ◳ ◲ ◱`) in task rows. Set `0` or `false` to freeze them and stop frequent loop-screen redraws. |
| `COOP_STREAM_TRACE` | off | Saves each streaming loop attempt's raw provider JSONL and rendered output under `.agent/runs/<run>.streams/`. |

You configure loop behavior in `.agent/loop.yaml`. See [the loop](loop.md) for the whole file.

| Key | What it does |
|---|---|
| `work.command` | Sets the command for each work iteration. |
| `work.agent`, `between.agent`, `signoff.agent` | Choose the agents for work, per-task review and final review. |
| `signoff.rounds` | Limits the number of final review rounds. |
| `preflight.enabled` | Runs the queue cleanup pass by default in this repository. |

The `--preflight` flag turns on the same cleanup for one run.

### Command-valued settings

co:op splits `COOP_GATE`, `COOP_RUN_ARGS` and the `COOP_<AGENT>_CMD` overrides into `argv`. Single
and double quotes group text into one argument, and `\` escapes a character. It doesn't run these
settings through a shell, so it doesn't expand wildcard paths or `$VAR` values.

For example, `COOP_GATE='bash -lc "make check && make lint"'` produces three arguments, with the
quoted text as one of them. co:op passes a bare `&&`, `|` or `$VAR` literally, so wrap commands that
need those shell features in `bash -lc "…"`. `COOP_REVIEW_CMD` is the exception: co:op runs it
through `sh -c`.

## coop.conf

`~/.config/coop/coop.conf` takes the same settings as `KEY=VALUE` lines. The environment wins over
the file.

The default file is optional, but a file you name with `COOP_CONF` must exist. If co:op can't read
the file, the error names the path. If a line is malformed, repeats a setting or names a setting
co:op doesn't read, the error names the path and line. Setting the same variable in the environment
doesn't let a broken file pass.

co:op reads `COOP_HOME`, `COOP_CONF`, `NO_COLOR`, `COOP_ACP_WARM`, `COOP_ACP_TRACE` and
`COOP_SPINNER` only from the process environment, so you can't set them inside `coop.conf`. The box
reads `COOP_NO_ASDF` and `COOP_CONSULT_STREAM_LIMIT`, so they belong in `agents/env`.

### What can sync

`~/.config/coop` holds your settings and accounts, so you can sync it across machines with Dropbox
or a similar service. If two machines refresh the same single-use sign-in, one of them can get
signed out. If that happens, run `coop login` on that machine.

Records that belong to one machine, such as each run's generated files, image records, usage caches and ACP traces,
live in `$XDG_STATE_HOME/coop` or `~/.local/state/coop`. They never need syncing. Of an account, only
`agents/<agent>/credentials/<account>/authority.json` needs to sync. Its other files are journals and
locks for one machine. With Dropbox, put these rules in `rules.dropboxignore` at the Dropbox root, and
`coop doctor` notes that the folder syncs by them.

co:op also writes a `.gitignore` in `~/.config/coop` if there is none, so a dotfiles repository
keeps only shared settings. Accounts and `agents/env` stay out of Git.

Optional [provider usage reports](usage-reports.md) use a separate host-only
`~/.config/coop/usage-reports.json`. co:op never adds their API spend to local usage estimates.
Reporting keys belong in host environment variables, not `coop.conf`, project files or `agents/env`.

## Project settings: .agent/project.yaml

`.agent/project.yaml` is the committed per-project config.

| Key | What it holds |
|---|---|
| `subprojects:` | A monorepo's members. See [monorepos](loop.md#monorepos). |
| `serve:` | The dev server ports co:op publishes. See [the dev server in your browser](box.md#dev-servers-in-your-browser). |
| `box:` | The box policy: the [`dockerfile` and `compose` paths](box.md#your-own-image-agentdockerfile), [`env` defaults](box.md#box-only-environment-boxenv), egress and [`egress_rules:`](networking.md), resource caps (`memory`, `cpus`, `pids`), `auto_up` and `network`. |
| `services:` | `require_real_files:`, the hidden files your services need to read. Listing a file grants no access. See [secret files in services](box.md#secret-files-in-services). |
| `gate:` | The merge gate: the check command that `coop fork merge` runs in the box. |
| `gate_sources:` | The exact paths of project-specific files that define the gate. The loop always reviews changes to them. |
| `review:` | Services and environment for review checks only. See [review checks](#review-checks). |
| `context:` | The routes for [path-routed context](#path-routed-context). |

Explicit `COOP_*` settings in your environment or `coop.conf` override `box:` and `gate:`.
Because the project settings file is committed and read on the host, it may tighten security
restrictions but can't relax them on its own:

- `egress` can pin `filtered` or `offline`.
- `open` is a request that a human approves with `coop approve`.
- You can't set `no_new_privileges` here.

### Review checks

`review:` applies only to the check that `coop fork review --gate` or a controller review runs:

```yaml
review:
  compose: .agent/review-compose.yml  # services the check needs, such as a test database
  env:
    DATABASE_URL: postgres://postgres:test@db:5432/app_test
```

`review.env` values are literal strings, set only in the check's box. Names beginning with `COOP_`
are reserved. A controller review with open networking starts the services in `review.compose`
before the check, then removes them and their volumes. `coop fork review --gate` doesn't start
them.

`COOP_REVIEW_BASE` names the commit the changes are compared against, so a check can look only at
what changed, for example `git diff --name-only "$COOP_REVIEW_BASE"...HEAD`. co:op sets it for the
project checks that `coop fork merge` and `coop fork review --gate` run, for controller reviews, and
in loop worker boxes. A worker gets the commit its task started from, so a check still covers the
task's changes once they are committed, including after a retry or after a review reopened the task.
A worker may also have work it hasn't committed: `git diff --name-only "$COOP_REVIEW_BASE"` compares
the base with the working tree, and `git ls-files --others --exclude-standard` lists new files. co:op
sets `COOP_REVIEW=1` only for a review (`coop fork review --gate` or a controller review), so a
script can tell a review from a merge check.

## Path-routed context

Use `coop context` to give an agent the committed docs relevant to the files it's working on,
without loading every rule and knowledge-base note for a small change. co:op always includes the
complete `AGENTS.md`/`CLAUDE.md`, then adds documents whose configured path patterns match.

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
current subproject, and never guesses them from a prompt. It reports an error if a route includes
a missing document or a path outside the repo.

Routes come from the committed `project.yaml`, so a fork uses its parent's routes but matches them
against paths in its own tree.

## Exit codes

Commands use these exit codes so CI and scripts can check the result without parsing messages.

| Code | Meaning |
|---|---|
| `0` | Success. |
| `1` | A failure, or findings, like `coop check-secrets` on a hit. |
| `2` | A usage error: an unknown command or flag, or bad arguments. |
| `3` | `coop loop` stopped with a task blocked on a human decision. `coop sessions check` found data this build would convert. `coop sessions quiescence` found queued or running work. |
| `4` | `coop sessions check` only: this build can't use the session data. |
| `130` | `coop loop` only: Ctrl-C interrupted it before the final verdict. |

If final verification is enabled and fails, `coop loop` keeps the completed work and exits with
`1`, or `3` if the only work left is blocked. An intentional `--max-tasks N` pause exits
successfully but leaves final review pending. See [the loop's exit codes](loop.md#exit-codes), and
[upgrading a worker](session-api.md#upgrading-a-worker) for `coop sessions check`.

## JSON output

Most commands that change state report success or failure through exit codes, not through
`--json`. Read-only commands offer JSON when other tools need the current state. The two main
views are:

- `coop tasks watch --json` is the standard snapshot of the project's tasks and activity.
- `coop fork ls --json` lists workspace URLs and fork status.

`coop context`, the `coop net` views and `coop sessions doctor` also accept `--json`.
`coop net export` always writes JSON.
