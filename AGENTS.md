# AGENTS.md — the contract every agent in this repo follows.
# CLAUDE.md should symlink here:  ln -s AGENTS.md CLAUDE.md

## BOOT — on a fresh start or after compaction, read in order:
1. this file
2. .agent/LOG.md      (what was done and why)
3. .agent/TASKS.md    (what's left)

## The gate (adapt to this repo)
`<format-check> && <build --warnings-as-errors> && <tests>`

## The contract
- States: `[ ]` todo · `[w]` claimed · `[x]` done+gated+committed · `[B]` blocked.
- Claim a task by flipping it to `[w]` BEFORE you start it.
- `[x]` only when the gate is green, the change is committed, and LOG.md has an entry.
- Blocked? `[B]` + a .agent/PENDING_DECISIONS.md entry. There is no fifth state.
- One task = one commit. Spot unrelated work? Put it in .agent/BACKLOG.md and stay on task.
- Never stop while a `[ ]` remains.

## The .agent/ working state
Durable working memory the BOOT protocol reads back. Only `rules/` is committed;
the rest is local (git-ignored) so it never creates commit noise or merge churn.
- `TASKS.md` — the work queue (the four states above).
- `BACKLOG.md` — work you discover *outside* the current task: capture it, stay on
  task, keep going. Not auto-worked, not scanned by the Stop hook; a human
  promotes an item into TASKS.md when it's time.
- `LOG.md` — your chain-of-thought: what you did and *why*, so intent survives a
  compaction. Append a short entry per decision/task, newest first.
  **Housekeeping is mandatory, not optional.** When LOG.md exceeds ~80 entries,
  trim older entries down to one-liners or remove them entirely in the same
  commit. Never postpone cleanup because the file is large — that is exactly
  when it must happen.
- `PENDING_DECISIONS.md` — anything needing a human call: the decision, the
  options, your recommendation. Mark the task `[B]`. Never guess on a one-way door.
- `IDEAS.md` — product ideas as short sketches. Never auto-implemented; a human
  approves and moves one into TASKS.md. The loop reads TASKS.md only.
- `rules/` — the taste knowledge base (the one committed part).

## Skills
Use the workflow skills instead of hand-rolling: `/spec` before a multi-file
change, `/work` to execute a plan step by step against the gate, `/sweep` to
drain `.agent/TASKS.md` unattended, `/verify-api` before calling anything you're
not sure exists. They live in `.claude/skills/` (Codex shares them).

## Taste
Every correction from me becomes a rule the same day: fix it, record it in
.agent/rules/, sweep the codebase for siblings, and graduate it into a lint/hook
when it's mechanically checkable.
