---
name: agent-instructions-use-in-box-capabilities
description: "agent-facing instructions name in-box capabilities, never host-side lifecycle commands"
scope: agent-workflow
sources: [AGENTS.md, internal/scaffold/scaffold.go]
check: "none"
updated: 2026-08-25
---

# Agent-facing instructions use in-box capabilities only

Instructions mounted into Claude, Codex, and Gemini run inside the coop box. They must
not tell the agent to run host-side Coop lifecycle commands such as `coop fork`:
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
- Do not recommend `coop fork` or other host-side lifecycle commands to
  agents. Those can stay in user/operator docs such as README command references.
- Naming a host-side command in order to PROHIBIT it is fine (AGENTS.md's hands-off
  destroyers list does exactly this): the rule bars recommending host commands as
  capabilities, not mentioning them as bans — a ban wastes no turns.
- If a native capability may not exist, phrase it as "if your runtime has it" and require
  the closest safe fallback instead of inventing slash commands or APIs.

## Changelog
- 2026-08-25 — removed the retired Fleet command from current examples and the repo's hands-off
  list; the host-versus-box capability boundary is unchanged.
- 2026-06-26 — created
- 2026-07-11 — revised
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: grepped every agent-facing scaffolded template
  (internal/scaffold/templates/AGENTS.md, templates/agent/tasks/README.md, templates/agent/
  loop.yaml, every templates/skills/*/SKILL.md) for `coop fork`/`coop fleet` — 0 hits, all clean.
  1 nuanced finding, not a clear violation: this repo's own dogfooded AGENTS.md:57 ("Hands-off
  destroyers") names `coop fleet down`, `coop fork rm`/`coop fork merge --force`, `coop tasks rm`,
  and `coop update` inside an agent-facing file — but to PROHIBIT them ("human-only, never run
  unattended... must never invoke"), the opposite of recommending them as a capability, so it
  doesn't trip the rule's actual "Why" (wasted turns from planning unavailable work). The card's
  "How to apply" reads as an absolute no-mention rule and doesn't clearly carve out this
  prohibit-by-naming exception, which looks like a deliberate, sensible safety guardrail already
  in practice. Flagged for the lead to consider clarifying, not fixed here.
- 2026-08-09 — drift repair from the backfill sweep's findings: prohibition carve-out recorded — naming a host command to BAN it is not a violation.
