# Migrating

## v10.1.2: upgrading from v8.1.0

This guide covers the changes since v8.1.0. Complete the applicable steps below before
starting workers, fork loops or MCP-enabled sessions with the new binary.

### Controller-owned workers and session schema 25

The worker connection is now one command:

```sh
coop sessions connect --controller https://controller.example --token-file /private/enrollment-token
```

Use `--state <path>` for a separate private worker root and `--ca-file <path>` for a private
controller CA. The connection owns its local service; an already-listening root is refused, not
reused or replaced. `coop worker`, `sessions serve`, `sessions policies` and `connect --config`
are gone. There is no worker JSON file or local execution-policy catalog to translate.

Before starting a new connection against an existing root, stop its owning connection/service and
back up that root. Use the old binary's `sessions compact --backup <path>` when available, or a
verified stopped-root copy. Opening state with the new service upgrades its SQLite schema to 25;
an older binary cannot reopen that upgraded root. Keep the backup and old binary for rollback.

Controllers must send worker protocol v2 and canonical version-2 JobSpec documents, and select
workers advertising live `job-setup:2`. Each job freezes source/companions, targets, mode and
networking plus explicit `environment`, `check.argv`, `check.environment`, and positive CPU,
memory and PID caps. Resolve repository defaults before submission: worker `COOP_GATE` and
repository `gate:` no longer choose controller reviews. The same frozen setup applies to work
and review, with check-only environment values overriding work values. See
[the session API](docs/session-api.md#sessions) for fields, limits and digest requirements.

Historical jobs remain readable, but jobs without current setup cannot execute new turns, reviews
or interrupted creates. Preserve required work and history, finish/close/discard through the
supported old workflow where possible, and resubmit unfinished work as a new v2 job; never edit
saved documents or synthesize authority to make an old record run. Authenticate provider accounts
on each worker with Coop's normal login flow; controllers do not transfer model credentials.

For network approval use `coop approve`, not `coop net approve`. Use `coop net blocked` instead
of `coop net explain`. The remaining configuration/runtime/task changes are listed below.

### Anchored fork and network identity

Linux overlay filesystems can immediately reuse every observed piece of directory metadata after
deletion, including inode and birth time. Coop therefore no longer treats allocator metadata as
durable authority for the two roots that authorize future sandbox work: fork generations and
project network approvals now use a two-link filesystem anchor.

A fork carries `.coop-fork-generation`, linked to its generation-specific file under the adjacent
owner-only fork state. A reviewed project carries `.coop-network-approval`, linked to a randomly
named file under Coop's owner-only network state. The visible files contain identifiers, not
credentials. Both names must still be the same owner-controlled `0600` inode with exactly two links
whenever Coop uses the authority.

The two names must be on one filesystem with hardlink support. Fork state is adjacent to its
workspace, so this is normally automatic. For network approvals, keep the checkout and
`$XDG_STATE_HOME` (or the default `~/.local/state`) on the same filesystem. Coop reports a clear
error and does not fall back to timestamps when it cannot create the link.

Stopped v1/v2 fork records migrate under the lifecycle lock only when their Git branch and
absolute local origin still match and they have no execution or land intent. Ordinary forks must
also have no reservation; remote sessions may retain only their exact same-generation, same-owner
reservation. An active legacy fork must be stopped and retried. Older network approvals never grant under the new binary:
run `coop approve` once to review and enroll the current project. Task records and remote-session
discard plans did not change format; they continue to combine logical identity, semantic state,
locks and live pinned handles rather than relying on another timestamp.

The markers are excluded from ordinary Git status. A manual `git clean -x` can still remove them.
For a project approval, inspect `ls -l -- ./.coop-network-approval`; after verifying a damaged name
is stale, run `rm -- ./.coop-network-approval` and then `coop approve`. An intact checkout moved to
a new path needs only `coop approve`: Coop proves and retires the old pair before enrolling the new
path. Copying marker bytes never transfers access or retires the original project's anchor.

For a damaged fork marker, first preserve any Git work, task notes, or other data you need. Do not
remove only `.coop-fork-generation`: the generation record and private link must retire together.
Use the normal, confirming `coop fork rm <name> --force` flow to remove that fork and its authority,
then recreate it. This permanently deletes the fork's unmerged work and service volumes, so inspect
the deletion preview before confirming. Coop's own checkpoint restore preserves an intact marker.
Stopped v1/v2 fork generations migrate to hardlink anchors after branch/origin checks. Existing
remote sessions keep only their exact same-generation, same-owner reservation during that migration;
a missing or foreign reservation, or live execution, is never guessed into ownership.

When an anchor exists, runtime arguments may mount the exact project, but not a path inside it that
an existing agent could swap before the runtime resolves the bind, or a writable ancestor that
could replace the project. No mount may expose Coop's private anchor, execution-record, or
launch-lock state, including paths that the first approval or fork will create, even read-only.
Opaque `--volumes-from` and custom volume drivers are refused; existing named volumes are inspected
before launch. Put a cache outside the checkout and mount that explicit, inspectable path instead.

Saved service-file approvals from earlier formats (including the first repository-scoped format)
no longer grant access. Run `coop up` at a terminal to review eligible read-only secret files again;
secret directories and writable binds cannot be approved. An external or custom-named Docker
volume now also needs an explicit terminal review listing its actual name,
read-only/read-write mode, and service targets; automatic, filtered, and non-terminal starts stay
closed even after approval. The grant binds the selected Docker daemon and the inspected plain-local
volume object; switching daemon or replacing a volume requires another review. Approval belongs to the repository's `.coop-service-approval` marker
and a private hardlink in Coop state. Keep both on one filesystem. Copying the marker into another
checkout does not copy its grant. Inspect a damaged marker before removing it and re-reviewing.
Sidecar repository binds using SELinux relabel or propagation options now refuse; remove those
options before retrying. A read-only session also requires its bind source to exist already,
because Compose's short syntax would otherwise create a directory on the host.

### Canonical tasks across isolated forks

Fork loops now schedule from the project's canonical task queue. They no longer copy a complete
`.agent/tasks` tree into each fork, and `coop tasks split` has been retired. Before upgrading, stop
every detached fork loop with the Coop version that started it, then inspect each fork's Git work
and copied task folders. Preserve any fork-only notes, decisions, artifacts, or unfinished changes
before starting the new loop; the new controller deliberately does not choose which of two copied
folders is true.

The first new loop on a fork refuses when that workspace still contains tasks under its own
`.agent/tasks` (including configured subproject queues). Review and reconcile those copies, then
recreate the fork with `coop fork <name> <target> --fresh --loop`; add `--force` only after reviewing
and intentionally disposing of unmerged Git work. Old `.agent/tasks.sliceN` directories in the
canonical checkout are not imported or deleted. Convert genuinely distinct work into canonical
tasks explicitly, and archive the old slice only after verifying every note and change has a home.

After migration, start any number of forks against the same queue:

```sh
coop fork perf codex --loop -d
coop fork docs claude --loop -d
coop tasks watch
```

`--tasks <path>` is now a canonical queue selector, not a copy destination. The host gives each
fork one durable assignment and exposes only that task in `.coop/task-executions/`. Stopping or a
crash retains the assignment; only the exact reviewed generation candidate landing through
`coop fork merge` completes the canonical task. Do not run an older copied-queue worker beside the
new scheduler.

Current detached workers use `owner-v2` state carrying an immutable fork generation. `owner-v1`
remains readable only so an older stopped/cleanup-pending worker can be handled safely; it cannot
claim current task work or attach to a replacement workspace. Never edit a pidfile to add a
generation. Stop the old worker, let Coop bind a fresh generation, and restart it.

Remote sessions created before their fork generation was persisted need the same care. Before
upgrading, finish or discard them with the version that created them and preserve any Git work you
intend to keep. On first start, new Coop adopts a generationless session only when an exact
host-owned reservation already names that same session (the crash-safe partial-upgrade case).
Otherwise it quarantines the record: session/turn history remains readable, but Coop does not run
turns, inspect or review the workspace, clean its runtime or services, close it, or discard it.
This prevents a deleted and recreated same-named fork from being mistaken for the old session.
Inspect and preserve the quarantined workspace directly, then create a new remote session; never
fabricate a generation or reservation file to make the old record attach. When you are done with
the record, retire it: `POST /v1/sessions/<id>/discard` with
`{"retire_quarantined":true,"expected_revision":<n>}` tombstones the row and leaves the workspace to
you. The same applies to a session Coop quarantines later because its workspace vanished.

### Strict `coop.conf`, one removal verb and supported runtimes

- **`coop.conf` is validated on every command.** An unknown key, a duplicate key, a malformed
  line, or a retired key stops Coop before any work, naming the file and line. Replace the retired
  loop settings with their `.agent/loop.yaml` fields: `COOP_LOOP_MODEL` → `work.agent`,
  `COOP_REVIEW_MODEL` → `signoff.agent`, `COOP_MAX_REVIEW_ROUNDS` → `signoff.rounds`,
  `COOP_LOOP_CMD` → `work.command`, `COOP_PREFLIGHT` → `preflight.enabled`.
- **Remove `COOP_AGENT_PACKAGES`.** Coop-managed images use locked client versions. If you need
  different tooling, review and explicitly select a custom image instead of overriding packages.
- **Project networking uses `box.egress: offline`, not `none`.** Update `.agent/project.yaml`;
  controller JobSpec documents still use `none` for their offline mode.
- **Restricted project images need an explicit build.** Review project build inputs, then run
  `coop build --egress filtered` before reusing that image under filtered networking. Old automatic
  build records do not authorize restricted reuse.
- **Removal/privacy flags have explicit names.** Replace `coop down -v` or `--volumes` with
  `coop down --delete-volumes` (still deletes stored service data). Replace
  `coop net export --include-destinations` with `--include-addresses` (still exposes hostnames/IPs).
- **`coop tasks clear` → `coop tasks rm --all-done`.** The old alias is gone.
- **`coop tasks split` is gone.** Parallel forks share the canonical queue; see
  [Canonical tasks across isolated forks](#canonical-tasks-across-isolated-forks) above.
- **Automatic runtime detection prefers Docker.** On a machine with both Docker and Apple
  `container` installed, Coop now selects Docker — previously `container` won on name order. Your
  box image must exist on the runtime you end up on, so run `coop build` once after upgrading if
  your images lived on the other one. To keep Apple `container`, set `COOP_RUNTIME=container`.
  Coop falls back to `container` only when Docker's daemon does not answer, and says so when it
  does.
- **Podman is no longer a container runtime.** Install Docker (or Apple `container` on macOS 26),
  then `coop build && coop doctor`. Auto-detection no longer looks for Podman, and an explicit
  `COOP_RUNTIME=podman` now stops with the reason instead of running. Coop no longer manages
  anything on the Podman side, so stop leftover sibling stacks there first:
  `podman compose -p <project> -f .agent/compose.yml down --remove-orphans`.
- **Session state root schema 25.** Back up before the new connection starts its service; see
  [the controller cutover](#controller-owned-workers-and-session-schema-25) above.

### One composition model, direct fork loops

Fusion was a second command grammar over capabilities Coop already exposes directly. Coop removes
the command and its mandatory "consult everyone before every action" governor prompt; it does not
remove presets, named peers, roles, or consultation.

| Retired | Use |
| --- | --- |
| `coop fusion <target> --peer <target>...` | `coop <target> --peer <target>...` — named peers remain read-only, explicit, and optional |
| `coop fusion <preset>` | `coop <preset>` — the preset lead, native/consult/delegate roles, ladders, and personas are unchanged |
| `coop acp fusion <target> --peer <target>...` | `coop acp <target> --peer <target>...` |
| `coop acp fusion <preset>` | `coop acp <preset>` |

Plain `coop acp` again starts automatically with the first signed-in provider in Claude, Codex,
Gemini, Grok order and its default account. An editor entry can use `["acp"]` and choose through
the live Preset, Provider, and Account selectors. Explicit targets and presets still take
precedence; `--bare` still requires a single explicit target. Preset ladders still rotate across
providers and accounts. Filtered sessions offer only compatible providers and whole presets;
an unrelated signed-in provider no longer prevents a supported lead from starting.
`coop-consult` still provides read-only fresh/continue sessions and target fallback for
named peers and preset consult roles.

Before the first MCP-enabled launch after upgrading, make the configured `COOP_MCP_FILE` a
private readable regular file no larger than 4 MiB. Shared MCP also inspects the selected Codex or Grok
native config, so apply the same preparation there. Gemini's native `settings.json` is projected
even without shared MCP and must be safe before any Gemini launch. Replace a final symlink with a
private regular copy rather than preserving the link, and keep the shared source outside
repositories, companion repositories, credential homes, and ACP session stores that Coop mounts
wholesale. Set `COOP_MCP_FILE` to a canonical path without `..`. Coop leaves an unsafe input untouched
and refuses the affected launch; after correcting the file or source path, retry the original
command.

Project Dockerfiles selected by `.agent/Dockerfile` or `box.dockerfile` must now be regular
in-repository files. Replace a symlink with a regular copy before `coop build`.

Fleet was a declarative wrapper over the fork and loop commands. Coop removes its command family,
live board, and `.agent/fleet.yaml` parser while keeping the direct primitives:

| Retired | Use |
| --- | --- |
| `coop fleet init` / `.agent/fleet.yaml` | No manifest. Start each fork explicitly with `coop fork <name> <target|preset> --loop -d --tasks <path>`. Coop does not delete an existing ignored file; remove it manually after translating its entries. |
| `coop fleet up` | Run the direct detached fork command once per worker. Point workers at the same canonical queue, or use `--tasks <path>` only to select a genuinely separate canonical queue. |
| `coop fleet down` | `coop fork stop <name>` for each running fork. |
| `coop fleet watch` | `coop tasks watch` for merged task progress; `coop fork ls` for fork state/cost; `coop fork logs -f` for output. |
| `coop fleet prune` | `coop fork rm <name>` for each obsolete fork (`--yes` confirms non-interactively; `--force` separately overrides dirty/unmerged protection). |

`coop fork merge --all` still lands every fork through a revalidating rebase queue. There is no
replacement manifest or batch up/down command; start and stop each worker explicitly.

Coop also no longer inspects or removes basename-only Compose projects created before per-workspace
hashed project names. Finish or stop sibling services before upgrading. If one of those old stacks
remains afterward, inspect it with `docker compose ls`, then run
`docker compose -p <legacy-project> -f .agent/compose.yml down --remove-orphans`. Coop manages only
projects named by the current `ComposeProject(workspace)` scheme.

Fork session re-entry uses one record: `.coop/session.<provider>.<account>`. Coop ignores the
older provider-only `.coop/session.<provider>` file and no longer adopts the latest Codex session by
cwd. The first re-entry without a current exact hint starts a fresh conversation. Coop records an
exact hint for later resumes when the provider creates one unambiguous session. Remove provider-only
files when convenient; Coop will neither read nor rewrite them.

`coop init` now maintains only the current scaffold and does not rewrite pre-v8 generated files.
If upgrading from a version older than v8:

- In `.githooks/prepare-commit-msg`, replace
  `$HOME/.config/coop/git-hooks/prepare-commit-msg` with
  `$HOME/.coop-git-hooks/prepare-commit-msg`, then run `chmod +x .githooks/prepare-commit-msg`.
- If `.agent/rules/` exists, run `mkdir -p .agent/kb` and
  `git mv .agent/rules .agent/kb/rules` so the rule cards remain tracked at their current path,
  then update project-owned instructions such as `AGENTS.md` to refer to `.agent/kb/rules/`.
- In `.gitignore`, remove the old Coop stanza containing `.agent/*` or `**/.agent/*` together with
  `!.agent/rules/` or `!**/.agent/rules/`, then run `coop init` once to append the current
  monorepo-aware stanza.

Existing project-owned hooks and custom hook paths remain protected; `init` will describe how to
chain Coop's current hook instead of overwriting them.

Audit-reopen authority is also current-only: active records remain version 3 and
non-authorizing pending records remain version 4. No tagged Coop release wrote the retired v1/v2
formats, so released-version upgrades need no conversion. The now-unused
`coop tasks unblock --adopt-audit-head` bridge has no replacement. If an untagged developer build
left a v1/v2 record, reconcile that task with the originating build before upgrading; current Coop
leaves the record intact and refuses to lease, complete, or unblock the task. Do not delete raw
task-authority registry files to bypass that refusal.

## v4: the target grammar — one way to name a run

A target names who runs: `provider[:model][/effort][@account]`
(`claude`, `claude:opus`, `claude/xhigh`, `claude:opus/xhigh`, `claude@work`, `claude:opus@work`). The provider is
required inside a target, while the model, an optional reasoning
`/effort` (`low`/`medium`/`high`/`xhigh`/`max`, passed straight to the agent's CLI — Gemini has
none and rejects it), and the account are all optional. `--model`, `--credential`, and the boolean
`--consult` retire; peers are named explicitly.

| Retired | Use |
| --- | --- |
| `coop <agent> --model <m>` | `coop <agent>:<m>` — e.g. `coop claude:opus` |
| `coop <agent> --credential <acct>` | `coop <agent>@<acct>` — e.g. `coop claude@work` |
| `coop login <agent> --credential <acct>` | `coop login <agent>@<acct>` |
| `coop loop --model m@work` | `coop loop <agent>:m@work` (account ladder: `<agent>@work,personal`) |
| bare `coop` / `coop loop` (defaulted to claude) | name the target — `coop claude`, `coop loop claude` (or positional `coop loop <preset>`, whose lead supplies it) |
| `coop <agent> --consult` (boolean) | `coop <target> --peer <target>...` — name each peer (repeatable): `--peer codex:gpt-6-astra --peer gemini` |
| `coop fusion <target>` (consulted every signed-in agent) | `coop <target> --peer <target>...` — name only the peers this run may consult |

The target grammar applies on every current launch surface — `coop <target>`, `loop`, `acp`,
`fork <name> [acp]`, and `login`. A Zed `agent_servers` entry can name a target as one token:
`["acp","claude:opus@work"]`, or use `["acp"]` for automatic startup and live selection.

Peers participate **only when named** — the old "every signed-in agent is a peer" policy is
gone. A named peer's credentials are the only ones mounted for consultation (the box's
`coop-consult` refuses any other), so an overnight run can't quietly hand your Codex login to a
Claude lead you never asked to consult it.

Name a preset in the positional who-runs slot — `coop <preset>` or `coop loop <preset>` — rather
than with a flag (a preset is an orthogonal axis — role wiring — not another spelling of the
target).

**Presets follow the same grammar** — `agent:` holds a target or target ladder (native roles
remain one Claude target); the separate `model:`/`models:` keys retire:

| Retired preset shape | Use |
| --- | --- |
| `lead: {agent: claude, models: [fable, opus@work]}` | `lead: {agent: [claude:fable, claude:opus@work]}` — one `agent:` ladder (each entry a target) |
| a role's `agent: codex` + `model: gpt-6-astra` | `agent: codex:gpt-6-astra` — the model rides `agent:` (a role runs its default account; no `@account`) |

A lead ladder MAY be cross-provider (`agent: [claude:opus, codex:gpt-6-astra]`) — the loop rotates
across vendors on a rate limit, running each rung's agent, and an ACP session does too (it
re-creates the session on the new provider and carries the conversation best-effort as a labeled
plain-text preamble). The lead (the default agent, and what a single run uses) is the first rung's
provider. Consult and delegate ROLE ladders fail over inside their wrappers after a proven non-zero
rate-limit response. Native roles remain one target because subagent frontmatter has no runtime
fallback hook. Role rungs always use each provider's default account; `@account` remains lead-only.
Unsigned-in providers are skipped, while every available rung's credential home is mounted in
the lead box.

## v3: retired command aliases

v3 has a clean CLI — no backward-compat aliases. Each retired form is unknown/tombstoned; rewrite:

| Retired | Use |
| --- | --- |
| `coop clone <name>` | `coop fork <name>` |
| `coop profiles …` | `coop credentials …` — a credential is a stored account/login; orchestration recipes are presets (`coop help presets`) |
| `--profile <name>` (login/launch flags) | put the account in the target — `<agent>@<name>` (see the target-grammar section above). `--profile` is no longer a coop flag at all: on an agent launch it forwards to the agent like any other arg (codex has its own `--profile`); elsewhere it's an unknown argument |
| `coop pool <add\|rm\|clear>` | Retired — there is no persistent pool. A loop rotates its preset lead's `agent:` target ladder (`coop help presets`); a bare `provider:model` rung in that ladder fans out across every signed-in account, which is what the pool used to do. A stray `pools.json` is ignored. |
| `coop profiles <default\|rm> <agent> <name>` (verb-first) | `coop credentials <agent> <name> <default\|rm>` (a path) |
| `coop profiles <name> model <m>` / a credential's model mark | Retired — a credential is just an account; the model is a separate axis. Set it inline in the target (`<agent>:<m>`) or in a preset lead's `agent:` target ladder (`coop help presets`). Both spellings of `coop credentials <cred> model` tombstone. |
| `coop status` | `coop tasks watch` (the queue + any active forks) / `coop fork ls` (fork state) |
| `coop tasks start <id>` | `coop tasks claim <id>` |
| `coop loop --debug` | `coop loop --debug-on-fail` |
| `<any> list` (e.g. `coop tasks list`) | `<any> ls` — `ls` is the only list verb |
| `<any> remove` (e.g. `coop tasks remove`) | `<any> rm` — `rm` is the only destructive verb |

## Monorepos: a hand-set `COOP_TASKS` → `.agent/project.yaml`

Not breaking — `COOP_TASKS` still works and still overrides — but if you were exporting
`COOP_TASKS="portal/.agent/tasks runner/.agent/tasks …"` to make coop see a monorepo's
queues, you can delete the export: commit a top-level `.agent/project.yaml` listing the
members and every task command derives the queue set from it (each member's queue plus
the root's own, for changes that span members):

```yaml
subprojects: [portal, runner, mcp, packs]
```

`coop init` at the root writes it for you (it detects direct child dirs that have a
`.agent/`) and scaffolds any member that's missing its queue.

## A legacy `.agent/TASKS.md` → the folder task system

Older coop repos kept the work queue in a single `.agent/TASKS.md` (with
`[ ]`/`[w]`/`[x]`/`[B]` checkboxes) plus a global `.agent/PENDING_DECISIONS.md`.
As of coop v3, that layout is **no longer read** — the format is a **folder per
task** under `.agent/tasks/`, where a task's state is its directory (`00_todo/` ·
`10_in_progress/` · `50_blocked/` · `99_done/`; the numeric prefix just sorts `ls`
in lifecycle order). Convert once with the prompt below; there is no fallback.

To convert, paste the prompt below to any coding agent (Claude, Codex, Gemini, …)
**running in the repo**. It's a one-time, content-preserving migration; an LLM
handles it well because the old task bodies are prose that needs mapping, not a
rigid parse. Afterward, verify with `coop tasks` and `coop tasks lint`.

> Tip: commit (or stash) first, so the conversion is easy to review as a diff.

---

```text
Convert this repo's legacy coop task queue to the folder-based format. Work
carefully and lose no task content.

SOURCE
- `.agent/TASKS.md`: each top-level line `- [ ] / [w] / [x] / [B] <title>` is one
  task; the indented bullets beneath it are that task's body.
- `.agent/PENDING_DECISIONS.md` (if present): human decisions, each tied by its text
  to a blocked task.
- Ignore the header/legend comments, the `[E]` example task, and any `- [ ]` lines
  inside ``` fenced code blocks (those are documentation, not tasks).

TARGET — a folder per task; the task's STATE is its directory (the NN_ prefix is
part of the directory name — use it verbatim):
  `- [ ]` → `.agent/tasks/00_todo/`        `- [w]` → `.agent/tasks/10_in_progress/`
  `- [B]` → `.agent/tasks/50_blocked/`      `- [x]` → `.agent/tasks/99_done/`

