# Migrating

## A legacy `.agent/TASKS.md` → the folder task system

Older coop repos kept the work queue in a single `.agent/TASKS.md` (with
`[ ]`/`[w]`/`[x]`/`[B]` checkboxes) plus a global `.agent/PENDING_DECISIONS.md`.
coop still reads that layout (it's auto-detected), but the current format is a
**folder per task** under `.agent/tasks/`, where a task's state is its directory
(`todo/` · `in_progress/` · `blocked/` · `done/`). See the README's task section.

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

TARGET — a folder per task; the task's STATE is its directory:
  `- [ ]` → `.agent/tasks/todo/`        `- [w]` → `.agent/tasks/in_progress/`
  `- [B]` → `.agent/tasks/blocked/`      `- [x]` → `.agent/tasks/done/`

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

3. For a `[B]` (blocked) task, also write `.agent/tasks/blocked/<id>/decision.md`:

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
   no task → create a new `blocked/` task for it.

CLEAN UP
4. Once every task and decision has been migrated, delete `.agent/TASKS.md` and
   `.agent/PENDING_DECISIONS.md`.

VERIFY
5. Run `coop tasks` (it should list the same number of tasks as the old file,
   grouped by state) and `coop tasks lint` (it must be clean). Then report a
   summary: tasks migrated per state, decisions folded in, and anything that did
   not map cleanly.
```
