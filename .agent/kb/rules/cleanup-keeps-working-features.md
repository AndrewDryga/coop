---
name: cleanup-keeps-working-features
description: a cleanup removes only what nothing can reach or run; a feature that works today stays unless the owner names it for removal
scope: agent-workflow
sources: [AGENTS.md, .agent/kb/mcp-authority-projection.md]
check: none
updated: 2026-11-04
---

# A cleanup removes what nothing can use, never a working feature

A cleanup or audit may remove code nothing can reach, tests that only pin something already gone,
notes about the past, stale docs and leftover files. Anything a person can run, configure or depend
on today stays, even when it looks old or is called "legacy" or "compatibility".

Don't recommend dropping a working feature as part of a cleanup. List it as working and say what it
does. If you think it should go, ask a separate question that says who loses what, and remove it only
when the owner names that feature.

**Why:** on 2026-11-04 the owner asked to remove all legacy, and a scan recommended dropping the
legacy SSE route for MCP servers because nobody could see who used it. That route works today for
anyone with an SSE-only server that needs a secret, the owner had decided to keep it on 2026-09-20,
and three of the scan's reviewers said keep. The owner stopped the work before anything was removed:
"wait you can't drop actual features someone might need!"

**How to apply:**
- "I can't see who uses it" is not evidence that nobody does, and one person's configuration only
  speaks for that person.
- Removal needs proof that nothing can reach it: no production caller, no stored data, no option a
  person can set. A working path with few users is not dead code.
- A path for a third party's format or transport is a feature for as long as someone could still be
  on it. State only Coop wrote follows [[one-upgrade-for-old-state]].
- A command or target a person can still type is a feature, even when no script calls it. A
  tombstone that only says where something went is not.
- Before offering a removal, look for an earlier decision in the rule and KB cards and in the
  `decision.md` of finished tasks.

See also [[fix-the-bug-not-the-feature]] and [[replacements-remove-old-paths]], which covers a
simplification the owner asked for.

## Changelog
- 2026-11-04 — created. Swept the scan's list
  (99_done/2026-11-04-list-all-legacy-code-compatibility-shims-history/findings.md): 6 violations,
  all my own recommendations and none acted on. CLI-17 (the SSE route) and CLI-21 (`make check-process`,
  `check-race`) moved to "must stay"; picks withdrawn on CLI-12, CLI-34, CLI-57, TLF-19 and TLF-30.
  Read the backlog and queue tasks that retire or remove something: 0 violations, since each carries
  an owner decision (audit decision 3.3 of 2026-10-15) or replaces a mechanism without losing what it
  does.