FOR EACH TASK
1. id = `YYYY-MM-DD-<slug>`: use a date from the task body if it has one, else
   today; slug = the title lowercased with every run of non-alphanumeric characters
   replaced by a single `-`, trimmed, ≤ 48 chars. Make each id unique.
2. Write `.agent/tasks/<state>/<id>/task.md`:

   ---
   id: <id>
   title: <the task's one-line title>
   labels: []
   updated: <today, ISO-8601>
   ---

   # <title>

   **Context:** <the body's Context, or a one-line summary of the task>

   **Acceptance criteria:** <the body's "Acceptance checks", if any>

   **Approach:** <the body's "Implementation direction", if any>

   ## Subtasks
   - [ ] <each concrete sub-step found in the body>

   Map the old body's Context / Likely files / Implementation direction /
   Acceptance checks into these fields. Omit the Subtasks section if there are no
   steps. Put anything that doesn't fit under a trailing `## Notes` heading —
   never drop content. Do NOT add a `status:` field; the directory is the status.

3. For a `[B]` (blocked) task, also write `.agent/tasks/50_blocked/<id>/decision.md`:

   # Decision: <the open question>

   **Blocks:** this task (`<id>`).

   **The decision:** <what must be chosen>

   **Options:**
   - **A — <name>:** <consequence>
   - **B — <name>:** <consequence>

   **Recommendation:** <if the body or PENDING_DECISIONS suggests one>

   ---

   **Resolution:** <fill in if it was already answered, else leave empty>

   If `.agent/PENDING_DECISIONS.md` has an entry matching this task (by the title or
   topic it names), fold it into this `decision.md`. A pending decision that matches
   no task → create a new `50_blocked/` task for it.

