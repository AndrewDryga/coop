# AGENTS.md — the contract every agent in this repo follows.
# CLAUDE.md should symlink here:  ln -s AGENTS.md CLAUDE.md

## BOOT — on a fresh start or after compaction, read in order:
1. this file
2. .agent/LOG.md      (what was done and why)
3. .agent/TASKS.md    (what's left)

## How we build (the creed)
- **Boring first.** Reach for the dull, proven shape; clever earns its place only when boring can't do the job — and you can say *why* in one sentence.
- **Wear the hats** before coding: PM (the right, smallest thing?), UX (obvious path; empty/error states handled?), Security (what's the abuse case?), Maintainer (clear in six months?).
- **Done means verified, not done-once** — formatted, gated green, tested including the failure path. Never "should work": show the gate, or say what you couldn't check.
- **Readable, no bloat.** Match the surrounding style; delete more than you add; no knobs nobody asked for; comments say *why*, not *what*.
- **Boy-scout rule.** Fix small, safe messes as you pass through; backlog the big ones — never smuggle an unrelated refactor into the commit.

## The gate (adapt to this repo)
`<format-check> && <build --warnings-as-errors> && <tests>`

## The contract
- States: `[ ]` todo · `[w]` claimed · `[x]` done+gated+committed · `[B]` blocked.
- Claim a task by flipping it to `[w]` BEFORE you start it.
- `[x]` only when the gate is green, the change is committed, and LOG.md has an entry.
- Blocked? `[B]` + a .agent/PENDING_DECISIONS.md entry. There is no fifth state.
- One task = one commit. Spot unrelated work? Put it in .agent/BACKLOG.md and stay on task.
- **Stay on the branch you're given.** Never create, switch, or delete a git branch unless explicitly asked — commit onto the current branch. (Coop checks you out on a branch already; a new one strands your work where the human isn't looking.)
- **Tasks are self-contained.** TASKS.md gets read by a fresh agent after a compaction or in a new session — so a task can't lean on prior chat, a past review, or memory not in the repo. Each one states: the problem + context, likely files, an implementation direction, and acceptance checks. If it can't stand on its own with just the BOOT files, it isn't ready for the queue.
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
- `PENDING_DECISIONS.md` — anything needing a human call: the decision, the
  options, your recommendation. Mark the task `[B]`. Never guess on a one-way door.
- `IDEAS.md` — product ideas as short sketches. Never auto-implemented; a human
  approves and moves one into TASKS.md. The loop reads TASKS.md only.
- `rules/` — the taste knowledge base (the one committed part).

## Skills
Use the workflow skills instead of hand-rolling: `/spec` before a multi-file
change, `/work` to execute a plan step by step against the gate, `/sweep` to
drain `.agent/TASKS.md` unattended, `/investigate` to root-cause a failure before
fixing, `/verify-api` before calling anything you're not sure exists. They live once in
`.agent/skills/`; each agent's dir (`.claude`, `.codex`, `.gemini`) symlinks to it.

## Taste
Every correction from me becomes a rule the same day: fix it, record it in
.agent/rules/, sweep the codebase for siblings, and graduate it into a lint/hook
when it's mechanically checkable.
