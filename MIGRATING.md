# Migrating

## v9: one composition model, direct fork loops

Fusion was a second command grammar over capabilities Coop already exposes directly. v9 removes
the command and its mandatory "consult everyone before every action" governor prompt; it does not
remove presets, named peers, roles, or consultation.

| Retired | Use |
| --- | --- |
| `coop fusion <agent> --peer <peer>...` | `coop <agent> --peer <peer>...` — named peers remain read-only, explicit, and optional |
| `coop fusion <preset>` | `coop <preset>` — the preset lead, native/consult/delegate roles, ladders, and personas are unchanged |
| `coop acp fusion <agent> --peer <peer>...` | `coop acp <agent> --peer <peer>...` |
| `coop acp fusion <preset>` | `coop acp <preset>` |

ACP still requires an explicit initial target or preset. After connection, its live Preset,
Provider, and Account selectors keep working; preset ladders still rotate across providers and
accounts. `coop-consult` still provides read-only fresh/continue sessions and target fallback for
named peers and preset consult roles.

Fleet was a declarative wrapper over the fork and loop commands. v9 removes its command family,
live board, and `.agent/fleet.yaml` parser while keeping the direct primitives:

| Retired | Use |
| --- | --- |
| `coop fleet init` / `.agent/fleet.yaml` | No manifest. Start each fork explicitly with `coop fork <name> <target|preset> --loop -d --tasks <path>`. Coop does not delete an existing ignored file; remove it manually after translating its entries. |
| `coop fleet up` | Run the direct detached fork command once per queue. `coop tasks split <n>` can create `.agent/tasks.sliceN` copy-trees first. |
| `coop fleet down` | `coop fork stop <name>` for each running fork. |
| `coop fleet watch` | `coop tasks watch` for merged task progress; `coop fork ls` for fork state/cost; `coop fork logs -f` for output. |
| `coop fleet prune` | `coop fork rm <name>` for each obsolete fork (`--yes` confirms non-interactively; `--force` separately overrides dirty/unmerged protection). |

`coop fork merge --all` still lands every fork through a revalidating rebase queue. There is no
replacement manifest or batch up/down command in v9; the smaller explicit surface is intentional.

Coop also no longer inspects or removes basename-only Compose projects created before per-workspace
hashed project names. Finish or stop sibling services before upgrading. If one of those old stacks
remains afterward, inspect it with `docker compose ls` (or `podman compose ls`), then run
`docker compose -p <legacy-project> -f .agent/compose.yml down --remove-orphans` (substitute
`podman compose` when applicable). v9 manages only projects named by the current
`ComposeProject(workspace)` scheme.

Fork session re-entry also has one v9 record: `.coop/session.<provider>.<account>`. Coop ignores the
older provider-only `.coop/session.<provider>` file and no longer adopts the latest Codex session by
cwd. The first re-entry without a current exact hint starts a fresh conversation. Coop records an
exact hint for later resumes when the provider creates one unambiguous session. Remove provider-only
files when convenient; Coop will neither read nor rewrite them.

`coop init` now maintains only the current scaffold and does not rewrite pre-v8 generated files.
For a direct pre-v8-to-v9 upgrade:

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

## v4: the target grammar — one way to name a run

Every launch names WHO runs with a single **target**: `provider[:model][/effort][@account]`
(`claude`, `claude:opus`, `claude/xhigh`, `claude:opus/xhigh`, `claude@work`, `claude:opus@work`). The provider is
**required** — there is no implicit `claude` default — while the model, an optional reasoning
`/effort` (`low`/`medium`/`high`/`xhigh`/`max`, passed straight to the agent's CLI — Gemini has
none and rejects it), and the account are all optional. `--model`, `--credential`, and the boolean
`--consult` retire; peers are named explicitly.

| Retired | Use |
| --- | --- |
| `coop <agent> --model <m>` | `coop <agent>:<m>` — e.g. `coop claude:opus` |
| `coop <agent> --credential <acct>` | `coop <agent>@<acct>` — e.g. `coop claude@work` |
| `coop login <agent> --credential <acct>` | `coop login <agent>@<acct>` |
| `coop loop --model m@work` | `coop loop <agent>:m@work` (account ladder: `<agent>@work,personal`) |
| bare `coop` / `loop` / `acp` / `fusion` (defaulted to claude) | name the agent — `coop claude`, `coop loop claude`, … (or positional `coop loop <preset>`, whose lead supplies it) |
| `coop <agent> --consult` (boolean) | `coop <agent> --peer <peer>…` — name each peer (repeatable): `--peer codex:gpt-5.5 --peer gemini` |
| `coop fusion <gov>` (consulted everyone signed in) | `coop fusion <gov> --peer <agent>…` — a council needs ≥1 named peer (repeatable) |

These apply on **every** launch surface — `coop <agent>`, `loop`, `acp`, `fusion`,
`fork <name> [acp]`, and `login`. A Zed `agent_servers` entry names the agent as one token:
`["acp","claude:opus@work"]` (a bare `["acp"]` now errors instead of defaulting).

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
| a role's `agent: codex` + `model: gpt-5.5` | `agent: codex:gpt-5.5` — the model rides `agent:` (a role runs its default account; no `@account`) |

A lead ladder MAY be cross-provider (`agent: [claude:opus, codex:gpt-5.5]`) — the loop rotates
across vendors on a rate limit, running each rung's agent, and an ACP session does too (it
re-creates the session on the new provider and carries the conversation best-effort as a labeled
plain-text preamble). The lead (the default agent, and what a single run uses) is the first rung's
provider. `coop fusion` refuses a cross-provider ladder (one governor for the whole council). A
Consult and delegate ROLE ladders fail over inside their wrappers after a proven non-zero
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
