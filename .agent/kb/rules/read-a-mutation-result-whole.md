---
name: read-a-mutation-result-whole
description: a state-changing command's result is read whole, or its effect checked, before it is ever re-run; a bounded tail is for long logs, never for a short mutating command
scope: agent-workflow
sources: [AGENTS.md, .agent/skills, .agent/kb, tools, Makefile, internal/tasks/cmd.go, internal/tasks/channel.go, internal/taskmcp/tools.go]
check: "make rules-check"
updated: 2026-09-19
---

# Read a mutation's result whole before you ever run it again

A command that changes state — `coop tasks block/claim/done/unblock/add`, `git commit`, a
`docker rm` — is short: read all of its output, or check its effect in the state itself (the task
folder, `git log`), before deciding it failed. Never re-run a mutation because a truncated view of
its output looked like a failure.

**Why:** on 2026-09-19 an agent ran `coop tasks block <id> 2>&1 | tail -2`. The first block had
succeeded, but the tail cut off the success line. The human then answered, moving the task back to
todo, and the agent, finding it in todo, blocked it again. That parked an answered decision as a
fresh question, and `coop tasks` asked the human something they had already decided (task
2026-09-19-never-re-park-erase-or-mislabel-a-decision-the-h). The tool now refuses that re-block
and keeps the earlier answer — or question — in log.md when a new one replaces it
(`go test ./internal/tasks ./internal/taskmcp`). The habit is the other half: the next mutating
command may have no such guard, so the `check:` here guards this repo's OWN instructions
(`tools/check_rules.py`, run by `make rules-check`): a truncated mutation in AGENTS.md, a skill, the
docs or the Makefile fails the gate, because that is where an agent learns the habit.

**How to apply:**
- Run a short state-changing command without `| tail`/`| head`. Its success or refusal line is
  usually the first line.
- Unsure whether it worked? Read the state it changes (`ls .agent/tasks/<state>/`,
  `git log -1`, `docker ps`) — not a second run.
- [[static-bounded-supervision]]'s bounded `tail` is for long supervised logs (gates, builds,
  loops), where the exit status is preserved separately. It is not for mutations.

## Changelog
- 2026-09-19 — created, and mechanized in `tools/check_rules.py`. Swept AGENTS.md, README.md, the
  Makefile, docs/, tools/, .agent/skills and .agent/kb for a truncated mutating command: none found.
