# Migrating

## v3: retired command aliases

v3 has a clean CLI — no backward-compat aliases. Each retired form is unknown/tombstoned; rewrite:

| Retired | Use |
| --- | --- |
| `coop clone <name>` | `coop fork <name>` |
| `coop profiles …` | `coop credentials …` — a credential is a stored account/login; orchestration recipes are presets (`coop help presets`) |
| `--profile <name>` (login/launch flags) | `--credential <name>` — same value, new name; the old spelling errors with this rewrite. An agent's OWN `--profile` still passes through after a `--` |
| `coop pool <add\|rm\|clear>` | Retired — there is no persistent pool. A loop rotates the model-first `models:` ladder of its preset's lead (`coop help presets`); a bare model in that ladder fans out across every signed-in account, which is what the pool used to do. A stray `pools.json` is ignored. |
| `coop profiles <default\|rm> <agent> <name>` (verb-first) | `coop credentials <agent> <name> <default\|rm>` (a path) |
| `coop profiles <name> model <m>` / a credential's model mark | Retired — a credential is just an account; the model is a separate axis. Set it with `--model` (per run) or a preset's `models:` ladder (`coop help presets`). Both spellings of `coop credentials <cred> model` tombstone. |
| `coop tasks start <id>` | `coop tasks claim <id>` |
| `coop loop --debug` | `coop loop --debug-on-fail` |
| `<any> list` (e.g. `coop tasks list`) | `<any> ls` — `ls` is the only list verb |
| `<any> remove` (e.g. `coop tasks remove`) | `<any> rm` — `rm` is the only destructive verb |

## The fleet file: `.agent/fleet` → `.agent/fleet.yaml`

The fleet is YAML-only. The pre-v3 one-line `.agent/fleet` is **not read** — its presence
(alone or alongside `fleet.yaml`) is an error until you translate and delete it. Each old line
`<name> [agent] <tasks-path> [profile=a,b] [model=m] [consult=1]` becomes a `forks:` entry:

```yaml
forks:
  <name>:
    agent: <agent>            # omit to default (or to take a preset's lead)
    tasks: <tasks-path>
    credential: a             # was profile=a,b — a fork runs one account; for a
                              #   multi-account rotation, point it at a preset instead
    model: <m>                # was model=m — may be model@account
    consult: true             # was consult=1
```

Delete `.agent/fleet` once translated. A fork takes a single `model:`/`credential:`;
for a full model-first ladder (fallbacks across models and accounts), set
`preset: <name>` instead — an orchestration preset from `.agent/presets/` whose lead
`models:` ladder the fork's loop rotates (see `coop help presets`).

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
