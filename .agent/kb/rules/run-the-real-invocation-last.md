---
name: run-the-real-invocation-last
description: a green gate is not proof a feature works — the last verification of a launch-path change is the user's own command, in a real repo, after the last commit
scope: agent-workflow
sources: [AGENTS.md, .agent/skills/work/SKILL.md, .agent/skills/sweep/SKILL.md]
check: none
updated: 2026-09-10
---

# The last thing you verify is the command the user types

A gate proves the tree is healthy. It does not prove the feature works, because every harness runs
a leaner shape than a person does: a smoke builds its own minimal config, a tagged suite drives an
internal seam, a unit test fakes the runtime. When a change touches a launch path, the final
verification is the real invocation — the command from the README or the help text, in a real
repository, with the operator's own environment (do not unset their `COOP_*` settings to make it
pass), run AFTER the last commit of the series rather than somewhere in the middle of it.

Two habits follow. Re-run it after the *last* commit, not the last one that touched the feature:
a change three commits later can break the launch and every suite still passes. And when a new
path grows a bound the old path never had — a deadline, a retry cap, a fixed timeout — treat that
bound as a regression risk on its own and ask what the ordinary path does instead.

**Why:** on 2026-09-10 restricted networking was reported as working on the strength of a green
`make check`, a green tagged runtime suite and a green `coop net setup`. The operator's next
`coop run --egress filtered --allow-domain example.com -- curl https://example.com` failed on a
15-second startup deadline that only the filtered path had, and they asked "did you even test your
work?". The setup smoke never hit it: it launches a private temp project with no homes,
credentials, MCP mounts or shadowed secrets, so its container starts in a fraction of the time a
real repo's mount set takes on a loaded machine.

**How to apply:** end the work with the user's command and paste its output in the report. If you
cannot run it — no credentials, no runtime, needs their machine — say exactly that instead of
reporting the suite as if it were the same thing.

## Changelog
- 2026-09-10 — created after the filtered-run failure above (fixed in 2340d85). Swept the sources:
  AGENTS.md's "Done means verified, not done-once" and work's "gate before moving on" both stop at
  the gate, and sweep's completion bar is the gate plus self-review; none of them names the real
  invocation, so this refines rather than repeats them. `check: none` — no command can tell that
  what you ran was the user's command.