CLEAN UP
4. Once every task and decision has been migrated, delete `.agent/TASKS.md` and
   `.agent/PENDING_DECISIONS.md`.

VERIFY
5. Run `coop tasks` (it should list the same number of tasks as the old file,
   grouped by state) and `coop tasks lint` (it must be clean). Then report a
   summary: tasks migrated per state, decisions folded in, and anything that did
   not map cleanly.
```

## A legacy `.agent/BACKLOG.md` → the backlog drawer

Older coop repos kept unscheduled ideas in a single `.agent/BACKLOG.md` (one `##`
section per idea). As of this release the backlog is a **task-folder drawer** —
`.agent/tasks/xx_backlog/` — managed with `coop backlog`, so an idea that's ready is
promoted with a folder move (`coop backlog promote <id>`) instead of a hand-rewrite,
and `coop init` no longer writes `BACKLOG.md`.

It's a short, do-it-by-hand migration — a backlog is usually a handful of items and
they're prose, not structured data. For each `##` section in `.agent/BACKLOG.md`:

```text
coop backlog add "<the item's title>"
```

then paste the section's notes into the new item's `task.md` (its path is printed by
`coop backlog`, or `coop tasks path <id>`). A `— DEFERRED (<why>)` item carries the
reason across; a shipped or cancelled one you can just drop. When every item has moved,
delete `.agent/BACKLOG.md` and verify with `coop backlog`. (There's nothing to convert
if you never used the file — `coop backlog add` creates the drawer on demand.)
