# Configuration

co:op reads its settings from environment variables, from `~/.config/coop/coop.conf`, and from the
committed `.agent/project.yaml`. The [website guide](https://coop.dryga.com/docs.html#config)
covers the settings you're most likely to change.

## Environment variables

Set these in your shell, or as lines in [`coop.conf`](#coopconf). Boolean settings accept
`1`/`true`/`yes`/`on` and `0`/`false`/`no`/`off`, case-insensitively. Any other explicit value is
an error.

### Box and runtime

| Variable | Default | What it does |
|---|---|---|
| `COOP_RUNTIME` | auto (Docker preferred) | Picks the runtime: `docker` or `container`. |
| `COOP_IMAGE` | auto | Forces a specific image. It overrides `.agent/Dockerfile` detection. |
| `COOP_BASE_IMAGE` | `coop-box` | The shared base image. `coop-box` means co:op's own, tagged `coop-box:<definition>` per box definition. |
| `COOP_REPO` | the git toplevel | The repo to operate on. It overrides detection from the current directory. |
| `COOP_WORKDIR` | the real path | Where the repo mounts in the box. |
| `COOP_HOME_IN_BOX` | `/home/node` | Where auth and instructions mount in the box. |
| `COOP_RUN_ARGS` | unset | Extra container runtime args. Filtered runs accept only bind mounts, `-e KEY=VALUE` and `--label KEY=VALUE`. |
| `COOP_BOX` | `1` in every box | A stable in-box identity marker. It's independent of serving and networking. |
| `COOP_PIDS` | `4096` | The box's pids limit, a fork-bomb cap. `0`, `unlimited` or empty turns it off. |
| `COOP_MEMORY`, `COOP_CPUS` | unset | Box memory and CPU caps, like `4g` and `2`. |
| `COOP_NO_NEW_PRIVILEGES` | `1` | Runs the box with `--security-opt no-new-privileges`. |
| `COOP_HOMES` | `1` | Mounts your per-agent home dirs (auth and settings) into the box. `0` keeps them out, which disables preset and consult runs because their routing contract can't mount. |
| `COOP_EGRESS` | `open` | `open`, `filtered` or `none`. `filtered` reaches only your AI provider and the domains you approve (see [networking](networking.md)). `none` cuts the box off the network (`--network none`). With no outbound traffic, a prompt-injected agent can't exfiltrate the repo, secrets or its credentials. It breaks installs and the model API, so it's opt-in. The default keeps full outbound. An `--egress` flag wins over this setting, and so does a project's stored `coop approve` decision; `box.egress` in `.agent/project.yaml` comes after it. |
| `COOP_NO_ASDF` | off | Skips runtime `.tool-versions` provisioning. Stale Node shim repair still runs. The box reads it, so set it in `agents/env` (forwarded into the box), not your host shell. |
| `COOP_NETWORK` | `1` | Joins the services network. |
| `COOP_CACHE` | `1` | Mounts the cache volume. |
| `COOP_AUTO_UP` | `1` | Auto-starts sibling services (`compose up`) for working boxes when a `.agent/compose.yml` is present. Loop runs use their own reusable stack, and reviewers use the worker's reported checks. It starts them only when egress is `open`, so a project set up by `coop init`, which asks for filtered networking, starts them with `coop up`. Set `0` to manage development services yourself with `coop up` and `coop down`. |
| `COOP_SERVICES_NET` | auto | The services network to join, so parallel forks can share one db. |

The resource and privilege caps (`COOP_PIDS`, `COOP_MEMORY`, `COOP_CPUS` and
`COOP_NO_NEW_PRIVILEGES`) apply on Docker. Apple's `container` CLI differs, so co:op skips them
there for now. On Docker the box also runs with all Linux capabilities dropped (`--cap-drop ALL`).
The agent workloads need none. Dropping them keeps root in the container (a repo
`.agent/Dockerfile` that does `USER root`) from holding `CAP_DAC_OVERRIDE`, `CAP_NET_RAW`,
`CAP_MKNOD` and the rest.

### Agents and config

| Variable | Default | What it does |
|---|---|---|
| `COOP_CONFIG_DIR` | `~/.config/coop/agents` | The per-agent auth and settings folder. |
| `COOP_CONF` | `~/.config/coop/coop.conf` | Environment only. Relocates `coop.conf`. Unlike the optional default path, an explicit path must name a readable regular file. |
| `NO_COLOR` | unset | Environment only. Present at any value, even empty, it disables ANSI color everywhere ([no-color.org](https://no-color.org)). |
| `COOP_<AGENT>_CMD`, like `COOP_CLAUDE_CMD` | the autonomous default | Overrides an agent's base command. |
| `COOP_<AGENT>_MODEL`, like `COOP_CLAUDE_MODEL` | the CLI's default | The agent-wide default model, everywhere that agent runs. See [picking models](agents.md#picking-models). |
| `COOP_CONSULT_TIMEOUT` | `0` (unlimited) | The per-peer `coop-consult` bound, in seconds. See below. |
| `COOP_MCP_FILE` | `~/.config/coop/agents/mcp.json` | The one MCP source of truth: a regular file up to 4 MiB, outside directories co:op mounts wholesale. |
| `COOP_SHELL` | `bash` | The shell `coop shell` opens. |
| `COOP_NO_UPDATE_CHECK` | off | Set it to opt out of the once-a-day "a newer coop/box is available" check. |

`COOP_CONSULT_TIMEOUT` is unbounded by default, because a clock can't tell a long answer from a
wedged one. Killing a working peer loses its answer and costs the retry that follows. The parent
attempt's own tool cap and ceiling already bound it from outside. Set a whole-second value (max
`86400`) to opt back into a bound. A peer that then doesn't answer in time is skipped, so the lead
synthesizes from whoever did.

### Forks and loop

| Variable | Default | What it does |
|---|---|---|
| `COOP_GATE` | unset | The worker-owned gate run in the box before a fork merge or controller-job review, like `make check`. |
| `COOP_EDITOR` | detected | The editor for `coop fork review --open`. |
| `COOP_REVIEW_CMD` | unset | A full override for `coop fork review`, run with `sh -c`. |
| `COOP_TASKS` | derived | Explicit task queue dir(s) for `coop tasks` and the loop, space-separated for several. When it's unset, the queues come from `.agent/project.yaml` (a [monorepo's](loop.md#monorepos) subproject queues), else `.agent/tasks`. `--tasks` replaces this for a run; it doesn't merge. |
| `COOP_CAFFEINATE` | `1` | While a loop runs, holds a system sleep inhibitor so the machine doesn't idle-sleep mid-drain (macOS `caffeinate`, released when the loop ends). `0` or `false` turns it off. |
| `COOP_ACP_WARM` | `1` | Environment only. Keeps one box ready for each other signed-in editor ACP provider, so switching to it at its default model skips the start. `0` or `false` disables the warm pool on low-memory hosts. |
| `COOP_SPINNER` | `1` | Environment only. Animates co:op's live-view spinners: the five-column Box Run beside progress bars, and the one-column Corner Run (`◰ ◳ ◲ ◱`) in dense task rows. `0` or `false` freezes them and suppresses the loop's fast repaint ticker, which is useful for debugging and terminal recording. |
| `COOP_STREAM_TRACE` | off | Set it to persist each streaming loop attempt's raw provider JSONL and rendered output under `.agent/runs/<run>.streams/`. |

You configure loop behavior in `.agent/loop.yaml`. See [the loop](loop.md) for the whole file.

| Key | What it does |
|---|---|
| `work.command` | Replaces the retired loop command variable. |
| `work.agent`, `between.agent`, `signoff.agent` | Select targets. |
| `signoff.rounds` | Caps review rounds. |
| `preflight.enabled` | Makes cleanup the default for that repository. |

The `--preflight` flag turns on the same cleanup for one invocation.

### Command-valued settings

co:op splits command-valued settings into `argv` with shell quoting: `COOP_GATE`, `COOP_RUN_ARGS`
and the `COOP_<AGENT>_CMD` overrides. Single and double quotes group, and `\` escapes. No shell
runs them, so there's no globbing and no `$VAR` expansion.

Quotes group as you'd expect: `COOP_GATE='bash -lc "make check && make lint"'` is three args, not
five. A bare `&&`, `|` or `$VAR` is a literal argument, so wrap those in `bash -lc "…"`.
`COOP_REVIEW_CMD` is the exception: co:op runs it via `sh -c`.

## coop.conf

`~/.config/coop/coop.conf` takes the same settings as `KEY=VALUE` lines. The environment wins over
the file.

The default file is optional, but a file you select with `COOP_CONF` must exist. A file access
error names its path. A content error names its path and line. Malformed, duplicate, retired and
unknown settings are errors, and an environment override never hides a broken file.

`COOP_CONF`, `NO_COLOR`, `COOP_ACP_WARM` and `COOP_SPINNER` are process-environment controls, so
you can't set them inside `coop.conf`.

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

`box:` and `gate:` fall below an explicit `COOP_*` setting in your environment or `coop.conf`. The
file is committed and read on the host, so it can only ever tighten your posture:

- `egress` can pin `filtered` or `offline`.
- `open` is a request that a human approves with `coop approve`.
- `no_new_privileges` isn't settable here.

## Path-routed context

In a big repo, an agent doesn't need every rule and KB card for every change. `coop context`
compiles only the committed docs relevant to the paths in play, so a session carries less. It
always includes the canonical `AGENTS.md`/`CLAUDE.md` whole, plus the routes whose globs match.

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

Scope is deterministic: explicit paths, git-changed paths, a task's declared paths, or the current
subproject. co:op never infers it from a prompt. A route include that's missing or escapes the
repo is an error. `coop context` never summarizes or truncates canonical instructions. The config
comes from the committed `project.yaml`, so a fork inherits the parent's routes, while scope comes
from the fork's own tree.

## Exit codes

Every command follows one contract, so CI and scripts can branch without parsing output.

| Code | Meaning |
|---|---|
| `0` | Success. |
| `1` | A failure, or findings, like `coop check-secrets` on a hit. |
| `2` | A usage error: an unknown command or flag, or bad arguments. |
| `3` | `coop loop` only: it stopped with a task blocked on a human decision. |
| `130` | `coop loop` only: Ctrl-C interrupted it before the final verdict. |

When final verification is turned on and fails, `coop loop` exits nonzero and preserves the
completed work. The code is `1` when no blocked-only outcome takes precedence. An intentional
`--max-tasks N` pause exits successfully without claiming the whole queue was verified. See
[the loop's exit codes](loop.md#exit-codes).

## JSON output

Mutating commands use exit codes as their machine contract instead of growing a second control
API, so `--json` is uncommon. Read-only views take it where a tool needs the state. The main two:

- `coop tasks watch --json` is the canonical project task and activity snapshot.
- `coop fork ls --json` discovers workspace URLs plus fork status.

`coop context`, the `coop net` views and `coop sessions doctor` take `--json` too, and `coop net
export` always writes JSON. For mutations, branch on exit codes. When a tool needs current joined
state, read those host-owned snapshots.
