---
name: agent-instructions-use-in-box-capabilities
description: "agent-facing instructions name in-box capabilities, never host-side `coop fork`/`fleet`"
scope: agent-workflow
sources: [AGENTS.md, internal/scaffold/scaffold.go]
check: "none"
updated: 2026-07-11
---

# Agent-facing instructions use in-box capabilities only

Instructions mounted into Claude, Codex, and Gemini run inside the coop box. They must
not tell the agent to run host-side Coop commands such as `coop fork` or `coop fleet`:
those commands are operator controls outside the isolated container, not capabilities the
boxed agent can rely on.

**Why:** recommending unavailable commands wastes turns and can make the agent plan work
it cannot execute. It also blurs the security boundary: Coop orchestration belongs to the
human/operator layer; the boxed agent should use only its runtime's native tools.

**How to apply:**
- Agent-facing files include repo `AGENTS.md`, scaffolded instruction templates, a user's
  global `~/.config/coop/agents/INSTRUCTIONS.md`, and any instruction text mounted into
  agent homes.
- Recommend native/runtime capabilities: subagents, task workers, goal trackers, batch or
  parallel read-only tool calls, and the repo task/log files.
- Do not recommend `coop fork`, `coop fleet`, or other host-side lifecycle commands to
  agents. Those can stay in user/operator docs such as README command references.
- If a native capability may not exist, phrase it as "if your runtime has it" and require
  the closest safe fallback instead of inventing slash commands or APIs.

## Changelog
- 2026-06-26 — created
- 2026-07-11 — revised
- 2026-08-06 — card metadata added (format v1); body unchanged
